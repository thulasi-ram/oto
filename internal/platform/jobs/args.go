package jobs

import (
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

// Payload carries the payload version every job in oto is required to have
// (SPEC §G.3): "Every job payload carries `v: <int>`. A worker that meets a
// payload version it does not understand MUST park the job."
//
// Embed it in every JobArgs struct. Zero means version 1, so a literal built
// without it is still valid today; a producer emitting version 2 or later MUST
// set V explicitly, and from that moment the old workers park it instead of
// misreading it. That asymmetry is the whole point: adding a field is safe,
// changing the meaning of one is not, and the version is how you say which
// you did.
type Payload struct {
	V int `json:"v,omitempty"`
}

// PayloadVersion is the declared version, normalising the zero value to 1.
func (p Payload) PayloadVersion() int {
	if p.V <= 0 {
		return 1
	}
	return p.V
}

// versioned is satisfied by every args struct through the embedded Payload. The
// worker runtime uses it to gate an unknown version before touching the handler.
type versioned interface {
	PayloadVersion() int
}

// ---------------------------------------------------------------- ingest

// IngestProcessBatchArgs processes one durably-recorded webhook batch. It is the
// ONLY write path into `alerts` (SPEC §G.4).
//
// Queue: ingest · Priority: high · Retry: retryable (12) · Payload v1
//
// IDEMPOTENCY KEY: BatchID.
// The handler's first act is to load `ingest_batches` and exit when
// `status != 'pending'`, so a redelivery of the same batch is a no-op. Upstream,
// `ingest_dedup UNIQUE (source_id, dedup_key)` (SPEC §C.5) already collapsed
// webhook replays onto one batch, so BatchID is a stable, oto-computed key and a
// duplicate insert of this job cannot double-apply anything.
type IngestProcessBatchArgs struct {
	Payload
	BatchID    uuid.UUID `json:"batch_id"`
	ReceivedAt time.Time `json:"received_at"`
}

// Kind implements db.JobArgs and river.JobArgs.
func (IngestProcessBatchArgs) Kind() string { return KindIngestProcessBatch }

// InsertOpts pins the queue, priority and retry ceiling of this job type.
func (IngestProcessBatchArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueIngest,
		Priority:    PriorityHigh,
		MaxAttempts: MaxAttemptsRetryable,
	}
}

// ---------------------------------------------------------------- enrichment

// PreNotificationBudget is how long a `fired` evaluation waits for the inline
// enrichment pass before going out without it (SPEC §F.3's 2 000 ms).
//
// It lives here, next to the two job types it orders, because it is a fact about
// the SCHEDULE and both producers must read the same number: `alerts` uses it as
// the backstop delay on `notify.evaluate`, `enrichment` treats it as the ceiling
// on the pass that releases that evaluation early.
//
// ⛔ IT IS A CEILING AND MUST NOT BE RAISED TO BUY MORE ENRICHMENT. Every
// millisecond here is a millisecond an operator does not know their service is
// down. An enricher that cannot fit belongs in the async phase, where its result
// amends a card somebody has already read.
const PreNotificationBudget = 2000 * time.Millisecond

// EnrichRunArgs runs the budgeted enrichment pipeline for one occurrence.
//
// Queue: enrich · Priority: normal · Retry: retryable (12) · Payload v1
//
// IDEMPOTENCY KEY: (OccurrenceID, Phase).
// Enrichment results are upserted on `(occurrence_id, enricher, enricher_version)`,
// so re-running a phase overwrites its own rows and never accumulates duplicates.
// Phase is part of the key because the inline (T1) and async passes are different
// budgets over the same occurrence and must both be allowed to run.
type EnrichRunArgs struct {
	Payload
	OccurrenceID uuid.UUID `json:"occurrence_id"`
	// Phase is "inline" or "async" (SPEC §G.3: re-enqueued on inline timeout).
	Phase string `json:"phase"`
	// Enrichers narrows the run to named enrichers. Empty means the policy's set.
	Enrichers []string `json:"enrichers,omitempty"`
}

// Kind implements db.JobArgs and river.JobArgs.
func (EnrichRunArgs) Kind() string { return KindEnrichRun }

