package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/store"
)

// Request states the status API reports, designed to map onto Silo's
// request_router statuses.
const (
	statusQueued    = "queued"    // tracked, nothing (in scope) pinned yet — includes unreleased/unaired
	statusCompleted = "completed" // requested scope pinned and servable
	statusFailed    = "failed"    // permanent give-up (unresolvable identity)
)

// requestStatus is wisp's authoritative view of a title, computed purely from
// the monitor record and the pin store — no network calls.
type requestStatus struct {
	State           string   `json:"state"`
	PinnedQualities []string `json:"pinned_qualities"`
	PinnedPaths     []string `json:"pinned_paths,omitempty"`
	Detail          string   `json:"detail"`
	RequestRef      string   `json:"request_ref,omitempty"`
}

// handleRequestStatus reports a title's state — GET /api/requests/status. The
// title is identified by media_type + tmdb_id, falling back to imdb_id. A 404
// means wisp is not tracking the title (no monitor and no pins).
func (a *app) handleRequestStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mediaType := strings.TrimSpace(q.Get("media_type"))
	tmdbID := strings.TrimSpace(q.Get("tmdb_id"))
	imdbID := strings.TrimSpace(q.Get("imdb_id"))
	if tmdbID == "" && imdbID == "" {
		http.Error(w, "provide tmdb_id or imdb_id", http.StatusBadRequest)
		return
	}

	mon := a.findMonitor(r.Context(), mediaType, tmdbID, imdbID)
	var searchIDs []string
	if mon != nil {
		searchIDs = append(searchIDs, monitorSearchID(*mon))
		if mon.TMDbID != "" && tmdbID == "" {
			tmdbID = mon.TMDbID // let the monitor's tmdb id find legacy imdb-keyed pins
		}
	} else {
		if imdbID != "" {
			searchIDs = append(searchIDs, imdbID)
		}
		if tmdbID != "" {
			searchIDs = append(searchIDs, "tmdb:"+tmdbID)
		}
	}
	pins := a.servablePins(r.Context(), searchIDs, tmdbID)

	if mon == nil && len(pins) == 0 {
		http.NotFound(w, r) // not tracked — the caller should (re)submit via /api/add
		return
	}
	writeJSON(w, computeRequestStatus(mon, pins, mediaType, time.Now(), a.mon.TierExhausted))
}

// findMonitor locates the monitored record for a title by tmdb id (preferred)
// or imdb id, honoring media_type when supplied. Monitors are few, so a scan is
// cheaper than maintaining a secondary index.
func (a *app) findMonitor(ctx context.Context, mediaType, tmdbID, imdbID string) *store.Monitored {
	items, err := a.store.ListMonitored(ctx)
	if err != nil {
		return nil
	}
	for i := range items {
		it := items[i]
		if mediaType != "" && it.MediaType != mediaType {
			continue
		}
		if (tmdbID != "" && it.TMDbID == tmdbID) || (imdbID != "" && it.IMDbID == imdbID) {
			return &it
		}
	}
	return nil
}

// servablePins returns the healthy (resolvable) pins for a title, looked up both
// by search id (imdb or "tmdb:<id>" slot) and by the persisted bare TMDbID, so
// legacy/direct pins keyed by imdb are found on a tmdb-only query. Deduped by
// virtual path.
func (a *app) servablePins(ctx context.Context, searchIDs []string, tmdbID string) []store.Pin {
	var out []store.Pin
	seen := map[string]bool{}
	add := func(pins []store.Pin) {
		for _, p := range pins {
			if p.Servable() && !seen[p.VirtualPath] {
				seen[p.VirtualPath] = true
				out = append(out, p)
			}
		}
	}
	for _, id := range searchIDs {
		if pins, err := a.store.PinsByMedia(ctx, id); err == nil {
			add(pins)
		}
	}
	if pins, err := a.store.PinsByTMDbID(ctx, tmdbID); err == nil {
		add(pins)
	}
	return out
}

// tierExhaustedFunc reports whether a quality tier's retry budget is spent — see
// monitor.Monitor.TierExhausted, which is the production implementation. A nil
// func means "nothing has been given up on", so a caller with no monitor wired in
// gets the pre-existing (strict) behavior.
type tierExhaustedFunc func(store.TierBackoffState) bool

