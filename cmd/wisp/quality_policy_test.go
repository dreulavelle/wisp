package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dreulavelle/wisp/internal/aiostreams"
	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/store"
)

// defaultPolicy is what wisp ships with: a 1080p floor and no 4K.
var defaultPolicy = library.QualityPolicy{Min: "1080p"}

// decodeErrorBody pulls the structured {error, message} contract out of a
// response, failing the test if it isn't there.
func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not JSON: %q", rec.Body.String())
	}
	return body
}

// Every intake path filters requested tiers through the policy before a monitor
// is created, so unsatisfiable work is never stored rather than filtered later.
func TestIntakeAppliesQualityPolicy(t *testing.T) {
	cases := []struct {
		name          string
		qualities     []string // as the caller sends them
		wantQualities []string // as stored on the monitor
	}{
		{name: "4K is dropped", qualities: []string{"1080p", "2160p"}, wantQualities: []string{"1080p"}},
		{name: "below the minimum is raised", qualities: []string{"720p"}, wantQualities: []string{"1080p"}},
		{name: "mixed list collapses", qualities: []string{"720p", "1080p", "4k"}, wantQualities: []string{"1080p"}},
		{name: "empty stays best-available", qualities: []string{}, wantQualities: nil},
	}

	for _, tc := range cases {
		// POST /api/monitors
		t.Run("monitors/"+tc.name, func(t *testing.T) {
			a := testApp(t)
			a.quality = defaultPolicy
			body, _ := json.Marshal(monitorRequest{
				MediaType: "movie", IMDbID: "tt1", Title: "Demo", Qualities: tc.qualities,
			})
			rec := httptest.NewRecorder()
			a.handleCreateMonitor(rec, httptest.NewRequest(http.MethodPost, "/api/monitors", strings.NewReader(string(body))))
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d (%s), want 201", rec.Code, rec.Body.String())
			}
			assertStoredQualities(t, a, tc.wantQualities)
		})

		// POST /api/add (request-shaped)
		t.Run("add/"+tc.name, func(t *testing.T) {
			a := offlineApp(t)
			a.quality = defaultPolicy
			specs := make([]qualitySpec, 0, len(tc.qualities))
			for _, q := range tc.qualities {
				specs = append(specs, qualitySpec{ID: q})
			}
			body, _ := json.Marshal(map[string]any{
				"media_type": "movie", "imdb_id": "tt1", "title": "Demo", "qualities": specs,
			})
			rec := httptest.NewRecorder()
			a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(string(body))))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d (%s), want 202", rec.Code, rec.Body.String())
			}
			assertStoredQualities(t, a, tc.wantQualities)
		})
	}
}

func assertStoredQualities(t *testing.T, a *app, want []string) {
	t.Helper()
	items, _ := a.store.ListMonitored(context.Background())
	if len(items) != 1 {
		t.Fatalf("monitors = %d, want 1", len(items))
	}
	got := items[0].Qualities
	if len(got) != len(want) {
		t.Fatalf("stored qualities = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("stored qualities = %v, want %v", got, want)
		}
	}
}

