package repository

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// occurrenceRow is the row model of `alert_occurrences`. Unexported, per the
// three-model rule.
type occurrenceRow struct {
	id      uuid.UUID
	orgID   uuid.UUID
	alertID uuid.UUID
	groupID *uuid.UUID
	seq     int32

	state             string
	suppressionReason *string
	suppressedBy      []byte

	startedAt      time.Time
	endedAt        *time.Time
	lastObservedAt time.Time

	sourceStartsAt  time.Time
	sourceEndsAt    *time.Time
	sourceUpdatedAt *time.Time

	resolveReason *string
	reopenCount   int32
	reopenOf      *uuid.UUID

	ackState     string
	ackedBy      *uuid.UUID
	ackedByLabel *string
	ackedAt      *time.Time
	ackNote      *string

	ruleSnapshotID *uuid.UUID
	value          *float64
	observedSkewMS int64
}

var occurrenceColumnList = []string{
	"id", "org_id", "alert_id", "group_id", "seq", "state", "suppression_reason", "suppressed_by",
	"started_at", "ended_at", "last_observed_at", "source_starts_at", "source_ends_at",
	"source_updated_at", "resolve_reason", "reopen_count", "reopen_of", "ack_state", "acked_by",
	"acked_by_label", "acked_at", "ack_note", "rule_snapshot_id", "value", "observed_skew_ms",
}

var occurrenceColumns = strings.Join(occurrenceColumnList, ", ")

func (r *occurrenceRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.alertID, &r.groupID, &r.seq, &r.state, &r.suppressionReason,
		&r.suppressedBy, &r.startedAt, &r.endedAt, &r.lastObservedAt, &r.sourceStartsAt,
		&r.sourceEndsAt, &r.sourceUpdatedAt, &r.resolveReason, &r.reopenCount, &r.reopenOf,
		&r.ackState, &r.ackedBy, &r.ackedByLabel, &r.ackedAt, &r.ackNote, &r.ruleSnapshotID,
		&r.value, &r.observedSkewMS,
	}
}

func (r *occurrenceRow) toDomain() (domain.Occurrence, error) {
	state, err := domain.NewState(r.state)
	if err != nil {
		return domain.Occurrence{}, errs.Internal("occurrence_state_invalid", err)
	}
	ack, err := domain.NewAckState(r.ackState)
	if err != nil {
		return domain.Occurrence{}, errs.Internal("occurrence_ack_state_invalid", err)
	}
	var sup domain.SuppressionReason
	if r.suppressionReason != nil {
		sup, err = domain.NewSuppressionReason(*r.suppressionReason)
		if err != nil {
			return domain.Occurrence{}, errs.Internal("occurrence_suppression_reason_invalid", err)
		}
	}
	var res domain.ResolveReason
	if r.resolveReason != nil {
		res, err = domain.NewResolveReason(*r.resolveReason)
		if err != nil {
			return domain.Occurrence{}, errs.Internal("occurrence_resolve_reason_invalid", err)
		}
	}

	o, err := domain.NewOccurrence(domain.OccurrenceParams{
		ID:                r.id,
		OrgID:             r.orgID,
		AlertID:           r.alertID,
		GroupID:           idOrNil(r.groupID),
		Seq:               int(r.seq),
		State:             state,
		SuppressionReason: sup,
		StartedAt:         r.startedAt,
		EndedAt:           timeOrZero(r.endedAt),
		LastObservedAt:    r.lastObservedAt,
		SourceStartsAt:    r.sourceStartsAt,
		SourceEndsAt:      timeOrZero(r.sourceEndsAt),
		SourceUpdatedAt:   timeOrZero(r.sourceUpdatedAt),
		ResolveReason:     res,
		ReopenCount:       int(r.reopenCount),
		ReopenOf:          idOrNil(r.reopenOf),
		AckState:          ack,
		AckedBy:           idOrNil(r.ackedBy),
		AckedByLabel:      strOrEmpty(r.ackedByLabel),
		AckedAt:           timeOrZero(r.ackedAt),
		AckNote:           strOrEmpty(r.ackNote),
		RuleSnapshotID:    idOrNil(r.ruleSnapshotID),
		Value:             r.value,
		ObservedSkew:      time.Duration(r.observedSkewMS) * time.Millisecond,
	})
	if err != nil {
		return domain.Occurrence{}, errs.Internal("occurrence_row_invalid", err)
	}
	return o, nil
}

