package plugin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
)

type stubSearcher struct {
	streams []aiostreams.Stream
	err     error
	calls   int
	gotType string
	gotID   string
	gotS    int
	gotE    int
}

func (s *stubSearcher) Search(_ context.Context, mediaType, imdbID string, season, episode int) ([]aiostreams.Stream, error) {
	s.calls++
	s.gotType, s.gotID, s.gotS, s.gotE = mediaType, imdbID, season, episode
	return s.streams, s.err
}

func TestParseResolvePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want ResolveRequest
	}{
		{
			name: "movie",
			path: "/resolve/movie/tt0133093",
			want: ResolveRequest{MediaType: "movie", ID: MediaID{SourceIMDb, "tt0133093"}, IMDbID: "tt0133093"},
		},
		{
			name: "series",
			path: "/resolve/series/tt0944947/1/9",
			want: ResolveRequest{MediaType: "series", ID: MediaID{SourceIMDb, "tt0944947"}, IMDbID: "tt0944947", Season: 1, Episode: 9},
		},
		{
			name: "double digit season and episode",
			path: "/resolve/series/tt0944947/10/22",
			want: ResolveRequest{MediaType: "series", ID: MediaID{SourceIMDb, "tt0944947"}, IMDbID: "tt0944947", Season: 10, Episode: 22},
		},
		{
			// Silo mounts plugin routes under a prefix whose depth we should
			// not have to know.
			name: "tolerates a mount prefix",
			path: "/api/v1/plugins/20/resolve/movie/tt0133093",
			want: ResolveRequest{MediaType: "movie", ID: MediaID{SourceIMDb, "tt0133093"}, IMDbID: "tt0133093"},
		},
		{
			name: "no leading slash",
			path: "resolve/movie/tt0133093",
			want: ResolveRequest{MediaType: "movie", ID: MediaID{SourceIMDb, "tt0133093"}, IMDbID: "tt0133093"},
		},
		{
			name: "special 0 season",
			path: "/resolve/series/tt0944947/0/1",
			want: ResolveRequest{MediaType: "series", ID: MediaID{SourceIMDb, "tt0944947"}, IMDbID: "tt0944947", Season: 0, Episode: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseResolvePath(tt.path)
			if err != nil {
				t.Fatalf("ParseResolvePath(%q) error = %v", tt.path, err)
			}
			if got != tt.want {
				t.Errorf("ParseResolvePath(%q) = %+v, want %+v", tt.path, got, tt.want)
			}
		})
	}
}

func TestParseResolvePathRejects(t *testing.T) {
	bad := []string{
		"/resolve",
		"/resolve/movie",
		"/resolve/series/tt0944947",     // missing season+episode
		"/resolve/series/tt0944947/1",   // missing episode
		"/resolve/movie/tt0133093/1/9",  // trailing segments on a movie
		"/resolve/series/tt0944947/x/9", // non-numeric season
		"/resolve/series/tt0944947/1/y", // non-numeric episode
		"/resolve/audiobook/tt0133093",  // unknown media type
		"/health",
		"",
	}
	for _, path := range bad {
		if got, err := ParseResolvePath(path); err == nil {
			t.Errorf("ParseResolvePath(%q) = %+v, want error", path, got)
		}
	}
}

func TestResolvePicksRequestedQuality(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn/1080.mkv", Resolution: "1080p", Filename: "a.1080p.mkv"},
		{URL: "https://cdn/2160.mkv", Resolution: "2160p", Filename: "a.2160p.mkv"},
	}}

	got, err := alwaysLive(NewResolver(stub)).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093", Quality: "2160p",
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.URL != "https://cdn/2160.mkv" {
		t.Errorf("URL = %q, want the 2160p candidate", got.URL)
	}
}

// A user pressing play wants something to play. Holding out for an exact tier
// that does not exist is a worse outcome than a lower one that does.
func TestResolveFallsBackWhenTierMissing(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn/1080.mkv", Resolution: "1080p"},
	}}

	got, err := alwaysLive(NewResolver(stub)).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093", Quality: "2160p",
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.URL != "https://cdn/1080.mkv" {
		t.Errorf("URL = %q, want the 1080p fallback", got.URL)
	}
}

