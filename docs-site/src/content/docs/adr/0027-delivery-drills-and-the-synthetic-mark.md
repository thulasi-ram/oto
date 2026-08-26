---
title: "0027 — Delivery drills: a synthetic alert through the real pipeline, and the mark that keeps it out of the numbers"
---
**Status:** Accepted · 2026-08-09
**Decided WITHOUT the owner.** See *How to overturn this*, below.
**Relates to:** [0024](/oto/adr/0024-retention-defaults-and-cold-storage/) (what retention deletes, and the
row-level prunes this joins), [0009](/oto/adr/0009-rule-snapshot-versioning-at-fire-time/) (why the
rule-snapshot stage may honestly skip), [0013](/oto/adr/0013-alert-first-scope-boundary/) (FR-1),
[0014](/oto/adr/0014-postgres-only-no-analytical-store/) (why `alert_quality_daily` exists at all)
**Amends:** SPEC §D (new table `delivery_drills`, `alerts.synthetic`, `alert_groups.synthetic`,
`ingest_batches.mode` widened), §E (new `Drills` tag), §G.1 (the synthetic accept mode)
**Resolves:** git-bug `f7063f7` — *"Nothing can push a synthetic alert through the real pipeline, so
the delivery path cannot be proven"*

## Context

`POST /api/v1/channels/{id}/test` renders one card and hands it to the provider. It proves the token
works, the Block Kit is legal and the conversation accepts a card of that size. It proves nothing
else, because it touches nothing else: not ingestion, not alert identity, not grouping, not the
notification policy match, not the rule snapshot, not `channel_threads`, not the ordering gate, not
the delivery record.

Every failure mode oto has lives in exactly the stages it skips. A policy that matches nothing. A
thread that will not open because the bot was never invited. A scope Slack accepts and silently
ignores. A renderer producing a block the outbound validator refuses. On a fresh install all of those
are invisible until the first real page — and the most common one of them, *no notification policy
exists yet*, makes an install that is working perfectly look completely dead.

An operator's question on day one is not "do I have `chat:write`". It is **"will an alert actually
reach my channel, in a thread, with the right card, and be recorded?"** Answering it today requires
breaking something in production.

## Decision

### 1. A drill runs the real chain, through the real front door

`POST /api/v1/drills` manufactures an Alertmanager v4 webhook body and hands it to
`ingestion/service.Service.Accept` — **the same object the webhook handler calls**. From there
nothing knows a drill is happening: the accept transaction, the transactional outbox, the decoder,
the bounds, the redactor, `ingest.process_batch`, the §B.3 state machine, §C.4 grouping, `enrich.run`,
`notify.evaluate`, the policy match, `channel_threads`, the ordering gate and `deliver.dispatch` all
run exactly as they do for a real alert.

There is no shortcut and no fast path, and the composition-root adapter that wires it says so: the
moment that port points anywhere else, a passing drill stops being evidence.

### 2. The mark is PROVENANCE, not payload — `ingest_batches.mode = 'synthetic'`

This is the load-bearing decision and it was the hard part of the issue.

**Rejected: a reserved label.** Two independent objections, either of which is fatal.

- *It is forgeable.* A label arrives from the wire. Any Alertmanager, anywhere, could set
  `oto_synthetic="true"` and evict its own alerts from oto's statistics — silently, permanently, in
  the table the hygiene report is sold from. A tenant-visible switch that deletes a tenant's own
  numbers is not a marking scheme.
- *It changes identity.* Labels participate in `alert_key` (§C.2). Marking an alert with a label
  makes it a **different Alert** from the same rule firing without it. The mark would not annotate
  the row; it would fork it.

**Rejected: a distinct source kind.** It would work, and it answers the wrong question. A drill's
whole value is that it takes the operator's *own* source — their cluster identity, their
`inject_labels`, their `ignore_labels`, their redaction, and therefore the policies their real alerts
meet. A synthetic source would prove that a synthetic source works.

**Chosen: a column, set from the mode of the batch that carried the alert.** `Mode` is written by the
code path that ACCEPTED the batch. It appears in no body and no header; no upstream can cause one.
It propagates `ingest_batches.mode` → `Observation.Synthetic` → `alerts.synthetic`, and separately to
`alert_groups.synthetic` at generation-open time.

The stated cost of a column is real and was paid in full: *it needs a migration, and it must be
threaded through every read that aggregates.* §3 is that list.

