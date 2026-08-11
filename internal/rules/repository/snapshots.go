package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/rules/domain"
)

// Error codes this repository mints.
const (
	// CodeNotFound means no such snapshot in the caller's org.
	CodeNotFound = "rules_snapshot_not_found"
	// CodeQueryFailed means Postgres refused a read.
	CodeQueryFailed = "rules_query_failed"
	// CodeWriteFailed means Postgres refused a write.
	CodeWriteFailed = "rules_write_failed"
	// CodeBadID means an id in the domain object is not a UUID.
	CodeBadID = "rules_invalid_id"
)

// snapshotRow is the row model of `rule_snapshots`. Unexported, per the
// three-model rule: no DTO and no domain type may embed it.
type snapshotRow struct {
	id             uuid.UUID
	orgID          uuid.UUID
	sourceID       uuid.UUID
	fingerprint    string
	file           string
	group          string
	name           string
	expr           string
	forSeconds     float64
	keepFiringFor  float64
	labels         []byte
	annotations    []byte
	origin         string
	prometheusURL  *string
	confidence     string
	candidateCount int
	capturedAt     time.Time
}

func (r snapshotRow) toDomain() (domain.Snapshot, error) {
	labels, err := decodeMap(r.labels)
	if err != nil {
		return domain.Snapshot{}, err
	}
	annotations, err := decodeMap(r.annotations)
	if err != nil {
		return domain.Snapshot{}, err
	}
	promURL := ""
	if r.prometheusURL != nil {
		promURL = *r.prometheusURL
	}
	return domain.Snapshot{
		ID:    r.id.String(),
		OrgID: r.orgID.String(),
		Key: domain.Key{
			SourceID: r.sourceID.String(),
			File:     r.file,
			Group:    r.group,
			Name:     r.name,
		},
		Fingerprint:          r.fingerprint,
		Expr:                 r.expr,
		ForSeconds:           r.forSeconds,
		KeepFiringForSeconds: r.keepFiringFor,
		Labels:               labels,
		Annotations:          annotations,
		Origin:               domain.Origin(r.origin),
		PrometheusURL:        promURL,
		Confidence:           domain.Confidence(r.confidence),
		CandidateCount:       r.candidateCount,
		CapturedAt:           r.capturedAt.UTC(),
	}, nil
}

// snapshotColumns is the read projection, in the order every scan expects.
const snapshotColumns = `id, org_id, source_id, rule_fingerprint, rule_file, rule_group, rule_name,
       expr, for_seconds, keep_firing_for_seconds, rule_labels, rule_annotations,
       origin, prometheus_url, match_confidence, candidate_count, captured_at`

func scanSnapshot(row pgx.Row, extra ...any) (snapshotRow, error) {
	var r snapshotRow
	dest := []any{
		&r.id, &r.orgID, &r.sourceID, &r.fingerprint, &r.file, &r.group, &r.name,
		&r.expr, &r.forSeconds, &r.keepFiringFor, &r.labels, &r.annotations,
		&r.origin, &r.prometheusURL, &r.confidence, &r.candidateCount, &r.capturedAt,
	}
	dest = append(dest, extra...)
	err := row.Scan(dest...)
	return r, err
}

// SnapshotRepository is the SQL over `rule_snapshots`.
//
// The table is append-only and content-addressed, so this type has no Update
// and no Delete: a changed rule is a NEW ROW, and that is the whole mechanism
// behind the drift feature. Every method joins the caller's transaction through
// db.FromContext.
type SnapshotRepository struct {
	q db.Querier
}

// NewSnapshotRepository builds the repository over a fallback querier, normally
// the general pool.
func NewSnapshotRepository(q db.Querier) *SnapshotRepository { return &SnapshotRepository{q: q} }

