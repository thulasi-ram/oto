# oto — the task runner.  `just` with no arguments lists everything.
#
# This is the only task runner. ADR 0021: a second one is a second answer to
# "what does green mean", and the two drifted. `just ci` is what CI runs.

set shell := ["bash", "-euo", "pipefail", "-c"]
set dotenv-load := true
set dotenv-filename := ".env"

web_dir    := "web"
docs_dir   := "docs-site"
migrations := "db/migrations"
db_url     := env_var_or_default("OTO_DB_URL", "postgres://oto:oto@localhost:5432/oto?sslmode=disable")
addr       := env_var_or_default("OTO_HTTP_ADDR", ":8080")
base       := "http://localhost" + addr
goose      := "go run github.com/pressly/goose/v3/cmd/goose@v3.27.3 -dir " + migrations + " postgres"

default:
    @just --list --unsorted

# ----------------------------------------------------------------- the big one

# Bring the whole stack up: infra, migrations, API+worker, and the UI.
[group('run')]
up: infra migrate
    #!/usr/bin/env bash
    set -euo pipefail
    trap 'kill 0' EXIT INT TERM
    echo "→ api+worker on {{base}}   ui on http://localhost:5173"
    go run ./cmd/oto &
    (cd {{web_dir}} && npm run dev) &
    wait

# Take a fresh checkout to a POPULATED, sign-in-able app: infra, migrations, the
# first org, and a believable fictional history on every screen.
#
# ⭐ IT EXISTS BECAUSE `just up` ON AN EMPTY DATABASE IS ELEVEN EMPTY STATES.
# Nothing in the product can be demonstrated, reviewed or screenshotted until an
# Alert has three Cases, a rule that changed between two of them and a delivery
# that died — and until this recipe there was no way to get there but to run a
# Prometheus for a fortnight or to paste hand-written SQL, which is how a fixture
# ends up violating an invariant nobody re-reads.
#
# It stops SHORT of starting the servers, deliberately: `just up` blocks, and a
# recipe that ends in a foreground process cannot print the credentials it just
# minted. The last thing it does is tell you to run `just setup` (once) and
# `just up`.
#
# The password comes from OTO_BOOTSTRAP_PASSWORD, or from the argument, and is
# echoed at the end because a demo login nobody can type is not a demo login.
# ⛔ `oto demo-seed` REFUSES with env=prod and refuses to run twice; re-running
# this recipe on a seeded database is a no-op with a sentence, not a duplicate.
[group('run')]
demo password=env_var_or_default("OTO_BOOTSTRAP_PASSWORD", "oto-demo-password"):
    #!/usr/bin/env bash
    set -euo pipefail
    just infra
    just migrate
    echo "→ bootstrap"
    if out=$(OTO_BOOTSTRAP_PASSWORD="{{password}}" go run ./cmd/oto bootstrap \
               --org-slug acme --org-name "Acme Corp" \
               --email ops@acme.example --name "Robin Vale" \
               --token-name demo 2>&1); then
      printf '%s\n' "$out"
    elif printf '%s' "$out" | grep -q 'already has an org'; then
      echo "→ already bootstrapped; keeping the org that is there"
    else
      printf '%s\n' "$out" >&2
      exit 1
    fi
    echo "→ demo-seed"
    go run ./cmd/oto demo-seed --org-slug acme
    cat <<EOF

    ✓ the demo is loaded.

      sign in as   ops@acme.example
      password     {{password}}

      Change it by re-running from an empty database:
          just reset && OTO_BOOTSTRAP_PASSWORD='something-longer' just demo

      Then, once per checkout:   just setup
      And to bring it up:        just up      → ui http://localhost:5173
    EOF

# Start Postgres, Alertmanager and Prometheus, blocking until all are healthy.
#
# Prometheus is named explicitly like the other two rather than left to a bare
# `up`, because `--wait` only waits for the services it is given. It carries the
# always-true rule in deploy/prometheus/oto-rules.yaml, so an alert reaches
# Alertmanager within a minute of this returning — that, and not a hand-written
# config, is what makes "bring the stack up and watch an alert arrive" true.
[group('run')]
infra: am-wire
    docker compose up -d --wait postgres alertmanager prometheus
    @echo "postgres :5432   alertmanager http://localhost:9093   prometheus http://localhost:9090"

