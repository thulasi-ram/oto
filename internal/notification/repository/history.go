package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// Summary is one notification intent plus the health of its fan-out.
//
// It answers the question CONTEXT.md §6 calls out by name: "was anybody told
// about this alert, and did it land?" DELIVERY FAILURE MUST BE VISIBLE PER
// ALERT. oto's silence must never be indistinguishable from "no alert", and the
// only way to make that true is for the counts to be readable next to the alert
// itself rather than buried in an operator dashboard.
type Summary struct {
	ID uuid.UUID
	// ⛔ `GroupID uuid.UUID` WAS HERE AND IS DELETED (git-bug `7570090`, migration
	// `00069`), AND THE PAIR BELOW IS NOT ITS RENAME. `group_id` was the DELIVERY
	// TARGET — which thread the card landed on — and it was being carried up to
	// `internal/alerts/api/map.go`, where the mapper HARDCODED
	// `SubjectKind: "alert_group"` and published the group id as the `subject_id`.
	// Two different questions were riding on one field, which is precisely the
	// confusion migration 00056's `(subject_kind, subject_id)` pair exists to end.
	//
	// So this is the SUBJECT, read from the columns that store it, and the mapper
	// stops guessing. It matters for more than tidiness: `subjectOf`
	// (notification/service/notify.go) writes `subject_kind = 'alert'` with the ALERT
	// id for the four alert-scoped Reasons — `acked`, `unacked`, `refired`,
	// `rule_changed` — and `'case'` for everything else. A mapper that hardcoded one
	// kind was wrong for every row of the other, and no caller could tell.
	//
	// ⚠️ THE DELIVERY TARGET IS SIMPLY NOT IN THIS PROJECTION ANY MORE. It was not
	// re-pointed at `conversation_id`, because no consumer of `Summary` ever asked
	// where the card landed — `alerts/api` only ever used the field as a subject. If
	// a reader ever needs the target, `(conversation_kind, conversation_id)` is the
	// pair to add, and it is a different field, not this one renamed back.
	SubjectKind string
	SubjectID   uuid.UUID
	AlertID     *uuid.UUID
	CaseID      *uuid.UUID
	Reason      string
	Status      string
	// SuppressedReason is oto's OWN vocabulary and never Alertmanager's four
	// suppression reasons (§B.8.2).
	SuppressedReason string
	StateVersion     int
	CreatedAt        time.Time

	DeliveriesTotal  int
	DeliveriesSent   int
	DeliveriesFailed int
	DeliveriesDead   int
}

// DefaultHistoryLimit is the page size when a caller does not ask for one.
const DefaultHistoryLimit = 50

// maxHistoryLimit bounds a caller that asks for too much.
const maxHistoryLimit = 200

// ⛔ `n.group_id` LEFT THIS SELECT LIST AND `n.subject_kind, n.subject_id` TOOK ITS
// PLACE (git-bug `7570090`, migration `00069`). The reasoning is on `Summary` above,
// because it is about what the field MEANS and not about which column exists.
//
// ⚠️ THE SELECT LIST WAS THE ONLY PLACE THE DROPPED COLUMN APPEARED. `group_id` was
// never in the WHERE, the GROUP BY or the ORDER BY of this statement — the filter is
// `n.alert_id`, the keyset is `(n.created_at, n.id)` — so there is no second, quieter
// failure hiding behind the one the SELECT list makes obvious. Said out loud because
// a dropped column in an ORDER BY fails exactly as hard and is much easier to miss.
const listForAlertSQL = `
SELECT n.id, n.subject_kind, n.subject_id, n.alert_id, n.case_id, n.reason, n.status,
       coalesce(n.suppressed_reason,''), n.state_version, n.created_at,
       count(d.id),
       count(d.id) FILTER (WHERE d.status IN ('sent','skipped')),
       count(d.id) FILTER (WHERE d.status = 'failed'),
       count(d.id) FILTER (WHERE d.status = 'dead')
  FROM notifications n
  LEFT JOIN notification_deliveries d ON d.notification_id = n.id
 WHERE n.org_id = $1
   AND n.alert_id = $2
   AND ($3::timestamptz IS NULL OR (n.created_at, n.id) < ($3, $4))
 GROUP BY n.id
 ORDER BY n.created_at DESC, n.id DESC
 LIMIT $5`

// ListForAlert reads the notification history of one alert, newest first.
//
// It rides `notif_alert_idx (org_id, alert_id, created_at DESC)` and pages by
// KEYSET rather than OFFSET: an alert that is actively firing gains rows while
// the operator is reading, and an offset page would silently repeat or skip one.
//
// A `skipped` delivery is counted as SENT. It means the destination already
// shows exactly this content — a coalesced no-op update — and reporting it as a
// failure would make a healthy, quiet thread look broken.
func (r *NotificationRepository) ListForAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, page db.Keyset,
) ([]Summary, db.Cursor, error) {
	limit := page.Limit
	switch {
	case limit <= 0:
		limit = DefaultHistoryLimit
	case limit > maxHistoryLimit:
		limit = maxHistoryLimit
	}

	var (
		afterAt *time.Time
		afterID uuid.UUID
	)
	if !page.Cursor.IsZero() {
		at := page.Cursor.SortKey
		afterAt, afterID = &at, page.Cursor.ID
	}

	// One extra row decides HasMore without a second count query.
	rows, err := r.db(ctx).Query(ctx, listForAlertSQL,
		s.OrgID(), alertID, afterAt, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "notification_not_found", "list notifications for an alert")
	}
	defer rows.Close()

	out := make([]Summary, 0, limit)
	for rows.Next() {
		var v Summary
		if err := rows.Scan(
			&v.ID, &v.SubjectKind, &v.SubjectID, &v.AlertID, &v.CaseID, &v.Reason, &v.Status,
			&v.SuppressedReason, &v.StateVersion, &v.CreatedAt,
			&v.DeliveriesTotal, &v.DeliveriesSent, &v.DeliveriesFailed, &v.DeliveriesDead,
		); err != nil {
			return nil, db.Cursor{}, mapErr(err, "notification_not_found", "scan a notification summary")
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "notification_not_found", "read notifications for an alert")
	}

	var cursor db.Cursor
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		cursor = db.Cursor{SortKey: last.CreatedAt, ID: last.ID, HasMore: true}
	}
	return out, cursor, nil
}

// SuppressedReasonOf is the typed form of a Summary's suppression, for a caller
// that wants to branch on it rather than print it.
func (v Summary) SuppressedReasonOf() domain.SuppressedReason {
	return domain.SuppressedReason(v.SuppressedReason)
}
