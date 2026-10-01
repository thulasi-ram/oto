package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/incidents/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// IncidentRepository is every statement against `incidents`, `incident_members`
// and `org_incident_numbers` (migration 00083), and every READ this module makes of
// the signal tables.
//
// ⭐⭐ IT READS `alert_cases` AND `alerts` IN SQL, AND THAT IS CONTEXT.md §4's
// FOURTH MECHANISM USED ON PURPOSE. An Incident's state is DERIVED from whether its
// member Cases are open (ADR 0052 §3), on every read, for every row of a list. A
// port into `alerts/service` would answer that one Case at a time — an N+1 on the
// list page — or grow an alerts method whose only caller is this module. A JOIN
// answers it in the statement that reads the Incident, so the state can never be
// a second, staler copy of what the Cases say. `notification/repository` reads
// `alert_sources` and `stats` reads ten tables it does not own on the same terms.
//
// ⛔ IT NEVER WRITES A SIGNAL TABLE. Nothing here INSERTs into or UPDATEs `alerts`,
// `alert_cases` or `alert_events`: the timeline fact a membership change produces
// goes through `alerts/service.AppendTimelineEvent`, behind a port this module's
// service declares, inside the same transaction.
//
// ⛔ EVERY METHOD IS TENANT-SCOPED, AND `org_id` IS IN EVERY PREDICATE rather than
// implied by an id, so another org's Incident number and another org's Case id are
// the same answer as ones that never existed: 404.
type IncidentRepository struct {
	q db.Querier
}

// NewIncidentRepository builds the repository over a fallback querier; a
// transaction travelling in the context wins over it.
func NewIncidentRepository(q db.Querier) *IncidentRepository {
	return &IncidentRepository{q: q}
}

func (r *IncidentRepository) db(ctx context.Context) db.Querier {
	return db.FromContext(ctx, r.q)
}

// TxRunner is this module's unit of work: the membership rows and the timeline
// facts about them commit together or not at all.
type TxRunner = db.TxRunner

// NewTxRunner builds the unit of work over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return db.NewTxRunner(pool) }

// mapErr is the §L.9 translation under this module's codes.
func mapErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "not_found",
		NotFoundMessage:    "no such row",
		QueryFailed:        "incidents_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
	})
}

// liveUniq is the AT-MOST-ONE index (migration 00083). A 23505 naming it is the
// rule doing its job under a race the service's read could not see, and it is
// translated to the same `case_in_incident` the read would have produced.
const liveUniq = "incident_members_case_live_uniq"

// ------------------------------------------------------------------ the summary

// ⭐ ONE SUMMARY STATEMENT SERVES THE LIST AND THE DETAIL, so the two pages cannot
// derive the state two different ways. It counts CURRENT members only — the
// `removed_at IS NULL` is in the JOIN, not the WHERE, so an Incident whose every
// member was removed still has a row, with zero members, and reads `quiet`.
//
// `array_agg(DISTINCT … ORDER BY …)` is legal because the ORDER BY is the DISTINCT
// expression itself; the slice caps the alertnames at domain.MaxAlertnames.
const summarySelect = `
SELECT i.id, i.number, i.drawn_at, i.drawn_by, i.drawn_by_label, i.drawn_by_correlator_id,
       count(c.id)::int AS member_count,
       (count(c.id) FILTER (WHERE c.state = 'open'))::int AS open_member_count,
       COALESCE((array_agg(DISTINCT a.alertname ORDER BY a.alertname)
                   FILTER (WHERE a.alertname IS NOT NULL))[1:10], '{}') AS alertnames
  FROM incidents i
  LEFT JOIN incident_members m ON m.incident_id = i.id AND m.removed_at IS NULL
  LEFT JOIN alert_cases c      ON c.id = m.case_id
  LEFT JOIN alerts a           ON a.id = c.alert_id`

// summaryRow is the row model of summarySelect. Unexported, per the three-model
// rule.
type summaryRow struct {
	id           uuid.UUID
	number       int64
	drawnAt      time.Time
	drawnBy      *uuid.UUID
	drawnByLabel *string
	correlatorID *uuid.UUID
	members      int
	open         int
	alertnames   []string
}

func (r *summaryRow) scanInto() []any {
	return []any{&r.id, &r.number, &r.drawnAt, &r.drawnBy, &r.drawnByLabel, &r.correlatorID,
		&r.members, &r.open, &r.alertnames}
}

func (r summaryRow) toDomain() (domain.Incident, error) {
	by, err := domain.Restore(deref(r.drawnBy), derefString(r.drawnByLabel), deref(r.correlatorID))
	if err != nil {
		return domain.Incident{}, err
	}
	names := r.alertnames
	if names == nil {
		names = []string{}
	}
	return domain.Incident{
		ID:              r.id,
		Number:          r.number,
		DrawnAt:         r.drawnAt.UTC(),
		DrawnBy:         by,
		MemberCount:     r.members,
		OpenMemberCount: r.open,
		Alertnames:      names,
	}, nil
}

