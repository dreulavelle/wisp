# Configuration

All configuration is via environment variables.

| Variable | Default | Notes |
|----------|---------|-------|
| `WISP_AIOSTREAMS_URL` | — | **Required.** Your AIOStreams manifest URL: `https://host/stremio/<uuid>/<config>/manifest.json` (or the alias form `.../stremio/u/<alias>/manifest.json`). |
| `WISP_AIOSTREAMS_PASSWORD` | — | Addon password. Paired with the UUID/alias from the URL for Search API auth. If it already contains `uuid:password`, it's used verbatim. |
| `WISP_LISTEN_ADDR` | `:8080` | HTTP bind address. |
| `WISP_DB_PATH` | `/data/wisp.db` | bbolt pin database. Persist this (a volume) to keep your library across restarts. |
| `WISP_MOUNT_PATH` | — | If set, wisp self-mounts the library here (needs `/dev/fuse` + `SYS_ADMIN`). Unset = serve HTTP only and mount it yourself. |
| `WISP_MOUNT_ALLOW_OTHER` | `true` | Expose the mount to other UIDs — needed when a media-server container reads the mount as a different user. |
| `WISP_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. `debug` narrates every serve + the full self-heal path. |

## AIOStreams URL & auth

wisp uses the AIOStreams **Search API** (`/api/v1/search`), which requires the
UUID + password. wisp derives the UUID from the `/stremio/<uuid>/…` path of the
manifest URL and pairs it with `WISP_AIOSTREAMS_PASSWORD`.

> The alias form (`ALIASED_CONFIGURATIONS`) works for the manifest path, but the
> Search API expects the real UUID — use the full `/stremio/<uuid>/<config>/…`
> URL for `WISP_AIOSTREAMS_URL` to be safe.

Because wisp goes through your AIOStreams instance, **all of your AIOStreams
config applies automatically** — provider order, quality/HDR/dub preferences,
debrid vs usenet, filtering. wisp adds no ranking or filtering of its own.

## Persisting data

Mount a volume at `WISP_DB_PATH`'s directory (default `/data`). The pin database
is the whole library — lose it and you'd re-add everything (pins re-resolve
fine, but the list is gone).

## Mount tuning (self-mount mode)

wisp embeds rclone's VFS with sensible defaults for streaming: cache **off**
(pure passthrough, no local disk), a 32 MiB initial read chunk that ramps to
512 MiB for efficient sequential playback while keeping seeks snappy, and a
short directory cache. These aren't env-configurable yet; open an issue if you
need to tune them.

See [Deployment](Deployment.md) for the `/dev/fuse`, `SYS_ADMIN`, and mount
propagation requirements.
