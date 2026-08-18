package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/drill/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// drillColumns is the projection every read of `delivery_drills` shares.
// ⛔ `status`, `failed_stage` and `started_by` are STORED AND NOT PROJECTED. The
// first two are denormalised out of `outcome` so an operator can grep for them in
// psql; reading them back onto the entity would give the verdict two homes that
// can disagree. `started_by` is written for attribution and never read, which is
// the shape SPEC R8 asks for: a per-person metric cannot be built from a column
// nothing selects.
const drillColumns = `id, source_id, drill_label, severity, batch_id, alert_id,
       case_id, group_id, notification_id, outcome,
       started_by_label, started_at, deadline_at, finished_at, disposed_at`

// drillRow is the row model. Unexported, per the three-model rule.
type drillRow struct {
	id             uuid.UUID
	sourceID       uuid.UUID
	label          string
	severity       string
	batchID        *uuid.UUID
	alertID        *uuid.UUID
	caseID         *uuid.UUID
	groupID        *uuid.UUID
	notificationID *uuid.UUID
	outcome        []byte
	startedByLabel string
	startedAt      time.Time
	deadlineAt     time.Time
	finishedAt     *time.Time
	disposedAt     *time.Time
}

func (r *drillRow) scanDest() []any {
	return []any{
		&r.id, &r.sourceID, &r.label, &r.severity, &r.batchID, &r.alertID,
		&r.caseID, &r.groupID, &r.notificationID, &r.outcome,
		&r.startedByLabel, &r.startedAt, &r.deadlineAt, &r.finishedAt, &r.disposedAt,
	}
}

func (r *drillRow) toDomain() (domain.Drill, error) {
	d := domain.Drill{
		ID:             r.id,
		SourceID:       r.sourceID,
		Label:          r.label,
		Severity:       r.severity,
		BatchID:        idOrNil(r.batchID),
		AlertID:        idOrNil(r.alertID),
		CaseID:         idOrNil(r.caseID),
		GroupID:        idOrNil(r.groupID),
		NotificationID: idOrNil(r.notificationID),
		StartedByLabel: r.startedByLabel,
		StartedAt:      r.startedAt.UTC(),
		DeadlineAt:     r.deadlineAt.UTC(),
		FinishedAt:     timeOrZero(r.finishedAt),
		DisposedAt:     timeOrZero(r.disposedAt),
	}
	if len(r.outcome) > 0 {
		res, err := decodeOutcome(r.outcome)
		if err != nil {
			return domain.Drill{}, errs.Internal("drill_outcome_invalid", err)
		}
		d.Outcome = &res
	}
	return d, nil
}

// DrillRepository is the SQL over `delivery_drills` and the artefacts a drill
// reports on.
type DrillRepository struct{ q db.Querier }

// NewDrillRepository builds the repository over a fallback querier.
func NewDrillRepository(q db.Querier) *DrillRepository { return &DrillRepository{q: q} }

func (r *DrillRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// ------------------------------------------------------------------- writes

const insertDrillSQL = `
INSERT INTO delivery_drills (id, org_id, source_id, drill_label, severity, status,
                             started_by, started_by_label, started_at, deadline_at)
VALUES ($1, $2, $3, $4, $5, 'running', $6, $7, $8, $9)
RETURNING ` + drillColumns

// Create mints a drill row BEFORE anything is pushed.
//
// ⭐ THE ORDER MATTERS. The row exists first so that a process that dies between
// minting and accepting leaves a visible, reapable drill rather than an
// unattributable synthetic alert nobody can find. The `oto_drill` label is the
// row's own id, so the artefacts a half-finished drill created are still
// discoverable from it.
func (r *DrillRepository) Create(
	ctx context.Context, s db.TenantScope, in domain.NewDrill,
) (domain.Drill, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Drill{}, err
	}
	if in.ID == uuid.Nil || in.SourceID == uuid.Nil {
		return domain.Drill{}, errs.Internal("drill_incomplete",
			errs.New(errs.KindInternal, "missing_field", "a drill needs an id and a source"))
	}
	var row drillRow
	err := r.db(ctx).QueryRow(ctx, insertDrillSQL,
		in.ID, s.OrgID(), in.SourceID, in.ID.String(), in.Severity,
		nilID(in.StartedBy), in.StartedByLabel, in.StartedAt.UTC(), in.DeadlineAt.UTC(),
	).Scan(row.scanDest()...)
	if err != nil {
		return domain.Drill{}, mapErr(err, "create delivery drill")
	}
	return row.toDomain()
}

