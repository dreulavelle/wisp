package notify

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// statusServer answers every request with a fixed status code.
func statusServer(code int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
}

// A 202 is what Silo's Autoscan intake returns for an accepted webhook, so the
// success test pins that specific code rather than a bare 200: treating only
// 200 as success would report a fully healthy production path as 100% failing.
func TestDeliveryCountsSuccessOnAccepted(t *testing.T) {
	server := statusServer(http.StatusAccepted)
	defer server.Close()

	tgt := newArrTarget(server.URL, "/mnt/wisp", discardLog())
	tgt.Import(context.Background(), "movie", "movies/New/movie.mkv")

	if got := tgt.stats.delivered.Load(); got != 1 {
		t.Errorf("delivered = %d, want 1", got)
	}
	if got := tgt.stats.failed.Load(); got != 0 {
		t.Errorf("failed = %d, want 0", got)
	}
}

func TestDeliveryCountsFailureOnNon2xx(t *testing.T) {
	server := statusServer(http.StatusInternalServerError)
	defer server.Close()

	tgt := newArrTarget(server.URL, "/mnt/wisp", discardLog())
	tgt.Import(context.Background(), "movie", "movies/New/movie.mkv")

	if got := tgt.stats.failed.Load(); got != 1 {
		t.Errorf("failed = %d, want 1", got)
	}
	if got := tgt.stats.delivered.Load(); got != 0 {
		t.Errorf("delivered = %d, want 0", got)
	}
}

// A transport error is the failure mode a durable journal would actually be
// built to survive, so it must land in the failure counter and not vanish.
func TestDeliveryCountsFailureOnTransportError(t *testing.T) {
	server := statusServer(http.StatusAccepted)
	url := server.URL
	server.Close() // nothing is listening now; the POST cannot connect

	tgt := newArrTarget(url, "/mnt/wisp", discardLog())
	tgt.Import(context.Background(), "movie", "movies/New/movie.mkv")

	if got := tgt.stats.failed.Load(); got != 1 {
		t.Errorf("failed = %d, want 1", got)
	}
	if got := tgt.stats.delivered.Load(); got != 0 {
		t.Errorf("delivered = %d, want 0", got)
	}
}

func TestMediaBrowserDeliveryOutcomesAreCounted(t *testing.T) {
	ok := statusServer(http.StatusNoContent)
	defer ok.Close()
	bad := statusServer(http.StatusUnauthorized)
	defer bad.Close()

	good := newMediaBrowserTarget(mediaBrowserConfig{
		flavor: "jellyfin", baseURL: ok.URL, createType: "Modified", mountPath: "/mnt/wisp",
	}, discardLog())
	good.Import(context.Background(), "movie", "movies/New/movie.mkv")
	if got := good.stats.delivered.Load(); got != 1 {
		t.Errorf("jellyfin delivered = %d, want 1", got)
	}

	rejected := newMediaBrowserTarget(mediaBrowserConfig{
		flavor: "emby", baseURL: bad.URL, pathPrefix: "/emby", createType: "Created", mountPath: "/mnt/wisp",
	}, discardLog())
	rejected.Import(context.Background(), "movie", "movies/New/movie.mkv")
	if got := rejected.stats.failed.Load(); got != 1 {
		t.Errorf("emby failed = %d, want 1", got)
	}
}

func TestPlexRefreshOutcomesAreCounted(t *testing.T) {
	var (
		mu          sync.Mutex
		refreshes   []plexRefresh
		sectionHits int
	)
	server := plexServer(t, &refreshes, &mu, &sectionHits)
	defer server.Close()

	tgt := newPlexTarget(server.URL, "tok", "/mnt/wisp", discardLog())
	tgt.Import(context.Background(), "movie", "movies/New/movie.mkv")

	if got := tgt.stats.delivered.Load(); got != 1 {
		t.Errorf("delivered = %d, want 1", got)
	}
	if got := tgt.stats.dropped.Load(); got != 0 {
		t.Errorf("dropped = %d, want 0", got)
	}
}

