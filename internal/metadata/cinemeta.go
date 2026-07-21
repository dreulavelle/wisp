// Package metadata resolves provider ids (TVDB/TMDB) for an IMDb id so wisp can
// tag folders in a way media servers match deterministically.
package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// cinemetaBase is the public Cinemeta endpoint; overridable in tests.
var cinemetaBase = "https://v3-cinemeta.strem.io"

// providerClient is shared so lookups reuse connections and TLS sessions
// instead of standing up a fresh client — and a fresh pool — per call.
var providerClient = &http.Client{Timeout: 15 * time.Second}

// imdbPattern is the only id shape Cinemeta accepts. Validating here keeps a
// value that reached us from a host request out of the request URL, where a
// "/", "?" or "#" would silently rewrite the path or query.
var imdbPattern = regexp.MustCompile(`^tt\d+$`)

type cinemetaMeta struct {
	Meta struct {
		TVDBID    json.Number `json:"tvdb_id"`
		MovieDBID json.Number `json:"moviedb_id"`
	} `json:"meta"`
}

// ProviderIDs looks up the TVDB and TMDB ids for an IMDb id via Cinemeta.
// Missing ids come back as "".
//
// The error is returned rather than folded into empty strings so callers can
// tell "Cinemeta has no TVDB id for this title" apart from "the lookup never
// happened". Collapsing the two sends an operator hunting a metadata problem
// when the real cause was a network failure.
func ProviderIDs(ctx context.Context, mediaType, imdbID string) (tvdb, tmdb string, err error) {
	if !imdbPattern.MatchString(imdbID) {
		return "", "", fmt.Errorf("metadata: %q is not an imdb id", imdbID)
	}

	kind := "movie"
	if mediaType == "series" {
		kind = "series"
	}
	endpoint := fmt.Sprintf("%s/meta/%s/%s.json", cinemetaBase, kind, url.PathEscape(imdbID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "wisp")

	resp, err := providerClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("cinemeta: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("cinemeta: HTTP %d", resp.StatusCode)
	}

	var meta cinemetaMeta
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&meta); err != nil {
		return "", "", fmt.Errorf("cinemeta: decode: %w", err)
	}
	return numString(meta.Meta.TVDBID), numString(meta.Meta.MovieDBID), nil
}

// numString renders a JSON number id as a plain string, "" if zero/empty.
func numString(n json.Number) string {
	s := n.String()
	if s == "" || s == "0" {
		return ""
	}
	return s
}
