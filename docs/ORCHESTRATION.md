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

Gates now in CI: **all four of §L.8.1** — G1 (`test/contract/`), G2
(`test/contract/server/`), G3 (`npm run generate:check`) and G4
(`npm run gen:validators:check`) — plus the AC-49 vocabulary lint
(`tools/lintvocab`), the reachability lint (`tools/lintreach`) and
`TestValidatorMatchesDDL`. `just ci` lists them in the order CI runs them.

G2 deviates from the SPEC, which names `schemathesis`: it is Go-native, driving
the real container over a real Postgres and validating the bytes each handler
writes against the contract. `test/contract/server/doc.go` carries the argument
— a Python runtime for one check, in a repository whose server cannot be started
without the Go test harness, costs more than it closes — and the exhaustiveness
schemathesis would have supplied is recovered by a ratchet: an operation the
contract declares and the probe table does not drive fails the gate.

Each gate was demonstrated to fail against a deliberately planted drift before
being declared done, which is the standard the depguard rules were held to.

## Open work handed on from the phase-4 audit

- ~~`escalate_after_seconds` still ships on the wire from
  `internal/notification/api/{dto,mapper}.go`; the Go DTO is the last holdout of
  the `escalation` → `unacked_reminder` rename and is listed in
  `tools/lintvocab/baseline.txt`.~~ **Closed the hard way (git-bug `bd0fb1d`):
  the owner withdrew the unacked reminder, so the field is gone from the wire,
  the DB and the UI rather than renamed.** `escalation` remains a banned word —
  it was banned for dragging in rotas and ownership, which is true whether or not
  oto reminds anyone.
- ~~G1 (Go DTO → OpenAPI), G2 (schemathesis against a running server) and G4
  (generated valibot validators) are unbuilt.~~ **Closed.** All three are built
  and in CI; see the gate table in `README.md`. G2 is Go-native rather than
  schemathesis, for the reasons in `test/contract/server/doc.go`.