// InsertOpts pins the queue, priority and retry ceiling of this job type.
func (EnrichRunArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueEnrich,
		Priority:    PriorityNormal,
		MaxAttempts: MaxAttemptsRetryable,
	}
}

// ---------------------------------------------------------------- notification

// NotifyEvaluateArgs evaluates notification policy for one group generation after
// a lifecycle transition. It is enqueued by `alerts`, which never imports
// `notification` — the queue is the seam (SPEC §I.1).
//
// Queue: notify · Priority: high · Retry: retryable (12) · Payload v1
//
// IDEMPOTENCY KEY: sha256(org_id, subject_kind, subject_id, reason, state_version)
// — literally `notifications.idempotency_key` (SPEC §C.7), enforced by
// `notifications_idem_uniq UNIQUE (org_id, idempotency_key)`. StateVersion is in
// the payload for exactly this reason: it pins the intent to the group state it
// was minted against, so a re-run at a newer version mints a NEW notification
// rather than resending an old one, and a re-run at the same version collides on
// the unique index and is swallowed. A 23505 here is the mechanism working, not
// an error (SPEC §L.9).
type NotifyEvaluateArgs struct {
	Payload
	GroupID uuid.UUID `json:"group_id"`
	// Reason is the §H.6 Reason enum value that triggered this evaluation.
	Reason string `json:"reason"`
	// StateVersion is the alert_groups.state_version this evaluation is about.
	StateVersion int `json:"state_version"`
	// AlertID is set when the fact is about one Alert. MANDATORY for the
	// alert-scoped reasons (acked, unacked, refired, rule_changed) — see
	// notifications_focus_ck.
	AlertID *uuid.UUID `json:"alert_id,omitempty"`
	// OccurrenceID narrows to one episode when the fact is about one.
	OccurrenceID *uuid.UUID `json:"occurrence_id,omitempty"`
	// Actor labels the human or system that caused this, for the rendered card.
	Actor string `json:"actor,omitempty"`
}

// Kind implements db.JobArgs and river.JobArgs.
func (NotifyEvaluateArgs) Kind() string { return KindNotifyEvaluate }

// InsertOpts pins the queue, priority and retry ceiling of this job type.
func (NotifyEvaluateArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueNotify,
		Priority:    PriorityHigh,
		MaxAttempts: MaxAttemptsRetryable,
	}
}

// DeliverDispatchArgs sends one NotificationDelivery on one Channel. It is the
// ONLY job subject to the per-thread ordering gate (SPEC §G.7); see
// platform/jobs/ordering.
//
// Queue: deliver_slack | deliver_webhook (from ChannelType) · Priority: high ·
// Retry: rate_limited ceiling (20), because a throttled Slack channel is the
// common case and must not burn the retryable budget · Payload v1
//
// IDEMPOTENCY KEY: DeliveryID, enforced by the optimistic-lock CLAIM
// (SPEC §G.5): `UPDATE notification_deliveries SET status='sending',
// attempts=attempts+1 WHERE id=$1 AND status IN ('pending','failed') RETURNING *`.
// A duplicate worker gets zero rows and exits. The fan-out itself is unique on
// `(notification_id, channel_id)`, so a delivery id is minted at most once per
// (notification, channel) pair.
//
// The genuinely ambiguous case — crash after Slack accepted, before the ts was
// recorded — is NOT solved here and cannot be: Slack has no idempotency key and
// oto never reads Slack back (C9). SPEC §G.5 resolves it by re-sending a
// `post_root` with `ambiguous = true` and a visible marker. Under-delivering a
// firing alert is worse than a labelled duplicate.
type DeliverDispatchArgs struct {
	Payload
	DeliveryID uuid.UUID `json:"delivery_id"`
	// ChannelType selects the queue: "slack" or "webhook". It is denormalised
	// into the payload so that queue routing needs no database read at insert
	// time, inside the caller's transaction, on the hot path.
	ChannelType string `json:"channel_type,omitempty"`
}

// Kind implements db.JobArgs and river.JobArgs.
func (DeliverDispatchArgs) Kind() string { return KindDeliverDispatch }

