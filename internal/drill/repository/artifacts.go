package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/drill/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// batchWindow bounds the partition scan for the drill's own batch.
//
// `ingest_batches` is partitioned by `received_at`, which is oto's clock at
// accept time — a few milliseconds after the drill's `started_at`. Without a
// bound the lookup would scan every daily partition in the retention window;
// with it the planner prunes to one. It is generous because a clock is a clock.
const batchWindow = 5 * time.Minute

// Artifacts reads back everything the REAL pipeline wrote for this drill.
//
// ⭐⭐ EVERY QUERY HERE IS A LOOK, NEVER A REPORT. No stage tells the drill it
// succeeded; the drill goes and finds the row. That is what makes a passing drill
// evidence rather than a claim — if a stage silently stops writing its row, the
// drill notices, where an instrumented pipeline would keep reporting a step that
// no longer happens.
//
// It is deliberately several small statements rather than one join. Each is
// index-backed on its own key, each is independently readable next to the stage
// it feeds, and a drill runs at most a few times a day — the round trips buy
// clarity at a price nobody can measure.
func (r *DrillRepository) Artifacts(
	ctx context.Context, s db.TenantScope, d domain.Drill,
) (domain.Artifacts, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Artifacts{}, err
	}
	var out domain.Artifacts

	if err := r.readBatch(ctx, s, d, &out); err != nil {
		return domain.Artifacts{}, err
	}
	if err := r.readAlert(ctx, s, d, &out); err != nil {
		return domain.Artifacts{}, err
	}
	if out.Alert.Found {
		if err := r.readCase(ctx, s, out.Alert.ID, &out); err != nil {
			return domain.Artifacts{}, err
		}
	}
	// ⛔ THE CASE IS THE HINGE, WHERE A `readGroup` USED TO STAND (git-bug `7570090`).
	// The walk was alert → case → GROUP → notification → thread, because the group
	// generation was what `notifications.group_id` and `channel_threads.subject_id`
	// both addressed. Both now address the Case, so the group read is not replaced by
	// another read — it is gone, and one fewer row has to exist before a drill can
	// say where the card went.
	if out.Case.Found {
		if err := r.readNotification(ctx, s, out.Case.ID, &out); err != nil {
			return domain.Artifacts{}, err
		}
		if err := r.readThreads(ctx, s, out.Case.ID, &out); err != nil {
			return domain.Artifacts{}, err
		}
	}
	if out.Notification.Found {
		if err := r.readDeliveries(ctx, s, out.Notification.ID, &out); err != nil {
			return domain.Artifacts{}, err
		}
	}
	return out, nil
}

func (r *DrillRepository) readBatch(
	ctx context.Context, s db.TenantScope, d domain.Drill, out *domain.Artifacts,
) error {
	if d.BatchID == uuid.Nil {
		return nil
	}
	from := d.StartedAt.Add(-batchWindow)
	to := d.StartedAt.Add(batchWindow)

	var batch domain.BatchFact
	err := r.db(ctx).QueryRow(ctx, `
SELECT status, COALESCE(error, ''), alert_count
  FROM ingest_batches
 WHERE org_id = $1 AND id = $2 AND received_at >= $3 AND received_at < $4`,
		s.OrgID(), d.BatchID, from, to,
	).Scan(&batch.Status, &batch.Error, &batch.AlertCount)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The batch aged out of its retention partition, which after 30 days is
		// policy rather than an error. There is nothing left to report on and the
		// drill's frozen outcome is what an operator reads by then.
		return nil
	case err != nil:
		return mapErr(err, "read the drill's ingest batch")
	}
	batch.Found = true
	out.Batch = batch

	rows, err := r.db(ctx).Query(ctx, `
SELECT reason, COALESCE(detail, '')
  FROM ingest_rejections
 WHERE org_id = $1 AND batch_id = $2 AND received_at >= $3 AND received_at < $4
 LIMIT 20`, s.OrgID(), d.BatchID, from, to)
	if err != nil {
		return mapErr(err, "read the drill's ingest rejections")
	}
	defer rows.Close()
	for rows.Next() {
		var f domain.RejectionFact
		if err := rows.Scan(&f.Reason, &f.Detail); err != nil {
			return mapErr(err, "scan an ingest rejection")
		}
		out.Rejections = append(out.Rejections, f)
	}
	return mapRowsErr(rows, "read the drill's ingest rejections")
}

