package plugin

import (
	"context"
	"errors"
	"net/http"
	"testing"

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
			want: ResolveRequest{MediaType: "movie", IMDbID: "tt0133093"},
		},
		{
			name: "series",
			path: "/resolve/series/tt0944947/1/9",
			want: ResolveRequest{MediaType: "series", IMDbID: "tt0944947", Season: 1, Episode: 9},
		},
		{
			name: "double digit season and episode",
			path: "/resolve/series/tt0944947/10/22",
			want: ResolveRequest{MediaType: "series", IMDbID: "tt0944947", Season: 10, Episode: 22},
		},
		{
			// Silo mounts plugin routes under a prefix whose depth we should
			// not have to know.
			name: "tolerates a mount prefix",
			path: "/api/v1/plugins/20/resolve/movie/tt0133093",
			want: ResolveRequest{MediaType: "movie", IMDbID: "tt0133093"},
		},
		{
			name: "no leading slash",
			path: "resolve/movie/tt0133093",
			want: ResolveRequest{MediaType: "movie", IMDbID: "tt0133093"},
		},
		{
			name: "special 0 season",
			path: "/resolve/series/tt0944947/0/1",
			want: ResolveRequest{MediaType: "series", IMDbID: "tt0944947", Season: 0, Episode: 1},
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
		"/resolve/movie/12345",          // tmdb id, not imdb
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

	got, err := NewResolver(stub).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt0133093", Quality: "2160p",
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

	got, err := NewResolver(stub).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt0133093", Quality: "2160p",
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

	got, err := NewResolver(stub).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt0133093", Quality: "1080p",
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

	got, err := NewResolver(stub).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt0133093",
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

	got, err := NewResolver(stub).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt0133093", Quality: "1080p",
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

	_, err := NewResolver(stub).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt0133093",
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("error = %v, want ErrNoMatch", err)
	}
}

func TestResolvePropagatesSearchError(t *testing.T) {
	sentinel := errors.New("upstream exploded")
	stub := &stubSearcher{err: sentinel}

	_, err := NewResolver(stub).Resolve(context.Background(), ResolveRequest{
		MediaType: "movie", IMDbID: "tt0133093",
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the underlying search error", err)
	}
}

func TestResolvePassesEpisodeCoordinates(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{{URL: "https://cdn/a.mkv", Resolution: "1080p"}}}

	_, err := NewResolver(stub).Resolve(context.Background(), ResolveRequest{
		MediaType: "series", IMDbID: "tt0944947", Season: 3, Episode: 9,
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
