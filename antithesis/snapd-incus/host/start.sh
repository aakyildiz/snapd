#!/bin/bash
# Main process of an Incus host container. Starts incusd, makes sure the
# "snapd" system container exists and runs, then waits on incusd.
set -u
NAME=snapd
log() { echo "[host $(date +%T)] $*"; }

# cgroup v2: move ourselves out of the root so incus can delegate controllers.
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
  mkdir -p /sys/fs/cgroup/init
  for p in $(cat /sys/fs/cgroup/cgroup.procs); do echo "$p" > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true; done
  for c in $(cat /sys/fs/cgroup/cgroup.controllers); do echo "+$c" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true; done
fi

mkdir -p /var/log/antithesis /var/lib/incus
touch /var/log/antithesis/snapd.log
tail -n0 -F /var/log/antithesis/snapd.log 2>/dev/null | sed -u 's/^/[snapd] /' &

# A killed container leaves stale sockets and pid files behind.
rm -f /var/lib/incus/unix.socket /var/lib/incus/devincus/sock

# Bind mounts don't survive a container restart. Redo the rootfs mount before
# incusd starts, so nothing incusd does on its own finds an empty root.
ROOTFS=/var/lib/incus/storage-pools/default/containers/$NAME/rootfs
if [ -d "$ROOTFS" ]; then
  mountpoint -q "$ROOTFS" || mount --bind /opt/rootfs "$ROOTFS" || { log "cannot bind-mount rootfs"; exit 1; }
fi
/usr/libexec/incus/incusd --group root --logfile /var/log/incus.log &
INCUSD=$!
trap 'kill -TERM $INCUSD; wait $INCUSD; exit 0' TERM INT
until incus admin waitready --timeout=5 2>/dev/null; do
  kill -0 $INCUSD 2>/dev/null || { log "incusd exited"; cat /var/log/incus.log; exit 1; }
  log "waiting for incusd"
done
log "incusd ready"
log "uid_map: $(tr -s ' ' < /proc/self/uid_map); $(grep -E '^(CapEff|Seccomp|NoNewPrivs)' /proc/self/status | tr '\n\t' '  ')"
mkdir -p /var/lib/incus/.chmodtest && chmod 700 /var/lib/incus/.chmodtest && log "chmod test ok" || log "chmod test FAILED"
rmdir /var/lib/incus/.chmodtest 2>/dev/null
pr=$(unshare -m sh -c 'mkdir -p /tmp/prt && mount -t tmpfs t /tmp/prt && mkdir /tmp/prt/old && cd /tmp/prt && pivot_root . old && echo ok' 2>&1 | tail -1)
log "pivot_root probe: $pr"
log "kernel $(uname -r); fuse: $(ls -l /dev/fuse 2>&1); loop: $(ls /dev/loop-control 2>&1); squashfs: $(grep -c squashfs /proc/filesystems)"

if ! incus storage show default >/dev/null 2>&1; then
  cat <<PRESEED | incus admin init --preseed
storage_pools:
- name: default
  driver: dir
profiles:
- name: default
  devices:
    root: {path: /, pool: default, type: disk}
PRESEED
fi

# Existence by config: `incus info` also reads live state, and fails while
# the instance is busy.
if ! incus config show "$NAME" >/dev/null 2>&1; then
  log "creating container"
  # An empty container whose root is the rootfs baked into this image (see
  # the bind mount below): unpacking a tarball fails in Antithesis.
  incus init --empty "$NAME" -c boot.autostart=false -c security.privileged=true -c security.nesting=true \
      -c raw.lxc="lxc.apparmor.profile=unconfined" || exit 1

  # snapd mounts snaps with squashfuse inside containers when /dev/fuse
  # exists; otherwise it uses kernel squashfs on loop devices.
  if [ -c /dev/fuse ]; then
    incus config device add "$NAME" fuse unix-char path=/dev/fuse >/dev/null
  else
    log "no /dev/fuse: snapd will use kernel squashfs"
  fi
  incus config device add "$NAME" api proxy listen=tcp:0.0.0.0:7000 \
      connect=unix:/run/snapd.socket bind=host >/dev/null
  # The fake store: snapd talks to 127.0.0.1:11028 in the container; the
  # proxy forwards to socat on the host, which resolves "store" per connection.
  incus config device add "$NAME" store proxy listen=tcp:127.0.0.1:11028 \
      connect=tcp:127.0.0.1:11029 bind=instance >/dev/null
  incus config device add "$NAME" logs disk source=/var/log/antithesis path=/var/log/antithesis >/dev/null
  if [ -f /usr/lib/libvoidstar.so ]; then
    # Antithesis' runtime library, and the files it talks to the platform
    # through (/opt/antithesis/shared_memory), for snapd's coverage and SDK.
    incus config device add "$NAME" voidstar disk source=/usr/lib/libvoidstar.so \
        path=/usr/lib/libvoidstar.so readonly=true >/dev/null
    # Files under /opt/antithesis are separate mounts (shared_memory is
    # mmapped), so the bind must be recursive or snapd sees empty files.
    [ -d /opt/antithesis ] && incus config device add "$NAME" antithesis disk \
        source=/opt/antithesis path=/opt/antithesis recursive=true >/dev/null
    log "antithesis runtime passed through: $(ls -la /opt/antithesis 2>&1 | tr '\n' ';')"
    log "antithesis mounts: $(grep -E 'antithesis|voidstar' /proc/self/mountinfo | awk '{print $5, $9, $10}' | tr '\n' ';')"
  fi
fi

# start.sh starts the instance itself, after the mount.
incus config set "$NAME" boot.autostart=false 2>/dev/null
mkdir -p "$ROOTFS"
mountpoint -q "$ROOTFS" || mount --bind /opt/rootfs "$ROOTFS" || { log "cannot bind-mount rootfs"; exit 1; }
[ -x "$ROOTFS/sbin/init" ] || { log "rootfs has no /sbin/init"; exit 1; }

if [ -f /usr/lib/libvoidstar.so ]; then
  printf '/usr/lib/libvoidstar.so\n' > /tmp/ld.so.preload
  incus file push /tmp/ld.so.preload "$NAME"/etc/ld.so.preload
fi
socat TCP-LISTEN:11029,bind=127.0.0.1,fork,reuseaddr TCP:store:11028 2>/dev/null &
log "starting container"
for i in 1 2 3 4 5 6; do
  st=$(incus list "$NAME" -c s -f csv 2>/dev/null)
  [ "$st" = RUNNING ] && break
  [ "$st" != STOPPED ] && incus stop --force "$NAME" >/dev/null 2>&1
  incus start "$NAME" 2>&1 && break
  log "start attempt $i failed (state $st)"
  [ "$i" = 6 ] && incus info "$NAME" --show-log | tail -30
  sleep 2
done

if [ -e /opt/antithesis/shared_memory ]; then
  log "shared_memory size outside: $(stat -c %s /opt/antithesis/shared_memory), inside: $(incus exec "$NAME" -- stat -c %s /opt/antithesis/shared_memory 2>&1)"
fi
/usr/local/bin/agent &
log "host up"
wait $INCUSD
log "incusd exited"
