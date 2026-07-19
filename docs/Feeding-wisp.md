# Feeding wisp

wisp is **fed**, not browsed. It has no request UI and no watchlist — it exposes
one endpoint, `POST /api/add`, and anything that can make an HTTP request can
drive it. Pick whatever request flow you already use.

The payload is deliberately generic:

```json
{ "media_type": "movie|series", "imdb_id": "tt…", "title": "…",
  "year": 2010, "season": 1, "episode": 4 }
```

## Options

### 1. Silo's native request router (recommended)

In the Silo-native stack this is the primary path: Silo's request system routes
each approved request to wisp through the
[silo-plugin-wisp](https://github.com/dreulavelle/silo-plugin-wisp)
`request_router` shim, which POSTs the request-shaped `/api/add` with the title's
ids and quality tiers. Series requests are whole-series (Silo's request contract
carries no season scoping) — wisp enumerates the episodes itself. There's no
template to hand-write; the shim owns the mapping, and Silo polls
`GET /api/requests/status` for state.

### 2. Any request tool or webhook

Anything that can fire a webhook can feed wisp. Map the request's IDs to an
`/api/add` call:

```sh
curl -X POST http://wisp:8080/api/add -d "{
  \"media_type\":\"movie\",\"imdb_id\":\"$IMDB\",\"title\":\"$TITLE\",\"year\":$YEAR
}"
```

A small script on a cron works the same way — turn monitored/wanted items into
`/api/add` calls. wisp de-dupes by virtual path, so re-adding is cheap; use
`GET /api/pins` to skip what's already there.

### 3. Directly / by hand

For a quick library or testing, just curl `/api/add`. See the
[API Reference](API-Reference.md).

## Handling "not available yet"

`POST /api/add` returns **`502`** when AIOStreams has no playable stream yet
(unreleased, or no rip). That's a normal "try again later" — not an error. A
good feeder treats 502 as "leave it monitored and re-add next cycle." Once a
stream exists, the same call returns `200` and the file appears. This is the
availability gate for free, without any logic in the feeder.
