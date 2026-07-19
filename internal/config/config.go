// Package config loads Wisp's runtime settings from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds everything Wisp needs to serve a resolver-backed library.
type Config struct {
	// AIOStreamsURL is the full Stremio manifest URL of the AIOStreams
	// instance, e.g. https://host/stremio/{uuid}/{blob}/manifest.json or
	// the alias form https://host/stremio/u/{alias}/manifest.json.
	AIOStreamsURL string
	// AIOStreamsPassword is the addon password (paired with the UUID/alias
	// derived from the URL for Search API basic auth).
	AIOStreamsPassword string
	// ListenAddr is the address the virtual-file HTTP server binds to.
	ListenAddr string
	// DBPath is where the pin database lives.
	DBPath string
	// SiloWebhookURL is the deprecated alias for NotifyArrWebhookURL
	// (WISP_SILO_WEBHOOK_URL). It still works; the canonical name wins if both
	// are set.
	SiloWebhookURL string
	// NotifyArrWebhookURL is the canonical ARR-compatible Autoscan webhook
	// (Silo/Sonarr/Radarr shape) notified after imports, renames, and deletions.
	NotifyArrWebhookURL string
	// NotifyJellyfinURL / NotifyJellyfinAPIKey point at a Jellyfin server (or
	// Silo's Jellyfin-compatible endpoint) rescanned via Library/Media/Updated.
	NotifyJellyfinURL    string
	NotifyJellyfinAPIKey string
	// NotifyEmbyURL / NotifyEmbyAPIKey point at an Emby server (same protocol,
	// routed under /emby).
	NotifyEmbyURL    string
	NotifyEmbyAPIKey string
	// NotifyPlexURL / NotifyPlexToken point at a Plex server refreshed via
	// per-folder partial scans.
	NotifyPlexURL   string
	NotifyPlexToken string
	// NotifyMountPath is the absolute library root as seen by notification
	// targets. Empty falls back to MountPath, and then to the notifier's
	// historical /mnt/wisp default.
	NotifyMountPath string
	// NotifyDebounce is the quiet period used to coalesce a burst of pins into
	// one media-server notification per parent directory. Zero disables
	// coalescing entirely, restoring one immediate notification per file — the
	// escape hatch for a consumer that cannot handle batched events. Clamped
	// to [1s, 60s] when non-zero.
	NotifyDebounce time.Duration
	// NotifyMountPathDefaulted reports that notification targets are configured
	// but neither WISP_NOTIFY_MOUNT_PATH nor WISP_MOUNT_PATH was set, so the
	// notifier's built-in default is in play. Load has no logger, so the caller
	// owns the deprecation warning.
	NotifyMountPathDefaulted bool
	// MountPath, when set, makes wisp self-mount the library there via the
	// embedded rclone VFS. Empty = serve HTTP only (mount it yourself).
	MountPath string
	// MountAllowOther exposes the mount to other users (needed when another
	// container reads the mount as a different UID).
	MountAllowOther bool
	// LogLevel is one of debug, info, warn, error.
	LogLevel string
	// ReadChunkSize is the initial VFS read chunk in bytes; it doubles up to
	// ReadChunkSizeLimit. Smaller reduces debrid over-fetch on seeks (more
	// concurrent viewers per bandwidth); larger favors sequential throughput.
	ReadChunkSize int64
	// ReadChunkSizeLimit caps the chunk ramp in bytes.
	ReadChunkSizeLimit int64

	// ScheduleInterval is the fallback ceiling for the monitor loop; the
	// scheduler otherwise wakes near a monitored item's next known air/release.
	ScheduleInterval time.Duration
	// ResolveConcurrency bounds how many episodes a single series resolves in
	// parallel during one scheduler pass. It is a global debrid safety valve —
	// titles are processed one at a time, so this is the peak resolver fan-out.
	// Clamped to [1, 16].
	ResolveConcurrency int
	// TierBackoffMax caps the per-quality-tier retry backoff. A requested tier that
	// consistently returns "results exist but not at this resolution" (e.g. 2160p
	// for a show with no 4K rips) is retried on an exponential schedule from
	// ScheduleInterval, never more than once per this duration — so wisp backs off
	// hard yet still catches a late release. Falls back on empty/unparseable input.
	TierBackoffMax time.Duration
	// TMDBAPIKey enables home-media release gating via TMDB (v3 key or v4 token).
	TMDBAPIKey string
	// TMDBMarkets is the ordered list of ISO-3166-1 regions whose digital/
	// physical release dates gate movies (any market releasing makes it eligible).
	TMDBMarkets []string

	// ProbeConcurrency is the global cap on in-flight probe HTTP requests across
	// ALL episodes — the debrid/resolver safety valve for transport probing.
	// Clamped to [1, 32].
	ProbeConcurrency int
	// ProbeWindow bounds how many candidate streams of ONE unit are probed
	// concurrently (rank-preserving sliding window). Clamped to [1, 8].
	ProbeWindow int
	// ProbeTimeout is the per-request network timeout for a single probe. It
	// starts only after the probe acquires a concurrency permit, so queue wait
	// never eats the network budget. Clamped to [2s, 30s].
	ProbeTimeout time.Duration

	// LazyResolutionRequested reports that WISP_LAZY_RESOLUTION was set to a true
	// value. The feature it enabled has been removed and the variable is ignored;
	// it is still parsed so existing deployments that set it keep starting. Load
	// has no logger, so the caller owns the deprecation warning.
	LazyResolutionRequested bool
}

