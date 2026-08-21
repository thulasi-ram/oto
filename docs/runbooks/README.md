# oto runbooks

oto is an alerting product. Its own metrics are expected to be exemplary, and a metric whose
meaning lives only in a Go comment is not exemplary — it is a number nobody on call can act on.
This directory is one page per `oto_*` metric the binary actually registers: what it counts, what a
non-zero or sustained value means, what to check, and what to do.

**Every metric listed here was read off the code, not invented.** The "Registered in" line on each
page is the file that constructs the collector; if a page and the code disagree, the code is right
and the page is a bug.

**Not every page here is a metric.**
[`alert-search-partial-match.md`](alert-search-partial-match.md) is a one-time, operator-run SQL
snippet (optional `pg_trgm` opt-in for substring alert-name search) — it lives here because it is
still ops-facing self-service documentation, just not one page per collector.

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
| [`oto_ingest_accepted_total`](oto_ingest_accepted_total.md) | on **absence** | Batches durably persisted and enqueued |
| [`oto_ingest_alerts_total`](oto_ingest_alerts_total.md) | no | Individual alerts accepted |
| [`oto_ingest_duplicate_total`](oto_ingest_duplicate_total.md) | no | Batches collapsed by `ingest_dedup`; non-zero is healthy |
| [`oto_ingest_rejected_total`](oto_ingest_rejected_total.md) | yes | Observations refused, by reason, with the evidence kept |
| [`oto_ingest_shed_total`](oto_ingest_shed_total.md) | yes | Deliberate 503 backpressure, by reason |
| [`oto_ingest_duration_seconds`](oto_ingest_duration_seconds.md) | yes | Accept latency; p99 budget 250 ms |
| [`oto_ingest_process_duration_seconds`](oto_ingest_process_duration_seconds.md) | yes | `ingest.process_batch` latency |
| [`oto_ingest_fingerprint_mismatch_total`](oto_ingest_fingerprint_mismatch_total.md) | yes | Wire fingerprint disagreed with oto's recomputation |
| [`oto_clock_skew_seconds`](oto_clock_skew_seconds.md) | yes | `received_at − startsAt`; measured, never corrected |

## Jobs — work accepted and then not done

`internal/platform/jobs/metrics.go`. `dead`, `panics` and `unknown_version` carry **ALERT ON THIS**
in their own help strings.

| Metric | Alertable | One line |
|---|---|---|
| [`oto_jobs_enqueued_total`](oto_jobs_enqueued_total.md) | no | Jobs inserted, by kind and queue |
| [`oto_jobs_started_total`](oto_jobs_started_total.md) | no | Executions begun |
| [`oto_jobs_succeeded_total`](oto_jobs_succeeded_total.md) | no | Executions that returned nil |
| [`oto_jobs_failed_total`](oto_jobs_failed_total.md) | yes | Executions that returned an error, by §G.6 class |
| [`oto_jobs_dead_total`](oto_jobs_dead_total.md) | **yes, page** | Jobs that will never run again |
| [`oto_jobs_panics_total`](oto_jobs_panics_total.md) | **yes, page** | Panics recovered in a handler. Always a bug |
| [`oto_jobs_unknown_version_total`](oto_jobs_unknown_version_total.md) | **yes, page** | Payload newer than this binary understands |
| [`oto_jobs_snoozed_total`](oto_jobs_snoozed_total.md) | yes, sustained | Deferrals that consume no attempt |
| [`oto_job_duration_seconds`](oto_job_duration_seconds.md) | yes | Wall time of one execution |
| [`oto_job_queue_depth`](oto_job_queue_depth.md) | yes | Backlog by queue and River state |

## Thread ordering — one channel, one order

`internal/platform/jobs/ordering/gate.go`.

| Metric | Alertable | One line |
|---|---|---|
| [`oto_thread_order_decisions_total`](oto_thread_order_decisions_total.md) | no | Gate verdicts, by action and reason |
| [`oto_thread_gap_recovered_total`](oto_thread_gap_recovered_total.md) | **yes** | Slots advanced past unsent. Sustained means a channel is broken |
| [`oto_thread_head_wait_seconds`](oto_thread_head_wait_seconds.md) | yes | How long a delivery waited behind the head |

