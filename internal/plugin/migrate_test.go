package plugin

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLibraryPlaceholder writes a placeholder through the real writer under
// a temporary library root and returns its path.
func writeLibraryPlaceholder(t *testing.T, root string, base string, signer *Signer, item Item) string {
	t.Helper()
	for _, rel := range LibraryRoots() {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path, err := NewWriter(root, base, signer).Write(item)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	return path
}

func readTarget(t *testing.T, path string) string {
	t.Helper()
	target, err := readPlaceholderTarget(path)
	if err != nil {
		t.Fatalf("readPlaceholderTarget(%s) error = %v", path, err)
	}
	return target
}

// Silo mints a new installation id on every plugin upgrade, so every
// placeholder on disk addresses a base that no longer routes. Retargeting must
// move the address and nothing else: same file, same identity, same folder —
// or the media server sees a vanished title and watch state goes with it.
func TestRetargetRewritesAStaleResolverBase(t *testing.T) {
	root := t.TempDir()
	signer := NewSignerFromSecret("secret-1")
	item := Item{
		MediaType: "movie", Title: "The Matrix", Year: 1999,
		ID: MediaID{SourceTMDB, "603"}, IMDbID: "tt0133093", Quality: "1080p",
	}
	path := writeLibraryPlaceholder(t, root, "http://127.0.0.1:8080/api/v1/plugins/8", signer, item)

	next := NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/9", signer)
	rewritten, failed, err := RetargetPlaceholders(root, next, nil)
	if err != nil {
		t.Fatalf("RetargetPlaceholders() error = %v", err)
	}
	if rewritten != 1 || failed != 0 {
		t.Fatalf("rewritten=%d failed=%d, want 1/0", rewritten, failed)
	}

	target := readTarget(t, path)
	if !strings.HasPrefix(target, "http://127.0.0.1:8080/api/v1/plugins/9/resolve/") {
		t.Errorf("target = %q, want the new resolver base", target)
	}

	// The identity survived byte-for-byte: same tuple, and a token the same
	// signer still verifies.
	ph, err := ParsePlaceholder(path)
	if err != nil {
		t.Fatalf("ParsePlaceholder() error = %v", err)
	}
	if ph.ID != item.ID || ph.IMDbID != item.IMDbID || ph.Quality != item.Quality {
		t.Errorf("identity changed: %+v", ph)
	}
	u, _ := url.Parse(target)
	req, err := ParseResolvePath(u.Path)
	if err != nil {
		t.Fatalf("ParseResolvePath() error = %v", err)
	}
	req.IMDbID = u.Query().Get("imdb")
	req.Quality = u.Query().Get("quality")
	if !signer.Verify(req, u.Query().Get("t")) {
		t.Error("rewritten token does not verify")
	}
}

// An up-to-date file must be left alone — mtime included, because a scanner
// treats a changed mtime as a changed file and re-ingests it.
func TestRetargetLeavesCurrentFilesUntouched(t *testing.T) {
	root := t.TempDir()
	signer := NewSignerFromSecret("secret-1")
	base := "http://127.0.0.1:8080/api/v1/plugins/9"
	path := writeLibraryPlaceholder(t, root, base, signer, Item{
		MediaType: "movie", Title: "Riddick", Year: 2013,
		ID: MediaID{SourceTMDB, "87421"}, IMDbID: "tt1411250", Quality: "1080p",
	})
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	rewritten, failed, err := RetargetPlaceholders(root, NewWriter(root, base, signer), nil)
	if err != nil || rewritten != 0 || failed != 0 {
		t.Fatalf("rewritten=%d failed=%d err=%v, want 0/0/nil", rewritten, failed, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("an up-to-date placeholder was rewritten")
	}
}

// A file signed under a retired key comes out re-signed with the current one:
// the durable heal, where AcceptAlso only papers over verification.
func TestRetargetResignsRetiredKeyTokens(t *testing.T) {
	root := t.TempDir()
	oldSigner := NewSignerFromSecret("old-secret")
	base := "http://127.0.0.1:8080/api/v1/plugins/9"
	item := Item{
		MediaType: "series", Title: "Game of Thrones", Year: 2011,
		ID: MediaID{SourceTVDB, "121361"}, IMDbID: "tt0944947",
		Season: 1, Episode: 9, Quality: "1080p",
	}
	path := writeLibraryPlaceholder(t, root, base, oldSigner, item)

	current := NewSignerFromSecret("new-secret").AcceptAlso(oldSigner)
	rewritten, failed, err := RetargetPlaceholders(root, NewWriter(root, base, current), nil)
	if err != nil || rewritten != 1 || failed != 0 {
		t.Fatalf("rewritten=%d failed=%d err=%v, want 1/0/nil", rewritten, failed, err)
	}

	u, _ := url.Parse(readTarget(t, path))
	req, err := ParseResolvePath(u.Path)
	if err != nil {
		t.Fatal(err)
	}
	req.IMDbID = u.Query().Get("imdb")
	req.Quality = u.Query().Get("quality")
	if !NewSignerFromSecret("new-secret").Verify(req, u.Query().Get("t")) {
		t.Error("token was not re-signed with the current key")
	}
}

// A hand-written placeholder can carry its IMDb id in the path with no imdb
// query parameter. The resolver reads the key from the path, so the rewritten
// token must sign that same key — not an empty one.
func TestRetargetPreservesBareIMDbLookupKey(t *testing.T) {
	root := t.TempDir()
	signer := NewSignerFromSecret("secret-1")
	dir := filepath.Join(root, "movies", "The Matrix (1999)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "The Matrix (1999).strm")
	if err := os.WriteFile(path, []byte("http://127.0.0.1:8080/api/v1/plugins/8/resolve/movie/tt0133093\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	base := "http://127.0.0.1:8080/api/v1/plugins/9"
	rewritten, failed, err := RetargetPlaceholders(root, NewWriter(root, base, signer), nil)
	if err != nil || rewritten != 1 || failed != 0 {
		t.Fatalf("rewritten=%d failed=%d err=%v, want 1/0/nil", rewritten, failed, err)
	}

	u, _ := url.Parse(readTarget(t, path))
	if got := u.Query().Get("imdb"); got != "tt0133093" {
		t.Errorf("imdb key = %q, want the one the path carried", got)
	}
}

// The read side supports comments, so the migration must not be the thing
// that deletes them. Only the target line changes.
func TestRetargetPreservesCommentsAndLayout(t *testing.T) {
	root := t.TempDir()
	signer := NewSignerFromSecret("secret-1")
	dir := filepath.Join(root, "movies", "The Matrix (1999) [tmdb-603]")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "The Matrix (1999).strm")
	original := "# hand-written, do not delete\n# second note\n" +
		"http://127.0.0.1:8080/api/v1/plugins/8/resolve/movie/tmdb:603?imdb=tt0133093\n" +
		"# trailing note\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	rewritten, failed, err := RetargetPlaceholders(root,
		NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/9", signer), nil)
	if err != nil || rewritten != 1 || failed != 0 {
		t.Fatalf("rewritten=%d failed=%d err=%v, want 1/0/nil", rewritten, failed, err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	want := []string{"# hand-written, do not delete", "# second note", "", "# trailing note"}
	if len(got) != len(want) {
		t.Fatalf("line count = %d, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if want[i] == "" { // the target line
			if !strings.HasPrefix(got[i], "http://127.0.0.1:8080/api/v1/plugins/9/") {
				t.Errorf("line %d = %q, want the retargeted URL", i, got[i])
			}
			continue
		}
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A symlinked placeholder is somebody's deliberate layout; an atomic rename
// would quietly turn it into a regular file.
func TestRetargetSkipsSymlinkedPlaceholders(t *testing.T) {
	root := t.TempDir()
	signer := NewSignerFromSecret("secret-1")
	real := filepath.Join(t.TempDir(), "real.strm")
	if err := os.WriteFile(real, []byte("http://127.0.0.1:8080/api/v1/plugins/8/resolve/movie/tmdb:603?imdb=tt0133093\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "movies", "The Matrix (1999) [tmdb-603]")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "The Matrix (1999).strm")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rewritten, failed, err := RetargetPlaceholders(root,
		NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/9", signer), nil)
	if err != nil || rewritten != 0 || failed != 0 {
		t.Fatalf("rewritten=%d failed=%d err=%v, want 0/0/nil", rewritten, failed, err)
	}
	if info, err := os.Lstat(link); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
}

// The walk reaches exactly as far as the writer does: a .strm somebody else
// put elsewhere under the library path is not Wisp's to re-address.
func TestRetargetIgnoresFilesOutsideTheLibraryRoots(t *testing.T) {
	root := t.TempDir()
	signer := NewSignerFromSecret("secret-1")
	foreign := filepath.Join(root, "someone-elses", "thing.strm")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "http://127.0.0.1:8080/api/v1/plugins/8/resolve/movie/tmdb:603?imdb=tt0133093\n"
	if err := os.WriteFile(foreign, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	rewritten, failed, err := RetargetPlaceholders(root,
		NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/9", signer), nil)
	if err != nil || rewritten != 0 || failed != 0 {
		t.Fatalf("rewritten=%d failed=%d err=%v, want 0/0/nil", rewritten, failed, err)
	}
	raw, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != original {
		t.Errorf("a file outside the roots was rewritten: %q", raw)
	}
}

// A missing library path is a configuration problem, not a bad placeholder.
func TestRetargetErrorsOnAMissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	_, _, err := RetargetPlaceholders(missing,
		NewWriter(missing, "http://127.0.0.1:8080/api/v1/plugins/9", NewSignerFromSecret("s")), nil)
	if err == nil {
		t.Error("a missing library path was reported as success")
	}
}

// One broken file must not strand the rest of the library.
func TestRetargetSurvivesJunkFiles(t *testing.T) {
	root := t.TempDir()
	signer := NewSignerFromSecret("secret-1")
	writeLibraryPlaceholder(t, root, "http://127.0.0.1:8080/api/v1/plugins/8", signer, Item{
		MediaType: "movie", Title: "Riddick", Year: 2013,
		ID: MediaID{SourceTMDB, "87421"}, IMDbID: "tt1411250", Quality: "1080p",
	})
	if err := os.WriteFile(filepath.Join(root, "movies", "junk.strm"), []byte("not a url at all\n#\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "movies", "notes.txt"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rewritten, failed, err := RetargetPlaceholders(root,
		NewWriter(root, "http://127.0.0.1:8080/api/v1/plugins/9", signer), nil)
	if err != nil {
		t.Fatalf("RetargetPlaceholders() error = %v", err)
	}
	if rewritten != 1 || failed != 1 {
		t.Errorf("rewritten=%d failed=%d, want the good file healed and the junk counted", rewritten, failed)
	}
}
