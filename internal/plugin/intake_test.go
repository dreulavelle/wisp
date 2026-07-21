package plugin

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

type stubEpisodes struct {
	eps []EpisodeRef
	err error
}

func (s *stubEpisodes) ReleasedEpisodes(context.Context, string) ([]EpisodeRef, error) {
	return s.eps, s.err
}

func newIntake(t *testing.T, eps EpisodeLister) (*Intake, *Library, string) {
	t.Helper()
	root := t.TempDir()
	lib := NewLibrary()
	w := NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/3", NewSigner("seed"))
	return NewIntake(w, lib, eps, slog.New(slog.DiscardHandler)), lib, root
}

func fulfillReq(mediaType, title string, year int, ids map[string]string, qualities ...string) *pluginv1.FulfillRequest {
	var qs []*pluginv1.RequestedQuality
	for _, q := range qualities {
		qs = append(qs, &pluginv1.RequestedQuality{Id: q})
	}
	return &pluginv1.FulfillRequest{
		Request: &pluginv1.RequestDescriptor{
			MediaType: mediaType, Title: title, Year: int32(year), ExternalIds: ids,
		},
		Qualities:   qs,
		Connections: []*pluginv1.RouterConnection{{Id: "conn-1"}},
	}
}

func TestFulfillMovie(t *testing.T) {
	in, lib, root := newIntake(t, nil)

	resp, err := in.Fulfill(context.Background(), fulfillReq("movie", "The Matrix", 1999,
		map[string]string{"tmdb": "603", "imdb": "tt0133093"}, "2160p"))
	if err != nil {
		t.Fatalf("Fulfill() error = %v", err)
	}

	if len(resp.GetTargets()) != 1 {
		t.Fatalf("got %d targets, want 1", len(resp.GetTargets()))
	}
	// On-demand playback has no download to wait on, so a pending status would
	// leave the request unresolved forever.
	if got := resp.GetTargets()[0].GetStatus(); got != "completed" {
		t.Errorf("status = %q, want completed", got)
	}
	if got := resp.GetTargets()[0].GetExternalId(); got != "tmdb:603" {
		t.Errorf("external id = %q, want the canonical identity", got)
	}

	want := filepath.Join(root, rootMovies, "The Matrix (1999) [tmdb-603]", "The Matrix (1999) [2160p].strm")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("placeholder not written at %s: %v", want, err)
	}
	if lib.Count() != 1 {
		t.Errorf("library tracks %d placeholders, want 1", lib.Count())
	}
}

func TestFulfillMovieWritesOnePlaceholderPerQuality(t *testing.T) {
	in, lib, _ := newIntake(t, nil)

	resp, err := in.Fulfill(context.Background(), fulfillReq("movie", "Dune", 2021,
		map[string]string{"tmdb": "438631", "imdb": "tt1160419"}, "2160p", "1080p"))
	if err != nil {
		t.Fatalf("Fulfill() error = %v", err)
	}
	if len(resp.GetTargets()) != 2 {
		t.Fatalf("got %d targets, want 2", len(resp.GetTargets()))
	}
	if lib.Count() != 2 {
		t.Errorf("library tracks %d placeholders, want 2", lib.Count())
	}
}

// A series request covers a show, so it fans out to every aired episode.
func TestFulfillSeriesExpandsToEpisodes(t *testing.T) {
	eps := &stubEpisodes{eps: []EpisodeRef{{1, 1}, {1, 2}, {2, 1}}}
	in, lib, root := newIntake(t, eps)

	resp, err := in.Fulfill(context.Background(), fulfillReq("series", "Severance", 2022,
		map[string]string{"tvdb": "371980", "imdb": "tt11280740"}, "1080p"))
	if err != nil {
		t.Fatalf("Fulfill() error = %v", err)
	}
	if got := resp.GetTargets()[0].GetStatus(); got != "completed" {
		t.Errorf("status = %q, want completed", got)
	}
	if lib.Count() != 3 {
		t.Errorf("library tracks %d placeholders, want 3", lib.Count())
	}

	for _, rel := range []string{
		"tv/Severance (2022) [tvdb-371980]/Season 01/Severance (2022) S01E01 [1080p].strm",
		"tv/Severance (2022) [tvdb-371980]/Season 01/Severance (2022) S01E02 [1080p].strm",
		"tv/Severance (2022) [tvdb-371980]/Season 02/Severance (2022) S02E01 [1080p].strm",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("missing placeholder %s", rel)
		}
	}
}

