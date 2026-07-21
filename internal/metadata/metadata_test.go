package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func date(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// testService points every provider base URL at one mux.
func testService(t *testing.T, mux *http.ServeMux) *Service {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := New()
	s.cinemetaBase, s.tvmazeBase = srv.URL, srv.URL
	return s
}

func TestEnrichAirDates(t *testing.T) {
	canonical := []Episode{
		{Season: 1, Number: 1, Aired: date("2026-01-01T00:00:00Z")},
		{Season: 1, Number: 2, Aired: date("2026-01-08T00:00:00Z")},
		{Season: 1, Number: 3}, // no canonical date
	}
	tvmaze := []Episode{
		{Season: 1, Number: 1, Aired: date("2026-01-01T02:30:00Z")}, // within 48h → enriches
		{Season: 1, Number: 2, Aired: date("2026-06-01T00:00:00Z")}, // months off → rejected
		{Season: 1, Number: 3, Aired: date("2026-01-15T00:00:00Z")}, // canonical zero → not trusted
	}
	enrichAirDates(canonical, tvmaze)
	if !canonical[0].Aired.Equal(date("2026-01-01T02:30:00Z")) {
		t.Fatalf("E1 not enriched: %v", canonical[0].Aired)
	}
	if !canonical[1].Aired.Equal(date("2026-01-08T00:00:00Z")) {
		t.Fatalf("E2 wrongly overwritten: %v", canonical[1].Aired)
	}
	if !canonical[2].Aired.IsZero() {
		t.Fatalf("E3 should stay unknown (no canonical date to corroborate): %v", canonical[2].Aired)
	}
}

func TestEpisodesMergeAndRelease(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meta/series/tt1.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{"videos":[
			{"season":1,"episode":1,"released":"2026-01-01T00:00:00Z"},
			{"season":1,"episode":2,"released":"2026-01-08T00:00:00Z"},
			{"season":1,"episode":3,"released":"2027-01-01T00:00:00Z"},
			{"season":0,"episode":1,"released":"2025-12-01T00:00:00Z"}
		]}}`))
	})
	mux.HandleFunc("/lookup/shows", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":42}`))
	})
	mux.HandleFunc("/shows/42/episodes", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"season":1,"number":1,"airstamp":"2026-01-01T01:00:00Z"}]`))
	})
	s := testService(t, mux)

	all, err := s.Episodes(context.Background(), "tt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 { // season-0 special dropped
		t.Fatalf("episodes = %d, want 3", len(all))
	}
	if !all[0].Aired.Equal(date("2026-01-01T01:00:00Z")) {
		t.Fatalf("E1 airstamp not enriched from TVmaze: %v", all[0].Aired)
	}
	released, err := s.ReleasedEpisodes(context.Background(), "tt1", date("2026-02-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 2 { // E3 airs 2027 → excluded
		t.Fatalf("released = %d, want 2", len(released))
	}
}
