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
	id          uuid.UUID
	orgID       uuid.UUID
	subjectKind string
	subjectID   uuid.UUID
	// groupID IS A POINTER SINCE MIGRATION 00058, and the pointer is the change.
	// `notifications.group_id` was NOT NULL for all eighteen signal Reasons and is
	// NULL for a digest, which spans many generations and has no single thread to
	// land in. Scanning a NULL into a bare `uuid.UUID` is a driver error, not a zero
	// value, so this field cannot stay a value type without failing every read that
	// happens to include a digest row — starting with the unfiltered audit list.
	groupID           *uuid.UUID
	alertID           *uuid.UUID
	caseID            *uuid.UUID
	reason            string
	policyID          *uuid.UUID
	stateVersion      int
	idempotencyKey    string
	status            string
	suppressedReason  *string
	digestWindowStart *time.Time
	digestCount       *int
	createdAt         time.Time
	updatedAt         time.Time
}

// scanInto is the ONE argument list for `notificationColumns`. Four queries in this
// file read the same seventeen columns, and the two new ones would otherwise have to
// be added to four hand-written Scan lists in the right position — a mistake that
// compiles and fails at run time on whichever path has no test.
func (r *notificationRow) scanInto() []any {
	return []any{
		&r.id, &r.orgID, &r.subjectKind, &r.subjectID, &r.groupID,
		&r.alertID, &r.caseID, &r.reason, &r.policyID,
		&r.stateVersion, &r.idempotencyKey, &r.status, &r.suppressedReason,
		&r.digestWindowStart, &r.digestCount, &r.createdAt, &r.updatedAt,
	}
}

func (r notificationRow) toDomain() domain.Notification {
	n := domain.Notification{
		ID:                r.id,
		OrgID:             r.orgID,
		SubjectKind:       domain.SubjectKind(r.subjectKind),
		SubjectID:         r.subjectID,
		AlertID:           r.alertID,
		CaseID:            r.caseID,
		Reason:            domain.Reason(r.reason),
		PolicyID:          r.policyID,
		StateVersion:      r.stateVersion,
		IdempotencyKey:    r.idempotencyKey,
		Status:            domain.Status(r.status),
		DigestWindowStart: r.digestWindowStart,
		DigestCount:       r.digestCount,
		CreatedAt:         r.createdAt,
		UpdatedAt:         r.updatedAt,
	}
	// The domain keeps `GroupID` a value, because eighteen of nineteen Reasons always
	// have one and forcing every reader through a pointer would be a cost paid on
	// every path to describe one. The zero UUID IS the absence, and
	// `Notification.Digest()` is how a caller asks whether to expect it.
	if r.groupID != nil {
		n.GroupID = *r.groupID
	}
	if r.suppressedReason != nil {
		n.SuppressedReason = domain.SuppressedReason(*r.suppressedReason)
	}
	return n
}

// nilGroup turns the domain's zero UUID into the SQL NULL the column now admits.
//
// ⛔ IT MUST NOT BE USED FOR THE OTHER IDS. `alert_id` and `case_id` are already
// pointers in the domain, so absence is expressible there; `group_id` is the one
// column whose absence had to be encoded, and encoding it as the zero UUID rather
// than NULL would defeat both `notifications_target_ck` and the FK to
// `alert_groups` — an id that references nothing while looking like an id.
func nilGroup(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
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
  id, org_id, subject_kind, subject_id, group_id, alert_id, case_id,
  reason, policy_id, state_version, idempotency_key, status, suppressed_reason,
  digest_window_start, digest_count, created_at, updated_at`

// ⚠️ THE ARBITER STAYS `(org_id, idempotency_key)` EVEN THOUGH A DIGEST HAS A
// SECOND UNIQUE INDEX. `notif_digest_uniq (org_id, policy_id, digest_window_start)
// WHERE subject_kind = 'digest'` (00058) is the readable spelling of the same key,
// and both are derived from the same triple — so a row that would violate one
// violates the other, and Postgres may report EITHER. `ON CONFLICT` handles only the
// arbiter it names, so a digest insert can come back as a bare 23505 on
// `notif_digest_uniq` instead of as zero rows. That is not an error either: it is
// the same "this window is already covered" answer arriving by the other index, and
// `InsertDigest` below is where it is translated. Naming both arbiters is not
// possible in one statement, and swapping the arbiter to the digest index would
// break every non-digest insert, which has no `digest_window_start` for it to key on.
const insertNotificationSQL = `
INSERT INTO notifications (
  id, org_id, subject_kind, subject_id, group_id, alert_id, case_id,
  reason, policy_id, state_version, idempotency_key, status, suppressed_reason,
  digest_window_start, digest_count, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16)
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
		n.ID, s.OrgID(), string(n.SubjectKind), n.SubjectID, nilGroup(n.GroupID),
		n.AlertID, n.CaseID, string(n.Reason), n.PolicyID, n.StateVersion,
		n.IdempotencyKey, string(n.Status), suppressed,
		n.DigestWindowStart, n.DigestCount, n.CreatedAt,
	).Scan(row.scanInto()...)
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
	err := r.db(ctx).QueryRow(ctx, selectByIdemSQL, s.OrgID(), key).Scan(row.scanInto()...)
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
	err := r.db(ctx).QueryRow(ctx, getNotificationSQL, s.OrgID(), id).Scan(row.scanInto()...)
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

// ⭐ IT KEYS ON `group_id`, THE DELIVERY TARGET, AND NOT ON THE SUBJECT.
// Migration 00056 widened `notifications_subjkind_ck` from the single value
// `alert_group` to `alert | case | alert_group`, so a subject predicate that used
// to match EVERY row now matches only the group-subject subset. The question this
// count answers is "how much has oto already said into this group's thread", and
// the answer is on `group_id`: it is NOT NULL for all eighteen Reasons, it carries
// the FK, and the thread is keyed by it whatever the subject says. Keying on the
// subject would silently exclude the per-alert facts (suppressed, unsuppressed,
// snoozed, unsnoozed, comment) and the per-case ones (acked, unacked, expired,
// refired, enriched, rule_changed) and make the throttle more permissive with
// every new subject somebody allocates.
//
// ⚠️ NO INDEX SERVES IT EXACTLY. `notifications` carries `notif_subject_idx`,
// `notif_alert_idx`, `notif_case_idx` and `notif_created_idx (org_id, created_at)`
// but nothing on `group_id` — the FK to `alert_groups` does not create one. The
// planner rides `notif_created_idx` for `org_id` plus the window and filters
// `group_id`, which is bounded by the throttle window and therefore small. The
// index to add if this ever shows up in a plan is
// `notif_group_idx (org_id, group_id, created_at DESC)`; adding it is a migration,
// deliberately not made here.
const countRecentSQL = `
SELECT count(*)
  FROM notifications
 WHERE org_id = $1
   AND group_id = $2
   AND created_at >= $3
   AND status <> 'suppressed'`

// CountRecent is the throttle's numerator: how many notifications have already
// been delivered to this group's thread inside the window.
//
// Suppressed rows are excluded. A throttle that counted its own suppressions
// would never let the group out of the window again — the cap would become a
// permanent mute, which is exactly what §B.8's bounds exist to prevent.
//
// The DELIVERY TARGET is the key, not the subject — see countRecentSQL. Contrast
// ExistsForReason below, which is genuinely a question about a SUBJECT and keeps
// the (kind, id) pair.
func (r *NotificationRepository) CountRecent(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID, since time.Time,
) (int, error) {
	var n int
	err := r.db(ctx).QueryRow(ctx, countRecentSQL, s.OrgID(), groupID, since).Scan(&n)
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
