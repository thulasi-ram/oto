---
title: oto_jobs_panics_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | `kind`, `queue` |
| Registered in | `internal/platform/jobs/metrics.go`; recovered in `internal/platform/jobs/worker.go` |
| Alertable | **YES — page on it.** The help string says `Always a bug. ALERT ON THIS` |
| Rule | `OtoJobPanic` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Panics recovered inside a job handler. River recovers panics itself, but it cannot say which of
oto's handlers blew up in oto's own metric namespace, so oto recovers first and records `kind`.

A recovered panic is converted to a **retryable** error on purpose: a transient nil dereference
during a rolling deploy should not discard an alert. A genuine one hits the attempt ceiling within
minutes and shows up on [`oto_jobs_dead_total`](/oto/runbooks/oto_jobs_dead_total/).

## What a non-zero value means

A bug in oto. There is no configuration, no payload and no upstream that is *supposed* to make a
handler panic — every hostile input is meant to be a classified error long before this point.

## What to check

1. The log record, which carries the stack:
   ```
   msg="jobs: panic in job handler" panic= stack= job_kind= queue= attempt=
   ```
2. Whether it repeats for the same `kind` on every attempt. Repeating means the payload is the
   trigger, and the payload is in the dead-letter record once the attempts are spent.
3. Whether it started at a deploy. `GET /api/v1/version` gives the commit.

## What to do

- File it. Include `kind`, the stack, and the payload from the dead-letter line if it got that far.
  Prefer a failing test in `test/integration/` over a defensive nil check: a panic that is patched
  without a reproduction comes back.
- If it is deploy-shaped and the previous build was clean, roll back — but check
  [`oto_jobs_unknown_version_total`](/oto/runbooks/oto_jobs_unknown_version_total/) first, because rolling back
  a payload-version bump strands jobs.
- Do not silence it by widening a `recover`. The counter exists so this cannot be absorbed quietly.
