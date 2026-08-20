package repository

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// caseRow is the row model of `alert_cases`. Unexported, per the
// three-model rule.
type caseRow struct {
	id      uuid.UUID
	orgID   uuid.UUID
	alertID uuid.UUID
	// ⛔ `groupID *uuid.UUID` WAS HERE AND IS DELETED (git-bug `7570090`, migration
	// `00069`). It was `alert_cases.group_id`, which since 00051 WAS the membership —
	// one column per episode instead of an `alert_group_members` row saying the same
	// thing. The table it pointed at is dropped, so the membership is not re-derived
	// from somewhere else; there is nothing left to be a member OF.
	seq int32

	state             string
	suppressionReason *string
	suppressedBy      []byte

	startedAt      time.Time
	endedAt        *time.Time
	lastObservedAt time.Time

	sourceStartsAt  time.Time
	sourceEndsAt    *time.Time
	sourceUpdatedAt *time.Time

	resolveReason *string

	resolvePendingAt    *time.Time
	resolvePendingEndAt *time.Time

	stateVersion  int32
	suppressCount int32

	ackState     string
	ackedBy      *uuid.UUID
	ackedByLabel *string
	ackedAt      *time.Time
	ackNote      *string

	ruleSnapshotID *uuid.UUID
	value          *float64
	observedSkewMS int64
}

// ⛔ `"group_id"` LEFT THIS LIST (git-bug `7570090`, migration `00069`), AND THE LIST
// IS WHY IT COULD LEAVE SAFELY. `caseColumnList`, `scanDest` and every `RETURNING`
// share one ordering, so a column is removed in two places rather than in every
// statement that reads a Case — which is the mistake that compiles and then scans
// `seq` into `state`. 26 columns now, not 27.
var caseColumnList = []string{
	"id", "org_id", "alert_id", "seq", "state", "suppression_reason", "suppressed_by",
	"started_at", "ended_at", "last_observed_at", "source_starts_at", "source_ends_at",
	"source_updated_at", "resolve_reason",
	"resolve_pending_at", "resolve_pending_end_at", "state_version",
	"suppress_count", "ack_state", "acked_by",
	"acked_by_label", "acked_at", "ack_note", "rule_snapshot_id", "value", "observed_skew_ms",
}

var caseColumns = strings.Join(caseColumnList, ", ")

func (r *caseRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.alertID, &r.seq, &r.state, &r.suppressionReason,
		&r.suppressedBy, &r.startedAt, &r.endedAt, &r.lastObservedAt, &r.sourceStartsAt,
		&r.sourceEndsAt, &r.sourceUpdatedAt, &r.resolveReason,
		&r.resolvePendingAt, &r.resolvePendingEndAt,
		&r.stateVersion, &r.suppressCount, &r.ackState, &r.ackedBy, &r.ackedByLabel, &r.ackedAt, &r.ackNote, &r.ruleSnapshotID,
		&r.value, &r.observedSkewMS,
	}
}

func (r *caseRow) toDomain() (domain.Case, error) {
	state, err := domain.NewCaseState(r.state)
	if err != nil {
		return domain.Case{}, errs.Internal("case_state_invalid", err)
	}
	ack, err := domain.NewAckState(r.ackState)
	if err != nil {
		return domain.Case{}, errs.Internal("case_ack_state_invalid", err)
	}
	var sup domain.SuppressionReason
	if r.suppressionReason != nil {
		sup, err = domain.NewSuppressionReason(*r.suppressionReason)
		if err != nil {
			return domain.Case{}, errs.Internal("case_suppression_reason_invalid", err)
		}
	}
	var res domain.ResolveReason
	if r.resolveReason != nil {
		res, err = domain.NewResolveReason(*r.resolveReason)
		if err != nil {
			return domain.Case{}, errs.Internal("case_resolve_reason_invalid", err)
		}
	}
	// `suppressed_by` was SELECTed and scanned here for its whole life and then
	// never unmarshalled, so the one column that answers "which silence is muting
	// this?" reached no read path at all.
	//
	// A malformed value is NOT fatal. The column is Alertmanager's raw attribution
	// and the case is legible without it; refusing to rehydrate an alert
	// because a foreign system wrote an unexpected shape into an advisory column
	// would take the whole signal off the screen to protect a deep link.
	var suppressedBy domain.SuppressedBy
	if len(r.suppressedBy) > 0 {
		if err := suppressedBy.UnmarshalJSON(r.suppressedBy); err != nil {
			suppressedBy = domain.SuppressedBy{}
		}
	}

	o, err := domain.NewCase(domain.CaseParams{
		ID:      r.id,
		OrgID:   r.orgID,
		AlertID: r.alertID,
		// ⛔ `GroupID: idOrNil(r.groupID)` WAS HERE AND IS DELETED (git-bug `7570090`).
		//
		// ⚠️ `domain.CaseParams.GroupID` AND `domain.Case.GroupID()` STILL EXIST AND
		// NOW ANSWER `uuid.Nil` FOR EVERY CASE EVER REHYDRATED. That is not a hidden
		// consequence of this line, it is the only possible one: the column is dropped,
		// so there is no value to supply and every reader of `GroupID()` —
		// `alerts/api/map.go`, `alerts/service/service.go`'s event payload,
		// `alerts/service/lifecycle.go` — sees the zero. Deleting the domain field is
		// the right end state and it is not in this package; recorded here so the zero
		// is read as "the entity is gone" rather than as a rehydration bug.
		Seq:               int(r.seq),
		State:             state,
		SuppressionReason: sup,
		SuppressedBy:      suppressedBy,
		StartedAt:         r.startedAt,
		EndedAt:           timeOrZero(r.endedAt),
		LastObservedAt:    r.lastObservedAt,
		SourceStartsAt:    r.sourceStartsAt,
		SourceEndsAt:      timeOrZero(r.sourceEndsAt),
		SourceUpdatedAt:   timeOrZero(r.sourceUpdatedAt),
		ResolveReason:     res,
		// The DELAYED CLOSE's receipt (migration 00057). NULL on every row until an
		// operator configures a retention window, which rehydrates exactly the Case
		// this mapper built before the columns existed.
		ResolvePendingAt:    timeOrZero(r.resolvePendingAt),
		ResolvePendingEndAt: timeOrZero(r.resolvePendingEndAt),
		StateVersion:        int(r.stateVersion),
		SuppressCount:       int(r.suppressCount),
		AckState:            ack,
		AckedBy:             idOrNil(r.ackedBy),
		AckedByLabel:        strOrEmpty(r.ackedByLabel),
		AckedAt:             timeOrZero(r.ackedAt),
		AckNote:             strOrEmpty(r.ackNote),
		RuleSnapshotID:      idOrNil(r.ruleSnapshotID),
		Value:               r.value,
		ObservedSkew:        time.Duration(r.observedSkewMS) * time.Millisecond,
	})
	if err != nil {
		return domain.Case{}, errs.Internal("case_row_invalid", err)
	}
	return o, nil
}

