# 0014 — Postgres is the only store; no TSDB or column store in v1

**Status:** Accepted · 2026-08-08
**Relates to:** [0001](0001-postgres-sole-datastore-river-job-queue.md) (Postgres as sole datastore),
[0003](0003-alert-occurrence-event-separation.md) (the event stream), [0016](0016-mcp-enrichment-no-firehose.md)
(the rule that protects this decision)
**Resolves:** red-team memo objection 5 — *"Postgres is right for the alert entity and wrong for the
event stream. Same word, two workloads."*

## Context

The red team's objection is well aimed: alerts are OLTP and the event stream is time-series, and
conflating them bites somewhere around 50–100M rows. The natural reflex is to reach for a second
engine — ClickHouse, TimescaleDB, DuckDB over Parquet — before the pain arrives.

The reflex is wrong here, for one reason that is easy to miss: **oto's data is human-scale, not
machine-scale.** Metrics are millions of samples per second, which is precisely why Prometheus exists
and why it sits *upstream* of us. An alert is rare by construction. A fleet large enough to be
interesting produces on the order of 1–2M events per day at the pessimistic end, and if an
installation genuinely produces more, that is a broken alerting configuration — which is a thing oto
is supposed to *report on*, not a scale target to engineer for.

Every hot query is scoped `(org_id, alert_id | group_id)` plus a time range, which prunes to a single
monthly partition. The only genuinely analytical workload — firing frequency, firing-duration
percentiles, noise reports — is served by precomputed daily rollups (`alert_quality_daily`) written by
a background job.

## Decision

Postgres is the only datastore. The event tables are RANGE-partitioned by time; analytical questions
are answered from rollup tables, not from scans of the event stream.

We do **not** add a time-series database, a column store, or an embedded analytical engine in v1.

The escape hatch is designed in rather than promised: the event tables are append-only and partitioned,
so a cold partition can be `DETACH PARTITION`'d and exported to Parquet for DuckDB or ClickHouse
without touching the write path or the live schema. That makes an analytical store a **bolt-on, not a
migration** — which is exactly what justifies deferring it.

Revisit when any of these is observed, and not before:

- alert-list p95 above 200 ms with a warm cache,
- a rollup job overrunning its own interval,
- a single org exceeding roughly 10M events per month.

## Consequences

- One dependency. `helm install` plus a Postgres URL remains the whole deployment story, which the red
  team identified as a real asset against Robusta (whose UI is enterprise-gated) and Keep.
- Transactional integrity is preserved end to end: the outbox property in
  [0001](0001-postgres-sole-datastore-river-job-queue.md) depends on the job queue and the state change
  sharing one transaction, which a second datastore would break.
- Rollups are eventually consistent by minutes. Analytics screens must state their as-of time rather
  than implying live numbers.
- We accept that a very large single-tenant installation will hit the revisit triggers before a
  small one does, and that the first response will be tuning partitions and rollups, not a new engine.

## Alternatives rejected

**TimescaleDB.** Keeps one wire protocol and adds hypertables and continuous aggregates. Rejected
because it is an extension many managed Postgres offerings do not permit, which would silently
constrain where oto can be self-hosted — a high price for a scale we do not have.

**ClickHouse or a column store from day one.** Genuinely better at the analytical shape, and genuinely
a second datastore to operate, back up, migrate and keep transactionally consistent with Postgres.
Premature at our volume, and reachable later via the Parquet path.

**Embedded DuckDB over exported Parquet, now.** Attractive because it adds no service. Rejected as
solving a problem we do not yet have, while adding a second query dialect and an export-sync path to
keep correct. This remains the *preferred* option if a revisit trigger fires.