// AIOStreams already parses, ranks, and filters per the operator's config.
// Re-sorting here would silently override settings made in one obvious place.
func TestResolveHonoursAddonOrderingWithinATier(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn/first.mkv", Resolution: "1080p"},
		{URL: "https://cdn/second.mkv", Resolution: "1080p"},
	}}

	got, err := alwaysLive(NewResolver(stub)).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093", Quality: "1080p",
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.URL != "https://cdn/first.mkv" {
		t.Errorf("URL = %q, want the addon's first candidate", got.URL)
	}
}

func TestResolveNoQualityTakesFirstPlayable(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "", Resolution: "2160p"}, // unplayable, must be skipped
		{URL: "https://cdn/ok.mkv", Resolution: "720p"},
	}}

	got, err := alwaysLive(NewResolver(stub)).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093",
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.URL != "https://cdn/ok.mkv" {
		t.Errorf("URL = %q, want the first candidate with a URL", got.URL)
	}
}

func TestResolveSkipsEmptyURLsAtRequestedTier(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "", Resolution: "1080p"},
		{URL: "https://cdn/good.mkv", Resolution: "1080p"},
	}}

	got, err := alwaysLive(NewResolver(stub)).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093", Quality: "1080p",
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.URL != "https://cdn/good.mkv" {
		t.Errorf("URL = %q, want the playable 1080p candidate", got.URL)
	}
}

func TestResolveNoPlayableCandidates(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "", Resolution: "1080p"},
		{URL: "   ", Resolution: "2160p"},
	}}

	_, err := alwaysLive(NewResolver(stub)).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093",
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("error = %v, want ErrNoMatch", err)
	}
}

func TestResolvePropagatesSearchError(t *testing.T) {
	sentinel := errors.New("upstream exploded")
	stub := &stubSearcher{err: sentinel}

	_, err := alwaysLive(NewResolver(stub)).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093",
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the underlying search error", err)
	}
}

