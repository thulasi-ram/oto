package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/incidents/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// CorrelatorRepository is every statement against `correlators` and
// `correlator_matches` (migration 00085), and the reads the evaluator makes of the
// Incident and signal tables to decide what a Correlator may do with one Case.
//
// ⭐ IT READS `alert_cases`, `alerts` AND `incident_members` IN SQL FOR
// IncidentRepository's REASON (CONTEXT.md §4's fourth mechanism): "is this Case
// in an Incident, was it ever taken out of one, and what is it labelled" is one
// statement here and three round trips through ports. It never writes a signal
// table, and it never writes `incident_members` either — membership changes go
// through IncidentRepository, the one writer of that table.
//
// ⛔ EVERY METHOD IS TENANT-SCOPED, `org_id` in every predicate.
type CorrelatorRepository struct {
	q db.Querier
}

// NewCorrelatorRepository builds the repository over a fallback querier; a
// transaction travelling in the context wins over it.
func NewCorrelatorRepository(q db.Querier) *CorrelatorRepository {
	return &CorrelatorRepository{q: q}
}

func (r *CorrelatorRepository) db(ctx context.Context) db.Querier {
	return db.FromContext(ctx, r.q)
}

// correlatorMapErr is mapErr under the Correlator's not-found code, so a missing
// row is `correlator_not_found` and a duplicate live name is a 409 naming
// `correlators_name_uniq`.
func correlatorMapErr(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CorrelatorNotFound()
	}
	return mapErr(err, what)
}

