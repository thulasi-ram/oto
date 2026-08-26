---
title: oto_clock_skew_seconds
---
|  |  |
|---|---|
| Type | histogram (buckets from −60 s to 1 h) |
| Labels | none |
| Registered in | `internal/ingestion/service/metrics.go` |
| Alertable | **yes** |
| Rule | `OtoClockSkew` in `deploy/prometheus/oto-rules.yaml` |

## What it observes

`received_at − startsAt` in seconds: oto's clock at accept time minus the timestamp the upstream
put on the alert.

**Skew is measured and surfaced, never corrected away and never a reason to reject (C12).** That is
why every event in oto carries two timestamps. The same fact is stored per source as
`source_health.clock_skew_ms` (an EWMA, folded in from the `Date` header on every reconcile pass)
and shown by `GET /api/v1/sources/{id}/health`.

## What a large value means

- **Large positive** (upstream behind oto, or delivery delayed): alerts appear in the timeline with
  a start time well before oto heard about them. Ordering *within* a source stays coherent; ordering
  *across* sources does not. Anything derived from "how long has this been firing" is off by the
  skew.
- **Large negative** (upstream ahead of oto): timestamps in the future. B12/B13 turn the extreme
  cases into `timestamp_out_of_window` on
  [`oto_ingest_rejected_total`](/oto/runbooks/oto_ingest_rejected_total/) — B12 drops the alert, B13 clamps and
  keeps it. So sustained skew here is the leading indicator of that rejection.
- **Bimodal**: two sources with different clocks. The histogram is global; the per-source EWMA on
  source health is what separates them.

## What to check

1. `GET /api/v1/sources/{id}/health` → `clock_skew_ms`, per source. Find which one.
2. NTP on the host running that Alertmanager, and on the oto host. One of the two is wrong.
3. `timestamp_out_of_window` on [`oto_ingest_rejected_total`](/oto/runbooks/oto_ingest_rejected_total/) — if it
   is climbing alongside, you are already losing observations.
4. Delivery delay masquerading as skew: an Alertmanager that retried for four minutes produces a
   four-minute "skew" with perfectly correct clocks. Cross-check against the retry counters
   upstream.

## What to do

- Fix NTP. There is no oto-side setting that makes a wrong clock right, deliberately.
- While it is wrong, read the timeline using oto's `received_at` column rather than the upstream
  timestamp; both are present on every event for exactly this case.
- Do not "fix" it by widening the B12/B13 window past what your rules' `for:` durations justify —
  see `docs/setup/tuning.md`.