`alert_groups.synthetic` is a second, denormalised copy. It exists because the dashboard counts open,
closed and storming groups straight off that table, and reaching `alerts` through
`alert_group_members` for every group in the window turns one indexed count into a nested loop.
`notification_deliveries` deliberately gets **no** column: it walks the two FKs it already has
(delivery → notification → group), because a fourth denormalised boolean is a fourth place a future
writer can forget.

### 3. Every aggregate that had to change

| Read | File | Change |
|---|---|---|
| `alert_quality_daily` — `occ` CTE | `stats/repository/rollup.go` | `AND NOT a.synthetic` |
| `alert_quality_daily` — `notif` CTE | same | `AND NOT a.synthetic` |
| `alert_quality_daily` — `flaps` CTE | same | `AND NOT a.synthetic` |
| Dashboard overview — alert counts | `stats/repository/stats.go` | `AND NOT synthetic` |
| Dashboard overview — group counts | same | `AND NOT synthetic` on `alert_groups` |
| Dashboard overview — delivery counts | same | join → `notifications` → `alert_groups`, `AND NOT ag.synthetic` |
| Alert list (`GET /alerts`) | `alerts/repository/alert.go` | `applyAlertFilter`: nil filter ⇒ `NOT synthetic` |
| Alert roll-ups (`GET /alerts/rollups`) | same | shares `applyAlertFilter` |
| Label-name typeahead | same | `AND NOT a.synthetic` |
| Label-value typeahead | same | `AND NOT a.synthetic` |
| Related alerts (enrichment) | `enrichment/repository/alertread.go` | `AND NOT a.synthetic` |

Reads deliberately **not** changed, with the reason: per-alert-id lookups (`GetByID`, occurrence
open, snooze create) address one row the caller already named; per-group rollups
(`grouping/repository/member.go`) count the members of one group, and a drill's group contains only
its own alert; `notification/repository/snapshot.go` builds the card for one group and a drill's card
must render through the identical path.

**The alert-list default is EXCLUDE, which is the opposite of `snoozed`.** A snoozed alert is a real
thing happening in a real cluster and hiding it is how an incident is lost (§B.8.6). A synthetic alert
is a rehearsal — nothing fired anywhere — and including it would put oto's own plumbing into the
customer's estate. `?synthetic=true` is the explicit way to see one; it is what a drill's result
screen links to, not a chip in the filter bar.

### 4. Disposal: a row-level prune inside `retention.prune`, and it does not contradict ADR 0024

ADR 0024 promises that `alerts`, `alert_occurrences`, `alert_groups`, `notifications`,
`notification_deliveries` and `channel_threads` are **never reaped** — *"retention deletes the
narrative, never the record"*.

That promise is about **the record of an upstream signal**. A drill records none: nothing fired, no
cluster was involved, oto manufactured every byte to answer a question an operator asked by pressing
a button. Deleting it destroys no history, because none was made. Drills therefore sit in ADR 0024's
*other* category — the side tables pruned **by row** because they cannot age out any other way, which
that ADR already lists as `ingest_dedup`, `sessions` and `enrichment_cache`. Drills join that list as
a fourth. **No partition is dropped and no row recording something a cluster actually did is
touched.**

Mechanics:

- A drill's synthetic rows are deleted **24 hours after its verdict is frozen** (`DisposeAfter`), by
  `retention.prune`. Not immediately, so an operator can still click through from the result to the
  alert row and the group timeline; 24 hours because nothing else needs them and every list excludes
  them anyway.
- `DELETE /api/v1/drills/{id}` does it now, for an operator who is finished looking.
- Deletion is **scoped by id, never by predicate**. There is no `DELETE … WHERE synthetic` anywhere
  in this codebase; both statements additionally carry `AND synthetic` as belt-and-braces, so even a
  corrupted manifest cannot delete a customer's alert.
- **The receipt survives.** `delivery_drills` keeps its row and its frozen staged outcome after the
  signal rows are gone — the same shape ADR 0024 designed for `retention_exports`. A year later an
  operator can still answer *"did the delivery path work last Tuesday"*; what they cannot do is find
  a fake alert in their history.
- The raw batch is **not** deleted: it ages out with `raw_retention_days` like every other batch, and
  it is the audit trail of what oto accepted at its own front door.
- The Slack message is not deleted and cannot be — oto never reads Slack back, and a `chat.delete` on
  a timer would be oto writing into somebody's channel history. The card says it is a drill.

### 5. The result names the stage that failed

