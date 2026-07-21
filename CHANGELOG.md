# Changelog

## [2.0.0](https://github.com/dreulavelle/wisp/compare/v1.6.0...v2.0.0) (2026-07-21)


### ⚠ BREAKING CHANGES

* rewrite Wisp as a Silo plugin with on-demand .strm playback ([#44](https://github.com/dreulavelle/wisp/issues/44))

### Features

* publish a catalog feed so Silo can install and update Wisp itself ([5341537](https://github.com/dreulavelle/wisp/commit/5341537df8a3676cdc2c4bb372716fc204af6a2f))
* rewrite Wisp as a Silo plugin with on-demand .strm playback ([#44](https://github.com/dreulavelle/wisp/issues/44)) ([0e9655b](https://github.com/dreulavelle/wisp/commit/0e9655b60cb4241d15ae9ec5c4759177f1b80cd6))

## [1.6.0](https://github.com/dreulavelle/wisp/compare/v1.5.0...v1.6.0) (2026-07-19)


### Features

* quality policy (1080p floor, 4K opt-in) + unblock request completion ([#41](https://github.com/dreulavelle/wisp/issues/41)) ([3284bcf](https://github.com/dreulavelle/wisp/commit/3284bcf2acf2c1b3124b20cacce9520a45e2e48c))

## [1.5.0](https://github.com/dreulavelle/wisp/compare/v1.4.0...v1.5.0) (2026-07-19)


### Features

* instrument media-server notification delivery ([#39](https://github.com/dreulavelle/wisp/issues/39)) ([155745b](https://github.com/dreulavelle/wisp/commit/155745b9c920bd95cc7a285a2a0e0b41d004b82f))
* optional bearer-token authentication for the API ([#38](https://github.com/dreulavelle/wisp/issues/38)) ([dc99eb7](https://github.com/dreulavelle/wisp/commit/dc99eb787fce1dc2c82b97e210fca06c7ec04618))

## [1.4.0](https://github.com/dreulavelle/wisp/compare/v1.3.2...v1.4.0) (2026-07-19)


### Features

* remove lazy resolution (placeholder pins) ([#35](https://github.com/dreulavelle/wisp/issues/35)) ([309d378](https://github.com/dreulavelle/wisp/commit/309d378030edae485bfefe44249552361b34cc39))
* remove the /api/ws WebSocket endpoint ([#37](https://github.com/dreulavelle/wisp/issues/37)) ([79308f3](https://github.com/dreulavelle/wisp/commit/79308f389f3fba509fae8d1193b66c865f2f9728))


### Performance

* **ci:** cross-compile arm64 instead of emulating it ([#34](https://github.com/dreulavelle/wisp/issues/34)) ([d29468f](https://github.com/dreulavelle/wisp/commit/d29468fabb07e628358821b19d27e6f80f111c23))

## [1.3.2](https://github.com/dreulavelle/wisp/compare/v1.3.1...v1.3.2) (2026-07-19)


### Bug Fixes

* coalesce media-server notifications; add /api/health ([#32](https://github.com/dreulavelle/wisp/issues/32)) ([6dccb3d](https://github.com/dreulavelle/wisp/commit/6dccb3d29f53d3ea76af15a78665bb89f8f59500))

## [1.3.1](https://github.com/dreulavelle/wisp/compare/v1.3.0...v1.3.1) (2026-07-19)


### Bug Fixes

* add WISP_NOTIFY_MOUNT_PATH and deprecate the implicit default ([3b3f7ce](https://github.com/dreulavelle/wisp/commit/3b3f7ce5d128060180901fe6d4fac599414f0bfd))
* announce on-demand placeholder resolution to media servers ([1a45070](https://github.com/dreulavelle/wisp/commit/1a45070e3baf631eba1a667d0b63e24a48ffa05f))
* repair lazy-resolution placeholder lifecycle and ws leaks ([4a8bb94](https://github.com/dreulavelle/wisp/commit/4a8bb94b3b0dddb9745acdaa5c35ec876a2d50c0))
* resolve default-quality naming, ws race, tmdb fallback, and placeholder sizing ([ede4147](https://github.com/dreulavelle/wisp/commit/ede4147fadd2a15a821a5290724c5eba186870e7))
* resolve lazy placeholder retry and document WISP_LAZY_RESOLUTION ([d3d095d](https://github.com/dreulavelle/wisp/commit/d3d095d0121e66e1bb40c2d432e7e468973e8482))

## [1.3.0](https://github.com/dreulavelle/wisp/compare/v1.2.1...v1.3.0) (2026-07-19)


### Features

* add WebSocket route and pinned_paths to status API ([030cc8f](https://github.com/dreulavelle/wisp/commit/030cc8f1014a39812d9539b95124685f3efa8b8b))
* add WISP_LAZY_RESOLUTION placeholder and on-demand streaming ([cc052dc](https://github.com/dreulavelle/wisp/commit/cc052dce63f3c470f32b28ed5df73c21f92efc66))


### Bug Fixes

* change placeholder size to 1 to force VFS read calls to hit the backend ([4035446](https://github.com/dreulavelle/wisp/commit/4035446fe86974c64eaebd21597ebeb4e5e0cbc4))


### Performance

* set DirCacheTime to 0 to disable VFS directory caching for instant file size updates ([d26455e](https://github.com/dreulavelle/wisp/commit/d26455ec2f910cfccbd9e7f1674d3c69fbad84b0))

## [1.2.1](https://github.com/dreulavelle/wisp/compare/v1.2.0...v1.2.1) (2026-07-19)


### Performance

* concurrent candidate probing with a global probe budget ([#28](https://github.com/dreulavelle/wisp/issues/28)) ([7b6e22b](https://github.com/dreulavelle/wisp/commit/7b6e22bb6df43428a6d6fdf40ac20e879dfb5797))

## [1.2.0](https://github.com/dreulavelle/wisp/compare/v1.1.1...v1.2.0) (2026-07-19)


### Features

* **monitor:** back off quality tiers that consistently return no results, so an unsatisfiable tier (e.g. 2160p for a show with no 4K) stops keeping a title on the fast retry cadence — capped, never a hard give-up ([#27](https://github.com/dreulavelle/wisp/issues/27))


### Performance

* parallelize series episode resolution with bounded concurrency, so a season resolves in tens of seconds instead of minutes ([#22](https://github.com/dreulavelle/wisp/issues/22))

## [1.1.1](https://github.com/dreulavelle/wisp/compare/v1.1.0...v1.1.1) (2026-07-19)


### Bug Fixes

* **aiostreams:** self-heal bypasses the search cache ([#21](https://github.com/dreulavelle/wisp/issues/21)) ([705aabf](https://github.com/dreulavelle/wisp/commit/705aabf02e5eeceaeaab2d1d08f191fba979944d))
* force-recheck on refresh + TMDB failure falls back to Cinemeta ([#19](https://github.com/dreulavelle/wisp/issues/19)) ([80bd79d](https://github.com/dreulavelle/wisp/commit/80bd79d6c6f15c45d6c57e7951e0f99d193dec64))


### Performance

* **aiostreams:** serve every quality tier from one Search per unit ([ed5549e](https://github.com/dreulavelle/wisp/commit/ed5549e72051acb59ca48a26db1b5d434a56714c))
* one AIOStreams search serves all requested quality tiers ([#18](https://github.com/dreulavelle/wisp/issues/18)) ([ed5549e](https://github.com/dreulavelle/wisp/commit/ed5549e72051acb59ca48a26db1b5d434a56714c))

## [1.1.0](https://github.com/dreulavelle/wisp/compare/v1.0.0...v1.1.0) (2026-07-19)


### Features

* **monitor:** log why a title can't pin yet instead of retrying silently ([#15](https://github.com/dreulavelle/wisp/issues/15)) ([c6ab8bf](https://github.com/dreulavelle/wisp/commit/c6ab8bf64b56085d9474760d587c7b37c0004e2b))

## [1.0.0](https://github.com/dreulavelle/wisp/compare/v0.7.1...v1.0.0) (2026-07-18)


### ⚠ BREAKING CHANGES

* The POST /api/seerr webhook endpoint and the WISP_SEERR_URL / WISP_SEERR_API_KEY environment variables are removed. Overseerr/Jellyseerr can no longer feed wisp directly; drive requests through Silo (silo-plugin-wisp request_router) or POST /api/add / POST /api/monitors instead.

### Features

* category-aware library layout, request-shaped intake, and status API ([#12](https://github.com/dreulavelle/wisp/issues/12)) ([e95fc58](https://github.com/dreulavelle/wisp/commit/e95fc589ebf7c382506d60fafbfaffb8dd1b7a9a))
* multi-target media-server notifications, /api/schedule, persistence warning ([#10](https://github.com/dreulavelle/wisp/issues/10)) ([ef3d25c](https://github.com/dreulavelle/wisp/commit/ef3d25c9a347cd240eba275fdb2f47ccb9db95a3))
* remove Seerr integration for Silo-native request flow ([#13](https://github.com/dreulavelle/wisp/issues/13)) ([de5aa37](https://github.com/dreulavelle/wisp/commit/de5aa3775c0f8e8d1d7b4e69c83f85b45c516400))


### Documentation

* document VFS read chunk tuning ([#14](https://github.com/dreulavelle/wisp/issues/14)) ([e1dc993](https://github.com/dreulavelle/wisp/commit/e1dc993bf16dde4802f13fc2cb463a2ac0f8179b))

## [0.7.1](https://github.com/dreulavelle/wisp/compare/v0.7.0...v0.7.1) (2026-07-18)


### Bug Fixes

* **seerr:** tolerate empty/number/null ids in webhook payload ([ef3815c](https://github.com/dreulavelle/wisp/commit/ef3815ceedfc8480fc17dc0c48c35d9e270ab6cc))


### Documentation

* be transparent that Seerr creds are optional-but-recommended ([0fdfd81](https://github.com/dreulavelle/wisp/commit/0fdfd81c039a3b627c5653fbe9bb5c53113f2c85))

## [0.7.0](https://github.com/dreulavelle/wisp/compare/v0.6.0...v0.7.0) (2026-07-18)


### Features

* ship compose.yaml with .env support ([2b4f986](https://github.com/dreulavelle/wisp/commit/2b4f986847a10ce012dba0d80a9d8f58802bffef))


### Documentation

* add .env.example with all configuration variables ([6b1fe9e](https://github.com/dreulavelle/wisp/commit/6b1fe9e295d02d2b2a40b8122dc124753793ead9))
* show the exact Seerr webhook URL, events, and JSON payload ([cd53641](https://github.com/dreulavelle/wisp/commit/cd536419b50105349bf02dfff649bc9595cd77c7))

## [0.6.0](https://github.com/dreulavelle/wisp/compare/v0.5.1...v0.6.0) (2026-07-18)


### Features

* **metadata:** release-intelligence foundation (standalone P1) ([38b7227](https://github.com/dreulavelle/wisp/commit/38b72278c2d49c1a3e95618d584d28b078f43341))
* **monitor:** persistent watchlist + air-date-aware scheduler (standalone P2) ([ae2b67c](https://github.com/dreulavelle/wisp/commit/ae2b67c7fdb7ef8d20a44ddba606a1e5a5ab5db7))
* **seerr:** request webhook + API client; graft PR [#5](https://github.com/dreulavelle/wisp/issues/5) monitor observability (P3) ([fb6c183](https://github.com/dreulavelle/wisp/commit/fb6c1831a14fb3925ef9df6b1ecd0f8caf45c18f))
* wire standalone request→pin pipeline into main (P4) ([93ebb6b](https://github.com/dreulavelle/wisp/commit/93ebb6b8ec4f4efb3f7b36015314d9213ef80337))


### Bug Fixes

* address dual-review findings on standalone rebuild ([0e72fa1](https://github.com/dreulavelle/wisp/commit/0e72fa16966b1d939d5f160b0b9312a77e7c50c5))


### Documentation

* clarify AIOStreams needs only URL + password (auth optional if unauthenticated enabled) ([218e213](https://github.com/dreulavelle/wisp/commit/218e213175d122c3f73646bbe8904ecfb7e20da3))
* document Seerr, TMDB, and schedule config vars ([56dea36](https://github.com/dreulavelle/wisp/commit/56dea36c25e4f1b82d642d13c775535e1d5fc26a))
* standalone architecture, Seerr requests, and monitor config ([f721e0c](https://github.com/dreulavelle/wisp/commit/f721e0c14c818a4c8f9213792a9ade5eb1b86781))
