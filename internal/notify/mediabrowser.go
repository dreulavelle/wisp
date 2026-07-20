package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// mediaBrowserConfig configures a Jellyfin/Emby-style target. Jellyfin and Emby
// share the "MediaBrowser" heritage and both accept POST {base}{prefix}/Library/
// Media/Updated with an X-Emby-Token header; they differ only in the route prefix
// (Emby routes under /emby) and the update type used for new files.
type mediaBrowserConfig struct {
	flavor     string // "jellyfin" | "emby" — for log lines
	baseURL    string
	apiKey     string
	pathPrefix string // "" for Jellyfin, "/emby" for Emby
	createType string // updateType for a new file: "Modified" (Jellyfin) / "Created" (Emby)
	mountPath  string
}

// mediaBrowserTarget notifies a Jellyfin or Emby server (or Silo's Jellyfin-
// compatible endpoint) that files under a path changed, prompting a rescan.
type mediaBrowserTarget struct {
	cfg        mediaBrowserConfig
	url        string
	httpClient *http.Client
	log        *slog.Logger
	stats      targetMetrics
}

// mediaUpdate is one entry in a Library/Media/Updated request. Jellyfin and Emby
// match these fields case-insensitively; lowercase keeps the payload minimal.
type mediaUpdate struct {
	Path       string `json:"path"`
	UpdateType string `json:"updateType"`
}

func newMediaBrowserTarget(cfg mediaBrowserConfig, log *slog.Logger) *mediaBrowserTarget {
	base := strings.TrimRight(strings.TrimSpace(cfg.baseURL), "/")
	return &mediaBrowserTarget{
		cfg:        cfg,
		url:        base + cfg.pathPrefix + "/Library/Media/Updated",
		httpClient: &http.Client{Timeout: 10 * time.Second},
		log:        log,
	}
}

func (t *mediaBrowserTarget) name() string { return t.cfg.flavor }

func (t *mediaBrowserTarget) metrics() *targetMetrics { return &t.stats }

func (t *mediaBrowserTarget) Import(ctx context.Context, _ /*mediaType*/, virtualPath string) {
	t.send(ctx, "import", []mediaUpdate{
		{Path: fullPath(t.cfg.mountPath, virtualPath), UpdateType: t.cfg.createType},
	})
}

// ImportBatch sends a coalesced burst as one request carrying every exact file
// path. Library/Media/Updated takes a list natively, so this target needs no
// folder-scoping: the server is told precisely as much about each individual
// file as before, just in one request instead of N.
func (t *mediaBrowserTarget) ImportBatch(ctx context.Context, b importBatch) {
	if len(b.files) == 0 {
		return
	}
	updates := make([]mediaUpdate, 0, len(b.files))
	for _, f := range b.files {
		updates = append(updates, mediaUpdate{
			Path: fullPath(t.cfg.mountPath, f), UpdateType: t.cfg.createType,
		})
	}
	t.send(ctx, "import", updates)
}

func (t *mediaBrowserTarget) Rename(ctx context.Context, _ /*mediaType*/, previousPath, newPath string) {
	t.send(ctx, "rename", []mediaUpdate{
		{Path: fullPath(t.cfg.mountPath, previousPath), UpdateType: "Deleted"},
		{Path: fullPath(t.cfg.mountPath, newPath), UpdateType: t.cfg.createType},
	})
}

func (t *mediaBrowserTarget) Delete(ctx context.Context, _ /*mediaType*/, virtualPath string) {
	t.send(ctx, "delete", []mediaUpdate{
		{Path: fullPath(t.cfg.mountPath, virtualPath), UpdateType: "Deleted"},
	})
}

func (t *mediaBrowserTarget) send(ctx context.Context, event string, updates []mediaUpdate) {
	if t == nil || t.cfg.baseURL == "" {
		return
	}
	body, err := json.Marshal(map[string]any{"Updates": updates})
	if err != nil {
		t.log.Warn("mediabrowser encode failed", "flavor", t.cfg.flavor, "event", event, "error", err)
		return
	}
	headers := map[string]string{}
	if t.cfg.apiKey != "" {
		headers["X-Emby-Token"] = t.cfg.apiKey
	}
	status, err := postJSON(ctx, t.httpClient, t.url, headers, body)
	t.stats.recordSend(status, err)
	if err != nil {
		t.log.Warn("mediabrowser delivery failed", "flavor", t.cfg.flavor, "event", event, "error", err)
		return
	}
	if !okStatus(status) {
		t.log.Warn("mediabrowser rejected", "flavor", t.cfg.flavor, "event", event, "status", status)
		return
	}
	t.log.Info("mediabrowser notified", "flavor", t.cfg.flavor, "event", event)
}
