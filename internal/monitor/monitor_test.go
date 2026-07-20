package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/store"
)

// fakeFul records pin calls and models existing pins + unavailable episodes.
// Pin is now invoked concurrently (bounded per-episode fan-out), so all mutable
// state is guarded by mu.
type fakeFul struct {
	mu        sync.Mutex
	pinned    map[PinKey]bool
	noStream  map[[2]int]bool // (season,episode) with no playable stream → NoResults
	noQuality map[string]bool // canonical quality absent for the whole title → NoQualityMatch
	failEp    map[[2]int]bool // (season,episode) whose Pin is a genuine fault
	calls     int
}

func newFakeFul() *fakeFul {
	return &fakeFul{
		pinned:    map[PinKey]bool{},
		noStream:  map[[2]int]bool{},
		noQuality: map[string]bool{},
		failEp:    map[[2]int]bool{},
	}
}

func (f *fakeFul) Pin(_ context.Context, t Target) (PinOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	q := library.NormalizeQuality(t.Quality)
	if f.failEp[[2]int{t.Season, t.Episode}] {
		return NoResults, errors.New("resolver fault")
	}
	if f.noQuality[q] {
		return NoQualityMatch, nil // results exist, but not at this resolution
	}
	if f.noStream[[2]int{t.Season, t.Episode}] {
		return NoResults, nil
	}
	f.pinned[PinKey{t.Season, t.Episode, q}] = true
	return Pinned, nil
}

func (f *fakeFul) PinnedKeys(_ context.Context, _ string) (map[PinKey]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[PinKey]bool, len(f.pinned))
	for k := range f.pinned {
		out[k] = true
	}
	return out, nil
}

func date(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testMonitor(t *testing.T, mux *http.ServeMux, ful Fulfiller, now time.Time) (*Monitor, *store.Store) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	meta := metadata.New("v3key", []string{"US"}, metadata.WithBaseURLs(srv.URL, srv.URL, srv.URL))
	st := newStore(t)
	m := New(st, meta, ful, time.Hour, 4, 7*24*time.Hour, slog.New(slog.DiscardHandler))
	m.now = func() time.Time { return now }
	return m, st
}

func TestMonitorPinsReleasedMovieAndMarksComplete(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/movie/500/release_dates", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"results":[{"iso_3166_1":"US","release_dates":[{"type":4,"release_date":"2026-01-01T00:00:00Z"}]}]}`))
	})
	ful := newFakeFul()
	now := date("2026-06-01T00:00:00Z")
	m, st := testMonitor(t, mux, ful, now)

	if err := m.Intake(context.Background(), Request{MediaType: "movie", IMDbID: "tt5", TMDbID: "500", Title: "Film", Year: 2026}); err != nil {
		t.Fatal(err)
	}
	m.checkDue(context.Background()) // released → pin + mark complete (kept for history)

	if ful.calls != 1 || len(ful.pinned) != 1 {
		t.Fatalf("expected 1 pin, got calls=%d pinned=%d", ful.calls, len(ful.pinned))
	}
	got, _ := st.GetMonitored(context.Background(), "movie:tt5")
	if got == nil || !got.Completed {
		t.Fatalf("movie should be kept and marked completed; got %#v", got)
	}
	// A completed movie is not reprocessed.
	before := ful.calls
	m.checkDue(context.Background())
	if ful.calls != before {
		t.Fatalf("completed movie reprocessed: calls %d → %d", before, ful.calls)
	}
}

