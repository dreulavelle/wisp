# Changelog

## [4.0.0](https://github.com/dreulavelle/wisp/compare/v3.1.1...v4.0.0) (2026-07-21)


### ⚠ BREAKING CHANGES

* **plugin:** the aiostreams_password setting is removed. Installs using the alias form of the manifest URL must switch to the full form from the AIOStreams configure page.
* rewrite Wisp as a Silo plugin with on-demand .strm playback ([#44](https://github.com/dreulavelle/wisp/issues/44))
* The POST /api/seerr webhook endpoint and the WISP_SEERR_URL / WISP_SEERR_API_KEY environment variables are removed. Overseerr/Jellyseerr can no longer feed wisp directly; drive requests through Silo (silo-plugin-wisp request_router) or POST /api/add / POST /api/monitors instead.

### Features

* add instant Silo autoscan webhooks ([9b6ccc5](https://github.com/dreulavelle/wisp/commit/9b6ccc57e78ad0fa1d2e81ee80e4fc5bac03d688))
* add WebSocket route and pinned_paths to status API ([030cc8f](https://github.com/dreulavelle/wisp/commit/030cc8f1014a39812d9539b95124685f3efa8b8b))
* add WISP_LAZY_RESOLUTION placeholder and on-demand streaming ([cc052dc](https://github.com/dreulavelle/wisp/commit/cc052dce63f3c470f32b28ed5df73c21f92efc66))
* **admin:** show titles on the dashboard instead of files ([56621ee](https://github.com/dreulavelle/wisp/commit/56621ee41b4014bba7d0b0eb57b408511d3b51ae))
* **admin:** show where a resolve actually spent its time ([1de0926](https://github.com/dreulavelle/wisp/commit/1de0926c344c5646213392fdb37ea1f563901578))
* category-aware library layout, request-shaped intake, and status API ([#12](https://github.com/dreulavelle/wisp/issues/12)) ([e95fc58](https://github.com/dreulavelle/wisp/commit/e95fc589ebf7c382506d60fafbfaffb8dd1b7a9a))
* instrument media-server notification delivery ([#39](https://github.com/dreulavelle/wisp/issues/39)) ([155745b](https://github.com/dreulavelle/wisp/commit/155745b9c920bd95cc7a285a2a0e0b41d004b82f))
* **metadata:** release-intelligence foundation (standalone P1) ([38b7227](https://github.com/dreulavelle/wisp/commit/38b72278c2d49c1a3e95618d584d28b078f43341))
* **monitor:** log why a title can't pin yet instead of retrying silently ([#15](https://github.com/dreulavelle/wisp/issues/15)) ([c6ab8bf](https://github.com/dreulavelle/wisp/commit/c6ab8bf64b56085d9474760d587c7b37c0004e2b))
* **monitor:** persistent watchlist + air-date-aware scheduler (standalone P2) ([ae2b67c](https://github.com/dreulavelle/wisp/commit/ae2b67c7fdb7ef8d20a44ddba606a1e5a5ab5db7))
* multi-target media-server notifications, /api/schedule, persistence warning ([#10](https://github.com/dreulavelle/wisp/issues/10)) ([ef3d25c](https://github.com/dreulavelle/wisp/commit/ef3d25c9a347cd240eba275fdb2f47ccb9db95a3))
* optional bearer-token authentication for the API ([#38](https://github.com/dreulavelle/wisp/issues/38)) ([dc99eb7](https://github.com/dreulavelle/wisp/commit/dc99eb787fce1dc2c82b97e210fca06c7ec04618))
* **plugin:** drop the AIOStreams password field ([48db6e9](https://github.com/dreulavelle/wisp/commit/48db6e9e5e2d1b458d9abf746ebd68aad9f72921))
* **plugin:** split anime into its own library roots ([1d3d793](https://github.com/dreulavelle/wisp/commit/1d3d7939d50202c35431b061402e44390b40eedc))
* **plugin:** tell Silo about new placeholders instead of waiting to be polled ([4b15f91](https://github.com/dreulavelle/wisp/commit/4b15f91ef1081048e9e74f3900ae4164450d402c))
* publish a catalog feed so Silo can install and update Wisp itself ([5341537](https://github.com/dreulavelle/wisp/commit/5341537df8a3676cdc2c4bb372716fc204af6a2f))
* quality policy (1080p floor, 4K opt-in) + unblock request completion ([#41](https://github.com/dreulavelle/wisp/issues/41)) ([3284bcf](https://github.com/dreulavelle/wisp/commit/3284bcf2acf2c1b3124b20cacce9520a45e2e48c))
* quality-specific pins and classified add failures (v0.5.0) ([ef7751c](https://github.com/dreulavelle/wisp/commit/ef7751cb4970166b16dcf00c401777e368c140f7))
* remove lazy resolution (placeholder pins) ([#35](https://github.com/dreulavelle/wisp/issues/35)) ([309d378](https://github.com/dreulavelle/wisp/commit/309d378030edae485bfefe44249552361b34cc39))
* remove Seerr integration for Silo-native request flow ([#13](https://github.com/dreulavelle/wisp/issues/13)) ([de5aa37](https://github.com/dreulavelle/wisp/commit/de5aa3775c0f8e8d1d7b4e69c83f85b45c516400))
* remove the /api/ws WebSocket endpoint ([#37](https://github.com/dreulavelle/wisp/issues/37)) ([79308f3](https://github.com/dreulavelle/wisp/commit/79308f389f3fba509fae8d1193b66c865f2f9728))
* resolve series episodes with bounded concurrency ([93951ee](https://github.com/dreulavelle/wisp/commit/93951eeb57040f00a583cfb31fac049e36c16c28))
* rewrite Wisp as a Silo plugin with on-demand .strm playback ([#44](https://github.com/dreulavelle/wisp/issues/44)) ([0e9655b](https://github.com/dreulavelle/wisp/commit/0e9655b60cb4241d15ae9ec5c4759177f1b80cd6))
* **seerr:** request webhook + API client; graft PR [#5](https://github.com/dreulavelle/wisp/issues/5) monitor observability (P3) ([fb6c183](https://github.com/dreulavelle/wisp/commit/fb6c1831a14fb3925ef9df6b1ecd0f8caf45c18f))
* ship compose.yaml with .env support ([2b4f986](https://github.com/dreulavelle/wisp/commit/2b4f986847a10ce012dba0d80a9d8f58802bffef))
* unpin files deleted through the mount ([82a22c5](https://github.com/dreulavelle/wisp/commit/82a22c5b1a3cb3f3d4a91f0573cb49b77c7e777c))
* wire standalone request→pin pipeline into main (P4) ([93ebb6b](https://github.com/dreulavelle/wisp/commit/93ebb6b8ec4f4efb3f7b36015314d9213ef80337))


### Bug Fixes

* add WISP_NOTIFY_MOUNT_PATH and deprecate the implicit default ([3b3f7ce](https://github.com/dreulavelle/wisp/commit/3b3f7ce5d128060180901fe6d4fac599414f0bfd))
* address dual-review findings on standalone rebuild ([0e72fa1](https://github.com/dreulavelle/wisp/commit/0e72fa16966b1d939d5f160b0b9312a77e7c50c5))
* **aiostreams:** self-heal bypasses the search cache ([#21](https://github.com/dreulavelle/wisp/issues/21)) ([705aabf](https://github.com/dreulavelle/wisp/commit/705aabf02e5eeceaeaab2d1d08f191fba979944d))
* announce on-demand placeholder resolution to media servers ([1a45070](https://github.com/dreulavelle/wisp/commit/1a45070e3baf631eba1a667d0b63e24a48ffa05f))
* change placeholder size to 1 to force VFS read calls to hit the backend ([4035446](https://github.com/dreulavelle/wisp/commit/4035446fe86974c64eaebd21597ebeb4e5e0cbc4))
* classify AIOStreams' 400 bad-credentials as auth, not transient (v0.5.1) ([9778610](https://github.com/dreulavelle/wisp/commit/9778610b2706649dc7546f700f629546af673595))
* coalesce media-server notifications; add /api/health ([#32](https://github.com/dreulavelle/wisp/issues/32)) ([6dccb3d](https://github.com/dreulavelle/wisp/commit/6dccb3d29f53d3ea76af15a78665bb89f8f59500))
* enforce and persist per-tier backoff ([09b166c](https://github.com/dreulavelle/wisp/commit/09b166c1a385da60a9e8c8c9c548fe12d2a60b81))
* force-recheck on refresh + TMDB failure falls back to Cinemeta ([#19](https://github.com/dreulavelle/wisp/issues/19)) ([80bd79d](https://github.com/dreulavelle/wisp/commit/80bd79d6c6f15c45d6c57e7951e0f99d193dec64))
* **monitor:** enforce and persist per-tier backoff ([#27](https://github.com/dreulavelle/wisp/issues/27)) ([09b166c](https://github.com/dreulavelle/wisp/commit/09b166c1a385da60a9e8c8c9c548fe12d2a60b81))
* **plugin:** address placeholders at this plugin's real installation id ([66f28ec](https://github.com/dreulavelle/wisp/commit/66f28ec095e37c534ec2d6eb57f59df1766edd00))
* **plugin:** authenticate from the URL and stop deriving the signing key from credentials ([115a220](https://github.com/dreulavelle/wisp/commit/115a2203325dbd8207f893cd7573803351b055e6))
* **plugin:** heal placeholders that address a retired installation id ([494a4b1](https://github.com/dreulavelle/wisp/commit/494a4b1b720ba809d1e3a3f47de5dbe221cef271))
* **plugin:** make the episode monitor actually run ([eaa8901](https://github.com/dreulavelle/wisp/commit/eaa890197489d3fb6c96392cc628751a410c9eb3))
* **plugin:** only hand back a stream that is actually serving ([73be98c](https://github.com/dreulavelle/wisp/commit/73be98c889d82fa87d65807c413a54f44e02a8a4))
* **plugin:** refuse to create a library root that does not exist ([a7bf64e](https://github.com/dreulavelle/wisp/commit/a7bf64e0b91384c5cabefa053c1133b82cb959ac))
* **plugin:** serve the dashboard before configuration ([0840eee](https://github.com/dreulavelle/wisp/commit/0840eeec162276c7bd4e03ecf0d4b751542429d1))
* **plugin:** stop autoscan going silently deaf after a restart ([0751d02](https://github.com/dreulavelle/wisp/commit/0751d021b614e4d502d478ce9e68a458a432dd85))
* quality-tier coexistence + canonical delete match (review round 2) ([f86058d](https://github.com/dreulavelle/wisp/commit/f86058d65599b089dc204d79aebb80fc57c6aa47))
* **release:** create the release as a draft so artifacts can be attached ([151dafe](https://github.com/dreulavelle/wisp/commit/151dafe4ac6f5adb84360767b03686ab314d279c))
* **release:** publish plugin artifacts and stop shipping test URLs ([89ec108](https://github.com/dreulavelle/wisp/commit/89ec10849771a76f13d117a57c7ed4b411975b68))
* repair lazy-resolution placeholder lifecycle and ws leaks ([4a8bb94](https://github.com/dreulavelle/wisp/commit/4a8bb94b3b0dddb9745acdaa5c35ec876a2d50c0))
* resolve default-quality naming, ws race, tmdb fallback, and placeholder sizing ([ede4147](https://github.com/dreulavelle/wisp/commit/ede4147fadd2a15a821a5290724c5eba186870e7))
* resolve lazy placeholder retry and document WISP_LAZY_RESOLUTION ([d3d095d](https://github.com/dreulavelle/wisp/commit/d3d095d0121e66e1bb40c2d432e7e468973e8482))
* **seerr:** tolerate empty/number/null ids in webhook payload ([ef3815c](https://github.com/dreulavelle/wisp/commit/ef3815ceedfc8480fc17dc0c48c35d9e270ab6cc))
* send complete ARR delete webhook payloads ([5086e14](https://github.com/dreulavelle/wisp/commit/5086e14fdd8ea8287ef48f826488127e68227e93))


### Performance

* **admin:** stop re-fetching the whole library every five seconds ([55545d2](https://github.com/dreulavelle/wisp/commit/55545d2d9ca1be506abe15b69bc58b7bdda8849f))
* **aiostreams:** serve every quality tier from one Search per unit ([ed5549e](https://github.com/dreulavelle/wisp/commit/ed5549e72051acb59ca48a26db1b5d434a56714c))
* cache the resolved CDN URL to skip the per-read permalink hop ([b12165a](https://github.com/dreulavelle/wisp/commit/b12165ab7f85b16078a62e5ad30bf6cc24ef3020))
* **ci:** cross-compile arm64 instead of emulating it ([#34](https://github.com/dreulavelle/wisp/issues/34)) ([d29468f](https://github.com/dreulavelle/wisp/commit/d29468fabb07e628358821b19d27e6f80f111c23))
* concurrent candidate probing with a global probe budget ([#28](https://github.com/dreulavelle/wisp/issues/28)) ([7b6e22b](https://github.com/dreulavelle/wisp/commit/7b6e22bb6df43428a6d6fdf40ac20e879dfb5797))
* one AIOStreams search serves all requested quality tiers ([#18](https://github.com/dreulavelle/wisp/issues/18)) ([ed5549e](https://github.com/dreulavelle/wisp/commit/ed5549e72051acb59ca48a26db1b5d434a56714c))
* parallelize series episode resolution (bounded) ([#22](https://github.com/dreulavelle/wisp/issues/22)) ([93951ee](https://github.com/dreulavelle/wisp/commit/93951eeb57040f00a583cfb31fac049e36c16c28))
* **plugin:** answer a playback session's re-resolve storm from memory ([ebaf1e8](https://github.com/dreulavelle/wisp/commit/ebaf1e838221c170ca2a2eca84621dfb0e0c91af))
* set DirCacheTime to 0 to disable VFS directory caching for instant file size updates ([d26455e](https://github.com/dreulavelle/wisp/commit/d26455ec2f910cfccbd9e7f1674d3c69fbad84b0))

## [3.1.1](https://github.com/dreulavelle/wisp/compare/v3.1.0...v3.1.1) (2026-07-21)


### Bug Fixes

* **plugin:** heal placeholders that address a retired installation id ([494a4b1](https://github.com/dreulavelle/wisp/commit/494a4b1b720ba809d1e3a3f47de5dbe221cef271))

## [3.1.0](https://github.com/dreulavelle/wisp/compare/v3.0.0...v3.1.0) (2026-07-21)


### Features

* **admin:** show titles on the dashboard instead of files ([56621ee](https://github.com/dreulavelle/wisp/commit/56621ee41b4014bba7d0b0eb57b408511d3b51ae))
* **admin:** show where a resolve actually spent its time ([1de0926](https://github.com/dreulavelle/wisp/commit/1de0926c344c5646213392fdb37ea1f563901578))
* **plugin:** tell Silo about new placeholders instead of waiting to be polled ([4b15f91](https://github.com/dreulavelle/wisp/commit/4b15f91ef1081048e9e74f3900ae4164450d402c))


### Bug Fixes

* **plugin:** make the episode monitor actually run ([eaa8901](https://github.com/dreulavelle/wisp/commit/eaa890197489d3fb6c96392cc628751a410c9eb3))
* **plugin:** only hand back a stream that is actually serving ([73be98c](https://github.com/dreulavelle/wisp/commit/73be98c889d82fa87d65807c413a54f44e02a8a4))
* **plugin:** refuse to create a library root that does not exist ([a7bf64e](https://github.com/dreulavelle/wisp/commit/a7bf64e0b91384c5cabefa053c1133b82cb959ac))


### Performance

* **admin:** stop re-fetching the whole library every five seconds ([55545d2](https://github.com/dreulavelle/wisp/commit/55545d2d9ca1be506abe15b69bc58b7bdda8849f))
* **plugin:** answer a playback session's re-resolve storm from memory ([ebaf1e8](https://github.com/dreulavelle/wisp/commit/ebaf1e838221c170ca2a2eca84621dfb0e0c91af))

## [3.0.0](https://github.com/dreulavelle/wisp/compare/v2.1.0...v3.0.0) (2026-07-21)


### ⚠ BREAKING CHANGES

* **plugin:** the aiostreams_password setting is removed. Installs using the alias form of the manifest URL must switch to the full form from the AIOStreams configure page.

### Features

* **plugin:** drop the AIOStreams password field ([48db6e9](https://github.com/dreulavelle/wisp/commit/48db6e9e5e2d1b458d9abf746ebd68aad9f72921))


### Bug Fixes

* **plugin:** authenticate from the URL and stop deriving the signing key from credentials ([115a220](https://github.com/dreulavelle/wisp/commit/115a2203325dbd8207f893cd7573803351b055e6))

## [2.1.0](https://github.com/dreulavelle/wisp/compare/v2.0.3...v2.1.0) (2026-07-21)


### Features

* **plugin:** split anime into its own library roots ([1d3d793](https://github.com/dreulavelle/wisp/commit/1d3d7939d50202c35431b061402e44390b40eedc))


### Bug Fixes

* **plugin:** address placeholders at this plugin's real installation id ([66f28ec](https://github.com/dreulavelle/wisp/commit/66f28ec095e37c534ec2d6eb57f59df1766edd00))

## [2.0.3](https://github.com/dreulavelle/wisp/compare/v2.0.2...v2.0.3) (2026-07-21)


### Bug Fixes

* **plugin:** serve the dashboard before configuration ([0840eee](https://github.com/dreulavelle/wisp/commit/0840eeec162276c7bd4e03ecf0d4b751542429d1))
* **plugin:** stop autoscan going silently deaf after a restart ([0751d02](https://github.com/dreulavelle/wisp/commit/0751d021b614e4d502d478ce9e68a458a432dd85))

## [2.0.2](https://github.com/dreulavelle/wisp/compare/v2.0.1...v2.0.2) (2026-07-21)


### Bug Fixes

* **release:** create the release as a draft so artifacts can be attached ([151dafe](https://github.com/dreulavelle/wisp/commit/151dafe4ac6f5adb84360767b03686ab314d279c))

## [2.0.1](https://github.com/dreulavelle/wisp/compare/v2.0.0...v2.0.1) (2026-07-21)


### Bug Fixes

* **release:** publish plugin artifacts and stop shipping test URLs ([89ec108](https://github.com/dreulavelle/wisp/commit/89ec10849771a76f13d117a57c7ed4b411975b68))

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
