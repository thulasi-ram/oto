package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// RemedyRepository is every statement against `remedies`, `remedy_approvals` and
// `remedy_transitions` (migration 00104, git-bug 4148256).
//
// ⛔ A PROPOSAL IS INSERTED ONCE AND NEVER REWRITTEN; EVERY LATER CHANGE IS A TRANSITION. The
// one UPDATE in this file moves `state` from the state the caller read to the next one, with
// the instants and the outcome that move sets, and writes the transition row beside it — so
// a Remedy's history is its rows, and `remedies_frozen` refuses anything else at the row.
// Approvals and transitions are only ever inserted (`remedy_record_refuse_change`).
//
// ⛔ EVERY STATEMENT IS TENANT-SCOPED, AND `org_id = $1` IS IN EVERY PREDICATE.
type RemedyRepository struct {
	q db.Querier
}

// NewRemedyRepository builds the repository over a fallback querier; a transaction
// travelling in the context wins over it.
func NewRemedyRepository(q db.Querier) *RemedyRepository {
	return &RemedyRepository{q: q}
}

func (r *RemedyRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

func mapRemedyErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "remedy_not_found",
		NotFoundMessage:    "no such Remedy",
		QueryFailed:        "investigator_query_failed",
		QueryFailedMessage: "could not " + what,
	})
}

const insertRemedySQL = `
INSERT INTO remedies
  (id, org_id, investigation_id, subject_kind, subject_id, proposed_by_label,
   tool_server_id, tool_server_name, tool_name, arguments, arguments_sha256,
   target, description, required_approvals, state, proposed_at, expires_at,
   risk_basis, risk_rule, risk_detail, risk_model_check, risk_model, risk_model_tokens)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 'proposed', $15, $16,
        $17, $18, $19, $20, $21, $22)`

