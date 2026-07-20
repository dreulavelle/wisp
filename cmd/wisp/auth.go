package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// Fixed rejection reasons. These are logged verbatim, so they are constants
// rather than anything derived from the request — nothing an attacker controls
// (least of all a wrong token) may reach the log.
const (
	reasonNoHeader   = "missing or malformed Authorization header"
	reasonBadToken   = "invalid token"
	authHeaderPrefix = "bearer "
)

// requireBearer returns middleware gating a handler on a bearer token.
//
// An empty token disables authentication: the returned middleware is the
// identity function, so an upgrade that doesn't set WISP_API_TOKEN behaves
// exactly as before, with no per-request cost. That default-off behavior is
// deliberate — wisp has shipped without auth, and existing deployments must
// keep working untouched.
//
// Every rejection is a 401, never a 403. A wrong token is a failed
// authentication, not a permission decision on an established identity, which
// is what 403 means (RFC 9110 §15.5.4). Answering 401 uniformly also avoids
// handing a prober an oracle: if a malformed header returned 401 and a
// well-formed-but-wrong one returned 403, the status alone would confirm both
// that the endpoint is token-protected and that a candidate reached the
// comparison. Clients benefit too — 401 reads as "fix your credentials" where
// 403 commonly reads as "permanent, don't retry".
func requireBearer(token string, log *slog.Logger) func(http.Handler) http.Handler {
	if token == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	// Compare digests, not the raw secrets: subtle.ConstantTimeCompare is only
	// constant-time for equal-length inputs and returns early otherwise, which
	// would leak the token's length. Hashing first makes both sides a fixed 32
	// bytes so the comparison is constant-time across every input.
	want := sha256.Sum256([]byte(token))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				writeUnauthorized(w, r, log, reasonNoHeader)
				return
			}
			got := sha256.Sum256([]byte(presented))
			if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				writeUnauthorized(w, r, log, reasonBadToken)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the credential from an Authorization header value. The
// scheme is matched case-insensitively (RFC 9110 §11.1 makes auth schemes
// case-insensitive); an absent header, a different scheme, or an empty
// credential all report false.
func bearerToken(header string) (string, bool) {
	if len(header) < len(authHeaderPrefix) || !strings.EqualFold(header[:len(authHeaderPrefix)], authHeaderPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(authHeaderPrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// writeUnauthorized answers 401 and logs the rejection with the remote address
// only. The presented token is never read back into the response or the log —
// not in the body, not via an echoed header — so a mistyped secret cannot land
// in a log file that is less protected than the secret itself.
func writeUnauthorized(w http.ResponseWriter, r *http.Request, log *slog.Logger, reason string) {
	log.Warn("api auth rejected", "remote_addr", remoteHost(r), "method", r.Method, "path", r.URL.Path, "reason", reason)
	// RFC 6750 §3 requires a challenge on a 401 from a bearer-protected
	// resource. It carries no error parameter on purpose: `error="invalid_token"`
	// would restore exactly the oracle the uniform 401 removes.
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":   "unauthorized",
		"message": "a valid bearer token is required",
	})
}

// remoteHost strips the ephemeral source port from RemoteAddr, which is noise
// for correlating repeated attempts from one host. Unparseable values (a
// non-TCP listener) pass through unchanged.
func remoteHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