// SelfMount reports whether wisp should mount the library itself.
func (c *Config) SelfMount() bool { return c.MountPath != "" }

// Load reads configuration from environment variables and validates it.
func Load() (*Config, error) {
	c := &Config{
		AIOStreamsURL:        strings.TrimSpace(os.Getenv("WISP_AIOSTREAMS_URL")),
		AIOStreamsPassword:   os.Getenv("WISP_AIOSTREAMS_PASSWORD"),
		ListenAddr:           envOr("WISP_LISTEN_ADDR", ":8080"),
		DBPath:               envOr("WISP_DB_PATH", "/data/wisp.db"),
		SiloWebhookURL:       strings.TrimSpace(os.Getenv("WISP_SILO_WEBHOOK_URL")),
		NotifyArrWebhookURL:  strings.TrimSpace(os.Getenv("WISP_NOTIFY_ARR_WEBHOOK_URL")),
		NotifyJellyfinURL:    strings.TrimSpace(os.Getenv("WISP_NOTIFY_JELLYFIN_URL")),
		NotifyJellyfinAPIKey: strings.TrimSpace(os.Getenv("WISP_NOTIFY_JELLYFIN_API_KEY")),
		NotifyEmbyURL:        strings.TrimSpace(os.Getenv("WISP_NOTIFY_EMBY_URL")),
		NotifyEmbyAPIKey:     strings.TrimSpace(os.Getenv("WISP_NOTIFY_EMBY_API_KEY")),
		NotifyPlexURL:        strings.TrimSpace(os.Getenv("WISP_NOTIFY_PLEX_URL")),
		NotifyPlexToken:      strings.TrimSpace(os.Getenv("WISP_NOTIFY_PLEX_TOKEN")),
		NotifyDebounce:       clampNotifyDebounce(optionalDurationEnv("WISP_NOTIFY_DEBOUNCE", 5*time.Second)),
		NotifyMountPath:      strings.TrimSpace(os.Getenv("WISP_NOTIFY_MOUNT_PATH")),
		MountPath:            strings.TrimSpace(os.Getenv("WISP_MOUNT_PATH")),
		MountAllowOther:      boolEnv("WISP_MOUNT_ALLOW_OTHER", true),
		LogLevel:             strings.ToLower(envOr("WISP_LOG_LEVEL", "info")),
		ReadChunkSize:        sizeEnv("WISP_READ_CHUNK_SIZE", 32<<20),
		ReadChunkSizeLimit:   sizeEnv("WISP_READ_CHUNK_SIZE_LIMIT", 512<<20),
		ScheduleInterval:     durationEnv("WISP_SCHEDULE_INTERVAL", 2*time.Hour),
		ResolveConcurrency:   clampInt(intEnv("WISP_RESOLVE_CONCURRENCY", 4), 1, 16),
		TierBackoffMax:       durationEnv("WISP_TIER_BACKOFF_MAX", 7*24*time.Hour),
		TMDBAPIKey:           strings.TrimSpace(os.Getenv("WISP_TMDB_API_KEY")),
		TMDBMarkets:          listEnv("WISP_TMDB_MARKETS", []string{"US", "CA", "GB", "AU", "DE", "FR", "IT", "ES", "JP", "IN"}),
		ProbeConcurrency:     clampInt(intEnv("WISP_PROBE_CONCURRENCY", 8), 1, 32),
		ProbeWindow:          clampInt(intEnv("WISP_PROBE_WINDOW", 3), 1, 8),
		ProbeTimeout:         clampDuration(durationEnv("WISP_PROBE_TIMEOUT", 10*time.Second), 2*time.Second, 30*time.Second),
		// Removed feature, still accepted so setting it is not a startup failure.
		LazyResolutionRequested: boolEnv("WISP_LAZY_RESOLUTION", false),
	}
	if c.AIOStreamsURL == "" {
		return nil, fmt.Errorf("WISP_AIOSTREAMS_URL is required")
	}
	if c.NotifyMountPath == "" {
		c.NotifyMountPath = c.MountPath
	}
	// Neither path is set but something wants notifying: the notifier's
	// /mnt/wisp default carries the deployment. That default is deprecated and
	// becomes an error in a future major, so flag it for the caller to warn on.
	c.NotifyMountPathDefaulted = c.NotifyMountPath == "" && c.notifyEnabled()
	return c, nil
}

