---
title: oto_jobs_dead_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | `kind`, `queue`, `class` |
| Registered in | `internal/platform/jobs/metrics.go`; incremented in `internal/platform/jobs/worker.go` |
| Alertable | **YES — page on it.** The help string says `ALERT ON THIS` |
| Rule | `OtoJobsDead` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Jobs that **will never run again**. There are three ways in, and they are distinguishable:

| How | `class` | `dead_reason` in the log |
|---|---|---|
| A terminal error class — `permanent`, `config_invalid`, `auth_expired` — which never retries | that class | `terminal_error` |
| The retry budget was spent (13 attempts retryable, 21 rate-limited) | `retryable` / `rate_limited` | `attempts_exhausted` |
| The payload version is newer than this binary | `permanent` | `unknown_payload_version` — also on [`oto_jobs_unknown_version_total`](/runbooks/oto_jobs_unknown_version_total/) |

## Why every increment matters

Together with `unknown_version`, this is the metric that means **work was accepted and then
silently not done**. Depending on `kind`, one increment is: an ingested batch that never became
alerts, an alert that never became a notification, or a notification that never reached a human.
oto's silence must never be indistinguishable from "no alert" — this counter is what makes the
difference visible.

## What to check

1. The dead-letter log record. The default sink logs one ERROR per dead job with **the full
   payload** — deliberately, because it is the last copy:
   ```
   msg="jobs: job is dead, it will never run again"
   job_id= job_kind= queue= attempt= attempts= error_class= dead_reason= payload= error=
   ```
   Search for `dead_reason` to split the three cases above.
2. `river_job` for that kind, in `discarded` / `cancelled` state.
3. If `kind="deliver.dispatch"`: `notification_deliveries` for the row, and
   `GET /api/v1/deliveries/{id}` for the classified error. Also check the channel's health — a
   `config_invalid` or `auth_expired` death sets it, with a UI banner.
4. If `kind="ingest.process_batch"`: the batch is still in `ingest_batches`; it was accepted and
   is now unprocessed.

## What to do, by class

- **`auth_expired`** — the credential was revoked. This is the case the class exists to separate
  from "Slack is flaky": re-authorise the workspace (`docs/setup/slack.md`) and the banner clears.
- **`config_invalid`** — oto built something the provider refused. If the message is a render/block
  problem it is an **oto bug**: file it with the delivery id (`docs/setup/slack.md` §errors).
  Otherwise fix the channel configuration.
- **`permanent`** — read the error. A channel that no longer exists, a conversation oto was removed
  from, a payload the provider will never accept.
- **`retryable` / `rate_limited` exhausted** — the dependency was down for longer than the budget.
  Fix the dependency first, then decide whether to replay: the payload in the log is complete, and
  re-enqueuing is safe for every idempotent kind (ingest is `ON CONFLICT`; delivery is guarded by
  the §G.5 claim).
- Do not raise `MaxAttempts` as a first response. Twelve retries already spans minutes; if that is
  not enough the dependency is not flaky, it is down.
