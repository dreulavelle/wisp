#!/usr/bin/env bash
#
# End-to-end test: placeholder -> library item -> playback -> redirect.
#
# This is the only test that exercises the seam between the two halves of the
# system. Everything else verifies one side in isolation, but the assumptions
# that carry real risk live between them:
#
#   * does Silo's scanner create a library item from a .strm at all?
#   * does it survive scans without the probe-repair loop firing at it?
#   * does playback reach the plugin and redirect the client?
#   * does the client end up at an external URL, never the host-local address
#     the placeholder itself points at?
#
# Two modes:
#
#   default    Installs the real plugin binary into Silo and drives playback
#              through it. This is the full seam.
#   --stub     Uses a standalone resolver instead of the plugin. Useful for
#              isolating a failure to one side or the other.
#
# In both modes AIOStreams is stubbed. Provider health is exactly the kind of
# external state that turns a regression suite into a coin flip, and the thing
# under test is Wisp, not a debrid service.
#
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
cd "$HERE"

PROJECT="wisp-e2e"
SILO_PORT="${SILO_PORT:-18110}"
STUB_PORT="${STUB_PORT:-18111}"
BASE="http://127.0.0.1:${SILO_PORT}"
ADMIN_USER="e2e"
ADMIN_PASS="e2e-password-123"
ADMIN_EMAIL="e2e@example.invalid"
RESOLVED_TARGET="https://cdn.e2e.invalid/movie.mkv?token=e2e"
AIO_URL="http://127.0.0.1:${STUB_PORT}/stremio/e2e/manifest.json"

MODE="plugin"
case "${1:-}" in
  --stub) MODE="stub" ;;
  --catalog) MODE="catalog" ;;
esac

pass() { printf '\033[32m  PASS\033[0m %s\n' "$1"; }
fail() { printf '\033[31m  FAIL\033[0m %s\n' "$1"; FAILURES=$((FAILURES + 1)); }
skip() { printf '\033[33m  SKIP\033[0m %s\n' "$1"; }
step() { printf '\n\033[36m==>\033[0m %s\n' "$1"; }
FAILURES=0

cleanup() {
  step "Tearing down"
  rm -f "$HERE/stub-bin" "$HERE/fixture-bin" "$HERE/repository.e2e.json"
  # The library is written by the container's user, so empty it from in there
  # while the container still exists — after `down` there is nothing left to
  # exec into and the host cannot remove those files.
  [[ -n "${CID:-}" ]] && docker exec "$CID" sh -c 'rm -rf /library/*' 2>/dev/null || true
  docker compose -p "$PROJECT" down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$HERE/library" 2>/dev/null || true
}
trap cleanup EXIT

psql_q() {
  docker compose -p "$PROJECT" exec -T db psql -U silo -d silo -tAc "$1" 2>/dev/null | tr -d '[:space:]'
}

# ---------------------------------------------------------------------------
step "Building test binaries (mode: $MODE)"
(cd "$REPO" && CGO_ENABLED=0 go build -o "$HERE/stub-bin" ./test/e2e/stub) \
  && pass "stub built" || { fail "stub build failed"; exit 1; }
(cd "$REPO" && CGO_ENABLED=0 go build -o "$HERE/fixture-bin" ./test/e2e/fixture) \
  && pass "fixture built" || { fail "fixture build failed"; exit 1; }

if [[ "$MODE" == "plugin" || "$MODE" == "catalog" ]]; then
  # Catalog installs download per-arch binaries, so dist/ is needed either way.
  (cd "$REPO" && make dist zip >/dev/null 2>&1) \
    && pass "plugin built and packaged" || { fail "plugin packaging failed"; exit 1; }
fi

# ---------------------------------------------------------------------------
step "Starting Silo"
# A previous run's library may be owned by the container's user, so tolerate a
# failure to remove it from the host; the container clears it on teardown.
rm -rf "$HERE/library" 2>/dev/null || true
mkdir -p "$HERE/library"
docker compose -p "$PROJECT" up -d >/dev/null 2>&1
for _ in $(seq 1 90); do
  curl -sf -o /dev/null "$BASE/api/v1/auth/setup" && break
  sleep 2