- `deploy/helm/oto/` now holds the chart of SPEC acceptance criterion 31: API and
  worker Deployments, Service, optional Ingress, ServiceAccount, ConfigMap,
  Secret with `existingSecret` support, `oto migrate` as a pre-install/pre-upgrade
  hook (the subcommand, so River's own migrator runs after goose) and an optional
  `oto bootstrap` post-install hook. Postgres stays external per ADR 0014.
  ~~**The criterion is still not fully met: no oto container image is
  published.**~~ **Closed.** `.github/workflows/release.yml` builds the
  root `Dockerfile` on every `v[0-9]+.[0-9]+.[0-9]+*` tag and pushes
  `ghcr.io/thulasi-ram/oto` (linux/amd64 + linux/arm64) tagged `X.Y.Z` — the tag
  `appVersion` resolves to — plus `vX.Y.Z`, `X.Y` and `sha-<commit>`, with the
  three linker stamps passed so `GET /api/v1/version` answers with a version and
  not `dev`. No `latest` is published while the chart is prerelease and tells
  operators to pin. ⚠️ The chart's default `image.repository` named the owner
  `thulasiram`, and the repository is `github.com/thulasi-ram/oto` — GHCR
  accepts the built-in `GITHUB_TOKEN` only for a package under the repository's
  OWN owner, so that path was one no workflow could ever have written to. It was
  corrected to `ghcr.io/thulasi-ram/oto` in the same change, and the workflow
  derives its path from `github.repository` rather than repeating a string that
  can drift. ~~The same `thulasiram` spelling survives in the Go module path
  and in `Chart.yaml`'s `home`/`sources` links; those are cosmetic where the
  image path was fatal, and which handle is canonical is the owner's call, not
  this change's.~~ **Wrong, twice over.** `Chart.yaml`'s `home`, `sources` and
  `maintainers[0].url` were not cosmetic: the release workflow makes the chart
  page reachable, and Artifacthub renders `sources` as its "go to source"
  button — a reader following either lands on a 404. They now read
  `github.com/thulasi-ram/oto`. All 23 `runbook_url` values in
  `deploy/prometheus/oto-rules.yaml` carried the same misspelling and are fixed
  too — an on-call engineer clicking through from a firing alert would have hit
  the same 404. What survives, correctly, is the Go module path itself
  (`go.mod` and every import) plus everything that quotes it verbatim
  (`.golangci.yml`'s `depguard` rules, `tools/lintreach/baseline.txt`,
  `CONTEXT.md`, and the module-path line and import sample in
  `docs/design/SPEC.md`) — renaming that is a large, invasive change out of
  scope here, and `deploy/helm/oto/values.yaml`'s comment, which quotes the old
  broken `ghcr.io/thulasiram/oto` deliberately, as history of the mistake
  already fixed there. What remains is operational, not missing code: **no
  `v*` tag has been pushed yet**, and a GHCR package is private until somebody
  makes it public (or `imagePullSecrets` is set, which the chart already
  projects into all four pod specs).
- `deploy/prometheus/` is no longer empty: `prometheus.yml` plus `oto-rules.yaml`
  (the path SPEC §H.8 names) are wired into `docker-compose.yml` and `just infra`,
  and every rule's `runbook_url` points at a page in `docs/runbooks/`, which now
  holds one page per `oto_*` metric the binary registers.
- SPEC AC-34 promises `oto_reconcile_divergence`,
  `oto_source_degraded_holds_total`, `oto_notification_suppressed_total`,
  `oto_delivery_attempts_total`, `oto_delivery_dead_total`,
  `oto_thread_recovered_total`, `oto_render_invalid_total` and
  `oto_check_violation_total`. **No collector in the tree constructs any of
  them** — the facts exist in tables and logs instead, and
  `oto_thread_recovered_total` shipped under the name
  `oto_thread_gap_recovered_total`. `docs/runbooks/README.md` lists where each
  one lives today so the gap stays visible rather than being discovered by an
  alert that never fires.

## Open defects found by implementers, awaiting SPEC amendment

- ~~`§G.7.2` ordering switch tests "root has not landed" before "thread is dead"~~
  **AMENDED.** `§G.7` now states the implemented order (dead → sequence →
  root; the `frozen` arm was removed by git-bug e5c060b and migration 00066,
  because nothing could ever enter that state), the bounded `MaxWait` escalation and its dead-letter
  outcome, and the three-phase send with the claim durable before the provider
  call. Reasoning is in ADR 0023.
- ~~`mention_on_reminder` lives in Slack channel config but neither
  `NotificationView` nor `RenderOptions` can carry it to the renderer. Worked
  around with a channel-scoped renderer copy; may want a port field instead.~~
  **CLOSED, BY DELETION.** git-bug `bd0fb1d`: the owner withdrew the unacked
  reminder and ruled the mention goes with it, so there is no mention surface
  anywhere in oto and nothing left to carry to the renderer. The
  channel-scoped renderer copy, `RenderOptions.Mentions`, `WithMentions` and
  the `For` method all went with it — the port field this item wanted was the
  right answer to a question that no longer has a subject.
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
7. **Retention defaults and cold storage (ADR 0024)** — was open question 5.
   Raw payloads 14d → **30d**, *derived* from the `alert_event_keys` idempotency
   horizon: past it a stored batch cannot be replayed without appending the
   timeline twice, so a longer window keeps bytes nothing can act on. Events stay
   **13 months**, but on ADR 0014's scale ceiling rather than the year-on-year
   reason that was recorded — year-on-year is served by `alert_quality_daily`,
   which is never reaped. Retention destroys the *narrative*, never the *record*:
   `alerts`, `alert_cases`, `rule_snapshots`, `notifications`,
   `notification_deliveries` and `channel_threads` have no reaper at all, so
   every clause of README's promise outlives both windows. What does die at 13
   months is human comments and unack notes, which live nowhere else — which is
   why cold-storage export is **scoped and unbuilt** rather than ruled out.
   Also fixed: `partitions.manage` read a process-global config and ignored
   `orgs.settings` entirely, so the per-org keys were enforced nowhere. It now
   drops at the **maximum** of every org's window — retention is a floor, never
   a ceiling.
8. **Tuning defaults derived from a real rule corpus (ADR 0026)** — was open
   question 4. `refire_grace` 600s → **1200s**, `group_close_delay` 300s →
   **1200s**, `flap_window` 1800s → **7200s**; `flap_threshold` stays **5** and
   `flap_digest_interval` stays **900s**. The evidence is two measured tables,
   not a plausible cluster: `group_interval: 5m` is the one Alertmanager number
   the ecosystem does not override (upstream, kube-prometheus-stack,
   kube-prometheus, OpenShift, Grafana all ship it), and `for: 15m` is the mode
   *and* median of the 155 rules kube-prometheus-stack 88.2.0 ships, with
   15m+10m+5m being 75.5% of them.
   Three defects, all arithmetic rather than taste: (a) `refire_grace`'s clock
   starts at the UPSTREAM `ended_at`, so a re-fire must pay the rule's whole
   `for:` again — 600s was unreachable for 76% of real rules; (b)
   `group_close_delay` was *shorter* than `refire_grace`, so the grace reopened
   the case and the closed generation posted a new Slack root anyway,
   which is exactly what the grace exists to prevent; (c) the flap ceiling has a
   TRANSPORT floor the old arithmetic missed — a cycle costs
   `group_interval + max(group_interval, for)`, so a 30-minute window held at
   most 6 transitions even for a rule with no `for:`, and 5-in-30m could never
   be crossed by anything. Bounds were re-checked and NONE moved: the
   `flap_window` floor of 300s is inert at `group_interval: 5m` and is kept
   because the one real capture in this repo runs `group_interval: 30s`, where
   it is exactly right.
   Also found and NOT fixed here: ADR 0020 grants `refired` a broadcast because
   its quiet form is invisible, and §H.6's verbosity table then drops that reply
   entirely at `firing_only` / `firing_and_resolved`. On those channels a
   re-fire inside the grace is silent. Recorded as an open defect in ADR 0026 —
   changing what `firing_only` means is a product decision, not a tuning one.
   **That defect was overtaken rather than fixed: ADR 0040 retired T8, so nothing
   produces `refired` at all and there is no reply for verbosity to drop.** The
   product question it named — how loud a re-fire should be on a quiet channel —
   is still open, and it is no longer a question about `group_close_delay` —
   ⛔ that setting is deleted (see decision 9's follow-up). A conversation now
   holds exactly one Case, so a re-fire is always a new root: loud, always, with
   no knob. Whether that is right is the open ruling recorded in SPEC §B.5.
9. **A Case is `open` or `closed`, and it is never reopened (ADR 0040)** — and
   this one **reverses shipped behaviour** rather than tuning it, so read §6 of
   that ADR before touching it. `alert_cases.state` held all four §B.2 words;
   three of them were facts about the Alert or restatements of a neighbouring
   column, so the column is now `open | closed` and the four-way reading is
   derived (migration 00054, lossless in both directions). With it went
   transition **T8**: a re-fire used to reopen the closed episode when it landed
   inside `refire_grace`, carrying its acknowledgement across a gap in the
   firing. Every re-fire is now T7 — the next `seq`, unacknowledged — because an
   acknowledgement is a receipt for one firing and the second firing is not the
   one that was signed for. `reopen_count`/`reopen_of` are dropped,
   `case.reopened` is retired on 00051's terms, and `refire_grace` survives as an
   inert setting whose future is deliberately undecided.
   ⛔ **OVERTAKEN — `refire_grace_s` IS DELETED** (git-bug `7287b28`, migration
   `00071`). "Deliberately undecided" was decided: the setting had no reader
   outside its own CRUD, and the owner's standing ruling is *delete, do not
   retire*, because a knob that clamps, validates and reports an origin while
   changing no outcome is a vocabulary entry the next person has to rule out.
   `group_close_delay_s` went with it — it timed the close of an `alert_groups`
   generation, and `00069` deleted the entity. Decision 8's two headline numbers
   therefore describe settings that no longer exist; the *derivation* stands as
   the record of how they were reached.

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

Retention defaults were question 5 here. They are now decision 7 above (ADR
0024). `refire_grace` and the flap thresholds were question 4. They are now
decision 8 above (ADR 0026). Both were decided without the owner and both are
written to be cheap to overturn — and `refire_grace` was in fact overturned:
see the ⛔ follow-up on decision 9.

4. **How loud should a re-fire be?** New, and it is the successor to the
   question decision 8 left open. With `alert_groups` deleted a conversation
   holds exactly one Case, so 500 firing alerts open 500 Slack threads by
   construction and the only collapse mechanism left is the **opt-in digest**.
   `test/load`'s `O(groups)` bound and its *"chatter ≤ alerts/10"* ratio are
   deleted with tombstones. This needs a product ruling; it cannot be tuned,
   because there is no longer a number to tune.
