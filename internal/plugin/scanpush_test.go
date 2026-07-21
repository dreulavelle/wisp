package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type stubPublisher struct {
	mu      sync.Mutex
	calls   []map[string]any
	names   []string
	failing bool
}

func (s *stubPublisher) PublishEvent(_ context.Context, name string, payload map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing {
		return errors.New("host unreachable")
	}
	s.names = append(s.names, name)
	s.calls = append(s.calls, payload)
	return nil
}

func (s *stubPublisher) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func TestPushReportsPathsAsFileScope(t *testing.T) {
	pub := &stubPublisher{}
	NewScanPusher(pub, nil).Push(context.Background(), []string{"/library/movies/A/A.strm"})

	if pub.count() != 1 {
		t.Fatalf("published %d event(s), want 1", pub.count())
	}
	if pub.names[0] != ScanEvent {
		t.Errorf("event name = %q, want %q", pub.names[0], ScanEvent)
	}
	// A placeholder is one file that now exists. Claiming subtree scope would
	// have the host walk the parent directory, which on a large library is a
	// great deal of work for one new episode.
	if got := pub.calls[0]["scope"]; got != "file" {
		t.Errorf("scope = %v, want file", got)
	}
	paths, ok := pub.calls[0]["source_paths"].([]any)
	if !ok || len(paths) != 1 || paths[0] != "/library/movies/A/A.strm" {
		t.Errorf("source_paths = %v", pub.calls[0]["source_paths"])
	}
}

// A season must arrive as one event, not one per episode: 24 separate
// notifications would have the host resolve and enqueue 24 times over for work
// it can do in a single pass.
func TestPushSendsOneEventForManyPaths(t *testing.T) {
	pub := &stubPublisher{}
	paths := make([]string, 24)
	for i := range paths {
		paths[i] = "/library/tv/Show/Season 01/E.strm"
	}
	NewScanPusher(pub, nil).Push(context.Background(), paths)

	if pub.count() != 1 {
		t.Fatalf("published %d event(s) for one batch, want 1", pub.count())
	}
}

// Nothing written means nothing to say. An empty push would still cost a round
// trip and log a report of zero files.
func TestPushIsSilentWithNothingToReport(t *testing.T) {
	pub := &stubPublisher{}
	p := NewScanPusher(pub, nil)
	p.Push(context.Background(), nil)
	p.Push(context.Background(), []string{})

	if pub.count() != 0 {
		t.Errorf("published %d event(s) with no paths", pub.count())
	}
}

// A failed push must not surface as an error. The placeholder is already on
// disk and the host's poll remains the backstop, so failing a request over a
// missed notification turns a slow success into an error for no gain.
func TestPushSurvivesAFailingHost(t *testing.T) {
	NewScanPusher(&stubPublisher{failing: true}, nil).
		Push(context.Background(), []string{"/library/movies/A/A.strm"})
	// Reaching here without a panic is the assertion.
}

// Running without a host connection must be a no-op, not a crash.
func TestPushWithoutAPublisherIsANoOp(t *testing.T) {
	NewScanPusher(nil, nil).Push(context.Background(), []string{"/a.strm"})
	var nilPusher *ScanPusher
	nilPusher.Push(context.Background(), []string{"/a.strm"})
}
