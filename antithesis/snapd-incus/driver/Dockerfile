# Build context: the snapd repository root (the driver uses snapd's client).
FROM golang:1-trixie AS build
RUN go install github.com/antithesishq/antithesis-sdk-go/tools/antithesis-go-toolexec@v0.9.0
WORKDIR /src
COPY go.mod go.sum ./
COPY antithesis/snapd-incus/driver/go.mod antithesis/snapd-incus/driver/go.sum antithesis/snapd-incus/driver/
RUN cd antithesis/snapd-incus/driver && go mod download
COPY . .
RUN ./mkversion.sh "$(cat antithesis/snapd-incus/host/VERSION)"
RUN cd antithesis/snapd-incus/driver && CGO_ENABLED=1 \
    ANTITHESIS_INSTRUMENT=snapd-incus/driver \
    go build -toolexec=antithesis-go-toolexec -o /out/driver .

FROM debian:trixie-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      squashfs-tools curl jq ca-certificates procps \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/driver /usr/local/bin/driver
COPY --from=build /symbols /symbols
COPY antithesis/snapd-incus/driver/first_start_workload /opt/antithesis/test/v1/main/first_start_workload
RUN chmod 755 /opt/antithesis/test/v1/main/first_start_workload
CMD ["/usr/local/bin/driver"]
