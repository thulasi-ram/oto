package repository

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// clusterRow is the row model of `clusters`.
type clusterRow struct {
	id          uuid.UUID
	orgID       uuid.UUID
	key         string
	displayName string
	sourceCount int64
	createdAt   time.Time
	updatedAt   time.Time
	deletedAt   *time.Time
}

func (r *clusterRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.key, &r.displayName, &r.sourceCount,
		&r.createdAt, &r.updatedAt, &r.deletedAt,
	}
}

func (r *clusterRow) toDomain() domain.Cluster {
	return domain.Cluster{
		ID:          r.id,
		OrgID:       r.orgID,
		Key:         r.key,
		DisplayName: r.displayName,
		SourceCount: int(r.sourceCount),
		CreatedAt:   r.createdAt,
		UpdatedAt:   r.updatedAt,
		DeletedAt:   r.deletedAt,
	}
}

// clusterColumns carries the live-source count as a correlated subquery rather
// than a GROUP BY join, so a cluster with no sources still appears.
const clusterColumns = `
	c.id, c.org_id, c.cluster_key, c.display_name,
	(SELECT count(*) FROM alert_sources s
	  WHERE s.cluster_id = c.id AND s.org_id = c.org_id AND s.deleted_at IS NULL),
	c.created_at, c.updated_at, c.deleted_at`

// ClusterRepository is the SQL over `clusters`.
type ClusterRepository struct {
	q     db.Querier
	clock clock.Clock
}

// NewClusterRepository builds the repository over a fallback querier.
func NewClusterRepository(q db.Querier, clk clock.Clock) *ClusterRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &ClusterRepository{q: q, clock: clk}
}

func (r *ClusterRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const getClusterSQL = `SELECT ` + clusterColumns + ` FROM clusters c WHERE c.org_id = $1 AND c.id = $2`

// Get reads one cluster.
func (r *ClusterRepository) Get(ctx context.Context, s db.TenantScope, clusterID uuid.UUID) (domain.Cluster, error) {
	if err := requireScope(s); err != nil {
		return domain.Cluster{}, err
	}
	var row clusterRow
	if err := r.db(ctx).QueryRow(ctx, getClusterSQL, s.OrgID(), clusterID).Scan(row.scanDest()...); err != nil {
		if isNoRows(err) {
			return domain.Cluster{}, errs.NotFound("cluster_not_found", "no such cluster")
		}
		return domain.Cluster{}, mapErr(err, "cluster_not_found", "read a cluster")
	}
	return row.toDomain(), nil
}

const getClusterByKeySQL = `SELECT ` + clusterColumns + `
  FROM clusters c WHERE c.org_id = $1 AND c.cluster_key = $2`

// GetByKey reads one cluster by the key that participates in alert identity.
func (r *ClusterRepository) GetByKey(ctx context.Context, s db.TenantScope, key string) (domain.Cluster, error) {
	if err := requireScope(s); err != nil {
		return domain.Cluster{}, err
	}
	var row clusterRow
	if err := r.db(ctx).QueryRow(ctx, getClusterByKeySQL, s.OrgID(), key).Scan(row.scanDest()...); err != nil {
		if isNoRows(err) {
			return domain.Cluster{}, errs.NotFound("cluster_not_found", "no such cluster")
		}
		return domain.Cluster{}, mapErr(err, "cluster_not_found", "read a cluster")
	}
	return row.toDomain(), nil
}

const listClustersSQL = `SELECT ` + clusterColumns + `
  FROM clusters c
 WHERE c.org_id = $1
   AND ($2 OR c.deleted_at IS NULL)
   AND ($3::timestamptz IS NULL OR (c.created_at, c.id) < ($3, $4))
 ORDER BY c.created_at DESC, c.id DESC
 LIMIT $5`

// List returns a keyset page of clusters, newest first.
func (r *ClusterRepository) List(
	ctx context.Context, s db.TenantScope, includeDeleted bool, p db.Keyset,
) ([]domain.Cluster, db.Cursor, error) {
	if err := requireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := clampLimit(p.Limit)

	var (
		afterAt *time.Time
		afterID uuid.UUID
	)
	if !p.Cursor.IsZero() {
		at := p.Cursor.SortKey.UTC()
		afterAt, afterID = &at, p.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, listClustersSQL, s.OrgID(), includeDeleted, afterAt, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "cluster_not_found", "list clusters")
	}
	defer rows.Close()

	out := make([]domain.Cluster, 0, limit+1)
	for rows.Next() {
		var row clusterRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, db.Cursor{}, mapErr(err, "cluster_not_found", "scan a cluster")
		}
		out = append(out, row.toDomain())
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "cluster_not_found", "read clusters")
	}

	page, hasMore := pageOf(out, limit)
	cur := db.Cursor{Hash: p.Cursor.Hash}
	if len(page) > 0 {
		last := page[len(page)-1]
		cur = nextCursor(last.CreatedAt, last.ID, p.Cursor.Hash, hasMore)
	}
	return page, cur, nil
}

