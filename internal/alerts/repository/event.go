package repository

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// eventRow is the row model of `alert_events`. Unexported, per the three-model
// rule.
type eventRow struct {
	id           uuid.UUID
	orgID        uuid.UUID
	alertID      *uuid.UUID
	occurrenceID *uuid.UUID
	groupID      *uuid.UUID
	typ          string
	occurredAt   time.Time
	recordedAt   time.Time
	actorKind    string
	actorID      *string
	actorLabel   *string
	summary      string
	payload      []byte
	dedupeKey    *string
}

var eventColumnList = []string{
	"id", "org_id", "alert_id", "occurrence_id", "group_id", "type", "occurred_at", "recorded_at",
	"actor_kind", "actor_id", "actor_label", "summary", "payload", "dedupe_key",
}

var eventColumns = strings.Join(eventColumnList, ", ")

func (r *eventRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.alertID, &r.occurrenceID, &r.groupID, &r.typ, &r.occurredAt,
		&r.recordedAt, &r.actorKind, &r.actorID, &r.actorLabel, &r.summary, &r.payload,
		&r.dedupeKey,
	}
}

func (r *eventRow) toDomain() (domain.Event, error) {
	typ, err := domain.NewEventType(r.typ)
	if err != nil {
		return domain.Event{}, errs.Internal("event_type_invalid", err)
	}
	kind, err := domain.NewActorKind(r.actorKind)
	if err != nil {
		return domain.Event{}, errs.Internal("event_actor_kind_invalid", err)
	}
	actor, err := domain.NewActor(kind, strOrEmpty(r.actorID), strOrEmpty(r.actorLabel))
	if err != nil {
		return domain.Event{}, errs.Internal("event_actor_invalid", err)
	}
	at, err := domain.NewObservationTime(r.occurredAt, r.recordedAt)
	if err != nil {
		return domain.Event{}, errs.Internal("event_time_invalid", err)
	}
	payload, err := decodeAnyMap(r.payload)
	if err != nil {
		return domain.Event{}, err
	}

	e, err := domain.NewEvent(domain.EventParams{
		ID:           r.id,
		OrgID:        r.orgID,
		AlertID:      idOrNil(r.alertID),
		OccurrenceID: idOrNil(r.occurrenceID),
		GroupID:      idOrNil(r.groupID),
		Type:         typ,
		At:           at,
		Actor:        actor,
		Summary:      r.summary,
		Payload:      payload,
		DedupeKey:    strOrEmpty(r.dedupeKey),
	})
	if err != nil {
		return domain.Event{}, errs.Internal("event_row_invalid", err)
	}
	return e, nil
}

// EventRepository is the SQL over `alert_events`. It implements
// service.EventRepository and it is APPEND ONLY.
//
// There is no Update and there is no Delete in this file, and there never will
// be: `alert_events` is the truth and everything else in §D.4 is a projection of
// it. Events age out by DROPping a monthly partition, never by a statement.
type EventRepository struct {
	q     db.Querier
	clock clock.Clock
}

// NewEventRepository builds the repository over a fallback querier. The clock
// supplies the open end of a TimeWindow, so that a timeline query stays testable
// without reaching for time.Now().
func NewEventRepository(q db.Querier, clk clock.Clock) *EventRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &EventRepository{q: q, clock: clk}
}