func TestMonitorDefersUnreleasedMovie(t *testing.T) {
	release := "2026-12-25T00:00:00Z"
	mux := http.NewServeMux()
	mux.HandleFunc("/movie/500/release_dates", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"results":[{"iso_3166_1":"US","release_dates":[{"type":4,"release_date":"` + release + `"}]}]}`))
	})
	ful := newFakeFul()
	now := date("2026-06-01T00:00:00Z")
	m, st := testMonitor(t, mux, ful, now)

	m.Intake(context.Background(), Request{MediaType: "movie", IMDbID: "tt5", TMDbID: "500", Title: "Film"})
	next := m.checkDue(context.Background())

	if ful.calls != 0 {
		t.Fatalf("unreleased movie should not pin; calls=%d", ful.calls)
	}
	if !next.Equal(date(release)) {
		t.Fatalf("next wake = %v, want the release date %s", next, release)
	}
	if n, _ := st.CountMonitored(context.Background()); n != 1 {
		t.Fatalf("unreleased movie should stay monitored; monitored=%d", n)
	}
}

func TestMonitorPinsAiredEpisodesAndSchedulesNextAir(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meta/series/tt7.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{"videos":[
			{"season":1,"episode":1,"released":"2026-01-01T00:00:00Z"},
			{"season":1,"episode":2,"released":"2026-01-08T00:00:00Z"},
			{"season":1,"episode":3,"released":"2026-12-01T00:00:00Z"}
		]}}`))
	})
	mux.HandleFunc("/lookup/shows", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":0}`)) })
	ful := newFakeFul()
	now := date("2026-02-01T00:00:00Z")
	m, _ := testMonitor(t, mux, ful, now)

	m.Intake(context.Background(), Request{MediaType: "series", IMDbID: "tt7", Title: "Show", Qualities: []string{"1080p", "2160p"}})
	next := m.checkDue(context.Background())

	// Episodes 1 & 2 aired (2 qualities each = 4 pins); episode 3 is future.
	if len(ful.pinned) != 4 {
		t.Fatalf("expected 4 pins (E1+E2 at 2 tiers), got %d", len(ful.pinned))
	}
	if !next.Equal(date("2026-12-01T00:00:00Z")) {
		t.Fatalf("next wake = %v, want the next airstamp 2026-12-01", next)
	}
	// Second pass must not re-pin already-pinned episodes.
	before := ful.calls
	m.now = func() time.Time { return date("2026-02-02T00:00:00Z") }
	m.checkDue(context.Background())
	if ful.calls != before {
		t.Fatalf("re-pinned already-pinned episodes: calls %d → %d", before, ful.calls)
	}
}

func TestMonitorRecoversFromStore(t *testing.T) {
	// An item persisted before "restart" is picked up by a fresh Monitor.
	mux := http.NewServeMux()
	mux.HandleFunc("/movie/500/release_dates", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"results":[{"iso_3166_1":"US","release_dates":[{"type":4,"release_date":"2026-01-01T00:00:00Z"}]}]}`))
	})
	ful := newFakeFul()
	now := date("2026-06-01T00:00:00Z")
	m, st := testMonitor(t, mux, ful, now)
	st.PutMonitored(context.Background(), store.Monitored{
		Key: "movie:tt5", MediaType: "movie", IMDbID: "tt5", TMDbID: "500", Title: "Film", DueAt: now, Enabled: true,
	})

	m.checkDue(context.Background()) // fresh monitor, item loaded from store
	if len(ful.pinned) != 1 {
		t.Fatalf("recovered item not processed; pinned=%d", len(ful.pinned))
	}
}

// A just-aired episode whose stream hasn't appeared must be retried at the
// interval, not deferred to the next (possibly distant) airstamp.
func TestMonitorRetriesUnavailableAiredEpisode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meta/series/tt7.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{"videos":[
			{"season":1,"episode":1,"released":"2026-01-01T00:00:00Z"},
			{"season":1,"episode":2,"released":"2027-06-01T00:00:00Z"}
		]}}`))
	})
	mux.HandleFunc("/lookup/shows", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":0}`)) })
	ful := newFakeFul()
	ful.noStream[[2]int{1, 1}] = true // E1 aired but no stream yet
	now := date("2026-02-01T00:00:00Z")
	m, st := testMonitor(t, mux, ful, now) // interval = 1h

	m.Intake(context.Background(), Request{MediaType: "series", IMDbID: "tt7", Title: "Show", Qualities: []string{"1080p"}})
	m.checkDue(context.Background())

	item, _ := st.GetMonitored(context.Background(), "series:tt7")
	if want := now.Add(time.Hour); !item.DueAt.Equal(want) {
		t.Fatalf("DueAt = %v, want interval retry %v (not the far E2 airstamp)", item.DueAt, want)
	}
}

// persistResult must not clobber a concurrent Intake's changes with a stale
// scheduler snapshot.
func TestPersistResultRespectsConcurrentIntake(t *testing.T) {
	ctx := context.Background()
	m, st := testMonitor(t, http.NewServeMux(), newFakeFul(), date("2026-02-01T00:00:00Z"))
	now := m.now()
	st.PutMonitored(ctx, store.Monitored{Key: "series:tt7", MediaType: "series", IMDbID: "tt7", Enabled: true, Qualities: []string{"1080p"}, DueAt: now})
	snapshot, _ := st.GetMonitored(ctx, "series:tt7")

	// Concurrent Intake: adds a 4K tier and demands reprocessing (DueAt=now).
	st.PutMonitored(ctx, store.Monitored{Key: "series:tt7", MediaType: "series", IMDbID: "tt7", Enabled: true, Qualities: []string{"1080p", "2160p"}, DueAt: now})

	// Scheduler finishes its pass on the STALE snapshot, computing a far DueAt.
	m.persistResult(ctx, *snapshot, passResult{due: now.Add(100 * time.Hour), reason: store.DueReasonRetry})

	cur, _ := st.GetMonitored(ctx, "series:tt7")
	if len(cur.Qualities) != 2 {
		t.Fatalf("concurrent Intake's qualities clobbered: %v", cur.Qualities)
	}
	if cur.DueAt.Equal(now.Add(100 * time.Hour)) {
		t.Fatal("scheduler clobbered the re-request's DueAt with its stale far-future value")
	}
}

