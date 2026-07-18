package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/monitor"
	"github.com/dreulavelle/wisp/internal/notify"
	"github.com/dreulavelle/wisp/internal/store"
)

func scheduleTestApp(t *testing.T) *app {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.DiscardHandler)
	a := &app{store: st, log: log, startedAt: time.Now(),
		meta:    metadata.New("", nil),
		webhook: notify.New(notify.Options{}, log)}
	a.mon = monitor.New(st, a.meta, a, 2*time.Hour, log)
	return a
}

func TestHandleScheduleReportsState(t *testing.T) {
	a := scheduleTestApp(t)
	ctx := context.Background()
	now := time.Now()

	// A waiting movie (release in the future), one requested tier, nothing pinned.
	if err := a.store.PutMonitored(ctx, store.Monitored{
		Key: "movie:tt1", MediaType: "movie", IMDbID: "tt1", Title: "Future Film",
		Qualities: []string{"1080p"}, DueAt: now.Add(48 * time.Hour), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	// A completed movie with a pin — pending 0.
	if err := a.store.PutMonitored(ctx, store.Monitored{
		Key: "movie:tt2", MediaType: "movie", IMDbID: "tt2", Title: "Done Film",
		Qualities: []string{"1080p"}, DueAt: now.Add(time.Hour), Enabled: true, Completed: true,
	}); err != nil {
		t.Fatal(err)
	}
	_ = a.store.Upsert(ctx, store.Pin{IMDbID: "tt2", MediaType: "movie", Quality: "1080p",
		VirtualPath: "movies/Done Film [1080p].mkv", ResolvedAt: now})
	// A due series (past DueAt), two tiers requested, one pinned → 1 pending.
	if err := a.store.PutMonitored(ctx, store.Monitored{
		Key: "series:tt3", MediaType: "series", IMDbID: "tt3", Title: "Show",
		Qualities: []string{"1080p", "2160p"}, DueAt: now.Add(-time.Minute), Enabled: true,
		LastChecked: now.Add(-2 * time.Minute), LastError: "boom",
	}); err != nil {
		t.Fatal(err)
	}
	_ = a.store.Upsert(ctx, store.Pin{IMDbID: "tt3", MediaType: "series", Season: 1, Episode: 1,
		Quality: "1080p", VirtualPath: "shows/Show/Season 01/Show S01E01 [1080p].mkv", ResolvedAt: now})

	rec := httptest.NewRecorder()
	a.handleSchedule(rec, httptest.NewRequest(http.MethodGet, "/api/schedule", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var resp scheduleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if resp.IntervalSeconds != 7200 {
		t.Fatalf("interval_seconds = %d, want 7200", resp.IntervalSeconds)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(resp.Items))
	}
	byKey := map[string]scheduleItem{}
	for _, it := range resp.Items {
		byKey[it.Key] = it
	}

	future := byKey["movie:tt1"]
	if future.State != "waiting" {
		t.Fatalf("tt1 state = %q, want waiting", future.State)
	}
	if future.NextRelease == nil {
		t.Fatal("tt1 should carry next_release")
	}
	if future.PendingTargets != 1 {
		t.Fatalf("tt1 pending = %d, want 1", future.PendingTargets)
	}

	done := byKey["movie:tt2"]
	if done.State != "completed" || done.PendingTargets != 0 {
		t.Fatalf("tt2 = %+v, want completed/0", done)
	}
	if done.Pinned != 1 {
		t.Fatalf("tt2 pinned = %d, want 1", done.Pinned)
	}

	series := byKey["series:tt3"]
	if series.State != "pending" {
		t.Fatalf("tt3 state = %q, want pending", series.State)
	}
	if series.LastError != "boom" {
		t.Fatalf("tt3 last_error = %q", series.LastError)
	}
	if series.PendingTargets != 1 { // 2160p tier has nothing pinned
		t.Fatalf("tt3 pending = %d, want 1", series.PendingTargets)
	}
	if series.NextRelease != nil {
		t.Fatal("a due (past) item must not carry next_release")
	}
	// tt3 is overdue, so the scheduler wakes now-ish (never past).
	if resp.NextWake < now.Unix()-1 {
		t.Fatalf("next_wake = %d is before now %d", resp.NextWake, now.Unix())
	}
}

func TestHandleScheduleEmpty(t *testing.T) {
	a := scheduleTestApp(t)
	rec := httptest.NewRecorder()
	a.handleSchedule(rec, httptest.NewRequest(http.MethodGet, "/api/schedule", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp scheduleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("items = %d, want 0", len(resp.Items))
	}
	if resp.IntervalSeconds != 7200 {
		t.Fatalf("interval = %d", resp.IntervalSeconds)
	}
}

func TestPausedItemState(t *testing.T) {
	a := scheduleTestApp(t)
	_ = a.store.PutMonitored(context.Background(), store.Monitored{
		Key: "movie:tt9", MediaType: "movie", IMDbID: "tt9", Enabled: false,
		DueAt: time.Now().Add(-time.Hour),
	})
	view, err := a.buildSchedule(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Items) != 1 || view.Items[0].State != "paused" {
		t.Fatalf("items = %+v, want one paused", view.Items)
	}
}
