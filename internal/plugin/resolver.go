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
	"sync"
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

	// Fresh demands a full resolution, bypassing the short reuse window and any
	// remembered probe failures. Set by a caller that has just watched the
	// previously-issued URL fail — reusing the answer that broke would hand the
	// failure straight back. Not part of a placeholder's signed tuple.
	Fresh bool
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

// freshSearcher is the optional ability to search past the client's own result
// cache. A Fresh resolution exists because a previously-issued URL just failed;
// handing it the cached result set that produced that URL would defeat the
// point, so when the searcher can bypass its cache, Fresh does.
type freshSearcher interface {
	SearchFresh(ctx context.Context, mediaType, imdbID string, season, episode int) ([]aiostreams.Stream, error)
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

	// now is the clock for the reuse windows, injectable so tests can move
	// time instead of sleeping through it.
	now func() time.Time

	// mu guards flights and probeFails.
	mu sync.Mutex

	// flights coalesces and briefly reuses resolutions per resolveKey. One
	// entry is one resolution: callers arriving while it runs share its
	// outcome, and callers arriving within resolvedTTL of it settling reuse
	// the answer outright.
	flights map[string]*flight

	// probeFails remembers candidates that recently failed a liveness check,
	// keyed by candidate URL.
	probeFails map[string]probeFailure
}

// flight is one resolution, shared by every caller who wants the same answer.
type flight struct {
	done      chan struct{} // closed once the outcome is set
	fresh     bool          // resolved past every cache; joinable by Fresh callers
	stream    aiostreams.Stream
	trace     Trace
	err       error
	expiresAt time.Time // set only on success; failures are never reused
}

// probeFailure is remembered evidence that a candidate was not serving.
type probeFailure struct {
	reason    string
	expiresAt time.Time
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
		streams:    streams,
		log:        log,
		probe:      &http.Client{Timeout: liveCheckTimeout},
		now:        time.Now,
		flights:    make(map[string]*flight),
		probeFails: make(map[string]probeFailure),
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
//
// Set to 6 rather than 4. The checks run concurrently, so wall-clock cost is one
// round trip regardless of the count — the budget bounds how many providers get
// hit, not how long the viewer waits. rankBySeekability front-loads the
// debrid-cached sources, but a title can legitimately carry two or three of them
// that are expired or mid-hiccup ahead of a live one; combined with the stricter
// probe now rejecting on-demand 202s and range-ignoring 200s, a little more
// depth is what guarantees the first genuinely-seekable source is reached rather
// than starved. Six concurrent range requests is still a modest, bounded fan-out.
const liveChecks = 6

// resolvedTTL bounds how long a verified answer is reused.
//
// A playback session is not one resolution: the media server re-resolves the
// placeholder on every ffmpeg restart, which means every seek at minimum and
// every stall recovery besides. Within this window that storm is answered from
// memory, so a seek costs a redirect rather than a search and a probe pass.
// The window is kept far short of any provider's link lifetime: this is
// seek-storm absorption, not storage — nothing durable ever holds a stream
// URL, and a fresh play after a quiet minute still resolves fresh.
const resolvedTTL = 60 * time.Second

// probeFailTTL bounds how long a candidate that just failed its liveness check
// is skipped without being re-probed.
//
// The expensive probe outcome is failure: a dead host costs its full timeout,
// and it is paid on the top-ranked candidate even when a healthy one sits
// right below it. A candidate that was dead seconds ago is dead now — a 202
// means the provider is still fetching, a timeout means the host is down — so
// within this window the candidate is skipped on remembered evidence instead
// of timing out again on every resolve.
const probeFailTTL = 2 * time.Minute

// liveCheckTimeout bounds one liveness check.
//
// Deliberately short. This is a request for the first byte of a file the
// provider claims to have ready — a provider that cannot answer that quickly is
// not going to stream a film smoothly either, and the cost of being wrong is
// small: the next candidate is tried. Every second here is a second a viewer
// spends looking at a spinner, and it is paid on the highest-ranked candidate
// even when that one is fine.
const liveCheckTimeout = 3 * time.Second

// liveCheckRangeStart and liveCheckRangeEnd bound the byte range the liveness
// probe asks for.
//
// The offset is deliberately NON-ZERO. A bytes=0-0 probe only proves the server
// can return the first byte of the file; it says nothing about whether a seek
// works, because a server that ignores Range entirely still happily returns
// byte 0. A player's every seek is a range request at an arbitrary mid-file
// offset, so probing at a real offset — and requiring a 206 that honors it — is
// the only check that predicts smooth seeking. 1 MiB is past any container
// header yet small enough to sit inside almost every real media file.
const (
	liveCheckRangeStart = 1 << 20 // 1 MiB
	liveCheckRangeEnd   = liveCheckRangeStart + 1
)

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

	// Reused marks an answer served from the reuse window rather than
	// resolved. Kept out of the latency picture: a reused answer took no time
	// because it did no work, and folding its zeros into a median would let a
	// seek-happy session mask a genuinely slow provider.
	Reused bool `json:"reused,omitempty"`
}

