package plugin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
)

// Router builds the HTTP surface Silo mounts for this plugin.
type Router struct {
	resolver *Resolver
	log      *slog.Logger
	started  time.Time
}

// NewRouter returns the plugin's HTTP handler.
func NewRouter(resolver *Resolver, log *slog.Logger) *Router {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Router{resolver: resolver, log: log, started: time.Now()}
}

// Handler returns the mux serving every plugin route.
func (rt *Router) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", rt.handleHealth)
	mux.HandleFunc("/resolve/", rt.handleResolve)
	return mux
}

func (rt *Router) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"uptime_seconds": int(time.Since(rt.started).Seconds()),
		"configured":     rt.resolver != nil,
	})
}

// handleResolve answers a placeholder with a redirect to a live stream.
//
// This is the latency-critical path: a user has already pressed play and is
// watching a spinner until this returns. Everything here is deliberately a
// single hop with no retries — a fast failure the client can retry beats a slow
// success it has already given up waiting for.
func (rt *Router) handleResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if rt.resolver == nil {
		writeError(w, http.StatusServiceUnavailable, "wisp is not configured yet")
		return
	}

	req, err := ParseResolvePath(r.URL.Path)
	if err != nil {
		rt.log.Warn("resolve: bad path", "path", r.URL.Path, "error", err)
		writeError(w, http.StatusBadRequest, "invalid resolver path")
		return
	}
	req.Quality = strings.TrimSpace(r.URL.Query().Get("quality"))

	start := time.Now()
	stream, err := rt.resolver.Resolve(r.Context(), req)
	elapsed := time.Since(start)

	if err != nil {
		rt.writeResolveError(w, req, err, elapsed)
		return
	}

	rt.log.Info("resolve: redirecting",
		"media_type", req.MediaType, "imdb_id", req.IMDbID,
		"season", req.Season, "episode", req.Episode,
		"requested_quality", req.Quality, "served_quality", stream.Resolution,
		"elapsed_ms", elapsed.Milliseconds())

	// Never cacheable: the target is short-lived, and a cached redirect would
	// strand a later playback on a dead link with no way to recover.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("X-Wisp-Resolved-Quality", stream.Resolution)
	http.Redirect(w, r, stream.URL, RedirectStatus)
}

// writeResolveError maps a resolution failure onto a status a media server can
// act on. The distinction that matters is retryable versus not: a title with no
// release yet should not look like a broken plugin.
func (rt *Router) writeResolveError(w http.ResponseWriter, req ResolveRequest, err error, elapsed time.Duration) {
	log := rt.log.With(
		"media_type", req.MediaType, "imdb_id", req.IMDbID,
		"season", req.Season, "episode", req.Episode,
		"elapsed_ms", elapsed.Milliseconds())

	switch {
	case errors.Is(err, ErrNoMatch):
		// Nothing playable right now. Normal for unreleased or thinly-seeded
		// titles, so this is info rather than an error.
		log.Info("resolve: no playable stream")
		writeError(w, http.StatusServiceUnavailable, "no playable stream for this title yet")

	case isTransient(err):
		log.Warn("resolve: upstream unavailable", "error", err)
		writeError(w, http.StatusBadGateway, "stream provider is temporarily unavailable")

	default:
		log.Error("resolve: failed", "error", err)
		writeError(w, http.StatusBadGateway, "resolution failed")
	}
}

// isTransient reports whether an error is worth retrying.
func isTransient(err error) bool {
	var searchErr *aiostreams.SearchError
	if errors.As(err, &searchErr) {
		return searchErr.Kind == aiostreams.KindTransient
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits a stable error shape. Upstream error text is deliberately
// not echoed: it can carry credentials from resolver URLs.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"status":  status,
		"message": message,
	}})
}
