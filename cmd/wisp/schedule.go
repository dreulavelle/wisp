package main

import (
	"context"
	"net/http"
	"time"

	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/store"
)

// scheduleResponse is the scheduler's current view: the fallback interval, the
// next time the loop will wake, and every tracked item.
type scheduleResponse struct {
	IntervalSeconds int            `json:"interval_seconds"`
	NextWake        int64          `json:"next_wake"`
	Items           []scheduleItem `json:"items"`
}

// scheduleItem is one monitored title's scheduler entry. All fields come from
// the persisted monitor record plus a cheap pin lookup — nothing is inferred
// from the network.
type scheduleItem struct {
	Key            string   `json:"key"`
	MediaType      string   `json:"media_type"`
	Title          string   `json:"title,omitempty"`
	State          string   `json:"state"`
	Enabled        bool     `json:"enabled"`
	Completed      bool     `json:"completed"`
	NextCheck      int64    `json:"next_check"`
	NextRelease    *int64   `json:"next_release,omitempty"`
	LastChecked    *int64   `json:"last_checked,omitempty"`
	LastError      string   `json:"last_error,omitempty"`
	Qualities      []string `json:"qualities,omitempty"`
	Seasons        []int    `json:"seasons,omitempty"`
	Pinned         int      `json:"pinned"`
	PendingTargets int      `json:"pending_targets"`
}

// handleSchedule returns the scheduler's plan — GET /api/schedule.
func (a *app) handleSchedule(w http.ResponseWriter, r *http.Request) {
	view, err := a.buildSchedule(r.Context())
	if err != nil {
		http.Error(w, "schedule failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}

// buildSchedule assembles the schedule view from the monitor store and pin
// store. Items are returned in the store's stable key order.
func (a *app) buildSchedule(ctx context.Context) (scheduleResponse, error) {
	items, err := a.store.ListMonitored(ctx)
	if err != nil {
		return scheduleResponse{}, err
	}
	now := time.Now()
	interval := a.mon.Interval()
	nextWake := now.Add(interval)

	out := make([]scheduleItem, 0, len(items))
	for _, it := range items {
		pins, _ := a.store.PinsByMedia(ctx, monitorSearchID(it))
		si := scheduleItem{
			Key:            it.Key,
			MediaType:      it.MediaType,
			Title:          it.Title,
			State:          scheduleState(it, now),
			Enabled:        it.Enabled,
			Completed:      it.Completed,
			NextCheck:      it.DueAt.Unix(),
			Qualities:      it.Qualities,
			Seasons:        it.Seasons,
			LastError:      it.LastError,
			Pinned:         len(pins),
			PendingTargets: pendingTargets(it, pins),
		}
		if !it.LastChecked.IsZero() {
			ts := it.LastChecked.Unix()
			si.LastChecked = &ts
		}
		// A future due time on an active item is its next known release/airstamp.
		if it.Enabled && !it.Completed && it.DueAt.After(now) {
			ts := it.DueAt.Unix()
			si.NextRelease = &ts
			if it.DueAt.Before(nextWake) {
				nextWake = it.DueAt
			}
		} else if it.Enabled && !it.Completed {
			nextWake = now // an item is due now
		}
		out = append(out, si)
	}
	if nextWake.Before(now) {
		nextWake = now
	}
	return scheduleResponse{
		IntervalSeconds: int(interval.Seconds()),
		NextWake:        nextWake.Unix(),
		Items:           out,
	}, nil
}

// scheduleState classifies an item by its scheduling position (health is carried
// separately by LastError):
//   - paused:    monitoring disabled
//   - completed: a movie whose every requested tier is pinned (kept as history)
//   - waiting:   nothing due until a future release/airstamp
//   - pending:   due now; the next pass will try to pin it
func scheduleState(it store.Monitored, now time.Time) string {
	switch {
	case !it.Enabled:
		return "paused"
	case it.Completed:
		return "completed"
	case it.DueAt.After(now):
		return "waiting"
	default:
		return "pending"
	}
}

// pendingTargets counts how many requested quality tiers have nothing pinned
// yet (0 once a movie is complete). An unspecified quality is the "best
// available" tier, satisfied by any pin. This is a cheap, store-only signal — it
// does not enumerate unaired episodes.
func pendingTargets(it store.Monitored, pins []store.Pin) int {
	if it.Completed {
		return 0
	}
	quals := it.Qualities
	if len(quals) == 0 {
		quals = []string{""} // default: best available
	}
	present := make(map[string]bool, len(pins))
	for _, p := range pins {
		present[library.NormalizeQuality(p.Quality)] = true
	}
	pending := 0
	for _, q := range quals {
		nq := library.NormalizeQuality(q)
		if nq == "" {
			if len(pins) == 0 {
				pending++ // best-available tier: unsatisfied only if nothing pinned
			}
			continue
		}
		if !present[nq] {
			pending++
		}
	}
	return pending
}

// monitorSearchID is the id an item's pins are stored under — imdb if known,
// else "tmdb:<id>" — matching how app.pin keys them.
func monitorSearchID(it store.Monitored) string {
	if it.IMDbID != "" {
		return it.IMDbID
	}
	return "tmdb:" + it.TMDbID
}