# Render the dev Alertmanager receiver's URL and ingest token into
# deploy/alertmanager/local/, which is gitignored and mounted at
# /etc/alertmanager/local. Both are read by `url_file` / `credentials_file`.
#
# This recipe exists because Alertmanager expands NO environment variables: a
# literal `$VAR` in its config is parsed as a URL and fails at load. Files are
# the only supported indirection, so something has to write them, and that
# something reads .env rather than asking you to paste a token into a tracked
# file. Runs before `infra` so the mount is never an empty directory.
#
# With OTO_AM_LOCAL_* unset it writes the all-zero uuid and a dummy token, which
# 404s loudly instead of posting to a route that does not exist.
[group('run')]
am-wire:
    #!/usr/bin/env bash
    set -euo pipefail
    dir=deploy/alertmanager/local
    mkdir -p "$dir"
    url="${OTO_AM_LOCAL_WEBHOOK:-http://host.docker.internal:8080/api/v1/ingest/alertmanager/00000000-0000-0000-0000-000000000000}"
    # Alertmanager dials from inside a container, where localhost is itself.
    # The .env value is written from oto's point of view, so translate it.
    host_url="${url//localhost/host.docker.internal}"
    host_url="${host_url//127.0.0.1/host.docker.internal}"
    printf '%s' "$host_url"                                   > "$dir/webhook_url"
    printf '%s' "${OTO_AM_LOCAL_INGEST_TOKEN:-oto_ingest_REPLACE_ME}" > "$dir/ingest_token"
    chmod 600 "$dir/ingest_token"
    if [[ "$host_url" != "$url" ]]; then
      echo "→ $dir/webhook_url  (localhost → host.docker.internal)"
    else
      echo "→ $dir/webhook_url"
    fi
    # Never echo the token itself, only whether one was found.
    if [[ -n "${OTO_AM_LOCAL_INGEST_TOKEN:-}" ]]; then
      echo "→ $dir/ingest_token  (from OTO_AM_LOCAL_INGEST_TOKEN)"
    else
      echo "→ $dir/ingest_token  (PLACEHOLDER — set OTO_AM_LOCAL_INGEST_TOKEN in .env)"
    fi

# Stop the containers, keeping the data volume.
[group('run')]
down:
    docker compose down

# Run the API and worker in one process (the default mode).
[group('run')]
api:
    go run ./cmd/oto

# Run only the background worker.
[group('run')]
worker:
    go run ./cmd/oto worker

# Run the Vite dev server; it proxies /api and /healthz to the Go server.
[group('run')]
ui:
    cd {{web_dir}} && npm run dev

# Run the Starlight docs site. Syncs docs/, README.md and CONTEXT.md into
# docs-site/src/content/docs on every start — see docs-site/scripts/sync-docs.mjs.
[group('run')]
docs:
    cd {{docs_dir}} && npm run dev

# Build the docs site the way .github/workflows/pages.yml does, and check every
# internal link resolves.
#
# ⛔ THE LINK CHECK IS THE POINT. ~280 in-content links are written by
# `sync-docs.mjs` and read by nothing else in the repository: `just
# lint-vocabulary` does not look at docs-site/, and the sync script leaves a
# link it cannot resolve alone rather than guessing. A doc renamed under `docs/`
# used to take every pointer at it down silently.
#
# Build the docs site and check every internal link.
[group('run')]
docs-check:
    cd {{docs_dir}} && npm run verify

# Tail container logs.
[group('run')]
logs service="":
    docker compose logs -f {{service}}

# ------------------------------------------------------------------- database

# Apply every pending migration: goose schema, then River's own tables.
# Running goose alone leaves River's tables absent and the worker dies at boot.
[group('db')]
migrate:
    go run ./cmd/oto migrate

# Roll back exactly one migration.
[group('db')]
rollback:
    {{goose}} "{{db_url}}" down

