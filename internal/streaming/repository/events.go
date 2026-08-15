package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/streaming/domain"
)

// uiEventRow is the row model of `ui_events`. Unexported, per the three-model
// rule: no DTO and no domain type may embed it.
type uiEventRow struct {
	seq        int64
	orgID      uuid.UUID
	kind       string
	resource   string
	resourceID uuid.UUID
	payload    []byte
	at         time.Time
}

func (r uiEventRow) toDomain() domain.Event {
	payload := json.RawMessage(r.payload)
	kind := domain.Kind(r.kind)
	alertID, groupID := domain.ScopeOf(kind, r.resourceID, payload)
	return domain.Event{
		Seq:        r.seq,
		OrgID:      r.orgID,
		Kind:       kind,
		Resource:   domain.Resource(r.resource),
		ResourceID: r.resourceID,
		Payload:    payload,
		At:         r.at,
		AlertID:    alertID,
		GroupID:    groupID,
	}
}

// EventRepository is the SQL over `ui_events`.
//
// Every method joins the caller's transaction through db.FromContext. That is not
// a convenience: the append and its NOTIFY are required to be in the WRITER's
// transaction (SPEC §E.4), because a notification announcing a row that never
// commits is worse than no notification at all.
type EventRepository struct {
	q db.Querier
}

// NewEventRepository builds the repository over a fallback querier, normally the
// general pool.
func NewEventRepository(q db.Querier) *EventRepository { return &EventRepository{q: q} }

