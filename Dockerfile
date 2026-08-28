# The oto image. ONE image runs every mode; the subcommand chooses which
# (`oto api`, `oto worker`, `oto migrate`, `oto bootstrap`, or no argument at all
# for API+worker in one process). The Helm chart in deploy/helm/oto passes the
# subcommand as `args`, which is why the entrypoint is the bare binary.
#
# ⛔ NO SECRET IS BAKED IN AND NONE CAN BE. Every credential oto needs arrives as
# an OTO_* environment variable at runtime; the build copies source and go.sum
# and nothing else.

ARG GO_VERSION=1.26
# ⚠️ THE SAME MAJOR ci.yml PINS (`NODE_VERSION: "24"`). A UI built here under a
# different major than the one `just ui-build` and ci's `ui` job tested is an
# artifact no gate has actually checked — the same drift the ui stage's use of
# `npm run build` (rather than a faster bare `vite build`) exists to avoid.
ARG NODE_VERSION=24

# ⭐ THE UI STAGE, AND WHY THE IMAGE NEEDS ONE. oto is specified as "self-hosted,
# with a UI" (SPEC §0) and SPEC §J's acceptance criteria have a subsection headed
# "Timeline and UI" — criterion 23 is about a browser tab replaying missed events
# after twenty minutes asleep, which no API can satisfy. Without this stage
# `web/dist` never exists inside the build, `web/embed.go` embeds only its
# placeholder, and the published image answers `/` with `404 page not found`:
# which is what 0.1.0 through 0.1.2 did, making README's own setup step ("create a
# source in the UI") impossible on a real deployment.
#
# ⛔ ON $BUILDPLATFORM, FOR THE SAME REASON THE GO STAGE IS. The output is
# JavaScript — identical bytes whatever the target architecture — so building it
# once natively is right and emulating `tsc` under QEMU once per architecture is
# minutes spent to produce the same file twice.
#
# ⚠️ `npm ci` AND NOT `npm install`. package-lock.json is committed and `ci`
# honours it exactly; `install` is free to resolve something newer, which would
# make the image's UI differ from the one `just ui-build` and ci.yml tested.
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS ui
WORKDIR /web

# The lockfile alone first, so a change under web/src does not re-resolve the
# dependency tree.
COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./

# ⭐ THE SAME SCRIPT ci.yml AND `just ui-build` RUN, WHICH IS THE POINT. It is
# `tsc --noEmit && vite build` (web/package.json), so a type error fails the image
# build too. Calling `vite build` directly would be faster and would let the
# artifact diverge from what the gates checked — the defect class this whole
# change exists to close.
#
# It does NOT regenerate the API types: `src/api/generated/` is committed and
# `npm run generate:check` in ci is what proves it matches api/openapi/openapi.yaml.
# So this stage needs no access to the OpenAPI document.
RUN npm run build

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

# ⛔ AFTER `COPY . .`, NEVER BEFORE, AND .dockerignore IS WHY THIS IS SAFE. That
# file excludes `web/dist`, so the source copy above cannot bring a developer's
# stale local build into the image — the only `web/dist` that can exist here is
# the one the ui stage just produced. Placing this before `COPY . .` would let the
# source copy delete it again.
COPY --from=ui /web/dist ./web/dist

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
