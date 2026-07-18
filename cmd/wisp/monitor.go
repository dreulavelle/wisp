package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/store"
)

type monitorRequest struct {
	MediaType string   `json:"media_type"`
	IMDbID    string   `json:"imdb_id"`
	TMDbID    string   `json:"tmdb_id"`
	TVDbID    string   `json:"tvdb_id"`
	Title     string   `json:"title"`
	Year      int      `json:"year"`
	Qualities []string `json:"qualities"`
}

func monitorID(kind, imdb string) string {
	return strings.ToLower(strings.TrimSpace(kind)) + ":" + strings.ToLower(strings.TrimSpace(imdb))
}

func (a *app) handleCreateMonitor(w http.ResponseWriter, r *http.Request) {
	var req monitorRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil || (req.MediaType != "movie" && req.MediaType != "series") || req.IMDbID == "" || req.Title == "" {
		http.Error(w, "media_type, imdb_id, and title are required", http.StatusBadRequest)
		return
	}
	qualities := []string{}
	seen := map[string]bool{}
	for _, q := range req.Qualities {
		if n := library.NormalizeQuality(q); n != "" && !seen[n] {
			seen[n] = true
			qualities = append(qualities, n)
		}
	}
	if len(qualities) == 0 {
		qualities = []string{"1080p"}
	}
	m := store.Monitor{ID: monitorID(req.MediaType, req.IMDbID), MediaType: req.MediaType, IMDbID: req.IMDbID, TMDbID: req.TMDbID, TVDbID: req.TVDbID, Title: req.Title, Year: req.Year, Qualities: qualities, Enabled: true}
	if err := a.store.UpsertMonitor(r.Context(), m); err != nil {
		http.Error(w, "store failed", 500)
		return
	}
	go a.refreshMonitors(context.Background())
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, m)
}

func (a *app) handleListMonitors(w http.ResponseWriter, r *http.Request) {
	items, err := a.store.ListMonitors(r.Context())
	if err != nil {
		http.Error(w, "list failed", 500)
		return
	}
	writeJSON(w, items)
}
func (a *app) handleDeleteMonitor(w http.ResponseWriter, r *http.Request) {
	ok, err := a.store.DeleteMonitor(r.Context(), strings.TrimSpace(r.URL.Query().Get("id")))
	if err != nil {
		http.Error(w, "delete failed", 500)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, map[string]bool{"deleted": true})
}
func (a *app) handleRefreshMonitors(w http.ResponseWriter, r *http.Request) {
	a.refreshMonitors(r.Context())
	writeJSON(w, map[string]bool{"refreshed": true})
}

func (a *app) runMonitorLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	a.refreshMonitors(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.refreshMonitors(ctx)
		}
	}
}

func (a *app) refreshMonitors(ctx context.Context) {
	a.monitorMu.Lock()
	defer a.monitorMu.Unlock()
	items, err := a.store.ListMonitors(ctx)
	if err != nil {
		a.log.Error("list monitors", "error", err)
		return
	}
	pins, _ := a.store.List(ctx)
	pinned := map[string]bool{}
	for _, p := range pins {
		pinned[fmt.Sprintf("%s:%d:%d:%s", p.IMDbID, p.Season, p.Episode, library.NormalizeQuality(p.Quality))] = true
	}
	for _, m := range items {
		if !m.Enabled || m.Completed {
			continue
		}
		m.LastChecked = time.Now()
		m.LastError = ""
		failures := 0
		if m.MediaType == "movie" {
			release, e := metadata.MovieRelease(ctx, m.IMDbID, m.TMDbID, a.tmdbKey, a.tmdbMarkets)
			if e != nil || release.After(time.Now()) {
				if e != nil {
					m.LastError = e.Error()
				}
				a.store.UpsertMonitor(ctx, m)
				continue
			}
			for _, q := range m.Qualities {
				key := fmt.Sprintf("%s:0:0:%s", m.IMDbID, q)
				if !pinned[key] {
					if e = a.invokeAdd(ctx, m, 0, 0, q); e != nil {
						failures++
						m.LastError = e.Error()
					} else {
						pinned[key] = true
					}
				}
			}
			m.Completed = failures == 0
		} else {
			episodes, e := metadata.ReleasedEpisodes(ctx, m.IMDbID, time.Now())
			if e != nil {
				m.LastError = e.Error()
				a.store.UpsertMonitor(ctx, m)
				continue
			}
			for _, ep := range episodes {
				for _, q := range m.Qualities {
					key := fmt.Sprintf("%s:%d:%d:%s", m.IMDbID, ep.Season, ep.Number, q)
					if !pinned[key] {
						if e = a.invokeAdd(ctx, m, ep.Season, ep.Number, q); e != nil {
							failures++
							m.LastError = e.Error()
						} else {
							pinned[key] = true
						}
					}
				}
			}
		}
		a.store.UpsertMonitor(ctx, m)
	}
}

func (a *app) invokeAdd(ctx context.Context, m store.Monitor, season, episode int, quality string) error {
	body, _ := json.Marshal(addRequest{MediaType: m.MediaType, IMDbID: m.IMDbID, TMDbID: m.TMDbID, TVDbID: m.TVDbID, Title: m.Title, Year: m.Year, Season: season, Episode: episode, Quality: quality})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/add", bytes.NewReader(body)).WithContext(ctx)
	a.handleAdd(rec, req)
	if rec.Code < 300 {
		return nil
	}
	return fmt.Errorf("add returned HTTP %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
}
