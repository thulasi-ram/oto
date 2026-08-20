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
	// ⛔ `groupID *uuid.UUID` WAS HERE AND IS DELETED (git-bug `7570090`, migration
	// `00069`), AND ITS LESSON IS WORTH MORE THAN THE FIELD. It was a POINTER because
	// `notifications.group_id` was NOT NULL for the signal Reasons and NULL for a
	// digest, and scanning a NULL into a bare `uuid.UUID` is a DRIVER ERROR rather
	// than a zero value — so a value type there would have failed every read that
	// happened to include a digest row, starting with the unfiltered audit list. The
	// pair below has no such asymmetry precisely because it was designed after that
	// was learned.
	//
	// The delivery-target pair (00064). Both NOT NULL, so both are value types, and
	// every row names a conversation — there is no longer one row shape with no
	// target.
	conversationKind  string
	conversationID    uuid.UUID
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
	// The SPAN (migration 00070). Nullable, and nullable for good: a digest written
	// before that migration does not know its own length, and the only way to invent
	// one is to multiply the stored start by the policy's window as it is today —
	// which is the inference these two columns exist to retire.
	digestCoveredFrom *time.Time
	digestCoveredTo   *time.Time
	createdAt         time.Time
	updatedAt         time.Time
}

// scanInto is the ONE argument list for `notificationColumns`. Five queries across
// this package read the same twenty columns, and the two migration 00070 added would
// otherwise have to be inserted into five hand-written Scan lists in the right
// position — a mistake that compiles and fails at run time on whichever path has no
// test.
func (r *notificationRow) scanInto() []any {
	return []any{
		&r.id, &r.orgID, &r.subjectKind, &r.subjectID,
		&r.conversationKind, &r.conversationID,
		&r.alertID, &r.caseID, &r.reason, &r.policyID,
		&r.stateVersion, &r.idempotencyKey, &r.status, &r.suppressedReason,
		&r.digestWindowStart, &r.digestCount,
		&r.digestCoveredFrom, &r.digestCoveredTo,
		&r.createdAt, &r.updatedAt,
	}
}

