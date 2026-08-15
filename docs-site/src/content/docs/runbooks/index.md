---
title: oto runbooks
---
oto is an alerting product. Its own metrics are expected to be exemplary, and a metric whose
meaning lives only in a Go comment is not exemplary — it is a number nobody on call can act on.
This directory is one page per `oto_*` metric the binary actually registers: what it counts, what a
non-zero or sustained value means, what to check, and what to do.

**Every metric listed here was read off the code, not invented.** The "Registered in" line on each
page is the file that constructs the collector; if a page and the code disagree, the code is right
and the page is a bug.

## How to see them

```bash
just metrics            # curl :8080/metrics | grep '^oto_'
just metrics oto_jobs_  # any prefix
```

`/metrics` is unauthenticated and served from the same port as the API
(`OTO_TELEMETRY_METRICS_PATH`, default `/metrics`; disable with
`OTO_TELEMETRY_METRICS_ENABLED=false`). The compose Prometheus scrapes it and evaluates
[`deploy/prometheus/oto-rules.yaml`](../../deploy/prometheus/oto-rules.yaml), whose
`runbook_url` annotations point back at these pages.

## Ingestion — the promise a 202 makes

`internal/ingestion/service/metrics.go`. `oto_ingest_accepted_total`, `oto_ingest_rejected_total`
and `oto_ingest_duration_seconds` are named in the published API contract, so their names are as
much a contract as any JSON field.

| Metric | Alertable | One line |
|---|---|---|
| [`oto_ingest_accepted_total`](/runbooks/oto_ingest_accepted_total/) | on **absence** | Batches durably persisted and enqueued |
| [`oto_ingest_alerts_total`](/runbooks/oto_ingest_alerts_total/) | no | Individual alerts accepted |
| [`oto_ingest_duplicate_total`](/runbooks/oto_ingest_duplicate_total/) | no | Batches collapsed by `ingest_dedup`; non-zero is healthy |
| [`oto_ingest_rejected_total`](/runbooks/oto_ingest_rejected_total/) | yes | Observations refused, by reason, with the evidence kept |
| [`oto_ingest_shed_total`](/runbooks/oto_ingest_shed_total/) | yes | Deliberate 503 backpressure, by reason |
| [`oto_ingest_duration_seconds`](/runbooks/oto_ingest_duration_seconds/) | yes | Accept latency; p99 budget 250 ms |
| [`oto_ingest_process_duration_seconds`](/runbooks/oto_ingest_process_duration_seconds/) | yes | `ingest.process_batch` latency |
| [`oto_ingest_fingerprint_mismatch_total`](/runbooks/oto_ingest_fingerprint_mismatch_total/) | yes | Wire fingerprint disagreed with oto's recomputation |
| [`oto_clock_skew_seconds`](/runbooks/oto_clock_skew_seconds/) | yes | `received_at − startsAt`; measured, never corrected |

## Jobs — work accepted and then not done

`internal/platform/jobs/metrics.go`. `dead`, `panics` and `unknown_version` carry **ALERT ON THIS**
in their own help strings.

| Metric | Alertable | One line |
|---|---|---|
| [`oto_jobs_enqueued_total`](/runbooks/oto_jobs_enqueued_total/) | no | Jobs inserted, by kind and queue |
| [`oto_jobs_started_total`](/runbooks/oto_jobs_started_total/) | no | Executions begun |
| [`oto_jobs_succeeded_total`](/runbooks/oto_jobs_succeeded_total/) | no | Executions that returned nil |
| [`oto_jobs_failed_total`](/runbooks/oto_jobs_failed_total/) | yes | Executions that returned an error, by §G.6 class |
| [`oto_jobs_dead_total`](/runbooks/oto_jobs_dead_total/) | **yes, page** | Jobs that will never run again |
| [`oto_jobs_panics_total`](/runbooks/oto_jobs_panics_total/) | **yes, page** | Panics recovered in a handler. Always a bug |
| [`oto_jobs_unknown_version_total`](/runbooks/oto_jobs_unknown_version_total/) | **yes, page** | Payload newer than this binary understands |
| [`oto_jobs_snoozed_total`](/runbooks/oto_jobs_snoozed_total/) | yes, sustained | Deferrals that consume no attempt |
| [`oto_job_duration_seconds`](/runbooks/oto_job_duration_seconds/) | yes | Wall time of one execution |
| [`oto_job_queue_depth`](/runbooks/oto_job_queue_depth/) | yes | Backlog by queue and River state |

## Thread ordering — one channel, one order

`internal/platform/jobs/ordering/gate.go`.

| Metric | Alertable | One line |
|---|---|---|
| [`oto_thread_order_decisions_total`](/runbooks/oto_thread_order_decisions_total/) | no | Gate verdicts, by action and reason |
| [`oto_thread_gap_recovered_total`](/runbooks/oto_thread_gap_recovered_total/) | **yes** | Slots advanced past unsent. Sustained means a channel is broken |
| [`oto_thread_head_wait_seconds`](/runbooks/oto_thread_head_wait_seconds/) | yes | How long a delivery waited behind the head |

## Delivery and inbound interactions

| Metric | Alertable | One line |
|---|---|---|
| [`oto_delivery_claim_lost_total`](/runbooks/oto_delivery_claim_lost_total/) | **yes, page** | A send landed and could not be recorded |
| [`oto_slack_unknown_action_total`](/runbooks/oto_slack_unknown_action_total/) | **yes** | A button a human pressed that oto answered 200 and could not route |

