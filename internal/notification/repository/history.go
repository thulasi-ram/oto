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
	ID           uuid.UUID
	GroupID      uuid.UUID
	AlertID      *uuid.UUID
	OccurrenceID *uuid.UUID
	Reason       string
	Status       string
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

const listForAlertSQL = `
SELECT n.id, n.group_id, n.alert_id, n.occurrence_id, n.reason, n.status,
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
			&v.ID, &v.GroupID, &v.AlertID, &v.OccurrenceID, &v.Reason, &v.Status,
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
