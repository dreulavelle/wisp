#!/usr/bin/env bash
#
# End-to-end test: placeholder -> library item -> playback -> redirect.
#
# This is the only test that exercises the seam between the two halves of the
# system. Everything else verifies one side in isolation, but the assumptions
# that actually carry risk live between them:
#
#   * does Silo's scanner create a library item from a .strm at all?
#   * does it survive a scan without the probe-repair loop firing at it?
#   * does playback resolve through the resolver and redirect the client?
#   * does the client end up pointed at a real, external, reachable URL?
#
# A resolver stub stands in for the plugin so this can run without credentials
# and without network access. The contract it implements is the same one the
# real plugin serves, so swapping it out changes nothing here.
#
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

PROJECT="wisp-e2e"
SILO_PORT="${SILO_PORT:-18110}"
STUB_PORT="${STUB_PORT:-18111}"
BASE="http://127.0.0.1:${SILO_PORT}"
ADMIN_USER="e2e"
ADMIN_PASS="e2e-password-123"
ADMIN_EMAIL="e2e@example.invalid"

pass() { printf '\033[32m  PASS\033[0m %s\n' "$1"; }
fail() { printf '\033[31m  FAIL\033[0m %s\n' "$1"; FAILURES=$((FAILURES + 1)); }
step() { printf '\n\033[36m==>\033[0m %s\n' "$1"; }
FAILURES=0

cleanup() {
  step "Tearing down"
  rm -f "$HERE/stub-bin"
  docker compose -p "$PROJECT" down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$HERE/library"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
step "Building resolver stub"
# The stub runs INSIDE the Silo container on loopback, which is exactly where a
# real plugin lives: the host launches plugins as subprocesses and reaches them
# over 127.0.0.1. Running it on the host instead would look external to Silo and
# skip the server-side hop this test exists to verify.
RESOLVED_TARGET="https://cdn.e2e.invalid/movie.mkv?token=e2e"
(cd "$HERE/../.." && CGO_ENABLED=0 go build -o "$HERE/stub-bin" ./test/e2e/stub) \
  && pass "stub built" || { fail "stub build failed"; exit 1; }

# ---------------------------------------------------------------------------
step "Writing .strm placeholders"
rm -rf "$HERE/library"
mkdir -p "$HERE/library/Movies/The Matrix (1999)"
# The placeholder addresses the resolver, never a stream. That is what makes it
# durable: its contents stay valid while the stream behind it expires.
printf 'http://127.0.0.1:%s/api/v1/plugins/1/resolve/movie/tt0133093?quality=1080p\n' "$STUB_PORT" \
  > "$HERE/library/Movies/The Matrix (1999)/The Matrix (1999) [1080p].strm"
pass "placeholder written ($(wc -c < "$HERE/library/Movies/The Matrix (1999)/The Matrix (1999) [1080p].strm") bytes)"

# ---------------------------------------------------------------------------
step "Starting Silo"
docker compose -p "$PROJECT" up -d >/dev/null 2>&1
for _ in $(seq 1 90); do
  curl -sf -o /dev/null "$BASE/api/v1/auth/setup" && break
  sleep 2
done
curl -sf -o /dev/null "$BASE/api/v1/auth/setup" && pass "silo is up" || { fail "silo never became ready"; exit 1; }

step "Starting resolver stub inside the Silo container (127.0.0.1:${STUB_PORT})"
CID="$(docker compose -p "$PROJECT" ps -q silo)"
docker cp "$HERE/stub-bin" "$CID:/tmp/stub" >/dev/null
docker exec -d "$CID" /tmp/stub -addr "127.0.0.1:${STUB_PORT}" -target "$RESOLVED_TARGET"
for _ in $(seq 1 20); do
  docker exec "$CID" sh -c "wget -qO- http://127.0.0.1:${STUB_PORT}/healthz >/dev/null 2>&1 || curl -sf http://127.0.0.1:${STUB_PORT}/healthz >/dev/null 2>&1" && break
  sleep 1
done
docker exec "$CID" sh -c "wget -qO- http://127.0.0.1:${STUB_PORT}/healthz >/dev/null 2>&1 || curl -sf http://127.0.0.1:${STUB_PORT}/healthz >/dev/null 2>&1" \
  && pass "stub reachable on container loopback" || fail "stub not reachable inside the container"

# ---------------------------------------------------------------------------
step "Creating admin and signing in"
curl -sf -X POST "$BASE/api/v1/auth/setup" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$ADMIN_USER\",\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" \
  -o /tmp/wisp-e2e-setup.json 2>/dev/null || true

TOKEN="$(curl -sf -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASS\"}" \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("token") or d.get("access_token") or (d.get("data") or {}).get("token") or "")' 2>/dev/null || true)"

if [[ -z "$TOKEN" ]]; then
  fail "could not obtain an auth token"
  echo "  setup response: $(head -c 300 /tmp/wisp-e2e-setup.json 2>/dev/null)"
  exit 1
fi
pass "authenticated"
AUTH=(-H "Authorization: Bearer $TOKEN")

# ---------------------------------------------------------------------------
step "Creating a library over the placeholder directory"
LIB="$(curl -sf -X POST "$BASE/api/v1/libraries" "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"name":"E2E Movies","type":"movie","paths":["/library/Movies"]}' || true)"
LIB_ID="$(printf '%s' "$LIB" | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin)
    d=d.get("data",d)
    print(d.get("id") or d.get("library",{}).get("id") or "")
except Exception: print("")' 2>/dev/null)"
[[ -n "$LIB_ID" ]] && pass "library created (id=$LIB_ID)" || fail "library creation failed: $(printf '%s' "$LIB" | head -c 200)"

