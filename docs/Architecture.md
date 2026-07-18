# Architecture

## The three layers

```
┌─────────────────────────────────────────────┐
│ media server (Silo / Plex / Jellyfin / Emby) │  plays: plan, transcode, seek
└───────────────┬─────────────────────────────┘
                │ scans + reads files
┌───────────────▼─────────────────────────────┐
│ wisp                                          │  storage: virtual files, self-heal
│  · pin store (bbolt)                          │
│  · range-proxy server                         │
│  · embedded rclone mount (go-fuse)            │
└───────────────┬─────────────────────────────┘
                │ resolves streams
┌───────────────▼─────────────────────────────┐
│ AIOStreams                                    │  brains: find, rank, filter, parse
└───────────────┬─────────────────────────────┘
                │
        debrid CDN / usenet / easynews
```

Each layer does exactly one job. wisp deliberately does **not** download,
transcode, rank, or filter — those already have great homes (debrid, the media
server, AIOStreams). wisp only makes a remote stream look like a local file.

## Lifecycle of a title

1. **Add** — `POST /api/add {media_type, imdb_id, title, year, season?, episode?}`.
   wisp calls the AIOStreams Search API, takes the top result (AIOStreams has
   already ranked by *your* config), and reads its size with one `HEAD`.
2. **Pin** — wisp stores `{virtual_path, source_url, size, …}` in bbolt. The
   `source_url` is the AIOStreams/torrentio resolver permalink — it re-unlocks
   the debrid link on every request, so it doesn't go stale between plays.
3. **Appear** — the pin projects into a `movies/…` or `shows/…/Season NN/…`
   layout. The embedded rclone mount exposes it as a real file with the pinned
   size (so `stat`/scan is instant — no bytes pulled).
4. **Play** — when the media server opens the file, wisp resolves the permalink
   to its CDN URL (cached after the first read — see Performance below) and
   range-proxies the requested bytes. Because these are **real bytes**, the media
   server runs real `ffprobe` (true codecs, duration, subtitles) and owns
   playback: direct play, transcode, and arbitrary seeking all work.

## The self-heal model

The pin's `source_url` is a permalink, not a frozen CDN URL — so it survives the
normal case (debrid re-unlocks each open). When it *does* fail (release DMCA'd,
link 404/410/5xx), wisp:

1. detects the bad upstream **before** committing any bytes to the client,
2. re-resolves through AIOStreams for a fresh stream,
3. persists the new `source_url` + size to the pin, and
4. retries once — the client (and the media server) never sees the failure.

If a size change happens (a different release), the media server simply
re-probes the file. This is why playback is durable without wisp ever
downloading anything.

> Re-resolve only happens **before** the first byte is written. Once a response
> is committed, a mid-stream failure is not retried (that would corrupt the
> stream) — the client reconnects and the next request heals.

## Why "virtual files" and not `.strm`

`.strm` files hand the media server a URL to open. Many servers don't support
them, and the ones that do can't probe real metadata (they see a text file).
wisp presents **real byte-range-readable files**, so:

- every media server works (no `.strm` support required),
- the server reads true codecs/duration/subtitles, and
- direct play + seeking behave exactly like local media.

## Storage: why bbolt

The pin store is a key-value workload with ordered prefix scans (directory
listings). bbolt (a B+tree) fits exactly — point lookups and cursor-seek
listings, no SQL engine, no CGO. It's the same store rclone already embeds.

## Provider-id folder tags

Media servers match a title by its folder name. A famous movie matches by title
alone, but obscure content (anime especially) does not — it lands unmatched with
no metadata or poster. So wisp tags every folder with a provider id the scanner
reads directly:

```
shows/The Villager of Level 999 (2026) [tvdb-467127]/Season 01/…
movies/Inception (2010) [tmdb-27205]/…
```

TVDB is preferred for series, TMDB for movies, IMDb as a universal fallback.
Feeders can pass `tmdb_id`/`tvdb_id`; when only an IMDb id is given, wisp
enriches the tvdb/tmdb id from Cinemeta. The same tags are read by Silo, Plex,
Jellyfin, and Emby.

## Performance: the CDN URL cache

The pinned `source_url` is a permalink whose 302 redirect adds latency on every
request — and ffmpeg's probe makes many small reads at stream start. So on the
first read wisp follows the redirect once, caches the resolved CDN URL per file
(15 min), and serves subsequent reads straight from it. First read ~1s;
everything after ~0.2s. A stale CDN URL falls back to the permalink; a dead
permalink triggers a full re-resolve.

## Durability: mount self-heal

The embedded mount runs under a supervisor. If the FUSE mount exits
unexpectedly or a health check finds it unresponsive, wisp remounts it
automatically with capped backoff — recovery is typically milliseconds.
`GET /api/status` reports live mount health. (Cross-container visibility after a
remount still depends on correct mount propagation — see [Deployment](Deployment.md).)

## What wisp is not

- **Not a downloader** — nothing is ever written to disk; bytes stream on demand.
- **Not a transcoder** — the media server transcodes; wisp serves source bytes.
- **Not a ranker/filter** — AIOStreams decides which stream is best.
- **Not a request UI** — wisp is *fed* (see [Feeding wisp](Feeding-wisp.md)).
- **Not a bundle** — one focused sidecar, not an all-in-one stack.
