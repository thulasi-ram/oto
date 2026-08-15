package idempotency

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Repository is the SQL over `idempotency_claims` (00041).
//
// The table is DELIBERATELY UNPARTITIONED, for the reason `ingest_dedup` is
// (conflict ruling 14): a UNIQUE index on a partitioned table must include the
// partition key, so it can only enforce uniqueness WITHIN a partition — and a
// client-supplied key has no partition key it could carry, while its uniqueness
// must hold across all of time or a retry that straddled midnight would mint the
// second credential this table exists to prevent. A small, aggressively pruned
// side table is the price of that guarantee, and `retention.prune` is what keeps
// it small.
type Repository struct {
	q db.Querier
}

// NewRepository builds the repository over the general pool.
func NewRepository(q db.Querier) *Repository { return &Repository{q: q} }

func (r *Repository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// insertClaimSQL takes the key if nobody holds it.
//
// `ON CONFLICT DO NOTHING` is not error handling: a key already held is the
// mechanism working, and the caller is told which of the three outcomes it is
// rather than being handed a 23505.
const insertClaimSQL = `
INSERT INTO idempotency_claims
       (org_id, principal_id, operation, idempotency_key, request_hash, created_ref, claimed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (org_id, principal_id, operation, idempotency_key) DO NOTHING`

// selectClaimSQL reads the incumbent — the claim that beat us to the key.
const selectClaimSQL = `
SELECT request_hash, created_ref, claimed_at
  FROM idempotency_claims
 WHERE org_id = $1 AND principal_id = $2 AND operation = $3 AND idempotency_key = $4`

// Claim takes the key for this request, or reports who already holds it.
//
// ⭐⭐ IT MUST RUN INSIDE THE CALLER'S TRANSACTION, and it refuses to run outside
// one. The mint and the claim are one unit of work or they are a bug: a mint that
// commits beside a claim that does not is precisely the orphaned live credential
// this whole mechanism exists to prevent. Transactions travel in the context
// (`db.FromContext`), so a caller wraps `db.Tx` around "mint, then claim" and a
// non-`Claimed` outcome rolls the mint back with it. Refusing here means that
// invariant cannot be forgotten at a call site.
//
// ⭐⭐ IT IS TWO STATEMENTS, NOT ONE CTE, AND THAT IS THE WHOLE CORRECTNESS
// ARGUMENT. The obvious shape is `ingest_dedup`'s: one statement whose CTE
// inserts and whose outer SELECT reads back the winner. It cannot be used here.
// A single statement runs under ONE snapshot, so when two requests race, the
// loser's INSERT waits on the winner's uncommitted row, and then its outer SELECT
// — pinned to a snapshot taken BEFORE the winner committed — cannot see the row
// it just lost to and returns NOTHING. `ingest_dedup` treats that emptiness as "we
// won" and records a duplicate batch, which is harmless there. Here "we won"
// means MINT A SECOND CREDENTIAL, so the read must be a separate statement: under
// READ COMMITTED each statement takes a fresh snapshot, so the second one sees
// exactly the row the first one lost to.
//
// ⛔ AND IT FAILS CLOSED. If the insert conflicted and the read then finds
// nothing — the incumbent was pruned in the microseconds between them — this
// returns an error rather than guessing. A wrong error costs the caller one
// retry, which succeeds; a wrong "you won" costs a live credential nobody knows
// about.
func (r *Repository) Claim(ctx context.Context, s db.TenantScope, c Claim) (Result, error) {
	if c.OrgID != s.OrgID() {
		// The scope is the authority; a row claiming a different org is a service
		// bug and must never reach the driver.
		return Result{}, errs.Internal("idempotency_scope_mismatch", nil)
	}
	if err := c.validate(); err != nil {
		return Result{}, err
	}
	if !db.InTx(ctx) {
		return Result{}, errs.Internal("idempotency_claim_outside_tx", errNoTx)
	}

	q := r.db(ctx)
	tag, err := q.Exec(ctx, insertClaimSQL,
		c.OrgID, c.PrincipalID, c.Operation.String(), c.Key.String(),
		c.RequestHash.Bytes(), nullableID(c.CreatedRef), c.ClaimedAt.UTC())
	if err != nil {
		return Result{}, mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		claimed := c
		claimed.ClaimedAt = c.ClaimedAt.UTC()
		return Result{Outcome: Claimed, Existing: claimed}, nil
	}

	incumbent := c
	var (
		hash []byte
		ref  *uuid.UUID
		at   time.Time
	)
	err = q.QueryRow(ctx, selectClaimSQL,
		c.OrgID, c.PrincipalID, c.Operation.String(), c.Key.String()).Scan(&hash, &ref, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, errs.Internal("idempotency_claim_vanished", errVanished)
	}
	if err != nil {
		return Result{}, mapErr(err)
	}

	stored, err := NewRequestHash(hash)
	if err != nil {
		return Result{}, errs.Internal("idempotency_claim_invalid", err)
	}
	incumbent.RequestHash = stored
	incumbent.CreatedRef = derefID(ref)
	incumbent.ClaimedAt = at.UTC()

	if stored != c.RequestHash {
		return Result{Outcome: Conflicted, Existing: incumbent}, nil
	}
	return Result{Outcome: Replayed, Existing: incumbent}, nil
}

const pruneClaimsSQL = `DELETE FROM idempotency_claims WHERE claimed_at < $1`

// Prune deletes claims past the horizon (RetentionWindow).
//
// The horizon is passed in rather than computed as `now() - interval '24 hours'`
// so the clock stays injectable — a sweeper whose window only exists in SQL is a
// sweeper no test can pin. It takes no TenantScope for the same reason
// `DedupRepository.Prune` does not: it is a housekeeping sweep over every tenant,
// run by `retention.prune` and reachable from no request.
func (r *Repository) Prune(ctx context.Context, before time.Time) (int64, error) {
	tag, err := r.db(ctx).Exec(ctx, pruneClaimsSQL, before.UTC())
	if err != nil {
		return 0, mapErr(err)
	}
	return tag.RowsAffected(), nil
}

var (
	errNoTx = errors.New(
		"a claim must run inside the caller's transaction, or the mint it guards can commit without it")
	errVanished = errors.New(
		"the key was held at insert and gone at read; the claim was pruned mid-request")
)

func nullableID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func derefID(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return uuid.Nil
	}
	return *p
}

