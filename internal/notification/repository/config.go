package repository

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// ⚠️ WHY THIS IS A SEPARATE TYPE FROM PolicyRepository.
//
// `PolicyRepository` is deliberately READ-ONLY: "a policy is configuration,
// written by the settings API; the notification module's job is to obey it, and
// giving the evaluator a write path would be handing the thing that reads the
// rules a way to change them." That reasoning is exactly right, and it is why the
// write path is a DIFFERENT type rather than four more methods on that one.
//
// It is the settings API's half of this module: policy CRUD, and the two
// filtered audit lists (`GET /notifications`, `GET /deliveries`) that no worker
// ever runs.

// ConfigRepository is the SQL the settings API needs over `notification_policies`,
// `notifications` and `notification_deliveries`.
//
// Nothing in `notification/service` is given this type, so the evaluator still
// cannot rewrite the rules it is evaluating.
type ConfigRepository struct {
	q     db.Querier
	clock clock.Clock
}

// NewConfigRepository builds the settings-side repository over a fallback
// querier, normally the general pool.
func NewConfigRepository(q db.Querier, clk clock.Clock) *ConfigRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &ConfigRepository{q: q, clock: clk}
}

func (r *ConfigRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// clampPageLimit applies the §E.1 page bounds.
func clampPageLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultHistoryLimit
	case n > maxHistoryLimit:
		return maxHistoryLimit
	default:
		return n
	}
}

// ------------------------------------------------------------------ policies

// scanPolicy reads one `policyColumns` row.
//
// ⛔ IT DELEGATES TO `policyRow.scanInto` AND MUST KEEP DOING SO. It used to spell
// its own argument list, which is exactly the drift that list was introduced to
// end: `policyColumns` gained `digest_window_s` and `digest_floor` in migration
// 00058 and this copy did not, so every policy read through the CONFIG repository
// — the settings list, the settings detail, the row returned from a create and a
// patch — was scanning fifteen columns into thirteen destinations. That does not
// fail to compile; it fails at runtime, on the settings screen only, because pgx
// was being handed a timestamp for an `INT`.
func scanPolicy(scan func(...any) error) (domain.Policy, error) {
	var row policyRow
	if err := scan(row.scanInto()...); err != nil {
		return domain.Policy{}, err
	}
	return row.toDomain()
}

const listPoliciesPageSQL = `
SELECT` + policyColumns + `
  FROM notification_policies
 WHERE org_id = $1
   AND deleted_at IS NULL
   AND ($2::int IS NULL OR (priority, id) > ($2, $3))
 ORDER BY priority ASC, id ASC
 LIMIT $4`

// ListPolicies returns a keyset page of live policies IN EVALUATION ORDER —
// priority ascending, lower first.
//
// The page is ordered by the same key evaluation uses, not by creation time, so
// the settings screen reads top-to-bottom in the order a fact actually walks. A
// list that showed policies in a different order from the one they fire in is a
// list that makes "why did this go to #general?" unanswerable.
//
// The cursor therefore keys on `(priority, id)` rather than on a timestamp. It
// still travels through the standard db.Cursor: SortKey is unused and ID carries
// the tiebreak, with the priority packed alongside — see the encode/decode pair
// below, which keeps that detail inside this file.
func (r *ConfigRepository) ListPolicies(
	ctx context.Context, s db.TenantScope, p db.Keyset,
) ([]domain.Policy, db.Cursor, error) {
	limit := clampPageLimit(p.Limit)

	var (
		afterPriority *int
		afterID       uuid.UUID
	)
	if !p.Cursor.IsZero() {
		// SortKey holds the priority as a Unix second, which is the one field the
		// generic cursor gives us. It is an integer either way and the round trip
		// is exact for the 0..10000 range policies_prio_ck allows.
		v := int(p.Cursor.SortKey.Unix())
		afterPriority, afterID = &v, p.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, listPoliciesPageSQL, s.OrgID(), afterPriority, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "policy_not_found", "list notification policies")
	}
	defer rows.Close()

	out := make([]domain.Policy, 0, limit+1)
	for rows.Next() {
		p, err := scanPolicy(rows.Scan)
		if err != nil {
			return nil, db.Cursor{}, mapErr(err, "policy_not_found", "scan notification policy")
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "policy_not_found", "read notification policies")
	}

	cursor := db.Cursor{Hash: p.Cursor.Hash}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		cursor = db.Cursor{
			SortKey: time.Unix(int64(last.Priority), 0).UTC(),
			ID:      last.ID,
			Hash:    p.Cursor.Hash,
			HasMore: true,
		}
	}
	return out, cursor, nil
}

