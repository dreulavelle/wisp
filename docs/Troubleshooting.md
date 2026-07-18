# Troubleshooting

Set `WISP_LOG_LEVEL=debug` first — it narrates every serve and the full
self-heal path.

## The host (or media server) sees an empty mount

The FUSE mount is created *inside* wisp's container; it only reaches the host
and other containers with shared propagation.

- Make the host mountpoint shared **once**: `mount --bind /mnt/wisp /mnt/wisp && mount --make-rshared /mnt/wisp`.
- Bind it into wisp with `:rshared`, into the media server with `:rslave`.
- After a hard mount loss + remount, a consumer may need to re-observe the
  mount (restart the media-server container). See [Deployment](Deployment.md).

Check `GET /api/status` → `"mounted": true` to confirm wisp's own mount is live.

## `permission denied` reading the mount

The media server runs as a different UID. Keep `WISP_MOUNT_ALLOW_OTHER=true`
(the default) and ensure the host's FUSE allows `allow_other` (it does by
default on modern kernels). The container needs `/dev/fuse` + `SYS_ADMIN`, and
some hosts also need `security_opt: [apparmor:unconfined]`.

## Playback returns 502 / "stream temporarily unavailable"

wisp tried the cached CDN URL, the permalink, and a full re-resolve, and none
returned a playable stream. Usually the title genuinely has no source right now
(unreleased, or pulled). Confirm with a direct search on your AIOStreams
instance for the same `id`. If AIOStreams returns results but wisp still 502s,
check `WISP_AIOSTREAMS_URL`/`WISP_AIOSTREAMS_PASSWORD`.

## `POST /api/add` returns 502 "no playable stream"

Not an error — AIOStreams has nothing to stream yet. Re-add later; a feeder
should treat this as "retry next cycle." See [Feeding wisp](Feeding-wisp.md).

## Slow to start playback

wisp caches the resolved CDN URL after the first read, so the first open pays a
one-time permalink resolve (~1s) and the rest are fast. If *every* read is slow,
the debrid CDN itself is slow, or the media server is re-encoding rather than
copying — check the media server's transcode settings (that's server-side, not
wisp).

## A title has no metadata / poster in the media server

The media server matches by the provider-id tag in the folder name. wisp tags
folders `[tvdb-…]` / `[tmdb-…]` (auto-enriched from Cinemeta when a feeder gives
only an IMDb id). If a title still won't match, its Cinemeta entry may lack a
tvdb/tmdb id — pass `tmdb_id`/`tvdb_id` explicitly on `POST /api/add`.

## Wrong quality label on a file

wisp labels files with the resolution AIOStreams parsed. The label is cosmetic —
the media server reads the real resolution from the file itself — but if it's
off, AIOStreams' parse of that release is the source.

## Nothing plays after a restart

Persist `WISP_DB_PATH` (the pin database) on a volume. Without it, pins are lost
on restart (they re-resolve fine, but the library list is gone).