// CaseRepository is the SQL over `alert_cases` — the table the
// authoritative §B.3 state machine runs on. It implements
// service.CaseRepository.
//
// It writes exactly what the domain machine produced. There is no `if` in this
// file that moves a state: assembling a Transition by hand is how a case
// acquires a state no §B.3 row permits.
type CaseRepository struct{ q db.Querier }

// NewCaseRepository builds the repository over a fallback querier.
func NewCaseRepository(q db.Querier) *CaseRepository {
	return &CaseRepository{q: q}
}

func (r *CaseRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// ------------------------------------------------------------------- open

// ⛔⛔ `group_id` LEFT THE COLUMN LIST **AND** `$4` LEFT THE SELECT LIST, AND THE
// SECOND HALF IS THE ONE THAT COULD HAVE CORRUPTED DATA (git-bug `7570090`,
// migration `00069`). An INSERT is the one shape where a dropped column is not
// automatically a loud 42703: remove the name and forget the value and the fourteen
// remaining values slide left by one, so `seq` is written into `state`, `started_at`
// into `seq`, and Postgres reports a type error at best and a CHECK violation at
// worst — but if the shifted types HAD lined up it would have written a wrong row and
// said nothing. The column count and the value count are 14 and 14; they are PREPAREd
// against a migrated container rather than counted by eye.
//
// The trailing placeholders are renumbered because `$4` is gone: what were `$5`…`$11`
// are now `$4`…`$10`, and `OpenCase` drops `in.GroupID` from its argument list. A
// bound-but-unreferenced `$11` would be a 42P18 at PREPARE, which is why the arity is
// verified and not assumed.
//
// The alert_id is re-proven to belong to the caller's org rather than trusted.
// alert_cases.org_id is denormalised, so writing a scope's org_id beside
// another org's alert_id would create a row that every org-scoped read agrees is
// ours — which is the shape a cross-tenant leak takes.
var openCaseSQL = `
INSERT INTO alert_cases (
    id, org_id, alert_id, seq, state, started_at, ended_at, last_observed_at,
    source_starts_at, source_ends_at, source_updated_at, value, observed_skew_ms,
    ack_state)
SELECT $1, a.org_id, a.id, $4, 'open', $5, NULL, $5, $6, $7, $8, $9, $10, 'unacked'
  FROM alerts a
 WHERE a.org_id = $2 AND a.id = $3
RETURNING ` + caseColumns

// OpenCase opens a new firing episode — T1 (first sighting) and T7 (a re-fire).
//
// A new case ALWAYS starts unacked: T10 says an acknowledgement does not
// survive into a new episode. The "at most one open case per alert"
// invariant is enforced by case_one_open_idx, in the database and not in Go, so a
// 23505 here is a genuine concurrency conflict rather than a race the
// application was expected to lose.
func (r *CaseRepository) OpenCase(
	ctx context.Context, s db.TenantScope, in domain.OpenCase,
) (domain.Case, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Case{}, err
	}
	if err := db.RequireID("case id", in.ID); err != nil {
		return domain.Case{}, err
	}
	if err := db.RequireID("alert_id", in.AlertID); err != nil {
		return domain.Case{}, err
	}
	if in.Seq < 1 {
		return domain.Case{}, errs.Internal("case_seq_invalid",
			errsMissing("seq must be >= 1"))
	}
	if in.StartedAt.IsZero() || in.SourceStartsAt.IsZero() {
		return domain.Case{}, errs.Internal("case_time_missing",
			errsMissing("started_at and source_starts_at are required"))
	}

	var row caseRow
	err := r.db(ctx).QueryRow(ctx, openCaseSQL,
		in.ID, s.OrgID(), in.AlertID, in.Seq, in.StartedAt.UTC(),
		in.SourceStartsAt.UTC(), in.SourceEndsAt, in.SourceUpdatedAt,
		in.Value, in.SkewMS,
	).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			return domain.Case{}, errs.NotFound("alert_not_found", "no such alert")
		}
		return domain.Case{}, mapErr(err, "open case")
	}
	return row.toDomain()
}

// ------------------------------------------------------------------- reads

// GetByID reads one case within the caller's org.
func (r *CaseRepository) GetByID(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) (domain.Case, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Case{}, err
	}
	var row caseRow
	err := r.db(ctx).QueryRow(ctx,
		`SELECT `+caseColumns+` FROM alert_cases WHERE org_id = $1 AND id = $2`,
		s.OrgID(), id).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			return domain.Case{}, errs.NotFound("case_not_found", "no such case")
		}
		return domain.Case{}, mapErr(err, "read case")
	}
	return row.toDomain()
}

// GetOpenByAlert reads the one open episode of an Alert, if there is one. At
// most one can exist (case_one_open_idx).
func (r *CaseRepository) GetOpenByAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
) (domain.Case, bool, error) {
	return r.optional(ctx, s,
		`SELECT `+caseColumns+`
		   FROM alert_cases
		  WHERE org_id = $1 AND alert_id = $2 AND ended_at IS NULL`,
		s.OrgID(), alertID)
}

// GetLatestByAlert reads the most recent episode of an Alert, open or ended.
// This is what the state machine reads to decide between T1, T7 and T8.
func (r *CaseRepository) GetLatestByAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
) (domain.Case, bool, error) {
	return r.optional(ctx, s,
		`SELECT `+caseColumns+`
		   FROM alert_cases
		  WHERE org_id = $1 AND alert_id = $2
		  ORDER BY seq DESC
		  LIMIT 1`,
		s.OrgID(), alertID)
}

// PreviousWithRuleSnapshot reads the newest episode of an Alert BEFORE beforeSeq
// that has a rule snapshot bound to it — the episode "has the rule changed since
// last time?" is answered against.
//
// ⭐ THE ORDER IS `seq`, NOT `started_at`. seq is the episode ordinal the state
// machine mints and case_seq_uniq makes unique per alert, so "the one before this
// one" is a fact about the row rather than a comparison of two timestamps that a
// backfill or a clock skew could tie. It rides case_alert_idx
// (org_id, alert_id, seq DESC) and reads exactly one row.
//
// ⛔ EPISODES WITH NO SNAPSHOT ARE STEPPED OVER, NOT STOPPED AT. A case
// that predates rule capture, or one whose capture never ran, holds no rule to
// compare — stopping there would report "nothing changed" for the one question
// this read exists to answer. It is the same choice `rules` makes with
// `LatestDefinition`, taken one table over.
func (r *CaseRepository) PreviousWithRuleSnapshot(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, beforeSeq int,
) (domain.Case, bool, error) {
	return r.optional(ctx, s,
		`SELECT `+caseColumns+`
		   FROM alert_cases
		  WHERE org_id = $1 AND alert_id = $2 AND seq < $3
		    AND rule_snapshot_id IS NOT NULL
		  ORDER BY seq DESC
		  LIMIT 1`,
		s.OrgID(), alertID, beforeSeq)
}

