package plugin

import (
	"embed"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

// maxActivity bounds the in-memory resolve log.
//
// This is diagnostic, not an audit trail: it answers "is resolution slow right
// now, and where is the time going" for an operator looking at the dashboard.
// Keeping it in memory and bounded means it costs nothing and cannot grow into
// a storage problem.
const maxActivity = 50

// Activity is one recorded resolve attempt, broken down by hop.
//
// The breakdown is the point. A single total latency tells an operator that
// playback is slow; the split tells them whether to look at their AIOStreams
// instance or at their own selection rules.
type Activity struct {
	At         time.Time `json:"at"`
	Title      string    `json:"title,omitempty"`
	MediaID    string    `json:"media_id,omitempty"`
	Quality    string    `json:"quality,omitempty"`
	SearchMS   int64     `json:"search_ms"`
	SelectMS   int64     `json:"select_ms"`
	RedirectMS int64     `json:"redirect_ms"`
	TotalMS    int64     `json:"total_ms"`
	Error      string    `json:"error,omitempty"`
}

// Recorder keeps a bounded, newest-first log of resolve attempts.
type Recorder struct {
	mu       sync.Mutex
	entries  []Activity
	resolved int
	failures int
	since    time.Time
	samples  []int64 // total latencies, for a median
}

// NewRecorder returns an empty recorder.
func NewRecorder() *Recorder { return &Recorder{since: time.Now()} }

// Since reports when this recorder started counting, which is what makes its
// monotonic totals interpretable.
func (r *Recorder) Since() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.since
}

// Record appends an attempt, evicting the oldest beyond maxActivity.
func (r *Recorder) Record(a Activity) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.entries = append([]Activity{a}, r.entries...)
	if len(r.entries) > maxActivity {
		r.entries = r.entries[:maxActivity]
	}
	if a.Error != "" {
		r.failures++
		return
	}
	r.resolved++
	r.samples = append(r.samples, a.TotalMS)
	if len(r.samples) > 200 {
		r.samples = r.samples[len(r.samples)-200:]
	}
}

// Snapshot returns the log newest-first.
func (r *Recorder) Snapshot() []Activity {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Activity, len(r.entries))
	copy(out, r.entries)
	return out
}

// Stats returns aggregate counters for the dashboard.
func (r *Recorder) Stats() (resolved, failures int, medianMS int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	resolved, failures = r.resolved, r.failures
	if len(r.samples) == 0 {
		return resolved, failures, 0
	}
	sorted := make([]int64, len(r.samples))
	copy(sorted, r.samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return resolved, failures, sorted[len(sorted)/2]
}

// adminIndex serves the dashboard shell.
//
// Silo gates this route to admins, so there is no auth check here — but the
// route is only safe because the manifest declares access "admin". Serving it
// from a route the manifest declares public would expose operational detail to
// anyone who can reach the server.
func (rt *Router) adminIndex(w http.ResponseWriter, r *http.Request) {
	page, err := webFS.ReadFile("web/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "dashboard is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The dashboard is a build artifact embedded in the binary, so it is safe to
	// cache for the life of a page view but must not outlive an upgrade.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(page)
}

func (rt *Router) adminStatus(w http.ResponseWriter, r *http.Request) {
	resolved, failures, median := rt.recorder.Stats()
	// These counters are monotonic for the life of the process, not windowed.
	// Naming them "today" or "24h" would state something false: on a plugin
	// that has been up for weeks, a failure count from a fortnight ago would
	// read as a live problem — the one question this panel exists to answer.
	// counting_since gives the operator the denominator instead.
	status := map[string]any{
		"version":           rt.version,
		"resolver_ready":    rt.resolver != nil,
		"uptime_seconds":    int(time.Since(rt.started).Seconds()),
		"placeholders":      rt.library.Count(),
		"resolved_total":    resolved,
		"failures_total":    failures,
		"counting_since":    rt.recorder.Since(),
		"median_resolve_ms": median,
	}

	// New episodes are the one thing that arrives without anybody asking, so
	// the dashboard has to show whether that is still happening. A pass that
	// finds nothing looks exactly like a monitor that never runs, and the
	// difference only shows up weeks later as episodes quietly missing.
	if rt.monitor != nil {
		last := rt.monitor.LastPass()
		monitor := map[string]any{
			"series_tracked": rt.monitor.TrackedSeries(),
			"has_run":        last.Ran,
		}
		if last.Ran {
			monitor["last_run_at"] = last.At
			monitor["last_written"] = last.Written
			monitor["last_failed"] = last.Failed
		}
		status["monitor"] = monitor
	}

	writeJSON(w, http.StatusOK, status)
}

// defaultPlaceholderPage bounds how many TITLES the dashboard is sent.
//
// Rows are titles rather than files: a single eight-season show is 352
// placeholders, and listing them meant a wall of near-identical lines a person
// had to read to learn one thing. The list is still unbounded in principle — a
// library is as large as somebody's appetite — and the dashboard polls, so it
// is also capped. The total comes back so the page can say what it is showing.
const defaultPlaceholderPage = 50

// maxPlaceholderPage caps what a caller may ask for, so a hand-written
// ?limit=100000 cannot turn the dashboard into a memory spike.
const maxPlaceholderPage = 500

func (rt *Router) adminPlaceholders(w http.ResponseWriter, r *http.Request) {
	all := Summarize(rt.library.List())
	limit := defaultPlaceholderPage
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = min(n, maxPlaceholderPage)
		}
	}

	items := all
	if len(items) > limit {
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"total": len(all),
	})
}

func (rt *Router) adminActivity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": rt.recorder.Snapshot()})
}

// adminSettings exposes configuration for display.
//
// Only the AIOStreams *host* is returned, never the configured URL: that URL
// embeds the instance's encrypted config blob, which is effectively a
// credential. The dashboard needs to show which instance is in use, not how to
// authenticate to it.
func (rt *Router) adminSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"aiostreams_host": rt.settings.AIOStreamsHost,
		"library_path":    rt.settings.LibraryPath,
		"default_quality": rt.settings.DefaultQuality,
	})
}
