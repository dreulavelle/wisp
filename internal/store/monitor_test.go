package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMonitorRoundTripAndDelete(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := Monitor{ID: "series:tt1", MediaType: "series", IMDbID: "tt1", Title: "Show", Qualities: []string{"1080p"}, Enabled: true}
	if err := s.UpsertMonitor(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListMonitors(context.Background())
	if err != nil || len(items) != 1 || items[0].IMDbID != "tt1" {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if ok, err := s.DeleteMonitor(context.Background(), m.ID); err != nil || !ok {
		t.Fatalf("deleted=%v err=%v", ok, err)
	}
}
