# API Reference

Base URL: `http://<host>:8080` (default). All bodies are JSON.

wisp's API is intentionally tiny — it is *fed* by whatever you already use
(see [Feeding wisp](Feeding-wisp.md)). There is no auth on the API today; keep it
on a trusted network.

---

## `POST /api/add`

Resolve a title via AIOStreams, pin the top stream, and create its virtual file.

**Body**

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `media_type` | string | ✅ | `movie` or `series` |
| `imdb_id` | string | ✅ | Stremio id — `tt…` or `tmdb:…` |
| `title` | string | ✅ | Used for the folder/file name |
| `year` | number | – | Used for the folder/file name |
| `season` | number | series | |
| `episode` | number | series | |
| `quality` | string | – | Omit → pin AIOStreams' top stream and label it with the resolution it returned. Set (`1080p`, `2160p`/`4k`, …) → pin a stream **of that resolution**, so `1080p` and `2160p` of one title become two distinct files |

**Responses**

- `202` → `{"monitoring":true,"state":"queued"}` for request-shaped intake (`qualities`, `request_ref`, or `is_anime` present). Wisp schedules release-aware placeholder creation immediately; each placeholder pin has size 1 and resolves through AIOStreams on first playback.

- `200` → `{"virtual_path": "...", "size": 1471496964}` — pinned.
- `4xx/5xx` → `{"error": "<code>", "message": "..."}` — a structured code so a
  feeder can distinguish a genuine no-stream condition from a
  configuration/throttling problem:

  | Status | `error` | Meaning | Feeder action |
  |--------|---------|---------|---------------|
  | `502` | `no_streams` | AIOStreams has no stream yet | keep monitoring, re-add next cycle |
  | `502` | `no_quality_match` | streams exist, none at the requested `quality` | keep monitoring for that tier |
  | `500` | `aiostreams_auth` | AIOStreams rejected credentials (401/403) | fix `WISP_AIOSTREAMS_PASSWORD` — do not silently retry |
  | `429` | `rate_limited` | AIOStreams throttled (echoes `Retry-After`) | back off |
  | `503` | `upstream_unavailable` | transient 5xx / unreachable | retry later |
  | `400` | – | invalid body / missing required field | fix the request |

```sh
curl -X POST http://localhost:8080/api/add -d '{
  "media_type":"series","imdb_id":"tt38262097",
  "title":"The Villager of Level 999","year":2026,"season":1,"episode":4
}'
```

---

## `GET /api/pins`

List every pin.

```json
[
  {
    "virtual_path": "shows/Show (2026)/Season 01/Show (2026) - S01E04 - [1080p].mkv",
    "media_type": "series", "imdb_id": "tt38262097",
    "season": 1, "episode": 4, "title": "Show", "year": 2026,
    "quality": "1080p", "size": 1471496964, "resolved_at": 1784345504
  }
]
```

Feeders use this to avoid re-adding episodes wisp already has.

---

## `DELETE /api/pins`

Remove a pin; its virtual file drops out of the mount.

- By path: `DELETE /api/pins?path=<virtual_path>`
- By identity: body `{"imdb_id":"tt…","season":1,"episode":4}` (omit season/episode for a movie; matches all pins for that id)
- By identity + quality: add `"quality":"2160p"` to remove only that resolution tier, leaving the others

```sh
curl -X DELETE "http://localhost:8080/api/pins?path=shows/Show%20(2026)/Season%2001/ep.mkv"
curl -X DELETE http://localhost:8080/api/pins -d '{"imdb_id":"tt38262097","season":1,"episode":4}'
curl -X DELETE http://localhost:8080/api/pins -d '{"imdb_id":"tt38262097","season":1,"episode":4,"quality":"2160p"}'
```

Response: `{"deleted": ["<virtual_path>", ...]}`.

With Wisp's self-mount enabled, deleting a media file directly from the mount
performs the same unpin operation as `DELETE /api/pins`.
The mount does not permit creating, modifying, or renaming media files.

---

## `GET /api/status`

```json
{ "version": "0.2.0", "uptime_seconds": 1234, "pins": 42,
  "mounted": true, "mount_path": "/mnt/wisp" }
```

---

## `GET /api/requests/status`

wisp's view of one title, computed from the monitor record and the pin store —
no network calls. Identify the title with `media_type` + `tmdb_id`, falling back
to `imdb_id`. A `404` means wisp is not tracking it (no monitor, no pins) and the
caller should (re)submit via `POST /api/add`.

```sh
curl "http://localhost:8080/api/requests/status?media_type=movie&tmdb_id=27205"
```

```json
{
  "state": "completed",
  "pinned_qualities": ["1080p", "2160p"],
  "pinned_paths": [
    "movies/Inception (2010) [tmdb-27205]/Inception (2010) - [1080p].mkv",
    "movies/Inception (2010) [tmdb-27205]/Inception (2010) - [2160p].mkv"
  ],
  "detail": "requested scope pinned",
  "request_ref": "silo-1234"
}
```

