package plugin

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dreulavelle/wisp/internal/aiostreams"
)

func TestSignerRoundTrip(t *testing.T) {
	s := NewSigner("https://aio.example/stremio/uuid", "password")
	req := ResolveRequest{MediaType: "movie", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093", Quality: "1080p"}

	token := s.Sign(req)
	if len(token) != tokenLength {
		t.Fatalf("token length = %d, want %d", len(token), tokenLength)
	}
	if !s.Verify(req, token) {
		t.Error("Verify rejected a token it just issued")
	}
}

// A token must authorize exactly one thing. If any coordinate can be changed
// while keeping the signature, the token stops being a capability for a
// specific item and becomes a general-purpose key.
func TestSignerTokenIsBoundToEveryCoordinate(t *testing.T) {
	s := NewSigner("seed")
	base := ResolveRequest{MediaType: "series", ID: MediaID{SourceTVDB, "121361"}, IMDbID: "tt0944947", Season: 1, Episode: 9, Quality: "1080p"}
	token := s.Sign(base)

	variants := map[string]ResolveRequest{
		"different identity": {MediaType: "series", ID: MediaID{SourceTVDB, "999"}, IMDbID: "tt0944947", Season: 1, Episode: 9, Quality: "1080p"},
		"swapped lookup key": {MediaType: "series", ID: MediaID{SourceTVDB, "121361"}, IMDbID: "tt0000001", Season: 1, Episode: 9, Quality: "1080p"},
		"different season":   {MediaType: "series", ID: MediaID{SourceTVDB, "121361"}, IMDbID: "tt0944947", Season: 2, Episode: 9, Quality: "1080p"},
		"different episode":  {MediaType: "series", ID: MediaID{SourceTVDB, "121361"}, IMDbID: "tt0944947", Season: 1, Episode: 10, Quality: "1080p"},
		"different type":     {MediaType: "movie", ID: MediaID{SourceTVDB, "121361"}, IMDbID: "tt0944947", Season: 1, Episode: 9, Quality: "1080p"},
		// Quality is signed because a token for 720p must not authorize a
		// 2160p fetch: that is a different amount of bandwidth and quota.
		"escalated quality": {MediaType: "series", ID: MediaID{SourceTVDB, "121361"}, IMDbID: "tt0944947", Season: 1, Episode: 9, Quality: "2160p"},
	}
	for name, v := range variants {
		if s.Verify(v, token) {
			t.Errorf("%s: token verified for a request it was not issued for", name)
		}
	}
}

func TestSignerRejectsMalformedTokens(t *testing.T) {
	s := NewSigner("seed")
	req := ResolveRequest{MediaType: "movie", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093"}
	valid := s.Sign(req)

	bad := []string{
		"", "short", valid + "x", valid[:len(valid)-1],
		"................", "AAAAAAAAAAAAAAAA",
	}
	for _, token := range bad {
		if s.Verify(req, token) {
			t.Errorf("Verify accepted malformed token %q", token)
		}
	}
}

// Different deployments must not produce interchangeable tokens, or a token
// leaked from one install would work against another.
func TestSignerKeysAreSeedSpecific(t *testing.T) {
	req := ResolveRequest{MediaType: "movie", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093"}
	a := NewSigner("instance-a", "password-a")
	b := NewSigner("instance-b", "password-b")

	if a.Sign(req) == b.Sign(req) {
		t.Error("signers with different seeds produced the same token")
	}
	if b.Verify(req, a.Sign(req)) {
		t.Error("a token from one deployment verified against another")
	}
}

// Placeholder URLs must survive a restart, so the same configuration has to
// derive the same key every time.
func TestSignerIsDeterministicAcrossInstances(t *testing.T) {
	req := ResolveRequest{MediaType: "movie", IMDbID: "tt0133093", Quality: "2160p"}
	if NewSigner("url", "pass").Sign(req) != NewSigner("url", "pass").Sign(req) {
		t.Error("the same seed produced different tokens; placeholders would break on restart")
	}
}

// The end-to-end property: the public resolver route must refuse to do upstream
// work for a request it did not authorize.
func TestResolveRejectsUnsignedRequests(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{{URL: "https://cdn/a.mkv", Resolution: "1080p"}}}
	signer := NewSigner("seed")
	rt := NewRouterWith(RouterOptions{
		Resolver: NewResolver(stub),
		Log:      slog.New(slog.DiscardHandler),
		Signer:   signer,
	})
	h := rt.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/resolve/movie/tmdb:603?imdb=tt0133093&quality=1080p", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unsigned request", rec.Code)
	}
	if stub.calls != 0 {
		t.Errorf("upstream was queried %d times for an unsigned request; it must cost nothing", stub.calls)
	}
}

