package notify

import "sync/atomic"

// targetMetrics are one target's delivery-outcome counters.
//
// Delivery is best-effort: a failed notification is logged and dropped, never
// retried. These counters exist to measure how often that actually happens, so
// the decision to build (or not build) a durable event journal rests on
// evidence rather than assumption.
//
// One counter is incremented per delivery *attempt*, where an attempt is a
// single outbound HTTP request. That is not always one per event: a Plex rename
// spanning two folders issues two refreshes, and a coalesced import batch is
// one attempt covering many files. The counters answer "what fraction of the
// requests wisp makes are accepted", which is the question that bears on
// reliability — not "how many files were notified".
//
// Every field is an atomic counter. Notifications fan out across goroutines
// (one per target per event) and pins arrive concurrently from the monitor's
// errgroup and the API path, so all increments must be race-free.
type targetMetrics struct {
	// delivered counts attempts that got a 2xx response.
	delivered atomic.Int64
	// failed counts attempts that got a transport error or a non-2xx response.
	failed atomic.Int64
	// dropped counts events discarded before any request was attempted.
	dropped atomic.Int64
}

// recordSend folds one finished HTTP attempt into the counters. status is the
// response code (0 when the request never completed) and err is the transport
// error, matching what postJSON and doAndDrain return.
func (m *targetMetrics) recordSend(status int, err error) {
	if err == nil && okStatus(status) {
		m.delivered.Add(1)
		return
	}
	m.failed.Add(1)
}

// recordDrop notes an event thrown away before wisp tried to deliver it.
func (m *targetMetrics) recordDrop() { m.dropped.Add(1) }

// TargetMetrics is a point-in-time read of one target's counters, for /metrics.
type TargetMetrics struct {
	// Target is the target's name, used as the Prometheus label value.
	Target string
	// Delivered is attempts answered 2xx. A 2xx means the consumer *accepted*
	// the notification, not that it acted on it: Silo answers 202 and may still
	// discard the event, and a media server may coalesce a scan away after
	// acknowledging it. This counter cannot see either case.
	Delivered int64
	// Failed is attempts that hit a transport error or a non-2xx response.
	// These events are lost — nothing retries them.
	Failed int64
	// Dropped is events discarded before any request was attempted, and so not
	// counted in Delivered or Failed. Only the Plex target drops this way
	// today: no configured library section covers the changed folder.
	Dropped int64
}

// Metrics returns a snapshot of every configured target's delivery counters, in
// the order the targets were configured. A Multi with no targets returns nil.
func (m *Multi) Metrics() []TargetMetrics {
	if m == nil {
		return nil
	}
	out := make([]TargetMetrics, 0, len(m.targets))
	for _, t := range m.targets {
		tm := t.metrics()
		out = append(out, TargetMetrics{
			Target:    t.name(),
			Delivered: tm.delivered.Load(),
			Failed:    tm.failed.Load(),
			Dropped:   tm.dropped.Load(),
		})
	}
	return out
}