// OccurrenceRepository is the SQL over `alert_occurrences` — the table the
// authoritative §B.3 state machine runs on. It implements
// service.OccurrenceRepository.
//
// It writes exactly what the domain machine produced. There is no `if` in this
// file that moves a state: assembling a Transition by hand is how an occurrence
// acquires a state no §B.3 row permits.
type OccurrenceRepository struct{ q db.Querier }

// NewOccurrenceRepository builds the repository over a fallback querier.
func NewOccurrenceRepository(q db.Querier) *OccurrenceRepository {
	return &OccurrenceRepository{q: q}
}

func (r *OccurrenceRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// ------------------------------------------------------------------- open

// The alert_id is re-proven to belong to the caller's org rather than trusted.
// alert_occurrences.org_id is denormalised, so writing a scope's org_id beside
// another org's alert_id would create a row that every org-scoped read agrees is
// ours — which is the shape a cross-tenant leak takes.
var openOccurrenceSQL = `
INSERT INTO alert_occurrences (
    id, org_id, alert_id, group_id, seq, state, started_at, ended_at, last_observed_at,
    source_starts_at, source_ends_at, source_updated_at, reopen_of, value, observed_skew_ms,
    ack_state)
SELECT $1, a.org_id, a.id, $4, $5, 'firing', $6, NULL, $6, $7, $8, $9, $10, $11, $12, 'unacked'
  FROM alerts a
 WHERE a.org_id = $2 AND a.id = $3
RETURNING ` + occurrenceColumns

// OpenOccurrence opens a new firing episode — T1 (first sighting) and T7 (a
// re-fire beyond refire_grace).
//
// A new occurrence ALWAYS starts unacked: T10 says an acknowledgement does not
// survive into a new episode. The "at most one open occurrence per alert"
// invariant is enforced by occ_one_open_idx, in the database and not in Go, so a
// 23505 here is a genuine concurrency conflict rather than a race the
// application was expected to lose.
func (r *OccurrenceRepository) OpenOccurrence(
	ctx context.Context, s db.TenantScope, in domain.OpenOccurrence,
) (domain.Occurrence, error) {
	if err := requireScope(s); err != nil {
		return domain.Occurrence{}, err
	}
	if err := requireID("occurrence id", in.ID); err != nil {
		return domain.Occurrence{}, err
	}
	if err := requireID("alert_id", in.AlertID); err != nil {
		return domain.Occurrence{}, err
	}
	if in.Seq < 1 {
		return domain.Occurrence{}, errs.Internal("occurrence_seq_invalid",
			errsMissing("seq must be >= 1"))
	}
	if in.StartedAt.IsZero() || in.SourceStartsAt.IsZero() {
		return domain.Occurrence{}, errs.Internal("occurrence_time_missing",
			errsMissing("started_at and source_starts_at are required"))
	}

	var row occurrenceRow
	err := r.db(ctx).QueryRow(ctx, openOccurrenceSQL,
		in.ID, s.OrgID(), in.AlertID, in.GroupID, in.Seq, in.StartedAt.UTC(),
		in.SourceStartsAt.UTC(), in.SourceEndsAt, in.SourceUpdatedAt, in.ReopenOf,
		in.Value, in.SkewMS,
	).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			return domain.Occurrence{}, errs.NotFound("alert_not_found", "no such alert")
		}
		return domain.Occurrence{}, mapErr(err, "open occurrence")
	}
	return row.toDomain()
}

