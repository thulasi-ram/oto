# The oto image. ONE image runs every mode; the subcommand chooses which
# (`oto api`, `oto worker`, `oto migrate`, `oto bootstrap`, or no argument at all
# for API+worker in one process). The Helm chart in deploy/helm/oto passes the
# subcommand as `args`, which is why the entrypoint is the bare binary.
#
# ⛔ NO SECRET IS BAKED IN AND NONE CAN BE. Every credential oto needs arrives as
# an OTO_* environment variable at runtime; the build copies source and go.sum
# and nothing else.

ARG GO_VERSION=1.26

# ⭐ THE BUILD STAGE STAYS ON THE BUILDER'S ARCHITECTURE AND CROSS-COMPILES.
# `--platform=$BUILDPLATFORM` pins this stage to the machine doing the building;
# `GOARCH=$TARGETARCH` below aims the output at the machine that will run it. The
# release workflow builds linux/amd64 and linux/arm64 from one amd64 runner, and
# without these two lines buildx would emulate the Go compiler under QEMU once
# per architecture to produce the same bytes.
#
# This is only correct BECAUSE CGO IS OFF (see below): a cgo build would need a
# cross toolchain and this would be a lie.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=""
ARG BUILD_DATE=""

# Set by buildkit, one value per requested platform. Empty on a legacy (non
# buildkit) build, which is harmless: the go command treats an empty GOARCH as
# unset and builds for the host.
ARG TARGETARCH

# ⭐ CGO OFF AND `timetzdata` EMBEDDED. The runtime layer is distroless/static:
# it has ca-certificates (oto dials Slack, Prometheus and Alertmanager over TLS)
# and no libc and no zoneinfo, so the timezone database is compiled in rather
# than discovered — a `time.LoadLocation` that fails in production and nowhere
# else is not a failure anyone finds quickly.
#
# The three linker stamps are what GET /api/v1/version answers with. An image
# built without them says "dev", which makes "which build is this" unanswerable
# during an incident.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
      -trimpath \
      -tags timetzdata \
      -ldflags "-s -w \
        -X main.Version=${VERSION} \
        -X main.Commit=${COMMIT} \
        -X main.BuildDate=${BUILD_DATE}" \
      -o /out/oto ./cmd/oto

# ⭐ DISTROLESS STATIC, NONROOT. No shell, no package manager, no writable
# filesystem needed: the binary writes nothing to disk (state is Postgres and
# only Postgres, ADR 0001), which is what lets the chart set
# readOnlyRootFilesystem: true. uid 65532 matches the chart's
# podSecurityContext.runAsUser.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/oto /usr/local/bin/oto

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/oto"]
