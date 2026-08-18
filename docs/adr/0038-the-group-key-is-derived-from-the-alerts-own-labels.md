# 0038 — The group key is derived from the alert's own labels

**Status:** Accepted · 2026-08-18

**Amends:** [0005](0005-durable-group-key-owns-the-slack-thread.md) — the group key's inputs, and
nothing else. ADR 0005's central claim survives **unamended**: the AlertGroup generation still owns
exactly one chat thread.

## Context

ADR 0005 made `group_key = H(org_id, source_id, receiver, canon(groupLabels))` and, in the same
breath, made the group the owner of a Slack thread. The consequence went unnoticed: **the identity
of oto's most visible output was computed from a grouping oto neither chooses nor can reproduce.**

Three demonstrations, each observable in the code before this change:

1. **Matchers can only see group labels.** There is exactly one policy match entry —
   `notification/service/notify.go` → `policy.go` — and it is fed `snap.Group.GroupLabels`; the
   reminder path uses the same. That is correct rather than lazy: group labels are the only label
   set true of *every* member. But it means a policy matching `namespace` matched **nothing** unless
   the operator had put `namespace` in `group_by`, and it failed quietly — as a `no_policy`
   suppression, not as an error.
2. **The reconciler could not build a group at all.** `GET /api/v2/alerts` returns no grouping, so
   reconciler-sourced groups got an empty receiver and no group labels
   (`db/migrations/00008_grouping.sql`, citing SPEC §C.4). oto had two ingest paths and two
   different answers to "which thread does this belong to". The path worked at all only because the
   reconciler made a **second** HTTP call, to `/api/v2/alerts/groups`, purely to recover the labels.
3. **`continue: true` yielded two threads for one alert.** `receiver` was in the key, so one alert
   reaching two receivers occupied two groups at once — which `alert_group_members`' `PRIMARY KEY
   (group_id, occurrence_id)` permits by design. That many-to-many shape is itself the proof that
   group membership was never an identity claim: a thing cannot be identical to two different things.

Editing `alertmanager.yml` shifted the whole key space — new groups, new threads, the in-flight ones
orphaned. An Alertmanager group is a declared notification-**batching** boundary, not a statement
that its members are related.

## Decision

```
group_key = "gk_" || base32hexLower(sha256(
      field(org_id_bytes) || field(cluster_key) || canon(SplitLabels(alert labels)) )[0:16])