const insertTransitionSQL = `
INSERT INTO remedy_transitions
  (id, org_id, remedy_id, from_state, to_state, actor_kind, actor_id, actor_label, at,
   failure_reason, detail, declared_incident_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

// InsertRemedy writes one proposed Remedy and its proposal transition, in the caller's
// transaction — the one that records the Finding it stands on.
func (r *RemedyRepository) InsertRemedy(ctx context.Context, s db.TenantScope, rem domain.Remedy, proposal domain.RemedyTransition) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if rem.State != domain.RemedyProposed || proposal.From != "" || proposal.To != domain.RemedyProposed {
		return errs.New(errs.KindInternal, "remedy_insert_state", "a Remedy is inserted proposed, with its proposal")
	}
	var toolServerID *uuid.UUID
	var toolServerName, toolName, args, hash *string
	if rem.Tool.Named() {
		toolServerID = nullableID(rem.Tool.ToolServerID)
		toolServerName, toolName = nullable(rem.Tool.ToolServerName), nullable(rem.Tool.Tool)
		args, hash = nullable(rem.Arguments), nullable(rem.ArgumentsSHA256)
	}
	// ⭐ How the tier was set (migration 00107): all six together, or none for a Remedy with
	// no Tool. Tokens are kept only when a model was asked.
	var basis, rule, detail, check, model *string
	var tokens *int64
	if rk := rem.Risk; rem.Tool.Named() && rk.Recorded() {
		basis, rule, detail = nullable(string(rk.Basis)), nullable(rk.Rule), nullable(rk.Detail)
		check, model = nullable(string(rk.Model)), nullable(rk.ModelIdentity)
		if rk.Model == domain.ModelKept || rk.Model == domain.ModelRaised || rk.Model == domain.ModelFailed {
			n := rk.ModelTokens
			tokens = &n
		}
	}
	if _, err := r.db(ctx).Exec(ctx, insertRemedySQL,
		rem.ID, s.OrgID(), rem.InvestigationID, string(rem.SubjectKind), rem.SubjectID, rem.ProposedBy,
		toolServerID, toolServerName, toolName, args, hash,
		rem.Target, rem.Description, rem.RequiredApprovals, rem.ProposedAt.UTC(), rem.ExpiresAt.UTC(),
		basis, rule, detail, check, model, tokens); err != nil {
		return mapRemedyErr(err, "record a Remedy")
	}
	return r.insertTransition(ctx, s, rem.ID, proposal)
}

func (r *RemedyRepository) insertTransition(ctx context.Context, s db.TenantScope, remedyID uuid.UUID, t domain.RemedyTransition) error {
	var actorID *uuid.UUID
	if t.Actor.Kind == domain.ActorUser {
		actorID = nullableID(t.Actor.UserID)
	}
	if _, err := r.db(ctx).Exec(ctx, insertTransitionSQL,
		t.ID, s.OrgID(), remedyID, nullable(string(t.From)), string(t.To), string(t.Actor.Kind), actorID, t.Actor.Label,
		t.At.UTC(), nullable(string(t.Failure)), nullable(t.Detail), nullableID(t.DeclaredIncidentID)); err != nil {
		return mapRemedyErr(err, "record a Remedy's transition")
	}
	return nil
}

const remedyColumns = `
SELECT r.id, r.org_id, r.investigation_id, r.subject_kind, r.subject_id, r.proposed_by_label,
       r.tool_server_id, r.tool_server_name, r.tool_name, r.arguments, r.arguments_sha256,
       r.target, r.description, r.required_approvals, r.state, r.proposed_at, r.expires_at,
       r.approved_at, r.executing_at, r.ended_at, r.failure_reason, r.detail, r.result,
       r.risk_basis, r.risk_rule, r.risk_detail, r.risk_model_check, r.risk_model, r.risk_model_tokens
  FROM remedies r`

const listRemediesSQL = remedyColumns + `
 WHERE r.org_id = $1 AND r.investigation_id = $2
 ORDER BY r.proposed_at, r.id`

// ListRemedies reads one Investigation's Remedies in the order they were proposed, each with
// its approvals and its transitions. Every one is read, whatever its state: a Remedy's record
// is the point of it.
func (r *RemedyRepository) ListRemedies(ctx context.Context, s db.TenantScope, investigationID uuid.UUID) ([]domain.Remedy, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, listRemediesSQL, s.OrgID(), investigationID)
	if err != nil {
		return nil, mapRemedyErr(err, "list Remedies")
	}
	out, err := collectRemedies(rows)
	if err != nil {
		return nil, mapRemedyErr(err, "list Remedies")
	}
	if err := r.attach(ctx, s, out); err != nil {
		return nil, err
	}
	return out, nil
}

const getRemedySQL = remedyColumns + `
 WHERE r.org_id = $1 AND r.id = $2`

// GetRemedy reads one Remedy with its approvals and transitions.
func (r *RemedyRepository) GetRemedy(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Remedy, error) {
	return r.one(ctx, s, id, getRemedySQL)
}

// LockRemedy reads one Remedy FOR UPDATE, inside the caller's transaction, so two humans
// approving at once — or a human and the executor — queue: the second reads what the first
// wrote.
func (r *RemedyRepository) LockRemedy(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Remedy, error) {
	return r.one(ctx, s, id, getRemedySQL+` FOR UPDATE`)
}

func (r *RemedyRepository) one(ctx context.Context, s db.TenantScope, id uuid.UUID, sql string) (domain.Remedy, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Remedy{}, err
	}
	if err := db.RequireID("remedy_id", id); err != nil {
		return domain.Remedy{}, err
	}
	rem, err := scanRemedy(r.db(ctx).QueryRow(ctx, sql, s.OrgID(), id))
	if err != nil {
		return domain.Remedy{}, mapRemedyErr(err, "read a Remedy")
	}
	list := []domain.Remedy{rem}
	if err := r.attach(ctx, s, list); err != nil {
		return domain.Remedy{}, err
	}
	return list[0], nil
}

const approvalsSQL = `
SELECT a.remedy_id, a.user_id, a.user_label, a.arguments_sha256, a.approved_at
  FROM remedy_approvals a
 WHERE a.org_id = $1 AND a.remedy_id = ANY($2)
 ORDER BY a.approved_at, a.id`

const transitionsSQL = `
SELECT t.remedy_id, t.id, t.from_state, t.to_state, t.actor_kind, t.actor_id, t.actor_label, t.at,
       t.failure_reason, t.detail, t.declared_incident_id
  FROM remedy_transitions t
 WHERE t.org_id = $1 AND t.remedy_id = ANY($2)
 ORDER BY t.at, t.id`

// attach reads the approvals and transitions of every Remedy in list, in two statements.
func (r *RemedyRepository) attach(ctx context.Context, s db.TenantScope, list []domain.Remedy) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(list))
	at := make(map[uuid.UUID]int, len(list))
	for i, rem := range list {
		ids[i], at[rem.ID] = rem.ID, i
		list[i].Approvals, list[i].Transitions = []domain.RemedyApproval{}, []domain.RemedyTransition{}
	}

	rows, err := r.db(ctx).Query(ctx, approvalsSQL, s.OrgID(), ids)
	if err != nil {
		return mapRemedyErr(err, "read a Remedy's approvals")
	}
	for rows.Next() {
		var (
			remedyID uuid.UUID
			userID   *uuid.UUID
			a        domain.RemedyApproval
		)
		if err := rows.Scan(&remedyID, &userID, &a.Label, &a.ArgumentsSHA256, &a.ApprovedAt); err != nil {
			rows.Close()
			return mapRemedyErr(err, "read a Remedy's approvals")
		}
		a.UserID, a.ApprovedAt = deref(userID), a.ApprovedAt.UTC()
		i := at[remedyID]
		list[i].Approvals = append(list[i].Approvals, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return mapRemedyErr(err, "read a Remedy's approvals")
	}

	rows, err = r.db(ctx).Query(ctx, transitionsSQL, s.OrgID(), ids)
	if err != nil {
		return mapRemedyErr(err, "read a Remedy's transitions")
	}
	defer rows.Close()
	for rows.Next() {
		var (
			remedyID                  uuid.UUID
			t                         domain.RemedyTransition
			from, failure, detail     *string
			to, actorKind             string
			actorID, declaredIncident *uuid.UUID
		)
		if err := rows.Scan(&remedyID, &t.ID, &from, &to, &actorKind, &actorID, &t.Actor.Label, &t.At,
			&failure, &detail, &declaredIncident); err != nil {
			return mapRemedyErr(err, "read a Remedy's transitions")
		}
		if from != nil {
			if t.From, err = domain.ParseRemedyState(*from); err != nil {
				return err
			}
		}
		if t.To, err = domain.ParseRemedyState(to); err != nil {
			return err
		}
		if t.Failure, err = domain.ParseRemedyFailure(derefS(failure)); err != nil {
			return err
		}
		t.Actor.Kind, t.Actor.UserID = domain.RemedyActorKind(actorKind), deref(actorID)
		t.At, t.Detail, t.DeclaredIncidentID = t.At.UTC(), derefS(detail), deref(declaredIncident)
		i := at[remedyID]
		list[i].Transitions = append(list[i].Transitions, t)
	}
	if err := rows.Err(); err != nil {
		return mapRemedyErr(err, "read a Remedy's transitions")
	}
	return nil
}

// ⭐ ONE PERSON, ONE APPROVAL: `remedy_approvals_user_uniq` is the authority, and a second
// approval by the same user inserts nothing.
const addApprovalSQL = `
INSERT INTO remedy_approvals (id, org_id, remedy_id, user_id, user_label, arguments_sha256, approved_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (remedy_id, user_id) DO NOTHING`

// AddApproval records one human's approval, in the transaction that locked the Remedy. A user
// who has approved it already is refused: they count once.
func (r *RemedyRepository) AddApproval(ctx context.Context, s db.TenantScope, remedyID uuid.UUID, a domain.RemedyApproval) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if a.UserID == uuid.Nil {
		return errs.New(errs.KindInternal, "remedy_approval_user", "an approval names the human who made it")
	}
	tag, err := r.db(ctx).Exec(ctx, addApprovalSQL, uuid.New(), s.OrgID(), remedyID, a.UserID, a.Label,
		a.ArgumentsSHA256, a.ApprovedAt.UTC())
	if err != nil {
		return mapRemedyErr(err, "record a Remedy's approval")
	}
	if tag.RowsAffected() != 1 {
		return domain.RemedyAlreadyApproved(a)
	}
	return nil
}

// ⭐ A MOVE FROM THE STATE THE CALLER READ, AND ONLY FROM IT. Each instant is stamped by the
// move that reaches it, GREATEST-guarded for the reason every app-clocked table is: N pods
// have N clocks, and skew must not put an approval before its proposal.
const transitionSQL = `
UPDATE remedies
   SET state          = $3,
       expires_at     = COALESCE($5, expires_at),
       approved_at    = CASE WHEN $3 = 'approved'  THEN GREATEST($4, proposed_at) ELSE approved_at END,
       executing_at   = CASE WHEN $3 = 'executing' THEN GREATEST($4, approved_at) ELSE executing_at END,
       ended_at       = CASE WHEN $3 IN ('executed','failed','declined','expired')
                             THEN GREATEST($4, proposed_at, approved_at, executing_at) ELSE ended_at END,
       failure_reason = $6,
       detail         = COALESCE($7, detail),
       result         = COALESCE($8, result)
 WHERE org_id = $1 AND id = $2 AND state = $9`

// Transition moves one Remedy from the state its caller read to the next, and records the
// transition beside it, in the caller's transaction. A Remedy that moved meanwhile matches
// nothing and is refused as such: the caller decided on a state that is no longer true.
//
// expiresAt, when set, re-stamps the deadline (an approval starts the window to execution);
// result, when set, is what the write Tool answered.
func (r *RemedyRepository) Transition(
	ctx context.Context, s db.TenantScope, remedyID uuid.UUID, t domain.RemedyTransition, expiresAt time.Time, result string,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	var expires *time.Time
	if !expiresAt.IsZero() {
		e := expiresAt.UTC()
		expires = &e
	}
	tag, err := r.db(ctx).Exec(ctx, transitionSQL, s.OrgID(), remedyID, string(t.To), t.At.UTC(), expires,
		nullable(string(t.Failure)), nullable(t.Detail), nullable(result), string(t.From))
	if err != nil {
		return mapRemedyErr(err, "record a Remedy's transition")
	}
	if tag.RowsAffected() != 1 {
		return errs.Conflict("remedy_moved", "this Remedy moved meanwhile; read it again")
	}
	return r.insertTransition(ctx, s, remedyID, t)
}

// ⭐ BOTH OPEN STATES, BY DEADLINE: a proposed Remedy nobody approved and an approved one the
// executor never claimed both expire, and the sweep records each. Served by
// `remedies_open_idx`. The clock is the application's, passed in.
const pastDeadlineSQL = `
SELECT r.id FROM remedies r
 WHERE r.org_id = $1 AND r.state IN ('proposed','approved') AND r.expires_at <= $2
 ORDER BY r.expires_at, r.id
 LIMIT $3`

// PastDeadline lists the Remedies still proposed or approved whose window has passed at `now`.
func (r *RemedyRepository) PastDeadline(ctx context.Context, s db.TenantScope, now time.Time, limit int) ([]uuid.UUID, error) {
	return r.ids(ctx, s, pastDeadlineSQL, now, limit, "list the Remedies past their deadline")
}

// ⭐ A CLAIM WITH NO ANSWER: `executing` since at or before $2, which the caller computes as
// now − RemedyOutcomeDeadline. Served by `remedies_open_idx`'s predicate and the org.
const outcomeOverdueSQL = `
SELECT r.id FROM remedies r
 WHERE r.org_id = $1 AND r.state = 'executing' AND r.executing_at <= $2
 ORDER BY r.executing_at, r.id
 LIMIT $3`

// OutcomeOverdue lists the Remedies claimed for execution at or before `claimedBy` that
// still have no recorded answer — their worker died mid-call.
func (r *RemedyRepository) OutcomeOverdue(ctx context.Context, s db.TenantScope, claimedBy time.Time, limit int) ([]uuid.UUID, error) {
	return r.ids(ctx, s, outcomeOverdueSQL, claimedBy, limit, "list the Remedies claimed with no answer")
}

func (r *RemedyRepository) ids(ctx context.Context, s db.TenantScope, sql string, at time.Time, limit int, what string) ([]uuid.UUID, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, sql, s.OrgID(), at.UTC(), limit)
	if err != nil {
		return nil, mapRemedyErr(err, what)
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, mapRemedyErr(err, what)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, mapRemedyErr(err, what)
	}
	return out, nil
}

func collectRemedies(rows pgx.Rows) ([]domain.Remedy, error) {
	defer rows.Close()
	out := []domain.Remedy{}
	for rows.Next() {
		rem, err := scanRemedy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rem)
	}
	return out, rows.Err()
}

func scanRemedy(row pgx.Row) (domain.Remedy, error) {
	var (
		out                                  domain.Remedy
		subjectKind, state                   string
		toolServerID                         *uuid.UUID
		toolServerName, toolName, args, hash *string
		approvedAt, executingAt, endedAt     *time.Time
		failure, detail, result              *string
		riskBasis, riskRule, riskDetail      *string
		riskCheck, riskModel                 *string
		riskTokens                           *int64
	)
	if err := row.Scan(&out.ID, &out.OrgID, &out.InvestigationID, &subjectKind, &out.SubjectID, &out.ProposedBy,
		&toolServerID, &toolServerName, &toolName, &args, &hash,
		&out.Target, &out.Description, &out.RequiredApprovals, &state, &out.ProposedAt, &out.ExpiresAt,
		&approvedAt, &executingAt, &endedAt, &failure, &detail, &result,
		&riskBasis, &riskRule, &riskDetail, &riskCheck, &riskModel, &riskTokens); err != nil {
		return domain.Remedy{}, err
	}
	if riskBasis != nil {
		out.Risk = domain.RemedyRisk{Approvals: out.RequiredApprovals, Basis: domain.RiskBasis(*riskBasis),
			Rule: derefS(riskRule), Detail: derefS(riskDetail), Model: domain.RiskModelCheck(derefS(riskCheck)),
			ModelIdentity: derefS(riskModel)}
		if riskTokens != nil {
			out.Risk.ModelTokens = *riskTokens
		}
	}
	var err error
	if out.SubjectKind, err = domain.ParseSubjectKind(subjectKind); err != nil {
		return domain.Remedy{}, err
	}
	if out.State, err = domain.ParseRemedyState(state); err != nil {
		return domain.Remedy{}, err
	}
	if out.Failure, err = domain.ParseRemedyFailure(derefS(failure)); err != nil {
		return domain.Remedy{}, err
	}
	if toolServerID != nil {
		out.Tool = domain.RemedyTool{ToolServerID: *toolServerID, ToolServerName: derefS(toolServerName), Tool: derefS(toolName)}
		out.Arguments, out.ArgumentsSHA256 = derefS(args), derefS(hash)
	}
	out.ProposedAt, out.ExpiresAt = out.ProposedAt.UTC(), out.ExpiresAt.UTC()
	if approvedAt != nil {
		out.ApprovedAt = approvedAt.UTC()
	}
	if executingAt != nil {
		out.ExecutingAt = executingAt.UTC()
	}
	if endedAt != nil {
		out.EndedAt = endedAt.UTC()
	}
	out.Detail, out.Result = derefS(detail), derefS(result)
	return out, nil
}