// InsertOpts routes the job to the queue for its channel type.
func (a DeliverDispatchArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       DeliverQueueFor(a.ChannelType),
		Priority:    PriorityHigh,
		MaxAttempts: MaxAttemptsRateLimited,
	}
}

// DeliverQueueFor maps a channel provider type onto its delivery queue.
//
// Anything that is not Slack goes to the generic queue: the webhook provider has
// no per-channel write limit and must never inherit Slack's narrow concurrency.
func DeliverQueueFor(channelType string) string {
	if channelType == "slack" {
		return QueueDeliverSlack
	}
	return QueueDeliverWebhook
}

// NotifyUnackedReminderArgs sweeps for open groups whose oldest member occurrence
// has been firing and unacked past the policy's `unacked_reminder_after_s`
// (SPEC §G.9). Periodic, 60 s, zero payload.
//
// Queue: lifecycle · Priority: normal · Retry: periodic (3) · Payload v1
//
// IDEMPOTENCY KEY: none needed at the job level — the sweep is a query, and the
// reminder it may mint is a Notification, unique on `(org_id, idempotency_key)`
// and fired AT MOST ONCE PER GROUP GENERATION by §G.9. Job-level uniqueness is
// by kind and period so two pods cannot both run the same tick.
//
// THERE IS EXACTLY ONE STAGE, FOREVER (SPEC §G.9.1, BINDING, PERMANENT). This
// job must never gain a stage index, a target other than the policy's existing
// channels, or any awareness of who is on call.
type NotifyUnackedReminderArgs struct {
	Payload
}

// Kind implements db.JobArgs and river.JobArgs.
func (NotifyUnackedReminderArgs) Kind() string { return KindNotifyUnackedReminder }

// InsertOpts pins the queue, priority, retry ceiling and tick uniqueness.
func (NotifyUnackedReminderArgs) InsertOpts() river.InsertOpts {
	return periodicOpts(QueueLifecycle, PriorityNormal, time.Minute)
}

// ------------------------------------------------------- channels (inbound)

// SlackInteractionArgs carries ONE verified Slack block action off the HTTP
// request and onto a queue.
//
// Queue: notify · Priority: CRITICAL · Retry: retryable (12) · Payload v1
//
// ⛔ IT EXISTS BECAUSE OF SLACK'S THREE-SECOND RULE (§H.8). An interaction that
// is not acknowledged within three seconds shows the user "This app is not
// responding", and resolving the org, reading the group's members and writing an
// acknowledgement is several round trips to Postgres. The endpoint therefore
// answers 200 the moment the HMAC verifies and enqueues this, so a slow database
// costs a late card, never a failed button.
//
// ⛔ EVERY FIELD IS COPIED FROM THE SIGNED ENVELOPE AND NONE OF IT IS AUTHORITY.
// `Value` is an opaque UUID (S8) resolved against oto's own tables, and the org
// is resolved from TeamID + ChannelID — never from anything the button carries.
// The signature proved the envelope came from Slack; it proved nothing about
// what the envelope may do.
//
// IDEMPOTENCY KEY: none of its own, deliberately. The acknowledgement itself is
// idempotent in the domain — `Occurrence.Acknowledge` refuses an already-acked
// episode with `already_acked` — so a replayed payload, a double-click and a
// retried job all converge on the same row. The uniqueness window the producer
// applies is a convenience that collapses a byte-identical replay while the
// first job is still in flight; it is not the correctness mechanism (§G.5).
type SlackInteractionArgs struct {
	Payload
	// ActionID is the button's stable id, e.g. `oto.ack`. It is the dispatch key.
	ActionID string `json:"action_id"`
	// Value is the button's opaque identifier — an alert group id for `oto.ack`.
	Value string `json:"value"`
	// TeamID and ChannelID are the Slack workspace and conversation the press
	// came from. TOGETHER THEY RESOLVE THE TENANT, and nothing else may.
	TeamID    string `json:"team_id"`
	ChannelID string `json:"channel_id"`
	// SlackUserID is the member who pressed it; SlackUserName is their display
	// name at press time, denormalised so a timeline entry survives a rename.
	SlackUserID   string `json:"slack_user_id"`
	SlackUserName string `json:"slack_user_name,omitempty"`
	// MessageTS is the root message the button sits on, for correlation in logs.
	MessageTS string `json:"message_ts,omitempty"`
	// ResponseURL is Slack's one-shot reply channel for this interaction. It is
	// how oto tells the user that an action could not apply, and it needs no
	// token and no scope. Empty means the user gets no reply.
	ResponseURL string `json:"response_url,omitempty"`
}