// Resolve returns a directly playable URL for a request.
//
// Candidate ordering is very nearly AIOStreams' own: it already parses, ranks,
// and filters according to the operator's configuration, so wholesale re-sorting
// here would silently override settings made in one obvious place. Selection is
// therefore "first candidate at an acceptable quality that is SEEKABLE and
// ACTUALLY SERVING BYTES", with one narrow, order-preserving exception below.
//
// The exception is seekability. AIOStreams interleaves two kinds of source that
// look identical in its ranking but behave nothing alike at play time: a
// debrid-cached link (…/resolve/alldebrid/…) that answers a mid-file range
// request with an instant 206, and an on-demand premium host (orionoid.com)
// that answers the same request with a 202 and keeps fetching for tens of
// seconds — playable eventually, but not seekable now. Observed live, the
// on-demand hosts sat at positions 1-5 and the seekable debrid link at 6+, so a
// probe budget spent top-down never reached it. rankBySeekability lifts the
// debrid-cached sources ahead of the rest before probing, STABLY — relative
// order inside each group is untouched, so AIOStreams still breaks every tie.
//
// The liveness half rejects what will not play smoothly. A debrid provider
// answers an uncached title with "202 Accepted, Content-Length: 0" — a promise
// to fetch it, not a stream — and a non-seekable host answers a real range
// request with "200 OK" from byte 0, ignoring the offset. Both, plus 502s and
// timeouts, are handed to a player as "Invalid data found when processing
// input": a failed or unseekable playback that looks like a Wisp bug rather
// than an unavailable or on-demand release. checkLive filters them out.
func (r *Resolver) Resolve(ctx context.Context, req ResolveRequest) (aiostreams.Stream, error) {
	stream, _, err := r.ResolveTraced(ctx, req)
	return stream, err
}

// ResolveTraced is Resolve with a timing breakdown.
//
// Resolutions for the same unit are coalesced and briefly reused. A playback
// session re-resolves its placeholder on every ffmpeg restart — each seek at
// minimum — and answering that storm from memory is the difference between a
// seek that starts instantly and one that waits out a search and a probe pass.
// Reuse is bounded by resolvedTTL and bypassed by req.Fresh, so nothing
// durable ever holds a stream URL.
func (r *Resolver) ResolveTraced(ctx context.Context, req ResolveRequest) (aiostreams.Stream, Trace, error) {
	// AIOStreams accepts IMDb ids and nothing else: tmdb: and tvdb: ids return
	// zero candidates against a live instance. Without a lookup key there is
	// nothing to search for.
	if req.IMDbID == "" {
		return aiostreams.Stream{}, Trace{}, ErrNoLookupKey
	}

	key := req.resolveKey()
	r.mu.Lock()
	if f, ok := r.flights[key]; ok {
		select {
		case <-f.done:
			if !req.Fresh && f.err == nil && r.now().Before(f.expiresAt) {
				r.mu.Unlock()
				return f.stream, Trace{Reused: true}, nil
			}
			// Settled but stale, failed, or bypassed: lead a new resolution.
		default:
			if !req.Fresh || f.fresh {
				r.mu.Unlock()
				return r.await(ctx, f, false)
			}
			// A Fresh caller must not ride an ordinary flight: it exists
			// because a URL that flight's caches may be about to re-serve
			// just failed. Lead a new, fully-fresh flight instead; the
			// ordinary one still settles for its own waiters.
		}
	}
	f := &flight{done: make(chan struct{}), fresh: req.Fresh}
	r.flights[key] = f
	r.mu.Unlock()

	// The resolution runs detached from the leader's own cancellation: the
	// callers coalesced onto this flight are healthy, and one client hanging
	// up must not hand every one of them a context error. The work stays
	// bounded by its own budget, and every caller — leader included — still
	// abandons its wait on its own ctx.
	go func() {
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resolveBudget)
		defer cancel()
		stream, trace, err := r.resolveUncached(flightCtx, req)

		r.mu.Lock()
		f.stream, f.trace, f.err = stream, trace, err
		if err == nil {
			f.expiresAt = r.now().Add(resolvedTTL)
		} else if r.flights[key] == f {
			// Failures are not reused: the next caller gets a fresh attempt,
			// not a replay of this one's bad luck. Guarded, because a Fresh
			// flight may have replaced this slot mid-resolution — its entry
			// must not be torn down by the flight it superseded.
			delete(r.flights, key)
		}
		r.pruneExpiredLocked()
		close(f.done)
		r.mu.Unlock()
	}()
	return r.await(ctx, f, true)
}

