package plugin

import (
	"testing"
	"time"
)

func ph(path, mediaType string, id MediaID, season, ep int) Placeholder {
	return Placeholder{Path: path, MediaType: mediaType, ID: id, Season: season, Episode: ep, CreatedAt: time.Now()}
}

// A single eight-season show is 352 placeholders. Listing files made a person
// read a wall of near-identical rows to learn one thing.
func TestSummarizeCollapsesASeriesToOneRow(t *testing.T) {
	id := MediaID{SourceTVDB, "73255"}
	var items []Placeholder
	for season := 1; season <= 2; season++ {
		for ep := 1; ep <= 3; ep++ {
			for _, q := range []string{"1080p", "2160p"} {
				items = append(items, ph(
					"/library/tv/House (2004) [tvdb-73255]/Season 0"+itoa(season)+"/House (2004) S0"+itoa(season)+"E0"+itoa(ep)+" ["+q+"].strm",
					"series", id, season, ep))
			}
		}
	}

	got := Summarize(items)
	if len(got) != 1 {
		t.Fatalf("got %d rows for one show, want 1", len(got))
	}
	s := got[0]
	if s.Title != "House" || s.Year != 2004 {
		t.Errorf("title = %q (%d), want House (2004)", s.Title, s.Year)
	}
	if s.Files != 12 || s.Episodes != 6 || s.Seasons != 2 {
		t.Errorf("files=%d episodes=%d seasons=%d, want 12/6/2", s.Files, s.Episodes, s.Seasons)
	}
}

// A show with one broken episode is a show with a problem. Averaging that away
// is exactly how it goes unnoticed — and the error text has to survive, because
// "failed" alone is not something an operator can act on.
func TestSummarizeSurfacesTheWorstStateAndItsReason(t *testing.T) {
	id := MediaID{SourceTVDB, "1"}
	played := time.Now().Add(-time.Hour)
	items := []Placeholder{
		{Path: "/library/tv/Show (2020) [tvdb-1]/Season 01/a.strm", MediaType: "series", ID: id, Season: 1, Episode: 1, LastResolvedAt: &played, Plays: 3},
		{Path: "/library/tv/Show (2020) [tvdb-1]/Season 01/b.strm", MediaType: "series", ID: id, Season: 1, Episode: 2, LastError: "not cached yet (HTTP 202)"},
	}

	got := Summarize(items)[0]
	if got.State != stateFault {
		t.Errorf("state = %q, want %q; one broken episode is a problem", got.State, stateFault)
	}
	if got.LastError == "" {
		t.Error("the reason was dropped; \"failed\" alone is not actionable")
	}
	if got.Plays != 3 {
		t.Errorf("plays = %d, want them summed across the title", got.Plays)
	}
}

// Movies stay one row each and keep their own identity.
func TestSummarizeKeepsMoviesSeparate(t *testing.T) {
	items := []Placeholder{
		ph("/library/movies/Riddick (2013) [tmdb-87421]/Riddick (2013) [1080p].strm", "movie", MediaID{SourceTMDB, "87421"}, 0, 0),
		ph("/library/movies/Riddick (2013) [tmdb-87421]/Riddick (2013) [2160p].strm", "movie", MediaID{SourceTMDB, "87421"}, 0, 0),
		ph("/library/movies/Pitch Black (2000) [tmdb-2787]/Pitch Black (2000) [1080p].strm", "movie", MediaID{SourceTMDB, "2787"}, 0, 0),
	}

	got := Summarize(items)
	if len(got) != 2 {
		t.Fatalf("got %d rows for two films, want 2", len(got))
	}
	for _, g := range got {
		if g.Episodes != 0 || g.Seasons != 0 {
			t.Errorf("%s reported episodes/seasons; it is a film", g.Title)
		}
	}
}

// Broken titles sort first: they are the only rows that need acting on.
func TestSummarizeSortsFaultsFirst(t *testing.T) {
	older := time.Now().Add(-24 * time.Hour)
	items := []Placeholder{
		{Path: "/library/movies/Fine (2020) [tmdb-1]/a.strm", MediaType: "movie", ID: MediaID{SourceTMDB, "1"}, CreatedAt: time.Now()},
		{Path: "/library/movies/Broken (2019) [tmdb-2]/b.strm", MediaType: "movie", ID: MediaID{SourceTMDB, "2"}, CreatedAt: older, LastError: "boom"},
	}
	if got := Summarize(items); got[0].Title != "Broken" {
		t.Errorf("first row = %q, want the broken title even though it is older", got[0].Title)
	}
}

func itoa(n int) string { return string(rune('0' + n)) }