// A path outside every configured section is thrown away without any request
// being made. That is a silent loss mode distinct from a failed delivery, so it
// must land in dropped and must NOT inflate the failure rate — otherwise a
// misconfigured section list would masquerade as an unreliable network and
// argue for a journal that would not have helped.
func TestPlexUnmatchedPathCountsAsDroppedNotFailed(t *testing.T) {
	var (
		mu          sync.Mutex
		refreshes   []plexRefresh
		sectionHits int
	)
	server := plexServer(t, &refreshes, &mu, &sectionHits)
	defer server.Close()

	tgt := newPlexTarget(server.URL, "tok", "/mnt/wisp", discardLog())
	// The section list covers /mnt/wisp/movies and /mnt/wisp/shows only.
	tgt.Import(context.Background(), "movie", "music/Album/track.mkv")

	if got := tgt.stats.dropped.Load(); got != 1 {
		t.Errorf("dropped = %d, want 1", got)
	}
	if got := tgt.stats.failed.Load(); got != 0 {
		t.Errorf("failed = %d, want 0", got)
	}
	if got := tgt.stats.delivered.Load(); got != 0 {
		t.Errorf("delivered = %d, want 0", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(refreshes) != 0 {
		t.Errorf("refreshes = %d, want 0", len(refreshes))
	}
}

// A failed section lookup loses the event before any refresh is attempted, but
// the cause is a failed round-trip to Plex rather than a configuration gap, so
// it belongs in failed rather than dropped.
func TestPlexSectionLookupFailureCountsAsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	tgt := newPlexTarget(server.URL, "tok", "/mnt/wisp", discardLog())
	tgt.Import(context.Background(), "movie", "movies/New/movie.mkv")

	if got := tgt.stats.failed.Load(); got != 1 {
		t.Errorf("failed = %d, want 1", got)
	}
	if got := tgt.stats.dropped.Load(); got != 0 {
		t.Errorf("dropped = %d, want 0", got)
	}
}

// Multi.Metrics must report every configured target separately: the whole point
// of the per-target split is telling which consumer is dropping events.
func TestMultiMetricsReportsEveryTargetSeparately(t *testing.T) {
	ok := statusServer(http.StatusAccepted)
	defer ok.Close()
	bad := statusServer(http.StatusBadGateway)
	defer bad.Close()

	m := New(Options{ArrWebhookURL: ok.URL, JellyfinURL: bad.URL, MountPath: "/mnt/wisp"}, discardLog())
	m.Import(context.Background(), "movie", "movies/New/movie.mkv")
	m.Close(context.Background())

	snap := m.Metrics()
	if len(snap) != 2 {
		t.Fatalf("targets = %d, want 2", len(snap))
	}
	by := map[string]TargetMetrics{}
	for _, s := range snap {
		by[s.Target] = s
	}
	if got := by["arr-webhook"]; got.Delivered != 1 || got.Failed != 0 {
		t.Errorf("arr-webhook = %+v, want 1 delivered / 0 failed", got)
	}
	if got := by["jellyfin"]; got.Failed != 1 || got.Delivered != 0 {
		t.Errorf("jellyfin = %+v, want 0 delivered / 1 failed", got)
	}
}

// A batch is one delivery attempt no matter how many files it carries, so the
// success rate stays a rate over requests rather than being weighted by burst
// size. Without this, one large burst would dominate the ratio.
func TestCoalescedBatchCountsAsOneDelivery(t *testing.T) {
	server := statusServer(http.StatusAccepted)
	defer server.Close()

	tgt := newArrTarget(server.URL, "/mnt/wisp", discardLog())
	tgt.ImportBatch(context.Background(), importBatch{
		mediaType: "series",
		dir:       "shows/Show/Season 01",
		files: []string{
			"shows/Show/Season 01/e01.mkv",
			"shows/Show/Season 01/e02.mkv",
			"shows/Show/Season 01/e03.mkv",
		},
	})

	if got := tgt.stats.delivered.Load(); got != 1 {
		t.Errorf("delivered = %d, want 1 (one request for the whole batch)", got)
	}
}