// Movies must carry TMDB and series TVDB. A missing identity has to fail the
// request rather than produce a placeholder that scans in and cannot play.
func TestFulfillRequiresTheRightIdentity(t *testing.T) {
	cases := map[string]struct {
		mediaType string
		ids       map[string]string
		wantIn    string
	}{
		"movie without tmdb": {
			"movie", map[string]string{"tvdb": "1", "imdb": "tt1"}, "tmdb",
		},
		"series without tvdb": {
			"series", map[string]string{"tmdb": "1", "imdb": "tt1"}, "tvdb",
		},
		"movie without imdb lookup key": {
			"movie", map[string]string{"tmdb": "603"}, "imdb",
		},
		"series without imdb lookup key": {
			"series", map[string]string{"tvdb": "121361"}, "imdb",
		},
		"no ids at all": {
			"movie", map[string]string{}, "tmdb",
		},
		"non-numeric tmdb": {
			"movie", map[string]string{"tmdb": "abc", "imdb": "tt1"}, "numeric",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in, lib, _ := newIntake(t, &stubEpisodes{eps: []EpisodeRef{{1, 1}}})

			resp, err := in.Fulfill(context.Background(), fulfillReq(tc.mediaType, "X", 2020, tc.ids, "1080p"))
			if err != nil {
				t.Fatalf("Fulfill() error = %v", err)
			}
			// Zero targets plus a message is how the contract reports a
			// submission failure.
			if len(resp.GetTargets()) != 0 {
				t.Errorf("got %d targets, want 0 for a rejected request", len(resp.GetTargets()))
			}
			if !strings.Contains(resp.GetMessage(), tc.wantIn) {
				t.Errorf("message = %q, want it to mention %q", resp.GetMessage(), tc.wantIn)
			}
			if lib.Count() != 0 {
				t.Errorf("wrote %d placeholders for a rejected request", lib.Count())
			}
		})
	}
}

func TestFulfillRejectsUnknownMediaType(t *testing.T) {
	in, _, _ := newIntake(t, nil)
	resp, err := in.Fulfill(context.Background(), fulfillReq("audiobook", "X", 2020,
		map[string]string{"tmdb": "1", "imdb": "tt1"}, "1080p"))
	if err != nil {
		t.Fatalf("Fulfill() error = %v", err)
	}
	if len(resp.GetTargets()) != 0 || resp.GetMessage() == "" {
		t.Errorf("unknown media type should be rejected with a message, got %+v", resp)
	}
}

// One bad episode must not sink a whole season: a 60-episode request should
// still deliver the 59 that work.
func TestFulfillSeriesToleratesIndividualEpisodeFailures(t *testing.T) {
	eps := &stubEpisodes{eps: []EpisodeRef{{1, 1}, {1, 0}, {1, 3}}} // episode 0 is invalid
	in, lib, _ := newIntake(t, eps)

	resp, err := in.Fulfill(context.Background(), fulfillReq("series", "Partial", 2020,
		map[string]string{"tvdb": "1", "imdb": "tt1"}, "1080p"))
	if err != nil {
		t.Fatalf("Fulfill() error = %v", err)
	}
	if got := resp.GetTargets()[0].GetStatus(); got != "completed" {
		t.Errorf("status = %q, want completed when some episodes succeeded", got)
	}
	if lib.Count() != 2 {
		t.Errorf("wrote %d placeholders, want the 2 valid episodes", lib.Count())
	}
}

