package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// CasePolicyConfigRepository is the SETTINGS side of `case_policy_config`: the
// list, the create, the patch and the delete an operator's screen needs.
//
// ⭐⭐ IT IS A SECOND REPOSITORY OVER ONE TABLE, ON PURPOSE. `CasePolicyRepository`
// in casepolicy.go is the READ the ingest path makes, and it has no writer because
// the evaluator must not be able to rewrite the rule it is evaluating — exactly as
// `notification/repository`'s PolicyRepository (read) is separate from its
// ConfigRepository (write). Merging the two would hand the §B.3 machine a way to
// edit W mid-episode, which is a class of bug no test would think to look for.
//
// ⛔ EVERY METHOD IS TENANT-SCOPED AND `org_id` IS ALWAYS IN THE PREDICATE, never
// only in the id. A `WHERE id = $1` here would let one org patch another's
// retention window by guessing a UUID; a missing row and another tenant's row are
// deliberately the same answer — `404` — because a `403` would confirm the row
// exists.
type CasePolicyConfigRepository struct {
	q     db.Querier
	clock clock.Clock
}

// NewCasePolicyConfigRepository builds the repository over a fallback querier.
func NewCasePolicyConfigRepository(q db.Querier, clk clock.Clock) *CasePolicyConfigRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &CasePolicyConfigRepository{q: q, clock: clk}
}

func (r *CasePolicyConfigRepository) db(ctx context.Context) db.Querier {
	return db.FromContext(ctx, r.q)
}

const casePolicyColumns = `
  id, org_id, namespace, alertname, retention_window_s, created_at, updated_at`

// casePolicyRow is the row model of `case_policy_config`. Unexported, per the
// three-model rule: no DTO and no domain type may embed it.
type casePolicyRow struct {
	id        uuid.UUID
	orgID     uuid.UUID
	namespace string
	alertname string
	windowSec int32
	createdAt time.Time
	updatedAt time.Time
}

// scanInto is the ONE argument list for `casePolicyColumns`, beside the constant
// it belongs to, so the two cannot drift as a column is added.
func (r *casePolicyRow) scanInto() []any {
	return []any{
		&r.id, &r.orgID, &r.namespace, &r.alertname,
		&r.windowSec, &r.createdAt, &r.updatedAt,
	}
}

func (r casePolicyRow) toDomain() domain.CasePolicy {
	return domain.CasePolicy{
		ID:              r.id,
		Namespace:       r.namespace,
		Alertname:       r.alertname,
		RetentionWindow: time.Duration(r.windowSec) * time.Second,
		CreatedAt:       r.createdAt,
		UpdatedAt:       r.updatedAt,
	}
}

// ⭐ THE PAGE IS ORDERED THE WAY AN OPERATOR READS IT — by alertname, then by
// namespace — which is what `case_policy_org_idx (org_id, alertname, namespace)`
// exists for and what its own DDL comment promises. Ordering by `created_at`
// instead would scatter the two rows for one alertname across the list.
//
// ⚠️ THE CURSOR CARRIES ONLY AN ID, AND THE COMPARISON IS A SUBQUERY. The generic
// db.Cursor holds a timestamp and a UUID, and this list keys on two TEXT columns,
// so there is nothing to pack a sort key into. Naming the last row and letting
// Postgres re-read its `(alertname, namespace, id)` tuple is exact, is one index
// probe on the primary key, and — because the subquery is itself scoped by
// `org_id` — cannot be steered at another tenant's row by forging a cursor.
const listCasePoliciesSQL = `
SELECT` + casePolicyColumns + `
  FROM case_policy_config
 WHERE org_id = $1
   AND ($2::uuid IS NULL
        OR (alertname, namespace, id) >
           (SELECT alertname, namespace, id FROM case_policy_config
             WHERE org_id = $1 AND id = $2))
 ORDER BY alertname ASC, namespace ASC, id ASC
 LIMIT $3`

// ListCasePolicies returns a keyset page of one org's retention windows.
func (r *CasePolicyConfigRepository) ListCasePolicies(
	ctx context.Context, s db.TenantScope, p db.Keyset,
) ([]domain.CasePolicy, db.Cursor, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 25
	}

	var after *uuid.UUID
	if !p.Cursor.IsZero() && p.Cursor.ID != uuid.Nil {
		id := p.Cursor.ID
		after = &id
	}

	rows, err := r.db(ctx).Query(ctx, listCasePoliciesSQL, s.OrgID(), after, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "list case retention policies")
	}
	defer rows.Close()

	out := make([]domain.CasePolicy, 0, limit+1)
	for rows.Next() {
		var row casePolicyRow
		if err := rows.Scan(row.scanInto()...); err != nil {
			return nil, db.Cursor{}, mapErr(err, "list case retention policies")
		}
		out = append(out, row.toDomain())
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "list case retention policies")
	}

	page, hasMore := db.PageOf(out, limit)
	cursor := db.Cursor{Hash: p.Cursor.Hash}
	if hasMore {
		// The sort key is deliberately zero: this list has no timestamp to key on
		// and the id alone names the position. See listCasePoliciesSQL.
		cursor = db.Cursor{ID: page[len(page)-1].ID, Hash: p.Cursor.Hash, HasMore: true}
	}
	return page, cursor, nil
}

