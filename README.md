# oto

**The alert history layer your Prometheus stack does not have — self-hosted, with a UI, without
adopting an AIOps platform or a paid SaaS.** For every alert that has ever fired, oto shows: when
it first appeared, every episode since, *what the rule said at that moment*, who was told, on
which channel, in which thread, who acknowledged it, and how it ended — as one continuous,
replayable timeline. Alertmanager holds no history; Grafana's alert history only covers
Grafana-managed rules; Robusta's UI is SaaS-or-enterprise; Grafana OnCall OSS was archived in
March 2026. oto fills that vacuum: a self-hostable, OSS, Slack-first alert layer over an existing
Prometheus/Alertmanager stack. It is not an incident manager, an on-call/paging system, a
workflow engine, a rule editor, an AI investigator, or anything with a write path into your
cluster.

## Running it

Prerequisites: Go 1.26, Node 24, Docker.

```bash
cp .env.example .env      # defaults already match docker-compose.yml
just infra                # postgres:17 + alertmanager, waits for health
just migrate              # apply migrations

# The first org, the first user and the first token. v1 has no signup route and
# no org-creation API on purpose, so a freshly migrated database has no
# credential that can log in — this subcommand is the only way to make one, and
# it refuses to run twice.
OTO_BOOTSTRAP_PASSWORD='a long passphrase' \
  go run ./cmd/oto bootstrap --org-slug acme --email you@example.com

just up                   # API on :8080 and UI on :5173, together
```

`bootstrap` prints a personal access token **once**; only its sha256 is stored. Then open
<http://localhost:5173> and log in with the email and password you just used. The landing page
calls `/healthz` through the dev proxy, which is the quickest way to confirm the front end and the
back end are talking.

| | |
|---|---|
| API | <http://localhost:8080> — `/healthz`, `/readyz`, `/metrics`, `/api/v1/…` |
| UI | <http://localhost:5173> |
| Alertmanager | <http://localhost:9093> — routes every alert to oto on the host |

`just` on its own lists every recipe. The ones you will use most:

```
just build            build ./bin/oto
just test             go test -race ./...   (integration tests need Docker)
just lint             golangci-lint, including the layering rules
just lint-vocabulary  SCOPE-BOUNDARY AC-49: the banned on-call vocabulary
just generate-check   gate G3: the checked-in TS client matches openapi.yaml
just ci               everything the GitHub workflow runs, in the same order
just status           which migrations have been applied
just reset            destroy the dev volume and start clean
```

`just` is the only task runner (ADR 0021). There was a Makefile alongside it and the two diverged —
`make ci` was green on a tree the GitHub `ui` job rejected. `just ci` and
`.github/workflows/ci.yml` run the same list, and keeping them identical is the only reason a
contributor's green means anything.

## Layout

```
cmd/oto/          the binary: server, worker, operational subcommands
internal/app/     explicit constructor wiring; the dependency graph is readable
internal/platform config, log, telemetry, db (two pools + tx), httpx, jobs, errs, id, clock
internal/<domain> api / service / repository / domain — see CONTEXT.md §5
pkg/alertkey/     canonical label serialisation and the identity keys
db/migrations/    goose SQL, expand/contract only
web/              Vite 6 + SolidJS + TypeScript strict + Tailwind v4
deploy/           compose, alertmanager config, and `slack/manifest.yaml` — the
                  Slack app manifest a customer pastes into Slack, with every
                  requested scope justified and sourced. ⚠️ `deploy/helm/` and
                  `deploy/prometheus/` are EMPTY DIRECTORIES: the Helm chart of
                  SPEC acceptance criterion 31 (`helm install oto` as the entire
                  install) is not built yet, and neither is the sample
                  prometheus.yml. Deploy with the binary and compose for now.
test/             ⚠️ fixtures only. `test/{contract,integration,load,harness}/`
                  each hold a single `doc.go` and no test; the first tests in
                  this repo live next to the code they cover
                  (`internal/**/*_test.go`, `web/src/**/*.test.ts`). Treat the
                  four empty directories as a plan, not as coverage.
```

## What is actually enforced

Enforcement is worth stating precisely, because this repository has previously claimed gates it did
not have. What CI runs today, and therefore what will stop a bad change:

| Gate | Where | Status |
|---|---|---|
| Layering (`depguard`) | `.golangci.yml`, `go-lint` job | **enforced** |
| gofmt / `go mod tidy` clean | `go-lint` job | **enforced** |
| `go build` + `go vet` | `go-build` job | **enforced** |
| `go test -race ./...` | `go-test` job | **enforced**, but the suite is young — a green here means little yet |
| G3: openapi.yaml → TS client is not stale | `ui` job, `npm run generate:check` | **enforced** |
| AC-49 vocabulary + forbidden columns | `vocabulary` job, `go run ./tools/lintvocab` | **enforced**, with known debt listed in `tools/lintvocab/baseline.txt` |
| `TestValidatorMatchesDDL` (§L.8) | `internal/platform/validate/ddl_test.go` | **enforced** — every canonical regex is compared byte-for-byte with its DDL `CHECK` |
| G1: Go DTO → OpenAPI | — | **not built** |
| G2: running server → OpenAPI (schemathesis) | — | **not built** |
| G4: OpenAPI → generated valibot validators | — | **not built**; the valibot schemas in `web/src` are hand-written, which §L.8.1 forbids |

The layering rules are mechanically enforced by `depguard` in `.golangci.yml`: `api` cannot import
`repository`, `repository` cannot import `api`, `domain` packages import no I/O at all, and no
domain can reach into another domain's internals. If `make lint` rejects an import, the import is
wrong — not the rule.

## Where to read next

- `CONTEXT.md` — the map: domain language, module map, layering rules, conventions. **Read first.**
- `docs/design/SPEC.md` — binding specification. Implement it literally.
- `docs/design/domain-research.md` — verified ground truth about Alertmanager and Slack.
- `docs/adr/` — amendments. The SPEC changes only through an ADR in the same commit.
- `docs/setup/slack.md` — connecting a workspace: the three credentials, and what to do about
  every Slack error oto classifies. Paste `deploy/slack/manifest.yaml` into Slack's
  "Create an app from manifest" flow rather than ticking scopes by hand.
- `docs/setup/tuning.md` — `refire_grace`, the flap and storm thresholds, and how the right value
  for each is derived from your own `alertmanager.yml` and your rules' `for:` durations.

## Licence

MIT — see `LICENSE` and [ADR 0019](docs/adr/0019-mit-licence.md). No `ee/` directory, no feature
behind a licence key, no CLA.

Issue tracking lives in the repository itself via [`git-bug`](https://github.com/git-bug/git-bug):
`git-bug bug` lists, `git-bug bug new` files.
