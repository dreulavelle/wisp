package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The health verdict is config-dependent: a self-mounting deployment must fail
// closed when its FUSE mount is down, while an HTTP-only deployment (where the
// operator mounts externally) must never be gated on a mount wisp doesn't own.
func TestHandleHealth(t *testing.T) {
	cases := []struct {
		name      string
		mountPath string // non-empty == self-mounting, mirrors Config.SelfMount()
		mounted   bool
		wantCode  int
		wantState string
	}{
		{
			name:      "self-mounted and mounted is healthy",
			mountPath: "/mnt/wisp",
			mounted:   true,
			wantCode:  http.StatusOK,
			wantState: "ok",
		},
		{
			name:      "self-mounted but unmounted is unhealthy",
			mountPath: "/mnt/wisp",
			mounted:   false,
			wantCode:  http.StatusServiceUnavailable,
			wantState: "mount_down",
		},
		{
			// The operator mounts externally; wisp has no mount to report on, so
			// requiring one would wedge this at 503 forever.
			name:      "http-only is healthy despite no mount",
			mountPath: "",
			mounted:   false,
			wantCode:  http.StatusOK,
			wantState: "ok",
		},
		{
			name:      "http-only is healthy regardless of mount state",
			mountPath: "",
			mounted:   true,
			wantCode:  http.StatusOK,
			wantState: "ok",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &app{
				mountPath:            tc.mountPath,
				mountHealthyOverride: func() bool { return tc.mounted },
			}
			rec := httptest.NewRecorder()
			a.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

			// The status code is the entire contract for a Docker healthcheck.
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			var body map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if body["status"] != tc.wantState {
				t.Errorf("status field = %v, want %q", body["status"], tc.wantState)
			}
			if body["self_mount"] != (tc.mountPath != "") {
				t.Errorf("self_mount = %v, want %v", body["self_mount"], tc.mountPath != "")
			}
			if tc.mountPath == "" {
				if _, ok := body["mounted"]; ok {
					t.Errorf("http-only response should omit mounted, got %v", body["mounted"])
				}
			} else if body["mounted"] != tc.mounted {
				t.Errorf("mounted = %v, want %v", body["mounted"], tc.mounted)
			}

			// It must not leak configuration detail — no paths, URLs, or tokens.
			for _, k := range []string{"mount_path", "url", "token", "webhook"} {
				if _, ok := body[k]; ok {
					t.Errorf("response leaks %q", k)
				}
			}
		})
	}
}

// A nil mount must not panic — that is the real shape of an HTTP-only app.
func TestHandleHealthNilMountIsHTTPOnlyHealthy(t *testing.T) {
	a := &app{} // no mountPath, no mnt
	rec := httptest.NewRecorder()
	a.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// A self-mounting app whose mount was never established reports 503 through the
// real (nil-safe) mount probe, not just the test override.
func TestHandleHealthSelfMountNilMountIsUnhealthy(t *testing.T) {
	a := &app{mountPath: "/mnt/wisp"} // mnt is nil: never mounted
	rec := httptest.NewRecorder()
	a.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