// Kind implements db.JobArgs and river.JobArgs.
func (SlackInteractionArgs) Kind() string { return KindSlackInteraction }

// InsertOpts pins the queue, priority and retry ceiling of this job type.
//
// PriorityCritical is the whole point: a human pressed a button and is watching
// the card. It rides the notify queue because what it produces is a notification
// — the acked card — and giving it a queue of its own would buy an isolation
// boundary against nothing.
func (SlackInteractionArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueNotify,
		Priority:    PriorityCritical,
		MaxAttempts: MaxAttemptsRetryable,
	}
}

// ---------------------------------------------------------------- sources

// SourceReconcileArgs runs the mandatory Alertmanager v2 reconciler for one
// source (SPEC §G.8). It is the ONLY producer of `suppressed`, and its
// divergence count is the canary for every correctness bug in the system.
//
// Queue: reconcile · Priority: normal · Retry: retryable (12) · Payload v1
//
// IDEMPOTENCY KEY: (SourceID, tick). The reconciler is a pure read-and-observe
// pass whose Observations feed the same state machine as ingestion, and that
// state machine is idempotent by `alert_event_keys UNIQUE (org_id, dedupe_key)`
// (SPEC §C.8). Running it twice therefore costs two HTTP calls and changes
// nothing. Tick uniqueness is enforced by River over (kind, args, period) so a
// slow run does not stack up behind itself.
type SourceReconcileArgs struct {
	Payload
	SourceID uuid.UUID `json:"source_id"`
}

// Kind implements db.JobArgs and river.JobArgs.
func (SourceReconcileArgs) Kind() string { return KindSourceReconcile }

// InsertOpts pins the queue, priority, retry ceiling and tick uniqueness.
func (SourceReconcileArgs) InsertOpts() river.InsertOpts {
	o := periodicOpts(QueueReconcile, PriorityNormal, 30*time.Second)
	o.MaxAttempts = MaxAttemptsRetryable
	return o
}

// SilencesSyncArgs mirrors one source's Alertmanager silences. READ ONLY — the
// silences module has no write path (R3).
//
// Queue: reconcile · Priority: background · Retry: retryable (12) · Payload v1
//
// IDEMPOTENCY KEY: (SourceID, tick). The sync is an upsert of a read-only mirror
// keyed by the upstream silence id, so re-running converges rather than
// accumulates.
type SilencesSyncArgs struct {
	Payload
	SourceID uuid.UUID `json:"source_id"`
}

// Kind implements db.JobArgs and river.JobArgs.
func (SilencesSyncArgs) Kind() string { return KindSilencesSync }

// InsertOpts pins the queue, priority, retry ceiling and tick uniqueness.
func (SilencesSyncArgs) InsertOpts() river.InsertOpts {
	o := periodicOpts(QueueReconcile, PriorityBackground, time.Minute)
	o.MaxAttempts = MaxAttemptsRetryable
	return o
}

// ---------------------------------------------------------------- lifecycle

// OccurrenceReapArgs expires occurrences oto has stopped hearing about
// (SPEC §B.4). Periodic, 60 s, zero payload.
//
// Queue: lifecycle · Priority: normal · Retry: periodic (3) · Payload v1
//
// IDEMPOTENCY KEY: none needed — the sweep selects candidates by state and time
// and every transition it writes is deduped by `alert_event_keys` (§C.8). Running
// one tenant's reap twice reaps the same candidates and appends nothing the
// second time; the snooze expiry that rides the same tick is a compare-and-set on
// an episode that is already unsnoozed. Tick uniqueness is by kind, ARGS and
// period, and TenantFanOut is what makes that per-tenant: each org gets its own
// once-a-minute slot instead of every org sharing one.
//
// THE REAPER IS BLOCKED while `source_health.status != 'healthy'`. Losing sight
// of an alert is not the alert resolving, and `expired` is never `resolved`.
type OccurrenceReapArgs struct {
	Payload
	TenantFanOut
}