SplitLabels(ls) = { alertname: ls.alertname }  ∪  { namespace: ls.namespace } if non-empty
```

- **Computed from the alert's own labels**, identically on the webhook path and the reconciler path.
  Every axis is present on every alert on both paths, which is precisely what `receiver` and
  `groupLabels` were not.
- **Fixed, not configurable.** A tunable split key reinvents `group_by` inside oto and re-inherits
  the problem it was built to escape. SPEC's `correlation` charter already words the requirement as
  "machine-derived groupings… with a **stated** algorithm" — stated, not configured.
- **`alert_groups.group_labels` becomes `SplitLabels`**, i.e. oto's own axes. This is what makes a
  policy matching `namespace` work regardless of the operator's `group_by`.
- **`cluster` is an axis but not a label.** It is resolved from the source's configuration,
  participates in Alert identity (§C.2) and stays first-class as `cluster_id`/`cluster_key`. Writing
  it into `group_labels` would invent a label the upstream never sent.
- **An absent `namespace` is its own partition, not an error.** It is the *absence* of the key, which
  `canon()`'s length prefixes make injective. An **empty** namespace folds onto absent, because
  Prometheus treats the two as equivalent and `alerts.namespace` stores NULL for both.

### What is deliberately not an axis

| omitted | why |
|---|---|
| `severity` | an escalation is the same problem getting worse, and a group's severity is an aggregate that only means something if both live in one group |
| `pod` / `instance` | that is the thing being grouped |
| `service` | omitted until evidence says otherwise. Adding it later **splits**, which is the safe direction |
| `receiver`, `source_id`, AM's `groupLabels`, AM's `groupKey` | the whole point: they are upstream's choices, not oto's |

## Consequences

- **Dropping `receiver` merges routes that deliberately separated the same alerts.** Two receivers
  fed by `continue: true` used to produce two threads for one alert; they now produce one.
  `cluster_key` is what must distinguish alerts that belong in different conversations — which it
  should be anyway, since alert identity is already `(org, cluster)`. **This is the trade this ADR
  exists to record.**
- **Dropping `source_id` merges HA Alertmanager replicas.** Two replicas are two Sources sharing one
  Cluster and reporting the same alert set; keying by the replica that happened to deliver the
  webhook split one grouping into two generations and therefore two threads. This one is strictly a
  fix.
- **A label-based split is structurally safe.** Alert identity *is* the label set, so an alert's
  split key is immutable for its whole life and no alert can ever move between threads — which
  matters, because Slack threads cannot be re-parented. The residual risk is choosing too *finely*
  up front, not re-parenting.
- **Splitting is the only safe direction.** It is decidable at receipt from data in hand; merging
  needs alerts that have not arrived yet, so it would require re-implementing `group_wait` inside oto.
- **One webhook is no longer one generation.** A `group_by: [cluster]` envelope carrying six
  alertnames now resolves six groups and six threads. That is the routing precision this buys, and it
  is also the number the replay harness exists to check before anyone believes it.
- **Existing groups are not re-keyed and are not migrated.** They keep their threads and their
  history and close through the ordinary sweep; the next observation opens a new generation under
  the new key. See `db/migrations/00050_derived_group_key.sql` for why a backfill is neither
  computable nor safe.
- **The reconciler's second Alertmanager call is gone**, along with the client's `AlertGroups` read.
  oto no longer mirrors Alertmanager's grouping anywhere.
- **Two delivery drills inside `group_close_delay` now share one generation.** Each still has its own
  Alert and case; isolating them again means giving each drill its own value on an *axis*, and
  inventing one for a single caller is what this ADR forbids. Recorded in
  `internal/drill/domain/payload.go`.
- **`alert_groups.receiver` and `source_group_key` become provenance.** They are recorded, rendered
  in a card's footer, and are part of no identity. Dropping the columns is an API-contract change
  (`AlertGroup` schema, the outbound webhook envelope, the `listAlertGroups` `receiver=` filter) and
  is deliberately not bundled here.

### ⚠️ The axes are as-yet unvalidated against production payloads

The key is computable from `ingest_batches.payload`, retained 30 days. `tools/groupreplay` replays
stored bodies, computes the derived key over each alert, and reports the group-size distribution
alongside the thread count Alertmanager's own grouping produced. **It has only been run against
synthetic fixtures.** Until it has been pointed at a real database, "the axes are right" is a design
argument and not a measurement.

## Alternatives rejected

| considered | rejected because |
|---|---|
| Keep AM's grouping, add an oto-side override | a tunable split key is `group_by` again, inside oto, with the same failure mode and a second place to be wrong |
| Make oto opaque to AM's grouping entirely, including `group_wait` | AM's batching is free noise reduction the product's promise rests on; going opaque before a grouping layer exists ships a regression |
| Merge across AM envelopes | needs alerts that have not arrived; re-implements `group_wait` inside oto |
| Add `service` as a fourth axis now | no evidence. Splitting later is safe; un-splitting is not |
| Keep `receiver` in the key "just for safety" | it is the field that produced two threads for one alert, and safety that depends on an operator's file is not safety |
| Re-key existing groups in a migration | not computable from `alert_groups` alone, and it would re-parent live Slack threads |
| Promote the axes to indexed columns on `alert_groups` | `namespace` is nullable, so every bucketing read wraps it in `COALESCE` and a plain btree on the bare column cannot range over the wrapped form. Hashing in Go and storing one opaque non-null key removes the question |