func TestFulfillSeriesWithNoAiredEpisodes(t *testing.T) {
	in, lib, _ := newIntake(t, &stubEpisodes{eps: nil})

	resp, err := in.Fulfill(context.Background(), fulfillReq("series", "Unaired", 2030,
		map[string]string{"tvdb": "1", "imdb": "tt1"}, "1080p"))
	if err != nil {
		t.Fatalf("Fulfill() error = %v", err)
	}
	if got := resp.GetTargets()[0].GetStatus(); got != "failed" {
		t.Errorf("status = %q, want failed for a show with nothing aired", got)
	}
	if lib.Count() != 0 {
		t.Errorf("wrote %d placeholders for an unaired show", lib.Count())
	}
}

func TestFulfillSeriesPropagatesEnumerationFailure(t *testing.T) {
	in, _, _ := newIntake(t, &stubEpisodes{err: errors.New("metadata unavailable")})

	resp, err := in.Fulfill(context.Background(), fulfillReq("series", "X", 2020,
		map[string]string{"tvdb": "1", "imdb": "tt1"}, "1080p"))
	if err != nil {
		t.Fatalf("Fulfill() error = %v", err)
	}
	if got := resp.GetTargets()[0].GetStatus(); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
}

// The host governs quality; no requested tier means "whatever is available".
func TestFulfillWithNoQualityRequested(t *testing.T) {
	in, lib, root := newIntake(t, nil)

	resp, err := in.Fulfill(context.Background(), fulfillReq("movie", "AnyQuality", 2020,
		map[string]string{"tmdb": "1", "imdb": "tt1"}))
	if err != nil {
		t.Fatalf("Fulfill() error = %v", err)
	}
	if len(resp.GetTargets()) != 1 {
		t.Fatalf("got %d targets, want 1", len(resp.GetTargets()))
	}
	if lib.Count() != 1 {
		t.Errorf("library tracks %d placeholders, want 1", lib.Count())
	}
	want := filepath.Join(root, rootMovies, "AnyQuality (2020) [tmdb-1]", "AnyQuality (2020).strm")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("placeholder not written at %s", want)
	}
}

// Autoscan must never be handed a path that does not exist yet, so registration
// happens after the write lands.
func TestFulfillRegistersOnlyWrittenPlaceholders(t *testing.T) {
	in, lib, _ := newIntake(t, nil)

	if _, err := in.Fulfill(context.Background(), fulfillReq("movie", "Registered", 2020,
		map[string]string{"tmdb": "1", "imdb": "tt1"}, "1080p")); err != nil {
		t.Fatal(err)
	}

	for _, p := range lib.List() {
		if _, err := os.Stat(p.Path); err != nil {
			t.Errorf("library registered %s but it is not on disk", p.Path)
		}
	}
}

func TestCheckStatusReportsCompleted(t *testing.T) {
	in, _, _ := newIntake(t, nil)

	resp, err := in.CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
		Targets: []*pluginv1.TargetRef{
			{Quality: "2160p", ConnectionId: "c1"},
			{Quality: "1080p", ConnectionId: "c1"},
		},
	})
	if err != nil {
		t.Fatalf("CheckStatus() error = %v", err)
	}
	if len(resp.GetStatuses()) != 2 {
		t.Fatalf("got %d statuses, want 2", len(resp.GetStatuses()))
	}
	for _, s := range resp.GetStatuses() {
		if s.GetStatus() != "completed" {
			t.Errorf("status = %q, want completed; a placeholder has no download to track", s.GetStatus())
		}
	}
}