// Kind implements db.JobArgs and river.JobArgs.
func (OccurrenceReapArgs) Kind() string { return KindOccurrenceReap }

// InsertOpts pins the queue, priority, retry ceiling and tick uniqueness.
func (OccurrenceReapArgs) InsertOpts() river.InsertOpts {
	return periodicOpts(QueueLifecycle, PriorityNormal, time.Minute)
}

// GroupCloseArgs closes group generations whose members have all ended, freezes
// their ChannelThreads, and — critically — performs the §G.7.3 GAP RECOVERY that
// advances `channel_threads.last_sent_seq` past a dead delivery so one poison
// message can never wedge a thread forever. Periodic, 60 s, zero payload.
//
// Queue: lifecycle · Priority: normal · Retry: periodic (3) · Payload v1
//
// IDEMPOTENCY KEY: none needed — closing a closed group and advancing an already
// advanced sequence are both no-ops under the thread advisory lock, so a
// re-delivered per-tenant pass converges rather than double-applying.
type GroupCloseArgs struct {
	Payload
	TenantFanOut
}

// Kind implements db.JobArgs and river.JobArgs.
func (GroupCloseArgs) Kind() string { return KindGroupClose }

// InsertOpts pins the queue, priority, retry ceiling and tick uniqueness.
func (GroupCloseArgs) InsertOpts() river.InsertOpts {
	return periodicOpts(QueueLifecycle, PriorityNormal, time.Minute)
}

// FlapScoreArgs recomputes flap scores (SPEC §B.6). Periodic, 300 s, zero payload.
//
// Queue: lifecycle · Priority: background · Retry: periodic (3) · Payload v1
//
// IDEMPOTENCY KEY: none needed — the score is a pure function of the event
// history, written with SetFlap, so recomputation is convergent and a per-tenant
// pass that runs twice writes the same score twice.
type FlapScoreArgs struct {
	Payload
	TenantFanOut
}

// Kind implements db.JobArgs and river.JobArgs.
func (FlapScoreArgs) Kind() string { return KindFlapScore }

// InsertOpts pins the queue, priority, retry ceiling and tick uniqueness.
func (FlapScoreArgs) InsertOpts() river.InsertOpts {
	return periodicOpts(QueueLifecycle, PriorityBackground, 5*time.Minute)
}

// ---------------------------------------------------------------- maintenance

// PartitionsManageArgs creates partitions ahead and DETACHes plus DROPs expired
// ones (SPEC §D.11). Periodic, 3600 s, zero payload.
//
// Queue: maintenance · Priority: background · Retry: periodic (3) · Payload v1
//
// IDEMPOTENCY KEY: none needed — every statement is CREATE … IF NOT EXISTS or
// DROP … IF EXISTS. It additionally takes db.LockNamespacePartition so two pods
// cannot race the same DDL.
//
// Retention is DROP TABLE, always. Never DELETE FROM a partitioned event table.
type PartitionsManageArgs struct {
	Payload
}

// Kind implements db.JobArgs and river.JobArgs.
func (PartitionsManageArgs) Kind() string { return KindPartitionsManage }

// InsertOpts pins the queue, priority, retry ceiling and tick uniqueness.
func (PartitionsManageArgs) InsertOpts() river.InsertOpts {
	return periodicOpts(QueueMaintenance, PriorityBackground, time.Hour)
}

