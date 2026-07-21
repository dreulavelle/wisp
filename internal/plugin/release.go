package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var errTMDBNoHomeRelease = fmt.Errorf("no digital or physical release date found")

// tmdbReleaseDatesResponse is a partial decode of the TMDB
// /movie/{id}/release_dates endpoint.
type tmdbReleaseDatesResponse struct {
	Results []struct {
		Country string `json:"iso_3166_1"`
		Dates   []struct {
			Date time.Time `json:"release_date"`
			Type int       `json:"type"`
		} `json:"release_dates"`
	} `json:"results"`
}

// fetchTMDBMovieRelease returns the earliest Digital (type 4) or Physical
// (type 5) release date for the given TMDB movie ID across all requested
// regions (comma-separated, e.g. "US,GB,CA"). If no home-media release exists
// for any of the regions, errTMDBNoHomeRelease is returned.
func fetchTMDBMovieRelease(ctx context.Context, tmdbID, apiKey, region string) (time.Time, error) {
	url := fmt.Sprintf("https://api.themoviedb.org/3/movie/%s/release_dates", tmdbID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return time.Time{}, err
	}
	// Support both v4 bearer tokens (long JWT) and legacy v3 API keys.
	if len(apiKey) > 40 {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		q := req.URL.Query()
		q.Set("api_key", apiKey)
		req.URL.RawQuery = q.Encode()
	}
	resp, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("TMDB release dates returned HTTP %d", resp.StatusCode)
	}
	var releases tmdbReleaseDatesResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&releases); err != nil {
		return time.Time{}, fmt.Errorf("decode TMDB release dates: %w", err)
	}

	// region may be a comma-separated list like "US,GB,CA"; return the
	// earliest home-media release found across any of the listed regions.
	regions := strings.Split(strings.ToUpper(strings.TrimSpace(region)), ",")
	var earliest time.Time
	for _, r := range regions {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if t, ok := selectTMDBRelease(releases, r); ok {
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
		}
	}
	if !earliest.IsZero() {
		return earliest, nil
	}
	return time.Time{}, errTMDBNoHomeRelease
}

// selectTMDBRelease returns the earliest Digital (type 4) or Physical (type 5)
// release date for a single region code. An empty region matches all results.
func selectTMDBRelease(releases tmdbReleaseDatesResponse, region string) (time.Time, bool) {
	results := releases.Results
	if region != "" {
		var regional []struct {
			Country string `json:"iso_3166_1"`
			Dates   []struct {
				Date time.Time `json:"release_date"`
				Type int       `json:"type"`
			} `json:"release_dates"`
		}
		for _, result := range results {
			if strings.EqualFold(result.Country, region) {
				regional = append(regional, result)
			}
		}
		if len(regional) > 0 {
			results = regional
		}
	}
	// Only home-media releases imply that a stream may legitimately exist.
	// TMDB types: 1=Premiere 2=Theatrical(limited) 3=Theatrical 4=Digital 5=Physical 6=TV
	for _, preferredType := range []int{4, 5} {
		var earliest time.Time
		for _, result := range results {
			for _, d := range result.Dates {
				if d.Type == preferredType && !d.Date.IsZero() {
					if earliest.IsZero() || d.Date.Before(earliest) {
						earliest = d.Date
					}
				}
			}
		}
		if !earliest.IsZero() {
			return earliest, true
		}
	}
	return time.Time{}, false
}

// cinemetaMeta holds the minimal metadata Cinemeta returns for a movie.
type cinemetaMeta struct {
	Meta struct {
		Released string `json:"released"`
	} `json:"meta"`
}

// fetchCinemetaMovieMetadata returns the release date reported by Cinemeta for
// the given IMDb ID. This is used as a fallback when no TMDB API key is set.
func fetchCinemetaMovieMetadata(ctx context.Context, imdbID string) (time.Time, error) {
	url := fmt.Sprintf("https://v3-cinemeta.strem.io/meta/movie/%s.json", imdbID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("cinemeta returned HTTP %d", resp.StatusCode)
	}
	var meta cinemetaMeta
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta); err != nil {
		return time.Time{}, fmt.Errorf("decode cinemeta meta: %w", err)
	}
	if meta.Meta.Released == "" {
		return time.Time{}, fmt.Errorf("cinemeta: no released field")
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, meta.Meta.Released); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cinemeta: cannot parse released %q", meta.Meta.Released)
}
