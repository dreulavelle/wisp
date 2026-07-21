package plugin

import (
	"context"
	"log/slog"
	"strconv"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func newScanSource(t *testing.T) (*ScanSource, *Library) {
	t.Helper()
	lib := NewLibrary()
	return NewScanSource(lib, slog.New(slog.DiscardHandler)), lib
}

func poll(t *testing.T, s *ScanSource, marker string) *pluginv1.PollChangesResponse {
	t.Helper()
	resp, err := s.PollChanges(context.Background(), &pluginv1.PollChangesRequest{Marker: marker})
	if err != nil {
		t.Fatalf("PollChanges() error = %v", err)
	}
	return resp
}

// markerFor builds the marker a library at the given position would emit.
func markerFor(l *Library, seq uint64) string {
	return strconv.FormatUint(l.Epoch(), 10) + ":" + strconv.FormatUint(seq, 10)
}

func addPlaceholder(l *Library, path string) {
	l.Add(Placeholder{Path: path, MediaType: "movie", ID: MediaID{SourceTMDB, "1"}})
}

// The contract says an empty marker means "start from now, do not replay
// history". Replaying would make configuring a source enqueue a full rescan of
// the library — the exact storm autoscan exists to avoid.
func TestFirstPollDoesNotReplayHistory(t *testing.T) {
	s, lib := newScanSource(t)
	for _, p := range []string{"/library/a.strm", "/library/b.strm", "/library/c.strm"} {
		addPlaceholder(lib, p)
	}

	resp := poll(t, s, "")

	if len(resp.GetSourcePaths()) != 0 || len(resp.GetChanges()) != 0 {
		t.Errorf("first poll replayed %d path(s); it must start from now", len(resp.GetSourcePaths()))
	}
	if got, want := resp.GetNextMarker(), markerFor(lib, 3); got != want {
		t.Errorf("next marker = %q, want the current cursor %q", got, want)
	}
}

func TestPollReportsOnlyNewPlaceholders(t *testing.T) {
	s, lib := newScanSource(t)
	addPlaceholder(lib, "/library/old.strm")

	marker := poll(t, s, "").GetNextMarker()

	addPlaceholder(lib, "/library/new1.strm")
	addPlaceholder(lib, "/library/new2.strm")

	resp := poll(t, s, marker)

	paths := resp.GetSourcePaths()
	if len(paths) != 2 {
		t.Fatalf("reported %d paths, want 2: %v", len(paths), paths)
	}
	for _, p := range paths {
		if p == "/library/old.strm" {
			t.Error("reported a placeholder that predates the marker")
		}
	}
}

// The host stores the marker verbatim, so out-of-order reporting would let a
// crash between polls lose whatever fell in between.
func TestPollReportsInWriteOrder(t *testing.T) {
	s, lib := newScanSource(t)
	marker := poll(t, s, "").GetNextMarker()

	want := []string{"/library/1.strm", "/library/2.strm", "/library/3.strm", "/library/4.strm"}
	for _, p := range want {
		addPlaceholder(lib, p)
	}

	got := poll(t, s, marker).GetSourcePaths()
	if len(got) != len(want) {
		t.Fatalf("reported %d paths, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path[%d] = %q, want %q (write order must be preserved)", i, got[i], want[i])
		}
	}
}

func TestPollIsIdempotentWithoutNewWrites(t *testing.T) {
	s, lib := newScanSource(t)
	marker := poll(t, s, "").GetNextMarker()
	addPlaceholder(lib, "/library/a.strm")

	first := poll(t, s, marker)
	if len(first.GetSourcePaths()) != 1 {
		t.Fatalf("first poll reported %d paths, want 1", len(first.GetSourcePaths()))
	}

	// Polling again with the advanced marker must report nothing: re-reporting
	// would make the host rescan the same file on every tick forever.
	second := poll(t, s, first.GetNextMarker())
	if len(second.GetSourcePaths()) != 0 {
		t.Errorf("second poll re-reported %d path(s)", len(second.GetSourcePaths()))
	}
	if second.GetNextMarker() != first.GetNextMarker() {
		t.Errorf("marker moved with no new writes: %q -> %q", first.GetNextMarker(), second.GetNextMarker())
	}
}

// A placeholder is one file that now exists. Saying so keeps the host from
// walking the parent directory, which on a large library is a lot of wasted
// work for a single new episode.
func TestPollMarksChangesAsFileScope(t *testing.T) {
	s, lib := newScanSource(t)
	marker := poll(t, s, "").GetNextMarker()
	addPlaceholder(lib, "/library/a.strm")

	for _, c := range poll(t, s, marker).GetChanges() {
		if c.GetScope() != pluginv1.ScanSourceChangeScope_SCAN_SOURCE_CHANGE_SCOPE_FILE {
			t.Errorf("scope = %v, want FILE", c.GetScope())
		}
	}
}

