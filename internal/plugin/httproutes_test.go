package plugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/dreulavelle/wisp/internal/aiostreams"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestHTTPRoutesUnconfigured(t *testing.T) {
	resp, err := NewHTTPRoutes().Handle(context.Background(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodGet, Path: "/healthz",
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if resp.GetStatusCode() != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 before configuration", resp.GetStatusCode())
	}
}

// The property the whole design rests on: playback is answered with a redirect,
// and that redirect has to survive the host's unary gRPC bridge with its
// Location header intact. If headers were dropped here, playback would break
// with no other symptom.
func TestHTTPRoutesPreservesRedirect(t *testing.T) {
	routes := NewHTTPRoutes()
	resolver := NewResolver(&stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn.example.com/movie.mkv?token=abc", Resolution: "1080p"},
	}})
	routes.SetHandler(NewRouter(resolver, slog.New(slog.DiscardHandler)).Handler())

	resp, err := routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodGet,
		Path:   "/resolve/movie/tmdb:603",
		Query:  mustStruct(map[string]any{"imdb": "tt0133093"}),
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	if resp.GetStatusCode() != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.GetStatusCode())
	}
	if got := resp.GetHeaders()["Location"]; got != "https://cdn.example.com/movie.mkv?token=abc" {
		t.Errorf("Location = %q, want the resolved URL", got)
	}
	if cc := resp.GetHeaders()["Cache-Control"]; cc == "" {
		t.Error("Cache-Control missing; a cached redirect strands playback on a dead link")
	}
}

func TestHTTPRoutesPassesQueryParameters(t *testing.T) {
	routes := NewHTTPRoutes()
	stub := &stubSearcher{streams: []aiostreams.Stream{
		{URL: "https://cdn/1080.mkv", Resolution: "1080p"},
		{URL: "https://cdn/2160.mkv", Resolution: "2160p"},
	}}
	routes.SetHandler(NewRouter(NewResolver(stub), slog.New(slog.DiscardHandler)).Handler())

	query, err := structpb.NewStruct(map[string]any{"quality": "2160p", "imdb": "tt0133093"})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodGet, Path: "/resolve/movie/tmdb:603", Query: query,
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got := resp.GetHeaders()["Location"]; got != "https://cdn/2160.mkv" {
		t.Errorf("Location = %q, want the 2160p candidate (quality param not honoured)", got)
	}
}

// The host encodes query values as JSON, so numbers arrive as float64. Whole
// numbers must not come back as "1e+06".
func TestEncodeQueryRendersScalars(t *testing.T) {
	q, err := structpb.NewStruct(map[string]any{
		"s": "text", "b": true, "n": float64(1000000), "f": 1.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := encodeQuery(q)
	for _, want := range []string{"s=text", "b=true", "n=1000000", "f=1.5"} {
		if !contains(got, want) {
			t.Errorf("encodeQuery() = %q, missing %q", got, want)
		}
	}
}

func TestHTTPRoutesHealth(t *testing.T) {
	routes := NewHTTPRoutes()
	routes.SetHandler(NewRouter(nil, slog.New(slog.DiscardHandler)).Handler())

	resp, err := routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodGet, Path: "/healthz",
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if resp.GetStatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.GetStatusCode())
	}
	var body map[string]any
	if err := json.Unmarshal(resp.GetBody(), &body); err != nil {
		t.Fatalf("health body is not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v, want ok", body["status"])
	}
}

// An unresolvable title is a normal state, not a broken plugin, and must be
// distinguishable so the media server can retry rather than give up.
func TestHTTPRoutesNoStreamIsRetryable(t *testing.T) {
	routes := NewHTTPRoutes()
	routes.SetHandler(NewRouter(NewResolver(&stubSearcher{}), slog.New(slog.DiscardHandler)).Handler())

	resp, err := routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodGet, Path: "/resolve/movie/tmdb:603",
		Query: mustStruct(map[string]any{"imdb": "tt0133093"}),
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if resp.GetStatusCode() != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 for a title with no stream yet", resp.GetStatusCode())
	}
}

func TestHTTPRoutesBadPath(t *testing.T) {
	routes := NewHTTPRoutes()
	routes.SetHandler(NewRouter(NewResolver(&stubSearcher{}), slog.New(slog.DiscardHandler)).Handler())

	resp, err := routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodGet, Path: "/resolve/movie/not-an-id",
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if resp.GetStatusCode() != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.GetStatusCode())
	}
}

// Upstream error text can carry credentials embedded in resolver URLs.
func TestHTTPRoutesDoesNotLeakUpstreamDetail(t *testing.T) {
	secret := "https://user:hunter2@aiostreams.internal/stremio/SECRETUUID"
	routes := NewHTTPRoutes()
	routes.SetHandler(NewRouter(
		NewResolver(&stubSearcher{err: &aiostreams.SearchError{Kind: aiostreams.KindTransient}}),
		slog.New(slog.DiscardHandler),
	).Handler())

	resp, err := routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodGet, Path: "/resolve/movie/tmdb:603",
		Query: mustStruct(map[string]any{"imdb": "tt0133093"}),
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if contains(string(resp.GetBody()), "hunter2") || contains(string(resp.GetBody()), secret) {
		t.Errorf("response body leaked upstream detail: %s", resp.GetBody())
	}
	if resp.GetStatusCode() != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for a transient upstream failure", resp.GetStatusCode())
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func mustStruct(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}