// SetBatch records the batch the accept produced.
func (r *DrillRepository) SetBatch(
	ctx context.Context, s db.TenantScope, id, batchID uuid.UUID,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	_, err := r.db(ctx).Exec(ctx,
		`UPDATE delivery_drills SET batch_id = $3, updated_at = now()
          WHERE org_id = $1 AND id = $2`, s.OrgID(), id, nilID(batchID))
	if err != nil {
		return mapErr(err, "record the drill's batch")
	}
	return nil
}

// RecordArtefacts caches the ids the stages discovered.
//
// ⭐ THEY ARE A DISPOSAL MANIFEST, NOT A CACHE OF THE ANSWER. The staged result is
// always recomputed from the live rows while a drill is running; these columns
// exist so that disposal can find what to delete without re-running a containment
// search over `alerts` — and so it still can after the label index has been
// vacuumed away with the rows.
//
// Every id is written with COALESCE so a later poll that saw less than an earlier
// one cannot unset something already found.
func (r *DrillRepository) RecordArtefacts(
	ctx context.Context, s db.TenantScope, id uuid.UUID, a domain.Artifacts,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	_, err := r.db(ctx).Exec(ctx, `
UPDATE delivery_drills
   SET alert_id        = COALESCE(alert_id, $3),
       case_id   = COALESCE(case_id, $4),
       group_id        = COALESCE(group_id, $5),
       notification_id = COALESCE(notification_id, $6),
       updated_at      = now()
 WHERE org_id = $1 AND id = $2`,
		s.OrgID(), id, nilID(a.Alert.ID), nilID(a.Case.ID),
		nilID(a.Group.ID), nilID(a.Notification.ID))
	if err != nil {
		return mapErr(err, "record the drill's artefacts")
	}
	return nil
}

// Freeze writes the terminal verdict exactly once.
//
// ⭐ THE `finished_at IS NULL` PREDICATE IS THE WHOLE CONCURRENCY STORY. Two
// browser tabs polling the same drill at the same moment both compute a terminal
// result; the first write wins and the second affects no rows, so a settled
// verdict can never be rewritten by a later, differently-timed observation.
func (r *DrillRepository) Freeze(
	ctx context.Context, s db.TenantScope, id uuid.UUID, res domain.Result, at time.Time,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	body, err := encodeOutcome(res)
	if err != nil {
		return errs.Internal("drill_outcome_encode_failed", err)
	}
	var failed *string
	if res.FailedStage != "" {
		v := res.FailedStage.String()
		failed = &v
	}
	_, err = r.db(ctx).Exec(ctx, `
UPDATE delivery_drills
   SET status = $3, outcome = $4, failed_stage = $5, finished_at = $6, updated_at = now()
 WHERE org_id = $1 AND id = $2 AND finished_at IS NULL`,
		s.OrgID(), id, res.Status.String(), body, failed, at.UTC())
	if err != nil {
		return mapErr(err, "freeze the drill verdict")
	}
	return nil
}

// ------------------------------------------------------------------- reads

// Get returns one drill.
func (r *DrillRepository) Get(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) (domain.Drill, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Drill{}, err
	}
	var row drillRow
	err := r.db(ctx).QueryRow(ctx,
		`SELECT `+drillColumns+` FROM delivery_drills WHERE org_id = $1 AND id = $2`,
		s.OrgID(), id).Scan(row.scanDest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Drill{}, errs.NotFound("drill_not_found", "no such delivery drill")
	}
	if err != nil {
		return domain.Drill{}, mapErr(err, "read delivery drill")
	}
	return row.toDomain()
}

