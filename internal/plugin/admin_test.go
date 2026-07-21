package plugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/dreulavelle/wisp/internal/aiostreams"
)

func testRouter(t *testing.T, resolver *Resolver) (*Router, http.Handler) {
	t.Helper()
	rt := NewRouterWith(RouterOptions{
		Resolver: resolver,
		Log:      slog.New(slog.DiscardHandler),
		Version:  "2.0.0",
		Settings: Settings{
			AIOStreamsHost: "aiostreams.example.com",
			LibraryPath:    "/library",
			DefaultQuality: "1080p",
		},
	})
	return rt, rt.Handler()
}

func TestAdminIndexServesDashboard(t *testing.T) {
	_, h := testRouter(t, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"<title>Wisp</title>", "/admin/api", "hop"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

// The AIOStreams URL embeds the instance's encrypted config blob, which is
// effectively a credential. The dashboard needs to show which instance is in
// use, never how to authenticate to it.
func TestAdminSettingsNeverExposesTheAIOStreamsURL(t *testing.T) {
	rt := NewRouterWith(RouterOptions{
		Log: slog.New(slog.DiscardHandler),
		Settings: Settings{
			AIOStreamsHost: "aiostreams.example.com",
			LibraryPath:    "/library",
		},
	})

	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil))

	body := rec.Body.String()
	for _, leak := range []string{"stremio", "manifest.json", "password", "SECRETUUID"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("settings response leaked %q: %s", leak, body)
		}
	}
	if !strings.Contains(body, "aiostreams.example.com") {
		t.Errorf("settings should still report the host: %s", body)
	}
}

func TestAdminStatusShape(t *testing.T) {
	rt, h := testRouter(t, alwaysLive(NewResolver(&stubSearcher{})))
	rt.library.Add(Placeholder{Path: "/library/Movies/A/A.strm", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status is not JSON: %v", err)
	}
	if body["resolver_ready"] != true {
		t.Error("resolver_ready should be true when a resolver is configured")
	}
	if body["placeholders"].(float64) != 1 {
		t.Errorf("placeholders = %v, want 1", body["placeholders"])
	}
}

// The dashboard's whole diagnostic value is showing where resolve time goes,
// so a resolve must leave a trace behind.
func TestResolveIsRecordedForTheDashboard(t *testing.T) {
	rt, h := testRouter(t, alwaysLive(NewResolver(&stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn/a.mkv", Resolution: "1080p", Filename: "A.1080p.mkv"},
	}})))
	rt.library.Add(Placeholder{Path: "/library/Movies/A/A.strm", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/resolve/movie/tmdb:603?imdb=tt0133093", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("resolve status = %d, want 302", rec.Code)
	}

	entries := rt.recorder.Snapshot()
	if len(entries) != 1 {
		t.Fatalf("recorded %d activities, want 1", len(entries))
	}
	if entries[0].Quality != "1080p" {
		t.Errorf("recorded quality = %q, want 1080p", entries[0].Quality)
	}

	items := rt.library.List()
	if items[0].Plays != 1 {
		t.Errorf("plays = %d, want 1", items[0].Plays)
	}
	if items[0].LastResolvedAt == nil {
		t.Error("LastResolvedAt was not set after a successful resolve")
	}
}

func TestFailedResolveIsRecordedWithoutLeakingDetail(t *testing.T) {
	rt, h := testRouter(t, alwaysLive(NewResolver(&stubSearcher{
		err: &aiostreams.SearchError{Kind: aiostreams.KindTransient},
	})))
	rt.library.Add(Placeholder{Path: "/library/Movies/A/A.strm", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/resolve/movie/tmdb:603?imdb=tt0133093", nil))

	entries := rt.recorder.Snapshot()
	if len(entries) != 1 || entries[0].Error == "" {
		t.Fatalf("expected one recorded failure, got %+v", entries)
	}
	if entries[0].Error != "provider unavailable" {
		t.Errorf("recorded error = %q, want a fixed short reason", entries[0].Error)
	}
	if items := rt.library.List(); items[0].LastError == "" {
		t.Error("placeholder was not marked failed")
	}
}

func TestRecorderIsBounded(t *testing.T) {
	r := NewRecorder()
	for i := 0; i < maxActivity+25; i++ {
		r.Record(Activity{At: time.Now(), TotalMS: int64(i)})
	}
	if got := len(r.Snapshot()); got != maxActivity {
		t.Errorf("kept %d entries, want the log capped at %d", got, maxActivity)
	}
}

func TestRecorderMedian(t *testing.T) {
	r := NewRecorder()
	for _, ms := range []int64{100, 200, 300, 400, 500} {
		r.Record(Activity{TotalMS: ms})
	}
	resolved, reused, failures, median := r.Stats()
	if resolved != 5 || reused != 0 || failures != 0 {
		t.Errorf("resolved=%d reused=%d failures=%d, want 5/0/0", resolved, reused, failures)
	}
	if median != 300 {
		t.Errorf("median = %d, want 300", median)
	}
}

// A reused answer took no time because it did no work. Folding its zeros into
// the median would let a seek-happy session mask a genuinely slow provider.
func TestRecorderKeepsReusedAnswersOutOfTheMedian(t *testing.T) {
	r := NewRecorder()
	r.Record(Activity{TotalMS: 400})
	for i := 0; i < 10; i++ {
		r.Record(Activity{TotalMS: 0, Reused: true})
	}
	resolved, reused, _, median := r.Stats()
	if resolved != 11 || reused != 10 {
		t.Errorf("resolved=%d reused=%d, want 11/10", resolved, reused)
	}
	if median != 400 {
		t.Errorf("median = %d, want the one full resolution's 400", median)
	}
}

// Re-adding a path must not wipe play history: Wisp rewrites placeholders when
// quality preferences change, and losing the record would make the dashboard
// lie about what has ever played.
func TestLibraryAddPreservesHistory(t *testing.T) {
	l := NewLibrary()
	l.Add(Placeholder{Path: "/a.strm", ID: MediaID{SourceTMDB, "1"}, Quality: "1080p"})
	l.MarkResolved(MediaID{SourceTMDB, "1"}, 0, 0)
	l.Add(Placeholder{Path: "/a.strm", ID: MediaID{SourceTMDB, "1"}, Quality: "2160p"})

	items := l.List()
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Plays != 1 {
		t.Errorf("plays = %d, want history preserved", items[0].Plays)
	}
	if items[0].Quality != "2160p" {
		t.Errorf("quality = %q, want the updated value", items[0].Quality)
	}
}

// The dashboard must load before any configuration exists. Gating it behind a
// configured resolver makes the page that explains what to set up unreachable
// until you have already set it up — which is exactly backwards.
func TestDashboardLoadsBeforeConfiguration(t *testing.T) {
	rt := NewRouterWith(RouterOptions{Log: slog.New(slog.DiscardHandler)}) // no resolver
	h := rt.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200 while unconfigured", rec.Code)
	}

	status := httptest.NewRecorder()
	h.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))
	if status.Code != http.StatusOK {
		t.Fatalf("status endpoint = %d, want 200 while unconfigured", status.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// It must say so plainly rather than pretending to be healthy.
	if body["resolver_ready"] != false {
		t.Errorf("resolver_ready = %v, want false while unconfigured", body["resolver_ready"])
	}
}

// The page has to tell an operator what to do about it.
func TestDashboardExplainsHowToConfigure(t *testing.T) {
	rt := NewRouterWith(RouterOptions{Log: slog.New(slog.DiscardHandler)})

	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))

	body := rec.Body.String()
	for _, want := range []string{"Not configured", "AIOStreams URL", "library path"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard does not mention %q", want)
		}
	}
}

