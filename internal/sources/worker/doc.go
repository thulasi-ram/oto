// Package worker holds the `source.reconcile` job handler (SPEC §G.3, §G.8).
//
// It is a thin adapter and nothing else: `platform/jobs` owns scheduling,
// retries, payload versioning, metrics and the dead letter, and
// `sources/service.Reconciler` owns what a pass MEANS. A handler that contained
// logic would be logic no test could reach without a queue.
package worker