// A forced refresh must process an item whose persisted DueAt is in the future
// (e.g. sitting in retry backoff), then let normal cadence resume — the override
// is one-shot and must not linger.
func TestForceRefreshOverridesFutureDueThenResumes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/movie/500/release_dates", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"results":[{"iso_3166_1":"US","release_dates":[{"type":4,"release_date":"2026-01-01T00:00:00Z"}]}]}`))
	})
	ful := newFakeFul()
	ful.noStream[[2]int{0, 0}] = true // released, but no stream yet → stays in retry backoff
	now := date("2026-06-01T00:00:00Z")
	m, st := testMonitor(t, mux, ful, now) // interval = 1h

	// A monitored, released movie parked with a far-future DueAt (retry backoff).
	if err := st.PutMonitored(context.Background(), store.Monitored{
		Key: "movie:tt5", MediaType: "movie", IMDbID: "tt5", TMDbID: "500", Title: "Film",
		Category: library.Root("movie", false), DueAt: now.Add(2 * time.Hour), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Not due yet → an ordinary pass skips it.
	m.checkDue(context.Background())
	if ful.calls != 0 {
		t.Fatalf("future-DueAt item must be skipped without force; calls=%d", ful.calls)
	}

	// A forced refresh treats every enabled item as due now → it's processed.
	m.ForceRefresh()
	m.checkDue(context.Background())
	if ful.calls != 1 {
		t.Fatalf("forced refresh must process the future-DueAt item; calls=%d", ful.calls)
	}

	// The override is one-shot: processing reset DueAt to the retry ceiling
	// (now+interval), NOT zeroed, and the next ordinary pass honors it again.
	got, _ := st.GetMonitored(context.Background(), "movie:tt5")
	if got == nil || !got.DueAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("forced pass should reset DueAt to the retry ceiling now+1h; got %#v", got)
	}
	m.checkDue(context.Background())
	if ful.calls != 1 {
		t.Fatalf("override must not persist; item re-processed after force: calls=%d", ful.calls)
	}
}

func TestMonitorRejectsUngatableMovie(t *testing.T) {
	st := newStore(t)
	m := New(st, metadata.New("", nil), newFakeFul(), time.Hour, 4, 7*24*time.Hour, slog.New(slog.DiscardHandler)) // no TMDB key
	// tmdb-only movie, no imdb, no TMDB key → no way to gate release.
	if err := m.Intake(context.Background(), Request{MediaType: "movie", TMDbID: "603", Title: "X"}); err == nil {
		t.Fatal("expected rejection of ungatable tmdb-only movie")
	}
	if n, _ := st.CountMonitored(context.Background()); n != 0 {
		t.Fatalf("ungatable movie was stored: %d", n)
	}
}

// instrumentedFul models per-Pin latency and tracks peak concurrent Pin calls so
// tests can assert the bounded fan-out. failEp episodes return an error; noStream
// episodes return (false, nil). All state is guarded for concurrent use.
type instrumentedFul struct {
	latency  time.Duration
	failEp   map[[2]int]bool
	noStream map[[2]int]bool

	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	calls       atomic.Int32

	mu     sync.Mutex
	seeded map[PinKey]bool // pre-existing pins reported by PinnedKeys
	pinned map[PinKey]bool // pins created via Pin
}

func newInstrumentedFul(latency time.Duration) *instrumentedFul {
	return &instrumentedFul{
		latency:  latency,
		failEp:   map[[2]int]bool{},
		noStream: map[[2]int]bool{},
		seeded:   map[PinKey]bool{},
		pinned:   map[PinKey]bool{},
	}
}

func (f *instrumentedFul) Pin(_ context.Context, t Target) (PinOutcome, error) {
	n := f.inFlight.Add(1)
	for { // publish the running peak
		cur := f.maxInFlight.Load()
		if n <= cur || f.maxInFlight.CompareAndSwap(cur, n) {
			break
		}
	}
	defer f.inFlight.Add(-1)
	f.calls.Add(1)
	if f.latency > 0 {
		time.Sleep(f.latency)
	}
	if f.failEp[[2]int{t.Season, t.Episode}] {
		return NoResults, errors.New("resolver hiccup")
	}
	if f.noStream[[2]int{t.Season, t.Episode}] {
		return NoResults, nil
	}
	f.mu.Lock()
	f.pinned[PinKey{t.Season, t.Episode, library.NormalizeQuality(t.Quality)}] = true
	f.mu.Unlock()
	return Pinned, nil
}

