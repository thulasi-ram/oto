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

// cacheRow is the row model of `enrichment_cache`. Unexported, per the
// three-model rule: it never leaves this package.
type cacheRow struct {
	key        string
	orgID      uuid.UUID
	payload    []byte
	computedAt time.Time
	expiresAt  time.Time
}

func (r *cacheRow) scanDest() []any {
	return []any{&r.key, &r.orgID, &r.payload, &r.computedAt, &r.expiresAt}
}

func (r *cacheRow) toDomain() (domain.CacheEntry, error) {
	return domain.NewCacheEntry(domain.CacheEntryParams{
		Key:        r.key,
		OrgID:      r.orgID.String(),
		Payload:    r.payload,
		ComputedAt: r.computedAt,
		ExpiresAt:  r.expiresAt,
	})
}

// Get returns a live entry. A miss and an expired entry are the same answer.
func (r *CacheRepository) Get(ctx context.Context, s db.TenantScope, key string) (domain.CacheEntry, bool, error) {
	// This guard stays in the repository even though NewCacheEntry now owns
	// enrichment_cache_key_ck: `key` here is a caller's LOOKUP string, which never
	// becomes a CacheEntry. A key the column cannot hold cannot be in the table,
	// so it is a miss, and answering it without a round trip is the point.
	if key == "" || len(key) > domain.MaxCacheKeyBytes {
		return domain.CacheEntry{}, false, nil
	}

	var row cacheRow
	err := r.db(ctx).QueryRow(ctx, getCacheSQL, key, s.OrgID()).Scan(row.scanDest()...)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.CacheEntry{}, false, nil
	case err != nil:
		return domain.CacheEntry{}, false, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not read the enrichment cache")
	}

	e, err := row.toDomain()
	if err != nil {
		return domain.CacheEntry{}, false, errs.Internal("enrichment_cache_row_invalid", err)
	}
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
//
// It re-checks nothing: enrichment_cache_key_ck and enrichment_cache_exp_ck are
// proven by NewCacheEntry, and a CacheEntry that reached this method is one the
// constructor already vouched for.
func (r *CacheRepository) Put(ctx context.Context, s db.TenantScope, e domain.CacheEntry) error {
	if _, err := r.db(ctx).Exec(ctx, putCacheSQL,
		e.Key(), s.OrgID(), e.Payload(), e.ComputedAt(), e.ExpiresAt()); err != nil {
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
