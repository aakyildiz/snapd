# Build context: the snapd repository root.
# snapd's own fake store (tests/lib/fakestore), serving the snaps and signed
# assertions the driver publishes into /srv/store (a volume shared with it).
FROM golang:1-trixie AS build
RUN go install github.com/antithesishq/antithesis-sdk-go/tools/antithesis-go-toolexec@v0.9.0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN ./mkversion.sh "$(cat antithesis/snapd-incus/host/VERSION)"
RUN CGO_ENABLED=1 ANTITHESIS_INSTRUMENT=github.com/snapcore/snapd/tests/lib/fakestore \
    go build -tags withtestkeys -toolexec=antithesis-go-toolexec -o /out/fakestore ./tests/lib/fakestore/cmd/fakestore

FROM debian:trixie-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl jq procps squashfs-tools \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/fakestore /usr/local/bin/fakestore
COPY --from=build /symbols /symbols
RUN mkdir -p /srv/store/asserts /srv/store/channels
# snapd's test-key package generates a small RSA key at init, which Go>=1.24
# refuses unless this is set.
ENV SNAPPY_TESTING=1 SNAPD_DEBUG=1 GODEBUG=rsa1024min=0
CMD ["/usr/local/bin/fakestore", "run", "--dir", "/srv/store", "--addr", "0.0.0.0:11028"]