func (f *instrumentedFul) PinnedKeys(_ context.Context, _ string) (map[PinKey]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[PinKey]bool{}
	for k := range f.seeded {
		out[k] = true
	}
	for k := range f.pinned {
		out[k] = true
	}
	return out, nil
}

// seriesEpisodesMux serves a Cinemeta series with n episodes, all aired in the
// past relative to the test clock, so processSeries resolves every one.
func seriesEpisodesMux(imdb string, n int) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/meta/series/"+imdb+".json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{"videos":[`))
		for i := 1; i <= n; i++ {
			if i > 1 {
				w.Write([]byte(","))
			}
			fmt.Fprintf(w, `{"season":1,"episode":%d,"released":"2026-01-01T00:00:00Z"}`, i)
		}
		w.Write([]byte(`]}}`))
	})
	mux.HandleFunc("/lookup/shows", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":0}`)) })
	return mux
}

func seriesItem(imdb string, qualities ...string) store.Monitored {
	return store.Monitored{
		Key: "series:" + imdb, MediaType: "series", IMDbID: imdb,
		Qualities: qualities, Enabled: true, Category: library.Root("series", false),
	}
}

// A season resolves its aired episodes in parallel, but never above the limit:
// peak concurrent Pin calls stays ≤ resolveConcurrency, and total wall-clock
// reflects the parallelism (≈ ceil(n/limit) waves, not n sequential).
func TestSeriesResolvesEpisodesWithBoundedConcurrency(t *testing.T) {
	const (
		episodes = 8
		limit    = 4
		latency  = 50 * time.Millisecond
	)
	ful := newInstrumentedFul(latency)
	now := date("2026-06-01T00:00:00Z")
	m, _ := testMonitor(t, seriesEpisodesMux("tt7", episodes), ful, now)
	m.resolveConcurrency = limit

	start := time.Now()
	res := m.processSeries(context.Background(), seriesItem("tt7", "1080p"), false)
	elapsed := time.Since(start)

	if got := ful.calls.Load(); got != episodes {
		t.Fatalf("Pin calls = %d, want %d (one per aired episode)", got, episodes)
	}
	if res.pendingAired != 0 {
		t.Fatalf("pendingAired = %d, want 0 (every episode resolved)", res.pendingAired)
	}
	if peak := ful.maxInFlight.Load(); peak > limit {
		t.Fatalf("peak concurrent Pin = %d, exceeds limit %d", peak, limit)
	} else if peak < 2 {
		t.Fatalf("peak concurrent Pin = %d, expected real parallelism (>1)", peak)
	}
	// Sequential would be episodes*latency = 400ms; ~2 waves is ~100ms. Allow
	// generous slack for a loaded CI while still proving it isn't serial.
	if maxWall := time.Duration(episodes) * latency; elapsed >= maxWall {
		t.Fatalf("wall-clock %v ≥ sequential floor %v — not parallel", elapsed, maxWall)
	}
}

// The aggregate pendingAired from the parallel path must equal the sequential
// (limit=1) result for a mix of already-pinned, freshly-pinned, and no-stream
// episodes.
func TestSeriesAggregateMatchesSequential(t *testing.T) {
	// E2 already pinned (dedupe → 0), E3 & E5 have no stream (→ remaining),
	// the rest pin cleanly (→ 0). Expected pendingAired = 2, both runs.
	build := func() *instrumentedFul {
		f := newInstrumentedFul(0)
		f.seeded[PinKey{1, 2, "1080p"}] = true
		f.noStream[[2]int{1, 3}] = true
		f.noStream[[2]int{1, 5}] = true
		return f
	}

	run := func(limit int) int {
		ful := build()
		now := date("2026-06-01T00:00:00Z")
		m, _ := testMonitor(t, seriesEpisodesMux("tt7", 6), ful, now)
		m.resolveConcurrency = limit
		return m.processSeries(context.Background(), seriesItem("tt7", "1080p"), false).pendingAired
	}

	seq := run(1)
	par := run(4)
	if seq != 2 {
		t.Fatalf("sequential pendingAired = %d, want 2", seq)
	}
	if par != seq {
		t.Fatalf("parallel pendingAired = %d != sequential %d", par, seq)
	}
}

