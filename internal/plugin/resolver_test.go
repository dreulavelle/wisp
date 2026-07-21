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