// ⭐ NEWEST FIRST BY NUMBER, and the cursor is the last row's id re-read inside
// the org — the `case_policy_config` shape. `number` is the order a human expects
// and `incidents_number_uniq (org_id, number)` already serves it backwards; the
// generic cursor's timestamp slot has nothing better to carry, and a forged cursor
// naming another org's id finds no row and therefore no position.
const listSQL = summarySelect + `
 WHERE i.org_id = $1
   AND ($2::uuid IS NULL
        OR i.number < (SELECT number FROM incidents WHERE org_id = $1 AND id = $2))
 GROUP BY i.id
 ORDER BY i.number DESC
 LIMIT $3`

// List returns a keyset page of one org's Incidents, newest first.
func (r *IncidentRepository) List(
	ctx context.Context, s db.TenantScope, p db.Keyset,
) ([]domain.Incident, db.Cursor, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 25
	}
	var after *uuid.UUID
	if p.Cursor.ID != uuid.Nil {
		v := p.Cursor.ID
		after = &v
	}

	rows, err := r.db(ctx).Query(ctx, listSQL, s.OrgID(), after, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "list incidents")
	}
	defer rows.Close()

	out := make([]domain.Incident, 0, limit+1)
	for rows.Next() {
		var row summaryRow
		if err := rows.Scan(row.scanInto()...); err != nil {
			return nil, db.Cursor{}, mapErr(err, "list incidents")
		}
		inc, err := row.toDomain()
		if err != nil {
			return nil, db.Cursor{}, err
		}
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "list incidents")
	}

	page, hasMore := db.PageOf(out, limit)
	cursor := db.Cursor{Hash: p.Cursor.Hash}
	if hasMore {
		cursor = db.Cursor{ID: page[len(page)-1].ID, Hash: p.Cursor.Hash, HasMore: true}
	}
	return page, cursor, nil
}

const getSummarySQL = summarySelect + `
 WHERE i.org_id = $1 AND i.number = $2
 GROUP BY i.id`

// ⭐ EVERY SPELL, CURRENT AND REMOVED, in the order they joined. The Case and its
// Alert are INNER joined: a membership row cannot outlive its Case (CASCADE), and a
// Case cannot outlive its Alert, so an inner join loses nothing and a LEFT join
// would only make every column nullable for a row that cannot exist.
const membersSQL = `
SELECT m.case_id, c.number, c.state, c.alert_id, a.alertname, a.labels,
       m.added_at, m.added_by, m.added_by_label, m.added_by_correlator_id,
       m.removed_at, m.removed_by_label, t.number
  FROM incident_members m
  JOIN alert_cases c ON c.id = m.case_id AND c.org_id = m.org_id
  JOIN alerts a      ON a.id = c.alert_id
  LEFT JOIN incidents t ON t.id = m.moved_to_incident_id
 WHERE m.org_id = $1 AND m.incident_id = $2
 ORDER BY m.added_at, m.id`

type memberRow struct {
	caseID       uuid.UUID
	caseNumber   int64
	caseState    string
	alertID      uuid.UUID
	alertname    string
	labels       []byte
	addedAt      time.Time
	addedBy      *uuid.UUID
	addedByLabel *string
	correlatorID *uuid.UUID
	removedAt    *time.Time
	removedBy    *string
	movedTo      *int64
}

func (r *memberRow) scanInto() []any {
	return []any{&r.caseID, &r.caseNumber, &r.caseState, &r.alertID, &r.alertname, &r.labels,
		&r.addedAt, &r.addedBy, &r.addedByLabel, &r.correlatorID,
		&r.removedAt, &r.removedBy, &r.movedTo}
}

func (r memberRow) toDomain() (domain.Member, error) {
	state, err := kernel.NewCaseState(r.caseState)
	if err != nil {
		return domain.Member{}, errs.Internal("incident_member_case_state_invalid", err)
	}
	by, err := domain.Restore(deref(r.addedBy), derefString(r.addedByLabel), deref(r.correlatorID))
	if err != nil {
		return domain.Member{}, err
	}
	labels := map[string]string{}
	if len(r.labels) > 0 {
		if err := json.Unmarshal(r.labels, &labels); err != nil {
			return domain.Member{}, errs.Internal("incident_member_labels_invalid", err)
		}
	}
	m := domain.Member{
		CaseID:         r.caseID,
		CaseNumber:     r.caseNumber,
		CaseState:      state,
		AlertID:        r.alertID,
		Alertname:      r.alertname,
		Labels:         labels,
		AddedAt:        r.addedAt.UTC(),
		AddedBy:        by,
		RemovedByLabel: derefString(r.removedBy),
	}
	if r.removedAt != nil {
		m.RemovedAt = r.removedAt.UTC()
	}
	if r.movedTo != nil {
		m.MovedToNumber = *r.movedTo
	}
	return m, nil
}