// One episode's resolver error must not abort the rest of the season: every
// other aired episode is still processed, and only the failed unit is counted
// as remaining.
func TestSeriesEpisodeErrorDoesNotAbortOthers(t *testing.T) {
	ful := newInstrumentedFul(0)
	ful.failEp[[2]int{1, 3}] = true // E3 errors on Pin
	now := date("2026-06-01T00:00:00Z")
	m, _ := testMonitor(t, seriesEpisodesMux("tt7", 6), ful, now)
	m.resolveConcurrency = 4

	res := m.processSeries(context.Background(), seriesItem("tt7", "1080p"), false)

	if got := ful.calls.Load(); got != 6 {
		t.Fatalf("Pin calls = %d, want 6 (error must not short-circuit the season)", got)
	}
	if res.pendingAired != 1 {
		t.Fatalf("pendingAired = %d, want 1 (only the failed episode)", res.pendingAired)
	}
	for _, ep := range []int{1, 2, 4, 5, 6} {
		if !ful.pinned[PinKey{1, ep, "1080p"}] {
			t.Fatalf("episode %d was not pinned despite E3 failing", ep)
		}
	}
}

// movieReleaseMux serves a movie whose home release is in the past (2026-01-01),
// so processMovie proceeds straight to pinning against the test clock.
func movieReleaseMux(tmdb string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/movie/"+tmdb+"/release_dates", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"results":[{"iso_3166_1":"US","release_dates":[{"type":4,"release_date":"2026-01-01T00:00:00Z"}]}]}`))
	})
	return mux
}

func movieItem(imdb, tmdb string, qualities ...string) store.Monitored {
	return store.Monitored{
		Key: "movie:" + imdb, MediaType: "movie", IMDbID: imdb, TMDbID: tmdb,
		Qualities: qualities, Enabled: true, Category: library.Root("movie", false),
	}
}

// TierExhausted fires only once the exponential ramp has saturated at the cap —
// the terminal state of a backoff that never permanently gives up. Below that,
// the tier is still being retried on a meaningful schedule and must not be
// treated as abandoned.
func TestTierExhausted(t *testing.T) {
	ful := newFakeFul()
	m, _ := testMonitor(t, movieReleaseMux("500"), ful, date("2026-06-01T00:00:00Z"))
	m.tierBackoffMax = 8 * time.Hour // interval = 1h → ramp 1h, 2h, 4h, 8h(cap)

	cases := []struct {
		misses int
		want   bool
	}{
		{misses: 0, want: false}, // no evidence at all
		{misses: 1, want: false}, // 1h — a single unanimous miss
		{misses: 2, want: false}, // 2h
		{misses: 3, want: false}, // 4h
		{misses: 4, want: true},  // 8h — saturated
		{misses: 9, want: true},  // still saturated
	}
	for _, tc := range cases {
		got := m.TierExhausted(store.TierBackoffState{Misses: tc.misses})
		if got != tc.want {
			t.Fatalf("TierExhausted(misses=%d) = %v, want %v", tc.misses, got, tc.want)
		}
	}
	// NextTry is deliberately irrelevant — inside or past its window, a saturated
	// tier is equally absent, and keying off it would flap the status API.
	inWindow := store.TierBackoffState{Misses: 4, NextTry: date("2027-01-01T00:00:00Z")}
	if !m.TierExhausted(inWindow) {
		t.Fatal("a saturated tier must stay exhausted while inside its backoff window")
	}
}

// A pass records which requested tier each unpinned episode is waiting on, so the
// status API can tell "10 episodes missing 2160p" from "10 episodes missing
// everything". The default ("") tier is never tracked.
func TestSeriesRecordsPendingByTier(t *testing.T) {
	ctx := context.Background()
	ful := newFakeFul()
	ful.noQuality["2160p"] = true     // no 4K rips exist for this title
	ful.noStream[[2]int{1, 2}] = true // E2 has no stream at any tier yet
	now := date("2026-06-01T00:00:00Z")
	m, _ := testMonitor(t, seriesEpisodesMux("tt42", 3), ful, now)

	res := m.processSeries(ctx, seriesItem("tt42", "1080p", "2160p"), false)

	// 3 episodes × 2160p absent, plus E2's 1080p with no stream at all = 4 pending.
	if res.pendingAired != 4 {
		t.Fatalf("pendingAired = %d, want 4", res.pendingAired)
	}
	if got := res.pendingByTier["2160p"]; got != 3 {
		t.Fatalf("pendingByTier[2160p] = %d, want 3", got)
	}
	if got := res.pendingByTier["1080p"]; got != 1 {
		t.Fatalf("pendingByTier[1080p] = %d, want 1", got)
	}
}

