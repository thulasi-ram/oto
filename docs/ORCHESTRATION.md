# Orchestration state

Live build log for the agent orchestrating oto's implementation. Any session
picking this up should read this file first, then `CONTEXT.md`, then
`docs/design/SPEC.md` (binding) and `docs/design/SCOPE-BOUNDARY.md` (binding).

## How this build is run

A thin orchestrator spawns purpose-built subagents. Each subagent owns a
disjoint set of paths, reads the SPEC as binding, and returns a short report.

For phases 0–3 subagents did **not** write or run tests, on the theory that a
later review phase would. That rule is now **withdrawn**: it produced a
repository with zero tests, a CI `ui` job that failed on the commit that
introduced it, and eight of thirty-six live API operations diverging from the
contract — every one of them the kind a single smoke test catches on day one.
From phase 4 onward an agent tests what it writes, and the review phase reviews
rather than backfills.

Rules that have held so far and should keep holding:

- One agent, one module, disjoint paths. Concurrency is safe only because of this.
- The SPEC is the contract between agents. If an agent finds the SPEC wrong,
  it reports the defect rather than silently diverging; the judge amends the
  SPEC and the change is applied as a numbered `§P` work item.
- `go build ./...`, `go vet ./...` and `golangci-lint run ./...` must pass
  before an agent reports done. `golangci-lint` enforces the layering via
  `depguard`, so an import complaint means the import is wrong, not the linter.

## Phase log

### Phase 0 — design (done)
Architect proposal, verified Alertmanager/Slack domain research, and a red-team
memo, reconciled by a judge into the binding SPEC, 13 ADRs and `CONTEXT.md`.

Findings that changed the design:
- Alertmanager drops suppressed alerts before the webhook, so suppression is
  only learnable from API v2 — the pull reconciler is mandatory.
- 4xx/429 to Alertmanager is permanent notification loss; only 5xx retries.
- `groupKey` embeds the route path, so a config reload changes it. Not durable.
- `chat.update` is ~50x cheaper than `chat.postMessage`; `notification_reason`
  (AM 0.32+) decides update-vs-post.

### Phase 1 — skeleton (done, commit `56bbe95`)
Domain-first Go tree, platform packages, goose migrations, docker-compose with
Postgres + Alertmanager, SolidJS/Vite/Tailwind web app, CI, depguard layering
rules proven to fire against planted violations.

### Phase 2 — kernel, schema, contract, doctrine (done, commit `0eb50e8`)
- `internal/platform/errs` + `validate`; `internal/alerts/domain` with the
  lifecycle state machine T1–T10, pure and clock-injected.
- 30 tables, 201 named CHECKs, 94 indexes, partitioned event tables, verified
  reversible round trip. 36/36 constraint-rejection tests behaved.
- `api/openapi/openapi.yaml`, 71 operations, lints clean.
- `docs/design/SCOPE-BOUNDARY.md` — the Flight Recorder Test, answering the
  owner's "don't become an incident management platform" constraint.

### Phase 3 — module implementation (in progress)
Done: Alertmanager/Prometheus clients + rule matching; River job queue,
per-thread ordering primitive, SSE streaming with durable resume; Slack Block
Kit renderer + provider, generic webhook provider, channel registry.

Also done: alerts + grouping repository/service, ingestion, enrichment + rules,
notification.

The blockage recorded here — "identity, every `api` sub-package, `internal/app`
and the UI are NOT implemented; nothing runs end-to-end" — is **resolved and this
paragraph was stale for some time**. The binary serves traffic today: `go run
./cmd/oto migrate && go run ./cmd/oto bootstrap && go run ./cmd/oto serve` brings
up an API that answers all 15 tags, ingests Alertmanager webhooks, dispatches
notifications and streams SSE. An audit exercised 36 operations against it.

### Phase 4 — review and test (in progress)
A conformance audit against the running binary is done and its findings are being
worked. What it established, so it is not re-established:

- **Structure is as documented.** Layering holds under `golangci-lint`; the four
  scope-boundary doors are shut in the live schema and the route tree; ADRs 0007,
  0014, 0016 and 0017 conform exactly. `CONTEXT.md` is a trustworthy map.