done
curl -sf -o /dev/null "$BASE/api/v1/auth/setup" && pass "silo is up" || { fail "silo never became ready"; exit 1; }
CID="$(docker compose -p "$PROJECT" ps -q silo)"

# ---------------------------------------------------------------------------
step "Starting AIOStreams stub on container loopback :${STUB_PORT}"
docker cp "$HERE/stub-bin" "$CID:/tmp/stub" >/dev/null
if [[ "$MODE" == "catalog" ]]; then
  # Point the feed at the stub so Silo downloads binaries from it.
  E2E_FEED="$HERE/repository.e2e.json"
  GITHUB_REPOSITORY=e2e/wisp python3 "$REPO/scripts/gen-repository.py" v0.0.0-e2e "$E2E_FEED" >/dev/null
  python3 - "$E2E_FEED" "http://127.0.0.1:${STUB_PORT}/binaries" <<'PY'
import json, sys
path, base = sys.argv[1], sys.argv[2]
d = json.load(open(path))
for name, binary in d["plugins"][0]["binaries"].items():
    binary["url"] = base + "/" + binary["url"].rsplit("/", 1)[1]
json.dump(d, open(path, "w"), indent=2)
PY
  docker cp "$E2E_FEED" "$CID:/tmp/repository.json" >/dev/null
  docker cp "$REPO/dist" "$CID:/tmp/dist" >/dev/null
  docker exec -d -e WISP_E2E_REPOSITORY=/tmp/repository.json -e WISP_E2E_DIST=/tmp/dist \
    "$CID" /tmp/stub -addr "127.0.0.1:${STUB_PORT}" -target "$RESOLVED_TARGET"
else
  docker exec -d "$CID" /tmp/stub -addr "127.0.0.1:${STUB_PORT}" -target "$RESOLVED_TARGET"
fi
# The image ships curl but not wget, so probe with curl.
stub_ready() { docker exec "$CID" curl -sf -o /dev/null --max-time 2 "http://127.0.0.1:${STUB_PORT}/healthz" 2>/dev/null; }
for _ in $(seq 1 20); do stub_ready && break; sleep 1; done
stub_ready && pass "stub reachable on container loopback" \
  || fail "stub not reachable inside the container"

# ---------------------------------------------------------------------------
step "Creating admin and signing in"
curl -sf -X POST "$BASE/api/v1/auth/setup" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$ADMIN_USER\",\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" \
  -o /tmp/wisp-e2e-setup.json 2>/dev/null || true

TOKEN="$(curl -sf -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASS\"}" \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("token") or d.get("access_token") or (d.get("data") or {}).get("token") or "")' 2>/dev/null || true)"

[[ -n "$TOKEN" ]] && pass "authenticated" || { fail "could not obtain an auth token"; exit 1; }
AUTH=(-H "Authorization: Bearer $TOKEN")

# ---------------------------------------------------------------------------
RESOLVER_BASE="http://127.0.0.1:${STUB_PORT}/api/v1/plugins/1"

if [[ "$MODE" == "catalog" ]]; then
  step "Installing the Wisp plugin from a catalog feed"
  REPO_ID="$(curl -s -X POST "$BASE/api/v1/admin/plugins/repositories" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"display_name\":\"E2E\",\"url\":\"http://127.0.0.1:${STUB_PORT}/repository.json\"}" 2>/dev/null \
    | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin); d=d.get("data",d); print(d.get("id",""))
except Exception: print("")' 2>/dev/null)"
  [[ -n "$REPO_ID" ]] && pass "repository added (id=$REPO_ID)" || fail "could not add the repository"

  CATALOG="$(curl -s "$BASE/api/v1/admin/plugins/catalog" "${AUTH[@]}" 2>/dev/null || true)"
  if printf '%s' "$CATALOG" | grep -q '"wisp"'; then
    pass "wisp appears in the catalog"
  else
    fail "wisp is not in the catalog: $(printf '%s' "$CATALOG" | head -c 200)"
  fi

  INSTALL="$(curl -s -X POST "$BASE/api/v1/admin/plugins/installations" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"repository_id\":$REPO_ID,\"plugin_id\":\"wisp\",\"version\":\"2.0.0\"}" 2>/dev/null || true)"
  INSTALL_ID="$(printf '%s' "$INSTALL" | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin); d=d.get("data",d); print(d.get("id",""))
