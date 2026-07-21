package plugin

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// RetargetPlaceholders rewrites every placeholder under root whose target URL
// is not the one w would produce for it today.
//
// A placeholder's contents were designed never to change, and for the stream
// half of that promise it holds: the file outlives every expiring debrid
// link. The address half turned out not to be durable. Silo mints a NEW
// installation id on every plugin upgrade, and the id is baked into every
// placeholder's URL — so the first auto-update after install strands the
// whole library: files address /api/v1/plugins/8/ while the plugin now
// answers at /api/v1/plugins/9/, and every press of play 404s.
//
// Run at configure, this brings the address current. The identity tuple —
// and with it the folder layout, the filename, and the media server's watch
// state — is untouched, and because the resolver token signs the tuple
// rather than the address, an up-to-date file is byte-identical to what the
// writer would produce and is left alone. Files signed under a retired key
// come out re-signed with the current one, which is the same heal the
// AcceptAlso machinery otherwise only papers over at verify time.
//
// Failures are counted and logged per file rather than aborting the walk: one
// unreadable placeholder must not leave the other three hundred stranded.
func RetargetPlaceholders(root string, w *Writer, log *slog.Logger) (rewritten, failed int, err error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if root == "" {
		return 0, 0, fmt.Errorf("retarget: library path is not configured")
	}
	// Stat the root rather than letting the walk report it as one failed file:
	// a missing library path is a configuration problem, not a bad placeholder,
	// and the two deserve different answers.
	if info, err := os.Stat(root); err != nil {
		return 0, 0, fmt.Errorf("retarget: library path %q: %w", root, err)
	} else if !info.IsDir() {
		return 0, 0, fmt.Errorf("retarget: library path %q is not a directory", root)
	}

	visit := func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A vanished subdirectory is not a reason to strand the rest.
			log.Warn("retarget: skipping an unreadable path", "path", path, "error", walkErr)
			failed++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), Extension) {
			return nil
		}
		// A symlinked placeholder is somebody's deliberate layout, and an
		// atomic rename would quietly replace the link with a regular file.
		if d.Type()&fs.ModeSymlink != 0 {
			log.Warn("retarget: skipping a symlinked placeholder", "path", path)
			return nil
		}
		changed, err := w.retarget(path)
		if err != nil {
			failed++
			log.Warn("retarget: skipping a placeholder", "path", path, "error", err)
			return nil
		}
		if changed {
			rewritten++
		}
		return nil
	}

	// Walk only the roots Wisp writes into. Everything below the library path
	// is somebody else's until proven otherwise, and a migration that rewrites
	// files should reach exactly as far as the writer that created them.
	for _, rel := range LibraryRoots() {
		sub := filepath.Join(root, filepath.FromSlash(rel))
		if _, statErr := os.Stat(sub); statErr != nil {
			continue // not created yet; EnsureRoots handles that
		}
		if walkErr := filepath.WalkDir(sub, visit); walkErr != nil {
			return rewritten, failed, walkErr
		}
	}
	return rewritten, failed, nil
}

// retarget rewrites one placeholder in place when its target is stale.
//
// Only the target line changes. Comments and any other content the file
// carries are written back untouched, because the read side supports them and
// an unattended migration has no business deleting what it did not author.
//
// The comparison is on the target line alone, so a file that is already
// current is left untouched — mtime included, which matters because a scanner
// treats a changed mtime as a changed file.
func (w *Writer) retarget(path string) (bool, error) {
	ph, err := ParsePlaceholder(path)
	if err != nil {
		return false, err
	}

	imdb := ph.IMDbID
	if imdb == "" && ph.ID.Source == SourceIMDb {
		// A bare-IMDb placeholder carries its lookup key in the path rather
		// than the query, and the resolver reads it from there. The rewritten
		// token must sign the same key the resolver will verify against.
		imdb = ph.ID.Value
	}

	desired := w.target(Item{
		MediaType: ph.MediaType,
		ID:        ph.ID,
		IMDbID:    imdb,
		Season:    ph.Season,
		Episode:   ph.Episode,
		Quality:   ph.Quality,
	})

	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	current, at, err := splitPlaceholder(string(raw), path)
	if err != nil {
		return false, err
	}
	if current == desired {
		return false, nil
	}

	lines := strings.Split(string(raw), "\n")
	lines[at] = desired
	if err := writeAtomic(path, strings.Join(lines, "\n")); err != nil {
		return false, err
	}
	return true, nil
}
