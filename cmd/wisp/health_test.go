package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Health used to be a verdict on the FUSE mount: a self-mounting deployment had
// to fail closed while its mount was down, so a media server would not start
// scanning an empty mountpoint. Placeholders removed the mount, and with it that
// entire failure mode.
//
// What remains is a liveness probe, and its most important property is what it
// does NOT consider. An AIOStreams outage must not mark wisp unhealthy:
// placeholders resolve again the moment the provider recovers, and restarting
// the container — which is what an unhealthy verdict invites — would not help.
func TestHandleHealthIsAlwaysOKWhileRunning(t *testing.T) {
	a := &app{}

	rec := httptest.NewRecorder()
	a.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("health body is not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v, want ok", body["status"])
	}
}

// The probe must not report mount state that no longer exists, or an operator
// reading it will draw conclusions about a subsystem that was removed.
func TestHandleHealthReportsNoMountState(t *testing.T) {
	a := &app{}

	rec := httptest.NewRecorder()
	a.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"mounted", "self_mount", "mount_path"} {
		if _, present := body[stale]; present {
			t.Errorf("health still reports %q; there is no mount any more", stale)
		}
	}
}