func TestResolvePassesEpisodeCoordinates(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{{URL: "https://cdn/a.mkv", Resolution: "1080p"}}}

	_, err := alwaysLive(NewResolver(stub)).Resolve(context.Background(), ResolveRequest{
		MediaType: "series", ID: MediaID{SourceTVDB, "121361"}, IMDbID: "tt0944947", Season: 3, Episode: 9,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if stub.gotType != "series" || stub.gotID != "tt0944947" || stub.gotS != 3 || stub.gotE != 9 {
		t.Errorf("Search got (%q,%q,%d,%d), want (series,tt0944947,3,9)",
			stub.gotType, stub.gotID, stub.gotS, stub.gotE)
	}
}

// Same regression guard as the fork side: FFmpeg caches 301/308 for the life of
// its HTTPContext, and every URL here is short-lived.
func TestRedirectStatusIsNotCacheableByFFmpeg(t *testing.T) {
	if RedirectStatus == http.StatusMovedPermanently || RedirectStatus == http.StatusPermanentRedirect {
		t.Fatalf("RedirectStatus = %d: FFmpeg caches 301/308 indefinitely; use 302", RedirectStatus)
	}
}

func TestAcceptableTiers(t *testing.T) {
	cases := map[string][]string{
		"":      {""},
		"2160p": {"2160p", "1080p", ""},
		"4K":    {"2160p", "1080p", ""},
		"1080p": {"1080p", "720p", ""},
		"720p":  {"720p", ""},
		"480p":  {"480p", ""},
	}
	for in, want := range cases {
		got := acceptableTiers(in)
		if len(got) != len(want) {
			t.Errorf("acceptableTiers(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("acceptableTiers(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}

// The resolver 302s straight to whatever the upstream returned, on a route that
// is public. A compromised or misconfigured AIOStreams must not be able to turn
// that into an open redirect to a local file or an internal address.
func TestResolveRejectsNonHTTPCandidates(t *testing.T) {
	for _, bad := range []string{
		"file:///etc/passwd",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"ftp://example.invalid/x.mkv",
		"://nonsense",
		"https://", // no host
		"   ",
	} {
		r := alwaysLive(NewResolver(&stubSearcher{streams: []aiostreams.Stream{
			{URL: bad, Resolution: "1080p"},
		}}))
		_, err := r.Resolve(context.Background(), ResolveRequest{
			MediaType: "movie", IMDbID: "tt1", Quality: "1080p",
		})
		if !errors.Is(err, ErrNoMatch) {
			t.Errorf("candidate %q was accepted (err = %v); want it skipped", bad, err)
		}
	}
}

// Skipping an unusable candidate must not skip the rest of the list.
func TestResolveSkipsBadCandidateAndTakesTheNext(t *testing.T) {
	r := alwaysLive(NewResolver(&stubSearcher{streams: []aiostreams.Stream{
		{URL: "file:///etc/passwd", Resolution: "1080p"},
		{URL: "https://cdn.example.invalid/real.mkv", Resolution: "1080p"},
	}}))
	got, err := r.Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt1", Quality: "1080p",
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.URL != "https://cdn.example.invalid/real.mkv" {
		t.Errorf("URL = %q, want the second candidate", got.URL)
	}
}

// alwaysLive disables liveness checking for tests whose candidates are
// synthetic URLs with nothing behind them.
func alwaysLive(r *Resolver) *Resolver {
	r.live = func(context.Context, string) error { return nil }
	return r
}

// A debrid provider answers an uncached title with "202 Accepted,
// Content-Length: 0" — a promise to fetch it, not a stream. Handing that to a
// player produces "Invalid data found when processing input" and a failure that
// looks like a Wisp bug rather than an unavailable release. Measured against a
// real title, five of fourteen ranked candidates were dead, the top one
// included.
func TestResolveSkipsCandidatesThatAreNotServing(t *testing.T) {
	dead := map[string]bool{
		"https://cdn.example.invalid/not-cached.mkv":  true,
		"https://cdn.example.invalid/bad-gateway.mkv": true,
	}
	r := NewResolver(&stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn.example.invalid/not-cached.mkv", Resolution: "1080p", Filename: "a"},
		{URL: "https://cdn.example.invalid/bad-gateway.mkv", Resolution: "1080p", Filename: "b"},
		{URL: "https://cdn.example.invalid/works.mkv", Resolution: "1080p", Filename: "c"},
	}})
	r.live = func(_ context.Context, u string) error {
		if dead[u] {
			return errors.New("not serving")
		}
		return nil
	}

	got, err := r.Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt1", Quality: "1080p",
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.URL != "https://cdn.example.invalid/works.mkv" {
		t.Errorf("URL = %q, want the first candidate that was actually serving", got.URL)
	}
}

// The check must not become a new way to fail playback. Once the budget is
// spent the best remaining candidate is returned unverified: it may still play,
// and a definite failure is worse than a probable success.
func TestResolveReturnsUnverifiedCandidateAfterBudget(t *testing.T) {
	var streams []aiostreams.Stream
	for i := 0; i < liveChecks+3; i++ {
		streams = append(streams, aiostreams.Stream{
			URL: "https://cdn.example.invalid/" + strconv.Itoa(i) + ".mkv", Resolution: "1080p",
		})
	}
	// Checks run concurrently now, so the counter has to be safe.
	var checks atomic.Int32
	r := NewResolver(&stubSearcher{streams: streams})
	r.live = func(context.Context, string) error {
		checks.Add(1)
		return errors.New("not serving")
	}

	got, err := r.Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt1", Quality: "1080p",
	})
	if err != nil {
		t.Fatalf("Resolve() returned no stream at all: %v", err)
	}
	if got.URL == "" {
		t.Error("returned an empty stream")
	}
	if got := int(checks.Load()); got != liveChecks {
		t.Errorf("ran %d checks, want the budget of %d", got, liveChecks)
	}
}

// Liveness must not reorder anything: AIOStreams' own first serving candidate
// at the requested tier still wins.
func TestResolveLivenessDoesNotReorderCandidates(t *testing.T) {
	r := alwaysLive(NewResolver(&stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn.example.invalid/first.mkv", Resolution: "1080p"},
		{URL: "https://cdn.example.invalid/second.mkv", Resolution: "1080p"},
	}}))

	got, err := r.Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt1", Quality: "1080p",
	})
	if err != nil || got.URL != "https://cdn.example.invalid/first.mkv" {
		t.Errorf("got %q (%v), want AIOStreams' own first candidate", got.URL, err)
	}
}

