package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// Error codes this repository mints.
const (
	// CodeQueryFailed means Postgres refused a read.
	CodeQueryFailed = "enrichment_query_failed"
	// CodeWriteFailed means Postgres refused a write.
	CodeWriteFailed = "enrichment_write_failed"
	// CodeEncodeFailed means a payload would not marshal to JSONB.
	CodeEncodeFailed = "enrichment_encode_failed"
)

// enrichmentRow is the row model of `enrichments`. Unexported, per the
// three-model rule.
type enrichmentRow struct {
	id          uuid.UUID
	orgID       uuid.UUID
	subjectKind string
	subjectID   uuid.UUID
	enricher    string
	version     int
	phase       int16
	status      string
	payload     []byte
	warnings    []string
	errText     *string
	durationMS  int32
	fromCache   bool
	computedAt  time.Time
	expiresAt   *time.Time
}

func (r enrichmentRow) toDomain() domain.Enrichment {
	out := domain.Enrichment{
		ID:          r.id.String(),
		OrgID:       r.orgID.String(),
		SubjectKind: r.subjectKind,
		SubjectID:   r.subjectID.String(),
		Enricher:    r.enricher,
		Version:     r.version,
		Phase:       domain.Phase(r.phase),
		Status:      domain.Status(r.status),
		Warnings:    r.warnings,
		Duration:    time.Duration(r.durationMS) * time.Millisecond,
		FromCache:   r.fromCache,
		ComputedAt:  r.computedAt.UTC(),
	}
	// The payload is handed back as raw JSON. Decoding it into the enricher's
	// own struct is the reader's job: this package must not know the shape of
	// every enricher's output, or adding an enricher would mean editing SQL.
	if len(r.payload) > 0 {
		out.Payload = json.RawMessage(r.payload)
	}
	if r.errText != nil {
		out.Error = *r.errText
	}
	if r.expiresAt != nil {
		out.ExpiresAt = r.expiresAt.UTC()
	}
	return out
}

const enrichmentColumns = `id, org_id, subject_kind, subject_id, enricher, enricher_version,
       phase, status, payload, warnings, error, duration_ms, from_cache, computed_at, expires_at`

// EnrichmentRepository is the SQL over `enrichments`.
//
// Every method joins the caller's transaction through db.FromContext, and every
// one of them is org-scoped: `enrichments` is a multi-tenant table and a read
// without an org_id predicate is a cross-tenant leak, not a slow query.
type EnrichmentRepository struct {
	q db.Querier
}

// NewEnrichmentRepository builds the repository over a fallback querier,
// normally the general pool.
func NewEnrichmentRepository(q db.Querier) *EnrichmentRepository {
	return &EnrichmentRepository{q: q}
}

func (r *EnrichmentRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const listBySubjectSQL = `
SELECT ` + enrichmentColumns + `
  FROM enrichments
 WHERE org_id = $1 AND subject_kind = $2 AND subject_id = $3
 ORDER BY enricher ASC`

// ListBySubject returns everything already computed about one subject.
func (r *EnrichmentRepository) ListBySubject(
	ctx context.Context, s db.TenantScope, subjectKind, subjectID string,
) ([]domain.Enrichment, error) {
	sid, err := uuid.Parse(subjectID)
	if err != nil {
		return nil, errs.New(errs.KindValidation, "enrichment_bad_subject_id",
			"a subject id must be a UUID")
	}

	rows, err := r.db(ctx).Query(ctx, listBySubjectSQL, s.OrgID(), subjectKind, sid)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not read the subject's enrichments")
	}
	defer rows.Close()

	var out []domain.Enrichment
	for rows.Next() {
		var row enrichmentRow
		if err := rows.Scan(
			&row.id, &row.orgID, &row.subjectKind, &row.subjectID, &row.enricher, &row.version,
			&row.phase, &row.status, &row.payload, &row.warnings, &row.errText,
			&row.durationMS, &row.fromCache, &row.computedAt, &row.expiresAt,
		); err != nil {
			return nil, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
				"could not read the subject's enrichments")
		}
		out = append(out, row.toDomain())
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.KindInternal, CodeQueryFailed,
			"could not read the subject's enrichments")
	}
	return out, nil
}

