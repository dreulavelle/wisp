package plugin

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestWriter(t *testing.T) (*Writer, string) {
	t.Helper()
	root := t.TempDir()
	return NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/3", NewSigner("seed")), root
}

func TestWriteMoviePlaceholder(t *testing.T) {
	w, root := newTestWriter(t)

	path, err := w.Write(Item{
		MediaType: "movie", Title: "The Matrix", Year: 1999,
		ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093", Quality: "2160p",
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	want := filepath.Join(root, "Movies", "The Matrix (1999) [tmdb-603]", "The Matrix (1999) [2160p].strm")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read placeholder: %v", err)
	}
	line := strings.TrimSpace(string(body))
	if !strings.HasPrefix(line, "http://127.0.0.1:8080/api/v1/plugins/3/resolve/movie/tmdb:603?") {
		t.Errorf("content = %q, want a resolver URL", line)
	}
	if !strings.Contains(line, "quality=2160p") {
		t.Errorf("content = %q, missing quality", line)
	}
	if !strings.Contains(line, "t=") {
		t.Errorf("content = %q, missing signature", line)
	}
}

func TestWriteEpisodePlaceholder(t *testing.T) {
	w, root := newTestWriter(t)

	path, err := w.Write(Item{
		MediaType: "series", Title: "Game of Thrones", Year: 2011,
		ID: MediaID{SourceTVDB, "121361"}, IMDbID: "tt0944947", Season: 1, Episode: 9, Quality: "1080p",
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	want := filepath.Join(root, "Shows", "Game of Thrones (2011) [tvdb-121361]", "Season 01",
		"Game of Thrones (2011) S01E09 [1080p].strm")
	if path != want {
		t.Errorf("path = %q\nwant %q", path, want)
	}

	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "/resolve/series/tvdb:121361/1/9?") {
		t.Errorf("content = %q, want season and episode in the path", body)
	}
}

// The written token must actually authorize the request the placeholder makes,
// or every placeholder is dead on arrival.
func TestWrittenPlaceholderIsSelfConsistent(t *testing.T) {
	signer := NewSigner("seed")
	w := NewWriter(t.TempDir(), "http://127.0.0.1:8080/api/v1/plugins/3", signer)

	item := Item{MediaType: "series", Title: "Severance", ID: MediaID{SourceTVDB, "371980"},
		IMDbID: "tt11280740", Season: 2, Episode: 7, Quality: "1080p"}
	path, err := w.Write(item)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	body, _ := os.ReadFile(path)
	u, err := url.Parse(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatalf("placeholder is not a URL: %v", err)
	}

	req, err := ParseResolvePath(u.Path)
	if err != nil {
		t.Fatalf("written path does not parse as a resolver path: %v", err)
	}
	req.Quality = u.Query().Get("quality")
	req.IMDbID = u.Query().Get("imdb")

	if !signer.Verify(req, u.Query().Get("t")) {
		t.Error("the token written into the placeholder does not verify against its own URL")
	}
}

// Titles reach us from metadata providers and, indirectly, from whoever
// requested the item. A title must never be able to escape the library root.
func TestWriteRejectsPathTraversal(t *testing.T) {
	w, root := newTestWriter(t)

	hostile := []string{
		"../../etc/passwd",
		"..",
		".",
		"../escape",
		"a/b/c",
		`..\..\windows`,
		"....//....//etc",
	}

	for _, title := range hostile {
		t.Run(title, func(t *testing.T) {
			path, err := w.Write(Item{MediaType: "movie", Title: title, ID: MediaID{SourceTMDB, "1"}, IMDbID: "tt1"})
			if err != nil {
				return // rejected outright, which is fine
			}
			abs, _ := filepath.Abs(path)
			rootAbs, _ := filepath.Abs(root)
			if !strings.HasPrefix(abs, rootAbs+string(os.PathSeparator)) {
				t.Errorf("title %q escaped the library root: %s", title, abs)
			}
		})
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"The Matrix":              "The Matrix",
		"  padded  ":              "padded",
		"a/b":                     "ab",
		"a:b*c?d":                 "abcd",
		`quote"less`:              "quoteless",
		"multi   space":           "multi space",
		"trailing...":             "trailing",
		".hidden":                 "hidden",
		"line\nbreak":             "line break",
		"tab\tsep":                "tab sep",
		"WALL·E":                  "WALL·E",
		"Amélie":                  "Amélie",
		"劇場版":                     "劇場版",
		"..":                      "",
		"...":                     "",
		"/":                       "",
		"Spider-Man: No Way Home": "Spider-Man No Way Home",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// A scan can run at any moment. A scanner that reads a half-written .strm would
// record an item with a truncated URL and no obvious way to notice, so the file
// must appear complete or not at all.
func TestWriteIsAtomic(t *testing.T) {
	w, root := newTestWriter(t)

	if _, err := w.Write(Item{MediaType: "movie", Title: "Atomic", Year: 2020, ID: MediaID{SourceTMDB, "1"}, IMDbID: "tt1"}); err != nil {
		t.Fatal(err)
	}

	// No temporary files may survive a successful write.
	var leftovers []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.Contains(filepath.Base(p), ".wisp-") {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	if len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestWriteIsIdempotent(t *testing.T) {
	w, _ := newTestWriter(t)
	item := Item{MediaType: "movie", Title: "Dune", Year: 2021, ID: MediaID{SourceTMDB, "438631"}, IMDbID: "tt1160419", Quality: "2160p"}

	first, err := w.Write(item)
	if err != nil {
		t.Fatal(err)
	}
	firstBody, _ := os.ReadFile(first)

	second, err := w.Write(item)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, _ := os.ReadFile(second)

	if first != second {
		t.Errorf("paths differ across writes: %q vs %q", first, second)
	}
	if string(firstBody) != string(secondBody) {
		t.Error("content differs across writes; placeholders must be stable")
	}
}

func TestWritePermissionsAreReadable(t *testing.T) {
	w, _ := newTestWriter(t)
	path, err := w.Write(Item{MediaType: "movie", Title: "Perms", Year: 2020, ID: MediaID{SourceTMDB, "1"}, IMDbID: "tt1"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// A media server typically scans as a different uid than the writer.
	if info.Mode().Perm()&0o044 == 0 {
		t.Errorf("mode = %v, want group/other readable", info.Mode().Perm())
	}
}

func TestWriteValidatesInput(t *testing.T) {
	w, _ := newTestWriter(t)

	bad := []Item{
		{MediaType: "movie", Title: "No ID"},
		{MediaType: "movie", Title: "No lookup key", ID: MediaID{SourceTMDB, "1"}},
		{MediaType: "movie", Title: "Bad lookup key", ID: MediaID{SourceTMDB, "1"}, IMDbID: "12345"},
		{MediaType: "audiobook", Title: "Wrong type", ID: MediaID{SourceTMDB, "1"}, IMDbID: "tt1"},
		{MediaType: "series", Title: "No episode", ID: MediaID{SourceTVDB, "1"}, IMDbID: "tt1", Season: 1},
		{MediaType: "movie", Title: "", ID: MediaID{SourceTMDB, "1"}, IMDbID: "tt1"},
		// Movies must be TMDB and series TVDB; a swapped authority is a
		// mis-filed item waiting to happen.
		{MediaType: "movie", Title: "Wrong authority", ID: MediaID{SourceTVDB, "1"}, IMDbID: "tt1"},
		{MediaType: "series", Title: "Wrong authority", ID: MediaID{SourceTMDB, "1"}, IMDbID: "tt1", Season: 1, Episode: 1},
	}
	for _, item := range bad {
		if _, err := w.Write(item); err == nil {
			t.Errorf("Write(%+v) unexpectedly succeeded", item)
		}
	}
}

func TestWriteRequiresLibraryRoot(t *testing.T) {
	w := NewWriter("", "http://127.0.0.1:8080/api/v1/plugins/3", NewSigner("seed"))
	if _, err := w.Write(Item{MediaType: "movie", Title: "X", ID: MediaID{SourceTMDB, "1"}, IMDbID: "tt1"}); err == nil {
		t.Error("Write() succeeded with no library root configured")
	}
}

func TestWriteMovieWithoutYear(t *testing.T) {
	w, root := newTestWriter(t)
	path, err := w.Write(Item{MediaType: "movie", Title: "Untitled", ID: MediaID{SourceTMDB, "42"}, IMDbID: "tt1"})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	want := filepath.Join(root, "Movies", "Untitled [tmdb-42]", "Untitled.strm")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// parseResolverURL parses a placeholder's contents back into the request it
// addresses, so tests can assert a written placeholder is self-consistent.
func parseResolverURL(t *testing.T, raw string) (*url.URL, ResolveRequest) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("placeholder is not a URL: %v", err)
	}
	req, err := ParseResolvePath(u.Path)
	if err != nil {
		t.Fatalf("placeholder path does not parse: %v", err)
	}
	req.Quality = u.Query().Get("quality")
	req.IMDbID = u.Query().Get("imdb")
	return u, req
}