const insertClusterSQL = `
INSERT INTO clusters (id, org_id, cluster_key, display_name, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $5)
RETURNING id`

// Create adds an identity/failure domain.
//
// ⭐ THE ID IS SUPPLIED BY THE CALLER AND NO LONGER MINTED HERE. `sources/service`
// records that id in the `Idempotency-Key` claim it takes in this same
// transaction, and a claim can only name a row whose id existed BEFORE the insert
// — otherwise the retry it is meant to replay hits `clusters_key_uniq` first and
// is answered with a name conflict, which is the defect ticket a6cc834 describes.
// A zero id still mints one, so a caller with no claim to record needs to know
// nothing about this.
func (r *ClusterRepository) Create(
	ctx context.Context, s db.TenantScope, clusterID uuid.UUID, key, displayName string,
) (domain.Cluster, error) {
	if err := requireScope(s); err != nil {
		return domain.Cluster{}, err
	}
	if strings.TrimSpace(key) == "" || strings.TrimSpace(displayName) == "" {
		return domain.Cluster{}, errs.Internal("cluster_incomplete",
			errsMissing("a cluster needs a key and a display name"))
	}
	if clusterID == uuid.Nil {
		clusterID = id.New()
	}

	var stored uuid.UUID
	err := r.db(ctx).QueryRow(ctx, insertClusterSQL,
		clusterID, s.OrgID(), key, displayName, r.clock.Now().UTC()).Scan(&stored)
	if err != nil {
		return domain.Cluster{}, mapErr(err, "cluster_not_found", "create a cluster")
	}
	return r.Get(ctx, s, stored)
}

// ⭐ GREATEST KEEPS `updated_at` MONOTONIC, and that is a correctness guard, not
// a nicety. Both of this row's timestamps come from the application — Create
// above names them from the injected clock — but "the application" is N pods
// with N clocks, and the pod serving a rename is rarely the pod that registered
// the cluster. A few milliseconds of lag between them would otherwise write an
// `updated_at` BELOW `created_at` and fail `clusters_time_ck` with a 23514 — a
// 500 on an ordinary rename, with nothing wrong. GREATEST makes the check
// unfalsifiable while leaving the value app-owned; it is the same idiom, for the
// same reason, as `channels`, `orgs` and OrderingStore.Advance.
const updateClusterSQL = `
UPDATE clusters SET display_name = $3, updated_at = GREATEST(updated_at, $4)
 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
RETURNING id`

// UpdateDisplayName renames a cluster.
//
// ⛔ THERE IS NO METHOD THAT CHANGES `cluster_key`, AND THERE MUST NOT BE. The
// key is hashed into `alert_key` (§C.2): changing it would re-key every alert
// identity in the cluster and silently fork the history of everything in it. The
// display name is the renameable half precisely because it is never hashed.
func (r *ClusterRepository) UpdateDisplayName(
	ctx context.Context, s db.TenantScope, clusterID uuid.UUID, displayName string,
) (domain.Cluster, error) {
	if err := requireScope(s); err != nil {
		return domain.Cluster{}, err
	}
	if strings.TrimSpace(displayName) == "" {
		return domain.Cluster{}, errs.Internal("cluster_incomplete", errsMissing("a display name is required"))
	}

	var stored uuid.UUID
	err := r.db(ctx).QueryRow(ctx, updateClusterSQL,
		s.OrgID(), clusterID, displayName, r.clock.Now().UTC()).Scan(&stored)
	if err != nil {
		if isNoRows(err) {
			return domain.Cluster{}, errs.NotFound("cluster_not_found", "no such cluster")
		}
		return domain.Cluster{}, mapErr(err, "cluster_not_found", "update a cluster")
	}
	return r.Get(ctx, s, stored)
}

// ClusterKeysFor resolves many cluster ids to their keys in ONE round trip, for
// the sources list which renders `cluster_key` beside every row.
func (r *ClusterRepository) ClusterKeysFor(
	ctx context.Context, s db.TenantScope, clusterIDs []uuid.UUID,
) (map[uuid.UUID]string, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	if len(clusterIDs) == 0 {
		return map[uuid.UUID]string{}, nil
	}

	rows, err := r.db(ctx).Query(ctx,
		`SELECT id, cluster_key FROM clusters WHERE org_id = $1 AND id = ANY($2)`,
		s.OrgID(), clusterIDs)
	if err != nil {
		return nil, mapErr(err, "cluster_not_found", "read cluster keys")
	}
	defer rows.Close()

	out := make(map[uuid.UUID]string, len(clusterIDs))
	for rows.Next() {
		var (
			cid uuid.UUID
			key string
		)
		if err := rows.Scan(&cid, &key); err != nil {
			return nil, mapErr(err, "cluster_not_found", "scan a cluster key")
		}
		out[cid] = key
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "cluster_not_found", "read cluster keys")
	}
	return out, nil
}
