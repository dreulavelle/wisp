package plugin

import (
	"sort"
	"sync"
	"time"
)

// Placeholder is one .strm file Wisp manages, as shown on the dashboard.
type Placeholder struct {
	Path           string     `json:"path"`
	MediaType      string     `json:"media_type"`
	IMDbID         string     `json:"imdb_id"`
	Season         int        `json:"season,omitempty"`
	Episode        int        `json:"episode,omitempty"`
	Quality        string     `json:"quality,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	LastResolvedAt *time.Time `json:"last_resolved_at"`
	LastError      string     `json:"last_error,omitempty"`
	Plays          int        `json:"plays"`
}

// Library tracks the placeholders Wisp has written.
//
// Deliberately in memory for now: the durable record of a placeholder is the
// .strm file itself, which the media server already scans. This index exists to
// answer dashboard questions ("what have we written, what has ever played")
// without making the filesystem the query interface.
type Library struct {
	mu    sync.RWMutex
	items map[string]*Placeholder
}

// NewLibrary returns an empty index.
func NewLibrary() *Library {
	return &Library{items: make(map[string]*Placeholder)}
}

// Add records a placeholder, keyed by path. Re-adding an existing path updates
// its descriptors but preserves play history.
func (l *Library) Add(p Placeholder) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if existing, ok := l.items[p.Path]; ok {
		existing.MediaType = p.MediaType
		existing.IMDbID = p.IMDbID
		existing.Season, existing.Episode = p.Season, p.Episode
		existing.Quality = p.Quality
		return
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	copyOf := p
	l.items[p.Path] = &copyOf
}

// MarkResolved records a successful play for a media key.
func (l *Library) MarkResolved(imdbID string, season, episode int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	for _, p := range l.items {
		if p.IMDbID == imdbID && p.Season == season && p.Episode == episode {
			p.LastResolvedAt = &now
			p.LastError = ""
			p.Plays++
		}
	}
}

// MarkFailed records a failed resolution for a media key.
func (l *Library) MarkFailed(imdbID string, season, episode int, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, p := range l.items {
		if p.IMDbID == imdbID && p.Season == season && p.Episode == episode {
			p.LastError = reason
		}
	}
}

// Count returns how many placeholders are tracked.
func (l *Library) Count() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.items)
}

// List returns placeholders ordered most-recently-active first, so the rows an
// operator is most likely to care about are the ones they see without scrolling.
func (l *Library) List() []Placeholder {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]Placeholder, 0, len(l.items))
	for _, p := range l.items {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].CreatedAt, out[j].CreatedAt
		if out[i].LastResolvedAt != nil {
			ti = *out[i].LastResolvedAt
		}
		if out[j].LastResolvedAt != nil {
			tj = *out[j].LastResolvedAt
		}
		return ti.After(tj)
	})
	return out
}
