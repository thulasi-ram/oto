---
title: oto_ingest_accepted_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | `mode` — `push` (an Alertmanager webhook) or `reconcile` (a pull pass) |
| Registered in | `internal/ingestion/service/metrics.go` |
| Alertable | **yes, on absence.** A high value is health; a flat line is the symptom |
| Rule | `OtoIngestStopped` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Batches durably persisted and enqueued. oto answers 202 only after the batch is committed, so a
2xx is a promise and this counter counts the promises. A batch collapsed by `ingest_dedup` is
**not** counted here — it lands on [`oto_ingest_duplicate_total`](/oto/runbooks/oto_ingest_duplicate_total/).

## What a value means

- Rising with `mode="push"` — the webhook path is alive.
- Rising with `mode="reconcile"` only — pushes have stopped and the mandatory reconciler
  (ADR 0006) is the only thing keeping state true. Alerts will appear, late.
- Flat for longer than your quietest period — either nothing is firing upstream, or oto is not
  being told. Those are different problems and the reconciler is what tells them apart: if
  `mode="reconcile"` is also flat, oto cannot reach Alertmanager at all.

## What to check

1. `GET /api/v1/sources/{id}/health` — `last_push_at`, `last_reconcile_at`, `status`,
   `consecutive_failures`, `last_error`. `status != healthy` also means the reaper is held, so
   nothing is being expired either (§B.4).
2. Upstream: Alertmanager's own `alertmanager_notifications_failed_total{integration="webhook"}`
   and its log. A 4xx there means Alertmanager has **deleted** the notification permanently.
3. Is anything else refusing the batch first — [`oto_ingest_shed_total`](/oto/runbooks/oto_ingest_shed_total/)
   (503s) or [`oto_ingest_rejected_total`](/oto/runbooks/oto_ingest_rejected_total/) with
   `reason="unknown_source"` (a soft-deleted source, or `push_enabled=false`).
4. Prove the path by hand: `just fire-alert <source-id> <ingest-token>`.

## What to do

- Receiver misconfigured: the URL is per source, `/api/v1/ingest/alertmanager/{source_id}`, with
  that source's own ingest token. See `deploy/alertmanager/alertmanager.yml`.
- Token rejected (401): rotate with `POST /api/v1/sources/{id}/rotate-token` and paste the new one
  into `webhook_config.http_config.authorization.credentials`.
- Source disabled: re-enable `push_enabled`, or accept reconcile-only ingestion knowingly.
- Genuinely quiet: raise the `for:` on the rule rather than deleting it. Silence that is never
  distinguished from breakage is how a broken pipeline survives a quarter.
