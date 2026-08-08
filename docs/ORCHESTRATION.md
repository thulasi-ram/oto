# Orchestration state

Live build log for the agent orchestrating oto's implementation. Any session
picking this up should read this file first, then `CONTEXT.md`, then
`docs/design/SPEC.md` (binding) and `docs/design/SCOPE-BOUNDARY.md` (binding).

## How this build is run

A thin orchestrator spawns purpose-built subagents. Each subagent owns a
disjoint set of paths, reads the SPEC as binding, and returns a short report.
Subagents do **not** write or run tests — a dedicated skeptical review phase
owns that, deliberately, so that the reviewers are not marking their own work.

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
- `docs/api/openapi.yaml`, 71 operations, lints clean.
- `docs/design/SCOPE-BOUNDARY.md` — the Flight Recorder Test, answering the
  owner's "don't become an incident management platform" constraint.

### Phase 3 — module implementation (in progress)
Done: Alertmanager/Prometheus clients + rule matching; River job queue,
per-thread ordering primitive, SSE streaming with durable resume; Slack Block
Kit renderer + provider, generic webhook provider, channel registry.

Also done: alerts + grouping repository/service, ingestion, enrichment + rules,
notification.

**BLOCKED.** The identity module, every module's `api` sub-package, the
`internal/app` composition root and the SolidJS UI screens are NOT implemented.
Both agents doing that work were terminated mid-write by an account spend limit.
Their partial output was recovered, the tree is green and committed at `2063979`.

To resume, re-run two agents with the briefs in the orchestrator transcript:
1. identity + API layer + `internal/app` + `cmd/oto` — wire every operationId in
   `docs/api/openapi.yaml`, separate ingest/UI connection pools, `/stream`
   mounted outside timeout middleware.
2. the SolidJS UI — alert list, grouping, sentry-style timeline with the rule
   drift diff, SSE with `Last-Event-ID` resume, JSON-Schema-driven channel forms.

Nothing runs end-to-end until step 1 lands: the modules are all implemented but
nothing constructs them.

### Phase 4 — review and test (not started)
A small team of skeptical reviewers, an auditor and a judge review the code
*before* any tests are written or run. Then tests, then run it for real against
the docker-compose Alertmanager.

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