// await blocks on a flight until it settles or the caller's own context ends.
//
// The leader's trace is returned as-is — that caller waited through the real
// work. A follower's answer is marked Reused instead: the work happened once,
// and recording each coalesced caller as a full resolution would count the
// same latency into the median once per rider.
func (r *Resolver) await(ctx context.Context, f *flight, leader bool) (aiostreams.Stream, Trace, error) {
	select {
	case <-f.done:
		if leader || f.err != nil {
			return f.stream, f.trace, f.err
		}
		trace := f.trace
		trace.Reused = true
		return f.stream, trace, nil
	case <-ctx.Done():
		return aiostreams.Stream{}, Trace{}, ctx.Err()
	}
}

// resolveKey identifies one resolvable unit at one requested tier — the grain
// at which an answer can be shared between callers.
func (req ResolveRequest) resolveKey() string {
	return strings.ToLower(req.MediaType) + "|" + strings.ToLower(req.ID.String()) + "|" +
		strings.ToLower(req.IMDbID) + "|" +
		strconv.Itoa(req.Season) + "|" + strconv.Itoa(req.Episode) + "|" +
		strings.ToLower(strings.TrimSpace(req.Quality))
}

// pruneExpiredLocked drops settled flights and probe failures whose windows
// have passed, keeping both maps bounded by what was touched within a TTL.
// The caller holds r.mu.
func (r *Resolver) pruneExpiredLocked() {
	now := r.now()
	for k, f := range r.flights {
		select {
		case <-f.done:
			if !now.Before(f.expiresAt) {
				delete(r.flights, k)
			}
		default:
		}
	}
	for u, pf := range r.probeFails {
		if !now.Before(pf.expiresAt) {
			delete(r.probeFails, u)
		}
	}
}

// resolveUncached performs one full resolution: search, then verify.
func (r *Resolver) resolveUncached(ctx context.Context, req ResolveRequest) (aiostreams.Stream, Trace, error) {
	var trace Trace

	searchStart := time.Now()
	search := r.streams.Search
	if fs, ok := r.streams.(freshSearcher); ok && req.Fresh {
		search = fs.SearchFresh
	}
	streams, err := search(ctx, req.MediaType, req.IMDbID, req.Season, req.Episode)
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
		}
		if len(candidates) == 0 {
			continue
		}
		// Choose which candidates to probe. Ranking must only ADD seekable
		// sources to the probe set, never evict an on-demand candidate the old
		// top-down budget would have reached — see probeSet. The AIOStreams top
		// pick at this tier is the fallback when nothing verifies, matching the
		// pre-ranking behaviour.
		fallback := candidates[0]
		probe := probeSet(candidates)
		if picked, ok := r.firstLive(ctx, probe, fallback, req.Fresh); ok {
			trace.VerifyMS = time.Since(verifyStart).Milliseconds()
			return picked, trace, nil
		}
	}
	return aiostreams.Stream{}, trace, ErrNoMatch
}

// seekableHostPatterns marks candidate URLs as debrid-cached — a link a debrid
// service has already fetched to its own storage and serves with genuine range
// support, i.e. instantly seekable.
//
// The pattern that matters is the resolver path a torrentio-style addon emits
// for an already-cached debrid link: /resolve/<provider>/. That shape answers a
// mid-file range request with a real 206 the instant it is asked. On-demand
// premium hosts (e.g. orionoid.com/stream/…) carry no such marker and stay at
// the back of the ranking, where the liveness probe correctly rejects the 202s
// they return until they finish fetching.
//
// Kept as a small, explicit list rather than a single hardcoded provider so a
// new debrid backend is one line, and matched case-insensitively on a substring
// because the surrounding path varies by addon.
var seekableHostPatterns = []string{
	"/resolve/alldebrid/",
	"/resolve/realdebrid/",
	"/resolve/premiumize/",
	"/resolve/debridlink/",
	"/resolve/torbox/",
	"/resolve/offcloud/",
	"/resolve/easydebrid/",
}

