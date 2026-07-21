#!/usr/bin/env python3
"""Generate repository.json, the catalog feed Silo installs and updates from.

Silo resolves a catalog install by looking up binaries[<os>/<arch>], verifying
the downloaded binary against that entry's checksum, and then reading the real
manifest out of the binary itself. The manifest in this file is therefore for
browsing and validation only — which is why it deliberately carries no checksum:
a single one could only ever be right for one architecture.
"""
import hashlib
import json
import os
import sys

# Resolve against the repo root so this works from any working directory.
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIST = os.path.join(ROOT, "dist")
PLUGIN = "silo-plugin-wisp"
PLATFORMS = ["linux/amd64", "linux/arm64"]


# Silo decodes this feed with encoding/json, not protojson, so protobuf enums
# arrive as integers rather than their symbolic names. A string here fails the
# whole document with a type error and the plugin never appears in the catalog.
ADMIN_FORM_CONTROL = {
    "ADMIN_FORM_CONTROL_UNSPECIFIED": 0,
    "ADMIN_FORM_CONTROL_TEXT": 1,
    "ADMIN_FORM_CONTROL_TEXTAREA": 2,
    "ADMIN_FORM_CONTROL_PASSWORD": 3,
    "ADMIN_FORM_CONTROL_NUMBER": 4,
    "ADMIN_FORM_CONTROL_SWITCH": 5,
    "ADMIN_FORM_CONTROL_SELECT": 6,
    "ADMIN_FORM_CONTROL_MULTI_SELECT": 7,
}


def to_snake(name):
    out = []
    for ch in name:
        if ch.isupper():
            out.append("_")
            out.append(ch.lower())
        else:
            out.append(ch)
    return "".join(out)


def snake_keys(value):
    """Convert manifest keys to snake_case.

    Silo reads repository.json with encoding/json straight into the protobuf
    manifest struct, whose tags are snake_case. A camelCase feed parses into an
    empty manifest, fails validation, and the plugin silently never appears in
    the catalog — with no error anywhere to explain why.
    """
    if isinstance(value, dict):
        out = {}
        for k, v in value.items():
            key = to_snake(k)
            if key == "control" and isinstance(v, str):
                out[key] = ADMIN_FORM_CONTROL.get(v, 0)
            else:
                out[key] = snake_keys(v)
        return out
    if isinstance(value, list):
        return [snake_keys(v) for v in value]
    return value


def sha256(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def main():
    repo = os.environ.get("GITHUB_REPOSITORY", "dreulavelle/wisp")
    manifest = json.load(open(os.path.join(ROOT, "cmd", PLUGIN, "manifest.json")))
    tag = sys.argv[1] if len(sys.argv) > 1 else f"v{manifest['version']}"
    # Tests generate a throwaway feed pointing at a local stub. Writing that to
    # the tracked file would publish test URLs to real operators — which is
    # exactly what happened once.
    out = sys.argv[2] if len(sys.argv) > 2 else os.path.join(ROOT, "repository.json")
    base = f"https://github.com/{repo}/releases/download/{tag}"

    # Browsing metadata only. Install reads the manifest from the binary, so a
    # checksum here would be noise at best and wrong for one arch at worst.
    manifest.pop("checksum", None)

    binaries = {}
    for platform in PLATFORMS:
        arch = platform.split("/")[1]
        path = os.path.join(DIST, f"{PLUGIN}-linux-{arch}")
        if not os.path.exists(path):
            sys.exit(f"missing {path} — run `make dist` first")
        binaries[platform] = {
            "url": f"{base}/{PLUGIN}-linux-{arch}",
            "checksum": sha256(path),
        }

    index = {
        "plugins": [
            {
                "manifest": snake_keys(manifest),
                "repo_url": f"https://github.com/{repo}",
                "binaries": binaries,
            }
        ]
    }

    with open(out, "w") as fh:
        json.dump(index, fh, indent=2)
        fh.write("\n")

    print(f"{out} → {tag}")
    for platform, binary in binaries.items():
        print(f"  {platform:14} {binary['checksum'][:16]}...")


if __name__ == "__main__":
    main()
