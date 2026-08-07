package repository

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// snoozeRow is the row model of `alert_snoozes`. Unexported, per the three-model
// rule.
type snoozeRow struct {
	id       uuid.UUID
	orgID    uuid.UUID
	alertID  uuid.UUID
	alertKey string

	snoozedAt      time.Time
	snoozedUntil   time.Time
	snoozedBy      *uuid.UUID
	snoozedByLabel string
	note           *string

	endedAt      *time.Time
	endedReason  *string
	endedBy      *uuid.UUID
	endedByLabel *string
}

var snoozeColumnList = []string{
	"id", "org_id", "alert_id", "alert_key", "snoozed_at", "snoozed_until", "snoozed_by",
	"snoozed_by_label", "note", "ended_at", "ended_reason", "ended_by", "ended_by_label",
}

var snoozeColumns = strings.Join(snoozeColumnList, ", ")

func (r *snoozeRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.alertID, &r.alertKey, &r.snoozedAt, &r.snoozedUntil, &r.snoozedBy,
		&r.snoozedByLabel, &r.note, &r.endedAt, &r.endedReason, &r.endedBy, &r.endedByLabel,
	}
}

func (r *snoozeRow) toDomain() (domain.Snooze, error) {
	key, err := domain.NewAlertKey(r.alertKey)
	if err != nil {
		return domain.Snooze{}, errs.Internal("snooze_alert_key_invalid", err)
	}
	var reason domain.SnoozeEndReason
	if r.endedReason != nil {
		reason, err = domain.NewSnoozeEndReason(*r.endedReason)
		if err != nil {
			return domain.Snooze{}, errs.Internal("snooze_end_reason_invalid", err)
		}
	}

	s, err := domain.NewSnooze(domain.SnoozeParams{
		ID:             r.id,
		OrgID:          r.orgID,
		AlertID:        r.alertID,
		AlertKey:       key,
		SnoozedAt:      r.snoozedAt,
		SnoozedUntil:   r.snoozedUntil,
		SnoozedBy:      idOrNil(r.snoozedBy),
		SnoozedByLabel: r.snoozedByLabel,
		Note:           strOrEmpty(r.note),
		EndedAt:        timeOrZero(r.endedAt),
		EndedReason:    reason,
		EndedBy:        idOrNil(r.endedBy),
		EndedByLabel:   strOrEmpty(r.endedByLabel),
	})
	if err != nil {
		return domain.Snooze{}, errs.Internal("snooze_row_invalid", err)
	}
	return s, nil
}

// SnoozeRepository is the SQL over `alert_snoozes` (§B.8, §F.5.2). It implements
// service.SnoozeRepository.
//
// "Exactly one active snooze per alert" is enforced by the partial unique index
// alert_snoozes_active_idx, NOT by application code: Create runs in the same
// transaction as the End that supersedes the previous one, so a 23505 here is a
// concurrency bug and never a race the application was expected to lose.
type SnoozeRepository struct {
	q     db.Querier
	clock clock.Clock
}

// NewSnoozeRepository builds the repository over a fallback querier.
func NewSnoozeRepository(q db.Querier, clk clock.Clock) *SnoozeRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &SnoozeRepository{q: q, clock: clk}
}

func (r *SnoozeRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

var createSnoozeSQL = `
INSERT INTO alert_snoozes (id, org_id, alert_id, alert_key, snoozed_at, snoozed_until,
                           snoozed_by, snoozed_by_label, note)
SELECT $1, a.org_id, a.id, a.alert_key, $4, $5, $6, $7, $8
  FROM alerts a
 WHERE a.org_id = $2 AND a.id = $3
RETURNING ` + snoozeColumns

// Create opens a snooze on an Alert.
//
// `alert_key` is denormalised from the Alert in the same statement so that the
// audit trail survives the Alert row, and `snoozed_at` comes from the injected
// clock rather than now() so the window bounds stay testable. The 5-minute and
// 30-day bounds are NOT checked here — that is a business rule and belongs to
// the service (§L.9) — but alert_snoozes_min_ck and _max_ck are the backstop.
func (r *SnoozeRepository) Create(
	ctx context.Context, s db.TenantScope, in domain.SnoozeRequest,
) (domain.Snooze, error) {
	return r.CreateWithID(ctx, s, id.New(), in)
}

// CreateWithID is Create with the row id supplied by the caller.
//
// It exists because domain.StartSnooze mints the `alert.snoozed` event with the
// snooze id in its payload, and the event and the row must agree. SnoozeRequest
// (§F.5.2) carries no id field, so the service passes one here rather than
// discovering afterwards that the two disagree.
func (r *SnoozeRepository) CreateWithID(
	ctx context.Context, s db.TenantScope, snoozeID uuid.UUID, in domain.SnoozeRequest,
) (domain.Snooze, error) {
	if err := requireScope(s); err != nil {
		return domain.Snooze{}, err
	}
	if err := requireID("snooze id", snoozeID); err != nil {
		return domain.Snooze{}, err
	}
	if err := requireID("alert_id", in.AlertID); err != nil {
		return domain.Snooze{}, err
	}
	if in.Until.IsZero() {
		// alert_snoozes.snoozed_until is NOT NULL, deliberately: there is no
		// indefinite snooze (§B.8.3).
		return domain.Snooze{}, errs.Internal("snooze_until_missing",
			errsMissing("snoozed_until is required"))
	}
	if strings.TrimSpace(in.ByLabel) == "" {
		return domain.Snooze{}, errs.Internal("snooze_label_missing",
			errsMissing("snoozed_by_label is required: a snooze is always attributed"))
	}

	var row snoozeRow
	err := r.db(ctx).QueryRow(ctx, createSnoozeSQL,
		snoozeID, s.OrgID(), in.AlertID, r.clock.Now().UTC(), in.Until.UTC(),
		in.By, in.ByLabel, in.Note,
	).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			return domain.Snooze{}, errs.NotFound("alert_not_found", "no such alert")
		}
		return domain.Snooze{}, mapErr(err, "create snooze")
	}
	return row.toDomain()
}