// A metadata failure is not evidence that a series caught up: the previous
// pending counts must survive the pass, or the status API would briefly report a
// half-pinned series as completed.
func TestSeriesEnumerateErrorKeepsPendingCounts(t *testing.T) {
	ctx := context.Background()
	now := date("2026-06-01T00:00:00Z")
	// A mux with no /meta/series route → enumeration fails.
	m, _ := testMonitor(t, http.NewServeMux(), newFakeFul(), now)

	it := seriesItem("tt43", "1080p", "2160p")
	it.PendingAired = 7
	it.PendingByTier = map[string]int{"2160p": 7}

	res := m.processSeries(ctx, it, false)
	if res.errMsg == "" {
		t.Fatal("expected an enumeration error")
	}
	if res.pendingAired != 7 {
		t.Fatalf("pendingAired = %d, want the previous 7 carried forward", res.pendingAired)
	}
	if got := res.pendingByTier["2160p"]; got != 7 {
		t.Fatalf("pendingByTier[2160p] = %d, want the previous 7 carried forward", got)
	}
}

// A tier that consistently returns NoQualityMatch (results exist, but not at this
// resolution) accrues misses and its NextTry backs off exponentially from the base
// interval, capped at tierBackoffMax; a successful pin of that tier resets it.
func TestTierBackoffAccruesAndCaps(t *testing.T) {
	ctx := context.Background()
	ful := newFakeFul()
	ful.noQuality["2160p"] = true // no 4K rips exist for this title
	clock := date("2026-06-01T00:00:00Z")
	m, _ := testMonitor(t, movieReleaseMux("500"), ful, clock) // interval = 1h
	m.now = func() time.Time { return clock }
	m.tierBackoffMax = 5 * time.Hour // cap after a few doublings

	item := movieItem("tt5", "500", "1080p", "2160p")

	// Each pass runs at the tier's due moment (the clock is advanced to the prior
	// NextTry between passes), so the tier is genuinely attempted and its miss
	// streak advances. The exponential ramp from the 1h base, capped at 5h: 1h, 2h,
	// 4h, 5h, 5h, … — the backoff a saturated tier settles into.
	wantDelays := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 5 * time.Hour, 5 * time.Hour}
	for pass, want := range wantDelays {
		res := m.processMovie(ctx, item, false)
		st, ok := res.tierBackoff["2160p"]
		if !ok {
			t.Fatalf("pass %d: 2160p tier not backed off; map=%v", pass+1, res.tierBackoff)
		}
		if st.Misses != pass+1 {
			t.Fatalf("pass %d: misses = %d, want %d", pass+1, st.Misses, pass+1)
		}
		if wantNext := clock.Add(want); !st.NextTry.Equal(wantNext) {
			t.Fatalf("pass %d: NextTry = %v, want now+%v", pass+1, st.NextTry, want)
		}
		// Every remaining target is the absent tier → DueAt follows the tier schedule.
		if res.reason != store.DueReasonTierBackoff {
			t.Fatalf("pass %d: reason = %q, want %q", pass+1, res.reason, store.DueReasonTierBackoff)
		}
		if wantDue := clock.Add(want); !res.due.Equal(wantDue) {
			t.Fatalf("pass %d: due = %v, want now+%v", pass+1, res.due, want)
		}
		item.TierBackoff = res.tierBackoff // thread state forward (persistResult would)
		clock = st.NextTry                 // advance to the tier's deadline for the next pass
	}

	// 4K rips finally appear → a successful pin of 2160p resets the tier: both tiers
	// pin, the movie completes, and no backoff state lingers.
	ful.noQuality["2160p"] = false
	res := m.processMovie(ctx, item, false)
	if !res.completed {
		t.Fatalf("movie should complete once every tier pins; res=%#v", res)
	}
	if _, ok := res.tierBackoff["2160p"]; ok {
		t.Fatalf("2160p backoff must reset on a successful pin; map=%v", res.tierBackoff)
	}
}