// A pass that finds nothing looks exactly like a monitor that never runs, and
// the difference only shows up weeks later as episodes quietly missing. The
// dashboard has to be able to tell them apart.
func TestStatusReportsWhetherEpisodesHaveEverBeenChecked(t *testing.T) {
	holder := NewMonitorHolder()
	rt := NewRouterWith(RouterOptions{
		Library: NewLibrary(), Recorder: NewRecorder(), Monitor: holder,
	})

	get := func() map[string]any {
		rec := httptest.NewRecorder()
		rt.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	m, ok := get()["monitor"].(map[string]any)
	if !ok {
		t.Fatal("status carries no monitor section")
	}
	if m["has_run"] != false {
		t.Errorf("has_run = %v before any pass; want false", m["has_run"])
	}

	// A configured monitor over an empty library has still genuinely run: it
	// looked and found nothing, which is the normal case and must be
	// distinguishable from never looking.
	holder.Set(NewMonitor(
		NewLibrary(),
		NewWriter(t.TempDir(), "http://127.0.0.1:8080/api/v1/plugins/1", nil),
		&stubEpisodes{},
		nil,
	))
	if _, err := holder.Run(context.Background(), &pluginv1.RunScheduledTaskRequest{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	m, _ = get()["monitor"].(map[string]any)
	if m["has_run"] != true {
		t.Errorf("has_run = %v after a pass; want true", m["has_run"])
	}
	if _, present := m["last_run_at"]; !present {
		t.Error("no last_run_at after a pass; the dashboard cannot say when it checked")
	}
}

// The placeholder list is unbounded in principle and the dashboard polls it.
// Sending everything meant 143KB per poll at 484 placeholders — megabytes a
// minute for one open tab, spent on rows nobody scrolled to.
func TestPlaceholdersArePagedWithATotal(t *testing.T) {
	lib := NewLibrary()
	for i := 0; i < defaultPlaceholderPage*3; i++ {
		lib.Add(Placeholder{
			Path:      "/library/movies/T" + strconv.Itoa(i) + "/T.strm",
			MediaType: "movie", ID: MediaID{SourceTMDB, strconv.Itoa(i + 1)},
		})
	}
	rt := NewRouterWith(RouterOptions{Library: lib, Recorder: NewRecorder()})

	get := func(query string) map[string]any {
		rec := httptest.NewRecorder()
		rt.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/placeholders"+query, nil))
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	body := get("")
	items, _ := body["items"].([]any)
	if len(items) != defaultPlaceholderPage {
		t.Errorf("returned %d items by default, want %d", len(items), defaultPlaceholderPage)
	}
	// The total must still be reported, or the page cannot say what it is showing.
	if total, _ := body["total"].(float64); int(total) != defaultPlaceholderPage*3 {
		t.Errorf("total = %v, want %d", body["total"], defaultPlaceholderPage*3)
	}

	// A caller may ask for fewer.
	if items, _ := get("?limit=5")["items"].([]any); len(items) != 5 {
		t.Errorf("limit=5 returned %d items", len(items))
	}

	// But not for an unbounded amount: a hand-written limit must not turn the
	// dashboard into a memory spike.
	if items, _ := get("?limit=100000")["items"].([]any); len(items) > maxPlaceholderPage {
		t.Errorf("limit=100000 returned %d items, want at most %d", len(items), maxPlaceholderPage)
	}

	// Nonsense falls back to the default rather than erroring or returning zero.
	if items, _ := get("?limit=abc")["items"].([]any); len(items) != defaultPlaceholderPage {
		t.Errorf("limit=abc returned %d items, want the default", len(items))
	}
}
