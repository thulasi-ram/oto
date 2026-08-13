// Package service holds the sources business logic and the port interfaces it declares for itself. Concretes are injected by internal/app.
//
// It is BOTH halves of the module's aggregate: the reads and probes in
// service.go, the §G.8 reconciler in reconcile.go, and the write path in
// write.go — register, edit and retire a source, rotate its ingest credential —
// including the transaction that makes a source and its token one fact and the
// `Idempotency-Key` claim taken inside it. Those used to live in
// internal/sources/api, which meant nothing but an HTTP request could perform
// them (ticket 0869f21).
package service