// computeRequestStatus maps a monitor + its servable pins onto the request
// state. mediaType is the query hint, used when no monitor/pin fixes it.
//
// Mapping rules (from the architecture memo):
//   - failed: permanent give-up only (Monitored.Failed) — never for an
//     unreleased/unaired title.
//   - completed: requested scope pinned and servable, DISCOUNTING tiers the
//     scheduler has exhausted its retry budget on (see abandonedTiers). Movie:
//     every requested-and-still-attainable tier has a servable pin. Series: a
//     servable pin exists AND no aired-but-unpinned episodes remain for an
//     attainable tier. Series monitors keep running for future episodes but still
//     report completed.
//   - queued: otherwise — tracked but nothing in scope pinned yet, whether
//     unreleased/unaired or in the released-but-no-stream-yet retry window.
//
// Nothing pinned is never completed, whatever the backoff state says: a title
// with every tier abandoned and no servable file is still queued, because the
// scheduler never stops retrying it.
func computeRequestStatus(mon *store.Monitored, pins []store.Pin, mediaType string, now time.Time, exhausted tierExhaustedFunc) requestStatus {
	// Only servable pins count toward completion or pinned_qualities — a pin whose
	// stream is gone is not "done". (The HTTP path pre-filters; this keeps the
	// function correct for any caller.)
	servable := pins[:0:0]
	for _, p := range pins {
		if p.Servable() {
			servable = append(servable, p)
		}
	}
	pins = servable

	mt := mediaType
	if mt == "" && mon != nil {
		mt = mon.MediaType
	}
	if mt == "" && len(pins) > 0 {
		mt = pins[0].MediaType
	}

	st := requestStatus{PinnedQualities: pinnedQualities(pins), PinnedPaths: pinnedPaths(pins)}
	if mon != nil {
		st.RequestRef = mon.RequestRef
	}
	abandoned := abandonedTiers(mon, pins, exhausted)

	switch {
	case mon != nil && mon.Failed:
		st.State = statusFailed
		st.Detail = failDetail(mon)
	case isCompleted(mt, mon, pins, abandoned):
		st.State = statusCompleted
		st.Detail = completedDetail(abandoned)
	default:
		st.State = statusQueued
		st.Detail = queuedDetail(mon, now)
	}
	return st
}

// abandonedTiers returns the sorted requested tiers the scheduler has given up
// on: in tier backoff with an exhausted retry budget, requested by this monitor,
// and not already pinned. Those three conditions together mean "wisp looked for
// this resolution across the whole title, repeatedly, and it does not exist" — so
// it should stop blocking completion, even though the scheduler keeps retrying it
// forever in the background.
func abandonedTiers(mon *store.Monitored, pins []store.Pin, exhausted tierExhaustedFunc) []string {
	if mon == nil || exhausted == nil || len(mon.TierBackoff) == 0 {
		return nil
	}
	pinned := make(map[string]bool, len(pins))
	for _, p := range pins {
		pinned[library.NormalizeQuality(p.Quality)] = true
	}
	requested := make(map[string]bool, len(mon.Qualities))
	for _, q := range mon.Qualities {
		if n := library.NormalizeQuality(q); n != "" {
			requested[n] = true
		}
	}
	var out []string
	for q, st := range mon.TierBackoff {
		if requested[q] && !pinned[q] && exhausted(st) {
			out = append(out, q)
		}
	}
	sort.Strings(out)
	return out
}

