package main

import (
	"context"
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
	"github.com/dreulavelle/wisp/internal/store"
)

func testApp(t *testing.T) *app {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.DiscardHandler)
	a := &app{
		store: st, log: log, startedAt: time.Now(),
		meta:    metadata.New("", nil),
		webhook: notify.New(notify.Options{}, log),
	}
	a.mon = monitor.New(st, a.meta, a, time.Hour, 4, 7*24*time.Hour, log) // Run not started → Intake only records
	return a
}

func TestMonitorCRUD(t *testing.T) {
	a := testApp(t)
	// Create
	rec := httptest.NewRecorder()
	a.handleCreateMonitor(rec, httptest.NewRequest(http.MethodPost, "/api/monitors",
		strings.NewReader(`{"media_type":"movie","imdb_id":"tt1375666","title":"Inception","year":2010,"qualities":["4k","1080p"]}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	items, _ := a.store.ListMonitored(context.Background())
	if len(items) != 1 {
		t.Fatalf("monitors after create = %d", len(items))
	}
	if got := items[0].Qualities; len(got) != 2 || got[0] != "2160p" || got[1] != "1080p" {
		t.Fatalf("qualities normalized = %v", got) // "4k" → "2160p"
	}
	key := items[0].Key

	// List
	rec = httptest.NewRecorder()
	a.handleListMonitors(rec, httptest.NewRequest(http.MethodGet, "/api/monitors", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Inception") {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	// Delete
	rec = httptest.NewRecorder()
	a.handleDeleteMonitor(rec, httptest.NewRequest(http.MethodDelete, "/api/monitors?key="+key, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if n, _ := a.store.CountMonitored(context.Background()); n != 0 {
		t.Fatalf("monitors after delete = %d", n)
	}
}

func TestLazyResolution(t *testing.T) {
	a := testApp(t)
	a.lazyResolution = true

	target := monitor.Target{
		MediaType: "movie",
		IMDbID:    "tt1375666",
		Title:     "Inception",
		Year:      2010,
		Quality:   "1080p",
		Category:  "movies",
	}

	// 1. First Pin should create a placeholder immediately
	outcome, err := a.Pin(context.Background(), target)
	if err != nil {
		t.Fatalf("first Pin failed: %v", err)
	}
	if outcome != monitor.Pinned {
		t.Fatalf("expected outcome monitor.Pinned, got %v", outcome)
	}

	// Verify placeholder is written in store
	pins, err := a.store.PinsByMedia(context.Background(), "tt1375666")
	if err != nil {
		t.Fatalf("PinsByMedia failed: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("expected 1 pin, got %d", len(pins))
	}
	p := pins[0]
	if p.SourceURL != "" {
		t.Fatalf("expected empty SourceURL for placeholder, got %q", p.SourceURL)
	}
	if p.Size != 1 {
		t.Fatalf("expected placeholder Size to be 1, got %d", p.Size)
	}

	// 2. PinnedKeys should NOT return placeholder pins as pinned
	keys, err := a.PinnedKeys(context.Background(), "tt1375666")
	if err != nil {
		t.Fatalf("PinnedKeys failed: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected 0 pinned keys for placeholder pins, got %d", len(keys))
	}
}

// A monitor created without an explicit quality list writes its placeholder under
// the "1080p" default label, but resolves best-available — so the real stream can
// land at a different tier and therefore a different virtual path. The upgrade
// must retire the placeholder rather than orphan a 1-byte file the media server
// has already imported.
func TestLazyResolutionUpgradeRetiresPlaceholder(t *testing.T) {
	backend := wispTestBackend(t)
	defer backend.Close()

	a := testApp(t)
	a.lazyResolution = true
	a.aio = aiostreams.New(backend.URL+"/stremio/uuid/blob/manifest.json", "pw")
	a.prober = testProber()
	renames := make(chan [2]string, 4)
	a.webhook = recordingNotifier{renames: renames}

	// No Quality: the placeholder labels itself 1080p, the resolve is
	// unconstrained and takes the top-ranked 2160p stream.
	target := monitor.Target{
		MediaType: "movie", IMDbID: "tt1375666", TMDbID: "27205", // TMDbID skips Cinemeta
		Title: "Inception", Year: 2010, Category: "movies",
	}

	if outcome, err := a.Pin(context.Background(), target); err != nil || outcome != monitor.Pinned {
		t.Fatalf("placeholder Pin = %v, %v", outcome, err)
	}
	pins, err := a.store.PinsByMedia(context.Background(), "tt1375666")
	if err != nil || len(pins) != 1 {
		t.Fatalf("pins after placeholder = %d (err %v), want 1", len(pins), err)
	}
	placeholderPath := pins[0].VirtualPath
	if pins[0].SourceURL != "" || pins[0].Quality != "1080p" {
		t.Fatalf("placeholder = %#v, want empty SourceURL at the 1080p default", pins[0])
	}

	// Second Pin takes the upgrade branch: resolve for real.
	if outcome, err := a.Pin(context.Background(), target); err != nil || outcome != monitor.Pinned {
		t.Fatalf("upgrade Pin = %v, %v", outcome, err)
	}

	pins, err = a.store.PinsByMedia(context.Background(), "tt1375666")
	if err != nil {
		t.Fatalf("PinsByMedia failed: %v", err)
	}
	if len(pins) != 1 {
		var paths []string
		for _, p := range pins {
			paths = append(paths, p.VirtualPath)
		}
		t.Fatalf("pins after upgrade = %d %v, want exactly 1 (placeholder retired)", len(pins), paths)
	}
	resolved := pins[0]
	if resolved.VirtualPath == placeholderPath {
		t.Fatalf("resolved pin kept the placeholder path %q", placeholderPath)
	}
	if resolved.SourceURL == "" || resolved.Quality != "2160p" {
		t.Fatalf("resolved pin = %#v, want a 2160p pin with a SourceURL", resolved)
	}
	if p, _ := a.store.ByPath(context.Background(), placeholderPath); p != nil {
		t.Fatalf("orphaned placeholder still in store at %q", placeholderPath)
	}

	// The media server imported the placeholder, so it must be told this is a
	// rename — not left holding a phantom entry that never gets a delete.
	select {
	case r := <-renames:
		if r[0] != placeholderPath || r[1] != resolved.VirtualPath {
			t.Fatalf("rename webhook = %q → %q, want %q → %q", r[0], r[1], placeholderPath, resolved.VirtualPath)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no rename webhook for the retired placeholder")
	}

	// Now that it is really resolved, the scheduler must see it as pinned.
	keys, err := a.PinnedKeys(context.Background(), "tt1375666")
	if err != nil || len(keys) != 1 {
		t.Fatalf("PinnedKeys after upgrade = %d (err %v), want 1", len(keys), err)
	}
}

// recordingNotifier captures Import and Rename calls; Delete is a no-op. Unlike
// the real notifier it delivers synchronously, so a test never races the fanout.
// A nil channel means the test doesn't care about that event.
type recordingNotifier struct {
	imports chan [2]string
	renames chan [2]string
}

func (n recordingNotifier) Import(_ context.Context, mediaType, virtualPath string) {
	select {
	case n.imports <- [2]string{mediaType, virtualPath}:
	default:
	}
}
func (n recordingNotifier) Rename(_ context.Context, _, previousPath, newPath string) {
	select {
	case n.renames <- [2]string{previousPath, newPath}:
	default:
	}
}
func (n recordingNotifier) Delete(context.Context, string, string) {}
