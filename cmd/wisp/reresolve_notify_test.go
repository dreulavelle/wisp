package main

import (
	"context"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
	"github.com/dreulavelle/wisp/internal/store"
)

// reResolveNotifyApp builds an app wired to the shared test backend with a
// recording notifier, plus the pin the test will resolve.
func reResolveNotifyApp(t *testing.T, imports chan [2]string) *app {
	t.Helper()
	backend := wispTestBackend(t)
	t.Cleanup(backend.Close)

	a := testApp(t)
	a.aio = aiostreams.New(backend.URL+"/stremio/uuid/blob/manifest.json", "pw")
	a.prober = testProber()
	a.webhook = recordingNotifier{imports: imports}
	return a
}

const reResolveNotifyPath = "movies/Inception (2010) [tmdb-27205]/Inception (2010) - [1080p].mkv"

// The ARR webhook — not the WebSocket event — is what the deployed media server
// listens on, so an on-demand placeholder resolve must re-announce the path.
// Without it the catalog keeps the 1-byte placeholder size indefinitely.
func TestReResolvePlaceholderFiresImportWebhook(t *testing.T) {
	imports := make(chan [2]string, 4)
	a := reResolveNotifyApp(t, imports)

	p := store.Pin{
		MediaType: "movie", IMDbID: "tt1375666", TMDbID: "27205",
		Title: "Inception", Year: 2010, Quality: "1080p",
		VirtualPath: reResolveNotifyPath, Size: 1, ResolvedAt: time.Now(),
	}
	if err := a.store.Upsert(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := a.reResolve(context.Background(), &p); err != nil {
		t.Fatalf("reResolve: %v", err)
	}
	if p.SourceURL == "" || p.Size <= 1 {
		t.Fatalf("resolved pin = %#v, want a real SourceURL and size", p)
	}

	select {
	case got := <-imports:
		if got[0] != "movie" || got[1] != reResolveNotifyPath {
			t.Fatalf("import webhook = %q %q, want %q %q", got[0], got[1], "movie", reResolveNotifyPath)
		}
	default:
		t.Fatal("placeholder resolution did not fire an import webhook")
	}
}

// A self-heal keeps the same path and size, so re-announcing it would only make
// a flaky upstream storm the media server with rescans mid-playback.
func TestReResolveSelfHealDoesNotFireWebhook(t *testing.T) {
	imports := make(chan [2]string, 4)
	a := reResolveNotifyApp(t, imports)

	p := store.Pin{
		MediaType: "movie", IMDbID: "tt1375666", TMDbID: "27205",
		Title: "Inception", Year: 2010, Quality: "1080p",
		VirtualPath: reResolveNotifyPath, SourceURL: "http://dead.invalid/stream",
		Size: 123, ResolvedAt: time.Now(),
	}
	if err := a.store.Upsert(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := a.reResolve(context.Background(), &p); err != nil {
		t.Fatalf("reResolve: %v", err)
	}

	select {
	case got := <-imports:
		t.Fatalf("self-heal fired an import webhook: %q %q", got[0], got[1])
	default:
	}
}

// A failed resolve leaves the store untouched, so nothing may be announced.
func TestReResolveFailureDoesNotFireWebhook(t *testing.T) {
	imports := make(chan [2]string, 4)
	a := reResolveNotifyApp(t, imports)
	// Point at a manifest the test backend does not serve so the search fails.
	a.aio = aiostreams.New("http://127.0.0.1:1/stremio/uuid/blob/manifest.json", "pw")

	p := store.Pin{
		MediaType: "movie", IMDbID: "tt1375666", TMDbID: "27205",
		Title: "Inception", Year: 2010, Quality: "1080p",
		VirtualPath: reResolveNotifyPath, Size: 1, ResolvedAt: time.Now(),
	}
	if err := a.store.Upsert(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := a.reResolve(context.Background(), &p); err == nil {
		t.Fatal("expected reResolve to fail against an unreachable backend")
	}

	select {
	case got := <-imports:
		t.Fatalf("failed resolve fired an import webhook: %q %q", got[0], got[1])
	default:
	}
}
