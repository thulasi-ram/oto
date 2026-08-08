package repository

// THE MIRROR WRITE — and the only write in this package.
//
// ⚠️ READ THE DISTINCTION BEFORE CHANGING ANYTHING HERE. "Read-only" (SPEC R3,
// CONTEXT.md §4) means oto has NO WRITE PATH INTO YOUR CLUSTER: it cannot create,
// edit or expire an Alertmanager silence. It does not mean the mirror table is
// immutable — `silences` is populated by the `silences.sync` job and by nothing
// else, exactly as the table's own COMMENT says. This file is that job's hand.
//
// Nothing below is reachable from the API. The service exposes List and Get; Sync
// is called by the worker, and there is no HTTP route that reaches it.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/silences/domain"
)

// upsertSilencesSQL writes one source's whole silence set in ONE round trip.
//
// ⭐ IT CONVERGES, IT DOES NOT ACCUMULATE. The natural key is Alertmanager's own
// silence id (`silences_source_uniq`), so re-running the sync against an
// unchanged upstream rewrites the same rows with the same values and changes
// nothing. That is what makes the job safely re-runnable under an at-least-once
// queue.
//
// ⛔ THERE IS NO DELETE. A silence that Alertmanager has garbage-collected keeps
// its last mirrored state — which is `expired`, because the sync asks for
// expired silences too. Deleting the row would erase the answer to "why was this
// alert quiet last Tuesday", which is the entire reason the mirror exists.
//
// `id` is NOT in the DO UPDATE list: oto's own row id is stable for the life of
// the mirror row, and re-keying it on every sync would break every link that
// points at it.
const upsertSilencesSQL = `
INSERT INTO silences (id, org_id, source_id, source_silence_id, matchers, starts_at, ends_at,
                      created_by, comment, annotations, state, source_updated_at, mirrored_at)
SELECT u.id, $1, u.source_id, u.source_silence_id, u.matchers, u.starts_at, u.ends_at,
       u.created_by, u.comment, u.annotations, u.state, u.source_updated_at, u.mirrored_at
  FROM unnest($2::uuid[], $3::uuid[], $4::text[], $5::jsonb[], $6::timestamptz[],
              $7::timestamptz[], $8::text[], $9::text[], $10::jsonb[], $11::text[],
              $12::timestamptz[], $13::timestamptz[])
    AS u(id, source_id, source_silence_id, matchers, starts_at, ends_at,
         created_by, comment, annotations, state, source_updated_at, mirrored_at)
ON CONFLICT (source_id, source_silence_id) DO UPDATE SET
    matchers          = EXCLUDED.matchers,
    starts_at         = EXCLUDED.starts_at,
    ends_at           = EXCLUDED.ends_at,
    created_by        = EXCLUDED.created_by,
    comment           = EXCLUDED.comment,
    annotations       = EXCLUDED.annotations,
    state             = EXCLUDED.state,
    source_updated_at = EXCLUDED.source_updated_at,
    mirrored_at       = EXCLUDED.mirrored_at`