`internal/notification/service/metrics.go` and `internal/channels/service/metrics.go`.

## Streaming — the SSE spine behind the UI

`internal/streaming/service/metrics.go`. These are about the UI being live. None of them can lose
an alert: `ui_events` is durable and the timeline is always re-fetchable.

| Metric | Alertable | One line |
|---|---|---|
| [`oto_stream_connections`](/runbooks/oto_stream_connections/) | no | Live SSE connections on this pod |
| [`oto_stream_events_published_total`](/runbooks/oto_stream_events_published_total/) | no | Events accepted into a subscriber's buffer |
| [`oto_stream_events_fetched_total`](/runbooks/oto_stream_events_fetched_total/) | no | `ui_events` rows read by the bridge |
| [`oto_stream_events_coalesced_total`](/runbooks/oto_stream_events_coalesced_total/) | no | Frames superseded inside the 250 ms window |
| [`oto_stream_notify_received_total`](/runbooks/oto_stream_notify_received_total/) | no | LISTEN/NOTIFY doorbells received |
| [`oto_stream_events_dropped_total`](/runbooks/oto_stream_events_dropped_total/) | yes | Buffer full; oto never blocks a writer for a reader |
| [`oto_stream_resync_total`](/runbooks/oto_stream_resync_total/) | yes | "Refetch everything", by reason |
| [`oto_stream_notify_malformed_total`](/runbooks/oto_stream_notify_malformed_total/) | yes | NOTIFY payloads that did not parse |
| [`oto_stream_listener_reconnects_total`](/runbooks/oto_stream_listener_reconnects_total/) | yes | LISTEN connection re-established |
| [`oto_stream_poll_recovered_total`](/runbooks/oto_stream_poll_recovered_total/) | yes, on a spike | Events the reconciling poll caught |
| [`oto_stream_fetch_errors_total`](/runbooks/oto_stream_fetch_errors_total/) | yes | Failed catch-up reads of `ui_events` |

## Struck from the contract — do not alert on these

Earlier drafts of SPEC AC-34 and `api/openapi/openapi.yaml` promised several metrics that no
collector in the tree constructs. **They have been removed from both contracts**, because a name on
`/metrics` that never produces a series is worse than no promise at all: a rule written against it
never fires, and a rule that never fires is indistinguishable from a healthy system.

They are kept here, not deleted, so that an operator who finds one of these names in an old
dashboard, an old alert rule or an old ADR can see at a glance that it is gone and what to read
instead. This table is the maintained copy; SPEC AC-34 mirrors it.

| Struck name | What answers the question instead |
|---|---|
| `oto_reconcile_divergence` | `source_health.divergence_count`, served by `GET /api/v1/sources/{id}/health` and summed by `GET /api/v1/stats/*`; plus the `sources: reconcile divergence` INFO log (`internal/sources/service/reconcile.go`) |
| `oto_source_degraded_holds_total` | `source_health.status`; while it is not `healthy` the reaper is blocked (§B.4). The hold is a state you can read, not a rate you must integrate |
| `oto_notification_suppressed_total{reason}` | `notifications.suppressed_reason` — the closed set in `internal/notification/domain/suppression.go`. §B.6 requires every suppression to be a durable row with a place in the UI, so the row is the primary artefact. (There is **no** `notification_suppressions` table; an earlier version of this page named one.) |
| `oto_delivery_attempts_total{class}` | `notification_deliveries.attempts` / `.error_class`; per-job failures are on `oto_jobs_failed_total{class}` |
| `oto_delivery_dead_total` | `notification_deliveries.status = 'dead'` with `error_class`; per-job deaths are on [`oto_jobs_dead_total`](/runbooks/oto_jobs_dead_total/), which is alertable and paged |
| `oto_render_invalid_total{check}` | The delivery record: `status='dead'`, `error_class='config_invalid'`, the failing check named in `notification_deliveries.error` and the payload kept in `.rendered` (retrievable via `GET /api/v1/deliveries/{id}`). `internal/channels/render/slack/validate.go` owns the check vocabulary; the rate is on `oto_jobs_dead_total` |
| `oto_check_violation_total{constraint}` | A `23514` maps to `errs.KindInternal` with the **constraint name as the error `Code`** (SPEC §L.9, `internal/*/repository/errors.go`), so the 500 and its log line name the constraint; on a job path it is counted by `oto_jobs_failed_total{class="internal"}` |

`oto_thread_recovered_total` was an eighth such name, but it is a **rename, not a gap**: the metric
shipped as [`oto_thread_gap_recovered_total`](/runbooks/oto_thread_gap_recovered_total/). The SPEC and the
OpenAPI description now use the registered name. The code was deliberately left alone — the shipped
name is already scraped, already has this page, and already backs a live rule in
`deploy/prometheus/oto-rules.yaml`.

## Adding a metric

1. Register it on the injected `prometheus.Registerer` — never a global registry. A collector built
   with a `nil` registry is invisible to `/metrics`, which has already happened twice here.
2. Write its page in this directory in the same commit.
3. If it is alertable, add the rule to `deploy/prometheus/oto-rules.yaml` with a `runbook_url`
   annotation pointing at that page.
