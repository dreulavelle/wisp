package plugin

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// maxPlaceholderSize bounds how much of a file we will read while rebuilding.
// A placeholder is one URL; anything larger is not one.
const maxPlaceholderSize = 64 << 10

// ParsePlaceholder reads a .strm file back into the item it describes.
//
// This is what makes the in-memory index disposable: the placeholder files are
// the durable record, so the index can always be reconstructed from disk rather
// than needing storage of its own that could drift out of agreement with them.
func ParsePlaceholder(path string) (Placeholder, error) {
	if !strings.EqualFold(filepath.Ext(path), Extension) {
		return Placeholder{}, fmt.Errorf("rebuild: %s is not a placeholder", path)
	}

	info, err := os.Stat(path)
	if err != nil {
		return Placeholder{}, err
	}
	if info.Size() > maxPlaceholderSize {
		return Placeholder{}, fmt.Errorf("rebuild: %s is too large to be a placeholder", path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Placeholder{}, err
	}

	target := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			target = line
			break
		}
	}
	if target == "" {
		return Placeholder{}, fmt.Errorf("rebuild: %s has no target", path)
	}

	u, err := url.Parse(target)
	if err != nil {
		return Placeholder{}, fmt.Errorf("rebuild: %s: %w", path, err)
	}

	req, err := ParseResolvePath(u.Path)
	if err != nil {
		return Placeholder{}, fmt.Errorf("rebuild: %s: %w", path, err)
	}

	q := u.Query()
	return Placeholder{
		Path:      path,
		MediaType: req.MediaType,
		ID:        req.ID,
		IMDbID:    strings.TrimSpace(q.Get("imdb")),
		Season:    req.Season,
		Episode:   req.Episode,
		Quality:   strings.TrimSpace(q.Get("quality")),
		Anime:     isAnimePath(path),
		CreatedAt: info.ModTime(),
	}, nil
}

// isAnimePath reports whether a placeholder sits under an anime root.
//
// The path is the storage for this: a placeholder's category was decided when
// it was written, and reading it back rather than re-deriving it is what keeps
// a later metadata correction from relocating an item already in someone's
// library.
func isAnimePath(path string) bool {
	slashed := filepath.ToSlash(path)
	for _, root := range []string{rootAnimeMovies, rootAnimeShows} {
		if strings.Contains(slashed, "/"+root+"/") {
			return true
		}
	}
	return false
}

// Rebuild repopulates the index by walking a library root.
//
// Called after configuration so a restarted plugin knows what it has already
// written. Unreadable or foreign files are skipped rather than failing the
// walk: a library is a shared directory, and one bad file must not stop the
// plugin from knowing about the rest.
//
// Returns how many placeholders were adopted and how many were skipped.
func (l *Library) Rebuild(root string) (adopted, skipped int, err error) {
	if strings.TrimSpace(root) == "" {
		return 0, 0, fmt.Errorf("rebuild: library path is not configured")
	}
	if _, err := os.Stat(root); err != nil {
		return 0, 0, fmt.Errorf("rebuild: library path %q: %w", root, err)
	}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory we cannot read is not a reason to abandon the rest.
			skipped++
			return nil //nolint:nilerr // deliberate: keep walking
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), Extension) {
			return nil
		}

		p, parseErr := ParsePlaceholder(path)
		if parseErr != nil {
			skipped++
			return nil
		}
		l.Add(p)
		adopted++
		return nil
	})

	return adopted, skipped, walkErr
}