// RetentionPruneArgs enforces the configured retention windows. Periodic, 3600 s.
//
// Queue: maintenance · Priority: background · Retry: periodic (3) · Payload v1
//
// ⭐ IT IS THE ONE KIND HERE WHOSE TWO SHAPES DO DIFFERENT WORK, because its body
// is genuinely mixed and forcing all of it into a per-tenant shape would be a
// lie. The fan-out shape does the GLOBAL prunes — `ingest_dedup`,
// `idempotency_claims`, `alert_event_keys`, expired sessions — which are
// unpartitioned, globally-unique tables with no tenant to loop over; the
// per-tenant shape does the drill sweep, which is the only per-org work in the
// job. Splitting the org loop out of a body that also guards webhook replay is
// what the ticket asked for; giving it a second job kind would have bought a new
// queue, a new retry policy and a new metric to express "the same hourly sweep".
//
// IDEMPOTENCY KEY: none needed — pruning is convergent in both shapes. Every
// global prune is a bounded DELETE by a time predicate, so a second run deletes
// what the first one left and nothing else; the per-tenant drill sweep settles
// drills whose window has closed and disposes rows already marked disposable, and
// a re-run finds them settled and disposed.
type RetentionPruneArgs struct {
	Payload
	TenantFanOut
}

// Kind implements db.JobArgs and river.JobArgs.
func (RetentionPruneArgs) Kind() string { return KindRetentionPrune }

// InsertOpts pins the queue, priority, retry ceiling and tick uniqueness.
func (RetentionPruneArgs) InsertOpts() river.InsertOpts {
	return periodicOpts(QueueMaintenance, PriorityBackground, time.Hour)
}

// StatsRollupArgs rolls one day into `alert_quality_daily`. Periodic, 900 s.
//
// Queue: maintenance · Priority: background · Retry: periodic (3) · Payload v1
//
// IDEMPOTENCY KEY: (OrgID, Day). The rollup is a full recomputation of one day
// upserted on `(org_id, day, cluster_key, alertname)`, so re-running any day for
// any tenant converges — it REPLACES the stored counters rather than accumulating
// onto them, which is the only safe shape under an at-least-once queue. Day is a
// string in RFC 3339 date form ("2026-08-07") rather than a time.Time so the
// payload is stable under timezone handling — a rollup keyed by an instant that
// re-serialises differently would silently produce two rows.
//
// ⚠️ THE DAY TRAVELS ON BOTH SHAPES. The fan-out tick is what reads the clock, and
// it copies its own Day onto every per-tenant job it queues, so a fan-out that
// straddles midnight UTC still rolls up ONE day for every tenant rather than
// giving the tenants at the end of the walk a different one from the tenants at
// the start.
//
// alert_quality_daily carries NO user column, by construction. NEVER PER-PERSON (R8).
type StatsRollupArgs struct {
	Payload
	TenantFanOut
	Day string `json:"day"`
}

// Kind implements db.JobArgs and river.JobArgs.
func (StatsRollupArgs) Kind() string { return KindStatsRollup }

// InsertOpts pins the queue, priority, retry ceiling and tick uniqueness.
func (StatsRollupArgs) InsertOpts() river.InsertOpts {
	return periodicOpts(QueueMaintenance, PriorityBackground, 15*time.Minute)
}

// CacheExpireArgs evicts expired enrichment cache entries. Periodic, 600 s.
//
// Queue: maintenance · Priority: background · Retry: periodic (3) · Payload v1
//
// IDEMPOTENCY KEY: none needed — eviction is convergent.
type CacheExpireArgs struct {
	Payload
}

// Kind implements db.JobArgs and river.JobArgs.
func (CacheExpireArgs) Kind() string { return KindCacheExpire }

// InsertOpts pins the queue, priority, retry ceiling and tick uniqueness.
func (CacheExpireArgs) InsertOpts() river.InsertOpts {
	return periodicOpts(QueueMaintenance, PriorityBackground, 10*time.Minute)
}

// periodicOpts is the shared shape of every scheduled job: a short retry budget
// and a uniqueness window matching the tick, so a slow run cannot stack up behind
// itself and two leader-elected pods cannot both insert the same tick.
//
// ByState is left at River's default, which excludes cancelled and discarded —
// a job that died must be allowed to be re-created by the next tick.
func periodicOpts(queue string, priority int, period time.Duration) river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue,
		Priority:    priority,
		MaxAttempts: MaxAttemptsPeriodic,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByQueue:  true,
			ByPeriod: period,
		},
	}
}
