# API Reference

Base URL: `http://<host>:8080` (default). All bodies are JSON.

wisp's API is intentionally tiny — it is *fed* by whatever you already use
(see [Feeding wisp](Feeding-wisp.md)).

---

## Authentication

Optional and **off by default**. With `WISP_API_TOKEN` unset, every endpoint is
open to anyone who can reach the port — keep it on a trusted network.

Set `WISP_API_TOKEN` and wisp requires a bearer token on every endpoint below
*except* `/api/health` and `/api/healthz` (a Docker healthcheck can't send a
header) and file serving (the FUSE mount reads through it). See
[Configuration → API authentication](Configuration.md#api-authentication) for
the full table and the limits of what this protects.

```
Authorization: Bearer <WISP_API_TOKEN>
```

```sh
curl -H "Authorization: Bearer $WISP_API_TOKEN" http://localhost:8080/api/pins
```

**Failures are always `401`** — a missing header, a non-`Bearer` scheme, an
empty credential, and a wrong token all return the same response. There is no
`403`: a wrong token is a failed authentication, not a permission decision, and
answering differently for "malformed" versus "wrong" would tell a prober that
its candidate reached the comparison.

```
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Bearer
Content-Type: application/json

{"error":"unauthorized","message":"a valid bearer token is required"}
```

The scheme is matched case-insensitively. Tokens are compared in constant time,
and a rejected token is never written to the response or the logs — auth
failures log at `warn` with the remote address only.

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

- `202` → `{"monitoring":true,"state":"queued"}` for request-shaped intake (`qualities`, `request_ref`, or `is_anime` present). Wisp records the monitor immediately and resolves each unit through AIOStreams on the next scheduler pass, once it is past its release/air date.

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
| `detail` | string | Why the title is in this state (e.g. `awaiting home-media release`, `resolving stream`), including any tier that was given up on |
| `request_ref` | string | Echoes the `request_ref` from intake, when one was supplied |

Only **servable** pins count — a pin whose stream has gone is neither
`completed` nor listed in `pinned_paths`.

### Tiers that never materialize

A requested tier that simply does not exist (a 2160p request for a title with no
4K rips) must not hold a request open for ever. Once the scheduler's per-tier
backoff for that tier has saturated at `WISP_TIER_BACKOFF_MAX` — meaning every
aired episode reported "results exist, but none at this resolution", repeatedly,
over days — the tier stops counting toward completion, and `detail` names it:

```json
{
  "state": "completed",
  "pinned_qualities": ["1080p"],
  "detail": "requested scope pinned; gave up on 2160p (no releases found at that quality after repeated checks)"
}
```

The monitor still retries the tier for ever in the background (wisp never
permanently abandons one), so a late 4K release is still pinned — the title just
stops reporting `queued` in the meantime. A title with **nothing** servable
pinned is never `completed`, however hopeless its tiers are.

---

## `GET /metrics`

Prometheus text-format metrics: `wisp_pins`, `wisp_mounted`, `wisp_uptime_seconds`,
`wisp_file_requests_total`, `wisp_link_cache_hits_total`,
`wisp_link_cache_misses_total`, `wisp_reresolves_total`, `wisp_link_cache_entries`.

### Media-server notification delivery

`wisp_notify_deliveries_total{target,result}` counts notification attempts per
configured target (`arr-webhook`, `jellyfin`, `emby`, `plex`). One count per
outbound HTTP request — a coalesced import batch is one attempt covering many
files, so this is a rate over requests, not over files.

`result="success"` means the consumer answered `2xx`. That is **acceptance, not
action**: Silo's Autoscan intake returns `202` and may still discard the event,
and media servers coalesce rapid rescan requests. A high success rate here rules
out delivery as a cause of lost notifications; it does not prove they landed.

`result="failure"` means a transport error or a non-2xx response. Delivery is
best-effort and nothing retries, so these events are lost.

`wisp_notify_dropped_total{target}` counts events discarded before any request
was attempted, and so counted in neither `result`. Only `plex` can drop this way
today: no configured library section covers the changed folder. It is split out
because it is a configuration gap rather than a network failure, and the two
call for different fixes.

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
self-mounting. This endpoint is never gated by `WISP_API_TOKEN`, and no paths,
URLs, or tokens are exposed.

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

These paths are **never** gated by `WISP_API_TOKEN` — gating them would break
both wisp's own mount and any external `rclone mount`. Setting a token does not
stop someone who can reach the port from browsing and streaming the library.
