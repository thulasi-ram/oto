package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// SuggestionRepository is every statement against `investigation_suggestions` (migration
// 00101, git-bug 8327c00).
//
// ⛔ A PROPOSAL IS ONLY EVER INSERTED, AND APPLIED ONCE. The one UPDATE in this file sets
// the three `applied_*` columns of a row that has none — and if any other were added,
// `investigation_suggestions_once` would refuse it at the row. There is no statement that
// declines, hides or deletes a Suggestion: an unapplied one lapses on the clock, and the
// rows go with their Investigation.
//
// ⛔ EVERY STATEMENT IS TENANT-SCOPED, AND `org_id = $1` IS IN EVERY PREDICATE.
type SuggestionRepository struct {
	q db.Querier
}

// NewSuggestionRepository builds the repository over a fallback querier; a transaction
// travelling in the context wins over it.
func NewSuggestionRepository(q db.Querier) *SuggestionRepository {
	return &SuggestionRepository{q: q}
}

func (r *SuggestionRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

func mapSuggestionErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "suggestion_not_found",
		NotFoundMessage:    "no such Suggestion",
		QueryFailed:        "investigator_query_failed",
		QueryFailedMessage: "could not " + what,
	})
}

const insertSuggestionSQL = `
INSERT INTO investigation_suggestions
  (id, org_id, investigation_id, kind, policy_id, policy_name, count_min, count_window_s,
   was_count_min, was_count_window_s, incident_id, incident_number, case_id, case_number,
   why, proposed_at, lapses_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`

// InsertSuggestions writes the Suggestions one run's Finding made, in the caller's
// transaction — the one that records the Finding — so a Suggestion never exists without
// the Finding that made it.
func (r *SuggestionRepository) InsertSuggestions(
	ctx context.Context, s db.TenantScope, investigationID uuid.UUID, drafts []domain.SuggestionDraft,
	at, lapsesAt time.Time,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	for i, d := range drafts {
		var (
			policyID, incidentID, caseID       *uuid.UUID
			policyName                         *string
			countMin, countWin, wasMin, wasWin *int
			incidentNumber, caseNumber         *int64
		)
		switch d.Kind {
		case domain.SuggestCountCondition:
			c := d.Count
			policyID, policyName = nullableID(c.PolicyID), nullable(c.PolicyName)
			countMin, countWin = intPtr(c.CountMin), intPtr(int(c.CountWindow/time.Second))
			if c.WasMin > 0 {
				wasMin, wasWin = intPtr(c.WasMin), intPtr(int(c.WasWindow/time.Second))
			}
		case domain.SuggestMembership:
			m := d.Membership
			incidentID, caseID = nullableID(m.IncidentID), nullableID(m.CaseID)
			incidentNumber, caseNumber = &m.IncidentNumber, &m.CaseNumber
		default:
			return errs.Newf(errs.KindInternal, "suggestion_kind", "unknown Suggestion kind %q", d.Kind)
		}
		// ⭐ PROPOSED IN ORDER: a microsecond apart, so the run's order survives the
		// `(proposed_at, id)` sort the read uses, whatever the ids are.
		proposed := at.UTC().Add(time.Duration(i) * time.Microsecond)
		if _, err := r.db(ctx).Exec(ctx, insertSuggestionSQL,
			id.New(), s.OrgID(), investigationID, string(d.Kind), policyID, policyName, countMin, countWin,
			wasMin, wasWin, incidentID, incidentNumber, caseID, caseNumber,
			d.Why, proposed, lapsesAt.UTC()); err != nil {
			return mapSuggestionErr(err, "record a Suggestion")
		}
	}
	return nil
}

// listSuggestionsSQL is one Investigation's Suggestions that are still SHOWN at $3: every
// applied one (its provenance stays readable) and every one whose lapse time has not come.
// ⭐ A LAPSED ONE IS NOT READ AT ALL — that is what "stops showing" means, and the clock is
// the application's, passed in, never the database's.
const listSuggestionsSQL = `
SELECT s.id, s.org_id, s.investigation_id, s.kind, s.policy_id, s.policy_name, s.count_min, s.count_window_s,
       s.was_count_min, s.was_count_window_s, s.incident_id, s.incident_number, s.case_id, s.case_number,
       s.why, s.proposed_at, s.lapses_at, s.applied_at, s.applied_by, s.applied_by_label
  FROM investigation_suggestions s
 WHERE s.org_id = $1 AND s.investigation_id = $2
   AND (s.applied_at IS NOT NULL OR s.lapses_at > $3)
 ORDER BY s.proposed_at, s.id`

// ListSuggestions reads one Investigation's shown Suggestions — applied, or not yet lapsed
// at `now` — in the order they were proposed, served by `investigation_suggestions_run_idx`.
func (r *SuggestionRepository) ListSuggestions(
	ctx context.Context, s db.TenantScope, investigationID uuid.UUID, now time.Time,
) ([]domain.Suggestion, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, listSuggestionsSQL, s.OrgID(), investigationID, now.UTC())
	if err != nil {
		return nil, mapSuggestionErr(err, "list Suggestions")
	}
	defer rows.Close()
	out := []domain.Suggestion{}
	for rows.Next() {
		sg, err := scanSuggestion(rows)
		if err != nil {
			return nil, mapSuggestionErr(err, "list Suggestions")
		}
		out = append(out, sg)
	}
	if err := rows.Err(); err != nil {
		return nil, mapSuggestionErr(err, "list Suggestions")
	}
	return out, nil
}

