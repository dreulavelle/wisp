# End-to-end test

Proves the seam between the two halves of the system. Every other test verifies
one side in isolation; the assumptions that actually carry risk live between
them.

```sh
./test/e2e/run.sh
```

Needs Docker and Go. No credentials, no network access to any provider, and no
privileged containers. Tears itself down completely on exit, including volumes.

## What it asserts

1. Silo's scanner creates a `media_files` row from a `.strm` placeholder.
2. The placeholder is **not** probed at scan time. Probing one would reach out
   to the resolver from inside a library scan, turning a full scan into a
   resolution storm against the upstream provider.
3. A rescan leaves placeholder rows stable. Without the probe-repair exemption a
   placeholder library re-queues itself on every scan forever — a self-inflicted
   denial of service against our own resolver.
4. Playback answers with a redirect.
5. The client is redirected to the **external** URL, never to the host-local
   address the placeholder itself points at.

## Why the stub runs inside the container

The resolver stub is copied into the Silo container and listens on
`127.0.0.1`. That is exactly where a real plugin lives: the host launches
plugins as subprocesses and reaches them over loopback.

Running the stub on the host instead makes its address look external to Silo,
which skips the server-side hop entirely — and that hop is the whole point of
assertion 5. An earlier version of this harness did exactly that and passed
assertion 4 while silently proving nothing about assertion 5.

## Swapping in the real plugin

The stub implements the same contract the plugin serves: `GET` a resolver path,
answer `302` with a `Location`. Replacing it with the real binary changes
nothing in this script.
