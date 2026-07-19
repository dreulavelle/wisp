package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
)

// arrTarget sends ARR-compatible Autoscan events (Sonarr/Radarr shape) to a
// single webhook URL — the format Silo's Autoscan sources accept natively.
type arrTarget struct {
	url        string
	mountPath  string
	httpClient *http.Client
	log        *slog.Logger
	stats      targetMetrics
}

func newArrTarget(webhookURL, mountPath string, log *slog.Logger) *arrTarget {
	return &arrTarget{
		url:        strings.TrimSpace(webhookURL),
		mountPath:  mountPath,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		log:        log,
	}
}

func (t *arrTarget) name() string { return "arr-webhook" }

func (t *arrTarget) metrics() *targetMetrics { return &t.stats }

func (t *arrTarget) Import(ctx context.Context, mediaType, virtualPath string) {
	full := fullPath(t.mountPath, virtualPath)
	payload := map[string]any{"eventType": "Download"}
	if mediaType == "series" {
		payload["episodeFile"] = map[string]string{"path": full}
	} else {
		payload["movieFile"] = map[string]string{"path": full}
	}
	t.send(ctx, "import", payload)
}

// ImportBatch announces a coalesced burst as ONE webhook carrying every exact
// file path, using the plural episodeFiles / movieFiles array form.
//
// Do not "simplify" this to send the parent directory instead. That was tried
// and disproven against a live Silo instance: a directory in episodeFile.path
// is rejected outright with "autoscan: webhook paths matched no library
// folder", queuing no scan at all — while still answering HTTP 202, so the
// accept status proves nothing. Folder-scoping here would turn the measured
// 3-of-7 failure into 0-of-7.
//
// Both plural forms are measured, not assumed. Probing the same instance:
// episodeFiles with N exact paths, and movieFiles with N exact paths, each
// produced N file-scoped ingests from a single request, with no warning. The
// singular file payload was separately confirmed as the working control.
//
// So this target converges on the same shape as Jellyfin/Emby — one request,
// every exact path — rather than being the odd one out. The burst is fixed by
// collapsing N requests into one, not by widening what any request points at.
//
// A single-file batch keeps the plain Import payload, so a disabled debounce
// window stays byte-for-byte equivalent to the pre-coalescing behavior.
//
// The batch is deliberately NOT chunked into several requests. Splitting would
// reintroduce exactly the hazard this fix exists to remove — multiple rapid
// webhooks, any of which the consumer may coalesce away, and a dropped one is
// silent. Size is not a concern: ~150 paths is on the order of 20 KiB, far
// below any default request-body limit, and the coalescer's max wait already
// caps how much a single batch can accumulate.
func (t *arrTarget) ImportBatch(ctx context.Context, b importBatch) {
	if len(b.files) == 0 {
		return
	}
	if len(b.files) == 1 {
		t.Import(ctx, b.mediaType, b.files[0])
		return
	}
	entries := make([]map[string]string, 0, len(b.files))
	for _, f := range b.files {
		entries = append(entries, map[string]string{"path": fullPath(t.mountPath, f)})
	}
	full := fullPath(t.mountPath, b.dir)
	payload := map[string]any{"eventType": "Download"}
	if b.mediaType == "series" {
		// The burst shares a season folder; the show folder is its parent.
		payload["series"] = map[string]string{"path": path.Dir(full)}
		payload["episodeFiles"] = entries
	} else {
		// The burst shares the movie folder itself (one file per quality tier).
		payload["movie"] = map[string]string{"folderPath": full}
		payload["movieFiles"] = entries
	}
	t.send(ctx, "import", payload)
}

func (t *arrTarget) Rename(ctx context.Context, mediaType, previousPath, newPath string) {
	entry := map[string]string{"previousPath": fullPath(t.mountPath, previousPath), "newPath": fullPath(t.mountPath, newPath)}
	payload := map[string]any{"eventType": "Rename"}
	if mediaType == "series" {
		payload["renamedEpisodeFiles"] = []map[string]string{entry}
	} else {
		payload["renamedMovieFiles"] = []map[string]string{entry}
	}
	t.send(ctx, "rename", payload)
}

func (t *arrTarget) Delete(ctx context.Context, mediaType, virtualPath string) {
	full := fullPath(t.mountPath, virtualPath)
	file := map[string]string{"path": full, "relativePath": path.Base(full)}
	payload := map[string]any{"instanceName": "Wisp", "deleteReason": "Manual"}
	if mediaType == "series" {
		payload["eventType"] = "EpisodeFileDelete"
		payload["series"] = map[string]string{"path": path.Dir(path.Dir(full))}
		payload["episodeFile"] = file
	} else {
		payload["eventType"] = "MovieFileDelete"
		payload["movie"] = map[string]string{"folderPath": path.Dir(full)}
		payload["movieFile"] = file
	}
	t.send(ctx, "delete", payload)
}

func (t *arrTarget) send(ctx context.Context, event string, payload any) {
	if t == nil || t.url == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.log.Warn("arr webhook encode failed", "event", event, "error", err)
		return
	}
	status, err := postJSON(ctx, t.httpClient, t.url, nil, body)
	t.stats.recordSend(status, err)
	if err != nil {
		t.log.Warn("arr webhook delivery failed", "event", event, "error", err)
		return
	}
	if !okStatus(status) {
		t.log.Warn("arr webhook rejected", "event", event, "status", status)
		return
	}
	t.log.Info("arr webhook delivered", "event", event)
}