func TestResolveAcceptsSignedRequests(t *testing.T) {
	stub := &stubSearcher{streams: []aiostreams.Stream{{URL: "https://cdn/a.mkv", Resolution: "1080p"}}}
	signer := NewSigner("seed")
	rt := NewRouterWith(RouterOptions{
		Resolver: NewResolver(stub),
		Log:      slog.New(slog.DiscardHandler),
		Signer:   signer,
	})

	req := ResolveRequest{MediaType: "movie", ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093", Quality: "1080p"}
	url := "/resolve/movie/tmdb:603?imdb=tt0133093&quality=1080p&t=" + signer.Sign(req)

	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 for a signed request", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "https://cdn/a.mkv" {
		t.Errorf("Location = %q", got)
	}
}

// A rejection must not reveal whether the title exists, or the endpoint becomes
// an oracle for enumerating a library.
func TestUnsignedRejectionLooksLikeAnUnknownPath(t *testing.T) {
	rt := NewRouterWith(RouterOptions{
		Resolver: NewResolver(&stubSearcher{}),
		Log:      slog.New(slog.DiscardHandler),
		Signer:   NewSigner("seed"),
	})
	h := rt.Handler()

	unsigned := httptest.NewRecorder()
	h.ServeHTTP(unsigned, httptest.NewRequest(http.MethodGet, "/resolve/movie/tmdb:603?imdb=tt0133093", nil))

	nonsense := httptest.NewRecorder()
	h.ServeHTTP(nonsense, httptest.NewRequest(http.MethodGet, "/resolve/movie/tmdb:999999?imdb=tt9999999", nil))

	if unsigned.Code != nonsense.Code || unsigned.Body.String() != nonsense.Body.String() {
		t.Errorf("responses differ and leak which titles exist:\n  %d %s\n  %d %s",
			unsigned.Code, unsigned.Body.String(), nonsense.Code, nonsense.Body.String())
	}
}

// IMDbID and Quality are read verbatim from query parameters at verify time, so
// the canonical encoding must stay injective no matter what they contain. With
// a delimiter-joined encoding, a field carrying the delimiter could be re-split
// into a different tuple that signs identically.
func TestCanonicalIsInjectiveAcrossFieldBoundaries(t *testing.T) {
	seen := map[string]ResolveRequest{}
	for _, req := range []ResolveRequest{
		{MediaType: "series", ID: MediaID{SourceTVDB, "1"}, IMDbID: "tt1", Season: 2, Episode: 3, Quality: "1080p"},
		// Fields stuffed with the old "|" delimiter and with the new "N:" shape.
		{MediaType: "series", ID: MediaID{SourceTVDB, "1"}, IMDbID: "tt1|2|3", Season: 0, Episode: 0, Quality: "1080p"},
		{MediaType: "series", ID: MediaID{SourceTVDB, "1"}, IMDbID: "tt1", Season: 2, Episode: 3, Quality: "1080p|x"},
		{MediaType: "series", ID: MediaID{SourceTVDB, "1"}, IMDbID: "3:tt1", Season: 2, Episode: 3, Quality: "1080p"},
		{MediaType: "series", ID: MediaID{SourceTVDB, "1"}, IMDbID: "", Season: 2, Episode: 3, Quality: "3:tt1"},
		{MediaType: "series", ID: MediaID{SourceTVDB, "1"}, IMDbID: "tt", Season: 12, Episode: 3, Quality: "1080p"},
		{MediaType: "series", ID: MediaID{SourceTVDB, "1"}, IMDbID: "tt1", Season: 23, Episode: 0, Quality: "1080p"},
	} {
		c := canonical(req)
		if prior, clash := seen[c]; clash {
			t.Errorf("distinct requests share a canonical form %q:\n  %+v\n  %+v", c, prior, req)
		}
		seen[c] = req
	}
}

