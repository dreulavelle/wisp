# Configuration

All configuration is via environment variables.

| Variable | Default | Notes |
|----------|---------|-------|
| `WISP_AIOSTREAMS_URL` | — | **Required.** Your AIOStreams manifest URL: `https://host/stremio/<uuid>/<config>/manifest.json` (or the alias form `.../stremio/u/<alias>/manifest.json`). |
| `WISP_AIOSTREAMS_PASSWORD` | — | Addon password. Paired with the UUID/alias from the URL for Search API auth. If it already contains `uuid:password`, it's used verbatim. |
| `WISP_LISTEN_ADDR` | `:8080` | HTTP bind address. |
| `WISP_DB_PATH` | `/data/wisp.db` | bbolt database for pins **and** monitors. Persist this (a volume) to keep your library and watchlist across restarts. |
| `WISP_API_TOKEN` | — | **Optional API authentication.** If set, wisp requires `Authorization: Bearer <token>` on the control-plane endpoints. **Unset — the default — means the API is open to anyone who can reach the port.** See [API authentication](#api-authentication). |
| `WISP_MOUNT_PATH` | — | If set, wisp self-mounts the library here (needs `/dev/fuse` + `SYS_ADMIN`). Unset = serve HTTP only and mount it yourself. |
| `WISP_NOTIFY_MOUNT_PATH` | — | Absolute library root as seen by notification targets. Falls back to `WISP_MOUNT_PATH`, then to `/mnt/wisp`. Setting it explicitly is preferred: relying on the `/mnt/wisp` default is deprecated (wisp warns at startup) and will become required in a future major version. |
| `WISP_SCHEDULE_INTERVAL` | `2h` | Fallback ceiling for the monitor loop. The scheduler otherwise wakes near a monitored item's next known airstamp/release — it doesn't poll on a fixed tick. |
| `WISP_RESOLVE_CONCURRENCY` | `4` | How many episodes of a series resolve in parallel per scheduler pass. Titles are still processed one at a time, so this is the peak resolver fan-out against your debrid provider — raise it to drain long seasons faster, lower it if you hit rate limits. Clamped to `1`–`16`. |
| `WISP_TIER_BACKOFF_MAX` | `168h` (7d) | Cap on the retry backoff for a requested quality tier that consistently returns "results exist, but not at this resolution" — e.g. a `2160p` request for a show with no 4K rips. Such a tier is retried on an exponential schedule from `WISP_SCHEDULE_INTERVAL` (interval, 2×, 4×, …), never more often than once per this duration once it saturates. wisp never permanently gives up, so a late release is still picked up; a successful pin of the tier resets it to the fast cadence. |
| `WISP_PROBE_CONCURRENCY` | `8` | Global cap on in-flight probe HTTP requests across **all** episodes — the debrid/resolver safety valve for transport probing. Composes with `WISP_RESOLVE_CONCURRENCY`: episodes resolve in parallel, but their combined probe fan-out is held to this ceiling. Clamped to `1`–`32`. |
| `WISP_PROBE_WINDOW` | `3` | How many ranked candidate streams of a single episode are probed concurrently. wisp verifies availability in AIOStreams rank order and commits the highest-ranked available stream, so a faster lower-ranked probe never wins over a still-pending higher rank. Clamped to `1`–`8`. |
| `WISP_PROBE_TIMEOUT` | `10s` | Per-request network timeout for one probe. The clock starts only **after** the probe acquires a concurrency permit, so time spent queued never eats the network budget. Clamped to `2s`–`30s`. |
| `WISP_TMDB_API_KEY` | — | TMDB v3 key or v4 token. Enables home-media release gating for movies (digital/physical dates); without it, wisp falls back to Cinemeta's release date. |
| `WISP_TMDB_MARKETS` | `US,CA,GB,AU,DE,FR,IT,ES,JP,IN` | ISO-3166-1 regions whose TMDB digital/physical release makes a movie eligible (any one releasing counts). |
| `WISP_NOTIFY_ARR_WEBHOOK_URL` | — | ARR-compatible Autoscan webhook (Silo/Sonarr/Radarr wire format) for instant import, rename, and delete rescans. Keep secret. |
| `WISP_SILO_WEBHOOK_URL` | — | Deprecated alias for `WISP_NOTIFY_ARR_WEBHOOK_URL` (canonical wins if both set). |
| `WISP_NOTIFY_JELLYFIN_URL` / `_API_KEY` | — | Jellyfin (or Silo's Jellyfin-compat) base URL + admin API key; rescans via `Library/Media/Updated`. |
| `WISP_NOTIFY_EMBY_URL` / `_API_KEY` | — | Emby base URL + API key (same protocol, routed under `/emby`). |
| `WISP_NOTIFY_PLEX_URL` / `WISP_NOTIFY_PLEX_TOKEN` | — | Plex base URL + `X-Plex-Token`; partial-scans just the changed folder. |
| `WISP_NOTIFY_DEBOUNCE` | `5s` | Quiet period for coalescing a burst of pins into one notification per folder (see [Notification coalescing](#notification-coalescing)). A group is also force-flushed after 6× this value, so a continuous stream can't starve it. Set to `0` to disable coalescing and notify per file, as wisp did before v1.4. Clamped to `1s`–`60s` when non-zero. |
| `WISP_MOUNT_ALLOW_OTHER` | `true` | Expose the mount to other UIDs — needed when a media-server container reads the mount as a different user. |
| `WISP_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. `debug` narrates every serve + the full self-heal path. |
| `WISP_READ_CHUNK_SIZE` | `32M` | Initial VFS read chunk (smaller = less debrid over-fetch on seeks). |
| `WISP_READ_CHUNK_SIZE_LIMIT` | `512M` | Cap for the read-chunk ramp. |
| `WISP_LAZY_RESOLUTION` | — | **Removed and ignored.** Lazy placeholder resolution no longer exists; every monitored title is resolved eagerly. Still accepted so existing deployments keep starting — setting it to a true value logs a warning at startup. Scheduled for deletion in a future major version. |

## AIOStreams URL & auth

You only fill in two things: `WISP_AIOSTREAMS_URL` (your manifest URL) and
`WISP_AIOSTREAMS_PASSWORD` (your AIOStreams password).

wisp uses the AIOStreams **Search API** (`/api/v1/search`), which authenticates
with HTTP Basic auth (`uuid:password`). wisp reads the **UUID from the
`/stremio/<uuid>/…` path** of the manifest URL automatically and pairs it with
`WISP_AIOSTREAMS_PASSWORD` — so the password is all you supply.

The password is required **unless** your AIOStreams instance has
`allowUnauthenticatedSearchApiRequests` enabled (check the instance's status
endpoint) — then you can leave `WISP_AIOSTREAMS_PASSWORD` unset. With auth
required and no password, wisp logs a startup warning and every add returns
`aiostreams_auth`.

> The alias form (`ALIASED_CONFIGURATIONS`) works for the manifest path, but the
> Search API expects the real UUID — use the full `/stremio/<uuid>/<config>/…`
> URL for `WISP_AIOSTREAMS_URL` to be safe.

Because wisp goes through your AIOStreams instance, **all of your AIOStreams
config applies automatically** — provider order, quality/HDR/dub preferences,
debrid vs usenet, filtering. wisp adds no ranking or filtering of its own.

## API authentication

Optional, and **off by default**. With `WISP_API_TOKEN` unset, wisp behaves as
it always has: every endpoint is open to anyone who can reach the port. Since
the common deployment publishes `8080` on `0.0.0.0`, that means anyone on the
network can list your library or delete every pin and monitor. Wisp logs a
warning at startup when no token is set.

Setting the variable turns authentication on:

```sh
# Generate a long random token — there is no rate limiting, so length is the
# only thing standing between an attacker and a brute force.
openssl rand -hex 32
```

```yaml
environment:
  WISP_API_TOKEN: ${WISP_API_TOKEN}   # keep it in .env, never in the compose file
```

Callers then send it as a bearer token:

```sh
curl -H "Authorization: Bearer $WISP_API_TOKEN" http://localhost:8080/api/pins
```

If you use `silo-plugin-wisp`, put the same value in its `wisp_token`
connection setting — the plugin already sends it as `Authorization: Bearer`.
Before this feature existed wisp silently ignored that field, so a filled-in
`wisp_token` did not actually protect anything.

### What the token protects

| Endpoint | Gated | Why |
|---|---|---|
| `POST /api/add` | ✅ | Mutating |
| `DELETE /api/pins` | ✅ | Mutating — deletes library entries |
| `POST` / `DELETE /api/monitors`, `POST /api/monitors/refresh` | ✅ | Mutating |
| `GET /api/pins`, `GET /api/monitors`, `GET /api/schedule`, `GET /api/requests/status` | ✅ | Read-only, but they disclose your entire library and what you've requested |
| `GET /api/status`, `GET /metrics` | ✅ | Counts, version, mount path. Nothing credential-less consumes them — Prometheus sends bearer tokens natively |
| `GET /api/health`, `GET /api/healthz` | ❌ | A Docker healthcheck (`wget --spider`) cannot send a header; gating these would wedge `depends_on: {condition: service_healthy}`. Their bodies expose only a status string and two booleans |
| File serving (`/`, `/<virtual_path>`) | ❌ | **See below** |

### File serving is not gated

The token does **not** protect the virtual filesystem — directory listings and
byte ranges. That is the data plane your FUSE mount reads through, and closing
it would break mounting: an external `rclone mount` (a supported deployment
mode wisp has no way to reconfigure) would start failing the moment you set a
token, and threading the secret into rclone's connection string risks it
surfacing in rclone's own logs and error messages.

The practical consequence: **anyone who can reach the port can still browse
your library tree and stream its files**, token or no token. The token stops
them from changing anything and from reading the API's structured views. If
that matters to you, keep the port on a trusted network or behind a reverse
proxy that authenticates — the token is defense in depth, not a substitute.

### Failure responses

Every rejection is `401 Unauthorized` with `WWW-Authenticate: Bearer` and a
small JSON body; see the [API reference](API-Reference.md#authentication).

## Persisting data

Mount a volume at `WISP_DB_PATH`'s directory (default `/data`). The pin database
is the whole library — lose it and you'd re-add everything (pins re-resolve
fine, but the list is gone).

## Media-server notifications

On every pin, rename, or delete, wisp tells your media server to rescan the
affected folder, so new content appears immediately. Configure any combination of
targets — all configured ones are notified. Paths are derived from
`WISP_NOTIFY_MOUNT_PATH`, the path the media server sees on disk. If it is unset,
wisp reuses an explicitly configured `WISP_MOUNT_PATH`, and failing that falls
back to `/mnt/wisp` with a startup deprecation warning. Set
`WISP_NOTIFY_MOUNT_PATH` explicitly — the fallback will be removed in a future
major version.

- **Silo (recommended)** — in **Autoscan → Sources**, add a *Sonarr/Radarr
  Webhook* source, click **Generate webhook URL**, and set it as
  `WISP_NOTIFY_ARR_WEBHOOK_URL`. No event checkboxes or path settings needed.
- **Jellyfin / Emby** — `WISP_NOTIFY_JELLYFIN_URL` / `WISP_NOTIFY_EMBY_URL` plus
  an admin API key; wisp posts a `Library/Media/Updated` hint.
- **Plex** — `WISP_NOTIFY_PLEX_URL` + `WISP_NOTIFY_PLEX_TOKEN`; wisp partial-scans
  just the changed folder.

Delivery is best-effort on a background goroutine: a slow or unreachable server is
logged but never blocks or fails a pin or delete. Webhook URLs and tokens are
secrets — rotate them if exposed.

### Notification coalescing

A single series request pins many files in a short burst — one per episode per
requested quality tier. Media servers defend themselves against that by
coalescing rapid rescan requests, and they scan only the path from the request
they kept. Because each of wisp's notifications named exactly one file, every
notification coalesced away on the server side was a file that never got
scanned: a measured 13-pin request (7 episodes × 2 tiers) produced only 5 scans
and landed **3 of 7** episodes, even though a later full library scan matched all
13 files. Many separate notifications are strictly worse than one that names
every file.

So wisp batches instead. Pins are grouped by parent directory and held until
either nothing new has joined the group for `WISP_NOTIFY_DEBOUNCE` (default
`5s` — comfortably longer than the ~2s gaps inside a real burst) or the group is
`6×` that old (default `30s`, bounding worst-case latency well inside a 60s
target). Two shows resolving at once stay two separate groups.

The fix is to collapse N requests into one — **not** to widen what any request
points at. Every target still names exact files; none is ever sent a bare
directory. What each receives depends on what its protocol can express:

- **ARR webhook (Silo/Sonarr/Radarr)** — one `Download` event carrying every
  exact path in the plural `episodeFiles` (series) or `movieFiles` (movies)
  array, which the consumer expands into one file-scoped ingest per path.
- **Jellyfin / Emby** — one `Library/Media/Updated` request listing **every**
  exact file path. That endpoint takes a list natively, so nothing is lost.
- **Plex** — one partial scan of the shared folder. Plex already scanned folders
  rather than files, so this just removes duplicate requests.

Deletes and renames are never coalesced: they carry specific paths that a
batched event cannot express, and they aren't part of the burst problem.

Set `WISP_NOTIFY_DEBOUNCE=0` to disable batching entirely and restore the
pre-v1.4 one-webhook-per-file behavior.

## Mount tuning (self-mount mode)

wisp embeds rclone's VFS with sensible defaults for streaming: cache **off**
(pure passthrough, no local disk), a 32 MiB initial read chunk that ramps to
512 MiB for efficient sequential playback while keeping seeks snappy, and a
short directory cache. Set `WISP_READ_CHUNK_SIZE` (default `32M`) to tune the
initial chunk and `WISP_READ_CHUNK_SIZE_LIMIT` (default `512M`) to cap the ramp.
Smaller chunks reduce over-fetch after seeks; larger chunks favor sequential
throughput.

See [Deployment](Deployment.md) for the `/dev/fuse`, `SYS_ADMIN`, and mount
propagation requirements.
