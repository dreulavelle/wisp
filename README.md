# wisp

![CI](https://github.com/dreulavelle/wisp/actions/workflows/ci.yml/badge.svg)

A standalone request-to-playback engine for [AIOStreams](https://github.com/Viren070/AIOStreams).

wisp turns the streams AIOStreams selects into ordinary-looking media files. It
never downloads anything — each virtual file's bytes are range-proxied from the
resolved stream on demand. Point a media server at the mount and it scans,
probes, and plays them like local files.

```
media server ──scans/reads──▶ wisp ──resolves via──▶ AIOStreams ──▶ debrid / usenet
   (plays)                (VFS + self-heal)      (finds / ranks / parses)
```

The stack is just **Silo + wisp + AIOStreams** — no `*arr` apps, no download
client, no separate request UI. Silo owns requests, approvals, users, and
availability; wisp owns scheduling, stream resolution, and the virtual library;
AIOStreams owns stream selection. Silo routes each approved request to wisp
through the [silo-plugin-wisp](https://github.com/dreulavelle/silo-plugin-wisp)
`request_router` shim, wisp fulfills it under `/mnt/wisp`, and wisp pings Silo's
autoscan webhook so the file imports the moment it's pinned.

> **AIOStreams is required.** wisp calls its Search API directly, so whatever you
> configure *there* — Torrentio, Comet, MediaFusion, Easynews, your debrid — all
> flows through to wisp. wisp adds no ranking or filtering of its own.

## How it works

- **Request** — Silo routes an approved request to wisp's `/api/add`. wisp pins
  what's available now and **monitors** the rest.
- **Monitor** — a persistent watchlist pins unreleased movies and newly-aired
  episodes as they land, waking near the next airstamp rather than polling.
- **Mount** — wisp self-mounts with embedded rclone; pins appear under four
  category roots that any media server scans.
- **Play** — on open, wisp range-proxies the stream's bytes and re-resolves
  through AIOStreams if a link has died, so playback self-heals.

Because the files carry real bytes, the media server reads real metadata and
owns playback end to end — direct play, transcode, and seeking all work.

## Library layout

wisp always presents four category roots under the mount, even when empty, so a
media server can validate every library path from a fresh install:

```
/mnt/wisp/
├── movies/          shows/
└── anime_movies/    anime_shows/
```

Point Silo at all four, one library per root. A title's category is decided
**once** and then permanent (the root is part of each file's path). Anime is
detected from an explicit `is_anime` flag when present, otherwise from a
conservative Cinemeta heuristic — see [Library layout](docs/Architecture.md#library-layout)
for the full rules.

## Quick start

wisp embeds rclone and self-mounts — one container, no separate rclone process.

```sh
cp .env.example .env      # fill in your AIOStreams URL + password
```

```yaml
services:
  wisp:
    image: ghcr.io/dreulavelle/wisp:latest
    container_name: wisp
    env_file: .env
    volumes:
      - ./data:/data                    # persist the pin + monitor database
      - /mnt/wisp:/mnt/wisp:rshared     # share the mount out to the host
    devices: [/dev/fuse]
    cap_add: [SYS_ADMIN]
    security_opt: [apparmor:unconfined]
    ports: ["8080:8080"]
```

Prepare the host mountpoint once so the FUSE mount propagates to the host (and
into your media-server container):

```sh
mkdir -p /mnt/wisp
mount --bind /mnt/wisp /mnt/wisp && mount --make-rshared /mnt/wisp
```

Then point your media server at `/mnt/wisp`. If it runs in its own container,
bind the mount `:ro,rslave` so it re-sees wisp's mount after a restart.
[Deployment](docs/Deployment.md) covers propagation and surviving reboots; to
serve over HTTP instead and mount it yourself, leave `WISP_MOUNT_PATH` unset.

## Notifying your media server

On every pin, rename, or delete, wisp tells your media server to rescan the
affected folder — so new content appears instantly. Delivery is fire-and-forget;
a slow server never blocks a pin. Configure any combination:

```yaml
# Silo (recommended) — Autoscan → Sources → generate a webhook URL:
WISP_NOTIFY_ARR_WEBHOOK_URL: https://silo.example.com/api/v1/autoscan/webhooks/<secret>
# Jellyfin / Emby — base URL + admin API key:
WISP_NOTIFY_JELLYFIN_URL: http://jellyfin:8096
WISP_NOTIFY_JELLYFIN_API_KEY: <api-key>
# Plex — base URL + token, partial-scans just the changed folder:
WISP_NOTIFY_PLEX_URL: http://plex:32400
WISP_NOTIFY_PLEX_TOKEN: <x-plex-token>
```

> The `ARR` in the Silo variable is the *wire format*, not an integration: wisp
> emits the JSON a Sonarr/Radarr webhook would send, because that's what
> media-server autoscan intakes accept natively. No Radarr or Sonarr involved.

## API

```sh
# Add a movie (pins the best stream now, monitors if unreleased)
curl -X POST http://localhost:8080/api/add \
  -d '{"media_type":"movie","imdb_id":"tt1375666","title":"Inception","year":2010}'

# Request-shaped intake (what the Silo shim calls — returns 202, resolves async)
curl -X POST http://localhost:8080/api/add \
  -d '{"media_type":"movie","tmdb_id":"27205","qualities":[{"id":"2160p","is4k":true}]}'

# Poll a title's state (for a request router)
curl "http://localhost:8080/api/requests/status?media_type=movie&tmdb_id=27205"
```

`/api/add` also drives monitoring directly (`/api/monitors`), lists pins
(`/api/pins`), and exposes the scheduler (`/api/schedule`). Full endpoints,
payloads, and status codes are in the [API Reference](docs/API-Reference.md).

## Configuration

Only `WISP_AIOSTREAMS_URL` and `WISP_AIOSTREAMS_PASSWORD` are required;
everything else has a sensible default. See [`.env.example`](.env.example) for a
commented template and [Configuration](docs/Configuration.md) for every variable.

## Documentation

[Architecture](docs/Architecture.md) · [API Reference](docs/API-Reference.md) ·
[Configuration](docs/Configuration.md) · [Deployment](docs/Deployment.md) ·
[Feeding wisp](docs/Feeding-wisp.md) · [Troubleshooting](docs/Troubleshooting.md)

## License

MIT