func (r *CaseRepository) optional(
	ctx context.Context, s db.TenantScope, sql string, args ...any,
) (domain.Case, bool, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Case{}, false, err
	}
	var row caseRow
	if err := r.db(ctx).QueryRow(ctx, sql, args...).Scan(row.scanDest()...); err != nil {
		if isNoRows(err) {
			return domain.Case{}, false, nil
		}
		return domain.Case{}, false, mapErr(err, "read case")
	}
	o, err := row.toDomain()
	if err != nil {
		return domain.Case{}, false, err
	}
	return o, true, nil
}

var latestByAlertsSQL = `
SELECT DISTINCT ON (alert_id) ` + caseColumns + `
  FROM alert_cases
 WHERE org_id = $1 AND alert_id = ANY($2)
 ORDER BY alert_id, seq DESC`

// LatestByAlerts is GetLatestByAlert for a whole webhook batch, in one round
// trip. A 200-alert payload must not become 200 round trips (§G.4); it rides
// case_alert_idx (org_id, alert_id, seq DESC).
func (r *CaseRepository) LatestByAlerts(
	ctx context.Context, s db.TenantScope, alertIDs []uuid.UUID,
) (map[uuid.UUID]domain.Case, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	if len(alertIDs) == 0 {
		return map[uuid.UUID]domain.Case{}, nil
	}

	rows, err := r.db(ctx).Query(ctx, latestByAlertsSQL, s.OrgID(), alertIDs)
	if err != nil {
		return nil, mapErr(err, "read latest cases")
	}
	defer rows.Close()

	out := make(map[uuid.UUID]domain.Case, len(alertIDs))
	for rows.Next() {
		var row caseRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "scan case")
		}
		o, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out[o.AlertID()] = o
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read latest cases")
	}
	return out, nil
}

// ListByAlert is the per-alert episode history, newest first.
//
// NOTE (planner): case_alert_idx is (org_id, alert_id, seq DESC), which serves the
// lookup but not the `started_at` ordering the keyset cursor is expressed in. seq
// and started_at are monotonic together, so the sort is over the handful of
// episodes one Alert has; there is no index to add here that is not a migration.
func (r *CaseRepository) ListByAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset,
) ([]domain.Case, db.Cursor, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := db.ClampLimit(p.Limit)

	sql := `SELECT ` + caseColumns + `
	          FROM alert_cases
	         WHERE org_id = $1 AND alert_id = $2
	           AND ($3::timestamptz IS NULL OR (started_at, id) < ($3, $4))
	         ORDER BY started_at DESC, id DESC
	         LIMIT $5`

	var cursorAt *time.Time
	var cursorID uuid.UUID
	if !p.Cursor.IsZero() {
		cursorAt = timePtr(p.Cursor.SortKey)
		cursorID = p.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, sql, s.OrgID(), alertID, cursorAt, cursorID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "list cases")
	}
	defer rows.Close()

	collected, err := collectCases(rows, limit+1)
	if err != nil {
		return nil, db.Cursor{}, err
	}
	page, hasMore := db.PageOf(collected, limit)
	if len(page) == 0 {
		return nil, db.Cursor{Hash: p.Cursor.Hash}, nil
	}
	last := page[len(page)-1]
	return page, db.NextCursor(last.StartedAt(), last.ID(), p.Cursor.Hash, hasMore), nil
}

// listCasesHead is everything before the two spliced dimensions. The keyset, the
// ordering and the `alerts` reach are all in listCasesTail; only the predicates
// whose SHAPE the planner has to see are assembled between them.
//
// ⛔ THE GROUP FACET IS DELETED AND EVERY PLACEHOLDER AFTER IT RENUMBERED (git-bug
// `7570090`, migration `00069`). It read
// `AND ($3::uuid[] IS NULL OR group_id = ANY($3))` over `alert_cases.group_id`,
// which is dropped, and it is the ONE dimension here with no successor: a
// notification's group filter could be re-pointed at `conversation_id` because a
// delivery target still exists, but a CASE has no conversation column to be filtered
// by — the Case IS the conversation. There is nothing to ask.
//
// ⚠️ RENUMBERING WAS FORCED, NOT CHOSEN, and it is the dangerous half of this edit.
// Leaving `$3` bound but unreferenced is a 42P18 at PREPARE — pgx sends twelve
// parameters for a statement that names eleven — so the trailing nine placeholders in
// `listCasesTail` all shift down by one, and `ListCases` drops one argument. A
// mistake here does not fail loudly: it silently binds `since` where `synthetic`
// belongs. This statement is PREPAREd against a migrated container to prove the
// arity, which the Go compiler cannot do for a spliced string.
//
// ⛔ AND THE FILTER IS NOW GONE END TO END, WHICH IS WHY THIS PACKAGE NO LONGER
// GUARDS IT. There was an interim in which `ListCases` short-circuited to an EMPTY
// page whenever `f.GroupIDs` was non-empty: the predicate had gone but `?group_id=`
// was still published, and deleting the predicate alone would have made the
// parameter return the whole org's case list — a SILENT WIDENING of a filtered read,
// which is a far worse failure than an error because nothing reports it. Empty was
// the truthful answer while the parameter existed. It no longer does: `?group_id=`
// is off `listCasesParams`, `ListCasesQuery.GroupID` and `domain.CaseFilter.GroupIDs`
// are deleted, and an old caller is refused `400 unknown_parameter` at the API edge
// before reaching here. The guard came out WITH the field, not before it.
var listCasesHead = `
SELECT ` + caseColumns + `
  FROM alert_cases
 WHERE org_id = $1
   AND ($3::timestamptz IS NULL OR started_at >= $3)`