// GetActive reads the Alert's one open snooze row, if it has one. "Open" is a
// fact about the ROW, which is exactly what alert_snoozes_active_idx is partial
// on; an open row whose clock has run out is one snooze.expire has not swept yet.
func (r *SnoozeRepository) GetActive(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
) (domain.Snooze, bool, error) {
	if err := requireScope(s); err != nil {
		return domain.Snooze{}, false, err
	}
	var row snoozeRow
	err := r.db(ctx).QueryRow(ctx,
		`SELECT `+snoozeColumns+`
		   FROM alert_snoozes
		  WHERE org_id = $1 AND alert_id = $2 AND ended_at IS NULL`,
		s.OrgID(), alertID).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			return domain.Snooze{}, false, nil
		}
		return domain.Snooze{}, false, mapErr(err, "read active snooze")
	}
	snz, err := row.toDomain()
	if err != nil {
		return domain.Snooze{}, false, err
	}
	return snz, true, nil
}

var endSnoozeSQL = `
UPDATE alert_snoozes SET
    ended_at       = GREATEST(snoozed_at, $3),
    ended_reason   = $4,
    ended_by       = $5,
    ended_by_label = $6
WHERE org_id = $1 AND id = $2 AND ended_at IS NULL
RETURNING ` + snoozeColumns

// End closes a snooze. The `ended_at IS NULL` predicate makes it a
// compare-and-set: closing an already-closed snooze affects zero rows, which is
// reported as a precondition failure rather than silently rewriting history.
func (r *SnoozeRepository) End(
	ctx context.Context, s db.TenantScope, in domain.SnoozeEnd,
) (domain.Snooze, error) {
	if err := requireScope(s); err != nil {
		return domain.Snooze{}, err
	}
	if err := requireID("snooze id", in.SnoozeID); err != nil {
		return domain.Snooze{}, err
	}
	if in.Reason == "" {
		return domain.Snooze{}, errs.Internal("snooze_end_reason_missing",
			errsMissing("a snooze ends for a stated reason"))
	}
	at := in.At
	if at.IsZero() {
		at = r.clock.Now()
	}

	var row snoozeRow
	err := r.db(ctx).QueryRow(ctx, endSnoozeSQL,
		s.OrgID(), in.SnoozeID, at.UTC(), in.Reason, in.By, in.ByLabel,
	).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			return domain.Snooze{}, errs.Precondition("snooze_already_ended",
				"this snooze does not exist or has already ended")
		}
		return domain.Snooze{}, mapErr(err, "end snooze")
	}
	return row.toDomain()
}

var expiredSnoozesSQL = `
SELECT ` + snoozeColumns + `
  FROM alert_snoozes
 WHERE org_id = $1 AND ended_at IS NULL AND snoozed_until <= $2
 ORDER BY snoozed_until ASC
 LIMIT $3`

// ExpiredCandidates feeds the 60-second `snooze.expire` job (§B.8.3, §G.3):
// active snoozes whose clock has run out, oldest first.
//
// NOTE (planner): alert_snoozes_expiry_idx is (snoozed_until) WHERE ended_at IS
// NULL and deliberately does not lead with org_id, because the sweep is a
// background job. The org predicate this port requires is applied after the
// index scan.
func (r *SnoozeRepository) ExpiredCandidates(
	ctx context.Context, s db.TenantScope, before time.Time, limit int,
) ([]domain.Snooze, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	if before.IsZero() {
		before = r.clock.Now()
	}
	n := clampLimit(limit)

	rows, err := r.db(ctx).Query(ctx, expiredSnoozesSQL, s.OrgID(), before.UTC(), n)
	if err != nil {
		return nil, mapErr(err, "list expired snoozes")
	}
	defer rows.Close()

	out := make([]domain.Snooze, 0, n)
	for rows.Next() {
		var row snoozeRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "scan snooze")
		}
		snz, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, snz)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read expired snoozes")
	}
	return out, nil
}

// ListByAlert is the snooze history for one Alert, newest first
// (alert_snoozes_org_idx). Membership of a snooze is history, not a boolean, and
// the UI banner of §B.8.6 is what makes the feature safe.
func (r *SnoozeRepository) ListByAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, limit int,
) ([]domain.Snooze, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	n := clampLimit(limit)

	rows, err := r.db(ctx).Query(ctx,
		`SELECT `+snoozeColumns+`
		   FROM alert_snoozes
		  WHERE org_id = $1 AND alert_id = $2
		  ORDER BY snoozed_at DESC
		  LIMIT $3`,
		s.OrgID(), alertID, n)
	if err != nil {
		return nil, mapErr(err, "list snoozes")
	}
	defer rows.Close()

	out := make([]domain.Snooze, 0, n)
	for rows.Next() {
		var row snoozeRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "scan snooze")
		}
		snz, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, snz)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read snoozes")
	}
	return out, nil
}
