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
just infra                # postgres:17 + alertmanager + prometheus, waits for health
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
| Prometheus | <http://localhost:9090> — scrapes oto's own `/metrics` and evaluates `deploy/prometheus/oto-rules.yaml` |

### Seeing an alert arrive

`just infra` also brings up a Prometheus configured by
[`deploy/prometheus/prometheus.yml`](deploy/prometheus/prometheus.yml). Its first rule,
`OtoStackSmokeTest`, is deliberately always true with a 15 s `for:`, so within a minute
<http://localhost:9093> shows a card without you having to break anything. That proves
Prometheus → Alertmanager.

The last hop, Alertmanager → oto, needs two values that cannot exist before you do: the ingest route
is per source (`/api/v1/ingest/alertmanager/{source_id}`) and authenticates with that source's own
token. Create a source, then paste its `webhook_url` and `ingest_token` — returned exactly once —
into `deploy/alertmanager/alertmanager.yml` and `docker compose restart alertmanager`. The file's
header comment has the exact call. Until then the receiver points at an all-zero uuid that 404s on
purpose, rather than at a URL that looks plausible and silently is not.

That same Prometheus is a legitimate `prometheus_url` for the source, which is what lets oto show
what a rule said at the moment it fired (ADR 0009).

`just` on its own lists every recipe. The ones you will use most:

```
just build            build ./bin/oto
just test             go test -race ./...   (integration tests need Docker)
just lint             golangci-lint, including the layering rules
just lint-vocabulary  SCOPE-BOUNDARY AC-49: the banned on-call vocabulary
just generate-check   gate G3: the checked-in TS client matches openapi.yaml
just helm-check       render the Helm chart and prove its guard rails still fire
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
db/migrations/    goose SQL, expand/contract only
web/              Vite 6 + SolidJS + TypeScript strict + Tailwind v4
deploy/           alertmanager config; `prometheus/` with the sample
                  `prometheus.yml` and the alert rules the compose stack
                  evaluates; and `slack/manifest.yaml` — the Slack app manifest a
                  customer pastes into Slack, with every requested scope
                  justified and sourced.
deploy/helm/oto/  the Helm chart of SPEC acceptance criterion 31. API and worker
                  Deployments, `oto migrate` as a pre-install/pre-upgrade hook
                  (it runs the SUBCOMMAND, not `goose up`, because goose alone
                  leaves River's tables absent and the worker dies at boot),
                  optional Ingress, `existingSecret` support, and a `values.yaml`
                  in which every key maps onto an environment variable
                  `internal/platform/config` actually reads. Postgres is EXTERNAL
                  and there is no subchart — ADR 0014, SPEC R7.
                  The image `image.repository` points at is built and pushed by
                  `.github/workflows/release.yml` on every `v*.*.*` tag:
                  `ghcr.io/thulasi-ram/oto`, linux/amd64 + linux/arm64, tagged
                  `X.Y.Z` (what `appVersion` resolves to), `vX.Y.Z`, `X.Y` and
                  `sha-<commit>`. No `latest` — the chart tells you to pin.
                  ⚠️ No release has been tagged yet, so nothing is at that path
                  until the first `v0.1.0` is pushed.
docs/runbooks/    one page per `oto_*` metric the binary registers: what it
                  counts, what a sustained value means, what to check and what to
                  do. The `runbook_url` on every rule in
                  `deploy/prometheus/oto-rules.yaml` points here.
test/harness/     real Postgres via testcontainers (one container, a migrated
                  template, a fresh database per test), fakes for the four true
                  externals — Alertmanager, Prometheus, Slack, outbound webhooks
                  — and object builders. Every DB test goes through this.
test/integration/ cross-module tests over the harness.
test/contract/    the drift gates. Nine test files, including
                  `coverage_test.go` — the RATCHET that fails when an operation
                  is added to `api/openapi/openapi.yaml` with no test behind it.
                  `server/` drives the real container over a real Postgres. G1
                  and G2 below run from here, and CI runs them.
test/load/        the storm cases: `storm_test.go` over `driver_test.go` and
                  `env_test.go`, with `RESULTS.md` recording what a run actually
                  measured. ⚠️ THEY ARE BEHIND `//go:build load`, so
                  `go test ./...` does not compile them in and CI never runs
                  them — `go test -tags load ./test/load/`. These are real
                  tests, but a green CI says nothing about them.
