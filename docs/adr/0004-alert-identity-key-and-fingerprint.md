# 0004 — oto owns alert identity; Alertmanager's fingerprint is recomputed, not trusted

**Status:** Accepted · 2026-08-07

## Context
Alertmanager's `fingerprint` is FNV-1a 64-bit over the sorted full label set, rendered as 16 hex
chars. It is deterministic, present since AM 0.19.0, identical across HA peers, and fully
reproducible locally by importing `prometheus/common/model`.

It also has no tenant in it, no cluster in it, and no way to say "`prometheus_replica` is not
part of identity" without editing someone else's Alertmanager config.

An earlier draft justified an oto-owned key by 64-bit birthday collisions. That justification is
overstated and is not the reason.

## Decision
Two keys, both stored, both indexed, with different jobs.

**`alert_key`** is the product identity and the dedup key:
```
"ak_" || base32hexLower(sha256(org_id || 0x00 || cluster_key || 0x00 || canon(labels, ignore))[0:16])
```
128 bits, scoped by `(org, cluster_key)`, with a per-source `ignore_labels` deny-list applied
before hashing. `UNIQUE (org_id, alert_key)` — dedup is enforced by the constraint, never by a
read-then-write check, which races under concurrency.

**`source_fingerprint`** is Alertmanager's fingerprint, **recomputed locally** over the full
label set. If the wire value differs from ours, we store ours and emit
`ingest.fingerprint_mismatch`. It is the join key for `/api/v2/alerts` reconciliation and for
debugging against upstream. It is never the product identity.

## Consequences
- Identical `KubePodCrashLooping{namespace="prod",pod="api-0"}` in `prod-eu` and `prod-us` are
  **different Alerts** — correct, because they are different problems with different blast radii.
- HA Alertmanager replicas registered against the same Cluster collapse into one Alert, one
  occurrence, one Slack thread.
- Cross-cluster correlation is never an implicit merge. It would be an explicit human act — and
  that capability is deferred post-v1.
- oto is immune to the `fingerprint` field being absent, wrong, or produced by a Grafana webhook.
- Changing `ignore_labels` creates new identities rather than re-keying. Documented; a re-key and
  merge job is deferred.

## Alternatives rejected
- **Use AM's fingerprint as the primary key:** dramatically simpler reconciliation, but no
  tenant/cluster scoping and no identity policy — permanently hostage to upstream label hygiene.
- **Trust the wire value without recomputing:** free, and wrong the first time a proxy, a custom
  `payload:` template, or Grafana changes the shape.
