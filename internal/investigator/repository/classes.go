package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ClassRepository is every statement against `investigation_classes` (migration
// 00096): the org's Classification set (ADR 0053 §5, git-bug 4298aa0).
//
// ⛔ NOTHING HERE TOUCHES `investigations`. A Finding copied the class NAME it was given
// onto its own row; replacing the set is a write to this table alone, so a renamed or
// removed class rewrites no Finding — by construction, not by care.
//
// ⛔ EVERY METHOD IS TENANT-SCOPED, AND `org_id` IS IN EVERY PREDICATE.
type ClassRepository struct {
	q db.Querier
}

// NewClassRepository builds the repository over a fallback querier; a transaction
// travelling in the context wins over it.
func NewClassRepository(q db.Querier) *ClassRepository { return &ClassRepository{q: q} }

func (r *ClassRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

func mapClassErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "investigation_classes_not_found",
		NotFoundMessage:    "no class set",
		QueryFailed:        "investigator_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
	})
}

// ClassSet reads the org's set in the operator's order, served by the primary key's
// org prefix and `investigation_classes_position_uniq`. No rows is the empty set.
func (r *ClassRepository) ClassSet(ctx context.Context, s db.TenantScope) (domain.ClassSet, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.ClassSet{}, err
	}
	rows, err := r.db(ctx).Query(ctx, `
SELECT name, description
  FROM investigation_classes
 WHERE org_id = $1
 ORDER BY position`, s.OrgID())
	if err != nil {
		return domain.ClassSet{}, mapClassErr(err, "read the class set")
	}
	defer rows.Close()
	classes := []domain.Class{}
	for rows.Next() {
		var c domain.Class
		if err := rows.Scan(&c.Name, &c.Description); err != nil {
			return domain.ClassSet{}, mapClassErr(err, "read the class set")
		}
		classes = append(classes, c)
	}
	if err := rows.Err(); err != nil {
		return domain.ClassSet{}, mapClassErr(err, "read the class set")
	}
	return domain.RestoreClassSet(classes)
}

// ReplaceClassSet writes the org's whole set: the old rows go and the new ones are
// written in the operator's order, in the caller's transaction.
//
// ⭐ SERIALISED ON THE ORG'S ADVISORY LOCK, so two operators saving at once cannot both
// delete the old set and then collide on the primary key: the second waits, and its set
// is the one that stands. Outside a transaction the lock would guard nothing, and
// db.AdvisoryXactLock refuses it.
func (r *ClassRepository) ReplaceClassSet(ctx context.Context, s db.TenantScope, set domain.ClassSet, at time.Time) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	key := db.AdvisoryKey(db.LockNamespaceInvestigations, "classes/"+s.OrgID().String())
	if err := db.AdvisoryXactLock(ctx, r.db(ctx), key); err != nil {
		return errs.Internal("investigation_classes_lock", err)
	}
	if _, err := r.db(ctx).Exec(ctx, `DELETE FROM investigation_classes WHERE org_id = $1`, s.OrgID()); err != nil {
		return mapClassErr(err, "replace the class set")
	}
	classes := set.Classes()
	if len(classes) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for i, c := range classes {
		batch.Queue(`INSERT INTO investigation_classes (org_id, name, description, position, created_at)
		             VALUES ($1, $2, $3, $4, $5)`, s.OrgID(), c.Name, c.Description, i, at.UTC())
	}
	if err := r.db(ctx).SendBatch(ctx, batch).Close(); err != nil {
		return mapClassErr(err, "store the class set")
	}
	return nil
}
