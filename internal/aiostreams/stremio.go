package aiostreams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// StremioClient resolves streams from any standard Stremio addon (e.g. Altmount,
// NzbDav) using the /stream/{type}/{id}.json endpoint.
//
// It satisfies the same interface as Client so the rest of Wisp needs no
// changes: just paste a plain Stremio manifest URL into the addon URL field
// and Wisp will pick this client automatically via IsStremioURL.
type StremioClient struct {
	base string // base URL with trailing slash, e.g. https://host/stremio/{hash}/
	http *http.Client
}

// NewStremio builds a StremioClient from a Stremio manifest URL.
//
// Example:
//
//	https://altmount.oneroot.media/stremio/{hash}/manifest.json
//	  → base: https://altmount.oneroot.media/stremio/{hash}/
func NewStremio(manifestURL string) *StremioClient {
	base := strings.TrimSpace(manifestURL)
	if idx := strings.LastIndex(base, "/manifest.json"); idx >= 0 {
		base = base[:idx+1]
	} else if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return &StremioClient{
		base: base,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// IsStremioURL reports whether addonURL is a plain Stremio addon manifest URL
// rather than an AIOStreams manifest URL.
//
// AIOStreams manifest URLs carry a UUID and an encrypted config blob as two
// path segments before /manifest.json:
//
//	https://aio.example.com/stremio/{uuid}/{config}/manifest.json
//
// Plain Stremio addon URLs (Altmount, NzbDav, …) have only a single identifier
// segment before /manifest.json:
//
//	https://host/stremio/{hash}/manifest.json
//
// The AIOStreams Client derives Basic-auth credentials from those two segments;
// if it cannot, the URL is treated as a plain Stremio addon.
func IsStremioURL(addonURL string) bool {
	return !New(addonURL).HasCredentials()
}

type stremioStreamItem struct {
	URL   string `json:"url"`
	Name  string `json:"name"`
	Title string `json:"title"`
}

type stremioResponse struct {
	Streams []stremioStreamItem `json:"streams"`
}

var resolutionPattern = regexp.MustCompile(`(?i)\b(2160p|1440p|1080p|720p|480p|360p)\b`)

func parseResolution(ss ...string) string {
	for _, s := range ss {
		if m := resolutionPattern.FindString(s); m != "" {
			return strings.ToLower(m)
		}
	}
	return ""
}

// Search implements the searcher interface using the Stremio /stream/ endpoint.
func (c *StremioClient) Search(ctx context.Context, mediaType, imdbID string, season, episode int) ([]Stream, error) {
	var path string
	switch mediaType {
	case "series":
		path = fmt.Sprintf("stream/series/%s:%d:%d.json", url.PathEscape(imdbID), season, episode)
	default:
		path = fmt.Sprintf("stream/movie/%s.json", url.PathEscape(imdbID))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &SearchError{Kind: KindTransient, cause: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, classifyFailure(resp)
	}

	var payload stremioResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode stremio stream response: %w", err)
	}

	streams := make([]Stream, 0, len(payload.Streams))
	for _, s := range payload.Streams {
		rawURL := strings.TrimSpace(s.URL)
		if rawURL == "" {
			continue
		}
		streams = append(streams, Stream{
			URL:        rawURL,
			Filename:   firstNonEmpty(s.Title, s.Name),
			Resolution: parseResolution(s.Name, s.Title),
		})
	}
	return streams, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}