# Show which migrations have been applied.
[group('db')]
status:
    {{goose}} "{{db_url}}" status

# Create a new migration: just new-migration add_widgets
[group('db')]
new-migration name:
    {{goose}} "{{db_url}}" create {{name}} sql

# Destroy the data volume and rebuild from empty. Irreversible.
[group('db')]
reset:
    docker compose down -v
    @just infra
    @just migrate

# Prove every Down works: up from empty, all the way down, then up again.
[group('db')]
migrate-roundtrip:
    {{goose}} "{{db_url}}" up
    {{goose}} "{{db_url}}" reset
    {{goose}} "{{db_url}}" up
    @echo "✓ round trip clean"

# Open a psql shell on the dev database.
[group('db')]
psql:
    psql "{{db_url}}"

# --------------------------------------------------------------------- poking

# Health, readiness and version.
[group('poke')]
health:
    @curl -fsS {{base}}/healthz && echo
    @curl -sS  {{base}}/readyz  && echo

# Fire a synthetic Alertmanager webhook at the ingest endpoint.
# Usage: just fire-alert <source-id> [token] [status]
[group('poke')]
fire-alert source_id token="dev" status="firing":
    #!/usr/bin/env bash
    set -euo pipefail
    now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    curl -fsS -X POST "{{base}}/api/v1/ingest/alertmanager/{{source_id}}" \
      -H 'content-type: application/json' \
      -H "authorization: Bearer {{token}}" \
      -d @- <<JSON | tee /dev/stderr
    {
      "version": "4",
      "groupKey": "{}:{alertname=\"OtoSmokeTest\"}",
      "truncatedAlerts": 0,
      "status": "{{status}}",
      "receiver": "oto-webhook",
      "groupLabels": {"alertname": "OtoSmokeTest"},
      "commonLabels": {"alertname": "OtoSmokeTest", "severity": "critical", "namespace": "payments"},
      "commonAnnotations": {"summary": "Synthetic alert from just fire-alert"},
      "externalURL": "http://localhost:9093",
      "alerts": [{
        "status": "{{status}}",
        "labels": {"alertname": "OtoSmokeTest", "severity": "critical", "namespace": "payments", "pod": "api-7f9c-2x4k"},
        "annotations": {"summary": "Synthetic alert from just fire-alert", "runbook_url": "https://example.com/runbook"},
        "startsAt": "$now",
        "endsAt": "0001-01-01T00:00:00Z",
        "generatorURL": "http://localhost:9090/graph?g0.expr=up+%3D%3D+0&g0.tab=1",
        "fingerprint": "3f8c1a2b9d4e5f60"
      }]
    }
    JSON
    @echo

# Watch the SSE stream. Usage: just stream <bearer-token>
[group('poke')]
stream token:
    curl -N -H "authorization: Bearer {{token}}" {{base}}/api/v1/stream

# Prometheus metrics for oto's own jobs and ingest.
[group('poke')]
metrics filter="oto_":
    @curl -fsS {{base}}/metrics | grep -E '^{{filter}}' || echo "no metrics matching {{filter}}"

# ------------------------------------------------------------------- quality

# Format Go and tidy go.mod.
[group('check')]
fmt:
    go fmt ./...
    go mod tidy

# Build, vet and lint the Go tree, including the gofmt check CI makes.
[group('check')]
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    unformatted="$(gofmt -l ./cmd ./internal ./db ./test ./tools)"
    if [ -n "$unformatted" ]; then echo "gofmt: $unformatted"; exit 1; fi
    go build ./...
    go vet ./...
    # golangci-lint is invoked by ABSOLUTE PATH and its absence is fatal.
    # Calling it by name goes through whatever wrapper is on PATH; the one on
    # this project's machines mis-parses `run` as a path, prints "No issues
    # found" and exits 0 EVEN WHEN THE BINARY IS NOT INSTALLED. That is a green
    # that means nothing, for the single gate that mechanically enforces the
    # CONTEXT.md §5 layering rules — so a missing linter must fail the build
    # rather than pass it silently.
    bin="$(go env GOPATH)/bin/golangci-lint"
    if [ ! -x "$bin" ]; then
      echo "golangci-lint is not installed at $bin — run: just setup" >&2
      exit 1
    fi
    "$bin" run ./...