// The dashboard showed a three-segment bar whose last two segments were
// hardcoded to zero — a single colour pretending to be a breakdown. The split
// that actually exists is worth seeing: time asking the provider, versus time
// discarding candidates that were not serving.
func TestResolveTracedSeparatesSearchFromVerification(t *testing.T) {
	slowSearch := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn.example.invalid/dead.mkv", Resolution: "1080p"},
		{URL: "https://cdn.example.invalid/live.mkv", Resolution: "1080p"},
	}}
	r := NewResolver(slowSearch)
	r.live = func(_ context.Context, u string) error {
		time.Sleep(30 * time.Millisecond)
		if strings.Contains(u, "dead") {
			return errors.New("not serving")
		}
		return nil
	}

	got, trace, err := r.ResolveTraced(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt1", Quality: "1080p",
	})
	if err != nil {
		t.Fatalf("ResolveTraced() error = %v", err)
	}
	if got.URL != "https://cdn.example.invalid/live.mkv" {
		t.Errorf("picked %q, want the serving candidate", got.URL)
	}
	// Verification is where the time went here, and the trace has to say so:
	// "the provider is handing out dead links" and "the provider is slow" are
	// different problems with different fixes.
	if trace.VerifyMS <= 0 {
		t.Error("verification time was not recorded; the bar would attribute it to search")
	}
}

// A playback session is not one resolution: the media server re-resolves the
// placeholder on every ffmpeg restart — every seek at minimum. Within the reuse
// window that storm must cost a map lookup, not a search and a probe pass.
func TestResolveReusesARecentAnswer(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn.example.invalid/a.mkv", Resolution: "1080p"},
	}}
	var probes atomic.Int32
	r := NewResolver(stub)
	r.live = func(context.Context, string) error { probes.Add(1); return nil }

	req := ResolveRequest{MediaType: "movie", IMDbID: "tt1", Quality: "1080p"}
	first, trace, err := r.ResolveTraced(context.Background(), req)
	if err != nil {
		t.Fatalf("first ResolveTraced() error = %v", err)
	}
	if trace.Reused {
		t.Error("first resolution reported itself as reused")
	}

	second, trace, err := r.ResolveTraced(context.Background(), req)
	if err != nil {
		t.Fatalf("second ResolveTraced() error = %v", err)
	}
	if !trace.Reused {
		t.Error("second resolution within the window was not reused")
	}
	if second.URL != first.URL {
		t.Errorf("reused URL = %q, want %q", second.URL, first.URL)
	}
	if stub.calls != 1 {
		t.Errorf("search ran %d times, want once", stub.calls)
	}
	if got := probes.Load(); got != 1 {
		t.Errorf("probe ran %d times, want once", got)
	}
}

// Reuse is seek-storm absorption, not storage. Once the window passes, the next
// caller resolves fresh.
func TestResolveReuseExpires(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn.example.invalid/a.mkv", Resolution: "1080p"},
	}}
	r := alwaysLive(NewResolver(stub))
	current := time.Now()
	r.now = func() time.Time { return current }

	req := ResolveRequest{MediaType: "movie", IMDbID: "tt1", Quality: "1080p"}
	if _, _, err := r.ResolveTraced(context.Background(), req); err != nil {
		t.Fatalf("first ResolveTraced() error = %v", err)
	}

	current = current.Add(resolvedTTL + time.Second)
	_, trace, err := r.ResolveTraced(context.Background(), req)
	if err != nil {
		t.Fatalf("second ResolveTraced() error = %v", err)
	}
	if trace.Reused {
		t.Error("an expired answer was reused")
	}
	if stub.calls != 2 {
		t.Errorf("search ran %d times, want a fresh search after expiry", stub.calls)
	}
}

// A caller that just watched the issued URL fail must not be handed the same
// answer back from the window.
func TestResolveFreshBypassesReuse(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn.example.invalid/a.mkv", Resolution: "1080p"},
	}}
	r := alwaysLive(NewResolver(stub))

	req := ResolveRequest{MediaType: "movie", IMDbID: "tt1", Quality: "1080p"}
	if _, _, err := r.ResolveTraced(context.Background(), req); err != nil {
		t.Fatalf("first ResolveTraced() error = %v", err)
	}

	req.Fresh = true
	_, trace, err := r.ResolveTraced(context.Background(), req)
	if err != nil {
		t.Fatalf("fresh ResolveTraced() error = %v", err)
	}
	if trace.Reused {
		t.Error("Fresh was answered from the reuse window")
	}
	if stub.calls != 2 {
		t.Errorf("search ran %d times, want Fresh to force a second", stub.calls)
	}
}

