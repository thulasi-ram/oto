// Package drill runs ONE SYNTHETIC ALERT THROUGH THE REAL PIPELINE and reports,
// stage by stage, what happened to it.
//
// ⭐⭐ WHY IT IS NOT THE CHANNEL TEST. `POST /channels/{id}/test` renders one card
// and hands it to the provider. It proves the token works and the Block Kit is
// legal, and it proves nothing else — it does not touch ingestion, alert
// identity, grouping, the notification policy match, the rule snapshot,
// `channel_threads`, the ordering gate or the delivery record. Every failure oto
// can have lives in the stages it skips: a policy that matches nothing, a thread
// that will not open, a scope Slack silently ignores. A drill runs them all,
// through the same endpoints, workers and SQL a real Alertmanager batch takes.
// Nothing here is a shortcut, and any shortcut added later stops it being
// evidence.
//
// ⭐⭐ THE HARD PART IS DISPOSABILITY, NOT DELIVERY. oto is sold on the history it
// keeps; a drill that leaked into `alert_quality_daily`, the dashboard or the
// alert list would make every number slightly false, and a slightly false number
// is believed. So the alert a drill manufactures carries a PROVENANCE MARK —
// `ingest_batches.mode = 'synthetic'`, propagated to `alerts.synthetic` and
// `alert_groups.synthetic` — that no payload can forge, every aggregate excludes,
// and `retention.prune` eventually deletes. See 00039_delivery_drills.sql.
//
// LAYERING. This package is a PERIPHERAL read-across module in the mould of
// `stats`: its repository reads several modules' tables because a staged result
// is a report over their artefacts, and everything it WRITES goes through a port
// some other module's service satisfies (§F.5 rule 4). It writes exactly one
// table of its own, `delivery_drills`.
//
// ⛔ THOSE READS ARE DECLARED, and the declaration is the gate. Because this package
// imports no other module, depguard and `test/arch/arch_test.go` are blind to it by
// construction — they read the import graph and there is nothing in it to read.
// `test/arch/sqltables_test.go` reads the SQL instead: every table named under
// `internal/drill/**` is listed there with its owner and how far this module may go
// against it, so a new table name fails CI the way a new import would. Adding one is
// an architectural change, and the reason goes in the claim.
package drill
