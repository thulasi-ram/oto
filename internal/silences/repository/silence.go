package repository

// THE SQL over `silences` — the read-only mirror.
//
// ⛔ There is NO INSERT, NO UPDATE and NO DELETE reachable from the API in this
// file. The table is populated exclusively by the `silences.sync` job, and oto has
// no write path into your cluster: creating or expiring a silence here would not
// affect Alertmanager, so it is deliberately impossible (SPEC R3).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/silences/domain"
)

// matcherWire is Alertmanager's own matcher JSON, mirrored verbatim into the
// `matchers` column. The field names are UPSTREAM'S and must not be tidied:
// this column is a copy of what Alertmanager said, not a model of it.
type matcherWire struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual *bool  `json:"isEqual,omitempty"`
}

// silenceRow is the row model of `silences`. Unexported, per the three-model
// rule: it never leaves this package.
type silenceRow struct {
	id              uuid.UUID
	orgID           uuid.UUID
	sourceID        uuid.UUID
	sourceSilenceID string
	matchers        []byte
	startsAt        time.Time
	endsAt          time.Time
	createdBy       string
	comment         string
	annotations     []byte
	state           string
	sourceUpdatedAt *time.Time
	mirroredAt      time.Time
}

var silenceColumns = strings.Join([]string{
	"id", "org_id", "source_id", "source_silence_id", "matchers", "starts_at", "ends_at",
	"created_by", "comment", "annotations", "state", "source_updated_at", "mirrored_at",
}, ", ")

func (r *silenceRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.sourceID, &r.sourceSilenceID, &r.matchers, &r.startsAt, &r.endsAt,
		&r.createdBy, &r.comment, &r.annotations, &r.state, &r.sourceUpdatedAt, &r.mirroredAt,
	}
}

func (r *silenceRow) toDomain() (domain.Silence, error) {
	var wire []matcherWire
	if len(r.matchers) > 0 {
		if err := json.Unmarshal(r.matchers, &wire); err != nil {
			return domain.Silence{}, errs.Internal("silence_matchers_invalid", err)
		}
	}
	matchers := make([]domain.Matcher, 0, len(wire))
	for _, mw := range wire {
		// Alertmanager omits `isEqual` on older versions, where it is implicitly
		// true. Reading a missing field as `false` would silently turn every
		// mirrored `=` into a `!=`, which is the exact inversion of what the
		// silence does.
		isEqual := true
		if mw.IsEqual != nil {
			isEqual = *mw.IsEqual
		}
		m, err := domain.NewMatcher(mw.Name, mw.Value, mw.IsRegex, isEqual)
		if err != nil {
			return domain.Silence{}, errs.Internal("silence_matcher_invalid", err)
		}
		matchers = append(matchers, m)
	}

	annotations := map[string]string{}
	if len(r.annotations) > 0 {
		if err := json.Unmarshal(r.annotations, &annotations); err != nil {
			return domain.Silence{}, errs.Internal("silence_annotations_invalid", err)
		}
	}

	state, err := domain.NewState(r.state)
	if err != nil {
		return domain.Silence{}, errs.Internal("silence_state_invalid", err)
	}

	var updated time.Time
	if r.sourceUpdatedAt != nil {
		updated = *r.sourceUpdatedAt
	}

	s, err := domain.New(domain.Params{
		ID:              r.id,
		OrgID:           r.orgID,
		SourceID:        r.sourceID,
		SourceSilenceID: r.sourceSilenceID,
		Matchers:        matchers,
		StartsAt:        r.startsAt,
		EndsAt:          r.endsAt,
		CreatedBy:       r.createdBy,
		Comment:         r.comment,
		Annotations:     annotations,
		State:           state,
		SourceUpdatedAt: updated,
		MirroredAt:      r.mirroredAt,
	})
	if err != nil {
		return domain.Silence{}, errs.Internal("silence_row_invalid", err)
	}
	return s, nil
}

