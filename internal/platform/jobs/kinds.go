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
	// QueueLifecycle carries the periodic state-machine sweeps: reaper, group
	// close, flap scoring, unacked reminders.
	QueueLifecycle = "lifecycle"
	// QueueMaintenance carries partition management, retention and rollups. One
	// worker: these are DDL-adjacent and must not race themselves.
	QueueMaintenance = "maintenance"
)

// AllQueues is every queue oto declares, in the order they appear in SPEC §G.3.
func AllQueues() []string {
	return []string{
		QueueIngest, QueueEnrich, QueueNotify,
		QueueDeliverSlack, QueueDeliverWebhook,
		QueueReconcile, QueueLifecycle, QueueMaintenance,
	}
}

// Job kinds (SPEC §G.3). These strings are a DURABLE WIRE CONTRACT: they are
// written into `river_job.kind` and are matched by workers on the other side of a
// deploy. Renaming one strands every queued job of that kind.
const (
	KindIngestProcessBatch = "ingest.process_batch"
	KindEnrichRun          = "enrich.run"
	KindNotifyEvaluate     = "notify.evaluate"
	KindDeliverDispatch    = "deliver.dispatch"
	// KindSlackInteraction is one verified Slack block action, taken off the HTTP
	// request so the endpoint can answer inside Slack's three-second window.
	KindSlackInteraction      = "slack.interaction"
	KindSourceReconcile       = "source.reconcile"
	KindSilencesSync          = "silences.sync"
	KindCaseReap              = "case.reap"
	KindGroupClose            = "group.close"
	KindFlapScore             = "flap.score"
	KindNotifyUnackedReminder = "notify.unacked_reminder"
	KindPartitionsManage      = "partitions.manage"
	KindRetentionPrune        = "retention.prune"
	KindStatsRollup           = "stats.rollup"
	KindCacheExpire           = "cache.expire"
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
)
