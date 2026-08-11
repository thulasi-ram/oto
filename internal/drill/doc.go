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
package drill