func (r notificationRow) toDomain() domain.Notification {
	n := domain.Notification{
		ID:                r.id,
		OrgID:             r.orgID,
		SubjectKind:       domain.SubjectKind(r.subjectKind),
		SubjectID:         r.subjectID,
		ConversationKind:  domain.ConversationKind(r.conversationKind),
		ConversationID:    r.conversationID,
		AlertID:           r.alertID,
		CaseID:            r.caseID,
		Reason:            domain.Reason(r.reason),
		PolicyID:          r.policyID,
		StateVersion:      r.stateVersion,
		IdempotencyKey:    r.idempotencyKey,
		Status:            domain.Status(r.status),
		DigestWindowStart: r.digestWindowStart,
		DigestCount:       r.digestCount,
		DigestCoveredFrom: r.digestCoveredFrom,
		DigestCoveredTo:   r.digestCoveredTo,
		CreatedAt:         r.createdAt,
		UpdatedAt:         r.updatedAt,
	}
	// The domain keeps `GroupID` a value, because seventeen of the eighteen Reasons
	// always have one and forcing every reader through a pointer would be a cost paid
	// on every path to describe one — `digest` is the eighteenth and the only Reason
	// without a group. The zero UUID IS the absence, and
	// `Notification.Digest()` is how a caller asks whether to expect it.
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

// ⛔ `group_id` LEFT THIS LIST (git-bug `7570090`, migration `00069`). The delivery
// target is the pair `(conversation_kind, conversation_id)`, which is what replaced it
// — 18 columns then, not 19.
//
// ⭐ AND `digest_covered_from` / `digest_covered_to` JOINED IT (git-bug `342e071`,
// migration `00070`), so it is 20. They are the SPAN the digest covered, which
// `digest_window_start` could never state on its own: a start is only a span in
// combination with the window length that was in force when it was sent, and no
// column held the length. Every reader that wanted a span had to multiply the start
// by the policy's CURRENT `digest_window_s`, which is the inference that re-reported
// a whole hour as six ten-minute digests the first time somebody narrowed a window.
const notificationColumns = `
  id, org_id, subject_kind, subject_id, conversation_kind, conversation_id,
  alert_id, case_id,
  reason, policy_id, state_version, idempotency_key, status, suppressed_reason,
  digest_window_start, digest_count, digest_covered_from, digest_covered_to,
  created_at, updated_at`

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
  id, org_id, subject_kind, subject_id, conversation_kind, conversation_id,
  alert_id, case_id,
  reason, policy_id, state_version, idempotency_key, status, suppressed_reason,
  digest_window_start, digest_count, digest_covered_from, digest_covered_to,
  created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$19)
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
		n.ID, s.OrgID(), string(n.SubjectKind), n.SubjectID,
		string(n.ConversationKind), n.ConversationID,
		n.AlertID, n.CaseID, string(n.Reason), n.PolicyID, n.StateVersion,
		n.IdempotencyKey, string(n.Status), suppressed,
		n.DigestWindowStart, n.DigestCount,
		n.DigestCoveredFrom, n.DigestCoveredTo, n.CreatedAt,
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
// the answer is on `group_id`: it is NOT NULL for all seventeen signal Reasons, it
// carries the FK, and the thread is keyed by it whatever the subject says. `digest`
// is the eighteenth Reason and the one exception — it has no group, and `$2` is never
// NULL here, so a digest row can match no group's numerator. Keying on the
// subject would silently exclude the per-alert facts (suppressed, unsuppressed,
// snoozed, unsnoozed, comment) and the per-case ones (acked, unacked, expired,
// refired, enriched, rule_changed) and make the throttle more permissive with
// every new subject somebody allocates.
//
// ⚠️ `notif_group_idx (org_id, group_id, created_at DESC)` SERVES IT, and migration
// 00056 created it in the same change for this query and for the group notification
// receipt in `snapshot.go`. Both readers used to filter `subject_kind = 'alert_group'`
// and ride `notif_subject_idx`, whose leading kind column was a constant while only
// one kind existed; widening the kind took that index away from them, and the 00011
// FK to `alert_groups` creates none on the referencing side. The trailing
// `created_at DESC` covers the window predicate below, so the throttle reads one
// index range rather than the org's whole day filtered by `group_id`.
// `notif_alert_idx`, `notif_case_idx` and `notif_created_idx (org_id, created_at)`
// answer other questions.
const countRecentSQL = `
SELECT count(*)
  FROM notifications
 WHERE org_id = $1
   AND conversation_id = $2
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
	ctx context.Context, s db.TenantScope, conversationID uuid.UUID, since time.Time,
) (int, error) {
	var n int
	err := r.db(ctx).QueryRow(ctx, countRecentSQL, s.OrgID(), conversationID, since).Scan(&n)
	if err != nil {
		return 0, mapErr(err, "notification_not_found", "count recent notifications")
	}
	return n, nil
}

// ⭐⭐ THE FLOOR'S NUMERATOR COUNTS SUBJECTS AND THE THROTTLE'S COUNTS ROWS, AND
// THE ASYMMETRY IS THE WHOLE POINT OF `subject_kinds` (migration `00072`,
// `policies_count_subject_ck`). A throttle asks "how much has oto already SAID into
// this thread", so its numerator is messages. A count condition asks "how many of
// these have HAPPENED", and it answers in CASES: `digest_floor` counts Cases because
// 00058 chose Cases in a comment, `subject_kinds` is that choice becoming a column,
// and `policies_count_case_ck` is the column being pinned to the one value the
// arithmetic below is honest for. So this count is `count(DISTINCT subject_id)` over
// that kind: five re-fires of one episode are one thing that happened five times, not
// five things, and `count(*)` here would clear a threshold of five on a single Case
// that was acked, enriched, expired and refired.
//
// ⛔ AND `subject_kind = 'alert'` IS UNREACHABLE HERE FOR A REASON THIS QUERY MAKES
// PLAIN. An alert-subject row's `subject_id` is the alert IDENTITY, one value across
// every firing, so `count(DISTINCT subject_id)` for one policy bound to `alert` counts
// OTHER alerts and never the re-firing in front of it — the permanent mute
// `policies_count_case_ck` now refuses at the door.
//
// ⭐ SUPPRESSED ROWS ARE COUNTED, WHICH IS THE OPPOSITE OF `countRecentSQL` AND IS
// FORCED RATHER THAN CHOSEN. The throttle excludes them so a cap cannot count its
// own suppressions and become a permanent mute. Read the same exclusion into a
// FLOOR and the failure is the same one from the other side: every fact below the
// threshold is suppressed BY the threshold, so an excluded-suppressions numerator
// would sit at zero forever and the policy would never speak at all — the
// permanent mute again, arrived at by symmetry. A suppressed row is oto's record
// that the fact HAPPENED, which is exactly the question this count asks; whether
// oto spoke about it is a different question and belongs to the throttle.
//
// ⚠️ AND IT IS SCOPED TO ONE POLICY, NOT TO THE CONVERSATION, BECAUSE THE
// CONVERSATION IS USUALLY THE THING BEING COUNTED. "PodRestart opened 5 Cases in
// an hour" (git-bug `7570090`) is five Cases and therefore five conversations, so a
// per-conversation numerator would read 1 on each of them and the condition could
// never be met by the facts it exists to describe. `policy_id` is what makes the
// count "a policy's floor on its own recent history" (`domain.CountOverWindow`),
// and it is NOT NULL on every row a routed evaluation writes — including the ones
// this very floor suppressed, which is what lets the count climb.
//
// ⚠️ THE SUBJECT BEING EVALUATED IS EXCLUDED HERE AND ADDED BACK IN GO. It has no
// row yet — the gate runs before `Insert` — and it may equally have several rows
// already from earlier facts about the same episode. `<> $4` makes those two cases
// one case, so the caller's `+ 1` is exact rather than approximately right: the
// counted set is "distinct other subjects in the window, plus this one". That is
// also what makes `MinCountThreshold = 2` honest — a threshold of 1 is cleared by
// every fact unconditionally, which is why the column refuses it.
//
// ⚠️ THE WINDOW IS CLOSED AT BOTH ENDS, AND THE UPPER BOUND IS NOT DECORATION. It
// used to be `created_at >= $5` alone, which is `[TakenAt - W, ∞)` — while this
// comment and `domain.CountOverWindow` both promise `[TakenAt - W, TakenAt]`. The
// evaluator's `TakenAt` is the SNAPSHOT instant, not `now`, so on a retried
// `notify.evaluate` (or any queue lag at all) every row written between the snapshot
// and the retry was counted: a floor that admits more facts each time it is retried
// clears itself by being retried, and two attempts at the same fact could reach two
// different verdicts. Both ends now come from the snapshot, so the numerator is a
// function of the fact rather than of when the worker got round to it.
//
// It rides `notif_policy_idx (org_id, policy_id, created_at DESC)` (migration
// 00073): two equalities and a bounded range on `created_at`, so the window stops
// the scan instead of the org's day being read and filtered.
const countPolicySubjectsSQL = `
SELECT count(DISTINCT subject_id)
  FROM notifications
 WHERE org_id = $1
   AND policy_id = $2
   AND subject_kind = $3
   AND subject_id <> $4
   AND created_at >= $5
   AND created_at <= $6`

// CountRecentSubjects is the count condition's numerator: how many DISTINCT
// subjects of one kind this policy has recorded a fact about inside the sliding
// window, not counting `excluding`.
//
// `excluding` is the subject of the fact being evaluated, which the caller adds
// back as one — see countPolicySubjectsSQL for why the arithmetic is split that
// way, and `domain.CountOverWindow` for the window's `[TakenAt - W, TakenAt]` shape.
// `since` and `until` are BOTH derived from the snapshot instant, which is what makes
// a retry count what the first attempt counted.
func (r *NotificationRepository) CountRecentSubjects(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID,
	kind domain.SubjectKind, excluding uuid.UUID, since, until time.Time,
) (int, error) {
	var n int
	err := r.db(ctx).QueryRow(ctx, countPolicySubjectsSQL,
		s.OrgID(), policyID, string(kind), excluding, since, until).Scan(&n)
	if err != nil {
		return 0, mapErr(err, "notification_not_found", "count recent notification subjects")
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
