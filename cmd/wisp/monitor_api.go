package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/monitor"
	"github.com/dreulavelle/wisp/internal/store"
)

// Pin implements monitor.Fulfiller: resolve+pin one target. A non-Pinned outcome
// means no stream is available yet (the monitor keeps trying, and uses the outcome
// to tell a transient miss from a resolution that may never appear); a returned
// error is a real fault (auth/rate-limit/store) worth surfacing.
func (a *app) Pin(ctx context.Context, t monitor.Target) (monitor.PinOutcome, error) {
	if a.lazyResolution {
		wantQuality := qualityLabel("", "", library.NormalizeQuality(t.Quality))
		searchID := t.IMDbID
		if searchID == "" && t.TMDbID != "" {
			searchID = "tmdb:" + t.TMDbID
		}
		ids := library.IDs{IMDb: searchID, TMDb: t.TMDbID, TVDb: t.TVDbID}
		if ids.TVDb == "" && ids.TMDb == "" && strings.HasPrefix(searchID, "tt") {
			tvdb, tmdb := metadata.ProviderIDs(ctx, t.MediaType, searchID)
			ids.TVDb, ids.TMDb = tvdb, tmdb
		}
		root := t.Category
		if root == "" {
			root = a.inheritCategory(ctx, searchID, t.MediaType)
		}

		ext := ".mkv"
		var vpath string
		if t.MediaType == "movie" {
			vpath = library.MoviePath(root, t.Title, t.Year, ids, wantQuality, ext)
		} else {
			vpath = library.EpisodePath(root, t.Title, t.Year, t.Season, t.Episode, ids, wantQuality, ext)
		}

		// If the placeholder already exists, let the background scheduler try to resolve it eagerly.
		existing, err := a.store.ByPath(ctx, vpath)
		if err == nil && existing != nil {
			resolvedPath, _, err := a.pin(ctx, pinSpec{
				MediaType: t.MediaType, IMDbID: searchID, TMDbID: t.TMDbID, TVDbID: t.TVDbID,
				Title: t.Title, Year: t.Year, Season: t.Season, Episode: t.Episode, Quality: t.Quality,
				Category: t.Category,
			})
			if err == nil {
				a.retirePlaceholder(ctx, t.MediaType, existing.VirtualPath, resolvedPath)
				return monitor.Pinned, nil
			}
			outcome, reason := pinOutcome(err)
			if reason == "" {
				return outcome, err
			}
			return outcome, nil
		}

		pin := store.Pin{
			MediaType: t.MediaType, IMDbID: searchID, TMDbID: ids.TMDb, TVDbID: ids.TVDb,
			Category: root, Season: t.Season, Episode: t.Episode,
			Title: t.Title, Year: t.Year, Quality: wantQuality, VirtualPath: vpath,
			SourceURL:  "",
			Size:       1,
			ResolvedAt: time.Now(),
		}

		if err := a.store.Upsert(ctx, pin); err != nil {
			return monitor.NoResults, err
		}

		a.webhook.Import(ctx, t.MediaType, vpath)
		a.broadcastPinCompleted(t.MediaType, searchID, ids.TMDb, ids.TVDb, vpath)
		a.log.Info("created placeholder pin", "path", vpath, "imdb", searchID)
		return monitor.Pinned, nil
	}

	_, _, err := a.pin(ctx, pinSpec{
		MediaType: t.MediaType, IMDbID: t.IMDbID, TMDbID: t.TMDbID, TVDbID: t.TVDbID,
		Title: t.Title, Year: t.Year, Season: t.Season, Episode: t.Episode, Quality: t.Quality,
		Category: t.Category,
	})
	if err == nil {
		return monitor.Pinned, nil
	}
	outcome, reason := pinOutcome(err)
	if reason == "" {
		return outcome, err // genuine fault (outcome ignored while err != nil)
	}
	// Surface WHY nothing pinned, at INFO — otherwise a title that AIOStreams
	// can't resolve (e.g. a debrid returning no playable links) just sits in
	// "retrying" with no explanation, which looks like wisp doing nothing.
	a.log.Info("no stream to pin yet", "reason", reason, "title", t.Title,
		"media_type", t.MediaType, "imdb", t.IMDbID, "season", t.Season,
		"episode", t.Episode, "quality", t.Quality)
	return outcome, nil // nothing (at this quality) to pin yet
}

