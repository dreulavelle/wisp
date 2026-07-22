# Making .strm placeholders installation-id independent (Design A)

## The problem this solves

Placeholder URLs embed Silo's mutable installation id:
`http://127.0.0.1:8080/api/v1/plugins/{installation_id}/resolve/...`

The installation id is minted fresh on every plugin reinstall/upgrade (observed: 4 → 8 → 9).
`.strm` files are durable (written once, live forever). So every upgrade orphans the entire
library, and `RetargetPlaceholders` exists only to paper over it. The durable artifact must
not embed a mutable id.

## Key enabling facts (verified in source)

- The HMAC token signs only the resolve tuple `(mediaType, id, imdb, season, episode, quality)`
  — NOT the URL or installation id (`wisp/internal/plugin/token.go:105` `canonical()`).
  **So changing the base/route changes zero tokens; every existing .strm stays valid.**
- Silo's strm follower treats the placeholder target as an opaque URL and only gates on the
  prefix `/api/v1/plugins/` + loopback + pinned port (`silo-server/internal/strm/resolve.go:33,56`).
  A path like `/api/v1/plugins/by-name/wisp/resolve/...` already passes that gate untouched.
  The strm package has NO dependency on the plugin registry — keep it that way.
- Silo already routes by stable plugin_id + capability elsewhere:
  `Service.ScanSourceClientByPluginID` (`silo-server/internal/plugins/service.go:596`) does
  `ListByPluginID` → filter `capability.Type == "scan_source.v1"` → dispatch. Mirror this for
  http_routes.v1.
- `plugin_installations.plugin_id` is the durable manifest id (`"wisp"`), stable across
  reinstall (`silo-server/migrations/sql/031_plugin_runtime.sql:16`). The numeric `id` is
  `BIGSERIAL` and mutable.
- Wisp already knows its stable id locally: `s.manifest.GetPluginId()` → `"wisp"`
  (`wisp/cmd/silo-plugin-wisp/main.go:320`). It does not need the host to build a stable base.

## Minimal diff

### Silo (`/home/spoked/docker/silo-server`)
1. Add a route beside the existing numeric one at `internal/api/router.go:1610`:
   ```go
   r.HandleFunc("/plugins/by-name/{plugin_id}/*", func(w, r) {
       pluginID := chi.URLParam(r, "plugin_id")
       // resolve current installation via ListByPluginID + http_routes.v1 capability filter,
       // mirroring ScanSourceClientByPluginID's enabled/ambiguity handling
       deps.PluginHTTPProxy.ServeRoute(w, r.WithContext(ctx), installationID, authenticated, admin)
   })
   ```
2. Add `HTTPRoutesInstallationByPluginID(pluginID)` / `ServeRouteByPluginID(...)` on the
   proxy/Service, copying the `ListByPluginID` → capability-`http_routes.v1` → enabled/ambiguity
   logic from `service.go:596-641`. Pick the single enabled installation; 404/409 on genuine
   ambiguity (do not silently guess lowest id).
3. `requestPluginSubpath` (`internal/plugins/http_proxy.go:231`) splits on the first segment
   after `/plugins/` — adjust so it strips `by-name/{plugin_id}` (or match on chi's `*` param).
4. No change to `internal/strm/*` — the prefix gate already accepts `by-name/...`.

### Wisp (`/home/spoked/docker/wisp`)
1. `resolverBase()` (`cmd/silo-plugin-wisp/main.go:283`): return the stable base from the
   manifest id — `fmt.Sprintf("%s/api/v1/plugins/by-name/%s", internalBaseURL, s.manifest.GetPluginId())` —
   dropping the `ListInstalledPlugins`/`installationID()` lookup (main.go:294-311) and the
   `fallbackResolverBase` guess (main.go:359).
2. `placeholder.go` / `token.go`: NO change — `target()` just concatenates the new base;
   tokens are unaffected.
3. `RetargetPlaceholders` (`internal/plugin/migrate.go`): becomes a one-time healer — on first
   Configure after upgrade it rewrites old `.../plugins/{N}/...` bases to the stable
   `.../plugins/by-name/wisp/...` base, then is a permanent no-op (files byte-identical, mtime
   preserved). It already relies on tokens signing the tuple not the address, so it re-bases
   without re-signing.

### Signing
None. HMAC covers the resolve tuple only; the base swap is transparent to Verify and to every
.strm already on disk.

## Edge to decide explicitly
`ListByPluginID` returning multiple ENABLED installations of `wisp` is "ambiguous". For a
resolver, pick the single enabled one (matching `ScanSourceClientByPluginID`'s enabled check at
`service.go:637`) and fail 404/409 on genuine ambiguity — a cleaner failure than the current
lowest-id guess (`main.go:341`).

## Why not Design B (id-free via request_router capability)
request_router.v1 is a REQUEST-TIME path (it writes placeholders); playback resolves via the
http_routes.v1 HTTP proxy. Routing playback through request_router would force the dependency-free
strm follower to grow a coupling to the plugin registry/DB, and targets the wrong capability.
Design A reuses existing patterns with zero new coupling.