// The three spellings of the state facet — the ONE liveness axis this endpoint
// has since ADR 0040 collapsed `?open=` into `?state=`.
//
// ⭐⭐ IT IS SPLICED TEXT AND IT IS SPELLED IN `ended_at`, AND BOTH HALVES OF THAT
// ARE MEASURED RATHER THAN ASSUMED. `case_terminal_ended` proves `state='open'`
// and `ended_at IS NULL` select exactly the same rows, so the two are the same
// question to a READER. They are not the same question to the PLANNER: a partial
// index is matched against the query's own restriction clauses and never against
// the table's CHECK constraints, and two of the three indexes in reach here are
// partial on `ended_at IS NULL`. Measured on Postgres 17 over 200k rows, the
// unacked-and-open page:
//
//	state = 'open'                       Index Scan on the FULL (org_id,
//	                                     started_at, id) index, `state` and
//	                                     `ack_state` both heap filters.
//	ended_at IS NULL                     Index Only Scan on the partial ack index,
//	                                     both equalities as Index Cond.
//	state = 'open' AND ended_at IS NULL  the partial index, and a redundant
//	                                     `state` filter that can never fail — plus
//	                                     a selectivity estimate multiplied twice
//	                                     for one restriction (211 rows against
//	                                     5299), which is a worse input to every
//	                                     later join decision for no rows saved.
//
// So the axis is emitted as the one spelling that is BOTH correct and visible,
// and the redundant one is left out. A parameter cannot do this at all: `$n IS
// NULL OR state = ANY($n)` is opaque at plan time, which is exactly why `?open=`
// had to exist as a separate boolean while `state` still held four values.
//
// ⛔ IT CARRIES NO CALLER INPUT. `f.States` is a parsed `[]domain.CaseState` over
// a two-value closed enum, and the branch below chooses between three CONSTANTS.
const (
	stateOpen   = "\n   AND ended_at IS NULL"
	stateClosed = "\n   AND ended_at IS NOT NULL"
)

// The two spellings of the ack facet, both reading the SAME `$3::text[]`.
//
// ⭐⭐ THE ONE-VALUE FORM IS AN EQUALITY AND NOT A ONE-ELEMENT `ANY`, AND THE
// DIFFERENCE IS WHETHER `LIMIT` STOPS THE SCAN. Measured on Postgres 17 against
// `case_ack_idx (org_id, ack_state, started_at DESC) WHERE ended_at IS NULL`:
//
//	ack_state = ANY($2)         Index Scan, ack_state as a FILTER, then a full
//	                            Sort — a ScalarArrayOp can return rows out of
//	                            `started_at` order across the array's values, so
//	                            Postgres cannot call the index presorted and has
//	                            to materialise every open unacked case in the org
//	                            before it can take fifty.
//	ack_state = ($2::text[])[1] Index Scan with BOTH equalities as Index Cond,
//	                            `Presorted Key: started_at`, and an Incremental
//	                            Sort that only breaks `started_at` ties on `id`.
//
// `?ack=unacked` is the whole point of this endpoint, so the hot shape is the one
// that gets the equality. The subscript is a stable expression over a bound
// parameter, which is why it plans as an index condition.
//
// ⛔ BOTH ARMS NAME `$2`, AND NEITHER MAY STOP DOING SO. Postgres derives a
// statement's parameter count from the highest `$n` its text mentions, so an arm
// that dropped the reference would leave `$2` typeless and the Parse would fail
// with "could not determine data type of parameter $2" — which is why "no ack
// filter" is spelled as the null-guard below rather than as an empty string. It
// is also why ADR 0040 RENUMBERED this statement when the state parameter went:
// leaving a `$2` nothing mentions behind would have failed the Parse for exactly
// the same reason, with the far less obvious symptom of an endpoint that never
// answers at all.
const (
	ackAnyOf   = "\n   AND ($2::text[] IS NULL OR ack_state = ANY($2))"
	ackEqualTo = "\n   AND ack_state = ($2::text[])[1]"
)

// listCasesTail reaches `alerts` for the four IDENTITY facets.
//
// ⛔ IT IS `EXISTS`, NOT A JOIN, and the reason is the same one `applyAlertFilter`
// gives for the snooze anti-join one table over: `alerts` and `alert_cases` share
// `id`, `org_id`, `state`, `created_at` and `updated_at`, so a real JOIN would
// make `caseColumns` and half the predicates above ambiguous and every one of
// them would need qualifying. Postgres flattens a correlated EXISTS into a
// semi-join anyway, and it cannot duplicate a row even if it did not, because
// `alert_id` is a FK onto a primary key.
//
// ⭐ THE `synthetic` PREDICATE IS UNCONDITIONAL, which is what makes the EXISTS
// unconditional. A synthetic alert is one oto manufactured for a delivery drill;
// its episodes are not part of the customer's history, so the default list must
// exclude them, and that means every row on this page has had its alert proven to
// exist inside the caller's org rather than merely referenced by a denormalised
// `alert_cases.org_id`.
var listCasesTail = `
   AND EXISTS (SELECT 1
                 FROM alerts a
                WHERE a.id = alert_cases.alert_id
                  AND a.org_id = alert_cases.org_id
                  AND a.synthetic = $4
                  AND ($5::text[] IS NULL OR a.severity    = ANY($5))
                  AND ($6::text[] IS NULL OR a.namespace   = ANY($6))
                  AND ($7::text[] IS NULL OR a.cluster_key = ANY($7))
                  AND ($8::text[] IS NULL OR a.alertname   = ANY($8)))
   AND ($9::timestamptz IS NULL OR (started_at, id) < ($9, $10))
 ORDER BY started_at DESC, id DESC
 LIMIT $11`

