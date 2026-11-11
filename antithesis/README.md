# Antithesis workloads for snapd

## snapd-incus

Tests the snapd daemon (built from this tree, with Antithesis coverage
instrumentation) running inside an **Incus system container** with systemd.
The Incus host is a privileged Docker container (`host/`); the driver
(`driver/`) is a separate container.

```
driver ──HTTP──▶ snapd1 (Docker, privileged: incusd)
                   ├─ proxy :7000 ──▶ /run/snapd.socket in the "snapd" Incus container
                   └─ agent :7001 ──▶ `incus exec`: kill/restart snapd, power-cycle the
                                      container, list /snap, blobs, mount units
```

- `host/Dockerfile` builds snapd (instrumented, `-tags withtestkeys`), an
  Ubuntu 24.04 rootfs with the distro snapd package overlaid by this build,
  and a Debian host with Incus. `host/start.sh` imports the rootfs as an Incus
  image and starts the container. Snaps mount with squashfuse (`/dev/fuse` is
  passed through). In Antithesis, the real `libvoidstar.so` is bind-mounted
  into the Incus container and preloaded, so snapd's coverage and assertions
  reach Antithesis.
- `driver/` is a [hegel](https://hegel.dev) state machine in Go that uses
  snapd's own `client` package. Before setup complete it installs a minimal
  `snapd` snap (classic systems refuse to install anything until one exists,
  and there is no store). Rules: sideload 1–3 generated snaps (per-snap or
  all-snaps transactions, some corrupt, some with a `network` plug), remove
  (whole snap or one revision, with or without purge), revert, enable/disable,
  set `refresh.retain`, abort an in-flight change, and crash snapd
  (SIGKILL, restart, or power-cycling the container). Antithesis adds its own
  faults on top.

### Properties
- `a change that does not complete leaves its snaps as they were`: atomicity,
  including abort and crash.
- `a completed install / remove / revision remove / revert / enable or disable …`:
  each completed change has its effect.
- `a snap only changes through changes that complete`: snapd's state matches
  the driver's model at quiescent points.
- `with no change in progress, the filesystem matches snapd's state`: blobs,
  mounts, mount units, `current` symlinks and data dirs, with no leaks.
- `an accepted change is never lost`.
- `snapd never answers a request with an internal server error`.
- Eventually (faults stop): snapd answers, `every change finishes once faults stop`, and the checks above hold.
- Exploration assertions in snapd: undo handlers, aborted tasks, and startup
  with changes in flight (`overlord/…`).

### Findings
See `STATUS.md` for the nine findings with moments, code references and
suggested fixes. Findings 1, 2 and 5 have their own properties
(`an aborted change that reports Done has taken effect`,
`snapd rejects an update of a disabled snap as a client error`,
`an undone change relinks a snap whose data it did not remove`). Findings 3
and 6-8 show up in the two filesystem properties. Finding 9 shows up in
Antithesis' default `No unexpected crashes: squashfuse`.

### Antithesis environment notes
- Antithesis applies a Docker-style default seccomp profile even to privileged
  containers (no `pivot_root`; `fchmodat2` refused with EPERM). The host
  service sets `security_opt: [seccomp=unconfined]`, and the rootfs is
  bind-mounted rather than unpacked at runtime.
- `/opt/antithesis` must be bind-mounted into the Incus container
  *recursively* (`recursive=true`): `shared_memory` is a separate mount, and
  without it the SDK in snapd crashes with SIGBUS.
- `start.sh` re-mounts the rootfs before incusd starts and disables Incus
  autostart, so the host recovers from Antithesis killing the container.

### Run it
- Local loop: `snapd-incus/scripts/local-run.sh <seconds> <out-dir> [eventually-after-seconds]`
- Launch: `snapd-incus/scripts/launch.sh <max-compute-hours> "<description>"`
  (faults: `high_availability`, driver excluded).