# Go tests. Integration tests need Docker.
#
# Under colima, testcontainers cannot find the daemon unless it is told where
# the socket is. Without that the container never starts and the failure
# surfaces deep in the suite, reading like broken code rather than a missing
# environment variable — which is how a contributor concludes the tests are
# flaky and starts skipping them (git-bug 7c47185). Detected rather than
# hard-coded, so Docker Desktop and a pre-set DOCKER_HOST are both left alone,
# and announced so nobody debugs a variable they cannot see.
[group('check')]
test:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -z "${DOCKER_HOST:-}" ] && [ -S "$HOME/.colima/default/docker.sock" ]; then
      export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
      export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
      echo "→ colima detected — DOCKER_HOST=$DOCKER_HOST"
    fi
    go test -race -count=1 ./...

# Build the oto binary into ./bin.
[group('check')]
build:
    go build -o bin/oto ./cmd/oto

# Remove build artefacts.
[group('check')]
clean:
    rm -rf bin
    # ⛔ THE CONTENTS, KEEPING `.gitkeep`. `web/embed.go` embeds `all:dist`, and a
    # go:embed pattern matching nothing is a COMPILE error — so removing the
    # directory outright leaves `go build ./...` broken until the next
    # `npm run build`, which is a confusing way to answer `just clean`.
    cd {{web_dir}} && find dist -mindepth 1 ! -name .gitkeep -delete 2>/dev/null || true

# Typecheck and build the UI.
[group('check')]
ui-build:
    cd {{web_dir}} && npm run build

# UI tests.
[group('check')]
ui-test:
    cd {{web_dir}} && npm run test

# Regenerate the TypeScript client from the OpenAPI contract.
[group('check')]
generate:
    go generate ./...
    cd {{web_dir}} && npm run generate

# Gate G1 (SPEC §L.8.1): Go DTO → OpenAPI. Reflects every `*DTO`/`*Request`/
# `*Query` struct and diffs the derived schema against api/openapi/openapi.yaml.
# It reads YAML and runs the reflector; it needs no Docker and takes a second.
[group('check')]
dto-check:
    go test -count=1 ./test/contract/

# Gate G2 (SPEC §L.8.1): running server → OpenAPI. Assembles the REAL container
# over a REAL migrated Postgres, drives every declared operation over HTTP, and
# validates the bytes each handler actually wrote against the schema the contract
# declares for that operation, status and media type.
#
# It DEVIATES from the SPEC's `schemathesis`, and test/contract/server/doc.go
# carries the argument: a Python runtime for one check, in a Go+Node repository
# whose server cannot be started without the Go test harness, costs more than it
# closes. Needs Docker.
[group('check')]
server-check:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -z "${DOCKER_HOST:-}" ] && [ -S "$HOME/.colima/default/docker.sock" ]; then
      export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
      export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
      echo "→ colima detected — DOCKER_HOST=$DOCKER_HOST"
    fi
    go test -count=1 ./test/contract/server/

# Gate G3 (SPEC §L.8.1): the checked-in TS client must match the contract byte
# for byte. A Go DTO that drifts from openapi.yaml fails here, not in a browser.
[group('check')]
generate-check:
    cd {{web_dir}} && npm run generate:check

# Gate G4 (SPEC §L.8.1): OpenAPI → valibot. `web/src/api/generated/validators.ts`
# is generated from the contract and CHECKED IN; this regenerates it and fails on
# any diff, exactly as G3 does for the TS types. It is what makes the §L.8 rule
# — no hand-written valibot schema may describe an API response — enforceable
# rather than aspirational.
[group('check')]
validators-check:
    cd {{web_dir}} && npm run gen:validators:check