// SilenceRepository is the SQL over `silences`.
type SilenceRepository struct{ q db.Querier }

// NewSilenceRepository builds the repository over a fallback querier.
func NewSilenceRepository(q db.Querier) *SilenceRepository { return &SilenceRepository{q: q} }

func (r *SilenceRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// listSilencesSQL is keyset-paginated by `(ends_at DESC, id DESC)`.
//
// That ordering is not arbitrary: `silences_active_idx` is
// `(org_id, state, ends_at)`, so "the live and upcoming silences, ordered by when
// they lapse" — which is the question the UI actually asks — is answered straight
// off the index. There is no OFFSET here or anywhere in oto.
var listSilencesSQL = `
SELECT ` + silenceColumns + `
  FROM silences
 WHERE org_id = $1
   AND ($2::text[] IS NULL OR state = ANY($2))
   AND ($3::uuid IS NULL OR source_id = $3)
   AND ($4::text IS NULL OR created_by = $4)
   AND ($5::text IS NULL OR comment ILIKE '%' || $5 || '%' OR matchers::text ILIKE '%' || $5 || '%')
   AND ($6::timestamptz IS NULL OR (ends_at, id) < ($6, $7))
 ORDER BY ends_at DESC, id DESC
 LIMIT $8`

// List returns one keyset page of mirrored silences.
func (r *SilenceRepository) List(
	ctx context.Context, s db.TenantScope, f domain.Filter, p db.Keyset,
) ([]domain.Silence, db.Cursor, error) {
	if !s.Valid() {
		return nil, db.Cursor{}, errs.Forbidden("forbidden", "a tenant scope is required")
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}

	var (
		states    any
		sourceID  any
		createdBy any
		query     any
		curKey    any
		curID     any
	)
	if len(f.States) > 0 {
		states = f.States
	}
	if f.SourceID != uuid.Nil {
		sourceID = f.SourceID
	}
	if f.CreatedBy != "" {
		createdBy = f.CreatedBy
	}
	if f.Query != "" {
		query = f.Query
	}
	if !p.Cursor.IsZero() {
		curKey, curID = p.Cursor.SortKey, p.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, listSilencesSQL,
		s.OrgID(), states, sourceID, createdBy, query, curKey, curID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, errs.Wrap(err, errs.KindInternal, "silences_list_failed",
			"could not read silences")
	}
	defer rows.Close()

	out := make([]domain.Silence, 0, limit)
	var next db.Cursor
	for rows.Next() {
		var row silenceRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, db.Cursor{}, errs.Wrap(err, errs.KindInternal, "silences_scan_failed",
				"could not read silences")
		}
		if len(out) == limit {
			// The extra row proves another page exists without a COUNT — there
			// is deliberately no total on an unbounded collection.
			next.HasMore = true
			break
		}
		sil, err := row.toDomain()
		if err != nil {
			return nil, db.Cursor{}, err
		}
		out = append(out, sil)
		next.SortKey, next.ID, next.Hash = sil.EndsAt(), sil.ID(), f.FilterHash
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, errs.Wrap(err, errs.KindInternal, "silences_list_failed",
			"could not read silences")
	}
	return out, next, nil
}

var getSilenceSQL = `
SELECT ` + silenceColumns + `
  FROM silences
 WHERE org_id = $1 AND id = $2`

// Get returns one mirrored silence.
func (r *SilenceRepository) Get(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) (domain.Silence, error) {
	if !s.Valid() {
		return domain.Silence{}, errs.Forbidden("forbidden", "a tenant scope is required")
	}
	var row silenceRow
	err := r.db(ctx).QueryRow(ctx, getSilenceSQL, s.OrgID(), id).Scan(row.scanDest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Silence{}, errs.NotFound("not_found", "no such silence")
	}
	if err != nil {
		return domain.Silence{}, errs.Wrap(err, errs.KindInternal, "silence_read_failed",
			"could not read the silence")
	}
	return row.toDomain()
}