```

## What is actually enforced

Enforcement is worth stating precisely, because this repository has previously claimed gates it did
not have. What CI runs today, and therefore what will stop a bad change:

| Gate | Where | Status |
|---|---|---|
| Layering (`depguard`) | `.golangci.yml`, `go-lint` job | **enforced** — but layering only. The `<module>-must-not-reach-into-other-domains` rules are symmetric (each re-allows every other module's `/service`), so they say nothing about direction |
| Module **direction** (CONTEXT.md §4) | `test/arch/arch_test.go`, `go-test` job | **enforced** — the real import graph is diffed against the declared edge list; a new cross-module edge fails, a declared edge the code has dropped fails, and an edge list with a cycle in it fails |
| gofmt / `go mod tidy` clean | `go-lint` job | **enforced** |
| `go build` + `go vet` | `go-build` job | **enforced** |
| `go test -race ./...` | `go-test` job | **enforced.** Every domain now has tests; the service and repository tiers largely do not. A green means the domain logic holds, not that a feature works end to end — that gap is what [ADR 0021](docs/adr/0021-correctness-and-testing-strategy.md) §3–§4 exist to close |
| G1: Go DTO → OpenAPI | `contract-dto` job, `go test ./test/contract/` (`just dto-check`) | **enforced** — reflects every `*DTO`/`*Request`/`*Query` struct and diffs json tags, required/optional, types, enums, nullability and `validate` bounds against the contract. Known debt is enumerated in the test and can only shrink |
| G2: running server → OpenAPI | `contract-server` job, `go test ./test/contract/server/` (`just server-check`) | **enforced** — the real container over a real Postgres, every declared operation called over HTTP, every response's status, Content-Type and BYTES validated against the contract. ⚠️ Go-native, not `schemathesis`; the argument is in `test/contract/server/doc.go` |
| G3: openapi.yaml → TS client is not stale | `ui` job, `npm run generate:check` (`just generate-check`) | **enforced** |
| G4: OpenAPI → generated valibot validators | `ui` job, `npm run gen:validators:check` (`just validators-check`) | **enforced** — `web/src/api/generated/validators.ts` is generated and checked in. The three remaining hand-written valibot schemas are FORM schemas, and each `v.pipe`s into the generated `*RequestSchema` as its final gate, which is what §L.8.1 asks for |
| AC-49 vocabulary + forbidden columns | `vocabulary` job, `go run ./tools/lintvocab` | **enforced**, with known debt listed in `tools/lintvocab/baseline.txt` |
| Helm chart renders, and its guard rails still fire | `helm` job, `bash deploy/helm/check.sh` (`just helm-check`) | **enforced** — `helm lint`, then `helm template` over defaults, over the three templates the defaults switch off (`ingress`, `hpa`, `job-bootstrap`), over `existingSecret` (which must render NO Secret) and over Slack in `http` mode, with every rendered manifest validated as Kubernetes by `kubeconform` at both ends of Chart.yaml's `kubeVersion` range. Each of the **eight** `fail` guard rails in `_helpers.tpl` is fed the input it exists to reject and must fail with its own sentence. ⚠️ `helm lint` alone is not the gate: it exits 0 on this chart at its own defaults, which `helm template` refuses to render |
| `TestValidatorMatchesDDL` (§L.8) | `internal/platform/validate/ddl_test.go` | **enforced** — every canonical regex is compared byte-for-byte with its DDL `CHECK` |
| Design tokens and measured contrast (§M.4–§M.7, AC-45) | `web/src/design/contrast.test.ts` and `tokens.test.ts`, `ui` job | **enforced** — every pair §M.4/§M.5 tabulate is recomputed from the hex the row quotes, that hex is checked against `tokens.css`, and both themes must declare the same token names. ⚠️ It found **13 of the 39 published ratios wrong** on the day it was written; they were corrected in the same commit |
| Tier-B colour scarcity, and the Slack/UI boundary (§M.2, §M.6, AC-47, AC-48) | `test/design/`, `internal/channels/render/slack/palette_test.go`, `go-test` job | **enforced** — no state hue outside a state badge, a row status or a timeline marker (with the permitted files listed and reasoned, and a stale entry failing too); the six §H.2 hexes pinned to the SPEC rather than to the code; no §H.2 literal under `web/src`; no `--oto-*` string in a renderer |
| Reduced motion (U4, U9, AC-47) | `web/src/index.css.test.ts`, `ui` job | **enforced structurally** — the guard sweeps `*` rather than naming classes, sits in the layer that lets it beat an important-flagged utility, and no motion is driven from JavaScript or an inline style where CSS cannot reach it. This replaced the Playwright snapshot §M.7 used to name: it proves more, and needs no browser |
| axe-core contrast over the rendered DOM, both themes (AC-45) | — | **NOT enforced.** The one UNWRITTEN row of SPEC §M.7. `web/e2e/` holds a `.gitkeep`; there is no Playwright, no axe and no CI job to run one. `contrast.test.ts` covers the pairs the SPEC tabulates, which is not the same as the pairs the product composes |

Each of the four drift gates has been demonstrated to FAIL against a deliberately planted drift —
a Go DTO field absent from the contract for G1, a handler returning the wrong shape for G2, a
hand-edited generated file for G3 and G4 — before being called done. A gate that has never failed
is a gate nobody knows works.

⚠️ **The four §M design gates have not yet been through that demonstration.** They are new with
git-bug `c49baaa`, and each carries an internal guard against reading nothing (an empty token set,
an unparsed table row, an exception that no longer matches) — but a guard is not a planted
violation. Until somebody has moved a hue, dropped a token from one theme and put a state colour in
a chrome component and watched all three go red, this paragraph does not cover them.

The chart gate was held to the same standard, against eight planted defects: a `Service` given
`apiVersion: v1beta9`, a `Deployment` with its `selector` removed, a mis-nested `{{- if }}` in
`hpa.yaml` (a template the defaults never render), one `fail` guard deleted, one inverted, a
`.Values` path renamed out from under two more, and the `{{- if not .Values.existingSecret }}` on
the migration hook's Secret inverted. All eight fail the gate; the chart at HEAD passes.
The first two are the ones that motivate `kubeconform` — `helm template` renders both and exits 0,
because it checks that the output is YAML, not that it is Kubernetes. The last is why the render
assertions count `kind:` documents rather than `# Source:` markers: `job-migrate.yaml` emits two
objects behind different conditions, so its filename is present under every value of
`existingSecret` and cannot say which of the two rendered.

The layering rules are mechanically enforced by `depguard` in `.golangci.yml`: `api` cannot import
`repository`, `repository` cannot import `api`, `domain` packages import no I/O at all, and no
domain can reach into another domain's internals. If `just lint` rejects an import, the import is
wrong — not the rule. Which *way* an edge may point is a separate question and a separate gate:
`test/arch/arch_test.go` owns it, because depguard's cross-domain rules are symmetric and cannot
express direction or acyclicity.

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
- `docs/runbooks/` — one page per `oto_*` metric: what it counts, what a sustained value means,
  what to check and what to do. [The index](docs/runbooks/README.md) says which metrics are worth
  paging on and which are purely informational, and lists the metrics the SPEC names that no
  collector builds yet. `just metrics` dumps the live values.

## Licence

MIT — see `LICENSE` and [ADR 0019](docs/adr/0019-mit-licence.md). No `ee/` directory, no feature
behind a licence key, no CLA.

Issue tracking lives in the repository itself via [`git-bug`](https://github.com/git-bug/git-bug):
`git-bug bug` lists, `git-bug bug new` files.
