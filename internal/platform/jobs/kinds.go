package jobs

// Queue names (SPEC §G.3). A queue is a concurrency and isolation boundary: a
// wedged Slack workspace must not be able to stall ingestion, which is why
// `deliver_slack` and `ingest` are different queues rather than different
// priorities on one.
const (
	// QueueIngest carries `ingest.process_batch` and nothing else. It is the only
	// queue whose workers use the ingest pool (SPEC §G.10).
	QueueIngest = "ingest"
	// QueueEnrich carries the budgeted enrichment pipeline.
	QueueEnrich = "enrich"
	// QueueNotify carries policy evaluation, which mints Notifications.
	QueueNotify = "notify"
	// QueueDeliverSlack carries Slack sends. Narrow on purpose: Slack's per-channel
	// write limit is ~1 msg/s, so more workers here buy nothing but contention.
	QueueDeliverSlack = "deliver_slack"
	// QueueDeliverWebhook carries generic-webhook sends, which have no such limit.
	QueueDeliverWebhook = "deliver_webhook"
	// QueueReconcile carries the mandatory Alertmanager reconciler and silence sync.
	QueueReconcile = "reconcile"
	// QueueLifecycle carries the periodic state-machine sweeps: the case reaper and
	// the two digest kinds (the tick and its detector) — and `incidents.correlate`,
	// the one per-Case kind here, which sits on this queue rather than on `notify`
	// precisely so that a Correlator can never occupy a worker a notification is
	// waiting for (ADR 0052, git-bug 61eeddf). ⛔ IT USED TO SAY "group
	// close, unacked reminders" as well, and both of those kinds are gone —
	// `notify.unacked_reminder` with the reminder (git-bug bd0fb1d) and `group.close`
	// with grouping (git-bug 7570090, kind deleted below). A queue described by work
	// it no longer carries is how somebody sizes its worker count for a sweep that
	// does not run.
	QueueLifecycle = "lifecycle"
	// QueueMaintenance carries partition management, retention and rollups. One
	// worker: these are DDL-adjacent and must not race themselves.
	QueueMaintenance = "maintenance"
	// QueueInvestigate carries `investigations.run` and nothing else (ADR 0053 §3,
	// git-bug 180a525). ⛔ IT IS NOT `enrich`, though a Finding is published as an
	// Enrichment: one run may hold its worker for up to its wall-time budget —
	// minutes, against an enrichment's two seconds — and the inline enrichment pass
	// is what releases a Case's FIRST notification early. Sharing `enrich` would let
	// a few slow Investigations sit in the very slots that pass waits for. On a queue
	// of its own a run competes only with other runs, and its width is the
	// deployment's ceiling on concurrent model calls.
	QueueInvestigate = "investigate"
)

// AllQueues is every queue oto declares, in the order they appear in SPEC §G.3.
func AllQueues() []string {
	return []string{
		QueueIngest, QueueEnrich, QueueNotify,
		QueueDeliverSlack, QueueDeliverWebhook,
		QueueReconcile, QueueLifecycle, QueueMaintenance,
		QueueInvestigate,
	}
}

