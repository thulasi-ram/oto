# oto — task runner.  `just` with no arguments lists everything.
#
# The Makefile remains the CI entrypoint; this file is for humans bringing the
# stack up and poking at it.

set shell := ["bash", "-euo", "pipefail", "-c"]
set dotenv-load := true
set dotenv-filename := ".env"

web_dir    := "web"
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

# Start Postgres and Alertmanager, blocking until both are healthy.
[group('run')]
infra:
    docker compose up -d --wait postgres alertmanager
    @echo "postgres :5432   alertmanager http://localhost:9093"

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

# Tail container logs.
[group('run')]
logs service="":
    docker compose logs -f {{service}}

# ------------------------------------------------------------------- database

# Apply every pending migration.
[group('db')]
migrate:
    {{goose}} "{{db_url}}" up

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

# Build, vet and lint the Go tree.
[group('check')]
lint:
    go build ./...
    go vet ./...
    golangci-lint run ./...

# Go tests. Integration tests need Docker.
[group('check')]
test:
    go test -race -count=1 ./...

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

# Gate G3 (SPEC §L.8.1): the checked-in TS client must match the contract byte
# for byte. A Go DTO that drifts from openapi.yaml fails here, not in a browser.
[group('check')]
generate-check:
    cd {{web_dir}} && npm run generate:check

# Everything CI runs.
[group('check')]
ci: fmt lint generate-check test ui-build

# Install the toolchain this repo expects.
[group('check')]
setup:
    go mod download
    cd {{web_dir}} && npm install
    @command -v golangci-lint >/dev/null || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
    @echo "✓ ready — now run: just up"
