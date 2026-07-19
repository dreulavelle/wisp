package notify

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeTarget records what the Multi delivers, without any network.
type fakeTarget struct {
	mu      sync.Mutex
	batches []importBatch
	renames [][2]string
	deletes []string
	got     chan struct{}
}

func newFakeTarget() *fakeTarget {
	return &fakeTarget{got: make(chan struct{}, 64)}
}

func (f *fakeTarget) name() string { return "fake" }

func (f *fakeTarget) ImportBatch(_ context.Context, b importBatch) {
	f.mu.Lock()
	// Copy: the batch's slice is owned by the coalescer.
	f.batches = append(f.batches, importBatch{
		mediaType: b.mediaType, dir: b.dir, files: slices.Clone(b.files),
	})
	f.mu.Unlock()
	select {
	case f.got <- struct{}{}:
	default:
	}
}

func (f *fakeTarget) Import(ctx context.Context, mediaType, virtualPath string) {
	f.ImportBatch(ctx, importBatch{mediaType: mediaType, files: []string{virtualPath}})
}

func (f *fakeTarget) Rename(_ context.Context, _, previousPath, newPath string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renames = append(f.renames, [2]string{previousPath, newPath})
}

func (f *fakeTarget) Delete(_ context.Context, _, virtualPath string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, virtualPath)
	select {
	case f.got <- struct{}{}:
	default:
	}
}

func (f *fakeTarget) snapshot() []importBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.batches)
}

// waitForBatches blocks until at least n batches have been delivered, or fails.
func (f *fakeTarget) waitForBatches(t *testing.T, n int, within time.Duration) []importBatch {
	t.Helper()
	deadline := time.After(within)
	for {
		if b := f.snapshot(); len(b) >= n {
			return b
		}
		select {
		case <-f.got:
		case <-deadline:
			t.Fatalf("timed out waiting for %d batches; got %d (%v)", n, len(f.snapshot()), f.snapshot())
		}
	}
}

// newTestMulti builds a Multi wired to a single fake target, keeping New's
// coalescer and waitgroup setup.
func newTestMulti(debounce time.Duration) (*Multi, *fakeTarget) {
	m := New(Options{ArrWebhookURL: "http://unused", MountPath: "/mnt/wisp", Debounce: debounce}, slog.New(slog.DiscardHandler))
	f := newFakeTarget()
	m.targets = []target{f}
	return m, f
}

// The measured defect: a burst of episode pins in one season folder must reach
// the media server as ONE request naming every file, not one request per file.
func TestBurstInOneDirectoryCoalescesToOneBatch(t *testing.T) {
	m, f := newTestMulti(50 * time.Millisecond)
	defer m.Close(context.Background())

	// The reproduction: 7 episodes x 2 quality tiers, arriving concurrently the
	// way the monitor's errgroup delivers them.
	var wg sync.WaitGroup
	want := []string{}
	for ep := 1; ep <= 7; ep++ {
		for _, tier := range []string{"1080p", "2160p"} {
			vpath := "shows/Foo/Season 01/Foo - S01E0" + string(rune('0'+ep)) + " - " + tier + ".mkv"
			want = append(want, vpath)
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.Import(context.Background(), "series", vpath)
			}()
		}
	}
	wg.Wait()

	batches := f.waitForBatches(t, 1, 3*time.Second)
	if len(batches) != 1 {
		t.Fatalf("burst produced %d notifications, want exactly 1: %v", len(batches), batches)
	}
	b := batches[0]
	if b.dir != "shows/Foo/Season 01" {
		t.Errorf("batch dir = %q, want the season folder", b.dir)
	}
	if len(b.files) != len(want) {
		t.Errorf("batch carried %d files, want %d", len(b.files), len(want))
	}
	// Every pinned file must be represented; none may be lost to coalescing.
	for _, w := range want {
		if !slices.Contains(b.files, w) {
			t.Errorf("file %q missing from the coalesced batch", w)
		}
	}

	// And nothing further should arrive afterwards.
	time.Sleep(200 * time.Millisecond)
	if got := f.snapshot(); len(got) != 1 {
		t.Fatalf("got %d notifications after settling, want 1", len(got))
	}
}

// Two shows resolving concurrently must stay two independent events — the
// coalescing key is the parent directory, not "any burst".
func TestDistinctDirectoriesProduceDistinctBatches(t *testing.T) {
	m, f := newTestMulti(50 * time.Millisecond)
	defer m.Close(context.Background())

	m.Import(context.Background(), "series", "shows/Foo/Season 01/a.mkv")
	m.Import(context.Background(), "series", "shows/Bar/Season 02/b.mkv")
	m.Import(context.Background(), "series", "shows/Foo/Season 01/c.mkv")

	batches := f.waitForBatches(t, 2, 3*time.Second)
	if len(batches) != 2 {
		t.Fatalf("got %d batches, want 2: %v", len(batches), batches)
	}
	dirs := []string{batches[0].dir, batches[1].dir}
	slices.Sort(dirs)
	if !slices.Equal(dirs, []string{"shows/Bar/Season 02", "shows/Foo/Season 01"}) {
		t.Fatalf("batch dirs = %v, want one per show", dirs)
	}
	for _, b := range batches {
		if b.dir == "shows/Foo/Season 01" && len(b.files) != 2 {
			t.Errorf("Foo batch has %d files, want 2", len(b.files))
		}
		if b.dir == "shows/Bar/Season 02" && len(b.files) != 1 {
			t.Errorf("Bar batch has %d files, want 1", len(b.files))
		}
	}
}

