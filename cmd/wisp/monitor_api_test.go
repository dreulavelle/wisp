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
