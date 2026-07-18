package seerr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client talks to the Seerr API to complete a request the webhook underspecifies
// (seasons, 4K intent, title/year). It authenticates with an API key.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New builds a Seerr API client. baseURL/apiKey may be empty (enrichment then
// no-ops, and the webhook's own fields are used as-is).
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:  strings.TrimSpace(apiKey),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Configured reports whether the client can reach Seerr.
func (c *Client) Configured() bool { return c.baseURL != "" && c.apiKey != "" }

// Enrich fills gaps in an intake from the Seerr API — the authoritative source
// for 4K intent and requested seasons (the webhook underspecifies both). It
// returns an error when the request lookup fails so the caller can surface that
// the request is proceeding on guessed data (standard/all-seasons). Filling
// title/year is non-critical (the webhook subject is a fallback) and never errors
// the call. A no-op (nil) when the client isn't configured or there's no id.
func (c *Client) Enrich(ctx context.Context, in *Intake) error {
	if in == nil || !c.Configured() || in.RequestID <= 0 {
		return nil
	}
	r, err := c.request(ctx, in.RequestID)
	if err != nil {
		return err
	}
	in.Is4K = r.Is4K // API is authoritative for 4K intent
	if in.TMDbID == "" {
		in.TMDbID = numToStr(r.Media.TMDbID)
	}
	if in.TVDbID == "" {
		in.TVDbID = numToStr(r.Media.TVDbID)
	}
	if in.IMDbID == "" {
		in.IMDbID = strings.TrimSpace(r.Media.IMDbID)
	}
	if len(in.Seasons) == 0 {
		for _, s := range r.Seasons {
			if s.SeasonNumber > 0 {
				in.Seasons = append(in.Seasons, s.SeasonNumber)
			}
		}
	}
	if in.Title == "" || in.Year == 0 {
		if title, year, err := c.mediaDetails(ctx, in.MediaType, in.TMDbID); err == nil {
			if in.Title == "" {
				in.Title = title
			}
			if in.Year == 0 {
				in.Year = year
			}
		}
	}
	return nil
}

type seerrRequest struct {
	Is4K  bool `json:"is4k"`
	Media struct {
		TMDbID json.Number `json:"tmdbId"`
		TVDbID json.Number `json:"tvdbId"`
		IMDbID string      `json:"imdbId"`
	} `json:"media"`
	Seasons []struct {
		SeasonNumber int `json:"seasonNumber"`
	} `json:"seasons"`
}

func (c *Client) request(ctx context.Context, id int) (seerrRequest, error) {
	var r seerrRequest
	err := c.getJSON(ctx, c.baseURL+"/api/v1/request/"+strconv.Itoa(id), &r)
	return r, err
}

// mediaDetails fetches a title and year from Seerr's media endpoint.
func (c *Client) mediaDetails(ctx context.Context, mediaType, tmdbID string) (title string, year int, err error) {
	kind := "movie"
	if mediaType == "series" {
		kind = "tv"
	}
	var d struct {
		Title        string `json:"title"`        // movie
		Name         string `json:"name"`         // tv
		ReleaseDate  string `json:"releaseDate"`  // movie
		FirstAirDate string `json:"firstAirDate"` // tv
	}
	if err := c.getJSON(ctx, c.baseURL+"/api/v1/"+kind+"/"+tmdbID, &d); err != nil {
		return "", 0, err
	}
	title = d.Title
	if title == "" {
		title = d.Name
	}
	for _, date := range []string{d.ReleaseDate, d.FirstAirDate} {
		if len(date) >= 4 {
			if y, e := strconv.Atoi(date[:4]); e == nil {
				year = y
				break
			}
		}
	}
	return title, year, nil
}

// getJSON fetches with a small retry for transient failures (transport errors
// and 5xx), so a momentary Seerr blip doesn't drop a request onto guessed data.
func (c *Client) getJSON(ctx context.Context, endpoint string, out any) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(300 * time.Millisecond):
			}
		}
		retryable, err := c.getJSONOnce(ctx, endpoint, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
	}
	return lastErr
}

func (c *Client) getJSONOnce(ctx context.Context, endpoint string, out any) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("User-Agent", "wisp")
	resp, err := c.http.Do(req)
	if err != nil {
		return true, err // transport blip — retry
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return true, fmt.Errorf("seerr GET returned HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("seerr GET returned HTTP %d", resp.StatusCode)
	}
	return false, json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func numToStr(n json.Number) string {
	s := strings.TrimSpace(n.String())
	if s == "" || s == "0" {
		return ""
	}
	return s
}