except Exception: print("")' 2>/dev/null)"
  if [[ -n "$INSTALL_ID" ]]; then
    pass "installed from the catalog (installation id=$INSTALL_ID)"
  else
    fail "catalog install failed: $(printf '%s' "$INSTALL" | head -c 300)"
  fi

elif [[ "$MODE" == "plugin" ]]; then
  step "Installing the Wisp plugin"
  UPLOAD="$(curl -s -X POST "$BASE/api/v1/admin/plugins/uploads" "${AUTH[@]}" \
    -F "archive=@$REPO/dist/silo-plugin-wisp.zip" 2>/dev/null || true)"
  INSTALL_ID="$(printf '%s' "$UPLOAD" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin); d=d.get("data",d)
    inst=d.get("installation") or d
    print(inst.get("id") or inst.get("installation_id") or "")
except Exception: print("")' 2>/dev/null)"

  if [[ -z "$INSTALL_ID" ]]; then
    fail "plugin upload failed"
    echo "        response: $(printf '%s' "$UPLOAD" | head -c 400)"
  else
    pass "plugin installed (installation id=$INSTALL_ID)"

    # The endpoint takes one config entry: {"key":..., "value":{...}}, keyed by
    # the configSchema key the manifest declares.
    CFG_CODE="$(curl -s -o /tmp/wisp-e2e-config.json -w '%{http_code}' \
      -X PUT "$BASE/api/v1/admin/plugins/installations/$INSTALL_ID/config" "${AUTH[@]}" \
      -H 'Content-Type: application/json' \
      -d "{\"key\":\"global\",\"value\":{\"aiostreams_url\":\"$AIO_URL\",\"library_path\":\"/library\"}}" 2>/dev/null || true)"
    if [[ "$CFG_CODE" == "200" || "$CFG_CODE" == "204" ]]; then
      pass "plugin configured"
    else
      fail "plugin config returned $CFG_CODE: $(head -c 200 /tmp/wisp-e2e-config.json)"
    fi

    curl -s -X PUT "$BASE/api/v1/admin/plugins/installations/$INSTALL_ID" "${AUTH[@]}" \
      -H 'Content-Type: application/json' -d '{"enabled":true}' >/dev/null 2>&1 || true

    # The dashboard must be reachable before configuration, or the page that
    # explains what to configure cannot be read until it is already configured.
    PRECFG="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/plugins/$INSTALL_ID/admin/" "${AUTH[@]}" 2>/dev/null || true)"
    [[ "$PRECFG" == "200" ]] && pass "dashboard loads before configuration" \
      || fail "dashboard returned $PRECFG before configuration"

    # Configure triggers a plugin restart, so give it a moment to come back.
    for _ in $(seq 1 15); do
      [[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/plugins/$INSTALL_ID/healthz" 2>/dev/null)" == "200" ]] && break
      sleep 1
    done
    HEALTH="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/plugins/$INSTALL_ID/healthz" 2>/dev/null || true)"
    if [[ "$HEALTH" == "200" ]]; then
      pass "plugin is serving through Silo's plugin proxy"
    else
      fail "plugin health via proxy returned $HEALTH"
      docker logs "$CID" 2>&1 | grep -iE "plugin|wisp" | tail -8 | sed 's/^/        /'
    fi
    RESOLVER_BASE="http://127.0.0.1:8080/api/v1/plugins/$INSTALL_ID"

    # The plugin has to work out its OWN installation id: Silo mounts routes at
    # /api/v1/plugins/<id>/ but tells the plugin neither the id nor the prefix.
    # Get this wrong and every placeholder 404s at playback, long after the
    # mistake. Assert it derived the same id Silo actually installed it under —
    # and specifically that it is not the hardcoded fallback of 1.
    CHOSE="$(docker logs "$CID" 2>&1 | grep -o 'resolver_base=[^ ]*' | tail -1 | cut -d= -f2-)"
    if [[ "$CHOSE" == "$RESOLVER_BASE" ]]; then
      pass "plugin discovered its own installation id ($INSTALL_ID)"
    else
      fail "plugin addressed itself at ${CHOSE:-<nothing logged>}, want $RESOLVER_BASE"
    fi
  fi
fi

