# Wisp

A Silo plugin for on-demand playback. A request becomes a playable library item
immediately; the actual stream is resolved only when someone presses play.

Wisp writes a small `.strm` placeholder into your library, Silo picks it up on
the next scan, and playback resolves through [AIOStreams](https://github.com/Viren070/AIOStreams)
at the moment it is needed.

Because nothing durable ever stores a stream URL, expiring debrid links stop
being a problem: every play resolves fresh.

## Requirements

- A Silo build with `.strm` support — [dreulavelle/silo](https://github.com/dreulavelle/silo)
- An AIOStreams instance with a debrid or usenet service configured, so it
  returns direct links rather than torrent handoffs

There is no container, no mount, and no privileged capability. The plugin runs
as a subprocess inside Silo, and a placeholder is an ordinary text file.

## Install

Add this under **Administration → Plugins → Repositories**:

```
https://raw.githubusercontent.com/dreulavelle/wisp/main/repository.json
```

Then install **Wisp** from the plugin catalog. Silo picks up later versions from
the same feed, so upgrading is a click rather than a rebuild — the release
workflow refreshes that URL on every tag.

<details>
<summary>Manual install instead</summary>

```sh
make zip
```

**Administration → Plugins → Manual Install**, upload
`dist/silo-plugin-wisp.zip`.
</details>

Then fill in:

| Setting | Value |
|---|---|
| AIOStreams URL | The full manifest URL from its configure page |
| AIOStreams Password | Its password, if it has one |
| Library Path | Where Wisp writes placeholders, e.g. `/library`. Must be writable by Silo. Wisp creates `movies/`, `tv/`, `anime/movies/` and `anime/shows/` under it. |

Finally, create a Silo library pointed at that same path.

## How it works

```
request  →  placeholder written  →  autoscan  →  item appears
play     →  Silo reads the .strm  →  plugin resolves  →  302 to the CDN
```

A placeholder addresses the plugin, never a stream:

```
http://127.0.0.1:8080/api/v1/plugins/3/resolve/movie/tmdb:603?imdb=tt0133093&quality=1080p&t=<sig>
```

Its contents never change. That is what makes it durable — the file stays
correct while the stream behind it expires.

### Identity

Movies are identified by TMDB and series by TVDB, in both the resolver path and
the folder name:

```
movies/The Matrix (1999) [tmdb-603]/The Matrix (1999) [2160p].strm
tv/Game of Thrones (2011) [tvdb-121361]/Season 01/... S01E09 [1080p].strm
```

TVDB is the authority media servers agree on for season and episode numbering.
IMDb's episode ordering diverges from it often enough to file episodes under the
wrong season, and the "correct" IMDb id does not always map to the "correct"
TVDB entry.

AIOStreams accepts IMDb ids and nothing else, so the lookup key travels inside
the signed placeholder URL. Resolving it at play time would put two metadata
calls in front of every playback.

### Library roots

Wisp writes into four roots, created under the configured library path when the
plugin is configured — before anything is requested, so there is something to
point a Silo library at:

```
movies/          anime/movies/
tv/              anime/shows/
```

Anime is separated because its season and absolute numbering needs scanner
settings that are wrong for everything else. Classification is deliberately
conservative: it requires the Animation genre **and** a Japanese language or
country signal, so Western animation stays in the general roots. An anime that
is missed still plays perfectly — it just sits alongside everything else.

The category is decided once, when the first placeholder is written, and is then
read back off the path rather than re-derived. A later metadata correction
therefore cannot relocate a title already in someone's library, which a media
server would see as the show vanishing and a new one appearing — taking watch
state with it.

Add one Silo library per root you intend to use.

### Security

The resolver route is public, because the client following a placeholder
redirect is ffmpeg or a browser and neither carries a Silo session. Every
placeholder therefore embeds an HMAC over the exact tuple it addresses,
including quality. Verification happens before any upstream work, and a
rejection is byte-identical to an unknown path so the endpoint cannot be used to
enumerate a library.

The signing key derives from configuration, so placeholder URLs survive
restarts with no secret to persist. Rotating AIOStreams credentials rotates the
key and invalidates existing placeholders — rare, visible, and it fails closed.

## Capabilities

| Capability | What it does |
|---|---|
| `http_routes.v1` | Playback resolver, plus an admin dashboard in Silo's sidebar |
| `scan_source.v1` | Reports new placeholders to autoscan |
| `request_router.v1` | Turns Silo requests into placeholders |
| `scheduled_task.v1` | Writes placeholders for newly aired episodes |

The episode-fill task runs at startup by default. Silo ignores a schedule
declared in a manifest, so set one under the plugin's task settings if you want
it to run periodically. It is idempotent and never contacts the stream provider,
so any cadence is safe.

## Development

```sh
make test      # unit tests
make zip       # build the installable archive
make e2e       # full pipeline against a throwaway Silo (needs Docker)
```

The end-to-end suite installs the real plugin into a real Silo and plays back
through it. It is the only test covering the seam between plugin and host, and
it stubs AIOStreams so provider health cannot turn the suite into a coin flip.

## License

MIT