func (r *EventRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// ------------------------------------------------------------------- append

const claimDedupeKeysSQL = `
INSERT INTO alert_event_keys (org_id, dedupe_key, event_id)
SELECT $1, k, e FROM unnest($2::text[], $3::uuid[]) AS t(k, e)
ON CONFLICT (org_id, dedupe_key) DO NOTHING
RETURNING dedupe_key`

const insertEventsSQL = `
INSERT INTO alert_events (id, org_id, alert_id, occurrence_id, group_id, type, occurred_at,
                          recorded_at, actor_kind, actor_id, actor_label, summary, payload,
                          dedupe_key)
SELECT t.id, $1, t.alert_id, t.occurrence_id, t.group_id, t.type, t.occurred_at, t.recorded_at,
       t.actor_kind, t.actor_id, t.actor_label, t.summary, t.payload, t.dedupe_key
  FROM unnest($2::uuid[], $3::uuid[], $4::uuid[], $5::uuid[], $6::text[], $7::timestamptz[],
              $8::timestamptz[], $9::text[], $10::text[], $11::text[], $12::text[], $13::jsonb[],
              $14::text[])
    AS t(id, alert_id, occurrence_id, group_id, type, occurred_at, recorded_at, actor_kind,
         actor_id, actor_label, summary, payload, dedupe_key)`

// Append writes one event, idempotently.
//
// ⭐ SPEC §C.8. The dedupe key is claimed in the UNPARTITIONED `alert_event_keys`
// table FIRST, in the same transaction. Zero rows affected means the fact is
// already recorded and the `alert_events` insert is SKIPPED — that is the
// idempotency mechanism working, not an error, which is why the second return
// value is `written bool` and not an error.
//
// The key table is unpartitioned deliberately (conflict ruling 14): a unique
// index on the partitioned parent would have to include `recorded_at`, and the
// entire point is to suppress a SECOND write at a DIFFERENT time.
func (r *EventRepository) Append(
	ctx context.Context, s db.TenantScope, e domain.Event,
) (domain.Event, bool, error) {
	n, err := r.AppendBatch(ctx, s, []domain.Event{e})
	if err != nil {
		return domain.Event{}, false, err
	}
	return e, n == 1, nil
}

// AppendBatch writes many events in one round trip and returns how many were
// actually written. A batch that is entirely deduped writes nothing and returns
// zero.
func (r *EventRepository) AppendBatch(ctx context.Context, s db.TenantScope, in []domain.Event) (int, error) {
	if err := requireScope(s); err != nil {
		return 0, err
	}
	if len(in) == 0 {
		return 0, nil
	}

	// Collapse duplicate dedupe keys inside the batch first: ON CONFLICT DO
	// NOTHING would swallow the second claim, but the event insert must not
	// carry both or the timeline gains a duplicate the key table cannot see.
	keyed := make([]domain.Event, 0, len(in))
	unkeyed := make([]domain.Event, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, e := range in {
		if e.ID() == uuid.Nil {
			return 0, errs.Internal("event_id_missing", errsMissing("event id is required"))
		}
		k := e.DedupeKey()
		if k == "" {
			unkeyed = append(unkeyed, e)
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		keyed = append(keyed, e)
	}

	accepted := unkeyed
	if len(keyed) > 0 {
		won, err := r.claimDedupeKeys(ctx, s, keyed)
		if err != nil {
			return 0, err
		}
		for _, e := range keyed {
			if _, ok := won[e.DedupeKey()]; ok {
				accepted = append(accepted, e)
			}
		}
	}

	if len(accepted) == 0 {
		return 0, nil
	}

	n := len(accepted)
	ids := make([]uuid.UUID, n)
	alertIDs := make([]*uuid.UUID, n)
	occIDs := make([]*uuid.UUID, n)
	groupIDs := make([]*uuid.UUID, n)
	types := make([]string, n)
	occurredAt := make([]time.Time, n)
	recordedAt := make([]time.Time, n)
	actorKinds := make([]string, n)
	actorIDs := make([]*string, n)
	actorLabels := make([]*string, n)
	summaries := make([]string, n)
	payloads := make([][]byte, n)
	dedupeKeys := make([]*string, n)

	for i, e := range accepted {
		payload, err := jsonbAny(e.Payload())
		if err != nil {
			return 0, err
		}
		ids[i] = e.ID()
		alertIDs[i] = idPtr(e.AlertID())
		occIDs[i] = idPtr(e.OccurrenceID())
		groupIDs[i] = idPtr(e.GroupID())
		types[i] = e.Type().String()
		occurredAt[i] = e.OccurredAt()
		recordedAt[i] = e.RecordedAt()
		actorKinds[i] = e.Actor().Kind().String()
		actorIDs[i] = strPtr(e.Actor().ID())
		actorLabels[i] = strPtr(e.Actor().Label())
		summaries[i] = e.Summary()
		payloads[i] = payload
		dedupeKeys[i] = strPtr(e.DedupeKey())
	}

	if _, err := r.db(ctx).Exec(ctx, insertEventsSQL, s.OrgID(), ids, alertIDs, occIDs, groupIDs,
		types, occurredAt, recordedAt, actorKinds, actorIDs, actorLabels, summaries, payloads,
		dedupeKeys); err != nil {
		return 0, mapErr(err, "append alert events")
	}
	return n, nil
}

// claimDedupeKeys inserts the C.8 keys and reports which ones this transaction
// won. A key it did not win names an event that already exists.
func (r *EventRepository) claimDedupeKeys(
	ctx context.Context, s db.TenantScope, events []domain.Event,
) (map[string]struct{}, error) {
	keys := make([]string, len(events))
	ids := make([]uuid.UUID, len(events))
	for i, e := range events {
		keys[i] = e.DedupeKey()
		ids[i] = e.ID()
	}

	rows, err := r.db(ctx).Query(ctx, claimDedupeKeysSQL, s.OrgID(), keys, ids)
	if err != nil {
		return nil, mapErr(err, "claim event dedupe keys")
	}
	defer rows.Close()

	won := make(map[string]struct{}, len(events))
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, mapErr(err, "scan event dedupe key")
		}
		won[k] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read event dedupe keys")
	}
	return won, nil
}

// ---------------------------------------------------------------- the pruner

// pruneDedupeKeysSQL is the only reader of `alert_event_keys_prune_idx
// (created_at)`, the index 00007 created FOR THIS STATEMENT and for no other — the
// idempotency probe itself rides the PRIMARY KEY, so until this query existed the
// index was an unbounded write cost serving nothing. Its INNER SELECT is the half
// that rides it, and only when the planner can see that the tail past the horizon
// is a small fraction of the table: measured on PG16 that case is a `Bitmap Index
// Scan on alert_event_keys_prune_idx`, while a first sweep over a mostly-expired
// table — or a generic plan, which a long-lived pooled connection may settle on
// once pgx has prepared this statement — reasonably takes a `Seq Scan` instead.
// Either is bounded: the LIMIT stops the scan at `batch` matching rows, and on an
// append-only table heap order tracks `created_at`, so the expired rows are the
// ones a sequential scan meets first.
//
// The inner SELECT is what bounds the tick. A bare `DELETE ... WHERE created_at <
// $1 LIMIT` is not legal SQL and an unbounded `DELETE ... WHERE created_at < $1`
// is the outage this shape exists to avoid: the first sweep on an install that has
// been growing this table since it shipped would otherwise be one statement over
// tens of millions of rows.
//
// ⛔ IT MATCHES ON `ctid`, AND THE OBVIOUS `(org_id, dedupe_key) IN (…)` IS THE
// TRAP — it makes the tick O(table) while reading as O(batch). The planner does
// not re-probe the PRIMARY KEY for each returned tuple; measured on PG16 with
// 400k ANALYZEd rows it takes a `Hash Semi Join` whose outer input is a full `Seq
// Scan on alert_event_keys`, so every hourly tick reads the whole table however
// small the batch is — the growing table this sweep exists to bound is the very
// thing it would then scan. Matching on `ctid` gets the plan the shape implies:
//
//	Delete on alert_event_keys
//	  ->  Nested Loop
//	        ->  HashAggregate  (the bounded batch)
//	        ->  Tid Scan on alert_event_keys  (actual rows=1 loops=20000)
//	              TID Cond: (ctid = "ANY_subquery".ctid)
//
// ⚠️ `ctid` IS ONLY SAFE HERE BECAUSE THIS TABLE IS APPEND-ONLY. A row's ctid moves
// when the row is UPDATEd, and nothing updates `alert_event_keys` — the C.8 write
// is `INSERT ... ON CONFLICT DO NOTHING` and this sweep is the only DELETE, so a
// tid captured by the subquery still names the same key when the outer half reaches
// it (one statement, one snapshot). Should anything ever UPDATE this table, this
// query has to go back to matching the key.
const pruneDedupeKeysSQL = `
DELETE FROM alert_event_keys
 WHERE ctid IN (
   SELECT ctid FROM alert_event_keys WHERE created_at < $1 LIMIT $2)`

// PruneDedupeKeys deletes claimed C.8 keys past the horizon, in bounded batches,
// and returns how many went.
//
// ⚠️ UNSCOPED BY DESIGN, exactly like `SessionRepository.DeleteExpired` and
// `DedupRepository.Prune`: it is a maintenance sweep across every tenant, run by
// `retention.prune` and reachable from no request. The horizon is passed in
// rather than written as `now() - interval '30 days'` so the clock stays
// injectable — a sweeper whose window only exists in SQL is a sweeper no test can
// pin.
//
// ⚠️ THE COLUMN IT KEYS ON HAS TWO WRITERS WITH TWO CLOCKS (00034 says so): this
// repository lets the DEFAULT stamp `created_at` from the database, while
// `notification/repository/events.go` names it from the injected clock. Both are
// wall clocks and the horizon is thirty days, so skew between them is immaterial
// here; it is recorded because this sweep is now the reader that makes the column
// mean something.
func (r *EventRepository) PruneDedupeKeys(ctx context.Context, before time.Time, batch int) (int64, error) {
	if batch <= 0 {
		batch = 1000
	}
	tag, err := r.db(ctx).Exec(ctx, pruneDedupeKeysSQL, before.UTC(), batch)
	if err != nil {
		return 0, mapErr(err, "prune alert event dedupe keys")
	}
	return tag.RowsAffected(), nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

// -------------------------------------------------------------------- reads

// ListByAlert is the alert-scoped timeline, newest first (ev_alert_idx).
func (r *EventRepository) ListByAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, w db.TimeWindow, p db.Keyset,
) ([]domain.Event, db.Cursor, error) {
	return r.listBy(ctx, s, "alert_id", alertID, w, p)
}

// ListByOccurrence is the episode-scoped timeline (ev_occ_idx).
func (r *EventRepository) ListByOccurrence(
	ctx context.Context, s db.TenantScope, occID uuid.UUID, w db.TimeWindow, p db.Keyset,
) ([]domain.Event, db.Cursor, error) {
	return r.listBy(ctx, s, "occurrence_id", occID, w, p)
}

// ListByGroup is §D.12(b), the GROUP TIMELINE — the signature UI view
// (ev_group_idx). The API defaults the window's lower bound to
// `group.first_seen_at`.
func (r *EventRepository) ListByGroup(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID, w db.TimeWindow, p db.Keyset,
) ([]domain.Event, db.Cursor, error) {
	return r.listBy(ctx, s, "group_id", groupID, w, p)
}

// listBy is the one timeline query, parameterised by subject column. The column
// name comes from a closed set of literals in this file and never from a caller,
// so there is no injection surface.
func (r *EventRepository) listBy(
	ctx context.Context, s db.TenantScope, column string, subject uuid.UUID,
	w db.TimeWindow, p db.Keyset,
) ([]domain.Event, db.Cursor, error) {
	if err := requireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	if err := requireID("subject id", subject); err != nil {
		return nil, db.Cursor{}, err
	}
	// ⭐ The window's lower bound is REQUIRED. `recorded_at` is the partition key
	// of alert_events; without it the planner scans thirteen months of partitions
	// instead of the one the caller meant.
	if w.From.IsZero() {
		return nil, db.Cursor{}, errs.Internal("event_window_unbounded",
			errsMissing("an event query requires a lower time bound"))
	}
	to := w.To
	if to.IsZero() {
		to = r.clock.Now()
	}
	if !to.After(w.From) {
		return nil, db.Cursor{}, errs.Validation("time_window_invalid",
			"the time window must end after it starts")
	}
	limit := clampLimit(p.Limit)

	sql := `SELECT ` + eventColumns + `
	          FROM alert_events
	         WHERE org_id = $1 AND ` + column + ` = $2
	           AND recorded_at >= $3 AND recorded_at < $4
	           AND ($5::timestamptz IS NULL OR (recorded_at, id) < ($5, $6))
	         ORDER BY recorded_at DESC, id DESC
	         LIMIT $7`

	var cursorAt *time.Time
	var cursorID uuid.UUID
	if !p.Cursor.IsZero() {
		cursorAt = timePtr(p.Cursor.SortKey)
		cursorID = p.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, sql, s.OrgID(), subject, w.From.UTC(), to.UTC(),
		cursorAt, cursorID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "list alert events")
	}
	defer rows.Close()

	collected, err := collectEvents(rows, limit+1)
	if err != nil {
		return nil, db.Cursor{}, err
	}
	page, hasMore := pageOf(collected, limit)
	if len(page) == 0 {
		return nil, db.Cursor{Hash: p.Cursor.Hash}, nil
	}
	last := page[len(page)-1]
	return page, nextCursor(last.RecordedAt(), last.ID(), p.Cursor.Hash, hasMore), nil
}

