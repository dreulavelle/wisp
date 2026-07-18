package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var tmdbBase = "https://api.themoviedb.org/3"
var tvmazeBase = "https://api.tvmaze.com"
var ErrNoHomeRelease = errors.New("no digital or physical release date")

type Episode struct {
	Season, Number int
	Released       time.Time
}
type metaResponse struct {
	Meta struct {
		Released time.Time `json:"released"`
		Videos   []struct {
			Season     int       `json:"season"`
			Episode    int       `json:"episode"`
			Number     int       `json:"number"`
			Released   time.Time `json:"released"`
			FirstAired time.Time `json:"firstAired"`
		} `json:"videos"`
	} `json:"meta"`
}
type tmdbResponse struct {
	Results []struct {
		Country string `json:"iso_3166_1"`
		Dates   []struct {
			Date time.Time `json:"release_date"`
			Type int       `json:"type"`
		} `json:"release_dates"`
	} `json:"results"`
}

func MovieRelease(ctx context.Context, imdbID, tmdbID, key string, markets []string) (time.Time, error) {
	if key != "" && tmdbID != "" {
		var data tmdbResponse
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, tmdbBase+"/movie/"+url.PathEscape(tmdbID)+"/release_dates", nil)
		if strings.Count(key, ".") == 2 {
			req.Header.Set("Authorization", "Bearer "+key)
		} else {
			q := req.URL.Query()
			q.Set("api_key", key)
			req.URL.RawQuery = q.Encode()
		}
		if err := getJSON(req, &data); err != nil {
			return time.Time{}, err
		}
		allowed := map[string]bool{}
		for _, market := range markets {
			allowed[strings.ToUpper(strings.TrimSpace(market))] = true
		}
		for _, typ := range []int{4, 5} {
			var first time.Time
			for _, result := range data.Results {
				if !allowed[strings.ToUpper(result.Country)] {
					continue
				}
				for _, d := range result.Dates {
					if d.Type == typ && !d.Date.IsZero() && (first.IsZero() || d.Date.Before(first)) {
						first = d.Date
					}
				}
			}
			if !first.IsZero() {
				return first, nil
			}
		}
		return time.Time{}, ErrNoHomeRelease
	}
	var data metaResponse
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cinemetaBase+"/meta/movie/"+url.PathEscape(imdbID)+".json", nil)
	if err := getJSON(req, &data); err != nil {
		return time.Time{}, err
	}
	if data.Meta.Released.IsZero() {
		return time.Time{}, fmt.Errorf("movie metadata has no release timestamp")
	}
	return data.Meta.Released, nil
}

func ReleasedEpisodes(ctx context.Context, imdbID string, through time.Time) ([]Episode, error) {
	var cm metaResponse
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cinemetaBase+"/meta/series/"+url.PathEscape(imdbID)+".json", nil)
	cmErr := getJSON(req, &cm)
	tv, tvErr := tvmazeEpisodes(ctx, imdbID)
	if cmErr != nil || len(cm.Meta.Videos) == 0 {
		if tvErr != nil {
			return nil, fmt.Errorf("cinemeta: %v; tvmaze: %w", cmErr, tvErr)
		}
		return released(tv, through), nil
	}
	canonical := make([]Episode, 0, len(cm.Meta.Videos))
	for _, v := range cm.Meta.Videos {
		n := v.Episode
		if n == 0 {
			n = v.Number
		}
		d := v.Released
		if d.IsZero() {
			d = v.FirstAired
		}
		canonical = append(canonical, Episode{v.Season, n, d})
	}
	if tvErr == nil {
		dates := map[[2]int]time.Time{}
		for _, ep := range tv {
			dates[[2]int{ep.Season, ep.Number}] = ep.Released
		}
		for i := range canonical {
			if d, ok := dates[[2]int{canonical[i].Season, canonical[i].Number}]; ok && !canonical[i].Released.IsZero() && abs(d.Sub(canonical[i].Released)) <= 48*time.Hour {
				canonical[i].Released = d
			}
		}
	}
	return released(canonical, through), nil
}

func tvmazeEpisodes(ctx context.Context, imdbID string) ([]Episode, error) {
	var show struct {
		ID int `json:"id"`
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, tvmazeBase+"/lookup/shows?imdb="+url.QueryEscape(imdbID), nil)
	if err := getJSON(req, &show); err != nil {
		return nil, err
	}
	var rows []struct {
		Season   int        `json:"season"`
		Number   int        `json:"number"`
		Airdate  string     `json:"airdate"`
		Airstamp *time.Time `json:"airstamp"`
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, tvmazeBase+"/shows/"+strconv.Itoa(show.ID)+"/episodes", nil)
	if err := getJSON(req, &rows); err != nil {
		return nil, err
	}
	out := make([]Episode, 0, len(rows))
	for _, row := range rows {
		var d time.Time
		if row.Airstamp != nil {
			d = *row.Airstamp
		} else {
			d, _ = time.Parse("2006-01-02", row.Airdate)
		}
		out = append(out, Episode{row.Season, row.Number, d})
	}
	return out, nil
}

func released(in []Episode, through time.Time) []Episode {
	seen := map[[2]int]Episode{}
	for _, ep := range in {
		if ep.Season > 0 && ep.Number > 0 && !ep.Released.IsZero() && !ep.Released.After(through) {
			seen[[2]int{ep.Season, ep.Number}] = ep
		}
	}
	out := make([]Episode, 0, len(seen))
	for _, ep := range seen {
		out = append(out, ep)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Season == out[j].Season {
			return out[i].Number < out[j].Number
		}
		return out[i].Season < out[j].Season
	})
	return out
}
func getJSON(req *http.Request, target any) error {
	req.Header.Set("User-Agent", "wisp")
	resp, err := (&http.Client{Timeout: 25 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(target)
}
func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