// isCompleted reports whether the requested scope is pinned and servable,
// treating the abandoned tiers as out of scope.
func isCompleted(mediaType string, mon *store.Monitored, pins []store.Pin, abandoned []string) bool {
	if len(pins) == 0 {
		return false // nothing servable is never done, however hopeless the rest is
	}
	if mediaType == "series" {
		// Need the scheduler's aired-coverage signal; without a monitor we can't
		// confirm every aired episode is present.
		return mon != nil && !mon.LastChecked.IsZero() && pendingAttainable(mon, abandoned) == 0
	}
	// Movie: every requested quality tier must have a servable pin — otherwise a
	// 1080p pin would report "completed" while a later 2160p request is still
	// unfulfilled, and the router would stop polling. A legacy direct pin (no
	// monitor) has no requested-tier list, so any servable pin is the whole scope.
	if mon == nil {
		return true
	}
	// The same reasoning as the series gate applies: a movie requesting 1080p+2160p
	// where 2160p demonstrably has no releases would otherwise report queued for
	// ever. Discount the abandoned tiers and require the rest.
	return allTiersPinned(withoutTiers(mon.Qualities, abandoned), pins)
}

// pendingAttainable is PendingAired minus the aired-but-unpinned work that
// belongs to an abandoned tier. PendingAired is a scalar over (episode × tier)
// units, so the per-tier breakdown is what makes the subtraction possible.
//
// The result is clamped at zero: PendingByTier is absent on records written
// before it existed (they simply don't discount anything until the next
// scheduler pass rewrites them) and must never push the count negative.
func pendingAttainable(mon *store.Monitored, abandoned []string) int {
	pending := mon.PendingAired
	for _, q := range abandoned {
		pending -= mon.PendingByTier[q]
	}
	if pending < 0 {
		return 0
	}
	return pending
}

// withoutTiers returns requested minus drop, preserving order.
func withoutTiers(requested, drop []string) []string {
	if len(drop) == 0 {
		return requested
	}
	skip := make(map[string]bool, len(drop))
	for _, q := range drop {
		skip[q] = true
	}
	out := make([]string, 0, len(requested))
	for _, q := range requested {
		if !skip[library.NormalizeQuality(q)] {
			out = append(out, q)
		}
	}
	return out
}

// completedDetail explains a completion, naming any tier that was given up on so
// an operator (and Silo, which passes Detail straight through) can see why the
// request closed with a resolution missing.
func completedDetail(abandoned []string) string {
	if len(abandoned) == 0 {
		return "requested scope pinned"
	}
	return "requested scope pinned; gave up on " + strings.Join(abandoned, ", ") +
		" (no releases found at that quality after repeated checks)"
}

// allTiersPinned reports whether every requested quality tier has a servable
// pin. An empty request list means "best available" — satisfied by any pin.
func allTiersPinned(requested []string, pins []store.Pin) bool {
	present := make(map[string]bool, len(pins))
	for _, p := range pins {
		present[library.NormalizeQuality(p.Quality)] = true
	}
	want := make([]string, 0, len(requested))
	for _, q := range requested {
		if n := library.NormalizeQuality(q); n != "" {
			want = append(want, n)
		}
	}
	if len(want) == 0 {
		return len(pins) > 0
	}
	for _, q := range want {
		if !present[q] {
			return false
		}
	}
	return true
}

// pinnedQualities returns the sorted, unique quality tiers among the pins.
func pinnedQualities(pins []store.Pin) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range pins {
		q := library.NormalizeQuality(p.Quality)
		if q == "" {
			q = p.Quality
		}
		if q != "" && !seen[q] {
			seen[q] = true
			out = append(out, q)
		}
	}
	sort.Strings(out)
	return out
}

// pinnedPaths returns the virtual paths of the pins.
func pinnedPaths(pins []store.Pin) []string {
	var out []string
	for _, p := range pins {
		if p.VirtualPath != "" {
			out = append(out, p.VirtualPath)
		}
	}
	return out
}

func failDetail(mon *store.Monitored) string {
	if mon.LastError != "" {
		return mon.LastError
	}
	return "permanent failure: unresolvable identity"
}

// queuedDetail explains why a queued title is not yet completed, using only
// stored state (DueReason/DueAt), so a caller can distinguish "awaiting release"
// from "resolving stream".
func queuedDetail(mon *store.Monitored, now time.Time) string {
	if mon == nil {
		return "resolving stream"
	}
	if mon.DueAt.After(now) {
		switch mon.DueReason {
		case store.DueReasonRelease:
			return "awaiting home-media release"
		case store.DueReasonAirstamp:
			return "awaiting next episode airing"
		}
	}
	return "resolving stream"
}
