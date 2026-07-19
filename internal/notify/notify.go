// Package notify tells media servers to rescan when wisp pins, renames, or
// deletes a virtual file. It fans out to any number of configured targets —
// an ARR-compatible webhook (Silo/Sonarr/Radarr), a MediaBrowser server
// (Jellyfin/Emby/Silo's Jellyfin-compat endpoint), or Plex — so one wisp
// instance can drive several libraries at once.
//
// Delivery is fire-and-forget: every target is notified on its own goroutine
// with a detached, timeout-bounded context, so a slow or unreachable media
// server never blocks (or fails) a pin or delete. Failures are logged per
// target and otherwise swallowed.
package notify

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// notifyTimeout bounds a single target's whole notification (Plex may need a
// section lookup plus a refresh). Individual HTTP clients keep tighter timeouts.
const notifyTimeout = 30 * time.Second

// defaultMountPath is used when no mount path is configured, matching the arr
// webhook's historical default.
const defaultMountPath = "/mnt/wisp"

// Notifier reports pin lifecycle events to media servers. Paths are
// library-relative virtual paths (e.g. "movies/Foo (2020)/Foo.mkv"); each
// target resolves them against the mount path as needed.
type Notifier interface {
	Import(ctx context.Context, mediaType, virtualPath string)
	Rename(ctx context.Context, mediaType, previousPath, newPath string)
	Delete(ctx context.Context, mediaType, virtualPath string)
}

// target is one concrete media server. Each implementation is responsible for
// its own payload shape and transport; the Multi fans events out to all of them.
type target interface {
	Notifier
	// name identifies the target in log lines.
	name() string
	// ImportBatch delivers a burst of imports that share a media type and a
	// parent directory. Each target decides how to represent the burst — the
	// right answer differs per protocol, so this is deliberately not a loop
	// over Import. A batch of exactly one file must be delivered identically
	// to the equivalent Import call, which is what makes a disabled debounce
	// window byte-for-byte equivalent to the pre-coalescing behavior.
	ImportBatch(ctx context.Context, b importBatch)
}

// importBatch is a set of imports coalesced over one debounce window. Every
// file shares mediaType and dir; files are library-relative virtual paths in
// arrival order, deduplicated.
type importBatch struct {
	mediaType string
	dir       string
	files     []string
}

// Options configures which targets a notifier drives. Empty fields disable the
// corresponding target; a notifier with no targets is a safe no-op.
type Options struct {
	// ArrWebhookURL is the canonical ARR-compatible webhook URL
	// (WISP_NOTIFY_ARR_WEBHOOK_URL).
	ArrWebhookURL string
	// SiloWebhookURL is the deprecated alias (WISP_SILO_WEBHOOK_URL). When both
	// are set the canonical one wins and a note is logged.
	SiloWebhookURL string

	// Jellyfin target (Jellyfin, or Silo's Jellyfin-compatible endpoint).
	JellyfinURL    string
	JellyfinAPIKey string

	// Emby target — same protocol as Jellyfin but routes under /emby and marks
	// new files "Created" per Emby convention.
	EmbyURL    string
	EmbyAPIKey string

	// Plex target.
	PlexURL   string
	PlexToken string

	// MountPath is where the media servers see wisp's library on disk; virtual
	// paths are resolved against it. Empty defaults to /mnt/wisp.
	MountPath string

	// Debounce is the quiet period used to coalesce a burst of imports into one
	// notification per parent directory. Zero (or negative) disables coalescing
	// and restores immediate per-file notifications. The companion max-wait
	// bound is derived from it (see maxWaitFor).
	Debounce time.Duration
}

// New builds a Notifier from the configured targets. It always returns a
// non-nil *Multi (with zero or more targets), so callers never need a nil check.
func New(opts Options, log *slog.Logger) *Multi {
	mountPath := strings.TrimSpace(opts.MountPath)
	if mountPath == "" {
		mountPath = defaultMountPath
	}

	arrURL := strings.TrimSpace(opts.ArrWebhookURL)
	if silo := strings.TrimSpace(opts.SiloWebhookURL); silo != "" {
		if arrURL != "" && arrURL != silo {
			log.Info("both WISP_NOTIFY_ARR_WEBHOOK_URL and WISP_SILO_WEBHOOK_URL are set; using the canonical WISP_NOTIFY_ARR_WEBHOOK_URL")
		} else if arrURL == "" {
			arrURL = silo
		}
	}

	m := &Multi{mountPath: mountPath, log: log}
	if arrURL != "" {
		m.targets = append(m.targets, newArrTarget(arrURL, mountPath, log))
	}
	if url := strings.TrimSpace(opts.JellyfinURL); url != "" {
		m.targets = append(m.targets, newMediaBrowserTarget(mediaBrowserConfig{
			flavor: "jellyfin", baseURL: url, apiKey: opts.JellyfinAPIKey,
			pathPrefix: "", createType: "Modified", mountPath: mountPath,
		}, log))
	}
	if url := strings.TrimSpace(opts.EmbyURL); url != "" {
		m.targets = append(m.targets, newMediaBrowserTarget(mediaBrowserConfig{
			flavor: "emby", baseURL: url, apiKey: opts.EmbyAPIKey,
			pathPrefix: "/emby", createType: "Created", mountPath: mountPath,
		}, log))
	}
	if url := strings.TrimSpace(opts.PlexURL); url != "" {
		m.targets = append(m.targets, newPlexTarget(url, opts.PlexToken, mountPath, log))
	}
	if opts.Debounce > 0 {
		m.coalesce = newCoalescer(opts.Debounce, maxWaitFor(opts.Debounce), func(b importBatch) {
			// A coalesced batch outlives whichever request produced it, so it
			// starts from a fresh context rather than a stored caller context.
			m.emitImport(context.Background(), b)
		})
	}
	// Hold one reference for Close to release. Keeping the counter above zero
	// for the notifier's whole life means fanout's Add can never race Wait.
	m.wg.Add(1)
	return m
}