const getPolicyLiveSQL = `
SELECT` + policyColumns + `
  FROM notification_policies WHERE org_id = $1 AND id = $2`

// GetPolicy reads one policy, deleted or not.
//
// A deleted policy is still readable because `notifications.policy_id` points at
// it until the row is purged: the audit trail of WHY something was sent has to
// survive the policy that caused it.
func (r *ConfigRepository) GetPolicy(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID,
) (domain.Policy, error) {
	p, err := scanPolicy(r.db(ctx).QueryRow(ctx, getPolicyLiveSQL, s.OrgID(), policyID).Scan)
	if err != nil {
		return domain.Policy{}, mapErr(err, "policy_not_found", "notification policy")
	}
	return p, nil
}

const insertPolicySQL = `
INSERT INTO notification_policies (
  id, org_id, name, priority, enabled, matchers, reasons, channel_ids,
  throttle, subject_kinds, digest_window_s, digest_floor,
  count_min, count_window_s,
  template_id,
  created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16)
RETURNING id`

// CreatePolicy writes one routing rule.
//
// The domain's own Validate has already run in the service layer; this method
// re-proves nothing except that the row is well formed enough to reach the
// driver. The CHECK constraints are the backstop, never the error message.
func (r *ConfigRepository) CreatePolicy(
	ctx context.Context, s db.TenantScope, in domain.PolicyDraft,
) (domain.Policy, error) {
	if strings.TrimSpace(in.Name) == "" {
		return domain.Policy{}, errs.Internal("policy_name_missing",
			errsMissing("a policy name is required"))
	}

	matchers, err := encodeMatchers(in.Matchers)
	if err != nil {
		return domain.Policy{}, err
	}
	throttle, err := encodeThrottle(in.Throttle)
	if err != nil {
		return domain.Policy{}, err
	}

	priority := domain.DefaultPolicyPriority
	if in.Priority != nil {
		priority = *in.Priority
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	reasons := make([]string, 0, len(in.Reasons))
	for _, k := range in.Reasons {
		reasons = append(reasons, string(k))
	}
	// ⚠️ THE EMPTY SLICE IS ALLOCATED HERE AND NOT LEFT NIL, which is the opposite of
	// what `policyRow.toDomain` does on the way back and is correct in both
	// directions. `subject_kinds` is `NOT NULL DEFAULT '{}'`, and pgx encodes a nil
	// `[]string` as SQL NULL — so a draft that names no binding would violate the NOT
	// NULL rather than take the default. On the read side nil and empty are the same
	// domain value; on the write side only one of them is a legal parameter.
	subjectKinds := make([]string, 0, len(in.Subjects))
	for _, k := range in.Subjects {
		subjectKinds = append(subjectKinds, string(k))
	}

	// A supplied id is the caller naming the row before it exists, which is what
	// lets `notification/service` record it in an `Idempotency-Key` claim taken in
	// this same transaction. Zero still mints one.
	newID := in.ID
	if newID == uuid.Nil {
		newID = id.New()
	}

	var stored uuid.UUID
	err = r.db(ctx).QueryRow(ctx, insertPolicySQL,
		newID, s.OrgID(), in.Name, priority, enabled, matchers, reasons,
		in.ChannelIDs, throttle, subjectKinds,
		// NULL is "no digest", which is the default and the state of every row
		// written before migration 00058. Nothing is defaulted here: a caller that
		// asked for no window must not acquire one from the repository.
		secondsPtr(in.DigestWindow), in.DigestFloor,
		// The same rule for the count condition (migration 00072): NULL on both is
		// "no condition", and a caller that asked for none must not acquire one.
		in.CountMin, secondsPtr(in.CountWindow),
		in.TemplateID,
		r.clock.Now().UTC(),
	).Scan(&stored)
	if err != nil {
		return domain.Policy{}, mapErr(err, "policy_not_found", "create a notification policy")
	}
	return r.GetPolicy(ctx, s, stored)
}

// ⭐ GREATEST KEEPS `updated_at` MONOTONIC, and that is a correctness guard, not
// a nicety. Both timestamps come from the application — CreatePolicy above names
// them from the injected clock — but "the application" is N pods with N clocks,
// and the pod serving a policy PATCH is rarely the pod that created the policy.
// A few milliseconds of lag between them would otherwise write an `updated_at`
// BELOW `created_at` and fail `policies_time_ck` with a 23514 — a 500 on an
// ordinary routing edit, with nothing wrong. GREATEST makes the check
// unfalsifiable while leaving the value app-owned; it is the same idiom, for the
// same reason, as `channels`, `orgs` and OrderingStore.Advance.
const updatePolicySQL = `
UPDATE notification_policies SET
    name        = COALESCE($3, name),
    priority    = COALESCE($4, priority),
    enabled     = COALESCE($5, enabled),
    matchers    = COALESCE($6, matchers),
    reasons     = COALESCE($7, reasons),
    channel_ids = COALESCE($8, channel_ids),
    throttle    = CASE WHEN $9  THEN $10 ELSE throttle END,
    digest_window_s = CASE WHEN $11 THEN $12 ELSE digest_window_s END,
    digest_floor    = CASE WHEN $13 THEN $14 ELSE digest_floor END,
    subject_kinds   = COALESCE($15, subject_kinds),
    count_min       = CASE WHEN $16 THEN $17 ELSE count_min END,
    count_window_s  = CASE WHEN $18 THEN $19 ELSE count_window_s END,
    -- A CASE and not a COALESCE, for the reason the five above use one: clearing
    -- the template is a real operation — it is how an operator puts a policy back
    -- on oto's built-in card — and COALESCE cannot express "set this to NULL".
    template_id     = CASE WHEN $20 THEN $21 ELSE template_id END,
    updated_at  = GREATEST(updated_at, $22)
 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
RETURNING id`

// UpdatePolicy applies a partial change.
//
// `throttle`, `digest_window_s`, `digest_floor`, `count_min` and `count_window_s`
// are written through a CASE rather than a COALESCE because all five are nullable
// and clearing them is a real operation: COALESCE cannot express "set this to
// NULL", and an operator turning a throttle, a digest or a count condition off
// must be able to say so.
//
// ⭐ `subject_kinds` IS THE ONE THAT GOES BACK TO COALESCE, and it is not an
// inconsistency — it is the column being `NOT NULL DEFAULT '{}'`. There is no NULL
// to set, so COALESCE's one limitation does not bite: a nil parameter means "leave
// it alone" and an EMPTY ARRAY means "claim every altitude", which is a real
// instruction the CASE machinery is not needed to express. Reaching for the CASE
// here anyway would add a boolean nothing reads.
//
// ⭐ THE TWO DIGEST COLUMNS MOVE IN ONE STATEMENT, which is what keeps
// `policies_digest_pair_ck` satisfiable. Clearing the window and clearing the
// floor arrive as two independent CASEs of the same UPDATE, so a patch that turns
// the whole digest off never passes through the momentary "floor without window"
// the constraint refuses. ⭐ THE SAME PROPERTY IS LOAD-BEARING FOR THE COUNT
// CONDITION AND MORE SO, because `policies_count_pair_ck` is SYMMETRIC: turning a
// condition off means clearing both halves, and either half alone is a violation.
// Two CASEs in one statement is the only shape in which "clear the whole
// condition" is a single legal transition.
func (r *ConfigRepository) UpdatePolicy(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID, p domain.PolicyPatch,
) (domain.Policy, error) {
	if p.IsEmpty() {
		return domain.Policy{}, errs.Validation("empty_patch", "supply at least one field to change")
	}

	var (
		matchers    *[]byte
		reasons     *[]string
		channels    *[]uuid.UUID
		setThrottle bool
		throttleVal []byte
		setWindow   bool
		windowVal   *int
		setFloor    bool
		floorVal    *int
		subjects    *[]string
		setCountMin bool
		countMinVal *int
		setCountWin bool
		countWinVal *int
		setTemplate bool
		templateVal *uuid.UUID
	)
	if p.Matchers != nil {
		b, err := encodeMatchers(*p.Matchers)
		if err != nil {
			return domain.Policy{}, err
		}
		matchers = &b
	}
	if p.Reasons != nil {
		out := make([]string, 0, len(*p.Reasons))
		for _, k := range *p.Reasons {
			out = append(out, string(k))
		}
		reasons = &out
	}
	if p.ChannelIDs != nil {
		channels = p.ChannelIDs
	}
	if p.Throttle != nil {
		setThrottle = true
		b, err := encodeThrottle(*p.Throttle)
		if err != nil {
			return domain.Policy{}, err
		}
		throttleVal = b
	}
	if p.DigestWindow != nil {
		setWindow = true
		windowVal = secondsPtr(*p.DigestWindow)
	}
	if p.DigestFloor != nil {
		setFloor = true
		floorVal = *p.DigestFloor
	}
	if p.Subjects != nil {
		// Allocated even when the binding is empty, for the reason CreatePolicy
		// allocates it: pgx encodes a nil `[]string` as SQL NULL, and NULL is the one
		// value this NOT NULL column cannot take. An empty array is `{}`, which is
		// "claim every altitude" — the instruction an operator sends to remove a
		// binding.
		out := make([]string, 0, len(*p.Subjects))
		for _, k := range *p.Subjects {
			out = append(out, string(k))
		}
		subjects = &out
	}
	if p.CountMin != nil {
		setCountMin = true
		countMinVal = *p.CountMin
	}
	if p.CountWindow != nil {
		setCountWin = true
		countWinVal = secondsPtr(*p.CountWindow)
	}
	if p.TemplateID != nil {
		setTemplate = true
		templateVal = *p.TemplateID
	}

	var stored uuid.UUID
	err := r.db(ctx).QueryRow(ctx, updatePolicySQL,
		s.OrgID(), policyID, p.Name, p.Priority, p.Enabled, matchers, reasons, channels,
		setThrottle, throttleVal,
		setWindow, windowVal, setFloor, floorVal,
		subjects,
		setCountMin, countMinVal, setCountWin, countWinVal,
		setTemplate, templateVal,
		r.clock.Now().UTC(),
	).Scan(&stored)
	if err != nil {
		return domain.Policy{}, mapErr(err, "policy_not_found", "notification policy")
	}
	return r.GetPolicy(ctx, s, stored)
}

// `deleted_at` records the caller's instant exactly; `updated_at` is monotonic
// for the reason given on updatePolicySQL.
const softDeletePolicySQL = `
UPDATE notification_policies
   SET deleted_at = $3, enabled = false, updated_at = GREATEST(updated_at, $3)
 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`

// SoftDeletePolicy stops future matching.
//
// ⛔ IT IS A SOFT DELETE AND MUST STAY ONE. `notifications.policy_id` is ON
// DELETE SET NULL, so a hard delete would silently erase the answer to "why was
// this sent?" on every notification the policy ever caused.
func (r *ConfigRepository) SoftDeletePolicy(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID,
) error {
	tag, err := r.db(ctx).Exec(ctx, softDeletePolicySQL, s.OrgID(), policyID, r.clock.Now().UTC())
	if err != nil {
		return mapErr(err, "policy_not_found", "delete a notification policy")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("policy_not_found", "no such notification policy")
	}
	return nil
}

// ------------------------------------------------------------- notifications

// ⛔ `group_id = $5` IS NOW `conversation_id = $5`, AND THIS IS A RE-POINT AND NOT A
// DELETION (git-bug `7570090`, migration `00069`).
//
// ⭐ THE QUESTION IT ASKS IS A DELIVERY-TARGET QUESTION, which is why it survives at
// all. `?group_id=…` on the audit meant "show me every intent that landed on THIS
// thread" — not "every intent ABOUT this thing" — and the delivery target did not
// disappear with `alert_groups`, it changed spelling to the pair
// `(conversation_kind, conversation_id)` (migration 00064). The kind is not tested
// here because the id alone is discriminating: `conversation_id` holds an
// `alert_cases.id` for a case conversation and a `notification_policies.id` for a
// digest, and a caller passing one cannot accidentally match the other. It rides
// `notif_conversation_idx (org_id, conversation_id, created_at DESC)`, which 00069
// created for exactly this shape of read.
//
// ⚠️ AND IT IS RE-POINTED RATHER THAN DELETED FOR A REASON THAT IS ABOUT SAFETY, NOT
// TASTE. `00069`'s header says the three `group_id` readers were "answered rather
// than re-pointed", but the API layer still PARSES `group_id`
// (`notification/api/audit.go:93`) and still hashes it into the keyset filter hash
// (`:400`). Deleting the predicate while the parameter is still bound would make
// `?group_id=X` return the WHOLE unfiltered page instead of an error or an empty
// one — a SILENT WIDENING of an audit list, which is a worse failure than a 42703
// because nothing anywhere says it happened. The precedent is
// `drill/repository/artifacts.go`: its `readGroup` was deleted outright, but
// `readNotification` — the read that asked where the card went — was re-pointed at
// `(conversation_kind, conversation_id)` exactly like this one.
//
// ⛔ THE PARAMETER IS STILL SPELLED `group_id` ONE LAYER UP AND THAT IS A REAL,
// KNOWN GAP. `domain.NotificationFilter.GroupID` and the `group_id` query parameter
// both need renaming to `conversation_id`, and the openapi parameter with them.
// Neither is in this package, so neither is done here — but an operator passing a
// pre-migration group id now gets an empty page, which is the honest answer (no
// conversation has that id any more) rather than a wrong one.
const listNotificationsSQL = `
SELECT` + notificationColumns + `
  FROM notifications
 WHERE org_id = $1
   AND ($2::text[] IS NULL OR status = ANY($2))
   AND ($3::text[] IS NULL OR reason = ANY($3))
   AND ($4::text[] IS NULL OR suppressed_reason = ANY($4))
   AND ($5::uuid IS NULL OR conversation_id = $5)
   AND ($6::uuid IS NULL OR alert_id = $6)
   AND ($7::uuid IS NULL OR policy_id = $7)
   AND ($8::timestamptz IS NULL OR created_at >= $8)
   AND ($9::timestamptz IS NULL OR created_at <= $9)
   AND ($10::timestamptz IS NULL OR (created_at, id) < ($10, $11))
 ORDER BY created_at DESC, id DESC
 LIMIT $12`

// ListNotifications audits the intents oto formed, newest first.
//
// ⛔ SUPPRESSED ROWS ARE IN THIS LIST AND ARE NEVER FILTERED OUT BY DEFAULT.
// Suppression is always recorded: damping that cannot be inspected is
// indistinguishable from a bug, and silence that cannot be explained destroys
// trust (§B.6).
func (r *ConfigRepository) ListNotifications(
	ctx context.Context, s db.TenantScope, f domain.NotificationFilter, p db.Keyset,
) ([]domain.Notification, db.Cursor, error) {
	limit := clampPageLimit(p.Limit)

	statuses := stringsOrNil(f.Statuses, func(v domain.Status) string { return string(v) })
	reasons := stringsOrNil(f.Reasons, func(v domain.Reason) string { return string(v) })
	suppressed := stringsOrNil(f.SuppressedReasons, func(v domain.SuppressedReason) string { return string(v) })
	// `suppressed_reason` IMPLIES `status = suppressed`: the constraint
	// notifications_supp_ck already guarantees the pairing, so narrowing status
	// here costs nothing and makes the intent explicit for the planner.
	if suppressed != nil && statuses == nil {
		statuses = []string{string(domain.StatusSuppressed)}
	}

	var (
		afterAt *time.Time
		afterID uuid.UUID
	)
	if !p.Cursor.IsZero() {
		at := p.Cursor.SortKey.UTC()
		afterAt, afterID = &at, p.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, listNotificationsSQL,
		s.OrgID(), statuses, reasons, suppressed,
		uuidOrNil(f.GroupID), uuidOrNil(f.AlertID), uuidOrNil(f.PolicyID),
		timeOrNil(f.Since), timeOrNil(f.Until), afterAt, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "notification_not_found", "list notifications")
	}
	defer rows.Close()

	out := make([]domain.Notification, 0, limit+1)
	for rows.Next() {
		var row notificationRow
		// ⛔ THIS WAS "THE ONE READ THAT MEETS A NULL `group_id` FIRST", AND THERE IS
		// NO SUCH NULL LEFT (git-bug `7570090`, migration `00069`). The note explained
		// why `notificationRow.groupID` had to be a `*uuid.UUID`: the audit list has no
		// mandatory filter, so a digest row (migration 00058) — the one row shape with
		// no group — arrived here on any unfiltered page, and scanning a NULL into a
		// bare `uuid.UUID` is a driver error rather than a zero value.
		//
		// ⭐ THE LESSON OUTLIVED THE COLUMN and is recorded on `notificationRow` itself.
		// `(conversation_kind, conversation_id)` is NOT NULL for every row including a
		// digest, so both halves are value types and this read has no asymmetry left to
		// be the first to meet. The argument list is still shared rather than retyped
		// per query, and that reason is unchanged: four statements read the same
		// columns and four hand-written Scan lists drift.
		if err := rows.Scan(row.scanInto()...); err != nil {
			return nil, db.Cursor{}, mapErr(err, "notification_not_found", "scan a notification")
		}
		out = append(out, row.toDomain())
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "notification_not_found", "read notifications")
	}

	cursor := db.Cursor{Hash: p.Cursor.Hash}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		cursor = db.Cursor{SortKey: last.CreatedAt.UTC(), ID: last.ID, Hash: p.Cursor.Hash, HasMore: true}
	}
	return out, cursor, nil
}