// readAlert finds the drill's Alert by LABEL CONTAINMENT rather than by
// `alert_key`.
//
// ⭐ THE REASON IS `ignore_labels`. A source can be configured to exclude labels
// from the §C.2 identity hash, so the drill cannot compute the `alert_key` its own
// payload will produce without re-implementing the source's config — and getting
// that subtly wrong would make a working drill report "no alert". Containment on
// `oto_drill` rides `alerts_labels_gin` and is true whatever the hash did.
func (r *DrillRepository) readAlert(
	ctx context.Context, s db.TenantScope, d domain.Drill, out *domain.Artifacts,
) error {
	var alert domain.AlertFact
	err := r.db(ctx).QueryRow(ctx, `
SELECT id, alert_key, synthetic, state
  FROM alerts
 WHERE org_id = $1 AND labels @> jsonb_build_object('oto_drill', $2::text)
 ORDER BY first_seen_at DESC
 LIMIT 1`, s.OrgID(), d.Label,
	).Scan(&alert.ID, &alert.Key, &alert.Synthetic, &alert.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapErr(err, "read the drill's alert")
	}
	alert.Found = true
	out.Alert = alert
	return nil
}

func (r *DrillRepository) readCase(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, out *domain.Artifacts,
) error {
	var (
		ac       domain.CaseFact
		snapshot *uuid.UUID
		ruleName *string
	)
	err := r.db(ctx).QueryRow(ctx, `
SELECT o.id, o.seq,
       -- ADR 0040: recomposed from the alert and resolve_reason, because
       -- alert_cases.state is open | closed. A drill asserts the episode it
       -- manufactured is FIRING, so the word matters and the column no longer
       -- spells it.
       -- ADR 0041: and the suppression axis is recomposed too, so a drill run
       -- while a silence is in force still reads the word a human expects.
       CASE WHEN o.state = 'open' AND a.suppression_reason IS NOT NULL THEN 'suppressed'
            WHEN o.state = 'open' THEN a.state
            WHEN o.resolve_reason = 'timeout' THEN 'expired'
            ELSE 'resolved' END,
       o.rule_snapshot_id, rs.rule_name
  FROM alert_cases o
  JOIN alerts a ON a.id = o.alert_id AND a.org_id = o.org_id
  LEFT JOIN rule_snapshots rs ON rs.id = o.rule_snapshot_id AND rs.org_id = o.org_id
 WHERE o.org_id = $1 AND o.alert_id = $2
 ORDER BY o.seq DESC
 LIMIT 1`, s.OrgID(), alertID,
	).Scan(&ac.ID, &ac.Seq, &ac.State, &snapshot, &ruleName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapErr(err, "read the drill's case")
	}
	ac.Found = true
	ac.RuleSnapshotID = idOrNil(snapshot)
	if ruleName != nil {
		ac.RuleName = *ruleName
	}
	out.Case = ac
	return nil
}

// ⛔ `readGroup` WAS HERE AND IS DELETED (git-bug `7570090`). It JOINed
// `alert_groups` onto `alert_cases` by `o.group_id` for the generation's key, its
// generation number, its `synthetic` mark and its title; the table and the column
// are both dropped. It also set `Member` unconditionally on the grounds that
// "membership IS how the group was found" — since 00051 that membership WAS
// `alert_cases.group_id`, so the join and the membership proof were the same fact
// twice, and both went with the column.
//
// ⭐ THE ARGUMENT THAT SURVIVES IT is the one about `alert_group_members`: a
// membership recorded in its own row could outlive the thing it recorded, because
// nothing ever wrote `left_at`. Deriving beat storing then, and this is the same
// move one step further — the conversation is not stored beside the Case, it IS the
// Case.