// matcherJSON is the stored shape of one element of `correlators.matchers`, which
// is `notification_policies.matchers`' shape exactly.
type matcherJSON struct {
	Name  string `json:"name"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

func encodeMatchers(ms []kernel.Matcher) ([]byte, error) {
	out := make([]matcherJSON, 0, len(ms))
	for _, m := range ms {
		out = append(out, matcherJSON{Name: m.Name, Op: string(m.Op), Value: m.Value})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, errs.Internal("correlator_matchers_encode", err)
	}
	return b, nil
}

const correlatorColumns = `
  id, name, priority, enabled, matchers, count_min, count_window_s, quiet_grace_s,
  incidents_are_conversations, created_at, updated_at`

// correlatorRow is the row model of `correlators`. Unexported, per the three-model
// rule.
type correlatorRow struct {
	id        uuid.UUID
	name      string
	priority  int
	enabled   bool
	matchers  []byte
	countMin  *int
	countWinS *int
	graceS    *int
	convs     bool
	createdAt time.Time
	updatedAt time.Time
}

func (r *correlatorRow) scanInto() []any {
	return []any{&r.id, &r.name, &r.priority, &r.enabled, &r.matchers,
		&r.countMin, &r.countWinS, &r.graceS, &r.convs, &r.createdAt, &r.updatedAt}
}

func (r correlatorRow) toDomain() (domain.Correlator, error) {
	var ms []matcherJSON
	if len(r.matchers) > 0 {
		if err := json.Unmarshal(r.matchers, &ms); err != nil {
			return domain.Correlator{}, errs.Internal("correlator_matchers_invalid", err)
		}
	}
	c := domain.Correlator{
		ID: r.id, Name: r.name, Priority: r.priority, Enabled: r.enabled,
		Matchers:  make([]kernel.Matcher, 0, len(ms)),
		CreatedAt: r.createdAt.UTC(), UpdatedAt: r.updatedAt.UTC(),
	}
	for _, m := range ms {
		c.Matchers = append(c.Matchers, kernel.Matcher{Name: m.Name, Op: kernel.MatchOp(m.Op), Value: m.Value})
	}
	if r.countMin != nil && r.countWinS != nil {
		c.Count = domain.Count{Min: *r.countMin, Window: time.Duration(*r.countWinS) * time.Second}
	}
	if r.graceS != nil {
		c.QuietGrace = time.Duration(*r.graceS) * time.Second
	}
	c.Conversations = r.convs
	return c, nil
}

// graceArg renders the grace as its nullable column: NULL for none.
func graceArg(g time.Duration) *int {
	if g <= 0 {
		return nil
	}
	s := int(g / time.Second)
	return &s
}

func countArgs(c domain.Count) (*int, *int) {
	if !c.Enabled() {
		return nil, nil
	}
	m, w := c.Min, int(c.Window/time.Second)
	return &m, &w
}

func (r *CorrelatorRepository) scanMany(rows pgx.Rows, what string) ([]domain.Correlator, error) {
	defer rows.Close()
	var out []domain.Correlator
	for rows.Next() {
		var row correlatorRow
		if err := rows.Scan(row.scanInto()...); err != nil {
			return nil, mapErr(err, what)
		}
		c, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, mapErr(rows.Err(), what)
}

// ⭐ THE OPERATOR'S ORDER, AND THE SAME ORDER FOR THE SETTINGS LIST AND THE WALK:
// priority, LOWER FIRST, then age, then id — `notification_policies`' tie-break —
// so the list an operator reorders is the list the evaluator walks. A settings page
// showing them in another order would make "why did the second one draw it?"
// unanswerable.
const listCorrelatorsSQL = `SELECT` + correlatorColumns + `
  FROM correlators
 WHERE org_id = $1 AND deleted_at IS NULL
 ORDER BY priority, created_at, id`

// List returns every live Correlator in the org, disabled ones included, in
// evaluation order.
func (r *CorrelatorRepository) List(ctx context.Context, s db.TenantScope) ([]domain.Correlator, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, listCorrelatorsSQL, s.OrgID())
	if err != nil {
		return nil, mapErr(err, "list correlators")
	}
	return r.scanMany(rows, "list correlators")
}

// Rides `correlators_eval_idx`, whose key is exactly this ORDER BY.
const liveCorrelatorsSQL = `SELECT` + correlatorColumns + `
  FROM correlators
 WHERE org_id = $1 AND enabled AND deleted_at IS NULL
 ORDER BY priority, created_at, id`

// Live returns the Correlators the evaluator walks: enabled, not deleted, in order.
func (r *CorrelatorRepository) Live(ctx context.Context, s db.TenantScope) ([]domain.Correlator, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, liveCorrelatorsSQL, s.OrgID())
	if err != nil {
		return nil, mapErr(err, "read live correlators")
	}
	return r.scanMany(rows, "read live correlators")
}

const getCorrelatorSQL = `SELECT` + correlatorColumns + `
  FROM correlators
 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`

// Get reads one live Correlator. A deleted one is not found: it draws nothing and
// cannot be edited, and its name survives only on the Incidents it drew.
func (r *CorrelatorRepository) Get(ctx context.Context, s db.TenantScope, correlatorID uuid.UUID) (domain.Correlator, error) {
	return r.getWith(ctx, s, getCorrelatorSQL, correlatorID)
}

// Lock is Get with the row lock taken for the rest of the caller's transaction.
//
// ⭐ IT IS WHAT MAKES ONE CORRELATOR'S DECISIONS SERIAL. Two Cases opening in the
// same second, both matching, are two `incidents.correlate` jobs on two workers;
// without this each would read "no active Incident of mine, threshold met" and
// each would draw one, and the storm would become two stories. Under it the second
// waits for the first to commit and then sees the Incident the first drew, and
// joins it. A PATCH takes the same lock, so an edit never lands between a
// decision's read and its write.
func (r *CorrelatorRepository) Lock(ctx context.Context, s db.TenantScope, correlatorID uuid.UUID) (domain.Correlator, error) {
	return r.getWith(ctx, s, getCorrelatorSQL+` FOR UPDATE`, correlatorID)
}

func (r *CorrelatorRepository) getWith(
	ctx context.Context, s db.TenantScope, sql string, correlatorID uuid.UUID,
) (domain.Correlator, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Correlator{}, err
	}
	var row correlatorRow
	if err := r.db(ctx).QueryRow(ctx, sql, s.OrgID(), correlatorID).Scan(row.scanInto()...); err != nil {
		return domain.Correlator{}, correlatorMapErr(err, "read a correlator")
	}
	return row.toDomain()
}

const insertCorrelatorSQL = `
INSERT INTO correlators
  (id, org_id, name, priority, enabled, matchers, count_min, count_window_s, quiet_grace_s,
   incidents_are_conversations, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $10, $11, $9, $9)
RETURNING` + correlatorColumns

// Create writes one Correlator. The caller has validated it.
func (r *CorrelatorRepository) Create(
	ctx context.Context, s db.TenantScope, c domain.Correlator, at time.Time,
) (domain.Correlator, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Correlator{}, err
	}
	matchers, err := encodeMatchers(c.Matchers)
	if err != nil {
		return domain.Correlator{}, err
	}
	countMin, countWin := countArgs(c.Count)
	var row correlatorRow
	if err := r.db(ctx).QueryRow(ctx, insertCorrelatorSQL,
		id.New(), s.OrgID(), c.Name, c.Priority, c.Enabled, matchers, countMin, countWin, at.UTC(),
		graceArg(c.QuietGrace), c.Conversations,
	).Scan(row.scanInto()...); err != nil {
		return domain.Correlator{}, correlatorMapErr(err, "create a correlator")
	}
	return row.toDomain()
}

// ⭐ A WHOLE-ROW WRITE OF THE MERGED CORRELATOR, under the lock the service took
// to read it. `GREATEST` for the reason every app-clocked table carries one: N pods
// have N clocks and `correlators_time_ck` must not fail on a few milliseconds of
// skew.
const updateCorrelatorSQL = `
UPDATE correlators
   SET name = $3, priority = $4, enabled = $5, matchers = $6,
       count_min = $7, count_window_s = $8, quiet_grace_s = $10,
       incidents_are_conversations = $11,
       updated_at = GREATEST($9, created_at)
 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
RETURNING` + correlatorColumns

// Update writes the merged Correlator back.
func (r *CorrelatorRepository) Update(
	ctx context.Context, s db.TenantScope, c domain.Correlator, at time.Time,
) (domain.Correlator, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Correlator{}, err
	}
	matchers, err := encodeMatchers(c.Matchers)
	if err != nil {
		return domain.Correlator{}, err
	}
	countMin, countWin := countArgs(c.Count)
	var row correlatorRow
	if err := r.db(ctx).QueryRow(ctx, updateCorrelatorSQL,
		s.OrgID(), c.ID, c.Name, c.Priority, c.Enabled, matchers, countMin, countWin, at.UTC(),
		graceArg(c.QuietGrace), c.Conversations,
	).Scan(row.scanInto()...); err != nil {
		return domain.Correlator{}, correlatorMapErr(err, "update a correlator")
	}
	return row.toDomain()
}

const deleteCorrelatorSQL = `
UPDATE correlators
   SET deleted_at = GREATEST($3, created_at), updated_at = GREATEST($3, created_at)
 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`

// Delete retires a Correlator: it draws nothing from now on, and every Incident and
// membership that names it keeps naming it (migration 00085, soft delete).
func (r *CorrelatorRepository) Delete(ctx context.Context, s db.TenantScope, correlatorID uuid.UUID, at time.Time) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx, deleteCorrelatorSQL, s.OrgID(), correlatorID, at.UTC())
	if err != nil {
		return mapErr(err, "delete a correlator")
	}
	if tag.RowsAffected() == 0 {
		return domain.CorrelatorNotFound()
	}
	return nil
}

// ------------------------------------------------------------- the evaluator

// ⭐ ONE STATEMENT ANSWERS EVERYTHING THE EVALUATOR ASKS OF THE CASE. Its labels
// (the matchers' input), its start (the window's anchor), and the two facts that
// make every Correlator skip it: a CURRENT membership — a Case is in at most one
// Incident — and ANY tombstone, because a Case a human ever removed or moved is
// never put back by a machine (ADR 0052 §4). A moved Case has a live membership
// as well as a tombstone; either alone is enough.
//
// ⛔ AND A DRILL'S SYNTHETIC CASE IS NEVER CLAIMED. A delivery drill pushes one
// synthetic alert through the real pipeline to prove a notification arrives
// (00039); a Correlator drawing it into an Incident would declare that Incident
// to the org's incident tool — a drill opening a real external incident — and the
// drill's disposal would then leave an Incident with no members behind. So
// `a.synthetic` reads as "already decided", the same answer as a live membership.
//
// ⭐ FOR SHARE OF c, the same Case lock `casesSQL` takes for the human verbs, and
// taken FIRST for the same reason: the evaluator then locks the Correlator and the
// Incident, so the order stays Case → Incident on every path.
const correlationCaseSQL = `
SELECT c.id, c.number, c.alert_id, c.started_at, a.labels, a.synthetic,
       EXISTS (SELECT 1 FROM incident_members m
                WHERE m.org_id = c.org_id AND m.case_id = c.id AND m.removed_at IS NULL),
       EXISTS (SELECT 1 FROM incident_members m
                WHERE m.org_id = c.org_id AND m.case_id = c.id AND m.removed_at IS NOT NULL)
  FROM alert_cases c
  JOIN alerts a ON a.id = c.alert_id
 WHERE c.org_id = $1 AND c.id = $2
   FOR SHARE OF c`

// CorrelationCase reads the Case the evaluator is deciding. A Case the org does not
// have is `case_not_found`.
func (r *CorrelatorRepository) CorrelationCase(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID,
) (domain.CorrelationCase, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.CorrelationCase{}, err
	}
	var (
		out    domain.CorrelationCase
		labels []byte
	)
	var synthetic bool
	err := r.db(ctx).QueryRow(ctx, correlationCaseSQL, s.OrgID(), caseID).Scan(
		&out.ID, &out.Number, &out.AlertID, &out.StartedAt, &labels, &synthetic, &out.InIncident, &out.EverRemoved)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.CorrelationCase{}, domain.CaseNotFound()
		}
		return domain.CorrelationCase{}, mapErr(err, "read a case to correlate")
	}
	out.StartedAt = out.StartedAt.UTC()
	out.Synthetic = synthetic
	out.Labels = map[string]string{}
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &out.Labels); err != nil {
			return domain.CorrelationCase{}, errs.Internal("correlation_case_labels_invalid", err)
		}
	}
	return out, nil
}

// ⭐ FIRST CLAIM STANDS. `correlator_matches` is keyed by the Case, so a retried job
// — or a configuration edit between two attempts — cannot make a second Correlator
// claim a Case the first already did; the second INSERT is a no-op.
const recordMatchSQL = `
INSERT INTO correlator_matches (case_id, org_id, correlator_id, case_started_at, matched_at)
VALUES ($2, $1, $3, $4, $5)
ON CONFLICT (case_id) DO NOTHING`

// RecordMatch records that correlatorID claimed the Case.
func (r *CorrelatorRepository) RecordMatch(
	ctx context.Context, s db.TenantScope, correlatorID uuid.UUID, c domain.CorrelationCase, at time.Time,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	_, err := r.db(ctx).Exec(ctx, recordMatchSQL, s.OrgID(), c.ID, correlatorID, c.StartedAt.UTC(), at.UTC())
	return mapErr(err, "record a correlator match")
}

// ⭐ THE COUNT'S NUMERATOR AND THE DRAW'S MEMBERSHIP, IN ONE READ. The Cases this
// Correlator claimed whose start is inside [from, to] and that are still FREE: not
// a current member of any Incident (one drawn earlier by this Correlator, or by a
// human, already holds them) and never removed by a human. A Case that has left
// the window, joined something or been taken out by a person does not count
// towards a new story and is not drawn into one. Rides
// `correlator_matches_window_idx`.
const windowCandidatesSQL = `
SELECT c.id, c.number, c.alert_id, x.case_started_at
  FROM correlator_matches x
  JOIN alert_cases c ON c.id = x.case_id AND c.org_id = x.org_id
 WHERE x.org_id = $1 AND x.correlator_id = $2
   AND x.case_started_at BETWEEN $3 AND $4
   AND x.case_id <> $5
   AND NOT EXISTS (SELECT 1 FROM incident_members m WHERE m.org_id = x.org_id AND m.case_id = x.case_id)
 ORDER BY x.case_started_at, x.case_id`

// WindowCandidates returns the free Cases correlatorID claimed inside [from, to],
// excluding the one being evaluated (the caller adds it, so a claim lost to a
// retry's first attempt still counts this Case exactly once).
//
// `NOT EXISTS` over ANY membership row covers both rules at once: a current one is
// "already in an Incident", a tombstone is "a human took it out".
func (r *CorrelatorRepository) WindowCandidates(
	ctx context.Context, s db.TenantScope, correlatorID uuid.UUID, from, to time.Time, except uuid.UUID,
) ([]domain.CaseAt, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, windowCandidatesSQL, s.OrgID(), correlatorID, from.UTC(), to.UTC(), except)
	if err != nil {
		return nil, mapErr(err, "read a correlator's window")
	}
	defer rows.Close()
	var out []domain.CaseAt
	for rows.Next() {
		var c domain.CaseAt
		if err := rows.Scan(&c.ID, &c.Number, &c.AlertID, &c.StartedAt); err != nil {
			return nil, mapErr(err, "read a correlator's window")
		}
		c.StartedAt = c.StartedAt.UTC()
		out = append(out, c)
	}
	return out, mapErr(rows.Err(), "read a correlator's window")
}

// ⭐ ONLY AN INCIDENT THIS CORRELATOR DREW. A human-drawn Incident never grows by
// itself (ADR 0052 §4), so the predicate is the author column and nothing else;
// rides `incidents_correlator_idx`. The LATEST one only: a Correlator's Incidents
// are drawn one after another — a new one is drawn only when the last could not be
// joined — so the latest is the only one a Case could join.
//
// ⭐ "WENT QUIET" IS THE THIRD COLUMN AND IT IS READ, NOT STORED (migration 00086):
// the latest close among the CURRENT members — the close that left no member open
// — or the latest removal, when a human taking the last open Case out is what
// quieted it. `GREATEST` ignores a NULL side, and the draw instant is the floor
// for an Incident with neither (every member removed before any closed). The one
// imprecision is conceded: removing an already-closed member from an already-quiet
// Incident moves this later, which can only lengthen a grace, never cut one short.
const latestDrawnBySQL = `
SELECT i.id, i.number,
       (SELECT count(*)::int
          FROM incident_members m
          JOIN alert_cases c ON c.id = m.case_id
         WHERE m.incident_id = i.id AND m.removed_at IS NULL AND c.state = 'open'),
       COALESCE(GREATEST(
         (SELECT max(c.ended_at)
            FROM incident_members m
            JOIN alert_cases c ON c.id = m.case_id
           WHERE m.incident_id = i.id AND m.removed_at IS NULL),
         (SELECT max(m.removed_at) FROM incident_members m WHERE m.incident_id = i.id)
       ), i.drawn_at)
  FROM incidents i
 WHERE i.org_id = $1 AND i.drawn_by_correlator_id = $2
 ORDER BY i.number DESC
 LIMIT 1`

// LatestDrawn is the most recent Incident correlatorID drew, with its open-member
// count — the one number its derived state is read off. ok is false when the
// Correlator has drawn none.
func (r *CorrelatorRepository) LatestDrawn(
	ctx context.Context, s db.TenantScope, correlatorID uuid.UUID,
) (domain.CorrelatorIncident, bool, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.CorrelatorIncident{}, false, err
	}
	var out domain.CorrelatorIncident
	err := r.db(ctx).QueryRow(ctx, latestDrawnBySQL, s.OrgID(), correlatorID).
		Scan(&out.ID, &out.Number, &out.OpenMembers, &out.QuietSince)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.CorrelatorIncident{}, false, nil
		}
		return domain.CorrelatorIncident{}, false, mapErr(err, "read a correlator's latest incident")
	}
	out.QuietSince = out.QuietSince.UTC()
	return out, true, nil
}
