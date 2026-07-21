package plugin

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func newMonitor(t *testing.T, eps EpisodeLister) (*Monitor, *Library, string) {
	t.Helper()
	root := t.TempDir()
	lib := NewLibrary()
	w := NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/3", NewSigner("seed"))
	return NewMonitor(lib, w, eps, slog.New(slog.DiscardHandler)), lib, root
}

func seedShow(t *testing.T, root string, lib *Library, eps ...[2]int) {
	t.Helper()
	w := NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/3", NewSigner("seed"))
	for _, e := range eps {
		path, err := w.Write(Item{
			MediaType: "series", Title: "Demo Show", Year: 2024,
			ID: MediaID{SourceTVDB, "999"}, IMDbID: "tt0944947",
			Season: e[0], Episode: e[1], Quality: "1080p",
		})
		if err != nil {
			t.Fatal(err)
		}
		lib.Add(Placeholder{
			Path: path, MediaType: "series", ID: MediaID{SourceTVDB, "999"},
			IMDbID: "tt0944947", Season: e[0], Episode: e[1], Quality: "1080p",
		})
	}
}

func run(t *testing.T, m *Monitor) {
	t.Helper()
	if _, err := m.Run(context.Background(), &pluginv1.RunScheduledTaskRequest{TaskKey: TaskFillEpisodes}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

// The point of the task: an episode that has aired since the last pass gets a
// placeholder, so it appears in the library without anyone re-requesting it.
func TestMonitorWritesNewlyAiredEpisodes(t *testing.T) {
	eps := &stubEpisodes{eps: []EpisodeRef{{1, 1}, {1, 2}, {1, 3}}}
	m, lib, root := newMonitor(t, eps)
	seedShow(t, root, lib, [2]int{1, 1}, [2]int{1, 2})

	run(t, m)

	if lib.Count() != 3 {
		t.Fatalf("library has %d placeholders, want 3", lib.Count())
	}
	want := filepath.Join(root, rootShows, "Demo Show (2024) [tvdb-999]", "Season 01",
		"Demo Show (2024) S01E03 [1080p].strm")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("new episode not written at %s", want)
	}
}

// Re-running must not rewrite what is already there, or every pass would churn
// the library and re-trigger autoscan for unchanged files.
func TestMonitorIsIdempotent(t *testing.T) {
	eps := &stubEpisodes{eps: []EpisodeRef{{1, 1}, {1, 2}}}
	m, lib, root := newMonitor(t, eps)
	seedShow(t, root, lib, [2]int{1, 1}, [2]int{1, 2})

	before := lib.Count()
	run(t, m)
	run(t, m)

	if lib.Count() != before {
		t.Errorf("count changed %d -> %d across passes with nothing new", before, lib.Count())
	}
}

// A show is monitored because placeholders exist for it. Nothing tracked means
// nothing to do — and in particular the provider is never consulted.
func TestMonitorDoesNothingWithAnEmptyLibrary(t *testing.T) {
	eps := &stubEpisodes{eps: []EpisodeRef{{1, 1}}}
	m, lib, _ := newMonitor(t, eps)

	run(t, m)

	if lib.Count() != 0 {
		t.Errorf("wrote %d placeholders for an empty library", lib.Count())
	}
}

// Movies have no episodes to fill, so they must not be enumerated.
func TestMonitorIgnoresMovies(t *testing.T) {
	eps := &stubEpisodes{eps: []EpisodeRef{{1, 1}}}
	m, lib, root := newMonitor(t, eps)

	w := NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/3", NewSigner("seed"))
	path, err := w.Write(Item{MediaType: "movie", Title: "A Movie", Year: 2024,
		ID: MediaID{SourceTMDB, "1"}, IMDbID: "tt1", Quality: "1080p"})
	if err != nil {
		t.Fatal(err)
	}
	lib.Add(Placeholder{Path: path, MediaType: "movie", ID: MediaID{SourceTMDB, "1"}, IMDbID: "tt1"})

	run(t, m)

	if lib.Count() != 1 {
		t.Errorf("library has %d entries, want the movie untouched", lib.Count())
	}
}

// A show upgraded to a higher tier must not start collecting new episodes at
// its old one.
func TestMonitorUsesTheHighestTierAlreadyPresent(t *testing.T) {
	eps := &stubEpisodes{eps: []EpisodeRef{{1, 1}, {1, 2}}}
	m, lib, root := newMonitor(t, eps)

	w := NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/3", NewSigner("seed"))
	for _, q := range []string{"1080p", "2160p"} {
		path, err := w.Write(Item{MediaType: "series", Title: "Demo Show", Year: 2024,
			ID: MediaID{SourceTVDB, "999"}, IMDbID: "tt0944947", Season: 1, Episode: 1, Quality: q})
		if err != nil {
			t.Fatal(err)
		}
		lib.Add(Placeholder{Path: path, MediaType: "series", ID: MediaID{SourceTVDB, "999"},
			IMDbID: "tt0944947", Season: 1, Episode: 1, Quality: q})
	}

	run(t, m)

	found := false
	for _, p := range lib.List() {
		if p.Season == 1 && p.Episode == 2 {
			found = true
			if p.Quality != "2160p" {
				t.Errorf("new episode quality = %q, want the highest tier in use", p.Quality)
			}
		}
	}
	if !found {
		t.Error("the new episode was never written")
	}
}

// One unreachable show must not stop the pass for the others.
func TestMonitorSurvivesEnumerationFailure(t *testing.T) {
	m, lib, root := newMonitor(t, &stubEpisodes{err: context.DeadlineExceeded})
	seedShow(t, root, lib, [2]int{1, 1})

	run(t, m) // must not panic or error

	if lib.Count() != 1 {
		t.Errorf("library changed despite enumeration failing")
	}
}

func TestMonitorRejectsUnknownTask(t *testing.T) {
	m, _, _ := newMonitor(t, &stubEpisodes{})
	if _, err := m.Run(context.Background(), &pluginv1.RunScheduledTaskRequest{TaskKey: "something-else"}); err == nil {
		t.Error("Run() accepted an unknown task key")
	}
}

func TestTitleFromPath(t *testing.T) {
	cases := map[string]struct {
		title string
		year  int
	}{
		"/library/tv/Demo Show (2024) [tvdb-999]/Season 01/x.strm":     {"Demo Show", 2024},
		"/library/tv/No Year [tvdb-1]/Season 01/x.strm":                {"No Year", 0},
		"/library/tv/Parens (In) Title (2001) [tvdb-2]/Season 01/x.st": {"Parens (In) Title", 2001},
	}
	for path, want := range cases {
		title, year := titleFromPath(path)
		if title != want.title || year != want.year {
			t.Errorf("titleFromPath(%q) = (%q, %d), want (%q, %d)", path, title, year, want.title, want.year)
		}
	}
}

// The index is in-memory, so a restarted plugin must recover what it wrote from
// the placeholders themselves rather than losing track of the library.
func TestRebuildRecoversTheIndexFromDisk(t *testing.T) {
	_, lib, root := newMonitor(t, nil)
	seedShow(t, root, lib, [2]int{1, 1}, [2]int{1, 2}, [2]int{2, 1})

	fresh := NewLibrary()
	adopted, skipped, err := fresh.Rebuild(root)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if adopted != 3 {
		t.Errorf("adopted %d placeholders, want 3 (skipped %d)", adopted, skipped)
	}

	for _, p := range fresh.List() {
		if p.ID.String() != "tvdb:999" {
			t.Errorf("identity = %q, want tvdb:999", p.ID.String())
		}
		if p.IMDbID != "tt0944947" {
			t.Errorf("lookup key = %q, want the imdb id", p.IMDbID)
		}
		if p.Quality != "1080p" {
			t.Errorf("quality = %q, want 1080p", p.Quality)
		}
	}
}

// A library is a shared directory. One unreadable or foreign file must not stop
// the plugin from learning about the rest.
func TestRebuildSkipsJunkWithoutFailing(t *testing.T) {
	_, lib, root := newMonitor(t, nil)
	seedShow(t, root, lib, [2]int{1, 1})

	junk := filepath.Join(root, rootShows, "broken.strm")
	if err := os.WriteFile(junk, []byte("not a url at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rootShows, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	fresh := NewLibrary()
	adopted, skipped, err := fresh.Rebuild(root)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if adopted != 1 {
		t.Errorf("adopted %d, want the 1 valid placeholder", adopted)
	}
	if skipped != 1 {
		t.Errorf("skipped %d, want the 1 malformed placeholder (txt is not counted)", skipped)
	}
}

func TestRebuildRequiresAnExistingRoot(t *testing.T) {
	if _, _, err := NewLibrary().Rebuild(""); err == nil {
		t.Error("Rebuild accepted an empty root")
	}
	if _, _, err := NewLibrary().Rebuild(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("Rebuild accepted a missing root")
	}
}

// A rebuilt index must be usable by the monitor: this is the restart path.
func TestRebuiltIndexIsMonitorable(t *testing.T) {
	_, seedLib, root := newMonitor(t, nil)
	seedShow(t, root, seedLib, [2]int{1, 1})

	fresh := NewLibrary()
	if _, _, err := fresh.Rebuild(root); err != nil {
		t.Fatal(err)
	}

	m := NewMonitor(fresh, NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/3", NewSigner("seed")),
		&stubEpisodes{eps: []EpisodeRef{{1, 1}, {1, 2}}}, slog.New(slog.DiscardHandler))
	run(t, m)

	if fresh.Count() != 2 {
		t.Errorf("after restart the monitor produced %d placeholders, want 2", fresh.Count())
	}
	for _, p := range fresh.List() {
		if p.Season == 1 && p.Episode == 2 && !strings.Contains(p.Path, "S01E02") {
			t.Errorf("unexpected path for the filled episode: %s", p.Path)
		}
	}
}

// A show's category is decided when its first placeholder is written and then
// read back off disk, never re-derived. If the monitor re-classified, a
// metadata correction could start filing new episodes into a different root
// than the ones already in the library — which a media server sees as the show
// vanishing and a new one appearing, taking watch state with it.
func TestMonitorKeepsNewEpisodesInTheRootTheShowAlreadyLivesIn(t *testing.T) {
	root := t.TempDir()
	w := NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/1", nil)

	// An anime show with one episode already on disk.
	first, err := w.Write(Item{
		MediaType: "series", Title: "Frieren", Year: 2023,
		ID: MediaID{SourceTVDB, "424536"}, IMDbID: "tt22248376",
		Season: 1, Episode: 1, Quality: "1080p", Anime: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.ToSlash(first), rootAnimeShows) {
		t.Fatalf("setup wrote outside the anime root: %s", first)
	}

	// Rebuild from disk, exactly as a restart would.
	lib := NewLibrary()
	if _, _, err := lib.Rebuild(root); err != nil {
		t.Fatal(err)
	}

	m := NewMonitor(lib, w, &stubEpisodes{eps: []EpisodeRef{
		{Season: 1, Episode: 1},
		{Season: 1, Episode: 2}, // newly aired
	}}, nil)
	if _, err := m.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := filepath.Join(root, filepath.FromSlash(rootAnimeShows),
		"Frieren (2023) [tvdb-424536]", "Season 01", "Frieren (2023) S01E02 [1080p].strm")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("new episode not written beside the existing ones: %v", err)
	}
	// And emphatically not in the general root.
	stray := filepath.Join(root, rootShows, "Frieren (2023) [tvdb-424536]")
	if _, err := os.Stat(stray); err == nil {
		t.Error("the show was split across two roots")
	}
}
