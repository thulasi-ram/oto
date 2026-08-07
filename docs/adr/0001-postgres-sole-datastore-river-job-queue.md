# 0001 — PostgreSQL is the sole datastore; River is the job queue

**Status:** Accepted · 2026-08-07

## Context
oto needs a system of record, an append-heavy event log, a job queue, a rate limiter and a
pub/sub bus. The obvious instinct is Redis for the queue, ClickHouse or Loki for events, and
Postgres for entities. The entire go-to-market for a self-hosted alerting tool is the install
experience: `helm install` plus one database. Every additional dependency is an operational tax
paid by the buyer, and it is exactly the footprint advantage oto has over Keep.

The decisive technical point is the **dual-write problem**. With an external broker, the state
change commits and the enqueue can fail (notification lost), or the enqueue succeeds and the
commit rolls back (duplicate notification). Both are unacceptable in a product whose premise is
"never lies".

## Decision
Postgres only. No Redis, no Kafka/NATS, no ClickHouse/Loki/Elasticsearch, no TimescaleDB.
The job queue is [River](https://riverqueue.com) — pgx-native, `SELECT … FOR UPDATE SKIP LOCKED`,
with `river.InsertTx(ctx, tx, args)` placing the job in the **same transaction** as the domain
write. That is a transactional outbox with none of the outbox boilerplate.

Volume is handled with native declarative partitioning: `alert_events` monthly, `ingest_batches`
daily, `ui_events` hourly. Retention is `DETACH` + `DROP`, never `DELETE`.

## Consequences
- Exactly one backup story, one connection-pool story, one failure mode.
- Enqueue is exactly-once with respect to the domain write. This is worth more than any
  throughput an external broker would add — the ceiling here is Slack's 1 msg/s/channel, not the queue's.
- Long transactions holding a job open would wreck autovacuum, so the delivery worker MUST
  claim-then-work-then-ack with short transactions and MUST NOT hold a transaction across the Slack call.
- Completed-job retention and queue bounds are mandatory, not optional.
- Revisit only above roughly low-thousands of jobs/sec. `db.Enqueuer` is an interface so the swap is possible.

## Alternatives rejected
- **asynq (Redis):** faster, but no transactional enqueue, and a second datastore in a
  one-install product.
- **Kafka/NATS:** correct at 100× the volume; an operational tax paid for nothing at ours.
- **TimescaleDB:** genuinely attractive (hypertables, compression) but it is still an extension
  to install, version and support. Native partitioning is sufficient at 50M events/month.
- **ClickHouse for the event stream:** doubles the operational surface and breaks the
  one-install promise, which is a competitive advantage worth protecting.
