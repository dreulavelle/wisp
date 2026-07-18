# Changelog

## [0.6.0](https://github.com/dreulavelle/wisp/compare/wisp-v0.5.1...wisp-v0.6.0) (2026-07-18)


### Features

* add instant Silo autoscan webhooks ([9b6ccc5](https://github.com/dreulavelle/wisp/commit/9b6ccc57e78ad0fa1d2e81ee80e4fc5bac03d688))
* **metadata:** release-intelligence foundation (standalone P1) ([38b7227](https://github.com/dreulavelle/wisp/commit/38b72278c2d49c1a3e95618d584d28b078f43341))
* **monitor:** persistent watchlist + air-date-aware scheduler (standalone P2) ([ae2b67c](https://github.com/dreulavelle/wisp/commit/ae2b67c7fdb7ef8d20a44ddba606a1e5a5ab5db7))
* quality-specific pins and classified add failures (v0.5.0) ([ef7751c](https://github.com/dreulavelle/wisp/commit/ef7751cb4970166b16dcf00c401777e368c140f7))
* **seerr:** request webhook + API client; graft PR [#5](https://github.com/dreulavelle/wisp/issues/5) monitor observability (P3) ([fb6c183](https://github.com/dreulavelle/wisp/commit/fb6c1831a14fb3925ef9df6b1ecd0f8caf45c18f))
* unpin files deleted through the mount ([82a22c5](https://github.com/dreulavelle/wisp/commit/82a22c5b1a3cb3f3d4a91f0573cb49b77c7e777c))
* wire standalone request→pin pipeline into main (P4) ([93ebb6b](https://github.com/dreulavelle/wisp/commit/93ebb6b8ec4f4efb3f7b36015314d9213ef80337))


### Bug Fixes

* address dual-review findings on standalone rebuild ([0e72fa1](https://github.com/dreulavelle/wisp/commit/0e72fa16966b1d939d5f160b0b9312a77e7c50c5))
* classify AIOStreams' 400 bad-credentials as auth, not transient (v0.5.1) ([9778610](https://github.com/dreulavelle/wisp/commit/9778610b2706649dc7546f700f629546af673595))
* quality-tier coexistence + canonical delete match (review round 2) ([f86058d](https://github.com/dreulavelle/wisp/commit/f86058d65599b089dc204d79aebb80fc57c6aa47))
* send complete ARR delete webhook payloads ([5086e14](https://github.com/dreulavelle/wisp/commit/5086e14fdd8ea8287ef48f826488127e68227e93))


### Performance

* cache the resolved CDN URL to skip the per-read permalink hop ([b12165a](https://github.com/dreulavelle/wisp/commit/b12165ab7f85b16078a62e5ad30bf6cc24ef3020))


### Documentation

* architecture, API, configuration, deployment, feeding, troubleshooting ([fcccb50](https://github.com/dreulavelle/wisp/commit/fcccb50f704c72247f2ce4d647b52837bd58f788))
* clarify AIOStreams is required (aggregator gives source breadth) ([ed7f927](https://github.com/dreulavelle/wisp/commit/ed7f927af03b4d51c2ae83aceec04f27a29de5dc))
* clarify AIOStreams needs only URL + password (auth optional if unauthenticated enabled) ([218e213](https://github.com/dreulavelle/wisp/commit/218e213175d122c3f73646bbe8904ecfb7e20da3))
* complete self-mounting stack with :rslave consumer ([b31aa62](https://github.com/dreulavelle/wisp/commit/b31aa62b11c77a09752b66f95765ea572bff6cef))
* document Seerr, TMDB, and schedule config vars ([56dea36](https://github.com/dreulavelle/wisp/commit/56dea36c25e4f1b82d642d13c775535e1d5fc26a))
* drop media-server service from compose, note the volume line instead ([3c32379](https://github.com/dreulavelle/wisp/commit/3c323796a664183e113cd110d8c82be558caf03a))
* single self-mounting compose in quick start ([d44a103](https://github.com/dreulavelle/wisp/commit/d44a103c7eb15b1a49d691c480d953e508976ee0))
* standalone architecture, Seerr requests, and monitor config ([f721e0c](https://github.com/dreulavelle/wisp/commit/f721e0c14c818a4c8f9213792a9ade5eb1b86781))