// -------------------------------------------------------------- deliveries

const listDeliveriesSQL = `
SELECT d.id, d.org_id, d.notification_id, d.channel_id, d.thread_id, d.thread_seq,
       d.mode, d.status, d.attempts, d.next_attempt_at, d.rendered, d.rendered_hash,
       d.rendered_fallback, d.provider_message_id, d.provider_conversation_id,
       d.provider_response, d.error, d.error_class, d.ambiguous, d.sent_at,
       d.created_at, d.updated_at,
       c.name::text, c.type
  FROM notification_deliveries d
  LEFT JOIN channels c ON c.id = d.channel_id AND c.org_id = d.org_id
 WHERE d.org_id = $1
   AND ($2::text[] IS NULL OR d.status = ANY($2))
   AND ($3::text[] IS NULL OR d.error_class = ANY($3))
   AND ($4::text[] IS NULL OR d.mode = ANY($4))
   AND ($5::uuid IS NULL OR d.channel_id = $5)
   AND ($6::uuid IS NULL OR d.notification_id = $6)
   AND ($7::boolean IS NULL OR d.ambiguous = $7)
   AND ($8::timestamptz IS NULL OR d.created_at >= $8)
   AND ($9::timestamptz IS NULL OR d.created_at <= $9)
   AND ($10::timestamptz IS NULL OR (d.created_at, d.id) < ($10, $11))
 ORDER BY d.created_at DESC, d.id DESC
 LIMIT $12`