The endpoint returns a **staged result**, not a boolean. Ten stages, read off the code:

`accept` · `process` · `identity` · `occurrence` · `group` · `rule_snapshot` · `policy` · `thread` ·
`ordering` · `delivery`

Three rules govern the report:

- **The drill is never told what happened; it looks.** Every stage's status is computed from the row
  the pipeline actually wrote. Nothing reports itself, so there is no callback to forget to fire —
  and a stage that silently stops writing its row is a stage the drill notices.
- **The first failure wins and the rest stay `pending`.** A chain that broke at `policy` has nothing
  to say about `thread`; eight cascading failures from one cause is how a diagnostic becomes noise.
- **`skipped` is honest, not generous.** Only `rule_snapshot` uses it: a drill's alert matches no
  Prometheus rule because oto did not write one in anybody's cluster, so there is nothing to capture
  and a green tick would be false confidence.

`timed_out` is a **different verdict from `failed`**. "Slack rejected the card" and "nothing picked
the job up in ninety seconds" send an operator to completely different places — the second usually
means no worker process is running, which no per-stage error could ever say.

### 6. `POST /channels/{id}/test` is KEPT, unchanged, and is not stage 0

The three options were replace it, fold it in as stage 0, or keep it. Keeping it, and this is not
inertia:

- **They answer different questions at different prices.** The channel test is scoped to a
  *destination* and costs one API call and about 300 ms: *is this token still good, does this
  conversation exist, does my Block Kit validate*. A drill is scoped to a *source*, costs a real
  Slack message in a real channel and up to ninety seconds, and answers *would an alert get here*. An
  operator editing a channel's config wants the first; an operator whose pager is silent wants the
  second.
- **Folding it in as stage 0 would make it worse, not better.** A drill's destinations are chosen by
  the policy match at stage 7. There is no channel to pre-check at stage 0 — and pre-checking every
  configured channel would send an operator a card from a destination the policy was never going to
  route to.
- **Replacing it would remove the only affordance on the channels screen.** The channel test lives
  where channels are configured and is the immediate feedback on the form the operator just filled
  in. A drill on the sources screen cannot serve that.

The two are now explicitly complementary, and the copy on both screens says which question each
answers.

## Consequences

- **A drill posts a real message in a real channel.** That is not a side effect, it is the deliverable
  — a test that did not would prove nothing. It is mitigated by an unmistakable `OtoDeliveryDrill`
  alertname, an explicit annotation, and one-drill-per-source-at-a-time so one button cannot make two.
- **The group labels are wide, and the result screen has to be honest about it.** A policy matches the
  *group's* labels, so a narrow set would make drills unroutable by policies real alerts would match.
  A wide set maximises the chance the drill reaches the right channel — at the cost that a policy
  matching on `namespace` will match a drill even where the operator's `alertmanager.yml` does not
  group by namespace and a real alert never would. The result names the policy that matched, so the
  operator can check their `group_by`.
- **`ingest_batches_mode_ck` was widened, which is safe under N/N+1** (release N writes a strict
  subset of what N+1 permits) but is a one-way door: a rollback past 00039 leaves `synthetic` batches
  violating the narrowed check. The Down migration is written; a rollback with live drill batches
  needs them deleted first.
- **Two `synthetic` columns can now disagree** — `alerts` and `alert_groups` — if a future writer sets
  one and not the other. That is why the drill reports it as a **failed** `identity` or `group` stage
  rather than shrugging: the drill is the regression test for its own marking scheme.
- **A drill is one more thing that can fill a channel** if someone scripts the endpoint. The
  one-in-flight rule bounds it to one message per source per ninety seconds, which is well below any
  rate that matters, and every drill is attributed.

## How to overturn this

The cheap parts, in order of how easy they are to change:

- *`DisposeAfter` is wrong.* One constant in `drill/domain`. Longer costs disk and nothing else;
  shorter risks an operator losing the click-through before they use it.
- *The drill should be two-phase* (fire, then resolve) so it exercises `chat.update`, the thread reply
  and `reply_broadcast` rather than reporting the decision. This was scoped out because it doubles the
  Slack noise one button makes, and the "done when" of the issue asks for a card in a thread and a
  delivery record. If the owner wants the update path proven, the second phase is additive: one more
  accept and three more stages.
- *The mark should be a label after all.* This one needs an ADR arguing against §2 by name, and it has
  to answer both objections — forgeability and identity — not just one.