// Job kinds (SPEC §G.3). These strings are a DURABLE WIRE CONTRACT: they are
// written into `river_job.kind` and are matched by workers on the other side of a
// deploy. Renaming one strands every queued job of that kind.
const (
	KindIngestProcessBatch = "ingest.process_batch"
	KindEnrichRun          = "enrich.run"
	KindNotifyEvaluate     = "notify.evaluate"
	// KindNotifyIncident evaluates notification policy for one Incident fact (ADR
	// 0052 §5). It is enqueued by `incidents`, which never imports `notification`
	// — the queue is the seam, as it is for `notify.evaluate`.
	KindNotifyIncident  = "notify.incident"
	KindDeliverDispatch = "deliver.dispatch"
	// KindIncidentsCorrelate runs one freshly opened Case through the org's
	// Correlators (ADR 0052 §2, §4). It is enqueued by `alerts`' Case-opening port,
	// which never imports `incidents` — the queue is the seam.
	KindIncidentsCorrelate = "incidents.correlate"
	// KindSlackInteraction is one verified Slack block action, taken off the HTTP
	// request so the endpoint can answer inside Slack's three-second window.
	KindSlackInteraction = "slack.interaction"
	KindSourceReconcile  = "source.reconcile"
	KindSilencesSync     = "silences.sync"
	KindCaseReap         = "case.reap"
	// ⛔ THERE IS NO `group.close` KIND ANY MORE, and this tombstone is the second
	// half of a deletion that stopped half-way. git-bug 7570090 deleted grouping —
	// `alert_groups`, generations, the `LifecyclePolicy{CloseDelay}` that was this
	// sweep's ONLY consumer — and unregistered the kind in registry.go, but left the
	// constant and `GroupCloseArgs` standing. What that leaves behind is not harmless
	// vocabulary: a declared kind with a declared args struct is an INVITATION, and
	// the next person adding a lifecycle sweep finds a fully-formed per-tenant
	// periodic sitting one `Handlers` field away from working. It swept open
	// generations idle past `group_close_delay_s` and closed them, which is what made
	// the next fire post a brand-new Slack root. A conversation is a Case now: it ends
	// when the Case ends, so there is no idle generation to close and no sweep to run.
	//
	// ⚠️ ITS DOC CLAIMED §G.7.3 GAP RECOVERY AND THAT CLAIM WAS ALREADY STALE — this
	// is the part worth carrying forward, because it is the one thing a reader might
	// think went missing here. Thread gap recovery — advancing
	// `channel_threads.last_sent_seq` past a finished-but-unsent slot so one poison
	// message cannot wedge a thread forever — lives in
	// `platform/jobs/ordering.Gate.Recover` and runs on the DELIVERY path, under the
	// thread advisory lock, at the moment the wedge is observed. It was never a
	// periodic sweep's job and nothing about it is lost.
	//
	// ⭐ NO MIGRATION, AND THE REASONING IS THE STANDING ONE (migration 00059's
	// header, spent again by `flap.score` and `notify.unacked_reminder` below). There
	// is no SCHEDULE for this kind, so nothing has been able to create a row of it
	// since 7570090; and a row that does exist is fetched, answered with River's
	// `UnknownJobKindError`, retried to its stored `MaxAttempts` (3, from
	// `periodicOpts`) and discarded. An unregistered kind never RUNS — it does not
	// wedge a queue and it does not stop a client — so there is nothing for DDL to
	// clean up, and `just reset` is the answer on the only database that exists.
	// ⛔ THERE IS NO `flap.score` KIND ANY MORE. It recomputed
	// `alerts.flap_score` / `alerts.is_flapping` every five minutes. The case
	// retention window W (migration 00057) damps a flap at CASE FORMATION, which
	// left the score counting lifecycle events a damped flap no longer appends —
	// false exactly when the alert was flapping — so the detector is retired and
	// the two columns keep their last value, readable and unwritten (ADR 0041,
	// Amendment 1). A queued tick of this kind is unhandled after this change;
	// there is no deployment to strand, and `just reset` is the answer on the
	// only database that exists (migration 00059 spends the same argument).
	// ⛔ THERE IS NO `notify.unacked_reminder` KIND ANY MORE, and the tombstone is
	// here for the same reason `flap.score`'s is above. It swept every minute for
	// open groups whose oldest member had been firing and unacked past
	// `unacked_reminder_after_s`, and sent one reminder per generation. The owner
	// withdrew the feature (git-bug bd0fb1d): oto sends nothing unprompted. A queued
	// tick of this kind is unhandled after this change; there is no deployment to
	// strand, and `just reset` is the answer on the only database that exists.
	// KindNotifyDigest is the DIGEST TICK: at each window boundary, count what
	// matched each digest policy and say so once. It is the only notification job
	// whose subject is a WINDOW rather than an object (migration 00058).
	KindNotifyDigest = "notify.digest"
	// KindNotifyDigestReconcile is the DIGEST DETECTOR, and it is a separate kind
	// from the tick above rather than extra work on it because the two have opposite
	// shapes: the tick is once a minute, narrow, and DELIVERS; this is hourly, folds a
	// day-wide candidate span per policy, and is forbidden from delivering anything
	// (`notification/service.ReconcileOrg`). One kind would have meant one queue slot,
	// one timeout and one retry budget covering both, so a detector that ran long
	// would eat the tick's minute and digests would go late to make a number
	// auditable — which is the wrong trade in the only direction that matters.
	KindNotifyDigestReconcile = "notify.digest.reconcile"
	KindPartitionsManage      = "partitions.manage"
	KindRetentionPrune        = "retention.prune"
	KindStatsRollup           = "stats.rollup"
	KindCacheExpire           = "cache.expire"
	// KindInvestigationsRun runs one Investigation (ADR 0053 §3, git-bug 180a525). It
	// is enqueued by `investigator` in the transaction that recorded the request, and
	// nothing on the notification path enqueues it or waits for it.
	KindInvestigationsRun = "investigations.run"
	// KindInvestigationsIncident turns one Incident fact — drawn, or a Case joining or
	// leaving — into the runs of the Investigators that investigate Incidents (ADR
	// 0053 §4, git-bug 74ea849). It is enqueued by the Incident's own membership
	// change, in its transaction, and decides and records runs; it runs none.
	KindInvestigationsIncident = "investigations.incident"
	// KindInvestigationsDigest arms the run for every digest window a policy asked to
	// have summarised, ahead of the window's close (ADR 0053 §4, git-bug 3e96f5a). A
	// per-tenant periodic that records runs and calls no model; nothing on the digest
	// path enqueues it, waits for it, or reads it.
	KindInvestigationsDigest = "investigations.digest"
	// KindRemediesSweep records what the clock decided about Remedies (ADR 0054 §2,
	// git-bug 4148256): every Remedy still proposed or approved past its approval window
	// is recorded `expired` by `system`, and declared. A per-tenant periodic; it never
	// reaches a ToolServer.
	KindRemediesSweep = "remedies.sweep"
	// KindRemediesExecute executes one approved Remedy (ADR 0054 §5, git-bug 4148256):
	// it claims it, calls the operator's write Tool with the approved arguments exactly,
	// and records what came back. Enqueued by the approval that completed it, in that
	// transaction. ⭐ AT MOST ONCE: the claim commits before the call, and a redelivery
	// that finds the Remedy claimed does nothing.
	KindRemediesExecute = "remedies.execute"
)