// gatedSearcher blocks inside Search until released, so a test can pile up
// concurrent callers on one in-flight resolution.
type gatedSearcher struct {
	release chan struct{}
	calls   atomic.Int32
	streams []aiostreams.Stream
}

func (g *gatedSearcher) Search(ctx context.Context, _, _ string, _, _ int) ([]aiostreams.Stream, error) {
	g.calls.Add(1)
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.streams, nil
}

// Concurrent resolves for the same unit share one resolution rather than each
// paying — and charging the provider for — their own.
func TestResolveCoalescesConcurrentCallers(t *testing.T) {
	gate := &gatedSearcher{
		release: make(chan struct{}),
		streams: []aiostreams.Stream{{URL: "https://cdn.example.invalid/a.mkv", Resolution: "1080p"}},
	}
	r := alwaysLive(NewResolver(gate))
	req := ResolveRequest{MediaType: "movie", IMDbID: "tt1", Quality: "1080p"}

	const callers = 5
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			_, _, err := r.ResolveTraced(context.Background(), req)
			results <- err
		}()
	}
	// Let the followers queue up behind the leader before releasing it. The
	// sleep only risks releasing early, which would still pass via the reuse
	// window — the assertion that matters is the single search call.
	time.Sleep(50 * time.Millisecond)
	close(gate.release)
	for i := 0; i < callers; i++ {
		if err := <-results; err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := gate.calls.Load(); got != 1 {
		t.Errorf("search ran %d times for %d concurrent callers, want once", got, callers)
	}
}

// The caller who happened to arrive first is not special: its client hanging
// up must not hand a context error to every healthy caller coalesced onto the
// same resolution.
func TestResolveLeaderCancellationDoesNotPoisonFollowers(t *testing.T) {
	gate := &gatedSearcher{
		release: make(chan struct{}),
		streams: []aiostreams.Stream{{URL: "https://cdn.example.invalid/a.mkv", Resolution: "1080p"}},
	}
	r := alwaysLive(NewResolver(gate))
	req := ResolveRequest{MediaType: "movie", IMDbID: "tt1", Quality: "1080p"}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, _, err := r.ResolveTraced(leaderCtx, req)
		leaderDone <- err
	}()
	for gate.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	followerDone := make(chan error, 1)
	go func() {
		_, _, err := r.ResolveTraced(context.Background(), req)
		followerDone <- err
	}()

	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Errorf("leader error = %v, want its own cancellation", err)
	}
	close(gate.release)
	if err := <-followerDone; err != nil {
		t.Errorf("follower error = %v, want the flight to survive the leader leaving", err)
	}
}

// Failures are not reused: the next caller gets a fresh attempt, not a replay
// of the last one's bad luck.
func TestResolveDoesNotReuseFailures(t *testing.T) {
	stub := &stubSearcher{err: errors.New("upstream down")}
	r := alwaysLive(NewResolver(stub))

	req := ResolveRequest{MediaType: "movie", IMDbID: "tt1"}
	if _, _, err := r.ResolveTraced(context.Background(), req); err == nil {
		t.Fatal("expected the first attempt to fail")
	}
	stub.err = nil
	stub.streams = []aiostreams.Stream{{URL: "https://cdn.example.invalid/a.mkv", Resolution: "1080p"}}
	if _, _, err := r.ResolveTraced(context.Background(), req); err != nil {
		t.Fatalf("second attempt error = %v, want the failure not to be replayed", err)
	}
	if stub.calls != 2 {
		t.Errorf("search ran %d times, want 2", stub.calls)
	}
}

