# 0007 — Webhook response contract: 202 or 503. Never 4xx, never 429.

**Status:** Accepted · 2026-08-07

## Context
Alertmanager's retry classifier is unambiguous:

```go
if statusCode/100 == 2 { return false, nil }              // success
retry := statusCode/100 == 5 || slices.Contains(r.RetryCodes, statusCode)
```

The webhook integration sets **no** `RetryCodes`. Therefore:
- **2xx** = success, recorded in the nflog, **body ignored**.
- **5xx** = retried.
- **4xx (including 429)** = failure, **not retried, permanently lost.**

Retries are unbounded but bounded by the group's context deadline:
`max(group_interval, 10s) + peer_position × cluster.peer-timeout` — about **5 minutes** with
common defaults. `notify.MinTimeout` is 10 seconds, which is the hard floor on how long we may take.

The trap is obvious and common: an overloaded service returns 429 or 503-shaped 4xx, and the
alert is silently deleted forever — during the exact window when the customer's cluster is on fire.

## Decision

| Condition | Status |
|---|---|
| Durably persisted (or a recognised duplicate) | **202** |
| Overloaded, pool exhausted, statement timeout, queue insert failed | **503 + `Retry-After: 5`** |
| Missing/invalid/revoked or wrongly-scoped ingest token | 401 |
| Body over 8 MiB | 413 (recorded in `ingest_rejections`) |
| Undecodable body | 400 (recorded in `ingest_rejections`) |
| **Anything transient** | **NEVER 4xx. NEVER 429.** |

The handler does exactly three things inside one short transaction on a **dedicated ingest
connection pool** with a 500 ms acquisition timeout and a 2 s statement timeout: insert the dedup
key, insert the raw batch, `river.InsertTx` the processing job. **No outbound network call is
permitted on this path.** Target p99 < 250 ms; hard ceiling 5 s.

A 2xx is a promise. Never return 2xx for a payload we failed to persist.

## Consequences
- Backpressure has a designed, correct channel: shed load deliberately with 503 and let
  Alertmanager's own ~5-minute retry budget absorb it.
- The ingest pool is sized separately from the read/UI pool, so a slow dashboard query can never
  starve ingestion.
- Storms become a queue-depth problem rather than a data-loss problem.
- We cannot signal anything back to Alertmanager on success — the body is ignored — so all
  operator feedback lives in oto's own UI and metrics.

## Alternatives rejected
- **429 for rate limiting:** the single most tempting mistake here; it deletes the alert.
- **Process synchronously and only notify asynchronously:** simpler to debug and gives
  read-your-writes, but couples webhook latency to write contention during storms and leaves no
  replay artefact.
- **An on-disk spool ahead of Postgres:** genuinely valuable, but Alertmanager's retry budget is
  a sufficient buffer for v1, and a local WAL is a second durability story to get right.