// ListDeliveries is the operator's view of whether messages actually landed.
//
// The channel name and type are joined in because a page of `channel_id` UUIDs is
// unreadable, and this screen exists to be read: `status=dead` finds the channels
// that have stopped working, `ambiguous=true` finds the messages oto re-sent
// after a crash and deliberately labelled as possible duplicates.
func (r *ConfigRepository) ListDeliveries(
	ctx context.Context, s db.TenantScope, f domain.DeliveryFilter, p db.Keyset,
) ([]domain.Delivery, map[uuid.UUID]domain.DeliveryContext, db.Cursor, error) {
	limit := clampPageLimit(p.Limit)

	statuses := stringsOrNil(f.Statuses, func(v domain.DeliveryStatus) string { return string(v) })
	classes := stringsOrNil(f.ErrorClasses, func(v domain.ErrorClass) string { return string(v) })
	modes := stringsOrNil(f.Modes, func(v domain.Mode) string { return string(v) })

	var (
		afterAt *time.Time
		afterID uuid.UUID
	)
	if !p.Cursor.IsZero() {
		at := p.Cursor.SortKey.UTC()
		afterAt, afterID = &at, p.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, listDeliveriesSQL,
		s.OrgID(), statuses, classes, modes,
		uuidOrNil(f.ChannelID), uuidOrNil(f.NotificationID), f.Ambiguous,
		timeOrNil(f.Since), timeOrNil(f.Until), afterAt, afterID, limit+1)
	if err != nil {
		return nil, nil, db.Cursor{}, mapErr(err, "delivery_not_found", "list deliveries")
	}
	defer rows.Close()

	out := make([]domain.Delivery, 0, limit+1)
	ctxByID := make(map[uuid.UUID]domain.DeliveryContext, limit+1)
	for rows.Next() {
		var (
			row         deliveryRow
			channelName *string
			channelType *string
		)
		if err := rows.Scan(
			&row.id, &row.orgID, &row.notificationID, &row.channelID, &row.threadID,
			&row.threadSeq, &row.mode, &row.status, &row.attempts, &row.nextAttemptAt,
			&row.rendered, &row.renderedHash, &row.renderedFallback, &row.providerMessageID,
			&row.providerConversationID, &row.providerResponse, &row.errText, &row.errClass,
			&row.ambiguous, &row.sentAt, &row.createdAt, &row.updatedAt,
			&channelName, &channelType,
		); err != nil {
			return nil, nil, db.Cursor{}, mapErr(err, "delivery_not_found", "scan a delivery")
		}
		d := row.toDomain()
		out = append(out, d)
		if channelName != nil || channelType != nil {
			c := domain.DeliveryContext{}
			if channelName != nil {
				c.ChannelName = *channelName
			}
			if channelType != nil {
				c.ChannelType = domain.ChannelType(*channelType)
			}
			ctxByID[d.ID] = c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, db.Cursor{}, mapErr(err, "delivery_not_found", "read deliveries")
	}

	cursor := db.Cursor{Hash: p.Cursor.Hash}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		cursor = db.Cursor{SortKey: last.CreatedAt.UTC(), ID: last.ID, Hash: p.Cursor.Hash, HasMore: true}
	}
	return out, ctxByID, cursor, nil
}

