# Build context: the snapd repository root.
#
# Stage 1: build snapd (instrumented) and its helpers on the same Ubuntu
# release that runs inside the Incus system container, so glibc matches.
FROM ubuntu:24.04 AS build
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      gcc libc6-dev libseccomp-dev xfslibs-dev pkg-config git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=golang:1-trixie /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH GOFLAGS=-mod=mod CGO_ENABLED=1
RUN go install github.com/antithesishq/antithesis-sdk-go/tools/antithesis-go-toolexec@v0.9.0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN ./mkversion.sh "$(cat antithesis/snapd-incus/host/VERSION)"
# snapd itself: coverage-instrumented and with its assertions cataloged. The
# faultinject tag compiles in the fault points of the finding reproductions
# (driver/repro.go); they do nothing unless SNAPD_FAULT_INJECT is set.
# Symbols land in /symbols. cmd/snapd is a multi-call binary: run as "snapd"
# it is the daemon, under any other name it is the snap CLI.
RUN go build -tags withtestkeys,faultinject -toolexec=antithesis-go-toolexec -o /out/snapd ./cmd/snapd \
 && cp /out/snapd /out/snap
# Helpers snapd execs; built from the same tree so versions match.
RUN go build -tags withtestkeys -o /out/snapctl ./cmd/snapctl \
 && cp /out/snapctl /out/snap-exec \
 && go build -tags withtestkeys -o /out/snap-failure ./cmd/snap-failure \
 && go build -tags withtestkeys -o /out/snap-seccomp ./cmd/snap-seccomp
# The host agent: a tiny HTTP service the driver uses for crash injection and
# for reading the container's filesystem state.
RUN cd antithesis/snapd-incus/host/agent && go build -o /out/agent .

# Stage 2: the root filesystem of the Incus system container that runs snapd.
FROM ubuntu:24.04 AS rootfs
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus snapd squashfuse fuse3 squashfs-tools \
      ca-certificates curl jq procps iproute2 less \
    && apt-get clean && rm -rf /var/lib/apt/lists/* /usr/share/doc /usr/share/man /usr/share/locale
COPY --from=build /out/snapd /out/snap-exec /out/snap-failure /out/snap-seccomp /usr/lib/snapd/
COPY --from=build /out/snapctl /usr/bin/
RUN ln -sf /usr/lib/snapd/snapd /usr/bin/snap
COPY antithesis/snapd-incus/host/rootfs/ /
RUN systemctl mask systemd-networkd.service systemd-resolved.service systemd-timesyncd.service \
      apt-daily.timer apt-daily-upgrade.timer motd-news.timer e2scrub_all.timer fstrim.timer \
      systemd-journald-audit.socket snapd.apparmor.service 2>/dev/null; \
    systemctl enable snapd.socket snapd.service; \
    echo snapd > /etc/hostname; true

# Stage 3: the Incus host. Its main process is incusd, under start.sh.
FROM debian:trixie-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      incus-base incus-client ca-certificates curl jq procps iproute2 xz-utils less socat \
    && rm -rf /var/lib/apt/lists/*
COPY --from=rootfs / /opt/rootfs/
COPY antithesis/snapd-incus/host/image-metadata.yaml /opt/image/metadata.yaml
# Kept as a directory: start.sh bind-mounts it as the Incus container's root.
# (Extracting a tarball at runtime fails in Antithesis: its seccomp profile
# refuses fchmodat2, which GNU tar uses through glibc.)
RUN rm -rf /opt/rootfs/proc/* /opt/rootfs/sys/* /opt/rootfs/dev/*
COPY --from=build /symbols /symbols
COPY --from=build /out/agent /usr/local/bin/agent
COPY antithesis/snapd-incus/host/start.sh /usr/local/bin/start.sh
RUN chmod 755 /usr/local/bin/start.sh
CMD ["/usr/local/bin/start.sh"]