# Re-trace the ensō from the painting embedded in assets/logo/oto-icon-mark.svg
# into web/src/components/ensoPaths.ts — three weights of one brush stroke.
#
# ⛔ NOT PART OF `just ci`, ON PURPOSE. It writes a generated file, needs potrace
# and `sips`, and its output only changes when the artwork or the ladder does —
# which is a deliberate design edit, not something a build should perform behind
# somebody's back. Run it by hand and commit the result; the generated file's own
# header says it is generated, and tools/ensotrace/ensotrace.py carries the whole
# argument for why the stroke is traced rather than drawn in code.
[group('design')]
enso-trace:
    python3 tools/ensotrace/ensotrace.py

# SCOPE-BOUNDARY AC-49 (SPEC §P-18): the banned vocabulary and the forbidden
# person-subject columns, over internal/, web/src/ and db/migrations/. Known debt
# lives in tools/lintvocab/baseline.txt and can only shrink.
[group('check')]
lint-vocabulary:
    go run ./tools/lintvocab

# A struct field with no production writer or no production reader, a json tag
# that does not round-trip with openapi.yaml, and an exported New* with no
# caller outside its own file are all the same defect: code that compiles, lints
# and is wired to nothing. Known debt lives in tools/lintreach/baseline.txt and
# can only shrink; `//oto:reachable-ok <reason>` is the escape hatch and an
# unused one is an error.
# The unreachable-feature gate: declarations wired to nothing.
[group('check')]
lint-reachability:
    go run ./tools/lintreach

# The chart gate: `helm lint`, then `helm template` across the value
# combinations that matter, then `kubeconform` over every rendered manifest.
#
# deploy/helm/oto/ is the artifact a customer touches FIRST and was the only
# contract in this repository with no gate at all — 1,131 lines of templates and
# eight `fail` guard rails, asserted by review only. Each of the eight is now
# fed the input it exists to reject and must fail with its own sentence: a guard
# whose `.Values` path was renamed out from under it stops firing silently,
# which is the tools/lintreach defect class one layer out.
#
# Needs `helm` and `kubeconform`; `just setup` installs both, pinned to the
# versions CI installs. Neither is optional — a check that skips when its checker
# is absent reports success, which is worse than no check — so check.sh exits
# non-zero and names `just setup` rather than passing.
[group('check')]
helm-check:
    bash deploy/helm/check.sh

# Everything CI runs, in the order CI runs it. Keep this list and
# .github/workflows/ci.yml in step -- a contributor's green must be CI's green.
#
# The four drift gates of SPEC §L.8.1 run BEFORE `test`: they are the cheapest
# failures in the list and the ones whose message points straight at the fix, and
# a contract failure buried behind a full `go test -race ./...` is a contract
# failure found ten minutes late.
[group('check')]
ci: lint lint-vocabulary lint-reachability helm-check dto-check server-check generate-check validators-check test ui-build ui-test

# Install the toolchain this repo expects.
[group('check')]
setup:
    #!/usr/bin/env bash
    set -euo pipefail
    gobin="$(go env GOPATH)/bin"
    mkdir -p "$gobin"

    go mod download
    (cd {{web_dir}} && npm install)

    # Tested by absolute path, not `command -v`: a wrapper on PATH answers for a
    # binary that is not there, which is how the tree ran without a layering
    # gate at all. See the note in `just lint`.
    test -x "$gobin/golangci-lint" || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

    # `just helm-check` renders the chart and validates the manifests as
    # Kubernetes, and `just ci` runs it. kubeconform is a Go binary, so it
    # installs the way golangci-lint does.
    test -x "$gobin/kubeconform" || go install github.com/yannh/kubeconform/cmd/kubeconform@v0.7.0

    # ⭐ helm IS INSTALLED HERE, NOT WARNED ABOUT. `just ci` depends on
    # `just helm-check`, so a tool this recipe could only print a sentence about
    # is a fresh checkout whose first `just ci` fails — which reads as a broken
    # repository rather than as a missing dependency. It is not a Go binary, but
    # it does not need to be: the release archive is one static executable, it
    # lands in the same GOPATH/bin as the other two, and so this needs neither
    # sudo nor Homebrew and is the same three lines on macOS and Linux.
    #
    # ⚠️ PINNED TO THE VERSION .github/workflows/ci.yml INSTALLS. A contributor's
    # green must be CI's green, and helm is a tool where that is not pedantry:
    # helm 4 changed what `helm lint` does with a template `fail` (it logs and
    # exits 0), which is the whole reason deploy/helm/check.sh asserts through
    # `helm template` instead. check.sh looks in GOPATH/bin before PATH, so this
    # copy is the one the gate runs even where a package manager left another.
    helm_version=v4.1.1
    if [ ! -x "$gobin/helm" ]; then
      os="$(uname -s | tr '[:upper:]' '[:lower:]')"
      arch="$(uname -m)"
      case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
      echo "→ helm $helm_version → $gobin/helm"
      curl -fsSL "https://get.helm.sh/helm-${helm_version}-${os}-${arch}.tar.gz" \
        | tar -xzO "${os}-${arch}/helm" > "$gobin/helm"
      chmod +x "$gobin/helm"
    fi

    echo "✓ ready — now run: just up"