- **Behaviour was optimistic.** Eight of thirty-six operations diverged from
  `api/openapi/openapi.yaml`, including two that made the product unusable (no
  source could be created; the SSE stream sent no `Content-Type`, so no browser
  could attach).
- **Enforcement was false.** §L.8.1 claimed "four gates, all in CI". G1, G2 and G4
  did not exist; G3 existed, was not in CI, and failed when run. The AC-49
  vocabulary lint did not exist. There were zero tests.

Gates now in CI: G3, the AC-49 vocabulary lint (`tools/lintvocab`) and
`TestValidatorMatchesDDL`. G1, G2 and G4 remain unbuilt and are named as such in
`README.md`. Tests are being written next to the code they cover, not in
`test/`, whose four subdirectories still hold nothing but a `doc.go`.

## Open work handed on from the phase-4 audit

- `escalate_after_seconds` still ships on the wire from
  `internal/notification/api/{dto,mapper}.go`. The DB column (00019), the
  contract and the UI all say `unacked_reminder_after_s(econds)`; the Go DTO is
  the last holdout and is listed in `tools/lintvocab/baseline.txt`.
- G1 (Go DTO → OpenAPI), G2 (schemathesis against a running server) and G4
  (generated valibot validators) are unbuilt.
- `deploy/helm/oto/` and `deploy/prometheus/` are empty directories; SPEC
  acceptance criterion 31 (`helm install oto` is the entire install) is unmet.

## Open defects found by implementers, awaiting SPEC amendment

- `§G.7.2` ordering switch tests "root has not landed" before "thread is dead",
  which wedges the exact case `§G.7.3` exists to prevent. Implementation
  inverted the order and added a bounded `MaxWait` escalation; SPEC must catch up.
- `mention_on_reminder` lives in Slack channel config but neither
  `NotificationView` nor `RenderOptions` can carry it to the renderer. Worked
  around with a channel-scoped renderer copy; may want a port field instead.
- `V11`'s bare-UUID assertion was scoped to `button.value` only, because
  `§H.3`'s own overflow option carries `"labels|<group_id>"`.

## Decisions taken on the owner's behalf

Recorded here so they can be reversed cheaply. Full reasoning in the ADRs.

1. Snooze is in v1 — suppresses oto's own notifications, writes nothing to the
   cluster. It was missing from the SPEC entirely rather than deferred.
2. Individual Slack `@`-mentions are permitted, capped at 10, with an explicit
   never-a-rota clause. Overruled the scope agent's usergroup-only restriction.
3. `internal/<domain>/{api,service,repository,domain}` rather than a literal
   `src/`, for compiler-enforced encapsulation. Same domain-first shape.
4. `org_id` in the schema from day one; no RBAC or SSO in v1.
5. Silences are a read-only mirror in v1. No write path into the cluster.
6. `pkg/alertkey` deleted; `internal/alerts/domain` is the shared kernel and the
   single sanctioned cross-domain `domain` import.

## Questions genuinely needing the owner

Collected for a single decision session rather than interrupting piecemeal.
The build is not blocked on any of them.

1. **Is oto k8s-*enriched* or only k8s-*shaped*?** All Kubernetes API enrichment
   is currently deferred — it needs cluster RBAC and it is Robusta's turf. Today
   oto groups by namespace/cluster and installs by Helm but does not read the
   cluster. This may contradict "k8s cloud native" as intended.
2. **Slack app distribution** — Marketplace, per-customer internal app, or
   BYO-token. Sets the rate-limit tier, which is an architecture input.
   Assumed: BYO-token self-hosted, Socket Mode default.
3. **Licence.** Keep and Robusta are both MIT-cored; AGPL would make oto the
   most restrictive option in a permissive field.
4. **`refire_grace` (10m) and flap thresholds (5-in-30m)** — the knobs that
   decide how noisy Slack is. Should be validated against a real
   `alertmanager.yml` before they harden.
5. **Retention defaults** — raw 14d, events 13mo. Regulated buyers needing years
   would make cold-storage export a v1 requirement.