// isSeekableHost reports whether a candidate URL points at a debrid-cached
// source per seekableHostPatterns.
func isSeekableHost(rawURL string) bool {
	lower := strings.ToLower(rawURL)
	for _, p := range seekableHostPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// rankBySeekability stably reorders candidates so debrid-cached sources come
// first and everything else follows.
//
// This is a STABLE partition, not a sort: relative order inside each group is
// preserved, so AIOStreams' own ranking still decides every tie. The only thing
// it changes is rescuing a seekable source that AIOStreams interleaved behind a
// run of on-demand hosts from being starved of a liveness probe — the exact
// failure observed live, where the debrid link sat at position 6 behind five
// on-demand ones and the probe budget never reached it.
func rankBySeekability(candidates []aiostreams.Stream) []aiostreams.Stream {
	preferred := make([]aiostreams.Stream, 0, len(candidates))
	rest := make([]aiostreams.Stream, 0, len(candidates))
	for _, s := range candidates {
		if isSeekableHost(s.URL) {
			preferred = append(preferred, s)
		} else {
			rest = append(rest, s)
		}
	}
	return append(preferred, rest...)
}

// probeSet chooses which candidates to liveness-check for a tier, ADDITIVELY:
// ranking may only widen the probe set, never shrink it below what the old
// top-down budget reached.
//
// The set is the UNION of two things, deduped and kept stable:
//
//  1. the first liveChecks candidates in AIOStreams' ORIGINAL order — every
//     candidate the pre-ranking budget would have probed, so a live on-demand
//     source can never be evicted by dead debrid links ranked ahead of it; and
//  2. every seekable (debrid-cached) candidate wherever AIOStreams placed it —
//     so a genuinely seekable link stranded past the budget is still reached,
//     which was the whole point of ranking.
//
// The union is then ordered seekable-first (a stable partition) so firstLive
// consumes debrid sources ahead of on-demand ones: the debrid preference still
// holds whenever those sources are live, but consuming a dead debrid link no
// longer costs a live on-demand one its probe. The fan-out is bounded by
// roughly liveChecks + (#seekable) — still a modest, concurrent, one-round-trip
// check.
func probeSet(candidates []aiostreams.Stream) []aiostreams.Stream {
	budget := liveChecks
	if budget > len(candidates) {
		budget = len(candidates)
	}
	// A single stable pass includes each index at most once, so the union is
	// deduped without a second structure: an index qualifies if it is within
	// the original budget OR points at a seekable host.
	selected := make([]aiostreams.Stream, 0, len(candidates))
	for i, s := range candidates {
		if i < budget || isSeekableHost(s.URL) {
			selected = append(selected, s)
		}
	}
	return rankBySeekability(selected)
}

// firstLive returns the earliest candidate that is actually serving.
//
// The checks run concurrently but the RESULT is taken in rank order, so
// AIOStreams' ordering is preserved exactly — this only skips what is broken,
// it never promotes anything. Sequential checking cost the sum of every dead
// candidate's timeout, which measured at nearly eight seconds on a title whose
// top-ranked link was unreachable; in parallel it is one round trip.
//
// When nothing verifies, the supplied fallback is returned anyway rather than
// failing: an unverified stream may still play, and a definite failure is worse
// than a probable success. The fallback is AIOStreams' own top pick at the tier
// — not candidates[0], which is the seekable-first ranking's head — so a run of
// dead debrid links never turns the unverified guess into something worse than
// the pre-ranking behaviour would have handed back.
//
// A candidate that failed a check within probeFailTTL is skipped on that
// remembered evidence rather than re-probed — unless fresh demands the
// re-probe. The expensive outcome is failure, and it recurs: a dead top-ranked
// candidate would otherwise charge its full timeout to every resolve.
func (r *Resolver) firstLive(ctx context.Context, candidates []aiostreams.Stream, fallback aiostreams.Stream, fresh bool) (aiostreams.Stream, bool) {
	checkCtx, cancel := context.WithCancel(ctx)
	// Cancelling on the way out abandons checks still in flight: once the
	// answer is known, the rest are wasted work against someone's provider.
	defer cancel()

	results := make([]chan error, len(candidates))
	remembered := make([]bool, len(candidates))
	for i := range candidates {
		results[i] = make(chan error, 1)
		if !fresh {
			if reason, ok := r.recentProbeFailure(candidates[i].URL); ok {
				remembered[i] = true
				results[i] <- fmt.Errorf("%s (remembered)", reason)
				continue
			}
		}
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
			// A success can only come from a check that actually ran, and it
			// supersedes any remembered failure: a provider that has finished
			// fetching must not stay blacklisted for the rest of the window.
			r.clearProbeFailure(candidates[i].URL)
			return candidates[i], true
		}
		// Only a check that actually ran is evidence. A remembered failure
		// must not refresh its own expiry — that would keep a candidate
		// blacklisted forever on one observation — and a cancelled context
		// says nothing about the candidate at all.
		if !remembered[i] && ctx.Err() == nil {
			r.rememberProbeFailure(candidates[i].URL, err)
		}
		r.log.Info("resolve: skipping a candidate that is not serving",
			"resolution", candidates[i].Resolution,
			"filename", candidates[i].Filename, "reason", err)
	}

	r.log.Warn("resolve: no candidate verified as serving; returning the best one unchecked",
		"checked", len(candidates))
	return fallback, true
}