// A tier in backoff (NextTry in the future) must NOT be attempted when the title
// wakes for another reason — its miss streak must not advance and Pin must not be
// called for it — yet a forced refresh must still attempt it.
func TestBackedOffTierNotAttemptedUntilDeadline(t *testing.T) {
	ctx := context.Background()
	ful := newFakeFul()
	ful.noQuality["2160p"] = true     // 4K absent (will back off)
	ful.noStream[[2]int{1, 2}] = true // E2's 1080p has no stream yet → the title keeps waking fast
	now := date("2026-06-01T00:00:00Z")
	m, _ := testMonitor(t, seriesEpisodesMux("tt7", 3), ful, now)
	m.resolveConcurrency = 4

	// 2160p already saturated: backed off with a far-future NextTry and a real streak.
	item := seriesItem("tt7", "1080p", "2160p")
	item.TierBackoff = map[string]store.TierBackoffState{"2160p": {Misses: 4, NextTry: now.Add(24 * time.Hour)}}

	res := m.processSeries(ctx, item, false)

	// The 2160p tier is still in backoff → skipped, not attempted: its streak holds
	// at 4 and its NextTry is unchanged.
	st := res.tierBackoff["2160p"]
	if st.Misses != 4 || !st.NextTry.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("backed-off tier advanced without force: %#v", st)
	}
	// Pin was called only for the 1080p tier of the 3 episodes (E1,E2,E3), never for
	// 2160p (3 calls, not 6).
	if ful.calls != 3 {
		t.Fatalf("Pin calls = %d, want 3 (2160p must not be attempted while backed off)", ful.calls)
	}
	// A normal tier (E2's 1080p) still pends → the title stays on the fast cadence.
	if res.reason != store.DueReasonRetry || !res.due.Equal(now.Add(time.Hour)) {
		t.Fatalf("due = %v (%s), want the fast interval retry", res.due, res.reason)
	}

	// A forced refresh overrides the tier backoff and re-attempts 2160p for every
	// episode (3 × 2160p = 3 more calls, plus 1080p re-checks).
	before := ful.calls
	forced := m.processSeries(ctx, item, true)
	if got := ful.calls - before; got < 3 {
		t.Fatalf("forced refresh attempted %d new pins, want ≥3 (2160p re-tried for all episodes)", got)
	}
	if fst := forced.tierBackoff["2160p"]; fst.Misses != 5 {
		t.Fatalf("forced 2160p misses = %d, want 5 (streak advanced under force)", fst.Misses)
	}
}

// A transient NoResults (no stream yet at all) must NOT accrue tier backoff — only
// the specific NoQualityMatch signal does. The title stays on the fast cadence.
func TestNoResultsDoesNotAccrueTierBackoff(t *testing.T) {
	ctx := context.Background()
	ful := newFakeFul()
	ful.noStream[[2]int{0, 0}] = true // movie released but no stream yet (transient)
	now := date("2026-06-01T00:00:00Z")
	m, _ := testMonitor(t, movieReleaseMux("500"), ful, now) // interval = 1h

	res := m.processMovie(ctx, movieItem("tt5", "500", "2160p"), false)

	if len(res.tierBackoff) != 0 {
		t.Fatalf("NoResults must not back off any tier; map=%v", res.tierBackoff)
	}
	if res.reason != store.DueReasonRetry {
		t.Fatalf("reason = %q, want the fast retry ceiling %q", res.reason, store.DueReasonRetry)
	}
	if want := now.Add(time.Hour); !res.due.Equal(want) {
		t.Fatalf("due = %v, want the tight interval %v", res.due, want)
	}
}

// When every remaining target of a series is a backed-off tier, DueAt is driven by
// the earliest tier NextTry — not the tight interval.
func TestSeriesDueDrivenByBackedOffTier(t *testing.T) {
	ctx := context.Background()
	ful := newFakeFul()
	ful.noQuality["2160p"] = true // no 4K rips for this show
	now := date("2026-06-01T00:00:00Z")
	m, _ := testMonitor(t, seriesEpisodesMux("tt7", 4), ful, now) // 4 aired episodes, none upcoming
	m.resolveConcurrency = 4

	// Pre-existing streak so the tier's NextTry is well past the tight interval.
	item := seriesItem("tt7", "1080p", "2160p")
	item.TierBackoff = map[string]store.TierBackoffState{"2160p": {Misses: 5, NextTry: now}}

	res := m.processSeries(ctx, item, false)

	// 1080p pins for all four → only 2160p remains, unanimously absent → backed off.
	st := res.tierBackoff["2160p"]
	if st.Misses != 6 {
		t.Fatalf("misses = %d, want 6 (streak continued)", st.Misses)
	}
	// delay(6) from a 1h base = 32h.
	if want := now.Add(32 * time.Hour); !res.due.Equal(want) {
		t.Fatalf("due = %v, want the tier NextTry now+32h", res.due)
	}
	if res.reason != store.DueReasonTierBackoff {
		t.Fatalf("reason = %q, want %q", res.reason, store.DueReasonTierBackoff)
	}
}