// A candidate that failed its check seconds ago is still dead: it must be
// skipped on remembered evidence rather than charged its timeout again.
func TestResolveRemembersProbeFailures(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn.example.invalid/dead.mkv", Resolution: "1080p"},
		{URL: "https://cdn.example.invalid/live.mkv", Resolution: "1080p"},
	}}
	deadProbes := 0
	r := NewResolver(stub)
	current := time.Now()
	r.now = func() time.Time { return current }
	r.live = func(_ context.Context, u string) error {
		if strings.Contains(u, "dead") {
			deadProbes++
			return errors.New("not serving")
		}
		return nil
	}

	req := ResolveRequest{MediaType: "movie", IMDbID: "tt1", Quality: "1080p"}
	if _, _, err := r.ResolveTraced(context.Background(), req); err != nil {
		t.Fatalf("first ResolveTraced() error = %v", err)
	}

	// Past the reuse window but inside the probe-failure window: the resolve
	// runs again, and the dead candidate must be skipped without a probe.
	current = current.Add(resolvedTTL + time.Second)
	got, _, err := r.ResolveTraced(context.Background(), req)
	if err != nil {
		t.Fatalf("second ResolveTraced() error = %v", err)
	}
	if got.URL != "https://cdn.example.invalid/live.mkv" {
		t.Errorf("URL = %q, want the serving candidate", got.URL)
	}
	if deadProbes != 1 {
		t.Errorf("dead candidate probed %d times, want once", deadProbes)
	}

	// Fresh demands the re-probe: remembered evidence is exactly what a caller
	// reporting a failure wants re-examined.
	req.Fresh = true
	if _, _, err := r.ResolveTraced(context.Background(), req); err != nil {
		t.Fatalf("fresh ResolveTraced() error = %v", err)
	}
	if deadProbes != 2 {
		t.Errorf("dead candidate probed %d times after Fresh, want 2", deadProbes)
	}
}

// A remembered failure must not refresh its own expiry, or one observation
// would blacklist a candidate forever.
func TestRememberedProbeFailureExpires(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn.example.invalid/dead.mkv", Resolution: "1080p"},
		{URL: "https://cdn.example.invalid/live.mkv", Resolution: "1080p"},
	}}
	deadProbes := 0
	r := NewResolver(stub)
	current := time.Now()
	r.now = func() time.Time { return current }
	r.live = func(_ context.Context, u string) error {
		if strings.Contains(u, "dead") {
			deadProbes++
			return errors.New("not serving")
		}
		return nil
	}

	req := ResolveRequest{MediaType: "movie", IMDbID: "tt1", Quality: "1080p"}
	if _, _, err := r.ResolveTraced(context.Background(), req); err != nil {
		t.Fatalf("first ResolveTraced() error = %v", err)
	}
	// A resolve inside the failure window skips the dead candidate — and must
	// not push its expiry out.
	current = current.Add(resolvedTTL + time.Second)
	if _, _, err := r.ResolveTraced(context.Background(), req); err != nil {
		t.Fatalf("second ResolveTraced() error = %v", err)
	}
	// Past the original failure window, the candidate deserves a real check.
	current = current.Add(probeFailTTL)
	if _, _, err := r.ResolveTraced(context.Background(), req); err != nil {
		t.Fatalf("third ResolveTraced() error = %v", err)
	}
	if deadProbes != 2 {
		t.Errorf("dead candidate probed %d times, want a re-probe after expiry", deadProbes)
	}
}

// freshCountingSearcher records which search entry point was used.
type freshCountingSearcher struct {
	stubSearcher
	freshCalls int
}

func (f *freshCountingSearcher) SearchFresh(ctx context.Context, mediaType, imdbID string, season, episode int) ([]aiostreams.Stream, error) {
	f.freshCalls++
	return f.stubSearcher.Search(ctx, mediaType, imdbID, season, episode)
}

// A Fresh resolution exists because a previously-issued URL just failed. When
// the searcher can bypass its own result cache, Fresh must take that path —
// the cached result set is what produced the dead URL.
func TestResolveFreshBypassesTheSearchCacheToo(t *testing.T) {
	stub := &freshCountingSearcher{stubSearcher: stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn.example.invalid/a.mkv", Resolution: "1080p"},
	}}}
	r := alwaysLive(NewResolver(stub))

	req := ResolveRequest{MediaType: "movie", IMDbID: "tt1", Quality: "1080p"}
	if _, _, err := r.ResolveTraced(context.Background(), req); err != nil {
		t.Fatalf("ResolveTraced() error = %v", err)
	}
	if stub.freshCalls != 0 {
		t.Errorf("plain resolve used SearchFresh %d times, want 0", stub.freshCalls)
	}

	req.Fresh = true
	if _, _, err := r.ResolveTraced(context.Background(), req); err != nil {
		t.Fatalf("fresh ResolveTraced() error = %v", err)
	}
	if stub.freshCalls != 1 {
		t.Errorf("Fresh used SearchFresh %d times, want 1", stub.freshCalls)
	}
}

// gatedFreshSearcher gates ordinary Search but answers SearchFresh
// immediately, so a test can hold an ordinary flight open while a Fresh caller
// resolves past it.
type gatedFreshSearcher struct {
	gatedSearcher
	freshCalls atomic.Int32
}

