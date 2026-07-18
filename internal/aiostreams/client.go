// Package aiostreams talks to an AIOStreams instance's REST API to select
// playable streams and resolve anime ID mappings. It reuses the exact
// auth-derivation and Search API contract validated in silo-plugin-aiostreams.
package aiostreams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const userAgent = "wisp"

// ErrorKind classifies why a Search call failed so callers can distinguish a
// genuine no-stream condition from a configuration or throttling problem.
type ErrorKind int

const (
	// KindUpstream is an unexpected/unclassified upstream status.
	KindUpstream ErrorKind = iota
	// KindAuth is 401/403: missing or wrong credentials.
	KindAuth
	// KindRateLimited is 429: throttled; RetryAfter may be set.
	KindRateLimited
	// KindTransient is a 5xx or a transport failure; retry later.
	KindTransient
)

// SearchError is a classified failure from the AIOStreams Search API. It carries
// no credentials or resolver URLs, so it is safe to log and return to callers.
type SearchError struct {
	Kind       ErrorKind
	Status     int           // upstream HTTP status; 0 for transport failures
	RetryAfter time.Duration // parsed from Retry-After on 429, else 0
	cause      error         // transport error, if any (no credentials/URLs)
}

func (e *SearchError) Error() string {
	switch e.Kind {
	case KindAuth:
		return fmt.Sprintf("aiostreams authentication failed (HTTP %d)", e.Status)
	case KindRateLimited:
		return fmt.Sprintf("aiostreams rate limited (HTTP %d)", e.Status)
	case KindTransient:
		if e.Status == 0 {
			return fmt.Sprintf("aiostreams unreachable: %v", e.cause)
		}
		return fmt.Sprintf("aiostreams temporarily unavailable (HTTP %d)", e.Status)
	default:
		return fmt.Sprintf("aiostreams search returned HTTP %d", e.Status)
	}
}

func (e *SearchError) Unwrap() error { return e.cause }

// parseRetryAfter reads a Retry-After header in either delta-seconds or
// HTTP-date form, returning 0 when absent or unparseable.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// Client is a thin AIOStreams REST client.
type Client struct {
	addonURL   string
	basicCreds string // "uuid:password"
	http       *http.Client
}

// Stream is one playable result from the Search API. Resolution and Filename
// come straight from AIOStreams' own release parsing — wisp does not re-parse.
type Stream struct {
	URL        string
	Filename   string
	Resolution string // e.g. "2160p", "1080p" (from AIOStreams parsedFile)
}

// New builds a client from an AIOStreams manifest URL and password.
func New(addonURL, password string) *Client {
	return &Client{
		addonURL:   strings.TrimSpace(addonURL),
		basicCreds: deriveCredentials(addonURL, password),
		http:       &http.Client{Timeout: 60 * time.Second},
	}
}

// deriveCredentials mirrors the plugin: a "uuid:password" secret is used
// verbatim; otherwise the UUID/alias is recovered from the /stremio/{id}/...
// path and paired with the password.
func deriveCredentials(addonURL, password string) string {
	password = strings.TrimSpace(password)
	if strings.Contains(password, ":") {
		return password
	}
	parsed, err := url.Parse(strings.TrimSpace(addonURL))
	if err != nil {
		return ""
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i, segment := range segments {
		if segment == "stremio" && i+1 < len(segments) {
			id := segments[i+1]
			if id == "u" && i+2 < len(segments) { // alias form: /stremio/u/{alias}
				id = segments[i+2]
			}
			if password == "" {
				return id
			}
			return id + ":" + password
		}
	}
	return ""
}

type searchResult struct {
	URL         string `json:"url"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Filename    string `json:"filename"`
	ParsedFile  struct {
		Resolution string `json:"resolution"`
	} `json:"parsedFile"`
}

type searchResponse struct {
	Success bool `json:"success"`
	Data    *struct {
		Results []searchResult `json:"results"`
	} `json:"data"`
}

// Search returns playable streams (those with a URL) for a movie or episode,
// ordered by AIOStreams' own ranking. mediaType is "movie" or "series".
func (c *Client) Search(ctx context.Context, mediaType, imdbID string, season, episode int) ([]Stream, error) {
	origin, err := c.origin()
	if err != nil {
		return nil, err
	}
	id := imdbID
	if mediaType == "series" {
		id = fmt.Sprintf("%s:%d:%d", imdbID, season, episode)
	}
	q := url.Values{}
	q.Set("type", mediaType)
	q.Set("id", id)
	q.Set("requiredFields", "url")
	endpoint := origin + "/api/v1/search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	c.applyAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &SearchError{Kind: KindTransient, cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, classifyFailure(resp)
	}
	var payload searchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}
	if !payload.Success || payload.Data == nil {
		return nil, fmt.Errorf("search response unsuccessful")
	}
	streams := make([]Stream, 0, len(payload.Data.Results))
	for _, r := range payload.Data.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		streams = append(streams, Stream{URL: r.URL, Filename: filenameFromResult(r), Resolution: r.ParsedFile.Resolution})
	}
	return streams, nil
}

// classifyFailure maps a non-200 Search response to a typed SearchError. It
// reads AIOStreams' structured error envelope because AIOStreams reports bad
// credentials as HTTP 400 with error.code "USER_INVALID_DETAILS" (not 401), so
// status alone would misclassify a permanent auth failure as transient.
func classifyFailure(resp *http.Response) *SearchError {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&body)
	switch strings.ToUpper(strings.TrimSpace(body.Error.Code)) {
	case "USER_INVALID_DETAILS", "UNAUTHORIZED", "FORBIDDEN":
		return &SearchError{Kind: KindAuth, Status: resp.StatusCode}
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &SearchError{Kind: KindAuth, Status: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &SearchError{Kind: KindRateLimited, Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	case resp.StatusCode >= 500:
		return &SearchError{Kind: KindTransient, Status: resp.StatusCode}
	default:
		return &SearchError{Kind: KindUpstream, Status: resp.StatusCode}
	}
}

// HasCredentials reports whether a usable "uuid:password" auth pair was derived.
// A uuid-only value (no password) cannot authenticate the Search API, so this
// lets the process warn at startup instead of failing every add with a 401.
func (c *Client) HasCredentials() bool {
	return strings.Contains(c.basicCreds, ":")
}

func (c *Client) applyAuth(req *http.Request) {
	if parts := strings.SplitN(c.basicCreds, ":", 2); len(parts) == 2 {
		req.SetBasicAuth(parts[0], parts[1])
	}
}

func (c *Client) origin() (string, error) {
	parsed, err := url.Parse(c.addonURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid AIOStreams URL")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// filenameFromResult uses AIOStreams' parsed filename when present, else
// recovers it from the resolver URL's last path segment.
func filenameFromResult(r searchResult) string {
	if strings.TrimSpace(r.Filename) != "" {
		return strings.TrimSpace(r.Filename)
	}
	if parsed, err := url.Parse(r.URL); err == nil {
		segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		for i := len(segments) - 1; i >= 0; i-- {
			if decoded, err := url.PathUnescape(segments[i]); err == nil {
				if strings.Contains(decoded, ".") {
					return decoded
				}
			}
		}
	}
	return strings.TrimSpace(r.Name)
}