// ListCases is `GET /api/v1/cases` (§E.3b): the ORG-WIDE episode list, newest
// first, keyset-paginated over `(started_at DESC, id DESC)`.
//
// ⭐ ONE LIVENESS AXIS, SPELLED THE WAY THE PLANNER CAN READ IT. `?state=` and
// `?open=` were the same axis asked twice once ADR 0040 narrowed the column to
// two values, so `open` is gone and `state` inherited its spliced spelling — see
// the constants above for the measurement behind that.
//
// NOTE (planner). Three indexes are in reach, and 00053 widened two of them by
// `id` so the first two carry the whole keyset sort key:
//
//   - `?state=open&ack=unacked` — the queue this endpoint exists for — rides
//     case_ack_idx `(org_id, ack_state, started_at DESC, id DESC) WHERE ended_at
//     IS NULL`. It carries both equalities, the partial predicate and the whole
//     sort key, so LIMIT stops the scan and no Sort node appears.
//   - ⛔ `?state=open&group_id=…` RODE case_group_live_idx `(org_id, group_id,
//     started_at DESC, id DESC) WHERE ended_at IS NULL`, AND BOTH THE QUERY AND THE
//     INDEX ARE GONE (git-bug `7570090`, migration `00069` drops
//     `case_group_live_idx` and `case_group_idx` by name along with the column they
//     led with). Nothing replaces the access path because nothing replaces the
//     question.
//   - Everything else falls to case_started_idx `(org_id, started_at, id)`, read
//     backwards for the DESC order — which, with the group facet gone, is now the
//     path for every page that is not the unacked queue.
//
// ⚠️ AND `?state=closed` REACHES NONE OF THE PARTIAL ONES, WHICH IS CORRECT
// RATHER THAN A GAP: `ended_at IS NOT NULL` is the complement of both partial
// predicates, so no partial index on live rows could serve it and the full
// case_started_idx — which carries the whole sort key — is the right access path
// for a page of ended episodes.
func (r *CaseRepository) ListCases(
	ctx context.Context, s db.TenantScope, f domain.CaseFilter, p db.Keyset,
) ([]domain.Case, db.Cursor, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := db.ClampLimit(p.Limit)

	// Both values is no constraint at all, and so is neither: the column has
	// exactly two, so a filter naming both selects every row and spelling it out
	// would cost a predicate to save nothing.
	state := ""
	if len(f.States) == 1 {
		if f.States[0].IsOpen() {
			state = stateOpen
		} else {
			state = stateClosed
		}
	}

	acks := make([]string, 0, len(f.AckStates))
	for _, a := range f.AckStates {
		acks = append(acks, a.String())
	}
	ack := ackAnyOf
	if len(acks) == 1 {
		ack = ackEqualTo
	}

	// nil means EXCLUDE, exactly as it does on the alert list.
	synthetic := false
	if f.Synthetic != nil {
		synthetic = *f.Synthetic
	}

	var cursorAt *time.Time
	var cursorID uuid.UUID
	if !p.Cursor.IsZero() {
		cursorAt = timePtr(p.Cursor.SortKey.UTC())
		cursorID = p.Cursor.ID
	}
	var since *time.Time
	if f.Since != nil {
		since = timePtr(f.Since.UTC())
	}

	rows, err := r.db(ctx).Query(ctx, listCasesHead+ack+state+listCasesTail,
		s.OrgID(), nilIfNoRows(acks), since,
		synthetic, nilIfNoRows(f.Severities), nilIfNoRows(f.Namespaces),
		nilIfNoRows(f.ClusterKeys), nilIfNoRows(f.AlertNames),
		cursorAt, cursorID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "list cases")
	}
	defer rows.Close()

	collected, err := collectCases(rows, limit+1)
	if err != nil {
		return nil, db.Cursor{}, err
	}
	page, hasMore := db.PageOf(collected, limit)
	if len(page) == 0 {
		return nil, db.Cursor{Hash: f.FilterHash}, nil
	}
	last := page[len(page)-1]
	return page, db.NextCursor(last.StartedAt(), last.ID(), f.FilterHash, hasMore), nil
}

// nilIfNoRows makes "no constraint" a SQL NULL rather than an empty array, which
// is what lets one static statement carry every optional dimension: `= ANY('{}')`
// matches nothing, while `$n IS NULL OR …` short-circuits the predicate away.
func nilIfNoRows(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return in
}

func collectCases(rows pgx.Rows, capacity int) ([]domain.Case, error) {
	out := make([]domain.Case, 0, capacity)
	for rows.Next() {
		var row caseRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "scan case")
		}
		o, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read cases")
	}
	return out, nil
}

// ------------------------------------------------------------------- writes

// ⭐⭐ `source_ends_at` IS MONOTONIC PER EPISODE. `GREATEST`, never plain
// assignment.
//
// This column is the ENTIRE input to the §B.4 reaper grace check, and T2 is the
// only edge that writes it without a state change to guard it. Alertmanager
// delivers at-least-once from an HA pair with no ordering guarantee between
// replicas, so a payload carrying an OLDER `endsAt` arriving after a newer one is
// ordinary traffic, not an exotic failure. Assigning it verbatim let that stale
// payload REWIND `source_ends_at` into the past, which makes the reaper's grace
// check pass on the very next tick and expire an alert that is firing — the same
// fabricated resolution the transition compare-and-set exists to prevent,
// arriving by a different road.
//
// Monotonicity is the right shape rather than a lock: `endsAt` on a FIRING
// observation is "this alert is valid until at least here", so two deliveries can
// be folded in either order and agree. A genuine end time that is EARLIER —
// upstream saying the alert actually stopped — arrives as a `resolved`
// observation and travels through Transition, which writes it verbatim, because
// there upstream is making a definite statement rather than extending a lease.
//
// `state_version` is bumped even though no state moves: see
// domain.TransitionPrecondition. A version blind to T2 would leave the reaper's
// compare-and-set blind to exactly the webhook that disproves the expiry.
const observeSQL = `
UPDATE alert_cases SET
    last_observed_at  = GREATEST(last_observed_at, $3),
    source_ends_at    = GREATEST(source_ends_at, $4),
    source_updated_at = GREATEST(source_updated_at, $5),
    value             = COALESCE($6, value),
    observed_skew_ms  = $7,
    -- ⭐⭐ A REPEAT FIRING OBSERVATION CANCELS A PENDING CLOSE (migration 00057).
    -- T2 is the edge a re-fire inside the retention window runs, and it persists
    -- HERE rather than through transitionSQL, so the clearing the domain performs
    -- on the Case has to be spelled again in this statement or it would never reach
    -- the row. A receipt left behind on a re-fired episode is a close waiting to
    -- happen against an alert that is demonstrably on fire.
    --
    -- Unconditional and safe: on a deployment with no configured window both
    -- columns are already NULL for every row, so this writes NULL over NULL.
    resolve_pending_at     = NULL,
    resolve_pending_end_at = NULL,
    state_version     = state_version + 1,
    updated_at        = now()
WHERE org_id = $1 AND id = $2`

// Observe folds a repeat observation (T2) into the open case.
//
// A field the observation did not supply is PRESERVED, never cleared: §L.3.1
// says a zero `endsAt` means "no end time known for this payload", not "forget
// the end time you already had". Clearing it would silently disable the reaper
// for that case, because case_reap_idx only sees rows with a non-null
// source_ends_at. `GREATEST` preserves it for free — in Postgres GREATEST ignores
// NULL arguments and returns NULL only when every argument is NULL, so it is
// COALESCE's behaviour plus the monotonicity above.
func (r *CaseRepository) Observe(
	ctx context.Context, s db.TenantScope, id uuid.UUID, o domain.Observation,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if err := db.RequireID("case id", id); err != nil {
		return err
	}
	if o.ObservedAt.IsZero() {
		return errs.Internal("observation_time_missing", errsMissing("observed_at is required"))
	}

	tag, err := r.db(ctx).Exec(ctx, observeSQL, s.OrgID(), id, o.ObservedAt.UTC(),
		timePtr(o.SourceEndsAt), timePtr(o.SourceUpdatedAt), o.Value, o.SkewMS)
	if err != nil {
		return mapErr(err, "record observation")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("case_not_found", "no such case")
	}
	return nil
}