const deliveriesForNotificationSQL = `
SELECT` + deliveryColumns + `
  FROM notification_deliveries
 WHERE org_id = $1 AND notification_id = $2
 ORDER BY thread_seq NULLS FIRST, created_at, id`

// DeliveriesFor reads every materialisation of one intent.
//
// Ordered by `thread_seq` because that is the causal order the messages were
// meant to appear in — the root, then its replies — and a detail page that showed
// them in insertion order would misrepresent the conversation.
func (r *ConfigRepository) DeliveriesFor(
	ctx context.Context, s db.TenantScope, notificationID uuid.UUID,
) ([]domain.Delivery, error) {
	rows, err := r.db(ctx).Query(ctx, deliveriesForNotificationSQL, s.OrgID(), notificationID)
	if err != nil {
		return nil, mapErr(err, "delivery_not_found", "list deliveries for a notification")
	}
	defer rows.Close()

	out := make([]domain.Delivery, 0, 4)
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, mapErr(err, "delivery_not_found", "scan a delivery")
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "delivery_not_found", "read deliveries for a notification")
	}
	return out, nil
}

const deliveriesForNotificationsSQL = `
SELECT` + deliveryColumns + `
  FROM notification_deliveries
 WHERE org_id = $1 AND notification_id = ANY($2)
 ORDER BY notification_id, thread_seq NULLS FIRST, created_at, id`