// ------------------------------------------------------------------- reads

// GetByID reads one occurrence within the caller's org.
func (r *OccurrenceRepository) GetByID(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) (domain.Occurrence, error) {
	if err := requireScope(s); err != nil {
		return domain.Occurrence{}, err
	}
	var row occurrenceRow
	err := r.db(ctx).QueryRow(ctx,
		`SELECT `+occurrenceColumns+` FROM alert_occurrences WHERE org_id = $1 AND id = $2`,
		s.OrgID(), id).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			return domain.Occurrence{}, errs.NotFound("occurrence_not_found", "no such occurrence")
		}
		return domain.Occurrence{}, mapErr(err, "read occurrence")
	}
	return row.toDomain()
}

// GetOpenByAlert reads the one open episode of an Alert, if there is one. At
// most one can exist (occ_one_open_idx).
func (r *OccurrenceRepository) GetOpenByAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
) (domain.Occurrence, bool, error) {
	return r.optional(ctx, s,
		`SELECT `+occurrenceColumns+`
		   FROM alert_occurrences
		  WHERE org_id = $1 AND alert_id = $2 AND ended_at IS NULL`,
		s.OrgID(), alertID)
}

// GetLatestByAlert reads the most recent episode of an Alert, open or ended.
// This is what the state machine reads to decide between T1, T7 and T8.
func (r *OccurrenceRepository) GetLatestByAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
) (domain.Occurrence, bool, error) {
	return r.optional(ctx, s,
		`SELECT `+occurrenceColumns+`
		   FROM alert_occurrences
		  WHERE org_id = $1 AND alert_id = $2
		  ORDER BY seq DESC
		  LIMIT 1`,
		s.OrgID(), alertID)
}

func (r *OccurrenceRepository) optional(
	ctx context.Context, s db.TenantScope, sql string, args ...any,
) (domain.Occurrence, bool, error) {
	if err := requireScope(s); err != nil {
		return domain.Occurrence{}, false, err
	}
	var row occurrenceRow
	if err := r.db(ctx).QueryRow(ctx, sql, args...).Scan(row.scanDest()...); err != nil {
		if isNoRows(err) {
			return domain.Occurrence{}, false, nil
		}
		return domain.Occurrence{}, false, mapErr(err, "read occurrence")
	}
	o, err := row.toDomain()
	if err != nil {
		return domain.Occurrence{}, false, err
	}
	return o, true, nil
}

var latestByAlertsSQL = `
SELECT DISTINCT ON (alert_id) ` + occurrenceColumns + `
  FROM alert_occurrences
 WHERE org_id = $1 AND alert_id = ANY($2)
 ORDER BY alert_id, seq DESC`

// LatestByAlerts is GetLatestByAlert for a whole webhook batch, in one round
// trip. A 200-alert payload must not become 200 round trips (§G.4); it rides
// occ_alert_idx (org_id, alert_id, seq DESC).
func (r *OccurrenceRepository) LatestByAlerts(
	ctx context.Context, s db.TenantScope, alertIDs []uuid.UUID,
) (map[uuid.UUID]domain.Occurrence, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	if len(alertIDs) == 0 {
		return map[uuid.UUID]domain.Occurrence{}, nil
	}

	rows, err := r.db(ctx).Query(ctx, latestByAlertsSQL, s.OrgID(), alertIDs)
	if err != nil {
		return nil, mapErr(err, "read latest occurrences")
	}
	defer rows.Close()

	out := make(map[uuid.UUID]domain.Occurrence, len(alertIDs))
	for rows.Next() {
		var row occurrenceRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "scan occurrence")
		}
		o, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out[o.AlertID()] = o
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read latest occurrences")
	}
	return out, nil
}