# ---------------------------------------------------------------------------
step "Scanning"
curl -sf -X POST "$BASE/api/v1/scan" "${AUTH[@]}" -H 'Content-Type: application/json' -d '{}' >/dev/null || true

FOUND=""
for _ in $(seq 1 45); do
  FOUND="$(docker compose -p "$PROJECT" exec -T db psql -U silo -d silo -tAc \
    "select file_path from media_files where file_path like '%.strm' limit 1;" 2>/dev/null | tr -d '[:space:]')"
  [[ -n "$FOUND" ]] && break
  sleep 2
done

if [[ -n "$FOUND" ]]; then
  pass "scanner created a media_files row for the placeholder"
  echo "        $FOUND"
else
  fail "scanner never created a row for the .strm placeholder"
fi

# ---------------------------------------------------------------------------
step "Checking the placeholder was NOT probed"
# Probing a placeholder would mean reaching out to the resolver from inside a
# library scan — turning a full scan into a resolution storm.
PROBE="$(docker compose -p "$PROJECT" exec -T db psql -U silo -d silo -tAc \
  "select coalesce(probe_source,'<null>')||'|'||coalesce(duration::text,'<null>') from media_files where file_path like '%.strm' limit 1;" 2>/dev/null | tr -d '[:space:]')"
echo "        probe_source|duration = ${PROBE:-<none>}"
if [[ "$PROBE" == placeholder* ]] || [[ "$PROBE" == *"|0" ]] || [[ "$PROBE" == *"<null>" ]]; then
  pass "placeholder was not probed at scan time"
else
  fail "placeholder appears to have been probed: $PROBE"
fi

# ---------------------------------------------------------------------------
step "Checking the repair loop does not re-queue placeholders"
# Without the probe-repair exemption a placeholder library re-probes itself on
# every scan forever, which is a self-inflicted denial of service against the
# resolver. Scan twice and assert nothing changes.
BEFORE="$(docker compose -p "$PROJECT" exec -T db psql -U silo -d silo -tAc \
  "select count(*) from media_files where file_path like '%.strm';" 2>/dev/null | tr -d '[:space:]')"
curl -sf -X POST "$BASE/api/v1/scan" "${AUTH[@]}" -H 'Content-Type: application/json' -d '{}' >/dev/null || true
sleep 12
AFTER="$(docker compose -p "$PROJECT" exec -T db psql -U silo -d silo -tAc \
  "select count(*) from media_files where file_path like '%.strm';" 2>/dev/null | tr -d '[:space:]')"
if [[ "$BEFORE" == "$AFTER" ]]; then
  pass "rescan left placeholder rows stable ($BEFORE)"
else
  fail "placeholder rows changed across rescans: $BEFORE -> $AFTER"
fi

# ---------------------------------------------------------------------------
step "Playing back the placeholder"
FILE_ID="$(docker compose -p "$PROJECT" exec -T db psql -U silo -d silo -tAc \
  "select id from media_files where file_path like '%.strm' limit 1;" 2>/dev/null | tr -d '[:space:]')"

if [[ -z "$FILE_ID" ]]; then
  fail "no media_file id to play"
else
  # Playback is session-based: start a session for the file, then fetch the
  # stream for that session. The stream fetch is what reaches ServeDirectPlay,
  # which is where the placeholder is recognized and resolved.
  # Playback is profile-scoped. A fresh install has no profile, so create one
  # if needed, then read the id back from the database — the profile API shape
  # is incidental to what this test proves.
  PROFILE_ID="$(docker compose -p "$PROJECT" exec -T db psql -U silo -d silo -tAc \
    "select id from user_profiles limit 1;" 2>/dev/null | tr -d '[:space:]')"
  if [[ -z "$PROFILE_ID" ]]; then
    curl -s -X POST "$BASE/api/v1/profiles" "${AUTH[@]}" -H 'Content-Type: application/json' \
      -d '{"name":"E2E"}' -o /tmp/wisp-e2e-profile.json 2>/dev/null || true
    PROFILE_ID="$(docker compose -p "$PROJECT" exec -T db psql -U silo -d silo -tAc \
      "select id from user_profiles limit 1;" 2>/dev/null | tr -d '[:space:]')"
  fi
  echo "        profile_id=${PROFILE_ID:-<none>}"

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
  fi

  CODE_AND_LOC="$(curl -s -o /dev/null -w '%{http_code}|%{redirect_url}' \
    "${AUTH[@]}" -H "X-Profile-Id: ${PROFILE_ID}" \
    "$BASE/api/v1/stream/$SESSION_ID" 2>/dev/null || true)"
  CODE="${CODE_AND_LOC%%|*}"
  LOC="${CODE_AND_LOC#*|}"
  echo "        HTTP $CODE -> ${LOC:-<none>}"

  if [[ "$CODE" == "302" || "$CODE" == "307" ]]; then
    pass "playback answered with a redirect"
  else
    fail "playback returned $CODE, want 302"
  fi

  if [[ "$LOC" == "$RESOLVED_TARGET" ]]; then
    pass "client was redirected to the resolved external URL"
  elif [[ "$LOC" == *"host.docker.internal"* || "$LOC" == *"127.0.0.1"* ]]; then
    fail "client was pointed at a host-local address it cannot reach: $LOC"
  else
    fail "unexpected redirect target: ${LOC:-<none>}"
  fi
fi

# ---------------------------------------------------------------------------
step "Result"
if [[ "$FAILURES" -eq 0 ]]; then
  printf '\033[32mAll end-to-end checks passed.\033[0m\n'
  exit 0
fi
printf '\033[31m%d check(s) failed.\033[0m\n' "$FAILURES"
exit 1
