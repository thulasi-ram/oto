---
title: oto_ingest_rejected_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | `reason` — the closed `ingest_rejections_reason_ck` enum |
| Registered in | `internal/ingestion/service/metrics.go` |
| Alertable | **yes** |
| Rule | `OtoIngestRejecting` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Observations oto refused to normalise. **Every increment has a row behind it** in
`ingest_rejections`, carrying the offending element post-redaction. oto never silently drops
(§C.9.1) — that row is what makes answering 202 for a partially bad payload legitimate.

## The reasons, and what each one means

| `reason` | Meaning | Alert kept? |
|---|---|---|
| `too_many_labels` | more than `MaxLabelsPerAlert` labels (B3) | no |
| `label_name_too_large` | a label name over cap (B4) | no |
| `label_value_too_large` | a label value, or `alertname`, over cap (B5, B11) | no |
| `labelset_too_large` | the whole serialised set over cap (B6) | no |
| `too_many_annotations` | excess annotations (B7) | **yes**, excess dropped |
| `annotation_too_large` | one annotation over cap (B8) | **yes**, value truncated |
| `annotation_unstorable` | an annotation carrying `U+0000` or invalid UTF-8 (B19) | **yes**, value's bad code points replaced with `U+FFFD`; an annotation with an unstorable *name* is dropped |
| `invalid_label_name` | not a valid Prometheus label name (B9) | no |
| `invalid_label_value` | a label value Postgres cannot store — `U+0000` or invalid UTF-8 (B18) | no |
| `missing_alertname` | no `alertname` (B10) | no |
| `timestamp_out_of_window` | upstream clock disagrees past what oto will model (B12 drops, B13 clamps) | mixed |
| `too_many_alerts` | batch truncated at `MaxAlertsPerBatch` (B2) | partly |
| `body_too_large` | the only rejection answering **413** (B1) | no |
| `undecodable` | not the webhook envelope at all — the only rejection answering **400** (B16) | no |
| `unknown_source` | soft-deleted source, or `push_enabled = false` | no |

## What to check

1. `GET /api/v1/sources/{id}` rejection feed, or `ingest_rejections` directly, filtered to
   `reason` and the partition for the hour in question — `received_at` is the partition key, so
   "what happened at 02:14" touches one partition.
2. The `raw` column: the actual element that was refused, already redacted. This is the evidence;
   read it before theorising.

## What to do, by shape

- **`undecodable` or `body_too_large` climbing**: somebody put a custom `payload:` template on the
  webhook receiver, or is posting something other than Alertmanager v4. Remove the template — oto
  requires the stock envelope.
- **`unknown_source`**: the receiver is pointed at a source id that oto will not serve. Re-enable
  `push_enabled`, or repoint Alertmanager at a live source.
- **`too_many_alerts`**: raise `group_by` cardinality upstream or accept truncation; check
  `docs/setup/tuning.md` before changing the cap.
- **`timestamp_out_of_window`**: this is a clock problem, not a payload problem — see
  [`oto_clock_skew_seconds`](/oto/runbooks/oto_clock_skew_seconds/).
- **Label/annotation caps**: usually one noisy rule attaching a stack trace as a label. Fix the
  rule; the caps exist so one rule cannot make the store unqueryable.
- **`invalid_label_value` or `annotation_unstorable`**: an upstream is putting raw bytes into a
  label or annotation — almost always `label_replace` over log-, exporter- or command-output-derived
  text, where a `U+0000` or a byte-wise truncation of a multi-byte character survived. These are
  **not** decoding problems: the payload parsed fine, and one field carried bytes Postgres cannot
  store in a UTF8 database. Fix the rule that builds the value. Note the two are not
  interchangeable — `invalid_label_value` means **an alert is missing from the timeline** (a label
  value is identity, and oto will not rewrite an identity in order to store it), while
  `annotation_unstorable` means **the alert is there with one altered sentence**.
