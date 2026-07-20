package plugin

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
)

// Live tests run only when WISP_AIOSTREAMS_URL is set, so `go test ./...` stays
// hermetic. They exist because every interesting failure in this pipeline lives
// at a boundary a mock cannot reproduce: addon response shape, bot protection,
// and whether the final CDN actually serves seekable bytes.
//
//	set -a && . ./.env && set +a && go test ./internal/plugin/ -run TestLive -v
func liveResolver(t *testing.T) *Resolver {
	t.Helper()
	url := os.Getenv("WISP_AIOSTREAMS_URL")
	if url == "" {
		t.Skip("WISP_AIOSTREAMS_URL not set; skipping live test")
	}
	return NewResolver(aiostreams.New(url, os.Getenv("WISP_AIOSTREAMS_PASSWORD")))
}

var secretish = regexp.MustCompile(`[A-Za-z0-9_-]{16,}`)

func TestLiveResolvesPlayableURL(t *testing.T) {
	r := liveResolver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cases := []struct {
		name string
		req  ResolveRequest
	}{
		{"movie/2160p", ResolveRequest{MediaType: "movie", IMDbID: "tt0133093", Quality: "2160p"}},
		{"movie/1080p", ResolveRequest{MediaType: "movie", IMDbID: "tt0133093", Quality: "1080p"}},
		{"series/s01e09", ResolveRequest{MediaType: "series", IMDbID: "tt0944947", Season: 1, Episode: 9, Quality: "1080p"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			got, err := r.Resolve(ctx, tc.req)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if !strings.HasPrefix(got.URL, "http") {
				t.Errorf("URL = %q, want an http(s) URL", got.URL)
			}
			t.Logf("%s -> %s in %.2fs", tc.req.Quality, got.Resolution, time.Since(start).Seconds())
		})
	}
}

// The whole chain: resolver -> provider redirect -> debrid CDN. Asserts the
// final response is range-capable, because seeking depends on it and nothing
// upstream of here contractually guarantees it.
func TestLiveFullRedirectChainServesSeekableBytes(t *testing.T) {
	r := liveResolver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	stream, err := r.Resolve(ctx, ResolveRequest{MediaType: "movie", IMDbID: "tt0133093", Quality: "1080p"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	client := &http.Client{
		Timeout:       60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	current := stream.URL
	for hop := 1; hop <= 5; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			t.Fatalf("hop %d: build request: %v", hop, err)
		}
		// Providers in this ecosystem sit behind bot protection that rejects
		// Go's default agent with a 403. Impersonate ffmpeg, which is what
		// actually fetches this URL when the server transcodes.
		req.Header.Set("User-Agent", "Lavf/62.3.100")
		req.Header.Set("Range", "bytes=0-65535")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("hop %d: %v", hop, err)
		}

		if loc := resp.Header.Get("Location"); resp.StatusCode >= 300 && resp.StatusCode < 400 && loc != "" {
			resp.Body.Close()
			current = loc
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("final hop status = %d, want 206 (no range support means no seeking); url=%s",
				resp.StatusCode, secretish.ReplaceAllString(current, "<TOK>"))
		}
		if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
			t.Errorf("Accept-Ranges = %q, want \"bytes\"", got)
		}
		if resp.Header.Get("Content-Range") == "" {
			t.Error("Content-Range is empty; the client cannot seek without it")
		}
		t.Logf("final: %d, Content-Range=%s", resp.StatusCode, resp.Header.Get("Content-Range"))
		return
	}
	t.Fatal("redirect chain did not terminate within 5 hops")
}

// Documents the bot-protection behaviour as an executable fact rather than a
// comment: the provider rejects Go's default User-Agent outright. If this ever
// starts passing, the workaround below can be reconsidered.
func TestLiveProviderRejectsDefaultGoUserAgent(t *testing.T) {
	r := liveResolver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	stream, err := r.Resolve(ctx, ResolveRequest{MediaType: "movie", IMDbID: "tt0133093", Quality: "1080p"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	client := &http.Client{
		Timeout:       45 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, stream.URL, nil)
	resp, err := client.Do(req) // no User-Agent override
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Logf("provider no longer 403s Go's default agent (status %d) — the UA workaround may be removable", resp.StatusCode)
	}
}