// ⭐⭐ THE COMPARE-AND-SET, and `state_version` IS THE WHOLE OF IT.
//
// `db.Tx` runs at READ COMMITTED, so an UPDATE keyed on `id` alone behaves like
// this under contention: the loser blocks on the row lock, the winner commits,
// the loser wakes, re-evaluates a predicate that mentions nothing but the primary
// key, and writes a verdict reached against a row that no longer exists. That is
// not a hypothetical — it stamps `expired`/`timeout` over a case a webhook
// has just proved is firing, and it puts `suppressed` with a NULL `ended_at` back
// over an episode ingest resolved.
//
// This predicate was briefly a four-column pre-image, for want of a version
// column. Migration 00023 added one, and one column is the stronger guard: a
// multi-column pre-image can be partially specified and still read as guarded.
// `state_version` is bumped by EVERY write that moves a decision input, Observe
// included, so there is exactly one question to ask and exactly one way to ask it.
const transitionSQL = `
UPDATE alert_cases SET
    state              = $3,
    suppression_reason = $4,
    suppressed_by      = COALESCE($5, suppressed_by),
    resolve_reason     = $6,
    ended_at           = $7,
    last_observed_at   = GREATEST(last_observed_at, $8),
    source_ends_at     = COALESCE($9, source_ends_at),
    source_updated_at  = COALESCE($10, source_updated_at),
    suppress_count     = COALESCE($11, suppress_count),
    value              = COALESCE($12, value),
    -- ⛔ WRITTEN VERBATIM, NOT COALESCEd, AND THAT IS DELIBERATE (migration 00057).
    -- These two are the DELAYED CLOSE's receipt, and every edge but the deferral
    -- itself passes NULL because it either never had one or has just spent it. A
    -- COALESCE here would leave a stale receipt on a re-fired episode and the sweep
    -- would close a case that is on fire.
    resolve_pending_at     = $13,
    resolve_pending_end_at = $14,
    state_version      = state_version + 1,
    updated_at         = now()
WHERE org_id = $1 AND id = $2 AND state_version = $15`

const caseExistsSQL = `SELECT state_version FROM alert_cases WHERE org_id = $1 AND id = $2`

// Transition persists one §B.3 edge, exactly as the domain machine produced it,
// as a COMPARE-AND-SET against the `state_version` the machine read.
//
// `ended_at` is written verbatim: it has ALREADY been clamped to >= started_at by
// §B.3.2 and re-deriving it here would give two answers to one question.
//
// ⛔ A NIL `EndedAt` CLEARS THE COLUMN, AND SINCE ADR 0040 NOTHING MAY USE THAT.
// It is what made T8 work — the reopen edge that put a closed episode back into
// `case_one_open_idx` — and T8 is retired. Every surviving edge that reaches this
// statement either leaves `ended_at` NULL (T2, T3, T4, on an already-open episode)
// or sets it (T5, T6). `case_terminal_ended` is what refuses the combination that
// would resurrect a closed one.
//
// ⛔ A Transition with no `Expected.StateVersion` is REFUSED. The precondition
// travels on the Transition rather than as an argument precisely so that it
// cannot be omitted: an unguarded state write is the defect this method exists to
// make unrepresentable, and a new call site must not be able to reintroduce it by
// forgetting a parameter. `state_version` is `NOT NULL DEFAULT 1` with
// `case_sver_ck (>= 1)`, so zero is unambiguously "never bound to a row" and never
// a legal value somebody meant.
//
// A write whose version no longer holds returns errs.KindConflict — NEVER
// success, and never a silent no-op. The caller decides what that means: ingest
// and the reconciler re-read and re-decide, the reaper abandons the transition as
// superseded.
func (r *CaseRepository) Transition(
	ctx context.Context, s db.TenantScope, id uuid.UUID, t domain.Transition,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if err := db.RequireID("case id", id); err != nil {
		return err
	}
	if t.ToState.IsZero() {
		return errs.Internal("transition_state_invalid", errsMissing("to_state is required"))
	}
	if t.LastObservedAt.IsZero() {
		return errs.Internal("transition_time_missing", errsMissing("last_observed_at is required"))
	}
	if t.Expected.StateVersion < 1 {
		return errs.Internal("transition_precondition_missing",
			errsMissing("a transition must carry the state_version it was computed from"))
	}

	var suppressedBy []byte
	if t.SuppressedBy != nil {
		b, err := t.SuppressedBy.MarshalJSON()
		if err != nil {
			return errs.Internal("suppressed_by_encode_failed", err)
		}
		suppressedBy = b
	}

	tag, err := r.db(ctx).Exec(ctx, transitionSQL, s.OrgID(), id,
		t.ToState.String(), t.SuppressionReason, suppressedBy, t.ResolveReason,
		t.EndedAt, t.LastObservedAt.UTC(), t.SourceEndsAt, t.SourceUpdatedAt,
		t.SuppressCount, t.Value,
		t.ResolvePendingAt, t.ResolvePendingEndAt, t.Expected.StateVersion)
	if err != nil {
		return mapErr(err, "apply transition")
	}
	if tag.RowsAffected() == 0 {
		return r.transitionMiss(ctx, s, id, t.Expected.StateVersion)
	}
	return nil
}

// transitionMiss tells a vanished case apart from a superseded pre-image.
//
// The two need different answers: "no such case" is a caller error, while
// "somebody else moved this row while you were deciding" is a concurrency
// conflict the caller is expected to handle by re-reading or by standing down. A
// single NotFound for both would send the reaper down the wrong branch on exactly
// the race this compare-and-set exists to catch.
func (r *CaseRepository) transitionMiss(
	ctx context.Context, s db.TenantScope, id uuid.UUID, want int,
) error {
	var live int32
	err := r.db(ctx).QueryRow(ctx, caseExistsSQL, s.OrgID(), id).Scan(&live)
	if err != nil {
		if isNoRows(err) {
			return errs.NotFound("case_not_found", "no such case")
		}
		return mapErr(err, "apply transition")
	}
	return errs.Conflict("case_superseded",
		"this case moved to state_version "+strconv.Itoa(int(live))+
			" while a transition against "+strconv.Itoa(want)+" was being computed")
}