// Multi fans notifications out to every configured target.
type Multi struct {
	targets   []target
	mountPath string
	log       *slog.Logger

	// coalesce batches import bursts; nil when debouncing is disabled.
	coalesce *coalescer

	// wg tracks in-flight delivery goroutines so Close can drain them.
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// MountPath returns the library root that virtual paths are resolved against,
// after the default has been applied (for startup logging).
func (m *Multi) MountPath() string { return m.mountPath }

// Targets returns the names of the configured targets (for startup logging).
func (m *Multi) Targets() []string {
	names := make([]string, 0, len(m.targets))
	for _, t := range m.targets {
		names = append(names, t.name())
	}
	return names
}

// Import announces a newly pinned file. When debouncing is enabled the event
// joins its parent directory's batch instead of being delivered immediately;
// see coalescer for why. Either way the call does not block on the network.
func (m *Multi) Import(ctx context.Context, mediaType, virtualPath string) {
	if m == nil || len(m.targets) == 0 {
		return
	}
	if m.coalesce == nil {
		m.emitImport(ctx, importBatch{mediaType: mediaType, dir: path.Dir(virtualPath), files: []string{virtualPath}})
		return
	}
	m.coalesce.add(mediaType, virtualPath)
}

// emitImport fans one finished batch out to every target.
func (m *Multi) emitImport(ctx context.Context, b importBatch) {
	m.fanout(ctx, func(ctx context.Context, t target) { t.ImportBatch(ctx, b) })
}

// Close flushes any pending coalesced imports and waits for in-flight
// deliveries to finish, bounded by ctx. Call it once, from the shutdown path,
// so a pin that landed inside the debounce window is not dropped by a restart.
// Events arriving after Close are delivered immediately rather than batched.
func (m *Multi) Close(ctx context.Context) {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		if m.coalesce != nil {
			m.coalesce.close()
		}
		m.wg.Done() // release the construction-time reference
	})

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		m.log.Warn("gave up draining media-server notifications", "error", ctx.Err())
	}
}

func (m *Multi) Rename(ctx context.Context, mediaType, previousPath, newPath string) {
	m.fanout(ctx, func(ctx context.Context, t target) { t.Rename(ctx, mediaType, previousPath, newPath) })
}

func (m *Multi) Delete(ctx context.Context, mediaType, virtualPath string) {
	m.fanout(ctx, func(ctx context.Context, t target) { t.Delete(ctx, mediaType, virtualPath) })
}

// fanout runs fn against every target on its own goroutine, detached from the
// caller's context so a returning request handler doesn't cancel delivery.
func (m *Multi) fanout(ctx context.Context, fn func(context.Context, target)) {
	if m == nil || len(m.targets) == 0 {
		return
	}
	base := context.WithoutCancel(ctx)
	for _, t := range m.targets {
		m.wg.Add(1)
		go func(t target) {
			defer m.wg.Done()
			fctx, cancel := context.WithTimeout(base, notifyTimeout)
			defer cancel()
			fn(fctx, t)
		}(t)
	}
}

// fullPath resolves a library-relative virtual path against the mount path so
// media servers receive the absolute path they see on disk.
func fullPath(mountPath, virtualPath string) string {
	return path.Join(mountPath, strings.TrimLeft(virtualPath, "/"))
}

// postJSON sends a JSON body and drains the response. It returns the status code
// (0 on transport error) so callers can log per-target outcomes.
func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wisp")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doAndDrain(client, req)
}

// doAndDrain executes req, drains up to 64KiB of the body, and returns the
// status code (0 on transport error).
func doAndDrain(client *http.Client, req *http.Request) (int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

// okStatus reports whether a status code is a 2xx success.
func okStatus(code int) bool { return code >= 200 && code < 300 }