// DeliveriesForMany reads the fan-out of a PAGE of intents in ONE round trip.
//
// ⭐ IT RETURNS ROWS AND NOT COUNTS, DELIBERATELY. Aggregating in SQL would be
// cheaper and would put a second definition of "sent" in the system: the API's
// `summarise` counts a `skipped` delivery as SENT, because a coalesced no-op
// means the destination already shows exactly this content and calling that a
// failure would make a healthy quiet thread look broken. A `GROUP BY status`
// cannot know that, so the list and the detail would eventually disagree about
// the same fan-out — and the list is the one an operator reads first.
//
// The volume is bounded by the page: at most `limit` intents times the sixteen
// destinations a policy may name.
func (r *ConfigRepository) DeliveriesForMany(
	ctx context.Context, s db.TenantScope, notificationIDs []uuid.UUID,
) (map[uuid.UUID][]domain.Delivery, error) {
	if len(notificationIDs) == 0 {
		return map[uuid.UUID][]domain.Delivery{}, nil
	}
	rows, err := r.db(ctx).Query(ctx, deliveriesForNotificationsSQL, s.OrgID(), notificationIDs)
	if err != nil {
		return nil, mapErr(err, "delivery_not_found", "list deliveries for a page of notifications")
	}
	defer rows.Close()

	out := make(map[uuid.UUID][]domain.Delivery, len(notificationIDs))
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, mapErr(err, "delivery_not_found", "scan a delivery")
		}
		out[d.NotificationID] = append(out[d.NotificationID], d)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "delivery_not_found", "read deliveries for a page of notifications")
	}
	return out, nil
}

