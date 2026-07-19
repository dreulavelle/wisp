package config

import (
	"testing"
	"time"
)

func TestSizeEnv(t *testing.T) {
	cases := map[string]int64{
		"16M":     16 << 20,
		"512M":    512 << 20,
		"1G":      1 << 30,
		"8k":      8 << 10,
		"1048576": 1 << 20,
		"":        99, // fallback
		"bogus":   99, // fallback
		"0M":      99, // non-positive → fallback
	}
	for in, want := range cases {
		t.Setenv("WISP_TEST_SIZE", in)
		if got := sizeEnv("WISP_TEST_SIZE", 99); got != want {
			t.Fatalf("sizeEnv(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestBoolEnv(t *testing.T) {
	for in, want := range map[string]bool{"true": true, "1": true, "on": true, "false": false, "0": false, "": true} {
		t.Setenv("WISP_TEST_BOOL", in)
		if got := boolEnv("WISP_TEST_BOOL", true); got != want {
			t.Fatalf("boolEnv(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDurationEnv(t *testing.T) {
	t.Setenv("WISP_TEST_DUR", "90m")
	if got := durationEnv("WISP_TEST_DUR", time.Hour); got != 90*time.Minute {
		t.Fatalf("dur = %s", got)
	}
	t.Setenv("WISP_TEST_DUR", "garbage")
	if got := durationEnv("WISP_TEST_DUR", 2*time.Hour); got != 2*time.Hour {
		t.Fatalf("fallback = %s", got)
	}
}

func TestIntEnv(t *testing.T) {
	cases := map[string]int{
		"8":     8,
		"0":     0,
		"-3":    -3,
		"":      7, // fallback
		"bogus": 7, // fallback
		"1.5":   7, // fallback (not an int)
	}
	for in, want := range cases {
		t.Setenv("WISP_TEST_INT", in)
		if got := intEnv("WISP_TEST_INT", 7); got != want {
			t.Fatalf("intEnv(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestClampInt(t *testing.T) {
	cases := []struct {
		n, lo, hi, want int
	}{
		{5, 1, 16, 5},   // in range
		{0, 1, 16, 1},   // below floor
		{-4, 1, 16, 1},  // below floor
		{99, 1, 16, 16}, // above ceiling
		{16, 1, 16, 16}, // at ceiling
		{1, 1, 16, 1},   // at floor
	}
	for _, c := range cases {
		if got := clampInt(c.n, c.lo, c.hi); got != c.want {
			t.Fatalf("clampInt(%d, %d, %d) = %d, want %d", c.n, c.lo, c.hi, got, c.want)
		}
	}
}

func TestClampDuration(t *testing.T) {
	lo, hi := 2*time.Second, 30*time.Second
	cases := []struct {
		d, want time.Duration
	}{
		{10 * time.Second, 10 * time.Second}, // in range
		{time.Second, lo},                    // below floor
		{time.Minute, hi},                    // above ceiling
		{lo, lo},                             // at floor
		{hi, hi},                             // at ceiling
	}
	for _, c := range cases {
		if got := clampDuration(c.d, lo, hi); got != c.want {
			t.Fatalf("clampDuration(%v) = %v, want %v", c.d, got, c.want)
		}
	}
}

// The probe knobs load with the documented defaults and clamp out-of-range env.
func TestLoadProbeDefaultsAndClamps(t *testing.T) {
	t.Setenv("WISP_AIOSTREAMS_URL", "https://host/stremio/uuid/blob/manifest.json")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ProbeConcurrency != 8 || c.ProbeWindow != 3 || c.ProbeTimeout != 10*time.Second {
		t.Fatalf("defaults = (%d, %d, %v), want (8, 3, 10s)", c.ProbeConcurrency, c.ProbeWindow, c.ProbeTimeout)
	}

	t.Setenv("WISP_PROBE_CONCURRENCY", "999")
	t.Setenv("WISP_PROBE_WINDOW", "0")
	t.Setenv("WISP_PROBE_TIMEOUT", "500ms")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ProbeConcurrency != 32 {
		t.Fatalf("ProbeConcurrency = %d, want 32 (clamped)", c.ProbeConcurrency)
	}
	if c.ProbeWindow != 1 {
		t.Fatalf("ProbeWindow = %d, want 1 (clamped)", c.ProbeWindow)
	}
	if c.ProbeTimeout != 2*time.Second {
		t.Fatalf("ProbeTimeout = %v, want 2s (clamped)", c.ProbeTimeout)
	}
}

// Notifications configured without any mount path must keep starting — the
// notifier's /mnt/wisp default carries the deployment — but Load has to flag it
// so main can emit the deprecation warning.
func TestLoadDefaultsNotificationMountPath(t *testing.T) {
	t.Setenv("WISP_AIOSTREAMS_URL", "https://host/stremio/uuid/blob/manifest.json")
	t.Setenv("WISP_NOTIFY_ARR_WEBHOOK_URL", "https://silo/autoscan")
	c, err := Load()
	if err != nil {
		t.Fatalf("notifications without a mount path must not fail: %v", err)
	}
	if c.NotifyMountPath != "" {
		t.Fatalf("NotifyMountPath = %q, want empty so notify applies its default", c.NotifyMountPath)
	}
	if !c.NotifyMountPathDefaulted {
		t.Fatal("NotifyMountPathDefaulted = false, want true so the caller warns")
	}

	// An explicit value is honoured verbatim and must not be flagged.
	t.Setenv("WISP_NOTIFY_MOUNT_PATH", "/silo/visible/wisp")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.NotifyMountPath != "/silo/visible/wisp" {
		t.Fatalf("NotifyMountPath = %q", c.NotifyMountPath)
	}
	if c.NotifyMountPathDefaulted {
		t.Fatal("NotifyMountPathDefaulted = true for an explicitly configured path")
	}
}

// With no notification targets there is nothing to warn about, however the
// mount paths are configured.
func TestLoadDoesNotFlagMountPathWithoutNotifyTargets(t *testing.T) {
	t.Setenv("WISP_AIOSTREAMS_URL", "https://host/stremio/uuid/blob/manifest.json")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.NotifyMountPathDefaulted {
		t.Fatal("NotifyMountPathDefaulted = true with no notification targets configured")
	}
}

func TestLoadUsesExplicitSelfMountForNotifications(t *testing.T) {
	t.Setenv("WISP_AIOSTREAMS_URL", "https://host/stremio/uuid/blob/manifest.json")
	t.Setenv("WISP_NOTIFY_ARR_WEBHOOK_URL", "https://silo/autoscan")
	t.Setenv("WISP_MOUNT_PATH", "/configured/wisp")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.NotifyMountPath != "/configured/wisp" {
		t.Fatalf("NotifyMountPath = %q", c.NotifyMountPath)
	}
	if c.NotifyMountPathDefaulted {
		t.Fatal("NotifyMountPathDefaulted = true when WISP_MOUNT_PATH supplied the path")
	}
}

func TestListEnv(t *testing.T) {
	t.Setenv("WISP_TEST_LIST", " us , gb ,, jp ")
	got := listEnv("WISP_TEST_LIST", []string{"X"})
	if len(got) != 3 || got[0] != "US" || got[2] != "JP" {
		t.Fatalf("list = %v", got)
	}
	t.Setenv("WISP_TEST_LIST", "")
	if got := listEnv("WISP_TEST_LIST", []string{"X"}); len(got) != 1 || got[0] != "X" {
		t.Fatalf("fallback = %v", got)
	}
}

func TestLoadNotifyDebounce(t *testing.T) {
	t.Setenv("WISP_AIOSTREAMS_URL", "https://host/stremio/uuid/blob/manifest.json")

	// Unset: coalescing is on by default.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.NotifyDebounce != 5*time.Second {
		t.Fatalf("default debounce = %v, want 5s", c.NotifyDebounce)
	}

	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"10s", 10 * time.Second},
		{"0", 0},                     // explicit zero disables — the escape hatch
		{"0s", 0},                    // ...in either spelling
		{"1ms", time.Second},         // clamped up
		{"10m", time.Minute},         // clamped down
		{"garbage", 5 * time.Second}, // unparseable falls back
		{"-5s", 5 * time.Second},     // negative falls back
	} {
		t.Setenv("WISP_NOTIFY_DEBOUNCE", tc.in)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.NotifyDebounce != tc.want {
			t.Errorf("debounce(%q) = %v, want %v", tc.in, c.NotifyDebounce, tc.want)
		}
	}
}
