package repository

import (
	"context"
	"encoding/json"
	"log/slog"
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

func (r enrichmentRow) toDomain() (domain.Enrichment, error) {
	p := domain.EnrichmentParams{
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
		p.Payload = json.RawMessage(r.payload)
	}
	if r.errText != nil {
		p.Error = *r.errText
	}
	if r.expiresAt != nil {
		p.ExpiresAt = r.expiresAt.UTC()
	}
	// The same door in as on the way out: a stored row is re-proved rather than
	// trusted, so a hand-edited or migration-damaged row is named here instead of
	// travelling on as a valid-looking record.
	return domain.NewEnrichment(p)
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
	// log reports a DEGRADED READ: a stored row this package skipped because the
	// domain could not interpret it. It is the only thing this repository logs,
	// because it is the only thing it does that a caller cannot see in the return
	// value.
	log *slog.Logger
}

// NewEnrichmentRepository builds the repository over a fallback querier,
// normally the general pool.
//
// The logger defaults to slog.Default(), which cmd/oto sets to the configured
// application logger at startup, so the zero-configuration repository still
// reports a degraded read to the real log.
func NewEnrichmentRepository(q db.Querier) *EnrichmentRepository {
	return &EnrichmentRepository{q: q, log: slog.Default()}
}

// WithLogger returns a copy that reports degraded reads to log.
//
// It is a copy rather than a setter because a repository is shared across every
// request in the process, and one that can be re-pointed after wiring is one
// whose logging destination depends on when you look.
func (r *EnrichmentRepository) WithLogger(log *slog.Logger) *EnrichmentRepository {
	if log == nil {
		return r
	}
	c := *r
	c.log = log
	return &c
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
		return nil, mapErr(err, CodeQueryFailed, "could not read the subject's enrichments")
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
			return nil, mapErr(err, CodeQueryFailed, "could not read the subject's enrichments")
		}
		e, err := row.toDomain()
		if err != nil {
			// A stored row the domain cannot interpret is ABSENT, not fatal.
			//
			// The DDL is LOOSER than the constructor and always will be: an empty
			// error string satisfies enrichments_err_ck but not NewEnrichment, so a
			// row written before the constructor existed — or by hand, or by a
			// migration — is legal in the table and unbuildable here. Failing the read
			// would wedge that row permanently: the pipeline reads this list BEFORE it
			// runs the enrichers, so the row that would repair it is written
			// downstream of the read that refuses it, and it can never self-heal.
			//
			// Skipping restores the repair path. The caller sees the enricher as
			// never having run, re-runs it, and UpsertMany overwrites the row on the
			// (subject_kind, subject_id, enricher) conflict target. A row we cannot
			// interpret is one we should recompute, which is exactly what absent
			// means here.
			//
			// It is ERROR, not WARN, and it carries the row's whole identity: nothing
			// this repository writes can produce such a row, so one existing is a bug
			// or an operator edit, and the log line is how it gets found. The WRITE
			// path stays strict — UpsertMany still refuses a record the domain never
			// vouched for.
			r.log.ErrorContext(ctx, "enrichment: skipping a stored row the domain cannot interpret",
				"enrichment_id", row.id,
				"org_id", row.orgID,
				"subject_kind", row.subjectKind,
				"subject_id", row.subjectID,
				"enricher", row.enricher,
				"error", err)
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, CodeQueryFailed, "could not read the subject's enrichments")
	}
	return out, nil
}

// upsertEnrichmentSQL writes one provenanced result.
//
// The conflict target is enrichments_subject_uniq (subject_kind, subject_id,
// enricher) — WITHOUT the version, deliberately. There is exactly one current
// answer per enricher per subject, and a version bump REPLACES the old answer
// rather than accumulating beside it. That is what makes `enrich.run`
// idempotent on (case_id, phase): re-running a phase overwrites its own
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
//
// It re-checks none of the CHECK constraints: every one of them is proven by
// domain.NewEnrichment, and an Enrichment that reached this method is one the
// constructor already vouched for. What is left here is the shape SQL needs and
// the domain does not — the subject id as a UUID rather than a string.
func (r *EnrichmentRepository) UpsertMany(ctx context.Context, s db.TenantScope, in []domain.Enrichment) error {
	if len(in) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, e := range in {
		subjectID, err := uuid.Parse(e.SubjectID())
		if err != nil {
			return errs.New(errs.KindValidation, "enrichment_bad_subject_id",
				"a subject id must be a UUID")
		}
		rowID, err := uuid.Parse(e.ID())
		if err != nil {
			rowID = id.New()
		}

		payload, err := encodePayload(e.Payload())
		if err != nil {
			return err
		}

		var errText *string
		if e.ErrorText() != "" {
			v := e.ErrorText()
			errText = &v
		}
		var expires *time.Time
		if !e.ExpiresAt().IsZero() {
			v := e.ExpiresAt()
			expires = &v
		}
		warnings := e.Warnings()
		if warnings == nil {
			warnings = []string{}
		}

		batch.Queue(upsertEnrichmentSQL,
			rowID, s.OrgID(), e.SubjectKind(), subjectID, e.Enricher(), e.Version(),
			int16(e.Phase()), string(e.Status()), payload, warnings, errText,
			int32(e.Duration().Milliseconds()), e.FromCache(), e.ComputedAt(), expires,
		)
	}

	results := r.db(ctx).SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()

	for range in {
		if _, err := results.Exec(); err != nil {
			return mapErr(err, CodeWriteFailed, "could not store the enrichment results")
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