// A normal-state tier (or a transient miss) keeps the whole title on the fast
// cadence even while another tier is backed off.
func TestSeriesNormalTierKeepsFastCadence(t *testing.T) {
	ctx := context.Background()
	ful := newFakeFul()
	ful.noQuality["2160p"] = true     // 4K absent (would back off)
	ful.noStream[[2]int{1, 2}] = true // but E2's 1080p has no stream yet (transient, normal)
	now := date("2026-06-01T00:00:00Z")
	m, _ := testMonitor(t, seriesEpisodesMux("tt7", 4), ful, now)
	m.resolveConcurrency = 4

	item := seriesItem("tt7", "1080p", "2160p")
	item.TierBackoff = map[string]store.TierBackoffState{"2160p": {Misses: 5, NextTry: now}}

	res := m.processSeries(ctx, item, false)

	// A normal (1080p) target still pending → fast cadence at now+interval, NOT the
	// far 2160p tier schedule.
	if res.reason != store.DueReasonRetry {
		t.Fatalf("reason = %q, want the fast retry %q", res.reason, store.DueReasonRetry)
	}
	if want := now.Add(time.Hour); !res.due.Equal(want) {
		t.Fatalf("due = %v, want the tight interval %v (a normal tier still pends)", res.due, want)
	}
	// 2160p still accrues its miss in the background (readable for later surfacing).
	if st := res.tierBackoff["2160p"]; st.Misses != 6 {
		t.Fatalf("2160p misses = %d, want 6 (still absent across the title)", st.Misses)
	}
}

// The per-tier tally is correct under the parallel episode loop: with many aired
// episodes resolved concurrently, an absent tier is detected as unanimously
// missing exactly once (misses=1), and a present tier is never falsely backed off.
// Run under -race to prove the aggregation is race-free.
func TestTierTallyConcurrencySafe(t *testing.T) {
	ctx := context.Background()
	ful := newFakeFul()
	ful.noQuality["2160p"] = true
	now := date("2026-06-01T00:00:00Z")
	m, _ := testMonitor(t, seriesEpisodesMux("tt7", 12), ful, now)
	m.resolveConcurrency = 8

	res := m.processSeries(ctx, seriesItem("tt7", "1080p", "2160p"), false)

	if st, ok := res.tierBackoff["2160p"]; !ok || st.Misses != 1 {
		t.Fatalf("2160p backoff = %#v (ok=%v), want misses=1", res.tierBackoff["2160p"], ok)
	}
	if _, ok := res.tierBackoff["1080p"]; ok {
		t.Fatalf("1080p pinned for every episode; must not be backed off; map=%v", res.tierBackoff)
	}
	if res.pendingAired != 12 {
		t.Fatalf("pendingAired = %d, want 12 (2160p missing for every episode)", res.pendingAired)
	}
}

// A tier that is only partially absent (present for some episodes) is NOT a hard
// backoff — the resolution demonstrably exists for the title, so the fast cadence
// is kept and no miss accrues.
func TestPartialTierAbsenceDoesNotBackOff(t *testing.T) {
	ctx := context.Background()
	ful := newFakeFul()
	// 2160p exists for E1 (pre-pinned), absent everywhere else — not unanimous.
	ful.pinned[PinKey{1, 1, "2160p"}] = true
	ful.noQuality["2160p"] = true
	now := date("2026-06-01T00:00:00Z")
	m, _ := testMonitor(t, seriesEpisodesMux("tt7", 4), ful, now)
	m.resolveConcurrency = 4

	res := m.processSeries(ctx, seriesItem("tt7", "2160p"), false)

	if len(res.tierBackoff) != 0 {
		t.Fatalf("partial absence must not back off (E1 has 2160p); map=%v", res.tierBackoff)
	}
	if res.reason != store.DueReasonRetry {
		t.Fatalf("reason = %q, want fast retry %q", res.reason, store.DueReasonRetry)
	}
}

// An idempotent re-request of an existing title must preserve its per-tier backoff
// streak — otherwise a repeated intake would reset an absent tier to Misses==0 and
// defeat the exponential backoff.
func TestIntakePreservesTierBackoff(t *testing.T) {
	ctx := context.Background()
	m, st := testMonitor(t, http.NewServeMux(), newFakeFul(), date("2026-06-01T00:00:00Z"))

	seed := store.Monitored{
		Key: "movie:tt5", MediaType: "movie", IMDbID: "tt5", TMDbID: "500",
		Title: "Film", Qualities: []string{"2160p"}, Enabled: true,
		Category:    library.Root("movie", false),
		TierBackoff: map[string]store.TierBackoffState{"2160p": {Misses: 3, NextTry: date("2026-06-05T00:00:00Z")}},
	}
	if err := st.PutMonitored(ctx, seed); err != nil {
		t.Fatal(err)
	}

	// Re-post the same title (a feeder re-requesting is idempotent).
	if err := m.Intake(ctx, Request{MediaType: "movie", IMDbID: "tt5", TMDbID: "500", Title: "Film", Qualities: []string{"2160p"}}); err != nil {
		t.Fatal(err)
	}

	got, _ := st.GetMonitored(ctx, "movie:tt5")
	tb, ok := got.TierBackoff["2160p"]
	if !ok || tb.Misses != 3 || !tb.NextTry.Equal(date("2026-06-05T00:00:00Z")) {
		t.Fatalf("re-request reset the tier streak: %#v", got.TierBackoff)
	}
}