// upsertEnrichmentSQL writes one provenanced result.
//
// The conflict target is enrichments_subject_uniq (subject_kind, subject_id,
// enricher) — WITHOUT the version, deliberately. There is exactly one current
// answer per enricher per subject, and a version bump REPLACES the old answer
// rather than accumulating beside it. That is what makes `enrich.run`
// idempotent on (occurrence_id, phase): re-running a phase overwrites its own
// rows, so a retry after a partial failure converges instead of double-counting.
const upsertEnrichmentSQL = `
INSERT INTO enrichments (
  id, org_id, subject_kind, subject_id, enricher, enricher_version,
  phase, status, payload, warnings, error, duration_ms, from_cache, computed_at, expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12,$13,$14,$15)
ON CONFLICT (subject_kind, subject_id, enricher) DO UPDATE SET
  enricher_version = EXCLUDED.enricher_version,
  phase            = EXCLUDED.phase,
  status           = EXCLUDED.status,
  payload          = EXCLUDED.payload,
  warnings         = EXCLUDED.warnings,
  error            = EXCLUDED.error,
  duration_ms      = EXCLUDED.duration_ms,
  from_cache       = EXCLUDED.from_cache,
  computed_at      = EXCLUDED.computed_at,
  expires_at       = EXCLUDED.expires_at`

// UpsertMany stores a whole phase's results in one round trip.
//
// A phase of four enrichers must not be four round trips: the inline phase has
// a 2 000 ms budget for everything including its own writes, and four
// sequential round trips over a busy pool is a meaningful fraction of it.
func (r *EnrichmentRepository) UpsertMany(ctx context.Context, s db.TenantScope, in []domain.Enrichment) error {
	if len(in) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, e := range in {
		if err := e.Validate(); err != nil {
			return err
		}
		subjectID, err := uuid.Parse(e.SubjectID)
		if err != nil {
			return errs.New(errs.KindValidation, "enrichment_bad_subject_id",
				"a subject id must be a UUID")
		}
		rowID, err := uuid.Parse(e.ID)
		if err != nil {
			rowID = id.New()
		}

		payload, err := encodePayload(e.Payload)
		if err != nil {
			return err
		}

		var errText *string
		if e.Error != "" {
			v := e.Error
			errText = &v
		}
		var expires *time.Time
		if !e.ExpiresAt.IsZero() {
			v := e.ExpiresAt.UTC()
			expires = &v
		}
		warnings := e.Warnings
		if warnings == nil {
			warnings = []string{}
		}

		batch.Queue(upsertEnrichmentSQL,
			rowID, s.OrgID(), e.SubjectKind, subjectID, e.Enricher, e.Version,
			int16(e.Phase), string(e.Status), payload, warnings, errText,
			int32(e.Duration.Milliseconds()), e.FromCache, e.ComputedAt.UTC(), expires,
		)
	}

	results := r.db(ctx).SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()

	for range in {
		if _, err := results.Exec(); err != nil {
			return errs.Wrap(err, errs.KindInternal, CodeWriteFailed,
				"could not store the enrichment results")
		}
	}
	return nil
}

// encodePayload marshals an enricher's typed output to JSONB.
//
// A nil payload becomes `{}` rather than `null`, because enrichments_payload_ck
// requires a JSON OBJECT: a null would be rejected by the constraint, and the
// place to notice that is here, not in a 3am stack trace.
func encodePayload(payload any) ([]byte, error) {
	if payload == nil {
		return []byte(`{}`), nil
	}
	if raw, ok := payload.(json.RawMessage); ok {
		if len(raw) == 0 {
			return []byte(`{}`), nil
		}
		return raw, nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindInternal, CodeEncodeFailed,
			"an enricher produced a payload that cannot be stored")
	}
	if len(b) == 0 || b[0] != '{' {
		// Anything that is not an object is wrapped rather than dropped: the
		// result is still provenance, and losing it to a CHECK would be worse.
		wrapped, werr := json.Marshal(map[string]json.RawMessage{"value": b})
		if werr != nil {
			return []byte(`{}`), nil
		}
		return wrapped, nil
	}
	return b, nil
}