# --------------------------------------------------------------------------- #
# Release                                                                      #
# --------------------------------------------------------------------------- #

# Cut a release tag, after proving the eight things that make one publishable.
#
# ⛔ THE TAG IS THE ONLY INPUT THE RELEASE PIPELINE HAS, AND IT IS PUSHED ONCE.
# .github/workflows/release.yml has no `workflow_dispatch` on purpose and no
# `cancel-in-progress`: everything it publishes is derived from the tag being
# pushed, and there is no later run on the same ref to correct a bad one. So the
# checks live HERE, before the tag exists, rather than in the workflow where the
# only remaining move is to fail after a name is already taken.
#
# ⭐ IT CUTS AND DOES NOT PUSH. Creating a tag is local and free to undo
# (`git tag -d`); pushing it publishes an immutable image to a public registry
# and is not. Two steps, so the irreversible one is typed deliberately — the
# recipe prints the exact command.
#
# ⚠️ THE PIPELINE RUNS NO TESTS, WHICH IS WHY CHECK 7 IS NOT OPTIONAL. release.yml
# says so in its own header: `ci` has already run on this commit, so re-running
# it would double the wall clock to re-answer an answered question. That bargain
# holds only if something confirms the answer was green, and nothing downstream
# of the tag does.
#
# Cut a release tag locally. Prints the push that publishes it.
[group('release')]
release version:
    #!/usr/bin/env bash
    set -euo pipefail
    v='{{version}}'

    # 1. The shape the pipeline actually triggers on. A tag that does not match
    #    release.yml's glob does not fail — it publishes NOTHING, quietly, and
    #    the first symptom is an operator pulling a tag that is not there.
    if [[ ! "$v" =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]]; then
      echo "✗ '$v' is not a release tag. release.yml triggers on v[0-9]+.[0-9]+.[0-9]+*" >&2
      echo "  (the trailing * admits a prerelease: v0.1.0-rc.1)" >&2
      exit 1
    fi
    bare="${v#v}"

    # 2. A tag names a commit, never a working tree. Tagging with changes
    #    unstaged publishes an image built from something nobody can check out.
    if [ -n "$(git status --porcelain)" ]; then
      echo "✗ the working tree is dirty. A tag names a commit, not your uncommitted work." >&2
      git status --short >&2
      exit 1
    fi

    # 3-4. On main, and in step with it. The image is built from the tagged
    #      commit; releasing from a branch nobody has merged ships code no pull
    #      request ever saw.
    branch="$(git rev-parse --abbrev-ref HEAD)"
    if [ "$branch" != "main" ]; then
      echo "✗ on '$branch'. A release is cut from main." >&2
      exit 1
    fi
    git fetch --quiet origin main
    if [ "$(git rev-parse HEAD)" != "$(git rev-parse origin/main)" ]; then
      echo "✗ HEAD and origin/main disagree. Push or pull first — the tag must name a commit others have." >&2
      git --no-pager log --oneline HEAD...origin/main >&2 || true
      exit 1
    fi

    # 5. A tag is pushed once. Re-using a name means either a rejected push or,
    #    worse, a second digest answering to a string somebody already pinned.
    if git rev-parse -q --verify "refs/tags/$v" >/dev/null; then
      echo "✗ tag $v already exists locally. Delete it (git tag -d $v) or pick the next version." >&2
      exit 1
    fi
    if [ -n "$(git ls-remote --tags origin "refs/tags/$v")" ]; then
      echo "✗ tag $v already exists on origin. A published tag is never re-cut." >&2
      exit 1
    fi

    # 6. ⭐ THE CHART'S DEFAULT IMAGE TAG IS `appVersion`, VERBATIM AND WITHOUT
    #    THE `v`. _helpers.tpl falls back to .Chart.AppVersion, and release.yml
    #    publishes `{{{{version}}}}` — the bare form — for exactly that reason. If
    #    the two disagree, `helm install oto deploy/helm/oto` resolves to an
    #    image tag nothing ever pushed, which is the ImagePullBackOff the whole
    #    workflow exists to end.
    app="$(awk -F'"' '/^appVersion:/ {print $2}' deploy/helm/oto/Chart.yaml)"
    if [ "$app" != "$bare" ]; then
      echo "✗ Chart.yaml appVersion is '$app', this tag is '$bare'." >&2
      echo "  The chart's default image.tag IS appVersion, so they must match. Bump it and commit first." >&2
      exit 1
    fi
    chart="$(awk '/^version:/ {print $2}' deploy/helm/oto/Chart.yaml)"
    if [ "$chart" != "$bare" ]; then
      # A warning, not a refusal: the chart version tracks template changes and
      # is allowed to move independently of the application it deploys.
      echo "⚠ Chart.yaml version is '$chart' while appVersion is '$bare'. Deliberate?" >&2
    fi

    # 7. CI green on this exact commit. See the header: the pipeline tests
    #    nothing, so this is the only thing standing between a red main and a
    #    published image.
    sha="$(git rev-parse HEAD)"
    if command -v gh >/dev/null 2>&1; then
      verdict="$(gh run list --branch main --workflow ci --limit 20 \
        --json headSha,conclusion,status \
        --jq "[.[] | select(.headSha == \"$sha\")] | first | .conclusion // \"none\"" 2>/dev/null || echo unknown)"
      case "$verdict" in
        success) echo "✓ ci is green on $sha" ;;
        none|null|"")
          echo "✗ no ci run found for $sha. Push the commit and let ci finish before tagging it." >&2
          exit 1 ;;
        unknown)
          echo "⚠ could not reach GitHub to check ci. Verify by hand before pushing the tag." >&2 ;;
        *)
          echo "✗ ci concluded '$verdict' on $sha. The release pipeline runs no tests of its own." >&2
          exit 1 ;;
      esac
    else
      echo "⚠ gh is not installed, so ci's verdict on $sha is unchecked. Verify by hand." >&2
    fi

    # 8. The image the pipeline will build, built here first. A cross-compile
    #    that breaks does so on a laptop in two minutes rather than after a tag
    #    has taken a name that cannot be reused.
    echo "→ proving both architectures cross-compile"
    docker buildx build --platform linux/amd64,linux/arm64 \
      --build-arg "VERSION=$v" --build-arg "COMMIT=$sha" \
      --output=type=cacheonly . >/dev/null

    git tag -a "$v" -m "oto $v"
    echo
    echo "✓ cut $v at $sha"
    echo
    echo "  Nothing is published yet. To release:"
    echo "      git push origin $v"
    echo
    echo "  That triggers .github/workflows/release.yml, which publishes"
    echo "      ghcr.io/thulasi-ram/oto:$bare, :$v, :${bare%.*} and :sha-$sha"
    echo "  To undo before pushing:  git tag -d $v"

# Watch the release pipeline for a tag, and report what it published.
[group('release')]
release-watch version:
    #!/usr/bin/env bash
    set -euo pipefail
    v='{{version}}'
    gh run watch "$(gh run list --workflow release --limit 20 \
      --json databaseId,headBranch --jq "[.[] | select(.headBranch == \"$v\")] | first | .databaseId")" \
      --exit-status
