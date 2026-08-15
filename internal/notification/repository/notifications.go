package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// notificationRow is the row model of `notifications`.
type notificationRow struct {
	id               uuid.UUID
	orgID            uuid.UUID
	subjectKind      string
	subjectID        uuid.UUID
	groupID          uuid.UUID
	alertID          *uuid.UUID
	occurrenceID     *uuid.UUID
	reason           string
	policyID         *uuid.UUID
	stateVersion     int
	idempotencyKey   string
	status           string
	suppressedReason *string
	createdAt        time.Time
	updatedAt        time.Time
}

func (r notificationRow) toDomain() domain.Notification {
	n := domain.Notification{
		ID:             r.id,
		OrgID:          r.orgID,
		SubjectKind:    domain.SubjectKind(r.subjectKind),
		SubjectID:      r.subjectID,
		GroupID:        r.groupID,
		AlertID:        r.alertID,
		OccurrenceID:   r.occurrenceID,
		Reason:         domain.Reason(r.reason),
		PolicyID:       r.policyID,
		StateVersion:   r.stateVersion,
		IdempotencyKey: r.idempotencyKey,
		Status:         domain.Status(r.status),
		CreatedAt:      r.createdAt,
		UpdatedAt:      r.updatedAt,
	}
	if r.suppressedReason != nil {
		n.SuppressedReason = domain.SuppressedReason(*r.suppressedReason)
	}
	return n
}

// NotificationRepository is the SQL over `notifications`.
type NotificationRepository struct {
	q db.Querier
}

// NewNotificationRepository builds the repository over a fallback querier.
func NewNotificationRepository(q db.Querier) *NotificationRepository {
	return &NotificationRepository{q: q}
}

