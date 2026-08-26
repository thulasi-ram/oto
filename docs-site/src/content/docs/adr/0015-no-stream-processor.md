---
title: 0015 — No stream processor; River in Postgres is the pipeline
---
**Status:** Accepted · 2026-08-08
**Relates to:** [0001](/oto/adr/0001-postgres-sole-datastore-river-job-queue/) (River and the transactional
outbox), [0014](/oto/adr/0014-postgres-only-no-analytical-store/) (one datastore)

## Context

oto's shape — ingest a webhook, transform it, fan out to sinks with retries and backpressure — is
almost exactly the shape Benthos / Redpanda Connect was built for. The question deserves a real answer
rather than a reflex, because on the surface it is a good match: durable buffering, batching, retry
policy, and Bloblang for mapping, all configuration rather than code.

## Decision

We do not run a stream processor. The pipeline is River jobs over Postgres, with the ordering and
suppression logic in Go.

The decisive argument is not "extra dependency" — it is that **adopting Benthos would remove a
correctness guarantee we currently hold.** River enqueues a job in the *same transaction* as the state
change that justifies it, which is what gives us the outbox property from
[0001](/oto/adr/0001-postgres-sole-datastore-river-job-queue/): either the occurrence opened and the
notification job exists, or neither happened. Benthos's at-least-once delivery is not transactional
with our Postgres writes. We would be trading an invariant for a pipe.

The supporting arguments:

- Every hard part of this system is *stateful in Postgres* — the occurrence state machine, dedup by
  identity key, per-thread sequence gating on advisory locks, and the suppressor precedence chain.
  None of it is expressible in Benthos. It would end up a thin pipe with all the logic still in Go.
- The one-`helm install`-plus-Postgres deployment story is a product asset, not just an operational
  convenience.
- A second configuration language (Bloblang) becomes a second place where routing behaviour is
  defined, and therefore a second place it can be wrong, in a system where being wrong means a missed
  page.

**What we keep from the idea.** Benthos is Go, and Bloblang is importable as a library. When we want
*user-authored input mapping* — "map this vendor's webhook into oto's alert model" without a
recompile — embedding Bloblang (or CEL, per [0017](/oto/adr/0017-matchers-over-cel/)) gives us that with no
second runtime. That is the sanctioned path if the need arrives.

## Consequences

- Throughput is bounded by Postgres and by River's worker pool rather than by a purpose-built stream
  engine. This is acceptable at the volumes established in [0014](/oto/adr/0014-postgres-only-no-analytical-store/).
- Retry, backoff, dead-lettering and backpressure are ours to own and to test. They are already
  implemented in `internal/platform/jobs`, including the per-thread ordering primitive that no
  off-the-shelf processor provides.
- Adding a new *input* source means writing Go, not editing a config file. Accepted for v1; the
  Bloblang-as-library path exists if the source count grows.

## Alternatives rejected

**Benthos / Redpanda Connect as a service.** Rejected above. The transactional-enqueue loss is the
disqualifier; the second runtime and second config language are the aggravating factors.

**Kafka partitioned by thread key.** Would make per-thread ordering correct by construction rather than
by advisory lock — a genuine improvement to the subtlest code in the system. Rejected because it adds a
broker to a product whose only dependency is Postgres, and imposes a partition ceiling on thread
concurrency. Reconsider only if the ordering primitive proves unreliable in practice.

**Temporal or a durable-execution engine.** A better conceptual fit for the multi-step notification
lifecycle than a stream processor is. Rejected for the same dependency reason, and because River's
transactional insert already covers the specific guarantee we care about.
