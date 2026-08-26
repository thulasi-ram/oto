---
title: oto_stream_listener_reconnects_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | none |
| Registered in | `internal/streaming/service/metrics.go` |
| Alertable | **yes**, on repeated reconnects in a short window |
| Rule | `OtoStreamListenerFlapping` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Times the LISTEN connection to Postgres was re-established. **Each one is a window of lost
notifications** — doorbells rung while nobody was listening — which the reconciling poll is designed
to cover.

The bridge holds one dedicated connection from the general pool and never returns it, because a
connection holding LISTEN cannot serve queries. It is taken from the general pool rather than the
ingest pool so that stream plumbing can never eat ingestion capacity.

## What a rising count means

One or two at startup or after a database restart is expected. Repeated reconnects mean the
connection keeps being killed, and every gap is covered only by the poll — so the UI's latency
degrades from "instant" to "one poll interval", and
[`oto_stream_poll_recovered_total`](/oto/runbooks/oto_stream_poll_recovered_total/) rises with it.

Usual causes: a connection-lifetime setting expiring the LISTEN connection, a pooler (PgBouncer in
transaction mode **cannot** carry LISTEN), an idle-session timeout, a network path with an
aggressive idle cutoff, or Postgres restarting/failing over.

## What to check

1. Postgres logs for terminations, and `pg_stat_activity` for the listening backend.
2. Is there a pooler between oto and Postgres? Transaction pooling breaks LISTEN outright — oto
   needs a session-level connection.
3. `idle_session_timeout`, `tcp_keepalives_*`, and any pool `max_conn_lifetime` on oto's side. A
   lifetime setting will cheerfully recycle the listening connection on a schedule.
4. `GET /readyz` → `pools`: is the general pool exhausted such that reacquiring is slow?

## What to do

- Route the LISTEN connection direct to Postgres, not through a transaction-mode pooler.
- Exempt it from idle and lifetime timeouts, or raise them well past the notification cadence.
- If it is failover, nothing to fix — confirm the poll covered the gap and move on.
