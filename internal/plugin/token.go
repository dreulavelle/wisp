package plugin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Signer authenticates resolver requests.
//
// The resolver route has to be reachable without a Silo session: the client
// following a placeholder redirect is ffmpeg or a browser, and neither carries
// one. That makes the route public, which without this would let anyone who can
// reach the server mint stream links against the operator's debrid account
// simply by guessing IMDb ids — they are public identifiers.
//
// So every placeholder embeds a signature over the exact thing it addresses.
// Knowing an IMDb id is not enough; you need a token Wisp issued. Anyone who
// can read the .strm files can of course extract tokens, but that already
// implies access to the library those placeholders live in.
type Signer struct {
	key []byte
}

// tokenLength is how many base64 characters of the digest are kept.
//
// 16 characters is 96 bits of the HMAC — far beyond guessing, while keeping
// placeholder URLs readable when an operator inspects one by hand.
const tokenLength = 16

// NewSigner derives a signing key from stable configuration.
//
// The key is derived rather than stored because a placeholder's URL must stay
// valid across restarts. Deriving it from configuration that already has to be
// stable removes any need to persist a secret.
//
// The security of this rests entirely on at least one seed being secret. With
// an AIOStreams password set, that holds: the password never leaves the host.
// Without one, the only seed is the AIOStreams URL — and for the alias form
// (/stremio/u/{alias}/manifest.json) that URL carries no encrypted config and
// is not secret, so anyone who knows it can recompute this key and mint valid
// tokens. Configure warns loudly in that case; see cmd/silo-plugin-wisp.
//
// The trade-off is that rotating AIOStreams credentials also rotates this key,
// invalidating existing placeholders until they are rewritten. That is rare,
// visible (playback fails closed rather than silently authorizing), and
// recoverable.
func NewSigner(seed ...string) *Signer {
	h := hmac.New(sha256.New, []byte("wisp-resolver-token-v1"))
	for _, s := range seed {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	return &Signer{key: h.Sum(nil)}
}

// canonical renders the exact tuple a token authorizes.
//
// Fields are length-prefixed rather than joined on a delimiter. Two of them —
// IMDbID and Quality — are read verbatim from query parameters at verify time,
// so a delimited encoding is not injective in general: a field containing the
// delimiter could be re-split into a different tuple that signs identically.
// Length prefixing makes the encoding unambiguous regardless of field content,
// which keeps that from becoming a forgery primitive if the tuple ever grows a
// free-form field.
//
// Quality is signed for audit and cache-key correctness. Note it is NOT a
// bandwidth ceiling: acceptableTiers falls back to accepting any resolution
// when no candidate matches the requested tier.
func canonical(req ResolveRequest) string {
	// The IMDb lookup key is signed alongside the identity. It travels in the
	// URL rather than being derived, so without covering it an attacker could
	// keep a valid token and swap in any title they liked.
	fields := []string{
		strings.ToLower(req.MediaType),
		strings.ToLower(req.ID.String()),
		strings.ToLower(req.IMDbID),
		strconv.Itoa(req.Season),
		strconv.Itoa(req.Episode),
		strings.ToLower(strings.TrimSpace(req.Quality)),
	}
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, "%d:%s", len(f), f)
	}
	return b.String()
}

// Sign returns the token for a resolver request.
func (s *Signer) Sign(req ResolveRequest) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(canonical(req)))
	sum := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return sum[:tokenLength]
}

// Verify reports whether a token authorizes a request.
//
// The comparison is constant time so that a wrong token cannot be refined by
// timing one character at a time.
func (s *Signer) Verify(req ResolveRequest, token string) bool {
	if len(token) != tokenLength {
		return false
	}
	return hmac.Equal([]byte(s.Sign(req)), []byte(token))
}
