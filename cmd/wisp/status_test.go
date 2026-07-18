package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/monitor"
	"github.com/dreulavelle/wisp/internal/notify"
	"github.com/dreulavelle/wisp/internal/seerr"
	"github.com/dreulavelle/wisp/internal/store"
)

// offlineApp builds an app whose metadata heuristic points at a stub Cinemeta
// that reports no genres, so intake never touches the real network.
func offlineApp(t *testing.T) *app {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{}}`))
	}))
	t.Cleanup(stub.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.DiscardHandler)
	a := &app{
		store: st, log: log, startedAt: time.Now(),
		meta:    metadata.New("", nil, metadata.WithBaseURLs(stub.URL, stub.URL, stub.URL)),
		seerr:   seerr.New("", ""),
		webhook: notify.New(notify.Options{}, log),
	}
	a.mon = monitor.New(st, a.meta, a, time.Hour, log)
	return a
}

// A request-shaped /api/add registers a monitor (async), returns 202, maps the
// qualities array, records the request_ref, and is idempotent per title.
func TestHandleAddRequestShapedIntake(t *testing.T) {
	a := offlineApp(t)
	body := `{"media_type":"series","imdb_id":"tt7","title":"Show","year":2026,
		"qualities":[{"id":"1080p"},{"id":"4k","is4k":true}],"request_ref":"silo-42"}`

	rec := httptest.NewRecorder()
	a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	items, _ := a.store.ListMonitored(context.Background())
	if len(items) != 1 {
		t.Fatalf("monitors = %d, want 1", len(items))
	}
	it := items[0]
	if it.RequestRef != "silo-42" {
		t.Fatalf("request_ref = %q", it.RequestRef)
	}
	if len(it.Qualities) != 2 || it.Qualities[0] != "1080p" || it.Qualities[1] != "2160p" {
		t.Fatalf("qualities = %v, want [1080p 2160p]", it.Qualities)
	}
	if it.Category != "shows" { // stub cinemeta → non-anime
		t.Fatalf("category = %q, want shows", it.Category)
	}
	if n, _ := a.store.Count(context.Background()); n != 0 {
		t.Fatalf("request-shaped add pinned synchronously: %d pins", n)
	}

	// Idempotent: re-posting the same title extends, not duplicates.
	rec = httptest.NewRecorder()
	a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("second add status = %d", rec.Code)
	}
	if items, _ := a.store.ListMonitored(context.Background()); len(items) != 1 {
		t.Fatalf("monitors after re-add = %d, want 1 (idempotent)", len(items))
	}
}

// The explicit is_anime flag routes a request-shaped add into the anime root.
func TestHandleAddRequestShapedAnime(t *testing.T) {
	a := offlineApp(t)
	body := `{"media_type":"movie","tmdb_id":"603","imdb_id":"tt6","title":"Akira","is_anime":true}`
	rec := httptest.NewRecorder()
	a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	items, _ := a.store.ListMonitored(context.Background())
	if len(items) != 1 || items[0].Category != "anime_movies" {
		t.Fatalf("monitored = %#v, want anime_movies", items)
	}
}

// A legacy direct-pin payload (imdb + season/episode/quality) still resolves and
// pins synchronously, under the non-anime root — byte-identical to before.
func TestHandleAddLegacyStillPinsSynchronously(t *testing.T) {
	backend := wispTestBackend(t)
	defer backend.Close()

	a := offlineApp(t)
	a.aio = aiostreams.New(backend.URL+"/stremio/uuid/blob/manifest.json", "pw")

	body := `{"media_type":"series","imdb_id":"tt7","title":"Demo","year":2026,
		"season":1,"episode":1,"quality":"1080p","tmdb_id":"555"}`
	rec := httptest.NewRecorder()
	a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy add status = %d, want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	vp, _ := resp["virtual_path"].(string)
	want := "shows/Demo (2026) [tmdb-555]/Season 01/Demo (2026) - S01E01 - [1080p].mkv"
	if vp != want {
		t.Fatalf("virtual_path = %q, want %q", vp, want)
	}
	if items, _ := a.store.ListMonitored(context.Background()); len(items) != 0 {
		t.Fatalf("legacy add created a monitor: %#v", items)
	}
}

// computeRequestStatus covers the mapping table without HTTP or network.
func TestComputeRequestStatus(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	future := now.Add(48 * time.Hour)
	servable := store.Pin{MediaType: "movie", Quality: "1080p", VirtualPath: "movies/x", SourceURL: "http://a", Size: 1}
	unservable := store.Pin{MediaType: "movie", Quality: "1080p", VirtualPath: "movies/x", SourceURL: "", Size: 0}

	cases := []struct {
		name       string
		mon        *store.Monitored
		pins       []store.Pin
		mediaType  string
		wantState  string
		wantDetail string
	}{
		{
			name:      "movie unreleased is queued, never failed",
			mon:       &store.Monitored{MediaType: "movie", DueAt: future, DueReason: store.DueReasonRelease},
			mediaType: "movie", wantState: statusQueued, wantDetail: "awaiting home-media release",
		},
		{
			name:      "movie released, no stream yet, retry window is queued",
			mon:       &store.Monitored{MediaType: "movie", DueAt: now.Add(-time.Hour), DueReason: store.DueReasonRetry, LastChecked: now},
			mediaType: "movie", wantState: statusQueued, wantDetail: "resolving stream",
		},
		{
			name:      "movie pinned is completed",
			mon:       &store.Monitored{MediaType: "movie", LastChecked: now},
			pins:      []store.Pin{servable},
			mediaType: "movie", wantState: statusCompleted,
		},
		{
			name:      "movie with only an unservable pin stays queued",
			mon:       &store.Monitored{MediaType: "movie", LastChecked: now},
			pins:      []store.Pin{unservable},
			mediaType: "movie", wantState: statusQueued,
		},
		{
			name:      "series unaired (checked, no pins) is queued",
			mon:       &store.Monitored{MediaType: "series", LastChecked: now, PendingAired: 0, DueAt: future, DueReason: store.DueReasonAirstamp},
			mediaType: "series", wantState: statusQueued, wantDetail: "awaiting next episode airing",
		},
		{
			name:      "series all aired episodes pinned is completed",
			mon:       &store.Monitored{MediaType: "series", LastChecked: now, PendingAired: 0},
			pins:      []store.Pin{{MediaType: "series", Quality: "1080p", VirtualPath: "shows/x", SourceURL: "http://a", Size: 1}},
			mediaType: "series", wantState: statusCompleted,
		},
		{
			name:      "series still catching up is queued",
			mon:       &store.Monitored{MediaType: "series", LastChecked: now, PendingAired: 2},
			pins:      []store.Pin{{MediaType: "series", Quality: "1080p", VirtualPath: "shows/x", SourceURL: "http://a", Size: 1}},
			mediaType: "series", wantState: statusQueued,
		},
		{
			name:      "permanent give-up is failed",
			mon:       &store.Monitored{MediaType: "series", Failed: true, LastError: "unresolvable identity"},
			mediaType: "series", wantState: statusFailed, wantDetail: "unresolvable identity",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeRequestStatus(tc.mon, tc.pins, tc.mediaType, now)
			if got.State != tc.wantState {
				t.Fatalf("state = %q, want %q", got.State, tc.wantState)
			}
			if tc.wantDetail != "" && got.Detail != tc.wantDetail {
				t.Fatalf("detail = %q, want %q", got.Detail, tc.wantDetail)
			}
		})
	}
}

// The HTTP endpoint round-trips: 404 for an untracked title, 200 + mapped state
// for a tracked one, matched by tmdb_id.
func TestHandleRequestStatusHTTP(t *testing.T) {
	a := offlineApp(t)
	ctx := context.Background()

	// Untracked → 404.
	rec := httptest.NewRecorder()
	a.handleRequestStatus(rec, httptest.NewRequest(http.MethodGet, "/api/requests/status?media_type=movie&tmdb_id=999", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("untracked status = %d, want 404", rec.Code)
	}

	// A monitored, completed movie matched by tmdb_id.
	_ = a.store.PutMonitored(ctx, store.Monitored{
		Key: "movie:tt6", MediaType: "movie", IMDbID: "tt6", TMDbID: "603",
		Category: "movies", LastChecked: time.Now(), RequestRef: "silo-7",
	})
	_ = a.store.Upsert(ctx, store.Pin{
		MediaType: "movie", IMDbID: "tt6", TMDbID: "603", Quality: "2160p",
		VirtualPath: "movies/Akira (1988)/a.mkv", SourceURL: "http://a", Size: 10,
	})

	rec = httptest.NewRecorder()
	a.handleRequestStatus(rec, httptest.NewRequest(http.MethodGet, "/api/requests/status?media_type=movie&tmdb_id=603", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp requestStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.State != statusCompleted {
		t.Fatalf("state = %q, want completed", resp.State)
	}
	if len(resp.PinnedQualities) != 1 || resp.PinnedQualities[0] != "2160p" {
		t.Fatalf("pinned_qualities = %v", resp.PinnedQualities)
	}
	if resp.RequestRef != "silo-7" {
		t.Fatalf("request_ref = %q", resp.RequestRef)
	}
}