func collectEvents(rows pgx.Rows, capacity int) ([]domain.Event, error) {
	out := make([]domain.Event, 0, capacity)
	for rows.Next() {
		var row eventRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "scan alert event")
		}
		e, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read alert events")
	}
	return out, nil
}

const stateChangeCountsSQL = `
SELECT alert_id, count(*)
  FROM alert_events
 WHERE org_id = $1
   AND recorded_at >= $2 AND recorded_at < $3
   AND alert_id IS NOT NULL
   AND type IN ('occurrence.opened','occurrence.reopened','occurrence.resolved',
                'occurrence.expired','occurrence.suppressed','occurrence.unsuppressed')
 GROUP BY alert_id`

// StateChangeCounts counts lifecycle transitions per Alert inside a window. It
// is the input to the `flap.score` job (§B.6), which is an EWMA of state
// transitions per hour.
//
// NOTE (planner): ev_type_idx is (org_id, type, recorded_at DESC), so this is
// six index ranges aggregated — the partition pruning from the window is what
// keeps it cheap. There is no (org_id, recorded_at, alert_id) index and adding
// one is a migration this module does not own.
func (r *EventRepository) StateChangeCounts(
	ctx context.Context, s db.TenantScope, w db.TimeWindow,
) (map[uuid.UUID]int, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	if w.From.IsZero() {
		return nil, errs.Internal("event_window_unbounded",
			errsMissing("a state-change count requires a lower time bound"))
	}
	to := w.To
	if to.IsZero() {
		to = r.clock.Now()
	}

	rows, err := r.db(ctx).Query(ctx, stateChangeCountsSQL, s.OrgID(), w.From.UTC(), to.UTC())
	if err != nil {
		return nil, mapErr(err, "count state changes")
	}
	defer rows.Close()

	out := map[uuid.UUID]int{}
	for rows.Next() {
		var id uuid.UUID
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, mapErr(err, "scan state change count")
		}
		out[id] = int(n)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read state change counts")
	}
	return out, nil
}