// notifyEnabled reports whether at least one media-server notification target
// is configured.
func (c *Config) notifyEnabled() bool {
	return c.NotifyArrWebhookURL != "" || c.SiloWebhookURL != "" ||
		c.NotifyJellyfinURL != "" || c.NotifyEmbyURL != "" || c.NotifyPlexURL != ""
}

// durationEnv parses a Go duration like "2h" or "90m", falling back on empty or
// unparseable input. A non-positive duration also falls back.
func durationEnv(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// optionalDurationEnv parses a Go duration like durationEnv, except that an
// explicit zero ("0", "0s") is honored rather than treated as absent — zero is
// how an optional knob is switched off. Empty, negative, or unparseable input
// still falls back.
func optionalDurationEnv(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return fallback
	}
	return d
}

// clampNotifyDebounce bounds a non-zero debounce window to a sane range; zero
// passes through untouched to mean "disabled".
func clampNotifyDebounce(d time.Duration) time.Duration {
	if d == 0 {
		return 0
	}
	return clampDuration(d, time.Second, time.Minute)
}

// intEnv parses a base-10 integer, falling back on empty or unparseable input.
func intEnv(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// clampInt bounds n to the inclusive range [lo, hi].
func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// clampDuration bounds d to the inclusive range [lo, hi].
func clampDuration(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

// listEnv parses a comma-separated list, upper-casing and trimming each entry
// (markets are ISO-3166-1 codes). Empty input yields the fallback.
func listEnv(key string, fallback []string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.ToUpper(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// sizeEnv parses a byte size like "16M", "512M", "1G", or a plain byte count,
// falling back to the default on an empty or unparseable value.
func sizeEnv(key string, fallback int64) int64 {
	v := strings.TrimSpace(strings.ToUpper(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(v, "G"):
		mult, v = 1<<30, strings.TrimSuffix(v, "G")
	case strings.HasSuffix(v, "M"):
		mult, v = 1<<20, strings.TrimSuffix(v, "M")
	case strings.HasSuffix(v, "K"):
		mult, v = 1<<10, strings.TrimSuffix(v, "K")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return fallback
	}
	return n * mult
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func boolEnv(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
