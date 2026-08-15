---
title: oto_ingest_fingerprint_mismatch_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | none |
| Registered in | `internal/ingestion/service/metrics.go` |
| Alertable | **yes, on a sustained rate** |
| Rule | `OtoFingerprintMismatch` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Alerts whose `fingerprint` on the wire disagreed with the one oto recomputed from the label set
(C10). **Never fatal. Always counted.** oto stores its own value and carries on, because oto's
identity keys must be derivable from the labels alone.

## What a non-zero value means

The upstream is not the Alertmanager oto assumes, or is not computing the fingerprint the way
Alertmanager does. In practice:

- an Alertmanager version whose hashing differs from the one `docs/design/domain-research.md`
  pinned down;
- a relay, proxy or bespoke sender that fabricates a `fingerprint` field;
- labels being injected or stripped between Alertmanager and oto — including by oto's own
  `inject_labels` / `ignore_labels` on the source, if those are applied to the wrong side.

It is not, by itself, data loss: grouping and alert identity are computed from labels
(ADR 0004, ADR 0022), not from the wire fingerprint. It is a signal that your assumption about the
upstream is wrong, and every other assumption about that upstream is now suspect.

## What to check

1. `GET /api/v1/sources/{id}/health` → `am_version`. Compare with the versions in
   `docs/design/domain-research.md`.
2. Whether anything sits between Alertmanager and oto. `externalURL` and `generatorURL` on the
   received payload usually give it away.
3. The source's `inject_labels` and `ignore_labels`: mutating labels before identity is computed
   changes what oto hashes.

## What to do

- One source, steady rate, otherwise healthy: record it and move on — oto's own key is correct.
- Rate that starts at an upgrade: note the Alertmanager version; the mismatch is upstream and
  harmless as long as it is *consistent*.
- A bespoke sender: prefer making it emit the stock envelope over teaching oto a second identity
  scheme. There is exactly one identity pre-image and it is length-prefixed for a reason
  (ADR 0022).
