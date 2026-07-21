package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/meta/series/tt38262097.json":
			w.Write([]byte(`{"meta":{"tvdb_id":467127,"moviedb_id":298994}}`))
		case "/meta/movie/tt1375666.json":
			w.Write([]byte(`{"meta":{"moviedb_id":27205}}`)) // no tvdb for movies
		case "/meta/movie/tt0.json":
			w.Write([]byte(`{"meta":{}}`)) // nothing mapped
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	old := cinemetaBase
	cinemetaBase = server.URL
	defer func() { cinemetaBase = old }()

	if tvdb, tmdb, err := ProviderIDs(context.Background(), "series", "tt38262097"); err != nil || tvdb != "467127" || tmdb != "298994" {
		t.Fatalf("series ids = %q, %q, %v", tvdb, tmdb, err)
	}
	if tvdb, tmdb, err := ProviderIDs(context.Background(), "movie", "tt1375666"); err != nil || tvdb != "" || tmdb != "27205" {
		t.Fatalf("movie ids = %q, %q, %v", tvdb, tmdb, err)
	}
	if tvdb, tmdb, err := ProviderIDs(context.Background(), "movie", "tt0"); err != nil || tvdb != "" || tmdb != "" {
		t.Fatalf("unmapped ids = %q, %q, %v", tvdb, tmdb, err)
	}
}

// A lookup failure must be distinguishable from "this title has no TVDB id" —
// the two call for different fixes, and folding them together points the
// operator at the wrong one.
func TestProviderIDsReportsLookupFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream is down", http.StatusBadGateway)
	}))
	defer server.Close()
	old := cinemetaBase
	cinemetaBase = server.URL
	defer func() { cinemetaBase = old }()

	if _, _, err := ProviderIDs(context.Background(), "series", "tt1"); err == nil {
		t.Fatal("a 502 must surface as an error, not as an empty result")
	}
}

// An id that reached us from a host request must never be interpolated raw into
// the request URL, where a "/" or "?" would rewrite the path or query.
func TestProviderIDsRejectsMalformedIDs(t *testing.T) {
	for _, bad := range []string{"", "tt", "../../etc/passwd", "tt1/../../x", "tt1?a=b", "tt1#frag", "nope"} {
		if _, _, err := ProviderIDs(context.Background(), "movie", bad); err == nil {
			t.Errorf("ProviderIDs(%q) was accepted; want a rejection", bad)
		}
	}
}
