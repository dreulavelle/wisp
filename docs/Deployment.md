# Deployment

## Self-mount (recommended)

wisp embeds rclone and mounts the library itself — one container, no separate
rclone.

```yaml
services:
  wisp:
    image: ghcr.io/dreulavelle/wisp:latest
    devices:
      - /dev/fuse
    cap_add:
      - SYS_ADMIN
    security_opt:
      - apparmor:unconfined      # some hosts need this for FUSE
    environment:
      WISP_AIOSTREAMS_URL: https://your-aiostreams/stremio/<uuid>/<config>/manifest.json
      WISP_AIOSTREAMS_PASSWORD: your-addon-password
      WISP_MOUNT_PATH: /mnt/wisp
    volumes:
      - ./data:/data
      - /mnt/wisp:/mnt/wisp:rshared
    ports:
      - "8080:8080"
    restart: unless-stopped
```

Then point your media server's library at `/mnt/wisp`.

## HTTP-only (bring your own mount)

Leave `WISP_MOUNT_PATH` unset and mount wisp however you like:

```yaml
services:
  wisp:
    image: ghcr.io/dreulavelle/wisp:latest
    environment:
      WISP_AIOSTREAMS_URL: https://your-aiostreams/stremio/<uuid>/<config>/manifest.json
      WISP_AIOSTREAMS_PASSWORD: your-addon-password
    volumes:
      - ./data:/data
    ports:
      - "8080:8080"
```

```sh
rclone mount :http: /mnt/wisp \
  --http-url http://wisp:8080 --read-only --allow-other --vfs-cache-mode off
```

## Mount propagation (the important part)

For a FUSE mount created *inside* the wisp container to be visible on the host
(and therefore to another container like your media server), propagation must be
shared:

1. On the host, make the mountpoint a shared bind mount **once**:
   ```sh
   mkdir -p /mnt/wisp
   mount --bind /mnt/wisp /mnt/wisp
   mount --make-rshared /mnt/wisp
   ```
2. Bind it into wisp with `:rshared` (as in the compose above).
3. Bind it into the media server with `:rslave` (read-only is fine):
   ```yaml
   volumes:
     - /mnt/wisp:/mnt/wisp:ro,rslave
   ```

Without shared propagation the host sees an empty directory even though wisp
mounted successfully inside its container.

## Requirements

- `/dev/fuse` device + `SYS_ADMIN` capability (FUSE mount). Some hosts also need
  `security_opt: [apparmor:unconfined]`.
- A trusted network — the API is unauthenticated today.
- Your media server container must be able to reach wisp's HTTP port if it
  fetches directly; for the mount, only propagation matters.

## Pointing a media server at the mount

| Server | How |
|--------|-----|
| **Silo** | Create a library with paths `/mnt/wisp/shows` (type shows) and `/mnt/wisp/movies` (type movies). |
| **Plex** | Add a Movies library → `/mnt/wisp/movies`; a TV library → `/mnt/wisp/shows`. |
| **Jellyfin / Emby** | Add libraries pointing at `/mnt/wisp/movies` and `/mnt/wisp/shows`. |

Real bytes mean real probes, so metadata, direct play, transcode, and seeking
all behave like local files. Trigger a scan after content is added.
