# 0003 — Alert, AlertOccurrence and AlertEvent are three distinct entities

**Status:** Accepted · 2026-08-07

## Context
"Alert" is overloaded to uselessness: Prometheus calls a rule an alert, Alertmanager calls a
label set an alert, humans call a Slack message an alert. The brief treated occurrence and event
as one concept. The product promise — a Sentry-style, replayable lifecycle timeline — cannot be
built on a mutable row.

## Decision
Three entities:

- **Alert** — the *identity of a label set* within `(org, cluster)`. Created on first sight,
  survives resolution forever. Millions of rows, bounded by label cardinality, not by time.
- **AlertOccurrence** — one *contiguous firing episode*, `(alert_id, seq)`. A stateful interval
  with a start and an end. It is what you ack and what you count for MTTR.
- **AlertEvent** — an immutable record of *one thing that happened at one instant*. Append-only.
  Never updated, never deleted; aged out by `DROP PARTITION`.

Rule of thumb for engineers: **if you would ever want to `UPDATE` it, it is not an Event.**

Every AlertEvent carries **two** timestamps: `occurred_at` (the upstream's claim) and
`recorded_at` (oto's clock). Timelines **ORDER BY `(recorded_at, id)`** and **display
`occurred_at`**. Ordering by a remote clock is how you get a "resolved" event above a "firing" event.

## Consequences
- The timeline is an honest audit trail, because rendering it never mutates it.
- Current state is a cheap, narrow, heavily-indexed projection (`alerts`), so the hot list query
  never touches the event log.
- "This has fired 47 times this quarter" survives, because a re-fire is a new occurrence on the
  *same* Alert, never a new Alert.
- Costs one extra join for detail views and one extra table to keep consistent. The occurrence
  projection onto `alerts` is written in the same transaction as the transition.
- Clock skew becomes measurable and is surfaced as a feature (`oto_clock_skew_seconds`), because
  skew is a real problem in the customer's cluster.

## Alternatives rejected
- **Collapse occurrence into event:** forces either mutating timeline rows (destroying the audit
  trail) or having no natural key for "the thing this Slack thread is about".
- **Collapse alert into occurrence:** destroys the history that *is* the product.
- **One timestamp per event:** the headline feature would visibly lie whenever a cluster's clock drifts.