func (g *gatedFreshSearcher) SearchFresh(context.Context, string, string, int, int) ([]aiostreams.Stream, error) {
	g.freshCalls.Add(1)
	return g.streams, nil
}

// A Fresh caller exists because a URL just failed. Riding an ordinary flight —
// which may be about to re-serve exactly that URL from a cache — would defeat
// the point; Fresh leads its own fully-fresh resolution instead.
func TestFreshDoesNotJoinAnOrdinaryFlight(t *testing.T) {
	gate := &gatedFreshSearcher{gatedSearcher: gatedSearcher{
		release: make(chan struct{}),
		streams: []aiostreams.Stream{{URL: "https://cdn.example.invalid/a.mkv", Resolution: "1080p"}},
	}}
	r := alwaysLive(NewResolver(gate))
	req := ResolveRequest{MediaType: "movie", IMDbID: "tt1", Quality: "1080p"}

	ordinaryDone := make(chan error, 1)
	go func() {
		_, _, err := r.ResolveTraced(context.Background(), req)
		ordinaryDone <- err
	}()
	for gate.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	// The ordinary flight is still in the air; Fresh must not wait on it.
	fresh := req
	fresh.Fresh = true
	if _, _, err := r.ResolveTraced(context.Background(), fresh); err != nil {
		t.Fatalf("fresh ResolveTraced() error = %v", err)
	}
	if got := gate.freshCalls.Load(); got != 1 {
		t.Errorf("SearchFresh ran %d times, want the fresh caller to lead its own flight", got)
	}

	close(gate.release)
	if err := <-ordinaryDone; err != nil {
		t.Errorf("ordinary caller error = %v, want it unaffected", err)
	}
}

// A provider that has finished fetching must not stay blacklisted: a probe
// success clears the remembered failure immediately, not at its expiry.
func TestProbeSuccessClearsRememberedFailure(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn.example.invalid/flaky.mkv", Resolution: "1080p"},
		{URL: "https://cdn.example.invalid/steady.mkv", Resolution: "1080p"},
	}}
	flakyDown := true
	flakyProbes := 0
	r := NewResolver(stub)
	current := time.Now()
	r.now = func() time.Time { return current }
	r.live = func(_ context.Context, u string) error {
		if strings.Contains(u, "flaky") {
			flakyProbes++
			if flakyDown {
				return errors.New("not serving")
			}
		}
		return nil
	}

	req := ResolveRequest{MediaType: "movie", IMDbID: "tt1", Quality: "1080p"}
	got, _, err := r.ResolveTraced(context.Background(), req)
	if err != nil || got.URL != "https://cdn.example.invalid/steady.mkv" {
		t.Fatalf("first resolve = %q (%v), want the steady candidate", got.URL, err)
	}

	// The provider finishes fetching; a Fresh resolve re-examines and clears
	// the remembered failure.
	flakyDown = false
	fresh := req
	fresh.Fresh = true
	got, _, err = r.ResolveTraced(context.Background(), fresh)
	if err != nil || got.URL != "https://cdn.example.invalid/flaky.mkv" {
		t.Fatalf("fresh resolve = %q (%v), want the recovered candidate", got.URL, err)
	}

	// An ordinary resolve after the reuse window must probe the recovered
	// candidate rather than skip it on the stale memory.
	current = current.Add(resolvedTTL + time.Second)
	got, _, err = r.ResolveTraced(context.Background(), req)
	if err != nil || got.URL != "https://cdn.example.invalid/flaky.mkv" {
		t.Fatalf("third resolve = %q (%v), want the recovered candidate", got.URL, err)
	}
	if flakyProbes != 3 {
		t.Errorf("recovered candidate probed %d times, want 3 (fail, fresh success, ordinary success)", flakyProbes)
	}
}

// A failure still reports where the time went — that is when it matters most.
func TestResolveTracedReportsTimingOnFailure(t *testing.T) {
	r := NewResolver(&stubSearcher{err: errors.New("upstream down")})
	_, trace, err := r.ResolveTraced(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt1",
	})
	if err == nil {
		t.Fatal("expected the search failure to surface")
	}
	if trace.SearchMS < 0 {
		t.Errorf("search time = %d", trace.SearchMS)
	}
}