// Get reads one Incident by the number a human quotes, with its whole membership
// history.
func (r *IncidentRepository) Get(ctx context.Context, s db.TenantScope, number int64) (domain.Detail, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Detail{}, err
	}
	var row summaryRow
	if err := r.db(ctx).QueryRow(ctx, getSummarySQL, s.OrgID(), number).Scan(row.scanInto()...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Detail{}, domain.NotFound()
		}
		return domain.Detail{}, mapErr(err, "read an incident")
	}
	inc, err := row.toDomain()
	if err != nil {
		return domain.Detail{}, err
	}

	rows, err := r.db(ctx).Query(ctx, membersSQL, s.OrgID(), inc.ID)
	if err != nil {
		return domain.Detail{}, mapErr(err, "read an incident's members")
	}
	defer rows.Close()
	members := make([]domain.Member, 0, inc.MemberCount)
	for rows.Next() {
		var mr memberRow
		if err := rows.Scan(mr.scanInto()...); err != nil {
			return domain.Detail{}, mapErr(err, "read an incident's members")
		}
		m, err := mr.toDomain()
		if err != nil {
			return domain.Detail{}, err
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return domain.Detail{}, mapErr(err, "read an incident's members")
	}
	return domain.Detail{Incident: inc, Members: members}, nil
}

const refSQL = `SELECT id, number FROM incidents WHERE org_id = $1 AND number = $2`

// Ref resolves an Incident number to the id its membership rows reference.
func (r *IncidentRepository) Ref(ctx context.Context, s db.TenantScope, number int64) (domain.Ref, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Ref{}, err
	}
	var ref domain.Ref
	if err := r.db(ctx).QueryRow(ctx, refSQL, s.OrgID(), number).Scan(&ref.ID, &ref.Number); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Ref{}, domain.NotFound()
		}
		return domain.Ref{}, mapErr(err, "resolve an incident number")
	}
	return ref, nil
}

const casesSQL = `
SELECT id, number, alert_id FROM alert_cases
 WHERE org_id = $1 AND id = ANY($2::uuid[])`

// Cases resolves Case ids inside the org. An id the org does not have is simply
// absent from the answer; the service decides what that means.
func (r *IncidentRepository) Cases(
	ctx context.Context, s db.TenantScope, ids []uuid.UUID,
) (map[uuid.UUID]domain.CaseRef, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, casesSQL, s.OrgID(), ids)
	if err != nil {
		return nil, mapErr(err, "resolve cases")
	}
	defer rows.Close()
	out := make(map[uuid.UUID]domain.CaseRef, len(ids))
	for rows.Next() {
		var c domain.CaseRef
		if err := rows.Scan(&c.ID, &c.Number, &c.AlertID); err != nil {
			return nil, mapErr(err, "resolve cases")
		}
		out[c.ID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "resolve cases")
	}
	return out, nil
}

const liveMembershipsSQL = `
SELECT m.case_id, i.id, i.number
  FROM incident_members m
  JOIN incidents i ON i.id = m.incident_id
 WHERE m.org_id = $1 AND m.case_id = ANY($2::uuid[]) AND m.removed_at IS NULL`

// LiveMemberships answers, for each Case that is in an Incident now, which one.
//
// ⚠️ THIS READ IS FOR THE MESSAGE, NOT FOR THE RULE. It is how a refusal can name
// the Incident holding the Case and the move that would take it out. The rule is
// `incident_members_case_live_uniq`, which AddMember meets whatever this said.
func (r *IncidentRepository) LiveMemberships(
	ctx context.Context, s db.TenantScope, caseIDs []uuid.UUID,
) (map[uuid.UUID]domain.Ref, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, liveMembershipsSQL, s.OrgID(), caseIDs)
	if err != nil {
		return nil, mapErr(err, "read live incident memberships")
	}
	defer rows.Close()
	out := map[uuid.UUID]domain.Ref{}
	for rows.Next() {
		var caseID uuid.UUID
		var ref domain.Ref
		if err := rows.Scan(&caseID, &ref.ID, &ref.Number); err != nil {
			return nil, mapErr(err, "read live incident memberships")
		}
		out[caseID] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read live incident memberships")
	}
	return out, nil
}

