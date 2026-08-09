# 0021 — Correctness and testing strategy

**Status:** Accepted · 2026-08-09
**Relates to:** [0001](0001-postgres-sole-datastore-river-job-queue.md) (River in Postgres — the
outbox is a real collaborator, never a mock), [0006](0006-reconciler-is-mandatory.md) (the reconciler
is the only way into `suppressed`), [0007](0007-webhook-response-contract.md) (the 202 promise),
[0011](0011-seven-layer-validation.md) (where each bound is enforced)

## Context

oto is a flight recorder. Its wedge is not throughput and not features — it is that **the record is
complete and true**: a 202 is a promise the payload is durable, `expired` is never dressed up as
`resolved`, and oto's silence is never indistinguishable from "no alert". That argues for proving
those specific paths rigorously.

Against that, heavy upfront testing slows iteration and couples tests to implementation. The
philosophy is classical, lean, grow-from-bugs.

There is a third pressure, and it is the one that actually bit. oto was written against a 4,852-line
specification and barely executed until late. That produced a defect class which grow-from-bugs
structurally cannot catch: **a feature that never worked has nothing to regress.** A setting the
operator sets that no branch reads (`OTO_INGEST_*`, `channels.config.transport`). A response field
the contract promises that no line populates (`delivery_summary` on four detail responses). A service
fully implemented, fully unit-tested, and never constructed (`NewInteractionService`). Fifteen-plus
instances were found by *running the product* for an afternoon. The response at the time was
`tools/lintreach` — a 1,375-line whole-module `go/types` analyzer that reads for the same shapes —
which now ships with a 328-entry baseline of accepted debt and cannot see the adjacent class (a field
that *is* written and read, with the wrong value).

Today the suite is 42 test files against 84,476 lines of production Go. Six of fourteen domains have
none: `grouping`, `rules`, `enrichment`, `streaming`, `stats`, `silences`. `test/harness` — the
package whose own doc comment claims to own "real Postgres via testcontainers, fake Slack, fake
Alertmanager, fake Prometheus and object builders" — is empty, and its testcontainers bootstrap is
instead copy-pasted into three unrelated files.

## Decision

**Classical (Detroit-school), lean, regression-driven, with a feature-acceptance floor.**

### 1. Real collaborators in-process; mock only true externals

Exercise the real service, repository and domain objects together. A test double is permitted **only
at an adapter boundary that leaves the process**:

| Real, always | Faked at the port |
|---|---|
| Postgres (testcontainers) | Slack Web API + interaction callbacks |
| River / `platform/jobs` (it is Postgres) | Alertmanager v2 and Prometheus v1 HTTP |
| Every `internal/*/service`, `repository`, `domain` | Enricher endpoints (HTTP, MCP) |
| The chi router and `httpx` stack | Outbound generic webhooks |
| `platform/clock` via `FakeClock` | — |

There is no Redis, no second datastore, and no message broker to stub
([0001](0001-postgres-sole-datastore-river-job-queue.md), [0015](0015-no-stream-processor.md)).
**Mocking a repository is not permitted** — a mocked DB lies about SQL semantics, and every invariant
that matters here (dedup on the identity key, per-thread sequence gating on advisory locks, the
`ON CONFLICT` that *is* the idempotency mechanism) lives in the SQL.

`test/harness` owns the one bootstrap: container, migrations, per-test schema isolation, the fakes
above, and object builders. The three hand-rolled `TestMain` copies collapse into it.

### 2. Lean in v1; grow the suite from real bugs

Enough integration coverage per module to catch glaring breakage, then **every fix lands with the
regression test that would have caught it.** Don't fight test infra; keep velocity. Renderers stay as
they are — pure functions with checked-in `testdata/*.golden.json` — because that shape is already
right.

### 3. Wedge exception — the record must be complete and true

Four targeted integration tests guard the paths that must not break, against real Postgres:

1. **The 202 is durable.** Kill the process between the webhook's commit and batch processing; on
   restart the raw batch is present and processed exactly once. A 2xx we cannot honour is the one
   failure that deletes an alert forever ([0007](0007-webhook-response-contract.md)).
2. **Never fabricate a resolution.** With `source_health.status != 'healthy'`, the reaper is blocked
   and no occurrence transitions to `expired`.
3. **Clamp, never reject.** A backward-skewed upstream `startsAt` clamps `ended_at`, flags
   `clamped: true`, records the skew, and does **not** abort the ingest transaction.
4. **Silence is distinguishable from success.** Every delivery for an alert dies; the alert detail
   response still reports `delivery_summary` with non-zero `dead` and a `last_error_class`.

This is *not* a deterministic-simulation framework. The hexagonal ports keep that door open at low
marginal cost if the wedge ever demands it.

### 4. Feature-acceptance rule — the floor that grow-from-bugs cannot reach

Every ingestion, alerts, notification, channels or sources feature lands with **at least one
functional test that executes the feature end to end through the real container and router**, fakes
at external ports only. **"Compiles, lints, and the JSON Schema validates" is not acceptance.**

Concretely, at land time:

- A new **config knob** lands with a test that asserts a *branch reads it* — two runs differing only
  in that knob must produce different observable behaviour. This is the `OTO_INGEST_*` class.
- A new **response field** lands with a test asserting it is populated with a value the test can
  predict, not merely present. This is the `delivery_summary` class.
- A new **service** lands reachable from `internal/app/container.go`, proven by a test that reaches
  it through an HTTP route or a registered River worker. This is the `NewInteractionService` class.

**The SolidJS UI is exempt** and stays grow-from-bugs, covered by the Playwright suite in `web/e2e/`.
A settings *form* is UI; the setting *behind* it is not, and falls under the config-knob rule above.

## Consequences

- Fast iteration; the test surface tracks real risk rather than line count.
- The four wedge paths get deliberate coverage that no amount of unit testing would have produced.
- **`tools/lintreach` stays**, as a second line rather than the only one. It catches the shape §4
  cannot: a declaration added between feature landings, in a module nobody is currently testing.
  §4 changes what it is *for* — it stops being the mechanism that substitutes for running the
  product and becomes a ratchet. The condition on keeping it is that **the 328-entry baseline only
  ever shrinks**: a gate with a permanent amnesty list is a gate nobody reads. Regenerating the
  baseline to absorb a new finding is not permitted; the finding is fixed, or the declaration is
  deleted. `tools/lintvocab` stays on the same terms for a different job — defending the scope
  boundary ([0013](0013-alert-first-scope-boundary.md)).
- Six untested domains are a backlog, not a blocker. Order by wedge proximity: `grouping` (it owns
  the thread), `rules` (the differentiator), `streaming`, `enrichment`, `silences`, `stats`.
- CI wall-clock rises with testcontainers. Accepted: one container per package, schema-per-test.

## Alternatives rejected

**Retire `lintreach` once §4 is in force.** Considered and rejected. The argument for retiring it is
real — two mechanisms for one property, a 328-entry amnesty list, and a permanent false-positive
maintenance cost (whole-struct conversions, embedded promotion, three reflection-based decoders
modelled by hand). The argument against is stronger: §4 is a discipline and `lintreach` is a
mechanism, and the failure mode we are guarding against is precisely a discipline lapsing. It stays,
under the shrink-only condition above. Revisit if the baseline stalls for two milestones — a ratchet
that never turns is just an amnesty.

**Mock repositories for speed.** Rejected. The invariants are in the SQL. A green suite over mocked
repositories would have been fully compatible with every one of the fifteen defects.

**Full DST harness in v1.** Proves the wedge hardest, but the harness cost lands before the product
has users. The ports keep it available later.

**e2e-first, defer the unit and integration tiers.** Fastest to write, weakest assurance for a product
whose entire claim is that its record is trustworthy.