func (r *SnapshotRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// upsertSnapshotSQL inserts a capture, or returns the row that already holds
// this content.
//
// The CTE is what makes "captured on every fire" cost one row: the INSERT is
// ON CONFLICT DO NOTHING against rule_snapshots_content_uniq, and the UNION arm
// reads the incumbent only when the insert produced nothing. The `inserted`
// column is how the service tells a NEW VERSION of a rule from the ten
// thousandth fire of an unchanged one, without a second round trip.
const upsertSnapshotSQL = `
WITH ins AS (
  INSERT INTO rule_snapshots (
    id, org_id, source_id, rule_fingerprint, rule_file, rule_group, rule_name,
    expr, for_seconds, keep_firing_for_seconds, rule_labels, rule_annotations,
    origin, prometheus_url, match_confidence, candidate_count, captured_at)
  VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12::jsonb,$13,$14,$15,$16,$17)
  ON CONFLICT (org_id, source_id, rule_fingerprint) DO NOTHING
  RETURNING ` + snapshotColumns + `, true AS inserted
)
SELECT * FROM ins
UNION ALL
SELECT ` + snapshotColumns + `, false AS inserted
  FROM rule_snapshots
 WHERE org_id = $2 AND source_id = $3 AND rule_fingerprint = $4
   AND NOT EXISTS (SELECT 1 FROM ins)
 LIMIT 1`

// Upsert stores one snapshot, returning the stored row and whether this call
// inserted it.
func (r *SnapshotRepository) Upsert(ctx context.Context, s db.TenantScope, snap domain.Snapshot) (domain.Snapshot, bool, error) {
	if err := snap.Validate(); err != nil {
		return domain.Snapshot{}, false, err
	}
	sourceID, err := uuid.Parse(snap.Key.SourceID)
	if err != nil {
		return domain.Snapshot{}, false, errs.New(errs.KindValidation, CodeBadID,
			"a rule snapshot's source id must be a UUID")
	}

	labels, err := encodeMap(snap.Labels)
	if err != nil {
		return domain.Snapshot{}, false, err
	}
	annotations, err := encodeMap(snap.Annotations)
	if err != nil {
		return domain.Snapshot{}, false, err
	}

	var promURL *string
	if snap.PrometheusURL != "" {
		v := snap.PrometheusURL
		promURL = &v
	}

	var inserted bool
	row, err := scanSnapshot(r.db(ctx).QueryRow(ctx, upsertSnapshotSQL,
		id.New(), s.OrgID(), sourceID, snap.Fingerprint,
		snap.Key.File, snap.Key.Group, snap.Key.Name,
		snap.Expr, snap.ForSeconds, snap.KeepFiringForSeconds, labels, annotations,
		string(snap.Origin), promURL, string(snap.Confidence), snap.CandidateCount,
		snap.CapturedAt,
	), &inserted)
	if err != nil {
		return domain.Snapshot{}, false, errs.Wrap(err, errs.KindInternal, CodeWriteFailed,
			"could not store the rule snapshot")
	}

	out, err := row.toDomain()
	if err != nil {
		return domain.Snapshot{}, false, err
	}
	return out, inserted, nil
}

const getSnapshotSQL = `
SELECT ` + snapshotColumns + `
  FROM rule_snapshots
 WHERE org_id = $1 AND id = $2`

// Get returns one snapshot by id.
func (r *SnapshotRepository) Get(ctx context.Context, s db.TenantScope, snapshotID uuid.UUID) (domain.Snapshot, error) {
	row, err := scanSnapshot(r.db(ctx).QueryRow(ctx, getSnapshotSQL, s.OrgID(), snapshotID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Snapshot{}, errs.NotFound(CodeNotFound, "no such rule snapshot")
	case err != nil:
		return domain.Snapshot{}, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not read the rule snapshot")
	}
	return row.toDomain()
}

const getSnapshotsByIDsSQL = `
SELECT ` + snapshotColumns + `
  FROM rule_snapshots
 WHERE org_id = $1 AND id = ANY($2)
 ORDER BY captured_at DESC, id DESC`

// GetMany reads many snapshots by id in ONE round trip.
//
// ⭐ IT IS WHAT LETS THE ALERT LIST SHOW WHAT THE RULE SAID. `include=rule` on
// `GET /alerts` carries the snapshot id and nothing more, so rendering `expr` on
// fifty rows used to mean fifty calls to `/alerts/{id}/rule`. Content addressing
// makes the batch cheaper than it looks: a page where every alert fired under
// the same unchanged rule collapses to ONE id, because the rows ARE the same row
// (ADR 0025).
//
// ⛔ An id with no row is ABSENT FROM THE RESULT, never an error. The table is
// append-only, so the only way to miss is an id from another org or one a caller
// invented — and failing the whole batch for one of those would blank an entire
// page's rule column. The caller joins by id and renders the misses as unknown.
//
// NOTE (planner): the predicate is the primary key with the org filter on top,
// so this is an index scan per id and never a table scan. The ordering is the
// same `(captured_at DESC, id DESC)` every other snapshot read uses, so a client
// that renders the batch as a list gets the order it already expects.
func (r *SnapshotRepository) GetMany(
	ctx context.Context, s db.TenantScope, ids []uuid.UUID,
) ([]domain.Snapshot, error) {
	if len(ids) == 0 {
		return []domain.Snapshot{}, nil
	}

	rows, err := r.db(ctx).Query(ctx, getSnapshotsByIDsSQL, s.OrgID(), ids)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not read the rule snapshots")
	}
	defer rows.Close()

	out := make([]domain.Snapshot, 0, len(ids))
	for rows.Next() {
		row, scanErr := scanSnapshot(rows)
		if scanErr != nil {
			return nil, errs.Wrap(scanErr, errs.KindInternal, CodeQueryFailed,
				"could not read the rule snapshots")
		}
		snap, convErr := row.toDomain()
		if convErr != nil {
			return nil, convErr
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not read the rule snapshots")
	}
	return out, nil
}

// keyPredicate builds the rule-key filter.
//
// rule_file and rule_group are matched ONLY when the caller supplied them. That
// is deliberate: a generatorURL capture knows the expression but not the file
// it is written in, so an empty component means "unknown", not "the empty
// string". Treating it as an equality would split one rule's history into two
// on the day Prometheus became reachable. The predicate stays a prefix of
// rule_snapshots_key_idx (org_id, source_id, rule_name, …) either way.
func keyPredicate(key domain.Key, args *[]any) (string, error) {
	sourceID, err := uuid.Parse(key.SourceID)
	if err != nil {
		return "", errs.New(errs.KindValidation, CodeBadID, "a rule key's source id must be a UUID")
	}
	*args = append(*args, sourceID, key.Name)
	sql := " AND source_id = $2 AND rule_name = $3"
	if key.Group != "" {
		*args = append(*args, key.Group)
		sql += fmt.Sprintf(" AND rule_group = $%d", len(*args))
	}
	if key.File != "" {
		*args = append(*args, key.File)
		sql += fmt.Sprintf(" AND rule_file = $%d", len(*args))
	}
	return sql, nil
}

// ListByKey returns every distinct capture for one rule key, oldest first.
func (r *SnapshotRepository) ListByKey(ctx context.Context, s db.TenantScope, key domain.Key, limit int) ([]domain.Snapshot, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	args := []any{s.OrgID()}
	pred, err := keyPredicate(key, &args)
	if err != nil {
		return nil, err
	}
	args = append(args, limit)

	sql := `SELECT ` + snapshotColumns + ` FROM rule_snapshots WHERE org_id = $1` + pred +
		fmt.Sprintf(` ORDER BY captured_at ASC, rule_fingerprint ASC LIMIT $%d`, len(args))

	rows, err := r.db(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not read the rule history")
	}
	defer rows.Close()

	var out []domain.Snapshot
	for rows.Next() {
		row, scanErr := scanSnapshot(rows)
		if scanErr != nil {
			return nil, errs.Wrap(scanErr, errs.KindInternal, CodeQueryFailed,
				"could not read the rule history")
		}
		snap, convErr := row.toDomain()
		if convErr != nil {
			return nil, convErr
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not read the rule history")
	}
	return out, nil
}

// ListPage returns one KEYSET PAGE of a rule's capture history, newest first.
//
// ⭐ It exists because `listRuleSnapshots` used to be served from `History()`,
// which is capped at DefaultHistoryLimit and returns the whole slice: the handler
// then took the first `limit` of them, set `has_more` if there were more, and
// emitted `next_cursor: null` — a page-one-only list whose second page could
// never be asked for. A rule edited two hundred and one times had a history the
// API could not show.
//
// Keyset over `(captured_at DESC, id DESC)`. `id` is a uuidv7 and breaks the tie
// deterministically, which matters here more than most places: two captures of
// two different rule texts can share a `captured_at` when one fire recovers both.
//
// NOTE (planner): the predicate is a prefix of rule_snapshots_key_idx
// (org_id, source_id, rule_name, rule_group, rule_file, captured_at DESC), so
// the rows are reached by an index range. `id` is NOT in that index, so the
// `(captured_at, id)` ordering costs a sort on top of the range — over the
// distinct texts of ONE rule key, which is a handful, not over the table. That
// is a deliberate trade: dropping `id` would make the order non-total and the
// cursor unsound the moment two captures shared a timestamp.
func (r *SnapshotRepository) ListPage(
	ctx context.Context, s db.TenantScope, key domain.Key, p db.Keyset,
) ([]domain.Snapshot, db.Cursor, error) {
	limit := p.Limit
	switch {
	case limit <= 0:
		limit = 50
	case limit > 200:
		limit = 200
	}

	args := []any{s.OrgID()}
	pred, err := keyPredicate(key, &args)
	if err != nil {
		return nil, db.Cursor{}, err
	}

	if !p.Cursor.IsZero() {
		args = append(args, p.Cursor.SortKey.UTC(), p.Cursor.ID)
		pred += fmt.Sprintf(" AND (captured_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)

	sql := `SELECT ` + snapshotColumns + ` FROM rule_snapshots WHERE org_id = $1` + pred +
		fmt.Sprintf(` ORDER BY captured_at DESC, id DESC LIMIT $%d`, len(args))

	rows, err := r.db(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, db.Cursor{}, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not read the rule history")
	}
	defer rows.Close()

	out := make([]domain.Snapshot, 0, limit+1)
	ids := make([]uuid.UUID, 0, limit+1)
	for rows.Next() {
		row, scanErr := scanSnapshot(rows)
		if scanErr != nil {
			return nil, db.Cursor{}, errs.Wrap(scanErr, errs.KindInternal, CodeQueryFailed,
				"could not read the rule history")
		}
		snap, convErr := row.toDomain()
		if convErr != nil {
			return nil, db.Cursor{}, convErr
		}
		out = append(out, snap)
		ids = append(ids, row.id)
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not read the rule history")
	}

	hasMore := len(out) > limit
	if hasMore {
		out, ids = out[:limit], ids[:limit]
	}
	if len(out) == 0 {
		return nil, db.Cursor{Hash: p.Cursor.Hash}, nil
	}
	last := out[len(out)-1]
	cursor := db.Cursor{Hash: p.Cursor.Hash}
	if hasMore {
		cursor = db.Cursor{
			SortKey: last.CapturedAt.UTC(),
			ID:      ids[len(ids)-1],
			Hash:    p.Cursor.Hash,
			HasMore: true,
		}
	}
	return out, cursor, nil
}

// Latest returns the newest capture for one rule key.
func (r *SnapshotRepository) Latest(ctx context.Context, s db.TenantScope, key domain.Key) (domain.Snapshot, bool, error) {
	args := []any{s.OrgID()}
	pred, err := keyPredicate(key, &args)
	if err != nil {
		return domain.Snapshot{}, false, err
	}

	sql := `SELECT ` + snapshotColumns + ` FROM rule_snapshots WHERE org_id = $1` + pred +
		` ORDER BY captured_at DESC, rule_fingerprint DESC LIMIT 1`

	row, err := scanSnapshot(r.db(ctx).QueryRow(ctx, sql, args...))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Never captured is a STATE, not a failure: the first fire of a new
		// rule has no predecessor and that is not an error condition.
		return domain.Snapshot{}, false, nil
	case err != nil:
		return domain.Snapshot{}, false, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not read the latest rule snapshot")
	}
	snap, err := row.toDomain()
	if err != nil {
		return domain.Snapshot{}, false, err
	}
	return snap, true, nil
}

func encodeMap(m map[string]string) ([]byte, error) {
	if m == nil {
		m = map[string]string{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindInternal, CodeWriteFailed,
			"could not encode the rule's labels")
	}
	return b, nil
}

func decodeMap(b []byte) (map[string]string, error) {
	out := map[string]string{}
	if len(b) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not decode the rule's labels")
	}
	return out, nil
}
