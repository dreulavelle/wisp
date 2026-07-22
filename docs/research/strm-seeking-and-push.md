# .strm seeking + push architecture — research findings

Research date: 2026-07-21. Sources are linked inline; claims marked ⚠️ are single-source
or unverified and should not be built on without checking.

---

## 0. The decisive fact about our topology

Every project surveyed (Riven, zurg, decypharr, mediaflow-proxy, StremThru, jf-resolve,
autopulse) is working *around* a media server it does not control. We control both ends:
Silo is our fork, Wisp is our plugin. Constraints that dominate the ecosystem's designs do
not apply to us:

- Jellyfin never ffprobes `.strm` at scan time → we can probe whenever we like.
- Jellyfin's `/Library/Media/Updated` has a 60s debounce floor and degenerates into a
  whole-library validate for genuinely-new paths (jellyfin#16176, jellyfin#16729, both
  open) → we already push in-process over gRPC.
- Plex cannot read `.strm` at all → irrelevant, Silo is the server.
- Emby has never implemented range support for `.strm` proxying (open since 2015).

So we should fix causes, not symptoms. Most ecosystem workarounds are not worth copying.

---

## 1. Why seeking breaks (general)

Ranked by how often they are the actual cause:

1. **No duration metadata.** An unprobed `.strm` has no runtime. The player cannot build a
   seek bar, and an HLS manifest with no declared total length reads as a *live* stream
   that grows — nothing to seek against. Items also mark themselves watched after seconds.
   (JellySTRMprobe; Emby "Strm Extract"; our own `scanner/probe_repair.go` comment.)