// A token minted for one tuple must not verify against a neighbouring one.
func TestTokenDoesNotTransferBetweenTuples(t *testing.T) {
	s := NewSigner("https://aio.example.invalid/x/manifest.json", "hunter2")
	base := ResolveRequest{MediaType: "series", ID: MediaID{SourceTVDB, "121361"}, IMDbID: "tt0944947", Season: 1, Episode: 9, Quality: "1080p"}
	token := s.Sign(base)

	for name, alt := range map[string]ResolveRequest{
		"different episode":      {MediaType: "series", ID: base.ID, IMDbID: base.IMDbID, Season: 1, Episode: 10, Quality: "1080p"},
		"different season":       {MediaType: "series", ID: base.ID, IMDbID: base.IMDbID, Season: 2, Episode: 9, Quality: "1080p"},
		"swapped imdb key":       {MediaType: "series", ID: base.ID, IMDbID: "tt0133093", Season: 1, Episode: 9, Quality: "1080p"},
		"swapped identity":       {MediaType: "series", ID: MediaID{SourceTVDB, "999"}, IMDbID: base.IMDbID, Season: 1, Episode: 9, Quality: "1080p"},
		"season/episode shifted": {MediaType: "series", ID: base.ID, IMDbID: base.IMDbID, Season: 19, Episode: 0, Quality: "1080p"},
	} {
		if s.Verify(alt, token) {
			t.Errorf("%s: token transferred to a different tuple", name)
		}
	}
	if !s.Verify(base, token) {
		t.Error("token failed to verify against the tuple it was issued for")
	}
}

// The whole point of a persisted secret: credentials change, tokens survive.
//
// Deriving the key from the AIOStreams URL and password meant editing either
// one silently invalidated every placeholder already on disk. The files stayed,
// scanned fine, and 404'd the moment anyone pressed play — recoverable only by
// rewriting every .strm in the library.
func TestPersistedSecretSurvivesCredentialChanges(t *testing.T) {
	req := ResolveRequest{
		MediaType: "movie", ID: MediaID{SourceTMDB, "87421"},
		IMDbID: "tt1411250", Quality: "1080p",
	}

	const secret = "a-persisted-random-secret"
	token := NewSignerFromSecret(secret).Sign(req)

	// Same secret, completely different AIOStreams configuration.
	if !NewSignerFromSecret(secret).Verify(req, token) {
		t.Fatal("a token stopped verifying under the same persisted secret")
	}
}

// Upgrading to a persisted secret must not orphan placeholders written under
// the old credential-derived key.
func TestLegacyDerivedTokensStillVerifyAfterUpgrade(t *testing.T) {
	req := ResolveRequest{
		MediaType: "series", ID: MediaID{SourceTVDB, "121361"},
		IMDbID: "tt0944947", Season: 1, Episode: 9, Quality: "1080p",
	}

	const (
		url = "https://aio.example.invalid/stremio/uuid-1/blob/manifest.json"
		pw  = "hunter2"
	)
	legacyToken := NewSigner(url, pw).Sign(req)

	upgraded := NewSignerFromSecret("fresh-random-secret").AcceptAlso(NewSigner(url, pw))
	if !upgraded.Verify(req, legacyToken) {
		t.Error("a placeholder written before the upgrade no longer verifies")
	}

	// New placeholders are signed with the durable secret, not the legacy key.
	newToken := upgraded.Sign(req)
	if newToken == legacyToken {
		t.Error("new tokens are still being signed with the legacy derived key")
	}
	if !upgraded.Verify(req, newToken) {
		t.Error("a freshly signed token does not verify")
	}
}

// Accepting a retired key must not weaken anything else: a token from an
// unrelated key stays rejected, and the signed tuple is still enforced.
func TestAcceptedKeysDoNotWeakenVerification(t *testing.T) {
	req := ResolveRequest{
		MediaType: "movie", ID: MediaID{SourceTMDB, "603"},
		IMDbID: "tt0133093", Quality: "1080p",
	}

	s := NewSignerFromSecret("primary").AcceptAlso(NewSigner("url", "pw"))

	if s.Verify(req, NewSignerFromSecret("some-other-secret").Sign(req)) {
		t.Error("a token from an unrelated key was accepted")
	}

	other := req
	other.ID = MediaID{SourceTMDB, "999"}
	if s.Verify(other, s.Sign(req)) {
		t.Error("a token transferred to a different title")
	}
}

// A nil or empty legacy signer must be ignored rather than admitting a
// zero-key signer that would verify attacker-computable tokens.
func TestAcceptAlsoIgnoresEmptySigners(t *testing.T) {
	s := NewSignerFromSecret("primary")
	if got := s.AcceptAlso(nil); got != s {
		t.Error("AcceptAlso(nil) did not return the receiver")
	}
	if len(s.accept) != 0 {
		t.Errorf("AcceptAlso(nil) registered %d key(s)", len(s.accept))
	}

	s.AcceptAlso(&Signer{})
	if len(s.accept) != 0 {
		t.Errorf("AcceptAlso(empty) registered %d key(s)", len(s.accept))
	}
}
