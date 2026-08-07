// Package jobs owns the river client, the queue definitions, the typed job
// registry, the worker runtime and job payload versioning. An unknown payload
// version parks the job.
//
// This package is the ONLY place in oto that may name River. Everything else
// depends on the db.Enqueuer port (SPEC §F.5, §G.3), which is why swapping the
// queue would be a one-package change and why a service is testable without a
// Postgres-backed queue.
//
// Three things live here and nowhere else:
//
//  1. The typed job registry (args.go): one JobArgs struct per job type in
//     SPEC §G.3, each carrying its own queue, priority, retry ceiling, payload
//     version and a documented idempotency-key derivation.
//  2. The transactional enqueue path (client.go): Enqueue joins the transaction
//     travelling in ctx, so a job commits with the state change that justified it.
//     This is oto's transactional outbox (ADR 0001).
//  3. The worker runtime (worker.go, runtime.go): panic recovery, structured
//     logging with job id and attempt, Prometheus metrics, payload-version
//     gating, error classification and the dead-letter path.
//
// Business logic lives in the owning domain's `worker` package and is injected as
// a Handler. Everything registered here by default is a stub that returns
// errs.Internal("not implemented"); the seam is Registry.Handle.
package jobs