// recentProbeFailure reports whether url failed a liveness check within
// probeFailTTL, and with what reason.
func (r *Resolver) recentProbeFailure(url string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pf, ok := r.probeFails[url]
	if !ok || !r.now().Before(pf.expiresAt) {
		return "", false
	}
	return pf.reason, true
}

// rememberProbeFailure records that url just failed a liveness check.
func (r *Resolver) rememberProbeFailure(url string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probeFails[url] = probeFailure{reason: err.Error(), expiresAt: r.now().Add(probeFailTTL)}
}

// clearProbeFailure forgets a remembered failure after url verified as serving.
func (r *Resolver) clearProbeFailure(url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.probeFails, url)
}

// liveCheck runs the configured liveness check.
func (r *Resolver) liveCheck(ctx context.Context, rawURL string) error {
	if r.live != nil {
		return r.live(ctx, rawURL)
	}
	return r.checkLive(ctx, rawURL)
}

// checkLive reports whether a candidate is actually serving SEEKABLE media
// bytes.
//
// A range request rather than a HEAD: providers in this ecosystem answer HEAD
// inconsistently, and asking for bytes is the same thing a player does, so a
// success here means what it appears to mean. Crucially the range starts at a
// non-zero offset (liveCheckRangeStart): the probe is not "can you return a
// byte" but "can you return a byte from the MIDDLE of the file", which is what
// every seek asks. The probe client follows redirects (default policy), so the
// status examined here is the FINAL response after any provider→CDN hops.
func (r *Resolver) checkLive(ctx context.Context, rawURL string) error {
	checkCtx, cancel := context.WithTimeout(ctx, liveCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", liveCheckRangeStart, liveCheckRangeEnd))
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
	case http.StatusPartialContent:
		// 206: the server honored a mid-file range and returned exactly the
		// slice asked for — the definition of seekable. A well-behaved 206
		// carries a Content-Range; if one is present it must be a byte range,
		// and a malformed value is treated as a non-answer rather than trusted.
		// Absence is tolerated: some providers omit it yet still stream a real
		// partial body, and rejecting those would trade a false positive for a
		// false negative.
		if cr := resp.Header.Get("Content-Range"); cr != "" &&
			!strings.HasPrefix(strings.ToLower(strings.TrimSpace(cr)), "bytes ") {
			return fmt.Errorf("malformed Content-Range %q", cr)
		}
		return nil
	case http.StatusRequestedRangeNotSatisfiable:
		// 416: the file may be smaller than our 1 MiB probe offset — a genuinely
		// small release whose server UNDERSTOOD range semantics (it rejected an
		// out-of-bounds range instead of ignoring Range and streaming from byte
		// 0). That is seekable. But a 416 can equally come from an expired or
		// error redirect target rejecting the range for unrelated reasons, and
		// this is the one place the probe is looser than the old bytes=0-0 check
		// (which rejected 416 outright). So accept it ONLY with a spec-compliant
		// unsatisfied-range Content-Range ("bytes */<complete-length>", per
		// RFC 7233 §4.2) — proof the server actually reasoned about the range —
		// and reject a bare or malformed 416 as a false seekable signal.
		cr := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Range")))
		if rest, ok := strings.CutPrefix(cr, "bytes */"); ok {
			if size, err := strconv.ParseUint(rest, 10, 64); err == nil && size > 0 {
				return nil
			}
		}
		return fmt.Errorf("416 without a satisfiable-range Content-Range %q", resp.Header.Get("Content-Range"))
	case http.StatusOK:
		// 200: the server ignored Range and is streaming the whole file from
		// byte 0. A player seeking mid-file gets the wrong bytes — this is
		// exactly the non-seekable source the harder probe exists to reject.
		return fmt.Errorf("range ignored (HTTP 200, not seekable)")
	case http.StatusAccepted:
		// The provider is still fetching it. Truthful, and useless right now.
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