// ListByAlert is the per-alert episode history, newest first.
//
// NOTE (planner): occ_alert_idx is (org_id, alert_id, seq DESC), which serves the
// lookup but not the `started_at` ordering the keyset cursor is expressed in. seq
// and started_at are monotonic together, so the sort is over the handful of
// episodes one Alert has; there is no index to add here that is not a migration.
func (r *OccurrenceRepository) ListByAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset,
) ([]domain.Occurrence, db.Cursor, error) {
	if err := requireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := clampLimit(p.Limit)

	sql := `SELECT ` + occurrenceColumns + `
	          FROM alert_occurrences
	         WHERE org_id = $1 AND alert_id = $2
	           AND ($3::timestamptz IS NULL OR (started_at, id) < ($3, $4))
	         ORDER BY started_at DESC, id DESC
	         LIMIT $5`

	var cursorAt *time.Time
	var cursorID uuid.UUID
	if !p.Cursor.IsZero() {
		cursorAt = timePtr(p.Cursor.SortKey)
		cursorID = p.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, sql, s.OrgID(), alertID, cursorAt, cursorID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "list occurrences")
	}
	defer rows.Close()

	collected, err := collectOccurrences(rows, limit+1)
	if err != nil {
		return nil, db.Cursor{}, err
	}
	page, hasMore := pageOf(collected, limit)
	if len(page) == 0 {
		return nil, db.Cursor{Hash: p.Cursor.Hash}, nil
	}
	last := page[len(page)-1]
	return page, nextCursor(last.StartedAt(), last.ID(), p.Cursor.Hash, hasMore), nil
}

func collectOccurrences(rows pgx.Rows, capacity int) ([]domain.Occurrence, error) {
	out := make([]domain.Occurrence, 0, capacity)
	for rows.Next() {
		var row occurrenceRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "scan occurrence")
		}
		o, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read occurrences")
	}
	return out, nil
}

// ------------------------------------------------------------------- writes

const observeSQL = `
UPDATE alert_occurrences SET
    last_observed_at  = GREATEST(last_observed_at, $3),
    source_ends_at    = COALESCE($4, source_ends_at),
    source_updated_at = COALESCE($5, source_updated_at),
    value             = COALESCE($6, value),
    observed_skew_ms  = $7,
    updated_at        = now()
WHERE org_id = $1 AND id = $2`

// Observe folds a repeat observation (T2) into the open occurrence.
//
// A field the observation did not supply is PRESERVED, never cleared: §L.3.1
// says a zero `endsAt` means "no end time known for this payload", not "forget
// the end time you already had". Clearing it would silently disable the reaper
// for that occurrence, because occ_reap_idx only sees rows with a non-null
// source_ends_at.
func (r *OccurrenceRepository) Observe(
	ctx context.Context, s db.TenantScope, id uuid.UUID, o domain.Observation,
) error {
	if err := requireScope(s); err != nil {
		return err
	}
	if err := requireID("occurrence id", id); err != nil {
		return err
	}
	if o.ObservedAt.IsZero() {
		return errs.Internal("observation_time_missing", errsMissing("observed_at is required"))
	}

	tag, err := r.db(ctx).Exec(ctx, observeSQL, s.OrgID(), id, o.ObservedAt.UTC(),
		timePtr(o.SourceEndsAt), timePtr(o.SourceUpdatedAt), o.Value, o.SkewMS)
	if err != nil {
		return mapErr(err, "record observation")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("occurrence_not_found", "no such occurrence")
	}
	return nil
}

const transitionSQL = `
UPDATE alert_occurrences SET
    state              = $3,
    suppression_reason = $4,
    suppressed_by      = COALESCE($5, suppressed_by),
    resolve_reason     = $6,
    ended_at           = $7,
    last_observed_at   = GREATEST(last_observed_at, $8),
    source_ends_at     = COALESCE($9, source_ends_at),
    source_updated_at  = COALESCE($10, source_updated_at),
    reopen_count       = COALESCE($11, reopen_count),
    value              = COALESCE($12, value),
    updated_at         = now()
WHERE org_id = $1 AND id = $2`

