#!/usr/bin/env bash
# Build the plugin from this working tree and install it into a running Silo.
#
# Development loop. Installing from the published catalog means every change has
# to be committed, released, and downloaded before it can be tested, which is
# far too slow to iterate against a live server — and it makes it easy to test
# a build that is not the code in front of you.
#
# The installation is switched to a manual update policy so Silo's auto-updater
# cannot quietly replace the local build with the last published release
# mid-test. Re-run after any change; it upgrades in place.
#
# Requires SILO_URL and SILO_API_KEY (or a key file at scripts/.silo-key).
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SILO_URL="${SILO_URL:-http://127.0.0.1:8090}"

if [[ -z "${SILO_API_KEY:-}" && -f "$REPO/scripts/.silo-key" ]]; then
  SILO_API_KEY="$(tr -d '[:space:]' < "$REPO/scripts/.silo-key")"
fi
if [[ -z "${SILO_API_KEY:-}" ]]; then
  echo "error: set SILO_API_KEY, or put the key in scripts/.silo-key (gitignored)" >&2
  exit 1
fi

api() {
  local method="$1" path="$2"; shift 2
  curl -fsS -X "$method" -H "Authorization: Bearer $SILO_API_KEY" "$SILO_URL$path" "$@"
}

echo "==> Building"
make -C "$REPO" zip >/dev/null
ARCHIVE="$REPO/dist/silo-plugin-wisp.zip"
VERSION="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["version"])' "$REPO/cmd/silo-plugin-wisp/manifest.json")"
echo "    $ARCHIVE (version $VERSION)"

echo "==> Uploading"
# Silo reads the real manifest out of the binary, so a mismatch between the
# archive manifest and the compiled one is rejected here rather than at runtime.
api POST /api/v1/admin/plugins/uploads -F "archive=@$ARCHIVE" >/dev/null
echo "    installed"

echo "==> Pinning to manual updates"
# Without this the auto-updater replaces the local build with the last published
# release, and the next test silently runs code that is not in this tree.
INSTALL_ID="$(api GET /api/v1/admin/plugins/installations \
  | python3 -c 'import json,sys
d = json.load(sys.stdin)
rows = d if isinstance(d, list) else (d.get("installations") or d.get("items") or [])
print(next((str(r["id"]) for r in rows if r.get("plugin_id") == "wisp"), ""))')"

if [[ -z "$INSTALL_ID" ]]; then
  echo "    warning: could not find the wisp installation; update policy left as-is" >&2
else
  api PUT "/api/v1/admin/plugins/installations/$INSTALL_ID" \
    -H 'Content-Type: application/json' \
    -d '{"update_policy":"manual"}' >/dev/null 2>&1 \
    || echo "    warning: could not set the update policy" >&2
  echo "    installation $INSTALL_ID pinned"
fi

echo "==> Live version"
api GET /api/v1/admin/plugins/installations \
  | python3 -c 'import json,sys
d = json.load(sys.stdin)
rows = d if isinstance(d, list) else (d.get("installations") or d.get("items") or [])
for r in rows:
    if r.get("plugin_id") == "wisp":
        print("    wisp %s  policy=%s  enabled=%s" % (r.get("version"), r.get("update_policy"), r.get("enabled")))'