// UpsertBatch mirrors one source's silences.
//
// Two entries carrying the same upstream id would make `ON CONFLICT DO UPDATE`
// touch one row twice in one statement, which Postgres refuses. The input is
// collapsed by natural key first, last one winning.
func (r *SilenceRepository) UpsertBatch(
	ctx context.Context, s db.TenantScope, in []domain.Silence,
) (int, error) {
	if !s.Valid() {
		return 0, errs.Forbidden("forbidden", "a tenant scope is required")
	}
	if len(in) == 0 {
		return 0, nil
	}

	type key struct {
		source uuid.UUID
		id     string
	}
	order := make([]key, 0, len(in))
	winner := make(map[key]domain.Silence, len(in))
	for _, sil := range in {
		k := key{source: sil.SourceID(), id: sil.SourceSilenceID()}
		if _, seen := winner[k]; !seen {
			order = append(order, k)
		}
		winner[k] = sil
	}

	n := len(order)
	ids := make([]uuid.UUID, n)
	sourceIDs := make([]uuid.UUID, n)
	upstreamIDs := make([]string, n)
	matchers := make([][]byte, n)
	startsAt := make([]time.Time, n)
	endsAt := make([]time.Time, n)
	createdBy := make([]string, n)
	comments := make([]string, n)
	annotations := make([][]byte, n)
	states := make([]string, n)
	sourceUpdated := make([]*time.Time, n)
	mirroredAt := make([]time.Time, n)

	for i, k := range order {
		sil := winner[k]
		if sil.ID() == uuid.Nil || sil.SourceID() == uuid.Nil {
			return 0, errs.Internal("silence_upsert_incomplete",
				errs.New(errs.KindInternal, "missing_field", "a mirrored silence needs an id and a source id"))
		}

		m, err := encodeMatchers(sil.Matchers())
		if err != nil {
			return 0, err
		}
		a, err := encodeJSONObject(sil.Annotations())
		if err != nil {
			return 0, err
		}

		ids[i] = sil.ID()
		sourceIDs[i] = sil.SourceID()
		upstreamIDs[i] = sil.SourceSilenceID()
		matchers[i] = m
		startsAt[i] = sil.StartsAt()
		endsAt[i] = sil.EndsAt()
		createdBy[i] = sil.CreatedBy()
		comments[i] = sil.Comment()
		annotations[i] = a
		states[i] = sil.State().String()
		if u := sil.SourceUpdatedAt(); !u.IsZero() {
			t := u
			sourceUpdated[i] = &t
		}
		mirroredAt[i] = sil.MirroredAt()
	}

	tag, err := r.db(ctx).Exec(ctx, upsertSilencesSQL,
		s.OrgID(), ids, sourceIDs, upstreamIDs, matchers, startsAt, endsAt,
		createdBy, comments, annotations, states, sourceUpdated, mirroredAt)
	if err != nil {
		return 0, errs.Wrap(err, errs.KindInternal, "silences_sync_failed",
			"the silence mirror could not be written")
	}
	return int(tag.RowsAffected()), nil
}

const upstreamIDsSQL = `SELECT source_silence_id, id FROM silences WHERE org_id = $1 AND source_id = $2`

// ExistingIDs maps a source's upstream silence ids onto the row ids oto already
// minted for them.
//
// It exists so the sync reuses an existing row id instead of generating a fresh
// one on every pass. Generating one every time would still converge — `id` is not
// in the DO UPDATE list — but it would burn a UUID per silence per minute and
// make the insert path indistinguishable from the update path in the logs.
func (r *SilenceRepository) ExistingIDs(
	ctx context.Context, s db.TenantScope, sourceID uuid.UUID,
) (map[string]uuid.UUID, error) {
	if !s.Valid() {
		return nil, errs.Forbidden("forbidden", "a tenant scope is required")
	}
	rows, err := r.db(ctx).Query(ctx, upstreamIDsSQL, s.OrgID(), sourceID)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindInternal, "silences_sync_failed",
			"the mirrored silence ids could not be read")
	}
	defer rows.Close()

	out := map[string]uuid.UUID{}
	for rows.Next() {
		var (
			upstream string
			id       uuid.UUID
		)
		if err := rows.Scan(&upstream, &id); err != nil {
			return nil, errs.Wrap(err, errs.KindInternal, "silences_sync_failed",
				"the mirrored silence ids could not be read")
		}
		out[upstream] = id
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.KindInternal, "silences_sync_failed",
			"the mirrored silence ids could not be read")
	}
	return out, nil
}

// encodeMatchers renders the matchers into the column's stored shape.
//
// `isEqual` is written EXPLICITLY, never omitted. The reader defaults a missing
// `isEqual` to true for Alertmanager versions that predate the field, so writing
// nothing for a genuine `!=` matcher would silently invert it.
func encodeMatchers(in []domain.Matcher) ([]byte, error) {
	wire := make([]matcherWire, 0, len(in))
	for _, m := range in {
		isEqual := m.IsEqual
		wire = append(wire, matcherWire{
			Name: m.Name, Value: m.Value, IsRegex: m.IsRegex, IsEqual: &isEqual,
		})
	}
	out, err := json.Marshal(wire)
	if err != nil {
		return nil, errs.Internal("jsonb_encode_failed", err)
	}
	return out, nil
}

// encodeJSONObject renders a string map as a jsonb object, never as `null`:
// `silences_annot_ck` requires an object.
func encodeJSONObject(in map[string]string) ([]byte, error) {
	if in == nil {
		in = map[string]string{}
	}
	out, err := json.Marshal(in)
	if err != nil {
		return nil, errs.Internal("jsonb_encode_failed", err)
	}
	return out, nil
}
