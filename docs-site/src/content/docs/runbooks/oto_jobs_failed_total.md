---
title: oto_jobs_failed_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | `kind`, `queue`, `class` — the §G.6 taxonomy: `retryable`, `rate_limited`, `permanent`, `config_invalid`, `auth_expired` |
| Registered in | `internal/platform/jobs/metrics.go` |
| Alertable | **yes, on a sustained rate** |
| Rule | `OtoJobsFailing` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Job executions that returned an error. **One job can increment this many times** — once per failed
attempt — so this is a rate, not a population. A snooze is *not* a failure and is not counted here
(see [`oto_jobs_snoozed_total`](/runbooks/oto_jobs_snoozed_total/)); the whole point is that a busy thread
must not look like a broken one.

`class` is the same closed set stored in `notification_deliveries.error_class`, so an operator
reading a dead delivery and an operator reading a dead job are reading the same word.

## What a sustained rate means

Retries are currently absorbing a real problem. It is a leading indicator: at the ceiling — 13
attempts for `retryable`, 21 for `rate_limited` — these become entries on
[`oto_jobs_dead_total`](/runbooks/oto_jobs_dead_total/), and *that* is unrecoverable work.

The terminal classes (`permanent`, `config_invalid`, `auth_expired`) never retry, so for those this
counter and the dead counter move together, once each.

## What to check

1. Split by `class` first. `retryable` climbing is a dependency; `auth_expired` is a credential;
   `config_invalid` is a configuration — or, for a render error, an oto bug.
2. Split by `kind`. `deliver.dispatch` is Slack or a webhook; `ingest.process_batch` is the
   database; `source.reconcile` is Alertmanager.
3. The per-attempt error in the job logs, keyed by `job_id`.
4. For delivery kinds: the channel's health, and its banner in the UI.

## What to do

- `retryable` — fix the dependency. Watch the ratio `failed / started`: below the ceiling nothing
  is lost yet.
- `rate_limited` — oto already honours `Retry-After` as a snooze rather than an attempt, so seeing
  this class *fail* rather than snooze means the provider is refusing beyond its own advice. Reduce
  fan-out (fewer channels per policy) before raising limits.
- `config_invalid` / `auth_expired` — go to [`oto_jobs_dead_total`](/runbooks/oto_jobs_dead_total/); the
  response is the same and it is already terminal.
