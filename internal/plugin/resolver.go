// Package plugin implements Wisp as a Silo plugin.
//
// Wisp writes .strm placeholders into a Silo library and answers the resolver
// requests those placeholders point at. A placeholder's contents never change:
// it addresses this plugin, and the actual stream URL is resolved fresh on every
// playback. That is what makes expiring debrid links a non-problem — nothing
// durable ever holds one.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/dreulavelle/wisp/internal/aiostreams"
)

// ErrNotResolverPath signals that a request path is not a resolver request.
var ErrNotResolverPath = errors.New("plugin: not a resolver path")

// ResolveRequest is a parsed resolver URL.
type ResolveRequest struct {
	MediaType string // "movie" or "series"
	IMDbID    string
	Season    int
	Episode   int
	Quality   string // requested tier, e.g. "2160p"; empty means no preference
}

// ParseResolvePath parses the path half of a resolver URL.
//
// Shapes:
//
//	/resolve/movie/tt0133093
//	/resolve/series/tt0944947/1/9
//
// The shape is deliberately identical to the one drondeseries' AIOStreams
// plugin used, so placeholders written by either implementation stay readable
// by the other.
func ParseResolvePath(path string) (ResolveRequest, error) {
	trimmed := strings.Trim(path, "/")
	parts := strings.Split(trimmed, "/")

	// Silo mounts plugin routes under a prefix, so tolerate anything ahead of
	// the "resolve" segment rather than assuming a fixed mount depth.
	idx := -1
	for i, p := range parts {
		if p == "resolve" {
			idx = i
			break
		}
	}
	if idx < 0 || len(parts) < idx+3 {
		return ResolveRequest{}, ErrNotResolverPath
	}

	req := ResolveRequest{
		MediaType: parts[idx+1],
		IMDbID:    parts[idx+2],
	}

	switch req.MediaType {
	case "movie":
		if len(parts) > idx+3 {
			return ResolveRequest{}, fmt.Errorf("plugin: movie resolver path has trailing segments: %q", path)
		}
	case "series":
		if len(parts) != idx+5 {
			return ResolveRequest{}, fmt.Errorf("plugin: series resolver path needs season and episode: %q", path)
		}
		season, err := strconv.Atoi(parts[idx+3])
		if err != nil || season < 0 {
			return ResolveRequest{}, fmt.Errorf("plugin: invalid season %q", parts[idx+3])
		}
		episode, err := strconv.Atoi(parts[idx+4])
		if err != nil || episode < 0 {
			return ResolveRequest{}, fmt.Errorf("plugin: invalid episode %q", parts[idx+4])
		}
		req.Season, req.Episode = season, episode
	default:
		return ResolveRequest{}, fmt.Errorf("plugin: unknown media type %q", req.MediaType)
	}

	if !strings.HasPrefix(req.IMDbID, "tt") {
		return ResolveRequest{}, fmt.Errorf("plugin: %q is not an IMDb id", req.IMDbID)
	}
	return req, nil
}

// searcher is the slice of the AIOStreams client the resolver needs, narrowed
// so tests can substitute a stub without standing up an HTTP server.
type searcher interface {
	Search(ctx context.Context, mediaType, imdbID string, season, episode int) ([]aiostreams.Stream, error)
}

// Resolver answers playback-time resolution requests.
type Resolver struct {
	streams searcher
}

// NewResolver builds a resolver over an AIOStreams client.
func NewResolver(streams searcher) *Resolver {
	return &Resolver{streams: streams}
}

// ErrNoMatch means nothing playable was found for the request.
var ErrNoMatch = errors.New("plugin: no playable stream for this title")

// Resolve returns a directly playable URL for a request.
//
// Candidate ordering is AIOStreams' own: it already parses, ranks, and filters
// according to the operator's configuration, so re-sorting here would silently
// override settings made in one obvious place. Selection is therefore "first
// candidate at an acceptable quality".
func (r *Resolver) Resolve(ctx context.Context, req ResolveRequest) (aiostreams.Stream, error) {
	streams, err := r.streams.Search(ctx, req.MediaType, req.IMDbID, req.Season, req.Episode)
	if err != nil {
		return aiostreams.Stream{}, err
	}

	for _, tier := range acceptableTiers(req.Quality) {
		for _, s := range streams {
			if strings.TrimSpace(s.URL) == "" {
				continue
			}
			if tier == "" || strings.EqualFold(s.Resolution, tier) {
				return s, nil
			}
		}
	}
	return aiostreams.Stream{}, ErrNoMatch
}

// acceptableTiers expands a requested quality into an ordered fallback list.
//
// Falling back matters more than holding out for an exact tier: a user pressing
// play wants something to play. A 1080p stream now beats a 2160p stream that
// does not exist. An empty request means "anything", expressed as a single
// empty tier that matches the first playable candidate.
func acceptableTiers(quality string) []string {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "":
		return []string{""}
	case "2160p", "4k", "uhd":
		return []string{"2160p", "1080p", ""}
	case "1080p":
		return []string{"1080p", "720p", ""}
	case "720p":
		return []string{"720p", ""}
	default:
		// Unknown tier: try it verbatim, then take anything rather than
		// failing a playback over a label we do not recognize.
		return []string{quality, ""}
	}
}

// RedirectStatus is the status the resolver answers with.
//
// Must stay 302. FFmpeg caches 301 and 308 for the life of its HTTPContext, and
// every URL this resolver hands out is short-lived, so a cached redirect would
// strand playback on a dead link with no way to recover.
const RedirectStatus = http.StatusFound