// Priority levels. River orders 1 (highest) before 4 (lowest) within a queue.
const (
	// PriorityCritical is reserved for work whose latency a human is watching.
	PriorityCritical = 1
	// PriorityHigh is the lifecycle-carrying path: ingest, notify, deliver.
	PriorityHigh = 2
	// PriorityNormal is the default.
	PriorityNormal = 3
	// PriorityBackground is best-effort housekeeping.
	PriorityBackground = 4
)

// Retry ceilings from SPEC §G.6, expressed as River MaxAttempts (the first
// attempt counts, so a "12 retries" budget is 13 attempts).
const (
	// MaxAttemptsRetryable is the §G.6 `retryable` budget: 12 retries then dead.
	MaxAttemptsRetryable = 13
	// MaxAttemptsRateLimited is the §G.6 `rate_limited` budget: 20 then dead.
	// Rate limiting is normally expressed as a snooze (§H.9), which does not
	// consume an attempt; this ceiling is the backstop for a channel that is
	// rate-limited indefinitely.
	MaxAttemptsRateLimited = 21
	// MaxAttemptsPeriodic is deliberately small. A periodic job that fails is
	// re-created by its schedule within a minute or two, so grinding through 13
	// retries of a stale tick only delays the fresh one.
	MaxAttemptsPeriodic = 3
	// MaxAttemptsRemedyExecute is deliberately small, and it is not what makes a Remedy run
	// at most once — the claim does (RemediesExecuteArgs). It bounds how long a database
	// that cannot answer before the claim keeps an approved Remedy waiting; one that never
	// runs expires on its window, on the record.
	MaxAttemptsRemedyExecute = 3
)
