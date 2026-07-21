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
	ID             MediaID    `json:"id"`
	IMDbID         string     `json:"imdb_id,omitempty"`
	Season         int        `json:"season,omitempty"`
	Episode        int        `json:"episode,omitempty"`
	Quality        string     `json:"quality,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	LastResolvedAt *time.Time `json:"last_resolved_at"`
	LastError      string     `json:"last_error,omitempty"`
	Plays          int        `json:"plays"`

	// seq orders placeholders by when Wisp wrote them. The autoscan marker is a
	// sequence number rather than a timestamp because two placeholders written
	// in the same millisecond must still be distinguishable — otherwise a poll
	// landing between them silently drops one.
	seq uint64
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
	seq   uint64
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
		existing.ID = p.ID
		existing.IMDbID = p.IMDbID
		existing.Season, existing.Episode = p.Season, p.Episode
		existing.Quality = p.Quality
		return
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	l.seq++
	p.seq = l.seq
	copyOf := p
	l.items[p.Path] = &copyOf
}

// MarkResolved records a successful play for a media key.
func (l *Library) MarkResolved(id MediaID, season, episode int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	for _, p := range l.items {
		if p.ID == id && p.Season == season && p.Episode == episode {
			p.LastResolvedAt = &now
			p.LastError = ""
			p.Plays++
		}
	}
}

// MarkFailed records a failed resolution for a media key.
func (l *Library) MarkFailed(id MediaID, season, episode int, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, p := range l.items {
		if p.ID == id && p.Season == season && p.Episode == episode {
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

// Cursor returns the current sequence position.
//
// Used to answer a first poll: the autoscan contract says an empty marker means
// "start from now", so a source that has just been configured must report where
// it is rather than replaying every placeholder ever written.
func (l *Library) Cursor() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.seq
}

// Since returns placeholders written after a sequence position, oldest first,
// along with the new position.
//
// Ordering matters: the host stores the returned marker verbatim, so reporting
// paths out of order would let a crash between polls lose the ones in between.
func (l *Library) Since(marker uint64) ([]Placeholder, uint64) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	var out []Placeholder
	for _, p := range l.items {
		if p.seq > marker {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })

	next := marker
	if len(out) > 0 {
		next = out[len(out)-1].seq
	}
	return out, next
}