// ⭐ ALLOCATED BY ONE STATEMENT, the 00081 shape: the counter is bumped and its
// value consumed by the INSERT that names the Incident, so no Incident exists
// without a number and no second round trip lets two draws read the same one.
const insertSQL = `
WITH allocated AS (
  INSERT INTO org_incident_numbers (org_id, next_number)
       VALUES ($2, 2)
  ON CONFLICT (org_id) DO UPDATE
          SET next_number = org_incident_numbers.next_number + 1
    RETURNING next_number - 1 AS number
)
INSERT INTO incidents (id, org_id, number, drawn_at, drawn_by, drawn_by_label, drawn_by_correlator_id)
SELECT $1, $2, number, $3, $4, $5, $6 FROM allocated
RETURNING number`

// Insert draws one Incident with no members yet; the service adds them in the
// same transaction. It returns the number the database allocated.
func (r *IncidentRepository) Insert(
	ctx context.Context, s db.TenantScope, at time.Time, by domain.Attribution,
) (domain.Ref, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Ref{}, err
	}
	ref := domain.Ref{ID: id.New()}
	userID, label, correlatorID := attributionArgs(by)
	err := r.db(ctx).QueryRow(ctx, insertSQL,
		ref.ID, s.OrgID(), at.UTC(), userID, label, correlatorID).Scan(&ref.Number)
	if err != nil {
		return domain.Ref{}, mapErr(err, "draw an incident")
	}
	return ref, nil
}

const addMemberSQL = `
INSERT INTO incident_members
  (id, org_id, incident_id, case_id, added_at, added_by, added_by_label, added_by_correlator_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// AddMember writes one CURRENT membership.
//
// ⭐⭐ THIS STATEMENT IS WHERE AT-MOST-ONE IS ENFORCED. A Case already live in any
// Incident — this one or another — meets `incident_members_case_live_uniq` and is
// refused here, whatever the service read beforehand; the refusal is translated to
// `case_in_incident` so a race and a read produce the same code.
func (r *IncidentRepository) AddMember(
	ctx context.Context, s db.TenantScope, incidentID, caseID uuid.UUID, at time.Time, by domain.Attribution,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	userID, label, correlatorID := attributionArgs(by)
	_, err := r.db(ctx).Exec(ctx, addMemberSQL,
		id.New(), s.OrgID(), incidentID, caseID, at.UTC(), userID, label, correlatorID)
	if err != nil {
		mapped := mapErr(err, "add a case to an incident")
		if errs.CodeOf(mapped) == liveUniq {
			return domain.RaceLostToAnotherIncident()
		}
		return mapped
	}
	return nil
}

// ⭐ THE TOMBSTONE, guarded by `removed_at IS NULL`: of two concurrent removals or
// moves of one membership exactly one matches, and the other is told the Case is
// no longer here rather than writing a second tombstone over the first.
const removeMemberSQL = `
UPDATE incident_members
   SET removed_at = GREATEST($4, added_at), removed_by = $5, removed_by_label = $6,
       moved_to_incident_id = $7
 WHERE org_id = $1 AND incident_id = $2 AND case_id = $3 AND removed_at IS NULL`

// RemoveMember tombstones the current membership of caseID in incidentID. movedTo
// is the destination when the removal is half of a move, and uuid.Nil otherwise.
//
// `removed_at` is GREATEST-guarded for the reason every app-clocked table's
// timestamps are: N pods have N clocks, and a few milliseconds of skew must not
// write a removal before the addition it ends (`incident_members_time_ck`).
func (r *IncidentRepository) RemoveMember(
	ctx context.Context, s db.TenantScope, incidentID, caseID uuid.UUID,
	at time.Time, by domain.Attribution, movedTo uuid.UUID,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if !by.IsHuman() {
		// ⛔ ONLY A HUMAN REMOVES (ADR 0052 §4); the schema has no column that could
		// say a Correlator did.
		return errs.New(errs.KindInternal, "incident_removal_not_human",
			"only a human removes a Case from an Incident")
	}
	userID, label, _ := attributionArgs(by)
	var moved *uuid.UUID
	if movedTo != uuid.Nil {
		moved = &movedTo
	}
	tag, err := r.db(ctx).Exec(ctx, removeMemberSQL,
		s.OrgID(), incidentID, caseID, at.UTC(), userID, label, moved)
	if err != nil {
		return mapErr(err, "remove a case from an incident")
	}
	if tag.RowsAffected() == 0 {
		return domain.MemberNotFound()
	}
	return nil
}

// attributionArgs renders an attribution as the three nullable columns the
// schema spells it in.
func attributionArgs(a domain.Attribution) (*uuid.UUID, *string, *uuid.UUID) {
	if !a.IsHuman() {
		c := a.CorrelatorID()
		return nil, nil, &c
	}
	label := a.Label()
	if a.UserID() == uuid.Nil {
		return nil, &label, nil
	}
	u := a.UserID()
	return &u, &label, nil
}

func deref(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return uuid.Nil
	}
	return *p
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