// ⭐ `state_version` IS ASSERTED HERE BUT NOT BUMPED, and the asymmetry is the
// §B.1 orthogonality rule made mechanical.
//
// ASSERTED, because the domain refuses to acknowledge a terminal case
// (Case.Acknowledge) — and it can only refuse against the snapshot it read.
// A T5 or T6 committing between that read and this write used to land the ack on
// an episode that had already ended, and the ack path's companion
// `alerts.SetProjection` then wrote the PRE-resolution `state` and
// `current_case_id` back onto the alert row. The list showed `firing` for
// an alert Alertmanager had resolved, pointing at the wrong episode.
//
// NOT BUMPED, because an acknowledgement is orthogonal to state (§B.1). If an ack
// bumped the version it would make a concurrent, entirely legitimate §B.3
// transition lose its compare-and-set — a human clicking "ack" must never be able
// to cancel a resolution.
const setAckSQL = `
UPDATE alert_cases SET
    ack_state      = $3,
    acked_by       = $4,
    acked_by_label = $5,
    acked_at       = $6,
    ack_note       = $7,
    updated_at     = now()
WHERE org_id = $1 AND id = $2 AND state_version = $8`

// SetAck writes T9 or T10. Ack fields are ALL-OR-NOTHING (case_ack_ck): writing
// three of the four is writing a row the database will refuse, so an unack
// clears every one of them together.
//
// `expectVersion` is the `state_version` the caller's case was read at. A
// lost assertion is errs.KindConflict, which the API renders as 409: the human is
// told the episode moved rather than being shown a green tick over a resolved
// alert.
//
// ⚠️ ACCEPTED, NOT FIXED: two humans acknowledging the same episode inside one
// round trip both pass this assertion, and the second one's `acked_by_label` and
// `ack_note` overwrite the first's. Both acks are on the timeline, which is the
// truth; the projection simply names one of them. Serialising it would cost a
// lock on the hot ack path to arbitrate between two people who agree.
func (r *CaseRepository) SetAck(
	ctx context.Context, s db.TenantScope, id uuid.UUID, a domain.AckChange, expectVersion int,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if err := db.RequireID("case id", id); err != nil {
		return err
	}
	if expectVersion < 1 {
		return errs.Internal("ack_precondition_missing",
			errsMissing("an acknowledgement must carry the state_version it was read at"))
	}

	var (
		by      *uuid.UUID
		byLabel *string
		at      *time.Time
		note    *string
	)
	if a.To.IsAcked() {
		if a.At.IsZero() {
			return errs.Internal("ack_time_missing", errsMissing("acked_at is required"))
		}
		if a.ByLabel == nil || strings.TrimSpace(*a.ByLabel) == "" {
			return errs.Internal("ack_label_missing", errsMissing("acked_by_label is required"))
		}
		by, byLabel, at, note = a.By, a.ByLabel, timePtr(a.At), a.Note
	}

	tag, err := r.db(ctx).Exec(ctx, setAckSQL, s.OrgID(), id,
		ackStateOrUnacked(a.To).String(), by, byLabel, at, note, expectVersion)
	if err != nil {
		return mapErr(err, "write acknowledgement")
	}
	if tag.RowsAffected() == 0 {
		return r.transitionMiss(ctx, s, id, expectVersion)
	}
	return nil
}

func ackStateOrUnacked(a domain.AckState) domain.AckState {
	if a.IsZero() {
		return domain.AckStateUnacked
	}
	return a
}

// BindRuleSnapshot binds the RuleSnapshot captured at fire time — what the rule
// SAID at that moment (R6).
func (r *CaseRepository) BindRuleSnapshot(
	ctx context.Context, s db.TenantScope, id, snapshotID uuid.UUID,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if err := db.RequireID("case id", id); err != nil {
		return err
	}
	if err := db.RequireID("rule_snapshot_id", snapshotID); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx,
		`UPDATE alert_cases SET rule_snapshot_id = $3, updated_at = now()
		  WHERE org_id = $1 AND id = $2`,
		s.OrgID(), id, snapshotID)
	if err != nil {
		return mapErr(err, "bind rule snapshot")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("case_not_found", "no such case")
	}
	return nil
}

// ------------------------------------------------------------------- reaper

var reapCandidatesSQL = `
SELECT ` + caseColumns + `
  FROM alert_cases
 WHERE org_id = $1
   AND ended_at IS NULL
   AND source_ends_at IS NOT NULL
   AND source_ends_at < $2
   -- ⛔ AN EPISODE HOLDING AN UPSTREAM RESOLVE IS NOT A REAP CANDIDATE (00057).
   -- A pending close leaves ended_at NULL, so without this line every case
   -- inside its retention window would be handed to T6 on the next tick and
   -- expired as timeout -- oto claiming it stopped hearing about an alert whose
   -- resolution it was holding, which is the one fabrication 00007 forbids. The
   -- domain refuses it and unreapable refuses it; this stops the sweep spending
   -- its whole budget on candidates that will all be turned down. It also keeps
   -- case_reap_idx usable: the predicate is a filter on the same index scan.
   AND resolve_pending_at IS NULL
 ORDER BY source_ends_at ASC
 LIMIT $3`

// ReapCandidates feeds T6: open episodes whose upstream end time plus
// resolve_grace has passed. `before` is already `now - resolve_grace`, computed
// by the caller against the injected clock.
//
// ⭐ THE REAPER GUARD (§B.4) IS THE CALLER'S. This method returns CANDIDATES, not
// verdicts. A case whose AlertSource is not healthy MUST be held, never
// expired: losing sight of an alert is not the same as the alert resolving.
//
// NOTE (planner): case_reap_idx is (source_ends_at) WHERE ended_at IS NULL AND
// source_ends_at IS NOT NULL and deliberately does NOT lead with org_id — the
// reaper is a global background sweep. The `org_id = $1` predicate this port's
// TenantScope requires is therefore a filter applied after the index scan, which
// is correct but not free on a multi-tenant deployment.
func (r *CaseRepository) ReapCandidates(
	ctx context.Context, s db.TenantScope, before time.Time, limit int,
) ([]domain.Case, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	if before.IsZero() {
		return nil, errs.Internal("reap_bound_missing", errsMissing("before is required"))
	}
	n := db.ClampLimit(limit)

	rows, err := r.db(ctx).Query(ctx, reapCandidatesSQL, s.OrgID(), before.UTC(), n)
	if err != nil {
		return nil, mapErr(err, "list reap candidates")
	}
	defer rows.Close()
	return collectCases(rows, n)
}

// ---------------------------------------------------- the delayed close (00057)

