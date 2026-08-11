// Package service orchestrates a delivery drill: mint the receipt, push the
// synthetic payload at oto's own front door, and report what the real pipeline
// did with it.
//
// ⛔ IT CAUSES NOTHING DIRECTLY. It writes one table of its own and calls exactly
// one port to make anything happen — `IngestAcceptor`, satisfied by the same
// `ingestion/service.Service` the webhook handler calls. Every stage after that
// runs because the pipeline runs, not because this package asked it to.
package service