const getCasePolicySQL = `
SELECT` + casePolicyColumns + `
  FROM case_policy_config WHERE org_id = $1 AND id = $2`

// GetCasePolicy reads one row.
//
// Another tenant's id answers `404`, because `org_id` is in the predicate rather
// than checked afterwards.
func (r *CasePolicyConfigRepository) GetCasePolicy(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID,
) (domain.CasePolicy, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.CasePolicy{}, err
	}
	var row casePolicyRow
	err := r.db(ctx).QueryRow(ctx, getCasePolicySQL, s.OrgID(), policyID).Scan(row.scanInto()...)
	if err != nil {
		if isNoRows(err) {
			return domain.CasePolicy{}, errs.NotFound("case_policy_not_found",
				"no such case retention policy")
		}
		return domain.CasePolicy{}, mapErr(err, "read a case retention policy")
	}
	return row.toDomain(), nil
}

const insertCasePolicySQL = `
INSERT INTO case_policy_config (
  id, org_id, namespace, alertname, retention_window_s, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$6)
RETURNING id`

// CreateCasePolicy writes one row.
//
// The domain's own Validate has already run in the service layer; this method
// re-proves nothing except that the row is well formed enough to reach the driver.
// The CHECK constraints are the backstop, never the error message.
//
// A second row for the same (namespace, alertname) meets `case_policy_axes_uniq`
// and comes back as a `409` naming that constraint, which is the answer: there is
// one window per pair, and the way to change it is a PATCH.
func (r *CasePolicyConfigRepository) CreateCasePolicy(
	ctx context.Context, s db.TenantScope, in domain.CasePolicyDraft,
) (domain.CasePolicy, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.CasePolicy{}, err
	}
	in = in.Normalised()
	if in.Alertname == "" {
		return domain.CasePolicy{}, errs.Internal("case_policy_alertname_missing",
			errsMissing("alertname is required"))
	}

	newID := in.ID
	if newID == uuid.Nil {
		newID = id.New()
	}

	var stored uuid.UUID
	err := r.db(ctx).QueryRow(ctx, insertCasePolicySQL,
		newID, s.OrgID(), in.Namespace, in.Alertname,
		windowSeconds(in.RetentionWindow), r.clock.Now().UTC(),
	).Scan(&stored)
	if err != nil {
		return domain.CasePolicy{}, mapErr(err, "create a case retention policy")
	}
	return r.GetCasePolicy(ctx, s, stored)
}

// `updated_at` is GREATEST-guarded for the reason every other app-clocked table's
// is: the timestamps are application-owned, "the application" is N pods with N
// clocks, and a few milliseconds of skew would otherwise write an `updated_at`
// below `created_at`.
const updateCasePolicySQL = `
UPDATE case_policy_config
   SET retention_window_s = COALESCE($3, retention_window_s),
       updated_at = GREATEST(updated_at, $4)
 WHERE org_id = $1 AND id = $2
RETURNING id`

// UpdateCasePolicy applies a partial change.
//
// ⛔ THE AXES ARE NOT UPDATABLE and this statement could not move them if it were
// asked: `domain.CasePolicyPatch` has no field for either. See the ⛔ note there.
func (r *CasePolicyConfigRepository) UpdateCasePolicy(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID, p domain.CasePolicyPatch,
) (domain.CasePolicy, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.CasePolicy{}, err
	}
	if p.IsEmpty() {
		return domain.CasePolicy{}, errs.Validation("empty_patch",
			"supply at least one field to change")
	}

	var window *int32
	if p.RetentionWindow != nil {
		v := windowSeconds(*p.RetentionWindow)
		window = &v
	}

	var stored uuid.UUID
	err := r.db(ctx).QueryRow(ctx, updateCasePolicySQL,
		s.OrgID(), policyID, window, r.clock.Now().UTC()).Scan(&stored)
	if err != nil {
		if isNoRows(err) {
			return domain.CasePolicy{}, errs.NotFound("case_policy_not_found",
				"no such case retention policy")
		}
		return domain.CasePolicy{}, mapErr(err, "update a case retention policy")
	}
	return r.GetCasePolicy(ctx, s, stored)
}

const deleteCasePolicySQL = `
DELETE FROM case_policy_config WHERE org_id = $1 AND id = $2`

// DeleteCasePolicy removes one row, which restores W=0 for that pair.
//
// ⭐ IT IS A HARD DELETE, AND THAT IS THE RIGHT KIND HERE. A soft delete exists to
// keep an audit trail something else still points at; nothing references
// `case_policy_config` — the §B.3 machine reads W and keeps no link to the row it
// read — so a tombstone would preserve nothing and would have to be excluded from
// `case_policy_axes_uniq`, which is the constraint doing the real work.
func (r *CasePolicyConfigRepository) DeleteCasePolicy(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx, deleteCasePolicySQL, s.OrgID(), policyID)
	if err != nil {
		return mapErr(err, "delete a case retention policy")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("case_policy_not_found", "no such case retention policy")
	}
	return nil
}

// windowSeconds renders W for the `INT` column. The domain has already refused a
// fractional or out-of-range duration; this is the unit conversion and nothing
// more, and it happens here and nowhere else.
func windowSeconds(d time.Duration) int32 {
	return int32(d / time.Second) //nolint:gosec // bounded by case_policy_window_ck
}
