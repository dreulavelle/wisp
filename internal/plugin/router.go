package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
)

// resolveBudget bounds one playback resolution end to end.
//
// Chosen against what the client will tolerate rather than what upstream might
// eventually manage: a viewer who pressed play has already waited through a
// scrape, and a clear "try again" beats a spinner that resolves after they have
// given up.
const resolveBudget = 15 * time.Second

// Settings is the operator-facing configuration the dashboard displays.
type Settings struct {
	// AIOStreamsHost is the host only, never the full URL: the URL embeds the
	// instance's encrypted config blob, which is effectively a credential.
	AIOStreamsHost string
	LibraryPath    string
	DefaultQuality string
}

// Router builds the HTTP surface Silo mounts for this plugin.
type Router struct {
	resolver *Resolver
	log      *slog.Logger
	started  time.Time
	version  string
	settings Settings
	library  *Library
	recorder *Recorder
	signer   *Signer
	monitor  *MonitorHolder
}

// RouterOptions configures a Router.
type RouterOptions struct {
	Resolver *Resolver
	Log      *slog.Logger
	Version  string
	Settings Settings
	Library  *Library
	Recorder *Recorder
	// Monitor surfaces the fill-episodes pass on the dashboard. Optional.
	Monitor *MonitorHolder
	// Signer authenticates resolver requests. Nil disables verification, which
	// is only appropriate in tests: the resolver route is public, so an
	// unsigned deployment lets anyone mint stream links.
	Signer *Signer
}

// NewRouter returns the plugin's HTTP handler.
func NewRouter(resolver *Resolver, log *slog.Logger) *Router {
	return NewRouterWith(RouterOptions{Resolver: resolver, Log: log})
}

// NewRouterWith builds a router with the full dashboard surface wired up.
func NewRouterWith(opts RouterOptions) *Router {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Library == nil {
		opts.Library = NewLibrary()
	}
	if opts.Recorder == nil {
		opts.Recorder = NewRecorder()
	}
	return &Router{
		resolver: opts.Resolver,
		log:      opts.Log,
		started:  time.Now(),
		version:  opts.Version,
		settings: opts.Settings,
		library:  opts.Library,
		recorder: opts.Recorder,
		signer:   opts.Signer,
		monitor:  opts.Monitor,
	}
}

// Handler returns the mux serving every plugin route.
//
// Access levels are declared in the manifest, not enforced here: Silo gates
// /admin/* to admins before the request ever reaches this process. The split
// matters because /resolve/* must stay reachable without a Silo session — the
// client following a placeholder redirect is ffmpeg or a browser, neither of
// which carries one.
func (rt *Router) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", rt.handleHealth)
	mux.HandleFunc("/resolve/", rt.handleResolve)

	mux.HandleFunc("/admin/api/status", rt.adminStatus)
	mux.HandleFunc("/admin/api/placeholders", rt.adminPlaceholders)
	mux.HandleFunc("/admin/api/activity", rt.adminActivity)
	mux.HandleFunc("/admin/api/settings", rt.adminSettings)
	mux.HandleFunc("/admin/", rt.adminIndex)
	mux.HandleFunc("/admin", rt.adminIndex)
	return mux
}

func (rt *Router) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"uptime_seconds": int(time.Since(rt.started).Seconds()),
		"configured":     rt.resolver != nil,
	})
}