## Delivery and inbound interactions

| Metric | Alertable | One line |
|---|---|---|
| [`oto_delivery_claim_lost_total`](oto_delivery_claim_lost_total.md) | **yes, page** | A send landed and could not be recorded |
| [`oto_slack_unknown_action_total`](oto_slack_unknown_action_total.md) | **yes** | A button a human pressed that oto answered 200 and could not route |

`internal/notification/service/metrics.go` and `internal/channels/service/metrics.go`.

## Streaming — the SSE spine behind the UI

`internal/streaming/service/metrics.go`. These are about the UI being live. None of them can lose
an alert: `ui_events` is durable and the timeline is always re-fetchable.

| Metric | Alertable | One line |
|---|---|---|
| [`oto_stream_connections`](oto_stream_connections.md) | no | Live SSE connections on this pod |
| [`oto_stream_events_published_total`](oto_stream_events_published_total.md) | no | Events accepted into a subscriber's buffer |
| [`oto_stream_events_fetched_total`](oto_stream_events_fetched_total.md) | no | `ui_events` rows read by the bridge |
| [`oto_stream_events_coalesced_total`](oto_stream_events_coalesced_total.md) | no | Frames superseded inside the 250 ms window |
| [`oto_stream_notify_received_total`](oto_stream_notify_received_total.md) | no | LISTEN/NOTIFY doorbells received |
| [`oto_stream_events_dropped_total`](oto_stream_events_dropped_total.md) | yes | Buffer full; oto never blocks a writer for a reader |
| [`oto_stream_resync_total`](oto_stream_resync_total.md) | yes | "Refetch everything", by reason |
| [`oto_stream_notify_malformed_total`](oto_stream_notify_malformed_total.md) | yes | NOTIFY payloads that did not parse |
| [`oto_stream_listener_reconnects_total`](oto_stream_listener_reconnects_total.md) | yes | LISTEN connection re-established |
| [`oto_stream_poll_recovered_total`](oto_stream_poll_recovered_total.md) | yes, on a spike | Events the reconciling poll caught |
| [`oto_stream_fetch_errors_total`](oto_stream_fetch_errors_total.md) | yes | Failed catch-up reads of `ui_events` |

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
| `oto_delivery_dead_total` | `notification_deliveries.status = 'dead'` with `error_class`; per-job deaths are on [`oto_jobs_dead_total`](oto_jobs_dead_total.md), which is alertable and paged |
| `oto_check_violation_total{constraint}` | A `23514` maps to `errs.KindInternal` with the **constraint name as the error `Code`** (SPEC §L.9, `internal/*/repository/errors.go`), so the 500 and its log line name the constraint; on a job path it is counted by `oto_jobs_failed_total{class="internal"}` |

⭐ `oto_render_invalid_total` was on this table and has been **built** — see
[`oto_render_invalid_total`](oto_render_invalid_total.md). Its entry here was not just thin, it was
wrong: it sent an operator to `oto_jobs_dead_total`, which never fires for a render failure because
the delivery is marked dead inside the job and the job then succeeds. A struck name whose
replacement fact is false is worse than the promise it replaced.

`oto_thread_recovered_total` was an eighth such name, but it is a **rename, not a gap**: the metric
shipped as [`oto_thread_gap_recovered_total`](oto_thread_gap_recovered_total.md). The SPEC and the
OpenAPI description now use the registered name. The code was deliberately left alone — the shipped
name is already scraped, already has this page, and already backs a live rule in
`deploy/prometheus/oto-rules.yaml`.

## Adding a metric

1. Register it on the injected `prometheus.Registerer` — never a global registry. A collector built
   with a `nil` registry is invisible to `/metrics`, which has already happened twice here.
2. Write its page in this directory in the same commit.
3. If it is alertable, add the rule to `deploy/prometheus/oto-rules.yaml` with a `runbook_url`
   annotation pointing at that page.