// Placeholders written by intake must resolve. This catches a whole class of
// wiring mistakes that would otherwise surface only when a user pressed play.
func TestFulfilledPlaceholdersAreResolvable(t *testing.T) {
	signer := NewSigner("seed")
	root := t.TempDir()
	lib := NewLibrary()
	in := NewIntake(NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/3", signer),
		lib, nil, slog.New(slog.DiscardHandler))

	if _, err := in.Fulfill(context.Background(), fulfillReq("movie", "Playable", 2020,
		map[string]string{"tmdb": "603", "imdb": "tt0133093"}, "1080p")); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(lib.List()[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimSpace(string(body))

	u, req := parseResolverURL(t, raw)
	if !signer.Verify(req, u.Query().Get("t")) {
		t.Error("a placeholder written by intake does not verify against its own token")
	}
	if req.IMDbID != "tt0133093" {
		t.Errorf("lookup key = %q, want the imdb id", req.IMDbID)
	}
	if req.ID.String() != "tmdb:603" {
		t.Errorf("identity = %q, want tmdb:603", req.ID.String())
	}
}

type stubIdentity struct {
	tvdb, tmdb string
	calls      int
}

func (s *stubIdentity) ProviderIDs(context.Context, string, string) (string, string, error) {
	s.calls++
	return s.tvdb, s.tmdb, nil
}

// Silo does not always carry a TVDB id. Deriving it from the IMDb id it does
// have keeps those requests working instead of refusing them.
func TestFulfillDerivesMissingIdentity(t *testing.T) {
	eps := &stubEpisodes{eps: []EpisodeRef{{1, 1}}}
	in, lib, root := newIntake(t, eps)
	resolver := &stubIdentity{tvdb: "121361"}
	in.WithIdentityResolver(resolver)

	resp, err := in.Fulfill(context.Background(), fulfillReq("series", "Game of Thrones", 2011,
		map[string]string{"imdb": "tt0944947"}, "1080p"))
	if err != nil {
		t.Fatalf("Fulfill() error = %v", err)
	}
	if got := resp.GetTargets()[0].GetStatus(); got != "completed" {
		t.Fatalf("status = %q, want completed after deriving the tvdb id: %s",
			got, resp.GetTargets()[0].GetMessage())
	}
	if resolver.calls == 0 {
		t.Error("identity resolver was never consulted")
	}
	if lib.Count() != 1 {
		t.Errorf("wrote %d placeholders, want 1", lib.Count())
	}
	want := filepath.Join(root, rootShows, "Game of Thrones (2011) [tvdb-121361]", "Season 01",
		"Game of Thrones (2011) S01E01 [1080p].strm")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("placeholder not written at the derived identity: %s", want)
	}
}

// Derivation costs a network call, so it must not happen when the host already
// supplied the id.
func TestFulfillSkipsDerivationWhenIdentityIsPresent(t *testing.T) {
	in, _, _ := newIntake(t, nil)
	resolver := &stubIdentity{tmdb: "999"}
	in.WithIdentityResolver(resolver)

	if _, err := in.Fulfill(context.Background(), fulfillReq("movie", "The Matrix", 1999,
		map[string]string{"tmdb": "603", "imdb": "tt0133093"}, "1080p")); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 0 {
		t.Errorf("resolver called %d times despite the host supplying tmdb", resolver.calls)
	}
}

// Nothing can be derived without an IMDb id, so the request must still fail
// clearly rather than silently producing a broken placeholder.
func TestFulfillDerivationNeedsAnImdbID(t *testing.T) {
	in, lib, _ := newIntake(t, nil)
	in.WithIdentityResolver(&stubIdentity{tmdb: "603"})

	resp, err := in.Fulfill(context.Background(), fulfillReq("movie", "X", 2020,
		map[string]string{}, "1080p"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetTargets()) != 0 || resp.GetMessage() == "" {
		t.Errorf("expected rejection with a message, got %+v", resp)
	}
	if lib.Count() != 0 {
		t.Error("wrote placeholders for an underivable request")
	}
}

func TestFulfillReportsWhenDerivationFinsNothing(t *testing.T) {
	in, _, _ := newIntake(t, nil)
	in.WithIdentityResolver(&stubIdentity{}) // resolves to nothing

	resp, err := in.Fulfill(context.Background(), fulfillReq("movie", "X", 2020,
		map[string]string{"imdb": "tt1"}, "1080p"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetTargets()) != 0 {
		t.Errorf("got %d targets, want 0", len(resp.GetTargets()))
	}
	if !strings.Contains(resp.GetMessage(), "derived") {
		t.Errorf("message = %q, should say derivation was attempted", resp.GetMessage())
	}
}