var closeDueCandidatesSQL = `
SELECT ` + caseColumns + `
  FROM alert_cases
 WHERE org_id = $1
   AND resolve_pending_at IS NOT NULL
   AND resolve_pending_at <= $2
 ORDER BY resolve_pending_at ASC
 LIMIT $3`

// CloseDueCandidates feeds the DELAYED CLOSE: open episodes whose upstream resolve
// has been held for the whole case retention window W and may now be closed
// (migration 00057).
//
// ⛔⛔ IT SELECTS ONLY ROWS THAT ALREADY CARRY A RECEIPT, AND THAT IS WHAT KEEPS
// "A RESOLUTION IS NEVER FABRICATED" TRUE OF A BACKGROUND SWEEP. Only an explicit
// upstream `status="resolved"` writes `resolve_pending_at`, so this statement
// cannot reach an episode nobody has resolved: it finds resolves that have been
// WAITING, never invents one. `ended_at IS NULL` is deliberately not repeated —
// `case_pending_open_ck` makes it redundant, and a redundant predicate in a scan
// is one more thing that can silently disagree with the CHECK.
//
// ⭐ IT IS EMPTY ON EVERY DEPLOYMENT THAT HAS SET NO W, and `case_close_due_idx` is
// empty with it, because 0 is the default window and a case with W=0 closes on the
// resolve without ever writing these columns.
//
// Like ReapCandidates this returns CANDIDATES, not verdicts: the caller re-reads
// each row inside its own transaction and writes as a compare-and-set. Unlike
// ReapCandidates it needs NO source-health guard — §B.4 protects oto from inferring
// an ending out of silence, and there is no inference here; the ending was stated
// upstream and is on the row.
func (r *CaseRepository) CloseDueCandidates(
	ctx context.Context, s db.TenantScope, asOf time.Time, limit int,
) ([]domain.Case, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		return nil, errs.Internal("close_due_bound_missing", errsMissing("as_of is required"))
	}
	n := db.ClampLimit(limit)

	rows, err := r.db(ctx).Query(ctx, closeDueCandidatesSQL, s.OrgID(), asOf.UTC(), n)
	if err != nil {
		return nil, mapErr(err, "list due closes")
	}
	defer rows.Close()
	return collectCases(rows, n)
}

// caseSourcesSQL walks case → alert → cluster → source, because the direct edge is
// gone.
//
// ⛔⛔ IT READ `JOIN alert_groups g ON g.id = o.group_id` FOR `g.source_id`, AND
// **BOTH** THE TABLE AND THE COLUMN ARE DROPPED (git-bug `7570090`, migration
// `00069`). `alert_groups.source_id` was the ONLY direct edge from a Case to an
// `alert_sources` row — `internal/notification/repository/snapshot_ack_test.go` says
// so in as many words while turning the Silence deep link off for the same reason.
//
// ⛔ DELETING THIS READ WOULD HAVE STOPPED THE REAPER, WHICH IS WHY IT IS
// RE-ATTRIBUTED INSTEAD. A case absent from the result is read by
// `alerts/service.resolveSources` as "cannot prove the source is healthy", and the
// §B.4 guard then HOLDS it. An always-empty map is therefore not a degraded answer:
// it is a permanent stall in which nothing is ever expired, announced only by an
// INFO log nobody reads. That is the silently-zeroed-metric failure in its most
// expensive form.
//
// ⭐ THE SURVIVING PATH IS THE CLUSTER, AND IT IS DELIBERATELY REFUSED WHEN IT IS
// AMBIGUOUS. `alerts.cluster_id` and `alert_sources.cluster_id` both name
// `clusters.id`, but `alert_sources_cluster_idx (org_id, cluster_id) WHERE deleted_at
// IS NULL` is NOT unique — a cluster may be fed by several sources — so "the source
// of this case" only has an answer when the cluster has exactly ONE live source. The
// `count(*) OVER ()` window is that test, and a cluster with two live sources yields
// NO row for its cases.
//
// ⚠️ WHICH IS THE SAFE DIRECTION AND THE ONLY DEFENSIBLE ONE. Picking any of several
// sources would let the reaper expire an episode on the health of a source that never
// carried it, which is precisely the mistake §B.4 exists to prevent; refusing leaves
// those cases held, exactly as they are held today when the port is unwired. A
// single-source cluster — the ordinary install — keeps the guard it had.
//
// ⚠️ AND THIS IS A JUDGEMENT, NOT A RESTORATION. The old answer was the source that
// actually delivered the alert; this one is the source that must have, given the
// cluster. They agree whenever the cluster has one source and the old answer is
// simply unavailable otherwise. If `alert_cases` ever carries a source of its own
// — which is what the snapshot note is waiting for — this should become that column.
const caseSourcesSQL = `
SELECT o.id, s.source_id
  FROM alert_cases o
  JOIN alerts al ON al.id = o.alert_id AND al.org_id = o.org_id
  JOIN (SELECT id AS source_id, cluster_id, org_id,
               count(*) OVER (PARTITION BY org_id, cluster_id) AS live_in_cluster
          FROM alert_sources
         WHERE deleted_at IS NULL) s
    ON s.cluster_id = al.cluster_id AND s.org_id = al.org_id
   AND s.live_in_cluster = 1
 WHERE o.org_id = $1 AND o.id = ANY($2)`

// SourceIDs resolves which AlertSource each case came from, by way of the cluster
// its Alert belongs to.
//
// It exists for the §B.4 reaper guard, which must load `source_health` for the
// owning source before it may expire anything. A case with no resolvable
// source is ABSENT from the result, and the caller must read that as "cannot
// prove the source is healthy" and HOLD it. Since git-bug `7570090` that absence
// covers one more shape than it used to: a case whose cluster has no live source, or
// more than one. See `caseSourcesSQL` for why refusing beats guessing.
func (r *CaseRepository) SourceIDs(
	ctx context.Context, s db.TenantScope, caseIDs []uuid.UUID,
) (map[uuid.UUID]uuid.UUID, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	if len(caseIDs) == 0 {
		return map[uuid.UUID]uuid.UUID{}, nil
	}

	rows, err := r.db(ctx).Query(ctx, caseSourcesSQL, s.OrgID(), caseIDs)
	if err != nil {
		return nil, mapErr(err, "resolve case sources")
	}
	defer rows.Close()

	out := make(map[uuid.UUID]uuid.UUID, len(caseIDs))
	for rows.Next() {
		var caseID, srcID uuid.UUID
		if err := rows.Scan(&caseID, &srcID); err != nil {
			return nil, mapErr(err, "scan case source")
		}
		out[caseID] = srcID
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read case sources")
	}
	return out, nil
}
