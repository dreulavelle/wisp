package plugin

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Writer creates .strm placeholders in a media library.
//
// A placeholder holds a URL addressing this plugin, never a stream. That is
// what makes it durable: the file's contents stay correct while the stream
// behind it expires, so nothing ever has to be rewritten to keep playing.
type Writer struct {
	root        string
	resolverURL string
	signer      *Signer
}

// NewWriter returns a writer rooted at a library directory.
//
// resolverURL is the base Silo will reach this plugin at, e.g.
// http://127.0.0.1:8080/api/v1/plugins/3 — host-local by design, because Silo
// resolves that hop itself rather than sending a client there.
func NewWriter(root, resolverURL string, signer *Signer) *Writer {
	return &Writer{
		root:        root,
		resolverURL: strings.TrimRight(strings.TrimSpace(resolverURL), "/"),
		signer:      signer,
	}
}

// Item describes something to write a placeholder for.
type Item struct {
	MediaType string // "movie" or "series"
	Title     string
	Year      int
	IMDbID    string
	Season    int
	Episode   int
	Quality   string
}

// Write creates the placeholder and returns its path.
//
// Writes are atomic: content goes to a temporary file in the destination
// directory and is then renamed into place. A library scan can run at any
// moment, and a scanner that reads a half-written .strm would record an item
// with a truncated URL and no obvious way to notice.
func (w *Writer) Write(item Item) (string, error) {
	if w.root == "" {
		return "", fmt.Errorf("placeholder: library path is not configured")
	}
	if !strings.HasPrefix(item.IMDbID, "tt") {
		return "", fmt.Errorf("placeholder: %q is not an IMDb id", item.IMDbID)
	}

	rel, err := w.relPath(item)
	if err != nil {
		return "", err
	}
	full := filepath.Join(w.root, rel)

	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("placeholder: create directory: %w", err)
	}

	content := w.target(item) + "\n"

	tmp, err := os.CreateTemp(filepath.Dir(full), ".wisp-*.tmp")
	if err != nil {
		return "", fmt.Errorf("placeholder: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return "", fmt.Errorf("placeholder: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("placeholder: close: %w", err)
	}
	// Media servers read these as any other user; 0644 matches what a scanner
	// running under a different uid expects to find.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", fmt.Errorf("placeholder: chmod: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return "", fmt.Errorf("placeholder: rename into place: %w", err)
	}
	return full, nil
}

// target builds the URL the placeholder points at.
func (w *Writer) target(item Item) string {
	req := ResolveRequest{
		MediaType: item.MediaType,
		IMDbID:    item.IMDbID,
		Season:    item.Season,
		Episode:   item.Episode,
		Quality:   item.Quality,
	}

	path := "/resolve/" + item.MediaType + "/" + url.PathEscape(item.IMDbID)
	if item.MediaType == "series" {
		path += fmt.Sprintf("/%d/%d", item.Season, item.Episode)
	}

	q := url.Values{}
	if item.Quality != "" {
		q.Set("quality", item.Quality)
	}
	if w.signer != nil {
		q.Set("t", w.signer.Sign(req))
	}

	target := w.resolverURL + path
	if encoded := q.Encode(); encoded != "" {
		target += "?" + encoded
	}
	return target
}

// relPath builds a library-conventional path for an item.
//
// The layout deliberately mirrors what Plex, Jellyfin, Emby and Silo all
// already parse — "Title (Year)" folders and "SxxEyy" episode names. Wisp does
// not need a scheme of its own here, and inventing one would mean every media
// server had to be taught about it.
func (w *Writer) relPath(item Item) (string, error) {
	title := sanitize(item.Title)
	if title == "" {
		return "", fmt.Errorf("placeholder: title is empty after sanitizing %q", item.Title)
	}

	quality := ""
	if item.Quality != "" {
		quality = " [" + sanitize(item.Quality) + "]"
	}

	switch item.MediaType {
	case "movie":
		name := title
		if item.Year > 0 {
			name = fmt.Sprintf("%s (%d)", title, item.Year)
		}
		return filepath.Join("Movies", name, name+quality+".strm"), nil

	case "series":
		if item.Season < 0 || item.Episode < 1 {
			return "", fmt.Errorf("placeholder: series needs a season and episode, got S%dE%d", item.Season, item.Episode)
		}
		show := title
		if item.Year > 0 {
			show = fmt.Sprintf("%s (%d)", title, item.Year)
		}
		episode := fmt.Sprintf("%s S%02dE%02d%s.strm", show, item.Season, item.Episode, quality)
		return filepath.Join("Shows", show, fmt.Sprintf("Season %02d", item.Season), episode), nil

	default:
		return "", fmt.Errorf("placeholder: unknown media type %q", item.MediaType)
	}
}

// sanitize makes a title safe to use as a single path segment.
//
// Titles come from metadata providers and, indirectly, from whoever requested
// the item, so they are untrusted input. Separators and traversal sequences
// must not survive: a title of "../../etc" would otherwise write outside the
// library root entirely.
func sanitize(s string) string {
	var b strings.Builder
	lastSpace := false

	for _, r := range strings.TrimSpace(s) {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|':
			// Path separators and characters no common filesystem accepts.
			continue
		case r == '\n' || r == '\r' || r == '\t' || r == '\v' || r == '\f':
			// Whitespace controls separate words, so collapse them to a space
			// rather than dropping them — deleting the newline in "line\nbreak"
			// would silently join two words into one.
			if !lastSpace && b.Len() > 0 {
				b.WriteRune(' ')
				lastSpace = true
			}
		case r < 0x20 || r == 0x7f:
			// Remaining control characters carry no meaning in a filename and
			// would make paths unreadable in logs.
			continue
		case unicode.IsSpace(r):
			if !lastSpace && b.Len() > 0 {
				b.WriteRune(' ')
				lastSpace = true
			}
		default:
			b.WriteRune(r)
			lastSpace = false
		}
	}

	out := strings.TrimSpace(b.String())
	// Leading dots hide the entry on unix and can produce "." or ".."; trailing
	// dots and spaces are silently stripped by Windows filesystems, which would
	// make paths disagree across platforms.
	out = strings.Trim(out, ". ")
	return out
}
