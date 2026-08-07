package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// CacheRepository is the SQL over `enrichment_cache`.
//
// This table is DISPOSABLE. It has no foreign key, participates in no cascade,
// and may be truncated at any moment with no loss of meaning — the provenanced
// record lives in `enrichments`. Everything here is written on that assumption:
// a failure to read or write the cache is logged by the caller and is never an
// error the pipeline acts on.
type CacheRepository struct {
	q db.Querier
}

// NewCacheRepository builds the repository over a fallback querier.
func NewCacheRepository(q db.Querier) *CacheRepository { return &CacheRepository{q: q} }

func (r *CacheRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// getCacheSQL reads one entry.
//
// The org_id predicate is REDUNDANT against the primary key — the key already
// embeds the org (domain.CacheKey) — and it is here anyway. A cache is exactly
// the kind of table where a subtle key-collision bug becomes a cross-tenant
// read, and a redundant predicate costs nothing to enforce that it cannot.
const getCacheSQL = `
SELECT cache_key, org_id, payload, computed_at, expires_at
  FROM enrichment_cache
 WHERE cache_key = $1 AND org_id = $2 AND expires_at > now()`

// Get returns a live entry. A miss and an expired entry are the same answer.
func (r *CacheRepository) Get(ctx context.Context, s db.TenantScope, key string) (domain.CacheEntry, bool, error) {
	if key == "" || len(key) > domain.MaxCacheKeyBytes {
		return domain.CacheEntry{}, false, nil
	}

	var (
		e     domain.CacheEntry
		orgID uuid.UUID
	)
	err := r.db(ctx).QueryRow(ctx, getCacheSQL, key, s.OrgID()).
		Scan(&e.Key, &orgID, &e.Payload, &e.ComputedAt, &e.ExpiresAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.CacheEntry{}, false, nil
	case err != nil:
		return domain.CacheEntry{}, false, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not read the enrichment cache")
	}
	e.OrgID = orgID.String()
	e.ComputedAt = e.ComputedAt.UTC()
	e.ExpiresAt = e.ExpiresAt.UTC()
	return e, true, nil
}

const putCacheSQL = `
INSERT INTO enrichment_cache (cache_key, org_id, payload, computed_at, expires_at)
VALUES ($1,$2,$3::jsonb,$4,$5)
ON CONFLICT (cache_key) DO UPDATE SET
  org_id      = EXCLUDED.org_id,
  payload     = EXCLUDED.payload,
  computed_at = EXCLUDED.computed_at,
  expires_at  = EXCLUDED.expires_at`

// Put writes an entry, overwriting any existing one for the key.
func (r *CacheRepository) Put(ctx context.Context, s db.TenantScope, e domain.CacheEntry) error {
	switch {
	case e.Key == "" || len(e.Key) > domain.MaxCacheKeyBytes:
		return errs.New(errs.KindValidation, "enrichment_bad_cache_key",
			"a cache key must be 1..512 bytes")
	case !e.ExpiresAt.After(e.ComputedAt):
		// enrichment_cache_exp_ck. An entry that expires before it was computed
		// is a clock bug, and storing it would make the constraint the thing
		// that reports it.
		return errs.New(errs.KindValidation, "enrichment_bad_cache_expiry",
			"expires_at must be strictly after computed_at")
	}
	payload := e.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}

	if _, err := r.db(ctx).Exec(ctx, putCacheSQL,
		e.Key, s.OrgID(), payload, e.ComputedAt.UTC(), e.ExpiresAt.UTC()); err != nil {
		return errs.Wrap(err, errs.KindInternal, CodeWriteFailed,
			"could not write the enrichment cache")
	}
	return nil
}

// deleteExpiredSQL evicts a bounded batch of dead entries.
//
// It is bounded because an unbounded DELETE on a table that can hold millions
// of rows takes a long lock and blocks the pipeline that depends on it. The
// `cache.expire` job runs every 600 s and will catch up; a maintenance sweep
// that causes an incident is worse than a slightly larger cache.
const deleteExpiredSQL = `
DELETE FROM enrichment_cache
 WHERE cache_key IN (
   SELECT cache_key FROM enrichment_cache WHERE expires_at <= $1 ORDER BY expires_at LIMIT $2)`

// DeleteExpired evicts entries whose expiry has passed.
//
// It takes no TenantScope on purpose: this is the global `cache.expire` sweep
// of SPEC §G.3, running under no principal over a table with no owner.
func (r *CacheRepository) DeleteExpired(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 10000
	}
	tag, err := r.db(ctx).Exec(ctx, deleteExpiredSQL, before.UTC(), limit)
	if err != nil {
		return 0, errs.Wrap(err, errs.KindInternal, CodeWriteFailed,
			"could not evict expired enrichment cache entries")
	}
	return tag.RowsAffected(), nil
}