func (r *EventRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// Append inserts one event and issues the NOTIFY, both inside the caller's
// transaction.
func (r *EventRepository) Append(ctx context.Context, s db.TenantScope, e domain.NewEvent) (domain.Event, error) {
	out, err := r.AppendBatch(ctx, s, []domain.NewEvent{e})
	if err != nil {
		return domain.Event{}, err
	}
	return out[0], nil
}

const insertEventBatchSQL = `
INSERT INTO ui_events (org_id, kind, resource, resource_id, payload)
SELECT $1, k, res, rid, pl
  FROM unnest($2::text[], $3::text[], $4::uuid[], $5::jsonb[]) AS t(k, res, rid, pl)
RETURNING seq, at, kind, resource, resource_id, payload`

// AppendBatch inserts many events in one round trip and issues ONE NOTIFY for the
// resulting seq range.
//
// One notification rather than N is deliberate: a 200-alert webhook produces
// hundreds of ui_events, and the listener re-reads a range anyway (SPEC §E.4), so
// N notifications would be N wakeups doing the same query. The payload names the
// highest seq; the listener's watermark covers everything below it.
func (r *EventRepository) AppendBatch(ctx context.Context, s db.TenantScope, in []domain.NewEvent) ([]domain.Event, error) {
	if len(in) == 0 {
		return nil, nil
	}

	kinds := make([]string, len(in))
	resources := make([]string, len(in))
	ids := make([]uuid.UUID, len(in))
	payloads := make([][]byte, len(in))
	for i, e := range in {
		kinds[i] = string(e.Kind)
		resources[i] = string(e.Resource)
		ids[i] = e.ResourceID
		if len(e.Payload) == 0 {
			payloads[i] = []byte(`{}`)
			continue
		}
		payloads[i] = e.Payload
	}

	rows, err := r.db(ctx).Query(ctx, insertEventBatchSQL, s.OrgID(), kinds, resources, ids, payloads)
	if err != nil {
		return nil, mapErr(err, "append ui events")
	}
	defer rows.Close()

	out := make([]domain.Event, 0, len(in))
	var maxSeq int64
	for rows.Next() {
		row := uiEventRow{orgID: s.OrgID()}
		if err := rows.Scan(&row.seq, &row.at, &row.kind, &row.resource, &row.resourceID, &row.payload); err != nil {
			return nil, mapErr(err, "scan ui event")
		}
		if row.seq > maxSeq {
			maxSeq = row.seq
		}
		out = append(out, row.toDomain())
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read ui events")
	}

	if err := db.Notify(ctx, r.db(ctx), db.EventsChannel, NotifyPayload(s.OrgID(), maxSeq)); err != nil {
		return nil, mapErr(err, "notify ui events")
	}
	return out, nil
}

// NotifyPayload is the wire form of a NOTIFY: `<org_id>:<seq>`.
//
// Deliberately tiny. NOTIFY has an 8 kB ceiling that FAILS THE TRANSACTION when
// exceeded, which on the ingest path would turn a UI nicety into a lost alert.
// The envelope travels through the table; the notification is only a doorbell.
func NotifyPayload(orgID uuid.UUID, seq int64) string {
	return orgID.String() + ":" + strconv.FormatInt(seq, 10)
}

const listSinceSQL = `
SELECT seq, kind, resource, resource_id, payload, at
  FROM ui_events
 WHERE org_id = $1 AND seq > $2 AND at >= $3
 ORDER BY seq ASC
 LIMIT $4`

// ListSince reads the replay window for one org in seq order.
//
// The `at >= $3` bound is what prunes partitions: `ui_events` is hourly-ranged on
// `at`, so without it this scans every retained partition instead of the handful
// the cursor can possibly be in. The cutoff is passed in rather than computed as
// `now() - interval '24 hours'` so that the clock stays injectable.
func (r *EventRepository) ListSince(
	ctx context.Context, s db.TenantScope, sinceSeq int64, cutoff time.Time, limit int,
) ([]domain.Event, error) {
	if limit <= 0 {
		limit = domain.MaxReplayRows
	}

	rows, err := r.db(ctx).Query(ctx, listSinceSQL, s.OrgID(), sinceSeq, cutoff, limit)
	if err != nil {
		return nil, mapErr(err, "list ui events")
	}
	defer rows.Close()

	out := make([]domain.Event, 0, 64)
	for rows.Next() {
		row := uiEventRow{orgID: s.OrgID()}
		if err := rows.Scan(&row.seq, &row.kind, &row.resource, &row.resourceID, &row.payload, &row.at); err != nil {
			return nil, mapErr(err, "scan ui event")
		}
		out = append(out, row.toDomain())
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read ui events")
	}
	return out, nil
}

const seqBoundsSQL = `
SELECT min(seq), max(seq) FROM ui_events WHERE org_id = $1 AND at >= $2`

// SeqBounds returns the lowest and highest seq still inside the replay window for
// this org, and whether the org has any retained events at all.
//
// The floor is how "can I honour this Last-Event-ID?" is answered. If it is ABOVE
// the client's cursor, then rows between the two have been dropped by retention
// and oto cannot prove the replay would be complete. The only honest answer is a
// `resync` — a partial replay that looks complete is worse than an explicit
// refetch. The ceiling seeds the live bridge's watermark.
func (r *EventRepository) SeqBounds(ctx context.Context, s db.TenantScope, cutoff time.Time) (int64, int64, bool, error) {
	var lo, hi *int64
	if err := r.db(ctx).QueryRow(ctx, seqBoundsSQL, s.OrgID(), cutoff).Scan(&lo, &hi); err != nil {
		return 0, 0, false, mapErr(err, "read ui event bounds")
	}
	if lo == nil || hi == nil {
		return 0, 0, false, nil
	}
	return *lo, *hi, true, nil
}

// mapErr turns a database error into an errs.Kind for this repository. The §L.9
// table itself lives in `db.MapError` and is shared by every repository — this
// module contributes only the two codes it alone can name. The repository never
// validates a business rule; it does own this translation.
//
// Three things changed when this stopped being its own copy, and all three are
// deliberate.
//
// FIRST, it spelled three rows and dropped everything else onto `KindInternal`,
// so a `57014` — the statement_timeout every pool sets — was a 500 where §L.9
// says 503. That is the ticket.
//
// SECOND, it discarded the constraint name in favour of `ui_event_conflict` and
// `ui_event_fk` even when Postgres had named one, which contradicts CONTEXT.md
// §6. A named constraint now wins; the two codes survive as the fallback they
// should always have been, so nothing branching on them breaks.
//
// THIRD, a `40001`/`40P01` is now a 409 rather than a 500. §L.1 defines
// KindConflict as "the caller must re-read and retry", which is exactly what a
// serialization failure asks for, and eight of the ten copies already said so —
// this was one of the two that had not written the row yet, not a decision
// against it. It is invisible to the worker that does almost all of the
// appending: `jobs.Classify` puts KindConflict and KindInternal in the same
// ClassRetryable, so nothing about retry or backoff moves.
//
// ⛔ THE APPEND IS NOT ON THE SYNCHRONOUS INGEST PATH, which is the only reason a
// 409 here is discussable at all. `ui_events` rows are written by `alerts` and
// `grouping` from inside `ingest.process_batch` (§G.4), a job — so C4's "never a
// 4xx to Alertmanager" is answered by the 202 the ingest handler already sent,
// not by this Kind.
func mapErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "ui_event_not_found",
		NotFoundMessage:    "no such ui event",
		QueryFailed:        "ui_event_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
		Codes:              uiEventCodes,
	})
}

// uiEventCodes are the codes this module published for the rows it spelled, kept
// as the fallback for when Postgres names no constraint.
var uiEventCodes = map[string]string{
	"23505": "ui_event_conflict",
	"23503": "ui_event_fk",
	"23514": "ui_event_check_violation",
}