// Transition persists one §B.3 edge, exactly as the domain machine produced it.
//
// `ended_at` is written verbatim: it has ALREADY been clamped to >= started_at by
// §B.3.2 and re-deriving it here would give two answers to one question. A nil
// EndedAt CLEARS the column, which is what makes T8 (reopen) work.
func (r *OccurrenceRepository) Transition(
	ctx context.Context, s db.TenantScope, id uuid.UUID, t domain.Transition,
) error {
	if err := requireScope(s); err != nil {
		return err
	}
	if err := requireID("occurrence id", id); err != nil {
		return err
	}
	if !t.ToState.IsOpen() && !t.ToState.IsTerminal() {
		return errs.Internal("transition_state_invalid", errsMissing("to_state is required"))
	}
	if t.LastObservedAt.IsZero() {
		return errs.Internal("transition_time_missing", errsMissing("last_observed_at is required"))
	}

	var suppressedBy []byte
	if t.SuppressedBy != nil {
		b, err := t.SuppressedBy.MarshalJSON()
		if err != nil {
			return errs.Internal("suppressed_by_encode_failed", err)
		}
		suppressedBy = b
	}

	tag, err := r.db(ctx).Exec(ctx, transitionSQL, s.OrgID(), id,
		t.ToState.String(), t.SuppressionReason, suppressedBy, t.ResolveReason,
		t.EndedAt, t.LastObservedAt.UTC(), t.SourceEndsAt, t.SourceUpdatedAt,
		t.ReopenCount, t.Value)
	if err != nil {
		return mapErr(err, "apply transition")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("occurrence_not_found", "no such occurrence")
	}
	return nil
}

const setAckSQL = `
UPDATE alert_occurrences SET
    ack_state      = $3,
    acked_by       = $4,
    acked_by_label = $5,
    acked_at       = $6,
    ack_note       = $7,
    updated_at     = now()
WHERE org_id = $1 AND id = $2`

// SetAck writes T9 or T10. Ack fields are ALL-OR-NOTHING (occ_ack_ck): writing
// three of the four is writing a row the database will refuse, so an unack
// clears every one of them together.
func (r *OccurrenceRepository) SetAck(
	ctx context.Context, s db.TenantScope, id uuid.UUID, a domain.AckChange,
) error {
	if err := requireScope(s); err != nil {
		return err
	}
	if err := requireID("occurrence id", id); err != nil {
		return err
	}

	var (
		by      *uuid.UUID
		byLabel *string
		at      *time.Time
		note    *string
	)
	if a.To.IsAcked() {
		if a.At.IsZero() {
			return errs.Internal("ack_time_missing", errsMissing("acked_at is required"))
		}
		if a.ByLabel == nil || strings.TrimSpace(*a.ByLabel) == "" {
			return errs.Internal("ack_label_missing", errsMissing("acked_by_label is required"))
		}
		by, byLabel, at, note = a.By, a.ByLabel, timePtr(a.At), a.Note
	}

	tag, err := r.db(ctx).Exec(ctx, setAckSQL, s.OrgID(), id,
		ackStateOrUnacked(a.To).String(), by, byLabel, at, note)
	if err != nil {
		return mapErr(err, "write acknowledgement")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("occurrence_not_found", "no such occurrence")
	}
	return nil
}

func ackStateOrUnacked(a domain.AckState) domain.AckState {
	if a.IsZero() {
		return domain.AckStateUnacked
	}
	return a
}