func (r *NotificationRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const notificationColumns = `
  id, org_id, subject_kind, subject_id, group_id, alert_id, occurrence_id,
  reason, policy_id, state_version, idempotency_key, status, suppressed_reason,
  created_at, updated_at`

const insertNotificationSQL = `
INSERT INTO notifications (
  id, org_id, subject_kind, subject_id, group_id, alert_id, occurrence_id,
  reason, policy_id, state_version, idempotency_key, status, suppressed_reason,
  created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14)
ON CONFLICT (org_id, idempotency_key) DO NOTHING
RETURNING` + notificationColumns

const selectByIdemSQL = `
SELECT` + notificationColumns + `
  FROM notifications
 WHERE org_id = $1 AND idempotency_key = $2`

// Insert records one notification INTENT, idempotently.
//
// The second result reports whether this call created the row. `false` means a
// notification with this §C.7 key already exists — which is the idempotency
// mechanism WORKING, not a failure (§L.9). The caller must treat it as success
// and must NOT fan out again: the previous run already did, or is doing so.
//
// The insert and the select are two statements rather than an upsert because
// `ON CONFLICT DO UPDATE` would touch `updated_at` on a redelivery and make an
// at-least-once job look like a state change every time it ran.
func (r *NotificationRepository) Insert(
	ctx context.Context, s db.TenantScope, n domain.Notification,
) (domain.Notification, bool, error) {
	if n.ID == uuid.Nil {
		return domain.Notification{}, false, mapErr(
			errors.New("notification id is required"), "notification_not_found", "insert notification")
	}

	var suppressed *string
	if n.SuppressedReason != "" {
		v := string(n.SuppressedReason)
		suppressed = &v
	}

	var row notificationRow
	err := r.db(ctx).QueryRow(ctx, insertNotificationSQL,
		n.ID, s.OrgID(), string(n.SubjectKind), n.SubjectID, n.GroupID,
		n.AlertID, n.OccurrenceID, string(n.Reason), n.PolicyID, n.StateVersion,
		n.IdempotencyKey, string(n.Status), suppressed, n.CreatedAt,
	).Scan(
		&row.id, &row.orgID, &row.subjectKind, &row.subjectID, &row.groupID,
		&row.alertID, &row.occurrenceID, &row.reason, &row.policyID,
		&row.stateVersion, &row.idempotencyKey, &row.status, &row.suppressedReason,
		&row.createdAt, &row.updatedAt,
	)
	switch {
	case err == nil:
		return row.toDomain(), true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return domain.Notification{}, false, mapErr(err, "notification_not_found", "insert notification")
	}

	existing, err := r.GetByIdempotencyKey(ctx, s, n.IdempotencyKey)
	if err != nil {
		return domain.Notification{}, false, err
	}
	return existing, false, nil
}

// GetByIdempotencyKey reads the notification that owns a §C.7 key.
func (r *NotificationRepository) GetByIdempotencyKey(
	ctx context.Context, s db.TenantScope, key string,
) (domain.Notification, error) {
	var row notificationRow
	err := r.db(ctx).QueryRow(ctx, selectByIdemSQL, s.OrgID(), key).Scan(
		&row.id, &row.orgID, &row.subjectKind, &row.subjectID, &row.groupID,
		&row.alertID, &row.occurrenceID, &row.reason, &row.policyID,
		&row.stateVersion, &row.idempotencyKey, &row.status, &row.suppressedReason,
		&row.createdAt, &row.updatedAt,
	)
	if err != nil {
		return domain.Notification{}, mapErr(err, "notification_not_found", "notification")
	}
	return row.toDomain(), nil
}

const getNotificationSQL = `
SELECT` + notificationColumns + `
  FROM notifications
 WHERE org_id = $1 AND id = $2`

// Get reads one notification by id.
func (r *NotificationRepository) Get(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) (domain.Notification, error) {
	var row notificationRow
	err := r.db(ctx).QueryRow(ctx, getNotificationSQL, s.OrgID(), id).Scan(
		&row.id, &row.orgID, &row.subjectKind, &row.subjectID, &row.groupID,
		&row.alertID, &row.occurrenceID, &row.reason, &row.policyID,
		&row.stateVersion, &row.idempotencyKey, &row.status, &row.suppressedReason,
		&row.createdAt, &row.updatedAt,
	)
	if err != nil {
		return domain.Notification{}, mapErr(err, "notification_not_found", "notification")
	}
	return row.toDomain(), nil
}

// ⭐ GREATEST KEEPS `updated_at` MONOTONIC, and that is a correctness guard, not
// a nicety. Both timestamps come from the application — Insert above names them
// from the caller's clock — but "the application" is N pods with N clocks, and
// the aggregate status is folded by a DISPATCH worker, never by the pod that
// recorded the intent. A few milliseconds of lag between them would otherwise
// write an `updated_at` BELOW `created_at` and fail `notifications_time_ck` with
// a 23514. That is the worst place in the module to take it: the delivery has
// already gone OUT and only the bookkeeping fails, so the job retries, sends
// again, and a human gets a duplicate at 3am for a clock reason.
const setNotificationStatusSQL = `
UPDATE notifications
   SET status = $3, updated_at = GREATEST(updated_at, $4)
 WHERE org_id = $1 AND id = $2 AND status IS DISTINCT FROM $3`

// SetStatus updates the aggregate status of one notification.
//
// It deliberately cannot set `suppressed`: notifications_supp_ck requires a
// suppressed row to carry a reason, and a status setter with no reason argument
// would be a way to lose one. Suppression is recorded at insert time, once,
// with its reason, or not at all.
func (r *NotificationRepository) SetStatus(
	ctx context.Context, s db.TenantScope, id uuid.UUID, status domain.Status, now time.Time,
) error {
	if status == domain.StatusSuppressed {
		return mapErr(errors.New("suppression must be recorded with its reason"),
			"notification_not_found", "update notification status")
	}
	_, err := r.db(ctx).Exec(ctx, setNotificationStatusSQL, s.OrgID(), id, string(status), now)
	return mapErr(err, "notification_not_found", "update notification status")
}

const countRecentSQL = `
SELECT count(*)
  FROM notifications
 WHERE org_id = $1
   AND subject_kind = $2
   AND subject_id = $3
   AND created_at >= $4
   AND status <> 'suppressed'`

// CountRecent is the throttle's numerator: how many notifications this subject
// has already produced inside the window.
//
// Suppressed rows are excluded. A throttle that counted its own suppressions
// would never let the subject out of the window again — the cap would become a
// permanent mute, which is exactly what §B.8's bounds exist to prevent.
func (r *NotificationRepository) CountRecent(
	ctx context.Context, s db.TenantScope,
	kind domain.SubjectKind, subjectID uuid.UUID, since time.Time,
) (int, error) {
	var n int
	err := r.db(ctx).QueryRow(ctx, countRecentSQL, s.OrgID(), string(kind), subjectID, since).Scan(&n)
	if err != nil {
		return 0, mapErr(err, "notification_not_found", "count recent notifications")
	}
	return n, nil
}

const existsReasonSQL = `
SELECT EXISTS (
  SELECT 1 FROM notifications
   WHERE org_id = $1 AND subject_kind = $2 AND subject_id = $3 AND reason = $4)`

// ExistsForReason reports whether this subject has EVER produced a notification
// with this reason.
//
// It is what makes the unacked reminder fire AT MOST ONCE PER GROUP GENERATION
// (§G.9). The §C.7 idempotency key cannot do it alone: the key includes
// `state_version`, so a group that changes state after a reminder would mint a
// second, perfectly valid key — and a reminder that repeats on every state
// change is a ladder built by accident.
func (r *NotificationRepository) ExistsForReason(
	ctx context.Context, s db.TenantScope,
	kind domain.SubjectKind, subjectID uuid.UUID, reason domain.Reason,
) (bool, error) {
	var found bool
	err := r.db(ctx).QueryRow(ctx, existsReasonSQL,
		s.OrgID(), string(kind), subjectID, string(reason)).Scan(&found)
	if err != nil {
		return false, mapErr(err, "notification_not_found", "check notification history")
	}
	return found, nil
}
