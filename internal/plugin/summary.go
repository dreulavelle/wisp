package plugin

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TitleSummary is one title as the dashboard shows it, rather than one file.
//
// A series arrives as a placeholder per episode per quality tier — a single
// eight-season show is 352 of them. Listing files meant a wall of near-identical
// rows that a person had to read to learn one thing: whether the show is
// working. Grouping answers that at a glance, and costs far less to send.
type TitleSummary struct {
	Title     string  `json:"title"`
	Year      int     `json:"year,omitempty"`
	MediaType string  `json:"media_type"`
	ID        MediaID `json:"id"`
	Anime     bool    `json:"anime,omitempty"`

	Files    int `json:"files"`
	Episodes int `json:"episodes,omitempty"`
	Seasons  int `json:"seasons,omitempty"`

	// State is the worst thing true of any file in the title. A show with one
	// broken episode is a show with a problem, and averaging that away is how
	// it goes unnoticed.
	State     string     `json:"state"`
	LastError string     `json:"last_error,omitempty"`
	LastPlay  *time.Time `json:"last_resolved_at"`
	Plays     int        `json:"plays"`

	// Newest orders the list. A title is as recent as its most recent file, so
	// adding one episode to an old show brings it back to the top, which is
	// where somebody who just requested it will look.
	newest time.Time
}

// Placeholder states, worst last. The dashboard colours on these.
const (
	stateIdle  = "idle"  // written, never played
	stateLive  = "live"  // has resolved at least once
	stateFault = "fault" // last resolve failed
)

// Summarize groups placeholders into one row per title.
func Summarize(items []Placeholder) []TitleSummary {
	groups := make(map[string]*TitleSummary)
	seasons := make(map[string]map[int]struct{})
	episodes := make(map[string]map[[2]int]struct{})

	for _, p := range items {
		key := p.ID.String()
		g := groups[key]
		if g == nil {
			title, year := titleFromPlaceholder(p)
			g = &TitleSummary{
				Title: title, Year: year,
				MediaType: p.MediaType, ID: p.ID, Anime: p.Anime,
				State: stateIdle,
			}
			groups[key] = g
			seasons[key] = map[int]struct{}{}
			episodes[key] = map[[2]int]struct{}{}
		}

		g.Files++
		g.Plays += p.Plays

		if p.MediaType == "series" {
			seasons[key][p.Season] = struct{}{}
			episodes[key][[2]int{p.Season, p.Episode}] = struct{}{}
		}

		// Worst state wins: a fault anywhere in a title is the thing worth
		// showing, and the error text with it — "failed" alone tells an
		// operator nothing they can act on.
		switch {
		case p.LastError != "":
			g.State = stateFault
			if g.LastError == "" {
				g.LastError = p.LastError
			}
		case p.LastResolvedAt != nil && g.State != stateFault:
			g.State = stateLive
		}

		if p.LastResolvedAt != nil && (g.LastPlay == nil || p.LastResolvedAt.After(*g.LastPlay)) {
			at := *p.LastResolvedAt
			g.LastPlay = &at
		}
		if p.CreatedAt.After(g.newest) {
			g.newest = p.CreatedAt
		}
	}

	out := make([]TitleSummary, 0, len(groups))
	for key, g := range groups {
		g.Seasons = len(seasons[key])
		g.Episodes = len(episodes[key])
		out = append(out, *g)
	}

	sort.Slice(out, func(i, j int) bool {
		// Anything broken first: it is the only row that needs acting on.
		if (out[i].State == stateFault) != (out[j].State == stateFault) {
			return out[i].State == stateFault
		}
		if !out[i].newest.Equal(out[j].newest) {
			return out[i].newest.After(out[j].newest)
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// titleFromPlaceholder recovers a display title from a placeholder's path.
//
// The title lives in the folder name because that is what a media server reads;
// nothing else on the placeholder carries it. Layouts differ by media type:
//
//	movies/Title (Year) [tmdb-N]/Title (Year) [1080p].strm
//	tv/Title (Year) [tvdb-N]/Season 01/Title (Year) S01E01 [1080p].strm
func titleFromPlaceholder(p Placeholder) (string, int) {
	parts := strings.Split(filepath.ToSlash(p.Path), "/")

	// The title folder is two up for a movie, three for an episode.
	idx := len(parts) - 2
	if p.MediaType == "series" {
		idx = len(parts) - 3
	}
	if idx < 0 || idx >= len(parts) {
		return p.ID.String(), 0
	}
	return splitTitleYear(parts[idx], p.ID)
}

// splitTitleYear strips the id tag and year from a library folder name.
func splitTitleYear(folder string, id MediaID) (string, int) {
	if i := strings.LastIndex(folder, " ["); i > 0 {
		folder = folder[:i]
	}
	year := 0
	if i := strings.LastIndex(folder, " ("); i > 0 && strings.HasSuffix(folder, ")") {
		if n, err := parseYear(folder[i+2 : len(folder)-1]); err == nil {
			year = n
			folder = folder[:i]
		}
	}
	folder = strings.TrimSpace(folder)
	if folder == "" {
		return id.String(), year
	}
	return folder, year
}