// A request whose only tier is disallowed is refused outright, with a response a
// caller can classify as permanent: a 4xx (never 5xx, never 429, no Retry-After)
// carrying the quality_not_allowed code. No monitor is created.
func TestIntakeRejects4KOnlyRequest(t *testing.T) {
	t.Run("POST /api/monitors", func(t *testing.T) {
		a := testApp(t)
		a.quality = defaultPolicy
		rec := httptest.NewRecorder()
		a.handleCreateMonitor(rec, httptest.NewRequest(http.MethodPost, "/api/monitors",
			strings.NewReader(`{"media_type":"movie","imdb_id":"tt1","title":"Demo","qualities":["2160p"]}`)))
		assertPermanentQualityRejection(t, rec)
		if items, _ := a.store.ListMonitored(context.Background()); len(items) != 0 {
			t.Fatalf("a rejected request created a monitor: %#v", items)
		}
	})

	t.Run("POST /api/add request-shaped", func(t *testing.T) {
		a := offlineApp(t)
		a.quality = defaultPolicy
		rec := httptest.NewRecorder()
		a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add",
			strings.NewReader(`{"media_type":"movie","imdb_id":"tt1","title":"Demo","qualities":[{"id":"4k","is4k":true}]}`)))
		assertPermanentQualityRejection(t, rec)
		if items, _ := a.store.ListMonitored(context.Background()); len(items) != 0 {
			t.Fatalf("a rejected request created a monitor: %#v", items)
		}
	})

	t.Run("POST /api/add legacy direct pin", func(t *testing.T) {
		backend := wispTestBackend(t)
		defer backend.Close()
		a := offlineApp(t)
		a.quality = defaultPolicy
		a.aio = aiostreams.New(backend.URL+"/stremio/uuid/blob/manifest.json", "pw")

		rec := httptest.NewRecorder()
		a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(
			`{"media_type":"series","imdb_id":"tt7","title":"Demo","year":2026,"season":1,"episode":1,"quality":"2160p","tmdb_id":"555"}`)))
		assertPermanentQualityRejection(t, rec)
		// Nothing was scraped or pinned: "4K off" means wisp never fetches it.
		if n, _ := a.store.Count(context.Background()); n != 0 {
			t.Fatalf("a rejected direct pin stored %d pins", n)
		}
	})
}