const lockSuggestionSQL = `
SELECT s.id, s.org_id, s.investigation_id, s.kind, s.policy_id, s.policy_name, s.count_min, s.count_window_s,
       s.was_count_min, s.was_count_window_s, s.incident_id, s.incident_number, s.case_id, s.case_number,
       s.why, s.proposed_at, s.lapses_at, s.applied_at, s.applied_by, s.applied_by_label
  FROM investigation_suggestions s
 WHERE s.org_id = $1 AND s.id = $2
   FOR UPDATE`

// LockSuggestion reads one Suggestion FOR UPDATE, inside the caller's transaction, so two
// humans applying it at once queue: the second reads it applied and is refused. A lapsed one
// is read too — the apply must be able to say it lapsed, rather than that it never existed.
func (r *SuggestionRepository) LockSuggestion(ctx context.Context, s db.TenantScope, sid uuid.UUID) (domain.Suggestion, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Suggestion{}, err
	}
	if err := db.RequireID("suggestion_id", sid); err != nil {
		return domain.Suggestion{}, err
	}
	out, err := scanSuggestion(r.db(ctx).QueryRow(ctx, lockSuggestionSQL, s.OrgID(), sid))
	if err != nil {
		return domain.Suggestion{}, mapSuggestionErr(err, "read a Suggestion")
	}
	return out, nil
}

// ⭐ APPLIED ONCE, guarded by `applied_at IS NULL`, and GREATEST-guarded for the reason every
// app-clocked table is: N pods have N clocks, and skew must not record an application before
// the proposal it applies (`investigation_suggestions_applied_ck`).
const markAppliedSQL = `
UPDATE investigation_suggestions
   SET applied_at = GREATEST($3, proposed_at), applied_by = $4, applied_by_label = $5
 WHERE org_id = $1 AND id = $2 AND applied_at IS NULL`

// MarkApplied records who applied a Suggestion and when, in the transaction that made the
// edit. A row already applied matches nothing and is refused as such.
func (r *SuggestionRepository) MarkApplied(
	ctx context.Context, s db.TenantScope, sid uuid.UUID, by domain.Requester, at time.Time,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx, markAppliedSQL, s.OrgID(), sid, at.UTC(), nullableID(by.UserID), by.Label)
	if err != nil {
		return mapSuggestionErr(err, "record a Suggestion as applied")
	}
	if tag.RowsAffected() != 1 {
		return errs.Conflict("suggestion_already_applied", "this Suggestion was applied meanwhile")
	}
	return nil
}

func scanSuggestion(row pgx.Row) (domain.Suggestion, error) {
	var (
		out                                domain.Suggestion
		kind                               string
		policyID, incidentID, caseID       *uuid.UUID
		policyName                         *string
		countMin, countWin, wasMin, wasWin *int
		incidentNumber, caseNumber         *int64
		appliedAt                          *time.Time
		appliedBy                          *uuid.UUID
		appliedLabel                       *string
	)
	if err := row.Scan(&out.ID, &out.OrgID, &out.InvestigationID, &kind, &policyID, &policyName,
		&countMin, &countWin, &wasMin, &wasWin, &incidentID, &incidentNumber, &caseID, &caseNumber,
		&out.Why, &out.ProposedAt, &out.LapsesAt, &appliedAt, &appliedBy, &appliedLabel); err != nil {
		return domain.Suggestion{}, err
	}
	k, err := domain.ParseSuggestionKind(kind)
	if err != nil {
		return domain.Suggestion{}, err
	}
	out.Kind = k
	switch k {
	case domain.SuggestCountCondition:
		out.Count = domain.CountChange{
			PolicyID: deref(policyID), PolicyName: derefS(policyName),
			CountMin: derefI(countMin), CountWindow: time.Duration(derefI(countWin)) * time.Second,
			WasMin: derefI(wasMin), WasWindow: time.Duration(derefI(wasWin)) * time.Second,
		}
	case domain.SuggestMembership:
		out.Membership = domain.MembershipChange{
			IncidentID: deref(incidentID), IncidentNumber: derefN(incidentNumber),
			CaseID: deref(caseID), CaseNumber: derefN(caseNumber),
		}
	}
	if appliedAt != nil {
		out.AppliedAt = appliedAt.UTC()
		out.AppliedBy = domain.Requester{UserID: deref(appliedBy), Label: derefS(appliedLabel)}
	}
	out.ProposedAt, out.LapsesAt = out.ProposedAt.UTC(), out.LapsesAt.UTC()
	return out, nil
}

func intPtr(n int) *int { return &n }

func deref(u *uuid.UUID) uuid.UUID {
	if u == nil {
		return uuid.Nil
	}
	return *u
}

func derefS(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefI(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

func derefN(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}