// ListForSource returns a source's drills, newest first.
func (r *DrillRepository) ListForSource(
	ctx context.Context, s db.TenantScope, sourceID uuid.UUID, limit int,
) ([]domain.Drill, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := r.db(ctx).Query(ctx,
		`SELECT `+drillColumns+` FROM delivery_drills
          WHERE org_id = $1 AND source_id = $2
          ORDER BY started_at DESC, id DESC LIMIT $3`, s.OrgID(), sourceID, limit)
	if err != nil {
		return nil, mapErr(err, "list delivery drills")
	}
	defer rows.Close()
	return collectDrills(rows)
}

// Unfinished returns the drills that have not settled, oldest deadline first.
// It is what `retention.prune` uses to close out a drill nobody polled.
func (r *DrillRepository) Unfinished(
	ctx context.Context, s db.TenantScope, limit int,
) ([]domain.Drill, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx,
		`SELECT `+drillColumns+` FROM delivery_drills
          WHERE org_id = $1 AND finished_at IS NULL
          ORDER BY deadline_at ASC LIMIT $2`, s.OrgID(), limit)
	if err != nil {
		return nil, mapErr(err, "list unfinished delivery drills")
	}
	defer rows.Close()
	return collectDrills(rows)
}

// Disposable returns settled, undisposed drills older than `before`.
func (r *DrillRepository) Disposable(
	ctx context.Context, s db.TenantScope, before time.Time, limit int,
) ([]domain.Drill, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx,
		`SELECT `+drillColumns+` FROM delivery_drills
          WHERE org_id = $1 AND finished_at IS NOT NULL AND disposed_at IS NULL
            AND finished_at < $2
          ORDER BY finished_at ASC LIMIT $3`, s.OrgID(), before.UTC(), limit)
	if err != nil {
		return nil, mapErr(err, "list disposable delivery drills")
	}
	defer rows.Close()
	return collectDrills(rows)
}

func collectDrills(rows pgx.Rows) ([]domain.Drill, error) {
	out := make([]domain.Drill, 0, 16)
	for rows.Next() {
		var row drillRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "scan delivery drill")
		}
		d, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read delivery drills")
	}
	return out, nil
}

func idOrNil(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return uuid.Nil
	}
	return *p
}

func nilID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func timeOrZero(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.UTC()
}

// mapErr turns a database error into an errs.Kind for this package. The §L.9
// table itself lives in `db.MapError` and is shared by every repository — this
// module contributes only the two codes it alone can name.
//
// It previously translated no SQLSTATE at all: everything that was not
// `pgx.ErrNoRows` became `drill_query_failed` with `KindInternal`. A drill has no
// unique key a caller can collide with, which is why that looked sufficient — but
// a `57014` is not about the caller's key. Every pool sets `statement_timeout`,
// and §L.9 says a query cancelled by it is 503 with a retry, not a 500.
//
// ⛔ `ComputedKeys` IS WHY THIS IS STILL NOT A 409. `delivery_drills` has exactly
// one unique key, its `id` PRIMARY KEY, and the service mints it with `id.New()`
// immediately before the INSERT — so a `23505` reaching Go is a uuid collision or
// a statement that drifted from the schema, which is §L.9 row 2's oto bug and
// never something a caller can fix by changing the request. Its foreign keys are
// the same shape: `org_id`, `source_id` and `started_by` are all resolved and
// checked before the row is written. Without this flag the shared table would
// have turned both into a 409 that nobody asked for and that no caller could act
// on; with it, every key violation stays the 500 it was before this ticket.
//
// A `40001`/`40P01` DOES become a 409 where it used to be a 500, and that one is
// deliberate. §L.1 defines KindConflict as "the caller must re-read and retry",
// which is precisely what a serialization failure asks of the two browser tabs
// racing on `Freeze`; eight of the ten copies of this table already said so, and
// this module simply never wrote the row. Drills are polled over HTTP rather than
// run from a job, so no `jobs.Classify` behaviour is involved — and in any case
// KindConflict and KindInternal are the same ClassRetryable there.
func mapErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "drill_not_found",
		NotFoundMessage:    "no such delivery drill",
		QueryFailed:        "drill_query_failed",
		QueryFailedMessage: "could not " + what,
		ComputedKeys:       true,
	})
}