func assertPermanentQualityRejection(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if rec.Code >= 500 || rec.Code == http.StatusTooManyRequests {
		t.Fatalf("status %d reads as transient; a policy rejection must be permanent", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Fatalf("Retry-After = %q; a permanent rejection must not invite a retry", ra)
	}
	body := decodeErrorBody(t, rec)
	if body["error"] != errCodeQualityNotAllowed {
		t.Fatalf("error code = %v, want %q", body["error"], errCodeQualityNotAllowed)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "WISP_ALLOW_2160P") {
		t.Fatalf("message = %q; it should name the setting that would enable 4K", msg)
	}
}

// A direct pin below the floor is raised rather than refused, and the pin lands
// at the raised tier.
func TestLegacyDirectPinRaisesBelowMinimum(t *testing.T) {
	backend := wispTestBackend(t)
	defer backend.Close()
	a := offlineApp(t)
	a.quality = defaultPolicy
	a.aio = aiostreams.New(backend.URL+"/stremio/uuid/blob/manifest.json", "pw")

	rec := httptest.NewRecorder()
	a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(
		`{"media_type":"series","imdb_id":"tt7","title":"Demo","year":2026,"season":1,"episode":1,"quality":"720p","tmdb_id":"555"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if vp, _ := resp["virtual_path"].(string); !strings.Contains(vp, "[1080p]") {
		t.Fatalf("virtual_path = %q, want the raised 1080p tier", vp)
	}
}

// Re-requesting a title must not smuggle a disallowed tier back in through
// Intake's union with the stored list.
func TestReRequestCannotReintroduceDisallowedTier(t *testing.T) {
	a := testApp(t)
	a.quality = library.QualityPolicy{Min: "1080p", Allow2160p: true}

	post := func(qualities string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.handleCreateMonitor(rec, httptest.NewRequest(http.MethodPost, "/api/monitors",
			strings.NewReader(`{"media_type":"movie","imdb_id":"tt1","title":"Demo","qualities":`+qualities+`}`)))
		return rec
	}
	if rec := post(`["1080p","2160p"]`); rec.Code != http.StatusCreated {
		t.Fatalf("seed status = %d", rec.Code)
	}

	// The operator turns 4K off. The startup migration strips the stored tier...
	a.quality = defaultPolicy
	n, failed, err := a.store.ApplyQualityPolicy(context.Background(), a.quality)
	if err != nil || n != 1 || len(failed) != 0 {
		t.Fatalf("migration = (%d, %v, %v), want (1, [], nil)", n, failed, err)
	}
	// ...and a re-request cannot union it back on.
	if rec := post(`["1080p","2160p"]`); rec.Code != http.StatusCreated {
		t.Fatalf("re-request status = %d", rec.Code)
	}
	assertStoredQualities(t, a, []string{"1080p"})
}

// The startup migration brings stored monitors in line so a disallowed tier
// stops being scraped, and closes out a monitor left with nothing to chase.
func TestApplyQualityPolicyMigration(t *testing.T) {
	ctx := context.Background()
	st := newPolicyTestStore(t)

	seed := []store.Monitored{
		{Key: "series:tt1", MediaType: "series", IMDbID: "tt1", Enabled: true,
			Qualities: []string{"1080p", "2160p"}, PendingAired: 12,
			PendingByTier: map[string]int{"2160p": 10, "1080p": 2},
			TierBackoff:   map[string]store.TierBackoffState{"2160p": {Misses: 8}}},
		{Key: "movie:tt2", MediaType: "movie", IMDbID: "tt2", Enabled: true, Qualities: []string{"2160p"}},
		{Key: "movie:tt3", MediaType: "movie", IMDbID: "tt3", Enabled: true, Qualities: []string{"1080p"}},
		{Key: "movie:tt4", MediaType: "movie", IMDbID: "tt4", Enabled: true, Qualities: nil},
		{Key: "movie:tt5", MediaType: "movie", IMDbID: "tt5", Enabled: true, Qualities: []string{"2160p"}, Completed: true},
	}
	for _, m := range seed {
		if err := st.PutMonitored(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	n, failed, err := st.ApplyQualityPolicy(ctx, defaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("monitors updated = %d, want 2 (tt1 rewritten, tt2 failed)", n)
	}
	if len(failed) != 1 || failed[0] != "movie:tt2" {
		t.Fatalf("failed keys = %v, want [movie:tt2]", failed)
	}

	byKey := map[string]store.Monitored{}
	items, _ := st.ListMonitored(ctx)
	for _, it := range items {
		byKey[it.Key] = it
	}

	// The mixed series keeps 1080p and loses every trace of 2160p, including the
	// pending work and backoff state that would otherwise block its completion.
	tt1 := byKey["series:tt1"]
	if len(tt1.Qualities) != 1 || tt1.Qualities[0] != "1080p" {
		t.Fatalf("tt1 qualities = %v, want [1080p]", tt1.Qualities)
	}
	if tt1.PendingAired != 2 {
		t.Fatalf("tt1 PendingAired = %d, want 2 (the 10 2160p units dropped)", tt1.PendingAired)
	}
	if _, ok := tt1.PendingByTier["2160p"]; ok {
		t.Fatal("tt1 kept pending work for the removed tier")
	}
	if _, ok := tt1.TierBackoff["2160p"]; ok {
		t.Fatal("tt1 kept backoff state for the removed tier")
	}
	if tt1.Failed {
		t.Fatal("tt1 still has an allowed tier and must not be failed")
	}

	// The 4K-only monitor is closed out, not silently downgraded to best-available.
	tt2 := byKey["movie:tt2"]
	if !tt2.Failed {
		t.Fatal("tt2 requested only a disallowed tier and must be marked failed")
	}
	if len(tt2.Qualities) != 1 || tt2.Qualities[0] != "2160p" {
		t.Fatalf("tt2 qualities = %v; a failed monitor keeps the request it was made with", tt2.Qualities)
	}
	if !strings.Contains(tt2.LastError, "quality policy") {
		t.Fatalf("tt2 LastError = %q; it should explain the policy rejection", tt2.LastError)
	}

	// Compliant, unconstrained, and already-completed monitors are untouched.
	if byKey["movie:tt3"].Failed || len(byKey["movie:tt3"].Qualities) != 1 {
		t.Fatalf("tt3 = %#v, want untouched", byKey["movie:tt3"])
	}
	if byKey["movie:tt4"].Failed || byKey["movie:tt4"].Qualities != nil {
		t.Fatalf("tt4 (best available) = %#v, want untouched", byKey["movie:tt4"])
	}
	if byKey["movie:tt5"].Failed {
		t.Fatal("tt5 already completed and must not be failed retroactively")
	}

	// Idempotent: a second run is a no-op.
	n2, failed2, err := st.ApplyQualityPolicy(ctx, defaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 || len(failed2) != 0 {
		t.Fatalf("second run = (%d, %v), want (0, [])", n2, failed2)
	}
}

func newPolicyTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
