package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dreulavelle/wisp/internal/server"
)

const testToken = "s3cret-token-value"

// muxFor builds the real route table over a test app, so these tests exercise
// the wiring in newMux rather than a re-declaration of it that could drift.
func muxFor(t *testing.T, token string, log *slog.Logger) *http.ServeMux {
	t.Helper()
	a := testApp(t)
	// handleMetrics reads srv.Metrics(); a nil ReResolve is fine, nothing here
	// serves bytes.
	a.srv = server.New(a.store, nil, a.log)
	return newMux(a, http.HandlerFunc(a.srv.FileHandler), token, log)
}

// protectedRoutes is every endpoint that must demand a token when one is
// configured. Bodies are deliberately absent — an unauthenticated request must
// be rejected before the handler ever looks at one.
var protectedRoutes = []struct {
	method string
	path   string
}{
	{http.MethodPost, "/api/add"},
	{http.MethodGet, "/api/pins"},
	{http.MethodDelete, "/api/pins"},
	{http.MethodPost, "/api/monitors"},
	{http.MethodGet, "/api/monitors"},
	{http.MethodDelete, "/api/monitors"},
	{http.MethodPost, "/api/monitors/refresh"},
	{http.MethodGet, "/api/schedule"},
	{http.MethodGet, "/api/requests/status"},
	{http.MethodGet, "/api/status"},
	{http.MethodGet, "/metrics"},
}

// publicRoutes must stay reachable without credentials in both modes: the
// health probes because a Docker healthcheck cannot send a header, and file
// serving because it is the data plane the FUSE mount reads through.
var publicRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/health"},
	{http.MethodGet, "/api/healthz"},
	{http.MethodGet, "/"},
	{http.MethodGet, "/shows/"},
}

func do(t *testing.T, mux *http.ServeMux, method, path, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// An unset token must leave wisp exactly as it was: every endpoint open. This
// is the upgrade-safety guarantee — deployments running unauthenticated today
// must not break on the release that adds this.
func TestAuthDisabledAllowsEveryEndpoint(t *testing.T) {
	mux := muxFor(t, "", slog.New(slog.DiscardHandler))
	for _, rt := range append(append([]struct{ method, path string }{}, protectedRoutes...), publicRoutes...) {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			if got := do(t, mux, rt.method, rt.path, "").Code; got == http.StatusUnauthorized {
				t.Fatalf("auth disabled but %s %s returned 401", rt.method, rt.path)
			}
		})
	}
}

// A stray Authorization header must not be rejected when auth is off — a
// client (like the Silo plugin) that always sends one keeps working.
func TestAuthDisabledIgnoresPresentedToken(t *testing.T) {
	mux := muxFor(t, "", slog.New(slog.DiscardHandler))
	if got := do(t, mux, http.MethodGet, "/api/pins", "Bearer anything-at-all").Code; got != http.StatusOK {
		t.Fatalf("GET /api/pins with an ignorable token = %d, want 200", got)
	}
}

func TestAuthEnabledRejectsBadCredentials(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"wrong scheme", "Token " + testToken},
		{"basic auth", "Basic dXNlcjpwYXNz"},
		{"scheme only", "Bearer"},
		{"empty credential", "Bearer "},
		{"wrong token", "Bearer not-the-token"},
		{"right token wrong case", "Bearer " + strings.ToUpper(testToken)},
		{"token as prefix", "Bearer " + testToken[:5]},
		{"token with suffix", "Bearer " + testToken + "x"},
	}
	mux := muxFor(t, testToken, slog.New(slog.DiscardHandler))
	for _, tc := range cases {
		for _, rt := range protectedRoutes {
			t.Run(tc.name+" "+rt.method+" "+rt.path, func(t *testing.T) {
				rec := do(t, mux, rt.method, rt.path, tc.header)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("%s %s with %q = %d, want 401", rt.method, rt.path, tc.header, rec.Code)
				}
				if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
				}
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
				}
				if body["error"] != "unauthorized" {
					t.Errorf("error = %v, want unauthorized", body["error"])
				}
			})
		}
	}
}

func TestAuthEnabledAcceptsCorrectToken(t *testing.T) {
	mux := muxFor(t, testToken, slog.New(slog.DiscardHandler))
	for _, rt := range protectedRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			if got := do(t, mux, rt.method, rt.path, "Bearer "+testToken).Code; got == http.StatusUnauthorized {
				t.Fatalf("%s %s with the correct token returned 401", rt.method, rt.path)
			}
		})
	}
}

// RFC 9110 §11.1 makes the auth scheme case-insensitive, and a credential
// padded with whitespace is a common hand-rolled-client mistake worth
// absorbing. The token itself stays byte-exact.
func TestAuthAcceptsSchemeVariants(t *testing.T) {
	mux := muxFor(t, testToken, slog.New(slog.DiscardHandler))
	for _, header := range []string{
		"Bearer " + testToken,
		"bearer " + testToken,
		"BEARER " + testToken,
		"BeArEr   " + testToken + "  ",
	} {
		t.Run(header, func(t *testing.T) {
			if got := do(t, mux, http.MethodGet, "/api/pins", header).Code; got != http.StatusOK {
				t.Fatalf("GET /api/pins with %q = %d, want 200", header, got)
			}
		})
	}
}

// Gating the health probes would wedge `depends_on: {condition:
// service_healthy}` for every dependent container, because the documented
// healthcheck (`wget --spider`) cannot attach a header.
func TestHealthAndFileServingStayPublicWhenAuthEnabled(t *testing.T) {
	for _, token := range []string{"", testToken} {
		mode := "auth disabled"
		if token != "" {
			mode = "auth enabled"
		}
		mux := muxFor(t, token, slog.New(slog.DiscardHandler))
		for _, rt := range publicRoutes {
			t.Run(mode+" "+rt.method+" "+rt.path, func(t *testing.T) {
				if got := do(t, mux, rt.method, rt.path, "").Code; got == http.StatusUnauthorized {
					t.Fatalf("%s %s must stay public, got 401", rt.method, rt.path)
				}
			})
		}
	}
}

// A mistyped secret must not be persisted anywhere less protected than the
// secret itself — not echoed into the response, not into the log line.
func TestRejectionLeaksNoToken(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	mux := muxFor(t, testToken, log)

	const wrong = "this-is-the-wrong-token-a1b2c3"
	rec := do(t, mux, http.MethodGet, "/api/pins", "Bearer "+wrong)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), wrong) {
		t.Errorf("response body echoes the presented token: %q", rec.Body.String())
	}
	for _, secret := range []string{wrong, testToken} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("log leaks %q:\n%s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "192.0.2.1") { // httptest's default RemoteAddr
		t.Errorf("log should record the remote address:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("auth failures should log at warn:\n%s", logs.String())
	}
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		header    string
		wantToken string
		wantOK    bool
	}{
		{"", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Bearer    ", "", false},
		{"Bear", "", false},
		{"Basic abc", "", false},
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"BEARER abc", "abc", true},
		{"Bearer  abc ", "abc", true},
		{"Bearer abc def", "abc def", true}, // preserved verbatim; it simply won't match
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			got, ok := bearerToken(tc.header)
			if got != tc.wantToken || ok != tc.wantOK {
				t.Fatalf("bearerToken(%q) = (%q, %v), want (%q, %v)", tc.header, got, ok, tc.wantToken, tc.wantOK)
			}
		})
	}
}