// handleResolve answers a placeholder with a redirect to a live stream.
//
// This is the latency-critical path: a user has already pressed play and is
// watching a spinner until this returns. Everything here is deliberately a
// single hop with no retries — a fast failure the client can retry beats a slow
// success it has already given up waiting for.
func (rt *Router) handleResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if rt.resolver == nil {
		writeError(w, http.StatusServiceUnavailable, "wisp is not configured yet")
		return
	}

	req, err := ParseResolvePath(r.URL.Path)
	if err != nil {
		rt.log.Warn("resolve: bad path", "path", r.URL.Path, "error", err)
		writeError(w, http.StatusBadRequest, "invalid resolver path")
		return
	}
	req.Quality = strings.TrimSpace(r.URL.Query().Get("quality"))
	if imdb := strings.TrimSpace(r.URL.Query().Get("imdb")); imdb != "" {
		req.IMDbID = imdb
	}

	// Authenticate before doing any upstream work. An unsigned request must
	// cost nothing: otherwise the public route becomes a way to drive load
	// against the operator's provider without even obtaining a stream.
	if rt.signer != nil && !rt.signer.Verify(req, r.URL.Query().Get("t")) {
		rt.log.Warn("resolve: rejected unsigned request",
			"path", r.URL.Path, "remote", r.RemoteAddr)
		// Deliberately indistinguishable from an unknown path: a distinct
		// "bad token" reply would confirm which titles exist.
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// Bound the upstream call explicitly. Nothing else on this path imposes a
	// deadline, so without one a hung AIOStreams leaves ffmpeg and the viewer
	// on a spinner for the full client timeout. A fast failure the client can
	// retry beats a slow success it has already given up waiting for.
	ctx, cancel := context.WithTimeout(r.Context(), resolveBudget)
	defer cancel()

	start := time.Now()
	stream, err := rt.resolver.Resolve(ctx, req)
	elapsed := time.Since(start)

	if err != nil {
		rt.recorder.Record(Activity{
			At: start, MediaID: req.ID.String(), Quality: req.Quality,
			SearchMS: elapsed.Milliseconds(), TotalMS: elapsed.Milliseconds(),
			Error: shortReason(err),
		})
		rt.library.MarkFailed(req.ID, req.Season, req.Episode, shortReason(err))
		rt.writeResolveError(w, req, err, elapsed)
		return
	}

	// Selection is in-process and effectively free; splitting it out anyway
	// makes it obvious on the dashboard that latency lives upstream, which is
	// the first question an operator asks when playback feels slow.
	rt.recorder.Record(Activity{
		At: start, Title: stream.Filename, MediaID: req.ID.String(),
		Quality:  stream.Resolution,
		SearchMS: elapsed.Milliseconds(), SelectMS: 0, RedirectMS: 0,
		TotalMS: elapsed.Milliseconds(),
	})
	rt.library.MarkResolved(req.ID, req.Season, req.Episode)

	rt.log.Info("resolve: redirecting",
		"media_type", req.MediaType, "id", req.ID.String(),
		"season", req.Season, "episode", req.Episode,
		"requested_quality", req.Quality, "served_quality", stream.Resolution,
		"elapsed_ms", elapsed.Milliseconds())

	// Never cacheable: the target is short-lived, and a cached redirect would
	// strand a later playback on a dead link with no way to recover.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("X-Wisp-Resolved-Quality", stream.Resolution)
	http.Redirect(w, r, stream.URL, RedirectStatus)
}

// writeResolveError maps a resolution failure onto a status a media server can
// act on. The distinction that matters is retryable versus not: a title with no
// release yet should not look like a broken plugin.
func (rt *Router) writeResolveError(w http.ResponseWriter, req ResolveRequest, err error, elapsed time.Duration) {
	log := rt.log.With(
		"media_type", req.MediaType, "id", req.ID.String(),
		"season", req.Season, "episode", req.Episode,
		"elapsed_ms", elapsed.Milliseconds())

	switch {
	case errors.Is(err, ErrNoLookupKey):
		log.Error("resolve: placeholder carries no IMDb lookup key")
		writeError(w, http.StatusBadRequest, "placeholder is missing its lookup key")

	case errors.Is(err, ErrNoMatch):
		// Nothing playable right now. Normal for unreleased or thinly-seeded
		// titles, so this is info rather than an error.
		log.Info("resolve: no playable stream")
		writeError(w, http.StatusServiceUnavailable, "no playable stream for this title yet")

	case isTransient(err):
		log.Warn("resolve: upstream unavailable", "error", err)
		writeError(w, http.StatusBadGateway, "stream provider is temporarily unavailable")

	default:
		log.Error("resolve: failed", "error", err)
		writeError(w, http.StatusBadGateway, "resolution failed")
	}
}

// isTransient reports whether an error is worth retrying.
func isTransient(err error) bool {
	var searchErr *aiostreams.SearchError
	if errors.As(err, &searchErr) {
		return searchErr.Kind == aiostreams.KindTransient
	}
	return false
}

// shortReason renders an error for display without echoing upstream text,
// which can carry credentials embedded in resolver URLs.
func shortReason(err error) string {
	switch {
	case errors.Is(err, ErrNoLookupKey):
		return "missing lookup key"
	case errors.Is(err, ErrNoMatch):
		return "no playable stream"
	case isTransient(err):
		return "provider unavailable"
	default:
		return "resolution failed"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits a stable error shape. Upstream error text is deliberately
// not echoed: it can carry credentials from resolver URLs.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"status":  status,
		"message": message,
	}})
}
