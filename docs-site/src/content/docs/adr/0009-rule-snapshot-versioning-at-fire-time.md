---
title: 0009 — Rule definitions are snapshotted and versioned at fire time
---
**Status:** Accepted · 2026-08-07

## Context
"We fetch the rule from Prometheus" is a 40-line HTTP client, not a moat. `GET /api/v1/rules`
returns `expr`, `for`, labels and annotations; Grafana already renders the rule query next to
the alert. As a differentiator it survives about a Tuesday.

What nobody ships is **provenance over time**: *"this alert fired — show me the rule expression
**as it was at that moment**, and how the threshold has moved since."* Grafana shows the
*current* rule. Fetching the rule **and versioning it** upgrades a trivial HTTP client into a
real capability, and it directly serves the alert-hygiene wedge.

Matching an alert instance back to its rule is genuinely ambiguous: `alertname` is not unique
across rule groups or files; `alert_relabel_configs` can rewrite it; rule `labels:` may contain
`{{ $labels.x }}` templates so a naive subset check fails; `external_labels` are added on the way
out so subset checks must be one-directional; and in a sharded setup the wrong Prometheus returns
nothing.

## Decision
Rule definitions are **content-addressed and captured at fire time**, then bound to the occurrence.

```
rule_fingerprint = sha256(expr || for || keep_firing_for || canon(labels) || canon(annotations))
rule_key         = (source_id, rule_file, rule_group, rule_name)
```

`rule_snapshots` is UNIQUE on `(org_id, source_id, rule_fingerprint)`.
`alert_occurrences.rule_snapshot_id` is the binding. **Drift** is "the snapshot bound to this
occurrence has a different `rule_fingerprint` than the one bound to the previous occurrence for
the same `rule_key`" → emit `rule.definition_changed` with a structured diff, and deliver a
`rule_changed` Slack thread reply **regardless of channel verbosity**.

Two acquisition paths, in order:
1. **Primary — parse `generatorURL`.** Its `g0.expr` is URL-encoded PromQL as evaluated. Zero API
   calls, zero ambiguity, and it works in multi-Prometheus setups. `origin='generator_url'`.
2. **Enrichment — `GET /api/v1/rules?type=alert&rule_name[]=<alertname>&exclude_alerts=true`** on
   the Prometheus identified by `generatorURL`'s scheme+host, for `for:`, `keep_firing_for:` and
   the raw rule labels/annotations. `origin='prometheus_api'`. Cached aggressively.

Ambiguity (N>1 candidates) is scored on non-templated label subset plus annotation-key overlap
and recorded as `match_confidence ∈ {exact, probable, ambiguous, none}`. **`ambiguous` is
surfaced in the UI and in Slack. It is never silently guessed.**

`duration` and `keepFiringFor` are float **seconds** on the wire (600 means `for: 10m`).

## Consequences
- oto answers "this alert fired because someone lowered the threshold from 90 % to 70 % two hours
  ago" — a support ticket resolved before it is opened. This is the strongest specific feature
  in the product.
- The alert timeline gains a genuinely new event type that no competitor emits.
- Snapshots are content-addressed, so a rule that never changes stores exactly one row across
  thousands of occurrences.
- Requires network reachability to Prometheus (optional per source). Without it, `origin` falls
  back to `generator_url` (expr only) or `unavailable` — degraded honestly, never faked.
- oto is strictly **read-only** against Prometheus rules and will never become a rule editor.

## Alternatives rejected
- **Fetch the current rule on demand at render time:** shows today's rule next to yesterday's
  alert. That is the bug this ADR exists to prevent.
- **Match on `alertname` alone:** wrong whenever two files define the same alert name, and silently so.
- **Store the whole `/api/v1/rules` response per occurrence:** enormous duplication and no diffing primitive.