if [[ "$MODE" == "catalog" && -n "${INSTALL_ID:-}" ]]; then
  CFG_CODE="$(curl -s -o /tmp/wisp-e2e-config.json -w '%{http_code}' \
    -X PUT "$BASE/api/v1/admin/plugins/installations/$INSTALL_ID/config" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"key\":\"global\",\"value\":{\"aiostreams_url\":\"$AIO_URL\",\"library_path\":\"/library\"}}" 2>/dev/null || true)"
  [[ "$CFG_CODE" == "200" || "$CFG_CODE" == "204" ]] && pass "plugin configured" \
    || fail "plugin config returned $CFG_CODE"

  for _ in $(seq 1 15); do
    [[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/plugins/$INSTALL_ID/healthz")" == "200" ]] && break
    sleep 1
  done
  HEALTH="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/plugins/$INSTALL_ID/healthz" || true)"
  [[ "$HEALTH" == "200" ]] && pass "catalog-installed plugin is serving" \
    || fail "catalog-installed plugin health returned $HEALTH"

  RESOLVER_BASE="http://127.0.0.1:8080/api/v1/plugins/$INSTALL_ID"
fi

# ---------------------------------------------------------------------------
step "Writing placeholders with the real writer"
# Produced by production code, not hand-rolled fixture text: a placeholder the
# real writer cannot produce is a bug this test should catch.
# Runs inside the container, as the plugin itself would: the plugin creates the
# library roots on Configure, owned by the container's user, so a host-side
# writer cannot get into them. Writing from in here is also simply more
# faithful — in production nothing outside the container ever writes a
# placeholder.
docker cp "$HERE/fixture-bin" "$CID:/tmp/fixture" >/dev/null
docker exec "$CID" /tmp/fixture -root /library -resolver-base "$RESOLVER_BASE" \
  -aiostreams-url "$AIO_URL" -quality 1080p \
  > /tmp/wisp-e2e-fixtures.txt 2>/tmp/wisp-e2e-fixture-err.txt \
  && pass "wrote $(wc -l < /tmp/wisp-e2e-fixtures.txt) placeholder(s)" \
  || { fail "fixture write failed: $(head -c 200 /tmp/wisp-e2e-fixture-err.txt)"; exit 1; }
sed 's/^/        /' /tmp/wisp-e2e-fixtures.txt

# ---------------------------------------------------------------------------
step "Creating libraries and scanning"
for spec in "E2E Movies|movie|/library/movies" "E2E Shows|series|/library/tv" \
            "E2E Anime Movies|movie|/library/anime/movies" "E2E Anime Shows|series|/library/anime/shows"; do
  IFS='|' read -r name type path <<< "$spec"
  curl -sf -X POST "$BASE/api/v1/libraries" "${AUTH[@]}" -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\",\"type\":\"$type\",\"paths\":[\"$path\"]}" >/dev/null 2>&1 || true
done
LIB_COUNT="$(psql_q "select count(*) from media_folders;")"
[[ "${LIB_COUNT:-0}" -ge 1 ]] && pass "libraries created ($LIB_COUNT)" || fail "no libraries created"

curl -sf -X POST "$BASE/api/v1/scan" "${AUTH[@]}" -H 'Content-Type: application/json' -d '{}' >/dev/null || true

# Wait for EVERY placeholder, not merely the first. Breaking as soon as one row
# appears leaves the count mid-scan, which then makes the rescan-stability check
# below compare a partial baseline against a complete one and report a spurious
# change.
WROTE="$(wc -l < /tmp/wisp-e2e-fixtures.txt)"
FOUND=0
for _ in $(seq 1 45); do
  FOUND="$(psql_q "select count(*) from media_files where file_path like '%.strm';")"
  [[ "${FOUND:-0}" -ge "$WROTE" ]] && break
  sleep 2
done
[[ "${FOUND:-0}" -ge "$WROTE" ]] && pass "scanner created $FOUND row(s) for $WROTE placeholder(s)" \
  || fail "scanner found $FOUND row(s), want $WROTE"

# ---------------------------------------------------------------------------
step "Checking placeholders were NOT probed"
# Probing one would reach out to the resolver from inside a library scan,
# turning a full scan into a resolution storm against the provider.
PROBED="$(psql_q "select count(*) from media_files where file_path like '%.strm' and probe_source is not null and probe_source <> '';")"
[[ "${PROBED:-0}" == "0" ]] && pass "no placeholder was probed at scan time" \
  || fail "$PROBED placeholder(s) were probed"