// retirePlaceholder removes the placeholder a lazy Pin created once the real
// resolution has landed at a different virtual path, and tells the media server
// to treat it as a rename rather than leaving a phantom 1-byte entry it already
// imported.
//
// pin() only supersedes an old path at the *same* quality tier (distinct tiers
// are legitimately concurrent targets and must coexist), so it cannot clean up
// after a placeholder whose label was the "1080p" default while the resolve —
// running with the monitor's empty/best-available quality — landed on another
// tier. Deleting only the exact path we looked up keeps sibling tiers intact.
//
// A no-op when pin() already superseded the path itself: Delete then reports
// nothing removed and the Rename webhook has already been sent.
func (a *app) retirePlaceholder(ctx context.Context, mediaType, placeholderPath, resolvedPath string) {
	if placeholderPath == "" || resolvedPath == "" || placeholderPath == resolvedPath {
		return
	}
	// Mirror pin()'s discipline: serialize the store mutation, notify outside the lock.
	a.pinMu.Lock()
	deleted, err := a.store.Delete(ctx, placeholderPath)
	a.pinMu.Unlock()
	if err != nil {
		a.log.Warn("remove superseded placeholder", "path", placeholderPath, "error", err)
		return
	}
	if !deleted {
		return
	}
	a.log.Info("placeholder resolved to a new path", "from", placeholderPath, "to", resolvedPath)
	a.webhook.Rename(ctx, mediaType, placeholderPath, resolvedPath)
}

// pinOutcome classifies an "unable to pin yet" error into a monitor.PinOutcome and
// a human-readable reason for logging. A genuine fault (auth/rate-limit/store)
// yields reason == "" so the caller propagates it as an error instead of a benign
// retry; its returned outcome is a placeholder never consumed while err != nil.
func pinOutcome(err error) (monitor.PinOutcome, string) {
	switch {
	case errors.Is(err, errNoResults):
		return monitor.NoResults, "AIOStreams returned no playable results (check debrid/indexer)"
	case errors.Is(err, errNoPlayable):
		return monitor.NotPlayable, "results found but none were probeable"
	case errors.Is(err, errNoQualityMatch):
		return monitor.NoQualityMatch, "no result at the requested quality"
	default:
		return monitor.NoResults, ""
	}
}

// PinnedKeys implements monitor.Fulfiller: what's already pinned for an id.
func (a *app) PinnedKeys(ctx context.Context, imdbID string) (map[monitor.PinKey]bool, error) {
	pins, err := a.store.PinsByMedia(ctx, imdbID)
	if err != nil {
		return nil, err
	}
	keys := make(map[monitor.PinKey]bool, len(pins))
	for _, p := range pins {
		if p.SourceURL == "" {
			continue // skip placeholder pins so the scheduler tries to resolve them in the background
		}
		keys[monitor.PinKey{Season: p.Season, Episode: p.Episode, Quality: library.NormalizeQuality(p.Quality)}] = true
	}
	return keys, nil
}

type monitorRequest struct {
	MediaType string   `json:"media_type"`
	IMDbID    string   `json:"imdb_id"`
	TMDbID    string   `json:"tmdb_id"`
	TVDbID    string   `json:"tvdb_id"`
	Title     string   `json:"title"`
	Year      int      `json:"year"`
	Qualities []string `json:"qualities"`
	Seasons   []int    `json:"seasons"`
}

// handleCreateMonitor registers a monitor directly (media-server-neutral, no
// request tool required) — POST /api/monitors.
func (a *app) handleCreateMonitor(w http.ResponseWriter, r *http.Request) {
	var req monitorRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.MediaType != "movie" && req.MediaType != "series" {
		http.Error(w, "media_type must be movie or series", http.StatusBadRequest)
		return
	}
	if req.IMDbID == "" && req.TMDbID == "" {
		http.Error(w, "imdb_id or tmdb_id is required", http.StatusBadRequest)
		return
	}
	if err := a.mon.Intake(r.Context(), monitor.Request{
		MediaType: req.MediaType, IMDbID: req.IMDbID, TMDbID: req.TMDbID, TVDbID: req.TVDbID,
		Title: req.Title, Year: req.Year, Qualities: normalizeQualities(req.Qualities), Seasons: req.Seasons,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"monitoring": true})
}

// handleListMonitors returns the watchlist — GET /api/monitors.
func (a *app) handleListMonitors(w http.ResponseWriter, r *http.Request) {
	items, err := a.store.ListMonitored(r.Context())
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, items)
}

// handleDeleteMonitor stops monitoring a title — DELETE /api/monitors?key=…
func (a *app) handleDeleteMonitor(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		http.Error(w, "provide ?key=", http.StatusBadRequest)
		return
	}
	if err := a.store.DeleteMonitored(r.Context(), key); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"deleted": key})
}

// handleRefreshMonitors forces an immediate full re-check — POST /api/monitors/refresh.
// Unlike a plain wake, this treats every enabled monitor as due now, so titles
// sitting in retry backoff (e.g. after a transient failure) are re-evaluated at
// once — the "operator fixed config, retry everything" path — before normal
// cadence resumes.
func (a *app) handleRefreshMonitors(w http.ResponseWriter, r *http.Request) {
	a.mon.ForceRefresh()
	writeJSON(w, map[string]any{"refreshing": true})
}

func normalizeQualities(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, q := range in {
		if n := library.NormalizeQuality(q); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
