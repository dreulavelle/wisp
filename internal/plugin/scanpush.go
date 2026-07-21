package plugin

import (
	"context"
	"log/slog"
	"time"
)

// ScanEvent is the event name Silo routes into autoscan. The host prefixes it
// with "plugin.wisp.", which is how it knows who sent it.
const ScanEvent = "scan.changes"

// pushTimeout bounds one push. It is an in-process gRPC call to the host, so
// this only exists to stop a wedged host from holding an intake open.
const pushTimeout = 10 * time.Second

// EventPublisher is the slice of the host connection ScanPusher needs, narrowed
// so tests do not need a live host.
type EventPublisher interface {
	PublishEvent(ctx context.Context, name string, payload map[string]any) error
}

// ScanPusher tells Silo about placeholders the moment they are written.
//
// Without this, a new placeholder waits for autoscan's next poll — up to ten
// minutes on the default interval. For on-demand playback that delay is the
// whole point of the feature: a request is supposed to become a playable item
// straight away, and an item nobody can see yet may as well not exist.
//
// Wisp writes the file, so it knows precisely when it appears. Saying so is
// strictly better than being asked later.
//
// Best-effort by design. A failed push is logged, never returned: the
// placeholder is already on disk and Silo's poll will find it regardless, so
// failing a request over a missed notification would turn a slow success into
// an error for no gain.
type ScanPusher struct {
	events EventPublisher
	log    *slog.Logger
}

// NewScanPusher returns a pusher over a host connection. A nil publisher makes
// every push a no-op, which is what running without a host should do.
func NewScanPusher(events EventPublisher, log *slog.Logger) *ScanPusher {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ScanPusher{events: events, log: log}
}

// Push reports written placeholder paths to the host.
func (p *ScanPusher) Push(ctx context.Context, paths []string) {
	if p == nil || p.events == nil || len(paths) == 0 {
		return
	}

	// The payload names each path as a file rather than a directory. A
	// placeholder is exactly one file that now exists, and saying so keeps the
	// host from walking the parent — which on a large library is a great deal
	// of work for one new episode.
	payload := map[string]any{
		"source_paths": toAny(paths),
		"scope":        "file",
	}

	pushCtx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	if err := p.events.PublishEvent(pushCtx, ScanEvent, payload); err != nil {
		// Not an error for the caller: the file is written, and the host's poll
		// remains the backstop. Worth logging, because the visible symptom is
		// items appearing minutes late rather than instantly.
		p.log.Warn("scanpush: could not tell Silo about new placeholders; they will appear on the next poll instead",
			"count", len(paths), "error", err)
		return
	}
	p.log.Info("scanpush: reported new placeholders to Silo", "count", len(paths))
}

// toAny converts paths for structpb, which only takes []any.
func toAny(paths []string) []any {
	out := make([]any, 0, len(paths))
	for _, p := range paths {
		out = append(out, p)
	}
	return out
}