// mapErr turns a database error into an errs.Kind for this package. The §L.9
// table itself lives in `db.MapError` and is shared by every repository, with the
// CONSTRAINT NAME as the machine code because those names are a runtime contract
// (CONTEXT.md §6).
//
// ⚠️ A 23505 IS DELIBERATELY INTERNAL HERE, which is what `ComputedKeys` says.
// Every unique violation this table can produce is swallowed by `ON CONFLICT DO
// NOTHING` above, so one reaching Go means the statement drifted from the
// constraint — which is §L.9 row 2's rule for an oto-swallowed key, and is never
// the caller's fault. The same holds for a `23503`: every row this table
// references, oto wrote itself.
//
// ⚠️ No message here may carry a pgx type, a row struct or a SQL string.
//
// The not-found codes are defensive: the one query that can return
// `pgx.ErrNoRows` answers it above with `idempotency_claim_vanished`, because a
// claim that disappeared between the INSERT and the SELECT is a bug and not a
// 404. The other two call sites are Execs, which never produce it.
func mapErr(err error) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "idempotency_not_found",
		NotFoundMessage:    "no such idempotency record",
		QueryFailed:        "idempotency_query_failed",
		QueryFailedMessage: "an internal error occurred",
		ComputedKeys:       true,
		Codes:              idempotencyCodes,
	})
}

// idempotencyCodes are the codes this package has always published where
// Postgres names no constraint. `idempotency_serialization_failure` is the one a
// caller can act on — two requests raced for the same key — and collapsing it to
// `sqlstate_40001` would break anything branching on it.
var idempotencyCodes = map[string]string{
	"23505": "idempotency_constraint_violation",
	"23503": "idempotency_constraint_violation",
	"23514": "idempotency_constraint_violation",
	"23502": "idempotency_constraint_violation",
	"40001": "idempotency_serialization_failure",
	"40P01": "idempotency_serialization_failure",
	"57014": "idempotency_query_timeout",
	"53300": "idempotency_overloaded",
}