// BindRuleSnapshot binds the RuleSnapshot captured at fire time — what the rule
// SAID at that moment (R6).
func (r *OccurrenceRepository) BindRuleSnapshot(
	ctx context.Context, s db.TenantScope, id, snapshotID uuid.UUID,
) error {
	if err := requireScope(s); err != nil {
		return err
	}
	if err := requireID("occurrence id", id); err != nil {
		return err
	}
	if err := requireID("rule_snapshot_id", snapshotID); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx,
		`UPDATE alert_occurrences SET rule_snapshot_id = $3, updated_at = now()
		  WHERE org_id = $1 AND id = $2`,
		s.OrgID(), id, snapshotID)
	if err != nil {
		return mapErr(err, "bind rule snapshot")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("occurrence_not_found", "no such occurrence")
	}
	return nil
}

// ------------------------------------------------------------------- reaper

var reapCandidatesSQL = `
SELECT ` + occurrenceColumns + `
  FROM alert_occurrences
 WHERE org_id = $1
   AND ended_at IS NULL
   AND source_ends_at IS NOT NULL
   AND source_ends_at < $2
 ORDER BY source_ends_at ASC
 LIMIT $3`

// ReapCandidates feeds T6: open episodes whose upstream end time plus
// resolve_grace has passed. `before` is already `now - resolve_grace`, computed
// by the caller against the injected clock.
//
// ⭐ THE REAPER GUARD (§B.4) IS THE CALLER'S. This method returns CANDIDATES, not
// verdicts. An occurrence whose AlertSource is not healthy MUST be held, never
// expired: losing sight of an alert is not the same as the alert resolving.
//
// NOTE (planner): occ_reap_idx is (source_ends_at) WHERE ended_at IS NULL AND
// source_ends_at IS NOT NULL and deliberately does NOT lead with org_id — the
// reaper is a global background sweep. The `org_id = $1` predicate this port's
// TenantScope requires is therefore a filter applied after the index scan, which
// is correct but not free on a multi-tenant deployment.
func (r *OccurrenceRepository) ReapCandidates(
	ctx context.Context, s db.TenantScope, before time.Time, limit int,
) ([]domain.Occurrence, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	if before.IsZero() {
		return nil, errs.Internal("reap_bound_missing", errsMissing("before is required"))
	}
	n := clampLimit(limit)

	rows, err := r.db(ctx).Query(ctx, reapCandidatesSQL, s.OrgID(), before.UTC(), n)
	if err != nil {
		return nil, mapErr(err, "list reap candidates")
	}
	defer rows.Close()
	return collectOccurrences(rows, n)
}

const occurrenceSourcesSQL = `
SELECT m.occurrence_id, g.source_id
  FROM alert_group_members m
  JOIN alert_groups g ON g.id = m.group_id
 WHERE m.org_id = $1 AND m.occurrence_id = ANY($2)`

// SourceIDs resolves which AlertSource each occurrence came from, by way of the
// AlertGroup generation it joined.
//
// It exists for the §B.4 reaper guard, which must load `source_health` for the
// owning source before it may expire anything. An occurrence with no resolvable
// source is ABSENT from the result, and the caller must read that as "cannot
// prove the source is healthy" and HOLD it.
func (r *OccurrenceRepository) SourceIDs(
	ctx context.Context, s db.TenantScope, occurrenceIDs []uuid.UUID,
) (map[uuid.UUID]uuid.UUID, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	if len(occurrenceIDs) == 0 {
		return map[uuid.UUID]uuid.UUID{}, nil
	}

	rows, err := r.db(ctx).Query(ctx, occurrenceSourcesSQL, s.OrgID(), occurrenceIDs)
	if err != nil {
		return nil, mapErr(err, "resolve occurrence sources")
	}
	defer rows.Close()

	out := make(map[uuid.UUID]uuid.UUID, len(occurrenceIDs))
	for rows.Next() {
		var occID, srcID uuid.UUID
		if err := rows.Scan(&occID, &srcID); err != nil {
			return nil, mapErr(err, "scan occurrence source")
		}
		out[occID] = srcID
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read occurrence sources")
	}
	return out, nil
}