// Structured changes are preferred by the host; source_paths keeps older hosts
// working. Both must describe the same set.
func TestPollPopulatesBothChangeRepresentations(t *testing.T) {
	s, lib := newScanSource(t)
	marker := poll(t, s, "").GetNextMarker()
	addPlaceholder(lib, "/library/a.strm")
	addPlaceholder(lib, "/library/b.strm")

	resp := poll(t, s, marker)
	if len(resp.GetSourcePaths()) != len(resp.GetChanges()) {
		t.Fatalf("source_paths has %d entries but changes has %d",
			len(resp.GetSourcePaths()), len(resp.GetChanges()))
	}
	for i, c := range resp.GetChanges() {
		if c.GetSourcePath() != resp.GetSourcePaths()[i] {
			t.Errorf("changes[%d] = %q but source_paths[%d] = %q",
				i, c.GetSourcePath(), i, resp.GetSourcePaths()[i])
		}
	}
}

// The host cannot interpret our marker, so a corrupt one is ours to recover
// from. Resyncing loses at most the writes during corruption; replaying the
// library would be far worse.
func TestPollRecoversFromCorruptMarker(t *testing.T) {
	s, lib := newScanSource(t)
	for i := 0; i < 5; i++ {
		addPlaceholder(lib, "/library/"+strconv.Itoa(i)+".strm")
	}

	for _, corrupt := range []string{"x:1", "1:x", "-1:2", "9e99:1", "  :  ", "1:1.5"} {
		resp := poll(t, s, corrupt)
		if len(resp.GetSourcePaths()) != 0 {
			t.Errorf("marker %q: replayed %d path(s) instead of resyncing",
				corrupt, len(resp.GetSourcePaths()))
		}
		if got, want := resp.GetNextMarker(), markerFor(lib, 5); got != want {
			t.Errorf("marker %q: next = %q, want a resync to %q", corrupt, got, want)
		}
	}
}

// A marker ahead of our cursor is a position this incarnation never issued.
// Echoing it back would make every later poll match nothing, so it is clamped
// to the cursor instead — the placeholders themselves are already on disk.
func TestPollClampsMarkerAheadOfCursor(t *testing.T) {
	s, lib := newScanSource(t)
	addPlaceholder(lib, "/library/a.strm")

	resp := poll(t, s, markerFor(lib, 999))
	if len(resp.GetSourcePaths()) != 0 {
		t.Errorf("reported %d path(s) for a marker ahead of the cursor", len(resp.GetSourcePaths()))
	}
	if got, want := resp.GetNextMarker(), markerFor(lib, 1); got != want {
		t.Errorf("next marker = %q, want it clamped to the cursor %q", got, want)
	}
}

// The regression this epoch exists to prevent. Sequence numbers restart with
// the process and Rebuild re-derives them from surviving files, so a library
// that lost placeholders comes back with a cursor BELOW the marker the host
// still holds. Before epochs, Since() then matched nothing forever and every
// new placeholder was silently invisible to autoscan.
func TestPollRecoversAfterRestartWithAShorterLibrary(t *testing.T) {
	// A previous incarnation got as far as position 800.
	before, _ := newScanSource(t)
	stale := before.marker(800)

	// The plugin restarts; rebuilding finds only three placeholders survived.
	s, lib := newScanSource(t)
	for _, p := range []string{"/library/a.strm", "/library/b.strm", "/library/c.strm"} {
		addPlaceholder(lib, p)
	}

	// The host still hands back the marker from the old incarnation.
	resp := poll(t, s, stale)
	if got, want := resp.GetNextMarker(), markerFor(lib, 3); got != want {
		t.Fatalf("next marker = %q, want a resync to %q", got, want)
	}

	// The point of resyncing: a placeholder written now must be reported.
	addPlaceholder(lib, "/library/new.strm")
	got := poll(t, s, resp.GetNextMarker()).GetSourcePaths()
	if len(got) != 1 || got[0] != "/library/new.strm" {
		t.Fatalf("new placeholder not reported after resync: %v", got)
	}
}

// A marker written before markers carried an epoch cannot be validated against
// this incarnation, so it must be treated as foreign rather than trusted.
func TestPollResyncsOnPreEpochMarker(t *testing.T) {
	s, lib := newScanSource(t)
	for i := 0; i < 4; i++ {
		addPlaceholder(lib, "/library/"+strconv.Itoa(i)+".strm")
	}

	resp := poll(t, s, "2")
	if len(resp.GetSourcePaths()) != 0 {
		t.Errorf("replayed %d path(s) for a pre-epoch marker", len(resp.GetSourcePaths()))
	}
	if got, want := resp.GetNextMarker(), markerFor(lib, 4); got != want {
		t.Errorf("next marker = %q, want a resync to %q", got, want)
	}
}