const channelContextSQL = `
SELECT id, name::text, type FROM channels WHERE org_id = $1 AND id = ANY($2)`

// ChannelContextFor resolves channel names and types for a set of channel ids in
// ONE round trip, so a page of deliveries is two queries rather than one per row.
func (r *ConfigRepository) ChannelContextFor(
	ctx context.Context, s db.TenantScope, channelIDs []uuid.UUID,
) (map[uuid.UUID]domain.DeliveryContext, error) {
	if len(channelIDs) == 0 {
		return map[uuid.UUID]domain.DeliveryContext{}, nil
	}
	rows, err := r.db(ctx).Query(ctx, channelContextSQL, s.OrgID(), channelIDs)
	if err != nil {
		return nil, mapErr(err, "channel_not_found", "read channel context")
	}
	defer rows.Close()

	out := make(map[uuid.UUID]domain.DeliveryContext, len(channelIDs))
	for rows.Next() {
		var (
			cid  uuid.UUID
			name string
			kind string
		)
		if err := rows.Scan(&cid, &name, &kind); err != nil {
			return nil, mapErr(err, "channel_not_found", "scan channel context")
		}
		out[cid] = domain.DeliveryContext{ChannelName: name, ChannelType: domain.ChannelType(kind)}
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "channel_not_found", "read channel context")
	}
	return out, nil
}