2. **Failed probe → spurious transcode.** `ffprobe failed - streams and format are both
   null` → `ContainerNotSupported, VideoCodecNotSupported` → transcode even when the client
   could direct play. Unknown codecs are treated as unsupported codecs. (jellyfin#11447)
3. **`Accept-Ranges: none` / 200-instead-of-206.** Telling the player seeking is impossible.
   Jellyfin shipped exactly this bug until 10.11.0 (commit `a7891b3f`, PR #14021).
4. **Container index at EOF.** MKV cues and non-faststart MP4 `moov` live at end-of-file, so
   seeking *requires* a range request. jellyfin-androidtv#794 documents same server, same
   upstream, MP4 seeks with proper 206s while MKV "only makes a single request to the
   destination without any content-ranges". Debrid content is overwhelmingly MKV.
5. **Forward-only stream semantics** — see §3.

---

## 2. What upstream must provide for seeking

Derived from Jellyfin's post-fix proxy (`a7891b3f`), not folklore:

| Requirement | Needed | Note |
|---|---|---|
| `Range` on **GET** | Required | HEAD is *not* required; Jellyfin's proxy only issues GET |
| `206 Partial Content` | Required | `upstreamSupportsRange = status == PartialContent` |
| `Content-Range` | Required | forwarded verbatim |
| `Accept-Ranges: bytes` | Strongly preferred | if absent but 206 returned, assume `bytes` |
| `Content-Length` | Required for a seek bar | only set if upstream supplies it |
| Correct `Content-Type` | Yes | fall back to `application/octet-stream`, never `text/plain` |

Chunked transfer-encoding without `Content-Length`, or any 200 answer to a `Range` request,
kills seeking.

---

## 3. Forward seek works, backward seek doesn't — mechanisms

All share one shape: a forward-only stream where forward seeks are faked by discarding
bytes and backward seeks have no equivalent.

- **Buffer-discard without reopen.** rclone's `vfs/read.go`: `ar.SkipBytes(offset - fh.offset)`
  only succeeds for a positive in-buffer delta. Implement this tier alone and you have built
  a forward-only seeker. Note the gap heuristic is explicitly forward-only:
  `if gap := off - fh.offset; gap > 0 && gap < int64(8*maxBuf)`.
- **Origin ignores Range, proxy relabels 200→206 without discarding.** Silent corruption:
  client is told "bytes 500M-501M", receives bytes 0-1M. Forward playback looks fine, every
  seek returns garbage. Requires `io.CopyN(io.Discard, body, start)` on the 200 path.
  ⚠️ Observed by source reading in decypharr `pkg/manager/stream.go:226-252`; not a filed issue.
- **Player latches "non-seekable" from the first response** and never issues a seek request
  at all. Restarting playback appears to fix it — misleading symptom.
- **Read-ahead buffer with no back buffer.** Adobe documented this exact class; the smart-seeking
  fix was maintaining *both* back and forward buffers.
- **Cache keyed on high-water mark with evict-oldest-first** — evicts precisely the earlier
  chunks a backward seek needs.
- **Expired debrid link.** Works on the already-open connection; the first seek needs a new
  connection and 403s. Looks like broken seeking, is broken link refresh.

**rclone's three-tier escalation is the reference fix:** buffer discard → `RangeSeek` on the
pooled connection → full reopen. Implementing only tier 1 gives forward-only seeking;
implementing only tier 3 makes every 2-second scrub cost a TLS handshake.

---

## 4. The 2026 consensus architecture

For a service writing `.strm` files that point at itself:

1. **Proxy bytes; offer redirect as an opt-in flag.** The load-bearing reason is *not*
   seeking — it is that debrid links are IP-locked, and a 302 makes the debrid see the
   client's IP instead of ours. This is why mediaflow-proxy, StremThru, and Comet all exist.
   ⚠️ Caveat for our topology: when Silo remuxes/transcodes server-side, the "client" hitting
   the CDN is our own ffmpeg, so no IP leak. The leak only occurs on true direct play where
   the end-user device follows the 302.
2. **Start from a stateless pass-through.** StremThru's entire range implementation:
   ```go
   copyHeaders(r.Header, request.Header, true)
   response, err := proxyHttpClient.Do(request)
   copyHeaders(response.Header, w.Header(), false)
   w.WriteHeader(response.StatusCode)
   return io.Copy(w, response.Body)
   ```
   A stateless proxy inherits correct seek semantics for free — the player already knows how
   to seek, and every seek arrives as an independent `Range` GET.
3. **Do not chunk, do not read-ahead, do not cache.** Chunking and VFS caches are workarounds
   for *not having the player's Range header*. rclone needs them because FUSE hides seek
   intent. We have the header. Oversized chunks measurably cause the buffering they were
   meant to prevent (rclone forum #47632: 4-10× bandwidth amplification).
4. **Assume the origin is hostile to Range.** Clamp with `io.LimitReader`, synthesize 206 +
   `Content-Range` when upstream returns 200 (and discard the leading bytes), and always set
   `Accept-Ranges: bytes` on a 206 even when upstream omits it — TorBox does omit it, and
   ExoPlayer then refuses to seek.
5. **Normalize degenerate client requests.** Strip empty `Range`/`If-Range`; 416 on
   `bytes=NaN-NaN`; if you auto-added `bytes=0-` and got 206, convert back to 200.
   Send `Accept-Encoding: identity` — compression invalidates byte offsets.
6. **Follow redirects server-side, bounded, draining intermediate bodies.**
7. **Read-idle timeouts only, never total.** A total deadline kills a 3-hour direct play.
8. **Resume transparently** on `ECONNRESET` by re-issuing `Range: bytes=<start+written>-`.
   Do *not* retry 429/509 — pass backpressure to the player.
9. **Put the real filename and extension at the end of the URL.** Players, ffprobe, and
   container detection all sniff the path. AIOStreams does `/${filename}` for this reason.

---

## 5. Go specifics worth knowing

- `http.ServeContent` is the only stdlib path implementing ranges correctly. Empirically
  verified deviations: `bytes=-0` returns 206 with an inverted `Content-Range` (should be
  416) ⚠️; syntactically-invalid ranges get 416 *without* `Content-Range`; 416 responses
  carry no `Accept-Ranges`.
- **Never put gzip middleware in front of a range handler.** `ServeContent` refuses to set
  `Content-Length` when `Content-Encoding` is set, silently killing ranges.
- `httputil.ReverseProxy` **is range-correct by default** — `Range`, `If-Range`,
  `Content-Range`, `Accept-Ranges`, `Content-Length` are all non-hop-by-hop and forwarded
  untouched. But set `FlushInterval` explicitly: a 206 with a real `Content-Length` does not
  get the immediate-flush path.
- Go's client **preserves `Range` across redirects but drops `Authorization`** on cross-host.
  Debrid links authenticate by URL token, so redirect is safe for them.
- `MaxIdleConnsPerHost` defaults to **2**. Every seek is a new upstream GET; past two
  concurrent streams every seek re-handshakes (50-200ms).
- Drain-or-close discipline: abandoning a partially-read 206 without draining discards the
  pooled connection. Heuristic: drain if remaining < ~256KB, else close.
- **Order is always:** resolve upstream → set `Content-Length`/`Content-Range`/
  `Accept-Ranges`/`Content-Type` → `WriteHeader(206)` → copy. Writing the header first
  freezes framing to chunked and makes the 206 unsendable.

---

## 6. Push vs poll

We already push (`internal/plugin/scanpush.go` → `PublishEvent("scan.changes")` →
Silo `internal/scanpush/`), with a ~10min poll as a correctness backstop. Our own design
note is correct: "Push is for latency; poll is for eventual correctness."

Ecosystem reference points, if we ever target external servers:

- **Plex:** `GET /library/sections/{id}/refresh?path=<url-encoded **directory**>` with
  `X-Plex-Token`. Genuinely partial, returns immediately. Must be a directory, must be
  URL-encoded, must be inside a section location (resolve via `GET /library/sections`).
- **Emby:** `POST /Library/Media/Updated`, body `{"Updates":[{"Path":…,"UpdateType":"Created"}]}`.
  Works, seconds.
- **Jellyfin:** not fit for purpose on 10.11.x. `UpdateType` is `string?` and ignored
  entirely — the controller only reads `item.Path`. 60s `LibraryMonitorDelay` floor, and
  `RestartTimer()` on every event means a burst of writes pushes the deadline out
  indefinitely. For genuinely-new paths `FindByPath` walks up to the CollectionFolder and
  validates the whole library.

**Governing insight on scan cost:** full-scan cost ≈ (number of directories) × (per-directory
stat latency). Targeted scan is O(1) in library size; full scan is O(directories) × a latency
you don't control. On a FUSE/WebDAV mount that latency is 10s-100s of ms, so full scans go
to hours. That ratio — not item count — is why push matters.

**Debounce and coalesce.** Riven#9: continuous updates made Plex "loop scanning libraries,
eventually causing Plex to stall or crash". Autoscan's `minimum-age` (10 min default) +
folder batching + dedup is a thundering-herd mitigation by construction. Autoscan itself is
**EOL** (explicit notice in README); the successor is autopulse (Rust, active, v2.0.0 June 2026).

**Never rely on inotify** across FUSE, NFS, CIFS, or Docker Desktop bind mounts. libfuse's
own wiki: "Fsnotify does not work right now with FUSE based filesystems and network
filesystems." NFS/CIFS never report changes made by other clients — the kernel has no
integration to forward watch requests to the server. On Linux + FUSE, host
`mount --make-rshared` + container `rshared`/`rslave` is required or a remount leaves the
container with a stale empty directory.

---

## Key sources

- Jellyfin range fix: commit `a7891b3f`, PR #14021 (10.11.0)
- Jellyfin scan defects: jellyfin#16176, jellyfin#16729 (both open)
- Container/seek split: jellyfin-androidtv#794
- Probe→transcode: jellyfin#11447
- rclone seek tiers: `vfs/read.go`, `fs/chunkedreader/sequential.go`
- rclone chunk anti-optimization: rclone forum #47632
- StremThru proxy core: `internal/shared/http.go`
- mediaflow-proxy range reconciliation: `mediaflow_proxy/handlers.py`, `utils/http_utils.py`
- decypharr 206 synthesis: `pkg/manager/stream.go`
- DebriDav bounded read-ahead: `stream/StreamingService.kt`
- Real-Debrid rate limit (250 req/min): https://api.real-debrid.com/
- Plugin-side targeted scan: github.com/d3v1l1989/targeted-scans
