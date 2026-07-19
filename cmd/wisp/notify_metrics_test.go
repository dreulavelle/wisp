package main

import (
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dreulavelle/wisp/internal/notify"
)

// The exposition must carry one labelled series per target per outcome, and it
// must emit zeros rather than omitting them: a target that has never failed
// still needs a failure series present, or a rate() over it has nothing to
// compare against on the first scrape after a failure appears.
func TestWriteNotifyMetricsRendersLabelledSeries(t *testing.T) {
	w := httptest.NewRecorder()
	writeNotifyMetrics(w, []notify.TargetMetrics{
		{Target: "arr-webhook", Delivered: 7, Failed: 2, Dropped: 0},
		{Target: "plex", Delivered: 0, Failed: 0, Dropped: 3},
	})
	body := w.Body.String()

	for _, want := range []string{
		`wisp_notify_deliveries_total{target="arr-webhook",result="success"} 7`,
		`wisp_notify_deliveries_total{target="arr-webhook",result="failure"} 2`,
		`wisp_notify_deliveries_total{target="plex",result="success"} 0`,
		`wisp_notify_deliveries_total{target="plex",result="failure"} 0`,
		`wisp_notify_dropped_total{target="arr-webhook"} 0`,
		`wisp_notify_dropped_total{target="plex"} 3`,
		"# TYPE wisp_notify_deliveries_total counter",
		"# TYPE wisp_notify_dropped_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}

	// Each metric family declares HELP and TYPE exactly once, before its series.
	if n := strings.Count(body, "# HELP wisp_notify_deliveries_total"); n != 1 {
		t.Errorf("HELP for deliveries appeared %d times, want 1", n)
	}

	// A HELP line must be a single line; a stray newline in the text would make
	// the rest of it parse as a metric sample and break the scrape.
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if line == "" {
			t.Error("blank line in exposition")
		}
	}
}

// With no targets configured there is nothing to say, and emitting a bare
// HELP/TYPE pair with no samples is noise on every scrape.
func TestWriteNotifyMetricsEmitsNothingWithoutTargets(t *testing.T) {
	w := httptest.NewRecorder()
	writeNotifyMetrics(w, nil)
	if got := w.Body.String(); got != "" {
		t.Errorf("body = %q, want empty", got)
	}
}

// The /metrics handler must reach the real notifier's counters. This is the
// wiring that a swapped-in test fake silently skips, so pin it explicitly.
func TestNotifierSatisfiesMetricsSource(t *testing.T) {
	var n any = notify.New(notify.Options{}, slog.New(slog.DiscardHandler))
	if _, ok := n.(notifyMetricsSource); !ok {
		t.Fatal("*notify.Multi does not satisfy notifyMetricsSource; /metrics would silently omit delivery counters")
	}
}