// readNotification takes the OLDEST intent for the Case's conversation, not the
// newest.
//
// ⭐ The first intent is the one the drill caused; anything after it is a
// consequence (a repeat, a resolution) and reporting the latest would make a
// drill's policy verdict drift as the Case lived on.
//
// ⛔ IT ADDRESSES THE CONVERSATION, NOT THE SUBJECT, and the two are different
// questions even when they carry the same uuid. This read exists to answer "where
// did the card go", so it matches `(conversation_kind, conversation_id)` — the pair
// that replaced `notifications.group_id` (migration 00064) — rather than
// `(subject_kind, subject_id)`, which answers "what was the fact about". A digest
// is about a window and lands in its policy's conversation; a drill's fact is about
// its Case and lands in the Case's, and asking the delivery-target half is what
// keeps this query reading the same column the thread is keyed by.
func (r *DrillRepository) readNotification(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID, out *domain.Artifacts,
) error {
	var (
		notif                  domain.NotificationFact
		policyID               *uuid.UUID
		policyName, suppressed *string
	)
	err := r.db(ctx).QueryRow(ctx, `
SELECT n.id, n.status, n.suppressed_reason, n.reason, n.policy_id, p.name
  FROM notifications n
  LEFT JOIN notification_policies p ON p.id = n.policy_id AND p.org_id = n.org_id
 WHERE n.org_id = $1 AND n.conversation_kind = 'case' AND n.conversation_id = $2
 ORDER BY n.created_at ASC
 LIMIT 1`, s.OrgID(), caseID,
	).Scan(&notif.ID, &notif.Status, &suppressed, &notif.Reason, &policyID, &policyName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapErr(err, "read the drill's notification")
	}
	notif.Found = true
	notif.PolicyID = idOrNil(policyID)
	if policyName != nil {
		notif.PolicyName = *policyName
	}
	if suppressed != nil {
		notif.SuppressedReason = *suppressed
	}
	out.Notification = notif
	return nil
}

// readThreads finds the conversation by its Case.
//
// ⚠️ THE COLUMN IS STILL SPELLED `subject_kind` AND THE VALUE IS NO LONGER
// `'alert_group'` (git-bug `7570090`). `channel_threads` keys a conversation by
// `(subject_kind, subject_id)` and `'alert_group'` named a generation; the kind a
// Case conversation writes is `'case'` (`ConversationCase.SubjectKind()`). Renaming
// the column to say `conversation_kind`, the way `notifications` already does, is
// 7570090's later stage — so this predicate reads a little crooked on purpose
// rather than reading a value nothing writes.
func (r *DrillRepository) readThreads(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID, out *domain.Artifacts,
) error {
	rows, err := r.db(ctx).Query(ctx, `
SELECT t.channel_id, c.name, t.state,
       COALESCE(t.provider_conversation_id, ''), COALESCE(t.provider_thread_id, ''),
       COALESCE(t.dead_reason, ''), t.last_sent_seq
  FROM channel_threads t
  JOIN channels c ON c.id = t.channel_id AND c.org_id = t.org_id
 WHERE t.org_id = $1 AND t.subject_kind = 'case' AND t.subject_id = $2`,
		s.OrgID(), caseID)
	if err != nil {
		return mapErr(err, "read the drill's threads")
	}
	defer rows.Close()
	for rows.Next() {
		var f domain.ThreadFact
		if err := rows.Scan(&f.ChannelID, &f.ChannelName, &f.State,
			&f.ProviderConversationID, &f.ProviderThreadID, &f.DeadReason, &f.LastSentSeq); err != nil {
			return mapErr(err, "scan a channel thread")
		}
		out.Threads = append(out.Threads, f)
	}
	return mapRowsErr(rows, "read the drill's threads")
}

func (r *DrillRepository) readDeliveries(
	ctx context.Context, s db.TenantScope, notificationID uuid.UUID, out *domain.Artifacts,
) error {
	rows, err := r.db(ctx).Query(ctx, `
SELECT d.channel_id, c.name, d.status, d.mode, COALESCE(d.thread_seq, 0), d.attempts,
       COALESCE(d.error, ''), COALESCE(d.error_class, ''),
       COALESCE(d.provider_message_id, ''), d.ambiguous
  FROM notification_deliveries d
  JOIN channels c ON c.id = d.channel_id AND c.org_id = d.org_id
 WHERE d.org_id = $1 AND d.notification_id = $2
 ORDER BY d.created_at ASC`, s.OrgID(), notificationID)
	if err != nil {
		return mapErr(err, "read the drill's deliveries")
	}
	defer rows.Close()
	for rows.Next() {
		var f domain.DeliveryFact
		if err := rows.Scan(&f.ChannelID, &f.ChannelName, &f.Status, &f.Mode, &f.ThreadSeq,
			&f.Attempts, &f.Error, &f.ErrorClass, &f.ProviderMessageID, &f.Ambiguous); err != nil {
			return mapErr(err, "scan a delivery")
		}
		out.Deliveries = append(out.Deliveries, f)
	}
	return mapRowsErr(rows, "read the drill's deliveries")
}

func mapRowsErr(rows pgx.Rows, what string) error {
	if err := rows.Err(); err != nil {
		return mapErr(err, what)
	}
	return nil
}
