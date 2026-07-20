package plugin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	rt, h := testRouter(t, NewResolver(&stubSearcher{}))
	rt.library.Add(Placeholder{Path: "/library/Movies/A/A.strm", IMDbID: "tt1"})

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
	rt, h := testRouter(t, NewResolver(&stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn/a.mkv", Resolution: "1080p", Filename: "A.1080p.mkv"},
	}}))
	rt.library.Add(Placeholder{Path: "/library/Movies/A/A.strm", IMDbID: "tt0133093"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/resolve/movie/tt0133093", nil))
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
	rt, h := testRouter(t, NewResolver(&stubSearcher{
		err: &aiostreams.SearchError{Kind: aiostreams.KindTransient},
	}))
	rt.library.Add(Placeholder{Path: "/library/Movies/A/A.strm", IMDbID: "tt0133093"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/resolve/movie/tt0133093", nil))

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
	resolved, failures, median := r.Stats()
	if resolved != 5 || failures != 0 {
		t.Errorf("resolved=%d failures=%d, want 5/0", resolved, failures)
	}
	if median != 300 {
		t.Errorf("median = %d, want 300", median)
	}
}

// Re-adding a path must not wipe play history: Wisp rewrites placeholders when
// quality preferences change, and losing the record would make the dashboard
// lie about what has ever played.
func TestLibraryAddPreservesHistory(t *testing.T) {
	l := NewLibrary()
	l.Add(Placeholder{Path: "/a.strm", IMDbID: "tt1", Quality: "1080p"})
	l.MarkResolved("tt1", 0, 0)
	l.Add(Placeholder{Path: "/a.strm", IMDbID: "tt1", Quality: "2160p"})

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