# ---------------------------------------------------------------------------
step "Checking the repair loop does not re-queue placeholders"
# Without the probe-repair exemption a placeholder library re-probes itself on
# every scan forever: a self-inflicted denial of service against the resolver.
BEFORE="$(psql_q "select count(*) from media_files where file_path like '%.strm';")"
curl -sf -X POST "$BASE/api/v1/scan" "${AUTH[@]}" -H 'Content-Type: application/json' -d '{}' >/dev/null || true
sleep 12
AFTER="$(psql_q "select count(*) from media_files where file_path like '%.strm';")"
[[ "$BEFORE" == "$AFTER" ]] && pass "rescan left placeholder rows stable ($BEFORE)" \
  || fail "placeholder rows changed across rescans: $BEFORE -> $AFTER"

# ---------------------------------------------------------------------------
step "Playing back a placeholder"
FILE_ID="$(psql_q "select id from media_files where file_path like '%.strm' order by id limit 1;")"

PROFILE_ID="$(psql_q "select id from user_profiles limit 1;")"
if [[ -z "$PROFILE_ID" ]]; then
  curl -s -X POST "$BASE/api/v1/profiles" "${AUTH[@]}" -H 'Content-Type: application/json' \
    -d '{"name":"E2E"}' >/dev/null 2>&1 || true
  PROFILE_ID="$(psql_q "select id from user_profiles limit 1;")"
fi

if [[ -z "$FILE_ID" || -z "$PROFILE_ID" ]]; then
  fail "no media file or profile to play"
else
  SESSION_JSON="$(curl -s -X POST "$BASE/api/v1/playback/start" "${AUTH[@]}" \
    -H 'Content-Type: application/json' -H "X-Profile-Id: ${PROFILE_ID}" \
    -d "{\"file_id\":$FILE_ID}" 2>/dev/null || true)"
  SESSION_ID="$(printf '%s' "$SESSION_JSON" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin); d=d.get("data",d)
    print(d.get("session_id") or d.get("sessionId") or "")
except Exception: print("")' 2>/dev/null)"

  if [[ -z "$SESSION_ID" ]]; then
    fail "could not start a playback session"
    echo "        response: $(printf '%s' "$SESSION_JSON" | head -c 300)"
  else
    pass "playback session started"

    RESULT="$(curl -s -o /dev/null -w '%{http_code}|%{redirect_url}' \
      "${AUTH[@]}" -H "X-Profile-Id: ${PROFILE_ID}" \
      "$BASE/api/v1/stream/$SESSION_ID" 2>/dev/null || true)"
    CODE="${RESULT%%|*}"
    LOC="${RESULT#*|}"
    echo "        HTTP $CODE -> ${LOC:-<none>}"

    [[ "$CODE" == "302" || "$CODE" == "307" ]] && pass "playback answered with a redirect" \
      || fail "playback returned $CODE, want 302"

    if [[ "$LOC" == "$RESOLVED_TARGET"* ]]; then
      pass "client was redirected to the resolved external URL"
    elif [[ "$LOC" == *"127.0.0.1"* || "$LOC" == *"localhost"* ]]; then
      fail "client was pointed at a host-local address it cannot reach: $LOC"
    else
      fail "unexpected redirect target: ${LOC:-<none>}"
    fi

    # In plugin mode the redirect proves the whole chain: Silo resolved the
    # host-local hop, the plugin answered it, and the plugin reached AIOStreams.
    if [[ "$MODE" == "plugin" && "$LOC" == *"q=1080p"* ]]; then
      pass "the plugin honoured the requested quality tier"
    elif [[ "$MODE" == "plugin" ]]; then
      skip "quality tier not visible in the redirect target"
    fi
  fi
fi

# ---------------------------------------------------------------------------
step "Result"
if [[ "$FAILURES" -eq 0 ]]; then
  printf '\033[32mAll end-to-end checks passed (mode: %s).\033[0m\n' "$MODE"
  exit 0
fi
printf '\033[31m%d check(s) failed (mode: %s).\033[0m\n' "$FAILURES" "$MODE"
exit 1
