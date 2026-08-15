---
title: 0017 — Alertmanager-style matchers, not CEL; and a filter AST that keeps the door open
---
**Status:** Accepted · 2026-08-08
**Relates to:** [0013](/adr/0013-alert-first-scope-boundary/) (policies route signals, never people),
[0014](/adr/0014-postgres-only-no-analytical-store/) (indexed queries are what make Postgres-only work)

## Context

keep.dev exposes CEL for searching alerts, and the question is whether oto should follow.

Keep's reason is sound *for Keep*: they ingest from roughly a hundred heterogeneous providers, so an
alert is effectively a schema-less JSON document, and a general expression language over arbitrary
fields is the natural fit. oto's model is Prometheus-shaped with a well-defined label set. The stronger
schema is precisely what makes indexed SQL viable, and it removes most of Keep's motivation.

There is a real argument on the other side and it should not be dismissed: **Kubernetes users now know
CEL**, from CRD `x-kubernetes-validations` and ValidatingAdmissionPolicy. `cel-go` is well maintained,
sandboxed and non-Turing-complete with bounded evaluation. "Users won't know it" is a weak objection
for our specific audience.

The strong objection is different. **CEL does not translate to SQL.** Evaluated in the application, it
requires fetching rows to test them — a scan, defeating keyset pagination and every index the schema
was built around. The real-world answer is a CEL→SQL translator, and only a *subset* of CEL survives
translation. That ships a language whose published semantics do not match what is actually supported,
and users discover the boundary at unpredictable places. That gap is a permanent support tax, and it is
what makes this look cheaper than it is.

## Decision

**Search uses Alertmanager-style label matchers plus structured field filters.**
`{namespace="payments", severity=~"critical|warning"}` is the native idiom of our audience, translates
directly to indexed SQL and GIN containment, and covers the overwhelming majority of real queries.
Combined with saved views, it is the v1 search surface.

**Filters are represented internally as an AST**, not as a bag of query parameters. This is the cheap
decision that keeps the door open: matchers compile to the AST today, and if CEL earns its place later
it compiles to the same AST. The translatable subset then becomes an explicit, testable capability list
rather than an emergent surprise.

**If CEL is ever added, anything untranslatable is rejected at parse time** with a precise message —
never silently degraded to a scan. A query that cannot use an index must fail loudly, because the
alternative is a filter that works in staging and times out during an incident.

**On the notification-policy path, be more conservative still.** An expression language on the routing
path is a foot-gun: a policy that errors at evaluation time means an alert is not routed. Matchers are
almost certainly sufficient. If richer predicates are ever added there, three properties are
non-negotiable — evaluation must be **total** (never errors), failure must be **fail-open** (notify
anyway; never silently drop), and it must be exercised through the existing dry-run preview. The scope
boundary continues to bind: predicates over signals only, never people, teams, schedules or times of
day.

The case that would justify going further is predicates over *derived* state — "only notify if this has
fired more than three times in 24h". That is better served by a small set of named conditions than by a
general language.

## Consequences

- Users cannot express arbitrary boolean logic over alert fields in v1. Matchers plus structured
  filters plus free-text search is the ceiling, and some genuinely reasonable query will not be
  expressible. Accepted.
- Every filter is index-backed, so search latency stays predictable as the event table grows — which is
  one of the load-bearing assumptions of [0014](/adr/0014-postgres-only-no-analytical-store/).
- The AST is extra indirection for a v1 that has only one front end compiling to it. That cost is
  deliberate and small.
- Saved views become the answer to "I need this complicated query often", which is usually the real
  request behind asking for an expression language.

## Alternatives rejected

**CEL as the primary search surface, with a CEL→SQL translator.** Rejected: the supported subset
diverges from the documented language, and the failure mode is a scan during an incident. Reconsider
only behind the AST, with the capability list defined first.

**Application-side CEL evaluation over fetched rows.** Rejected outright — it discards pagination and
indexing, and its cost grows with exactly the data volume that matters.

**A bespoke query language.** Rejected: all of CEL's translation cost, none of its familiarity, plus a
grammar to maintain.