// A continuous stream of pins whose gaps never exceed the quiet period must
// still be flushed, bounded by the max wait — otherwise a long season starves.
func TestMaxWaitFlushesUnderContinuousStream(t *testing.T) {
	const quiet = 60 * time.Millisecond
	// Derived from the constant, not from maxWaitFor, so the deadline this test
	// waits on can never be widened by a bug in the function under test.
	const wantMaxWait = quiet * notifyMaxWaitFactor // 360ms

	m, f := newTestMulti(quiet)
	defer m.Close(context.Background())

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Feed well inside the quiet period, so the quiet period alone would
		// never fire, for meaningfully longer than the max wait.
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(quiet / 4):
			}
			m.Import(context.Background(), "series", "shows/Foo/Season 01/ep.mkv."+string(rune('a'+i%26)))
		}
	}()

	start := time.Now()
	// A fixed, absolute deadline: an implementation with no max wait never
	// flushes at all under this stream, so it fails here rather than hanging.
	f.waitForBatches(t, 1, 5*time.Second)
	elapsed := time.Since(start)

	close(stop)
	<-done

	if elapsed < quiet {
		t.Fatalf("flushed after %v, sooner than the quiet period %v", elapsed, quiet)
	}
	// Generous slack for scheduler jitter, but tight enough to catch a max wait
	// that has drifted far from the documented 6x the quiet period.
	if elapsed > wantMaxWait*4 {
		t.Fatalf("flushed after %v, far past the expected max wait %v", elapsed, wantMaxWait)
	}
	t.Logf("max-wait flush fired after %v (want ~%v)", elapsed, wantMaxWait)
}

// The escape hatch: a zero window restores immediate per-file notification.
func TestDisabledDebouncePreservesPerFileBehavior(t *testing.T) {
	m, f := newTestMulti(0)
	defer m.Close(context.Background())

	if m.coalesce != nil {
		t.Fatal("zero debounce must not install a coalescer")
	}
	files := []string{
		"shows/Foo/Season 01/a.mkv",
		"shows/Foo/Season 01/b.mkv",
		"shows/Foo/Season 01/c.mkv",
	}
	for _, vp := range files {
		m.Import(context.Background(), "series", vp)
	}

	batches := f.waitForBatches(t, len(files), 3*time.Second)
	if len(batches) != len(files) {
		t.Fatalf("got %d notifications, want one per file (%d)", len(batches), len(files))
	}
	for i, b := range batches {
		if len(b.files) != 1 {
			t.Fatalf("notification %d carried %d files, want exactly 1", i, len(b.files))
		}
	}
	var got []string
	for _, b := range batches {
		got = append(got, b.files[0])
	}
	slices.Sort(got)
	if !slices.Equal(got, files) {
		t.Fatalf("delivered %v, want %v", got, files)
	}
}

// A pin landing inside the debounce window must survive a restart, so Close
// flushes rather than dropping.
func TestCloseFlushesPendingImports(t *testing.T) {
	// A window far longer than the test, so nothing can flush on its own.
	m, f := newTestMulti(time.Minute)

	m.Import(context.Background(), "series", "shows/Foo/Season 01/a.mkv")
	m.Import(context.Background(), "series", "shows/Foo/Season 01/b.mkv")

	if got := f.snapshot(); len(got) != 0 {
		t.Fatalf("expected nothing delivered before Close, got %v", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.Close(ctx)

	// Close must have drained delivery, so no waiting is needed here.
	batches := f.snapshot()
	if len(batches) != 1 {
		t.Fatalf("Close delivered %d batches, want 1: %v", len(batches), batches)
	}
	if len(batches[0].files) != 2 {
		t.Fatalf("flushed batch carried %d files, want 2", len(batches[0].files))
	}

	// Close is idempotent and post-close imports still get through.
	m.Close(ctx)
	m.Import(context.Background(), "series", "shows/Foo/Season 01/c.mkv")
	f.waitForBatches(t, 2, 3*time.Second)
}

// Deletes carry an exact path and must never be batched or folder-collapsed.
func TestDeletesAreNotDebounced(t *testing.T) {
	m, f := newTestMulti(time.Minute)
	defer m.Close(context.Background())

	m.Delete(context.Background(), "series", "shows/Foo/Season 01/a.mkv")

	deadline := time.After(3 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.deletes)
		f.mu.Unlock()
		if n == 1 {
			return
		}
		select {
		case <-f.got:
		case <-deadline:
			t.Fatal("delete was not delivered immediately")
		}
	}
}

// A duplicate pin of the same path inside one window collapses to one entry.
func TestBatchDeduplicatesRepeatedPaths(t *testing.T) {
	m, f := newTestMulti(50 * time.Millisecond)
	defer m.Close(context.Background())

	for range 3 {
		m.Import(context.Background(), "series", "shows/Foo/Season 01/a.mkv")
	}

	batches := f.waitForBatches(t, 1, 3*time.Second)
	if len(batches[0].files) != 1 {
		t.Fatalf("batch carried %d files, want 1 after dedup: %v", len(batches[0].files), batches[0].files)
	}
}