| Field | Type | Notes |
|-------|------|-------|
| `state` | string | `queued` (tracked, nothing in scope pinned yet — includes unreleased/unaired), `completed` (requested scope pinned and servable), or `failed` (permanent give-up on an unresolvable identity) |
| `pinned_qualities` | string[] | Sorted, unique resolution tiers that have a servable pin |
| `pinned_paths` | string[] | Virtual paths of those servable pins, in the same order wisp stores them — one entry per pinned file, so a caller can map a tier to the exact file the mount exposes. Omitted when nothing is pinned |
| `detail` | string | Why the title is in this state (e.g. `awaiting home-media release`, `resolving stream`) |
| `request_ref` | string | Echoes the `request_ref` from intake, when one was supplied |

Only **servable** pins count — a pin whose stream has gone is neither
`completed` nor listed in `pinned_paths`.

---

## `GET /api/ws`

WebSocket endpoint (`ws://<host>:8080/api/ws`) that pushes an event whenever a
pin becomes playable. It exists for the [lazy-resolution](Configuration.md#lazy-resolution)
flow: a 1-byte placeholder is cataloged instantly, and this event is how a
media-server plugin learns to rescan and promote that entry to its real size.

wisp broadcasts `pin_completed` when:

- a pin resolves eagerly (`POST /api/add` or a scheduler pass),
- a placeholder pin is created, and
- a placeholder resolves on first playback.

A [self-heal](Architecture.md#the-self-heal-model) re-resolve does **not** emit an
event — the virtual path the media server already holds is unchanged.

**Event payload**

```json
{
  "event": "pin_completed",
  "media_type": "series",
  "imdb_id": "tt38262097",
  "tmdb_id": "215061",
  "tvdb_id": "441314",
  "virtual_path": "shows/Show (2026) [tvdb-441314]/Season 01/Show (2026) - S01E04 - [1080p].mkv"
}
```

| Field | Type | Notes |
|-------|------|-------|
| `event` | string | Always `pin_completed` today |
| `media_type` | string | `movie` or `series` |
| `imdb_id` | string | The id wisp searched with — `tt…`, or `tmdb:…` for a tmdb-only title |
| `tmdb_id` / `tvdb_id` | string | Enriched from Cinemeta when only an IMDb id was known; either may be empty |
| `virtual_path` | string | Path relative to the library root — append it to `WISP_NOTIFY_MOUNT_PATH` for the on-disk path |

**Keepalive.** wisp ignores anything a client sends, but it does require the
client to send *something*: a connection with no inbound frame for **120
seconds** is closed. Without this a client that vanishes without a TCP FIN
(laptop sleep, wifi drop, NAT rebind) would hold its slot forever. Send any
frame (a `ping` string is fine) on a shorter interval — 30–60 s — and reconnect
on close.

## `GET /metrics`

Prometheus text-format metrics: `wisp_pins`, `wisp_mounted`, `wisp_uptime_seconds`,
`wisp_file_requests_total`, `wisp_link_cache_hits_total`,
`wisp_link_cache_misses_total`, `wisp_reresolves_total`, `wisp_link_cache_entries`.

## `GET /api/healthz`

`200 ok` — liveness probe. Always `200` if the process is serving HTTP at all;
it says nothing about the mount. Use `/api/health` to gate a dependent container.

## `GET /api/health`

Readiness probe. **The status code is the contract:**

- `200` — ready.
- `503` — not ready.

The body is for humans; don't parse it.

```json
{ "status": "ok", "self_mount": true, "mounted": true }
```

`status` is `ok` or `mount_down`. `mounted` is present only when wisp is
self-mounting. No auth, and no paths, URLs, or tokens are exposed.

What "ready" means depends on how wisp is deployed:

| Deployment | Ready when |
|---|---|
| **Self-mounting** (`WISP_MOUNT_PATH` set) | The HTTP server is up **and** the FUSE mount is live. An alive-but-unmounted wisp would let a media server scan an empty mountpoint, so that reports `503`. |
| **HTTP-only** (no `WISP_MOUNT_PATH`) | The HTTP server is up. The operator mounts externally, so wisp has no mount to report on and mount state is not part of the verdict. |

The check is deliberately cheap and dependency-free: it makes no AIOStreams or
TMDb call (an upstream outage must not mark wisp unhealthy — already-pinned
files still serve) and no database read.

Recommended Docker healthcheck — gate a media server on wisp being mounted with
`depends_on: { wisp: { condition: service_healthy } }`:

```yaml
healthcheck:
  test: ["CMD", "wget", "--spider", "-q", "http://127.0.0.1:8080/api/health"]
  interval: 10s
  timeout: 3s
  retries: 3
  start_period: 30s
```

`wget --spider` exits non-zero on a `503`, so no output parsing is involved.

---

## File serving (what the media server hits)

Everything not under `/api/` is the virtual filesystem:

- `GET /` and directory paths → HTML listings (what rclone's `:http:` backend walks).
- `HEAD /<virtual_path>` → the pinned size, no upstream call (cheap `stat`).
- `GET /<virtual_path>` (with `Range`) → range-proxied bytes from the pinned
  stream, with [self-heal](Architecture.md#the-self-heal-model) on a dead upstream.

You normally don't call these directly — the rclone mount does.