// `next_attempt_at` is the SCHEDULE and takes the caller's instant verbatim — a
// monotonic version of it would refuse to bring an attempt forward, which is the
// whole point of a manual re-queue. `updated_at` is the row's version and is
// monotonic: `deliveries_time_ck` is `updated_at >= created_at`, and the operator
// pressing "retry" is served by whichever pod the load balancer chose, not by the
// one that fanned the delivery out.
const requeueDeliverySQL = `
UPDATE notification_deliveries
   SET status = 'pending', next_attempt_at = $3, error = NULL, error_class = NULL,
       updated_at = GREATEST(updated_at, $3)
 WHERE org_id = $1 AND id = $2 AND status = 'dead'
RETURNING` + deliveryColumns

// RequeueDead re-queues a delivery that has been given up on.
//
// ⛔ ONLY A `dead` DELIVERY MAY BE RETRIED THIS WAY, and the predicate is in the
// UPDATE rather than in a prior SELECT: a read-then-write would race a worker
// that is claiming the same row. Zero rows means it was not dead, which the
// caller reports as `412` — pending and failed deliveries are already on their own
// backoff schedule and do not need a nudge, and nudging one would double-send.
//
// `error` and `error_class` are cleared because they describe the attempt that
// killed it; leaving a stale `auth_expired` on a re-queued row would make the
// dead-letter screen keep accusing a credential that has just been rotated.
// `attempts` is deliberately NOT reset: the history of how hard oto tried is a
// fact, and deliveries_attempts_ck caps it at 32 either way.
func (r *ConfigRepository) RequeueDead(
	ctx context.Context, s db.TenantScope, deliveryID uuid.UUID,
) (domain.Delivery, bool, error) {
	now := r.clock.Now().UTC()
	d, err := scanDelivery(r.db(ctx).QueryRow(ctx, requeueDeliverySQL, s.OrgID(), deliveryID, now))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Not dead — or not there. The caller re-reads to tell those apart and
			// answers 412 or 404 accordingly.
			return domain.Delivery{}, false, nil
		}
		return domain.Delivery{}, false, mapErr(err, "delivery_not_found", "re-queue a delivery")
	}
	return d, true, nil
}

// ------------------------------------------------------------------- helpers

func encodeMatchers(in []domain.Matcher) ([]byte, error) {
	out := make([]matcherJSON, 0, len(in))
	for _, m := range in {
		out = append(out, matcherJSON{Name: m.Name, Op: string(m.Op), Value: m.Value})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, errs.Internal("jsonb_encode_failed", err)
	}
	return b, nil
}

// encodeThrottle renders a throttle as jsonb. A nil or disabled throttle becomes
// `{}` rather than SQL NULL, because policies_throttle_ck requires an object.
func encodeThrottle(t *domain.Throttle) ([]byte, error) {
	if t == nil || !t.Enabled() {
		return []byte(`{}`), nil
	}
	b, err := json.Marshal(throttleJSON{Max: t.Max, WindowS: int(t.Window / time.Second)})
	if err != nil {
		return nil, errs.Internal("jsonb_encode_failed", err)
	}
	return b, nil
}

func secondsPtr(d *time.Duration) *int {
	if d == nil || *d <= 0 {
		return nil
	}
	v := int(*d / time.Second)
	return &v
}

func uuidOrNil(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	v := id
	return &v
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	v := t.UTC()
	return &v
}

// stringsOrNil renders a typed enum slice as a text[] parameter, or nil for "no
// constraint". nil rather than an empty array matters: `= ANY('{}')` matches
// nothing, which would silently return an empty page for an absent filter.
func stringsOrNil[T any](in []T, render func(T) string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, render(v))
	}
	return out
}

func errsMissing(what string) error { return &configError{what: what} }

type configError struct{ what string }

func (e *configError) Error() string { return "repository: " + e.what }
