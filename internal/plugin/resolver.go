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
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
)

// ErrNotResolverPath signals that a request path is not a resolver request.
var ErrNotResolverPath = errors.New("plugin: not a resolver path")

// ResolveRequest is a parsed resolver URL.
type ResolveRequest struct {
	MediaType string // "movie" or "series"

	// ID is the canonical library identity: TMDB for movies, TVDB for series.
	// It is what the library is organized by and what a media server matches on.
	ID MediaID

	// IMDbID is the lookup key handed to the stream provider. It travels with
	// the request rather than being derived at play time, because deriving it
	// would put two metadata API calls in front of every playback — latency a
	// user spends staring at a spinner.
	IMDbID string

	Season  int
	Episode int
	Quality string // requested tier, e.g. "2160p"; empty means no preference
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

	id, err := ParseMediaID(parts[idx+2])
	if err != nil {
		return ResolveRequest{}, err
	}

	req := ResolveRequest{
		MediaType: parts[idx+1],
		ID:        id,
	}
	// A bare IMDb id in the path is accepted so hand-written placeholders keep
	// working, but it is a lookup key rather than an identity.
	if id.Source == SourceIMDb {
		req.IMDbID = id.Value
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
	log     *slog.Logger

	// probe performs liveness checks. Redirects are followed, because a
	// provider handing out a redirect to its CDN is the normal case.
	probe *http.Client

	// live is the liveness check, swappable so tests can drive both outcomes
	// without standing up a provider. Nil means use the real one.
	live func(ctx context.Context, rawURL string) error
}

// NewResolver builds a resolver over an AIOStreams client.
func NewResolver(streams searcher) *Resolver {
	return NewResolverWith(streams, nil)
}

// NewResolverWith builds a resolver with a logger.
func NewResolverWith(streams searcher, log *slog.Logger) *Resolver {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Resolver{
		streams: streams,
		log:     log,
		probe:   &http.Client{Timeout: liveCheckTimeout},
	}
}

// ErrNoMatch means nothing playable was found for the request.
var ErrNoMatch = errors.New("plugin: no playable stream for this title")

// ErrNoLookupKey means the request carries a canonical identity but no IMDb id
// to search the provider with. A placeholder written by Wisp always carries
// one; this indicates a hand-written or truncated placeholder.
var ErrNoLookupKey = errors.New("plugin: request has no IMDb lookup key")

// liveChecks bounds how many candidates are checked for liveness before giving
// up. Each check is a small range request, and this sits inside the wait before
// playback starts — enough to get past a run of dead links without turning a
// missing title into a long stall.
const liveChecks = 4

// liveCheckTimeout bounds one liveness check.
//
// Deliberately short. This is a request for the first byte of a file the
// provider claims to have ready — a provider that cannot answer that quickly is
// not going to stream a film smoothly either, and the cost of being wrong is
// small: the next candidate is tried. Every second here is a second a viewer
// spends looking at a spinner, and it is paid on the highest-ranked candidate
// even when that one is fine.
const liveCheckTimeout = 3 * time.Second

// Trace breaks down where a resolution spent its time.
//
// The dashboard used to show a three-segment bar whose last two segments were
// hardcoded to zero, so it was a single colour pretending to be a breakdown.
// The split that actually exists is worth seeing: time in AIOStreams' own
// search versus time spent discarding candidates that were not serving. If
// verification dominates, the provider is handing out dead links; if search
// does, the provider itself is slow. Those call for different fixes.
type Trace struct {
	SearchMS int64 `json:"search_ms"`
	VerifyMS int64 `json:"verify_ms"`
}

// Resolve returns a directly playable URL for a request.
//
// Candidate ordering is AIOStreams' own: it already parses, ranks, and filters
// according to the operator's configuration, so re-sorting here would silently
// override settings made in one obvious place. Selection is therefore "first
// candidate at an acceptable quality THAT IS ACTUALLY SERVING BYTES".
//
// That last part is not re-ranking, it is liveness. A debrid provider answers
// an uncached title with "202 Accepted, Content-Length: 0" — a promise to
// fetch it, not a stream — and also serves 502s and connection timeouts.
// Measured against one real title, five of fourteen ranked candidates were
// dead, including the top-ranked one. Handing any of those to a player gets
// "Invalid data found when processing input" and a failed playback that looks
// like a Wisp bug rather than an unavailable release.
func (r *Resolver) Resolve(ctx context.Context, req ResolveRequest) (aiostreams.Stream, error) {
	stream, _, err := r.ResolveTraced(ctx, req)
	return stream, err
}

// ResolveTraced is Resolve with a timing breakdown.
func (r *Resolver) ResolveTraced(ctx context.Context, req ResolveRequest) (aiostreams.Stream, Trace, error) {
	var trace Trace

	// AIOStreams accepts IMDb ids and nothing else: tmdb: and tvdb: ids return
	// zero candidates against a live instance. Without a lookup key there is
	// nothing to search for.
	if req.IMDbID == "" {
		return aiostreams.Stream{}, trace, ErrNoLookupKey
	}

	searchStart := time.Now()
	streams, err := r.streams.Search(ctx, req.MediaType, req.IMDbID, req.Season, req.Episode)
	trace.SearchMS = time.Since(searchStart).Milliseconds()
	if err != nil {
		return aiostreams.Stream{}, trace, err
	}

	verifyStart := time.Now()
	defer func() { trace.VerifyMS = time.Since(verifyStart).Milliseconds() }()

	for _, tier := range acceptableTiers(req.Quality) {
		var candidates []aiostreams.Stream
		for _, s := range streams {
			if !isPlayableURL(s.URL) {
				continue
			}
			if tier != "" && !strings.EqualFold(s.Resolution, tier) {
				continue
			}
			candidates = append(candidates, s)
			if len(candidates) >= liveChecks {
				break
			}
		}
		if len(candidates) == 0 {
			continue
		}
		if picked, ok := r.firstLive(ctx, candidates); ok {
			trace.VerifyMS = time.Since(verifyStart).Milliseconds()
			return picked, trace, nil
		}
	}
	return aiostreams.Stream{}, trace, ErrNoMatch
}

// firstLive returns the earliest candidate that is actually serving.
//
// The checks run concurrently but the RESULT is taken in rank order, so
// AIOStreams' ordering is preserved exactly — this only skips what is broken,
// it never promotes anything. Sequential checking cost the sum of every dead
// candidate's timeout, which measured at nearly eight seconds on a title whose
// top-ranked link was unreachable; in parallel it is one round trip.
//
// When nothing verifies, the best candidate is returned anyway rather than
// failing: an unverified stream may still play, and a definite failure is worse
// than a probable success.
func (r *Resolver) firstLive(ctx context.Context, candidates []aiostreams.Stream) (aiostreams.Stream, bool) {
	checkCtx, cancel := context.WithCancel(ctx)
	// Cancelling on the way out abandons checks still in flight: once the
	// answer is known, the rest are wasted work against someone's provider.
	defer cancel()

	results := make([]chan error, len(candidates))
	for i := range candidates {
		results[i] = make(chan error, 1)
		go func(i int) {
			results[i] <- r.liveCheck(checkCtx, candidates[i].URL)
		}(i)
	}

	// Consumed in rank order, not completion order. A faster lower-ranked
	// candidate must not overtake a slower higher-ranked one, or liveness
	// quietly becomes re-ranking by latency.
	for i := range candidates {
		err := <-results[i]
		if err == nil {
			return candidates[i], true
		}
		r.log.Info("resolve: skipping a candidate that is not serving",
			"resolution", candidates[i].Resolution,
			"filename", candidates[i].Filename, "reason", err)
	}

	r.log.Warn("resolve: no candidate verified as serving; returning the best one unchecked",
		"checked", len(candidates))
	return candidates[0], true
}

// liveCheck runs the configured liveness check.
func (r *Resolver) liveCheck(ctx context.Context, rawURL string) error {
	if r.live != nil {
		return r.live(ctx, rawURL)
	}
	return r.checkLive(ctx, rawURL)
}

// checkLive reports whether a candidate is actually serving media bytes.
//
// A range request rather than a HEAD: providers in this ecosystem answer HEAD
// inconsistently, and asking for the first bytes is the same thing a player
// does first, so a success here means what it appears to mean.
func (r *Resolver) checkLive(ctx context.Context, rawURL string) error {
	checkCtx, cancel := context.WithTimeout(ctx, liveCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Range", "bytes=0-0")
	// Some providers reject the default Go agent outright.
	req.Header.Set("User-Agent", liveCheckUserAgent)

	resp, err := r.probe.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		return nil
	case http.StatusAccepted:
		// The provider is fetching it. Truthful, and useless right now.
		return fmt.Errorf("not cached yet (HTTP 202)")
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
}

// liveCheckUserAgent identifies liveness checks to providers.
const liveCheckUserAgent = "wisp/1.0"

// isPlayableURL reports whether a candidate is safe to redirect a client to.
//
// The resolver route is public and this URL goes straight into a Location
// header, so a compromised or misconfigured upstream would otherwise turn it
// into an open redirect to file://, javascript:, or an internal address.
// Restricting to http/https costs nothing — every real debrid or usenet link
// is one of the two.
func isPlayableURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
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
