package api

// REMEDY RISK RULES (ADR 0054 §3; git-bug eb4f21b): the org's rules over a Remedy's command and
// its risk model, READ at `GET /api/v1/remedy-risk-rules`; and, on every RemedyDTO, how its
// tier was set.
//
// ⛔⛔ NO HANDLER HERE WRITES A RULE (owner ruling 2026-10-05, refined by O3, 2026-10-06). A rule
// saying one lets one grant holder approve alone, so a member who could write one over HTTP could
// approve alone. The rules are written in `internal/app`: by `oto remedy-rules apply` from the host
// shell, or by a change that a DIFFERENT member's browser session confirmed. What is mounted here is
// a PROPOSAL, which changes no tier, that confirmation, and a discard.

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// RemedyRiskRuleDTO renders `RemedyRiskRuleDTO`: one rule, normalised as stored.
type RemedyRiskRuleDTO struct {
	Name          string   `json:"name"`
	Tool          *string  `json:"tool"`
	Verbs         []string `json:"verbs"`
	Kinds         []string `json:"kinds"`
	Namespaces    []string `json:"namespaces"`
	Reversibility *string  `json:"reversibility"`
	Approvals     int      `json:"approvals"`
}

// RemedyRiskRulesDTO renders `RemedyRiskRulesDTO`: the org's rules in order, its risk model,
// who wrote them last, and the verbs oto knows to be reversible.
type RemedyRiskRulesDTO struct {
	Rules               []RemedyRiskRuleDTO `json:"rules"`
	RiskModelProviderID *uuid.UUID          `json:"risk_model_provider_id"`
	WrittenByLabel      *string             `json:"written_by_label"`
	WrittenAt           *time.Time          `json:"written_at"`
	ReversibleVerbs     []string            `json:"reversible_verbs"`
	// PendingChange is the change waiting for a second person; null when there is none.
	PendingChange *RemedyRiskChangeDTO `json:"pending_change"`
}

// RemedyRiskChangeDTO renders `RemedyRiskChangeDTO`: a proposed replacement of the rules, which
// changes no Remedy's tier until a different member confirms it.
type RemedyRiskChangeDTO struct {
	ID                  uuid.UUID           `json:"id"`
	Rules               []RemedyRiskRuleDTO `json:"rules"`
	RiskModelProviderID *uuid.UUID          `json:"risk_model_provider_id"`
	Status              string              `json:"status"`
	ProposedByLabel     string              `json:"proposed_by_label"`
	ProposedAt          time.Time           `json:"proposed_at"`
	// ProposedByYou is whether the caller proposed it, and so cannot confirm it. The proposer's
	// user id is not served: the label says who, and this says whether it is you.
	ProposedByYou bool `json:"proposed_by_you"`
}

func remedyRiskChangeDTO(c domain.RiskChange, caller uuid.UUID) RemedyRiskChangeDTO {
	out := RemedyRiskChangeDTO{ID: c.ID, Status: string(c.Status), ProposedByLabel: c.ProposedBy.Label,
		ProposedAt: c.ProposedAt, ProposedByYou: c.ProposedByUser(caller),
		Rules: ruleDTOs(c.Rules.Rules())}
	if c.RiskModelProviderID != uuid.Nil {
		id := c.RiskModelProviderID
		out.RiskModelProviderID = &id
	}
	return out
}

func ruleDTOs(rules []domain.RiskRule) []RemedyRiskRuleDTO {
	out := make([]RemedyRiskRuleDTO, 0, len(rules))
	for _, r := range rules {
		out = append(out, RemedyRiskRuleDTO{Name: r.Name, Tool: optString(r.Tool),
			Verbs: r.Verbs, Kinds: r.Kinds, Namespaces: r.Namespaces,
			Reversibility: optString(string(r.Reversibility)), Approvals: r.Approvals})
	}
	return out
}

// RemedyRiskRuleRequest is one rule as proposed: the names of `RemedyRiskRuleDTO`. The domain
// validates it exactly as `oto remedy-rules apply` does, and its violations name `rules/<i>/<field>`.
type RemedyRiskRuleRequest struct {
	Name          string   `json:"name"          validate:"required,max=63"`
	Tool          *string  `json:"tool"`
	Verbs         []string `json:"verbs"         validate:"omitempty,max=20"`
	Kinds         []string `json:"kinds"         validate:"omitempty,max=20"`
	Namespaces    []string `json:"namespaces"    validate:"omitempty,max=20"`
	Reversibility *string  `json:"reversibility"`
	Approvals     int      `json:"approvals"     validate:"required,oneof=1 2"`
}

// ProposeRemedyRiskChangeRequest is the body of `POST /api/v1/remedy-risk-rules/changes`: the WHOLE
// rule set, in order, and the risk model. `rules: []` is legal and says every Remedy needs two.
type ProposeRemedyRiskChangeRequest struct {
	Rules               []RemedyRiskRuleRequest `json:"rules"                  validate:"required,max=100,dive"`
	RiskModelProviderID *uuid.UUID              `json:"risk_model_provider_id"`
}

func (r ProposeRemedyRiskChangeRequest) toDomain() domain.RiskChangeDraft {
	d := domain.RiskChangeDraft{Rules: make([]domain.RiskRule, 0, len(r.Rules))}
	for _, in := range r.Rules {
		rule := domain.RiskRule{Name: in.Name, Verbs: in.Verbs, Kinds: in.Kinds, Namespaces: in.Namespaces,
			Approvals: in.Approvals}
		if in.Tool != nil {
			rule.Tool = *in.Tool
		}
		if in.Reversibility != nil {
			rule.Reversibility = domain.Reversibility(*in.Reversibility)
		}
		d.Rules = append(d.Rules, rule)
	}
	if r.RiskModelProviderID != nil {
		d.RiskModelProviderID = *r.RiskModelProviderID
	}
	return d
}

func remedyRiskRulesDTO(set domain.RemedyRiskSettings) RemedyRiskRulesDTO {
	rules := set.Rules.Rules()
	out := RemedyRiskRulesDTO{Rules: make([]RemedyRiskRuleDTO, 0, len(rules)),
		WrittenByLabel: optString(set.WrittenByLabel), WrittenAt: optTime(set.WrittenAt),
		ReversibleVerbs: domain.ReversibleVerbs()}
	if set.RiskModelProviderID != uuid.Nil {
		id := set.RiskModelProviderID
		out.RiskModelProviderID = &id
	}
	out.Rules = ruleDTOs(rules)
	return out
}

// RemedyRiskDTO renders `RemedyRiskDTO`: how a Remedy's required approvals were set, shown
// under its exact command. `set_by` is rule, no_rule, unparseable, risk_model (it raised the
// tier), risk_model_failed or risk_model_budget (the day's token budget was spent, so it was
// not asked: two).
type RemedyRiskDTO struct {
	SetBy           string  `json:"set_by"`
	Rule            *string `json:"rule"`
	Detail          *string `json:"detail"`
	RiskModelCheck  string  `json:"risk_model_check"`
	RiskModel       *string `json:"risk_model"`
	RiskModelTokens *int64  `json:"risk_model_tokens"`
}

// remedyRiskDTO is nil for a Remedy with no risk record: one that names no Tool, or one
// proposed before the rules existed.
func remedyRiskDTO(r domain.RemedyRisk) *RemedyRiskDTO {
	if !r.Recorded() {
		return nil
	}
	out := &RemedyRiskDTO{SetBy: r.SetBy(), Rule: optString(r.Rule), Detail: optString(r.Detail),
		RiskModelCheck: string(r.Model), RiskModel: optString(r.ModelIdentity)}
	switch r.Model {
	case domain.ModelKept, domain.ModelRaised, domain.ModelFailed:
		n := r.ModelTokens
		out.RiskModelTokens = &n
	}
	return out
}

// getRemedyRiskRules serves GET /api/v1/remedy-risk-rules.
func (rt *Router) getRemedyRiskRules(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := plainScope(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	set, err := rt.svc.RemedyRisk(r.Context(), scope)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto := remedyRiskRulesDTO(set)
	pending, ok, err := rt.svc.PendingRemedyRiskChange(r.Context(), scope)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if ok {
		caller := uuid.Nil
		if p, _, err := authn.Scope(r.Context()); err == nil {
			caller = p.UserID
		}
		c := remedyRiskChangeDTO(pending, caller)
		dto.PendingChange = &c
	}
	httpx.Data(w, r, http.StatusOK, dto, started)
}

// humanOnRiskChange is the scope and the human behind a rule change, refusing anything that is not a
// human, and — for a proposal or a confirmation — anything but a browser SESSION.
//
// ⛔⛔ A TOKEN DOES NOT CHANGE THE RULES, for an approval's reason (`remedyApprovalNeedsASession`): a
// script holding two members' personal access tokens would be the two people that make a change
// count. Discarding stays open to a token — saying no is the safe direction.
func humanOnRiskChange(r *http.Request, sessionOnly bool) (db.TenantScope, domain.Requester, error) {
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		return db.TenantScope{}, domain.Requester{}, err
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		return db.TenantScope{}, domain.Requester{}, err
	}
	if kind, err := kernel.NewActorKind(p.ActorKind()); err != nil || !kind.IsHuman() {
		return db.TenantScope{}, domain.Requester{}, errs.Forbidden("forbidden", "changing the Remedy rules requires a human actor")
	}
	if sessionOnly && p.Kind != authn.KindSession {
		return db.TenantScope{}, domain.Requester{}, errs.Forbidden("remedy_risk_change_needs_a_session",
			"the Remedy rules are changed only from a signed-in browser session; a token cannot propose or confirm a change")
	}
	by, err := domain.NewRequester(p.UserID, p.ActorLabel())
	if err != nil {
		return db.TenantScope{}, domain.Requester{}, err
	}
	return scope, by, nil
}

// proposeRemedyRiskChange serves POST /api/v1/remedy-risk-rules/changes. ⛔ It writes no rule: it
// stores a pending change that a different member must confirm, superseding any that was pending.
func (rt *Router) proposeRemedyRiskChange(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, by, err := humanOnRiskChange(r, true)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[ProposeRemedyRiskChangeRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	c, err := rt.svc.ProposeRemedyRiskChange(r.Context(), scope, by, dto.toDomain())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, remedyRiskChangeDTO(c, by.UserID), started)
}

// confirmRemedyRiskChange serves POST /api/v1/remedy-risk-rules/changes/{id}/confirm: a DIFFERENT
// member's browser session applying the pending change. It answers the rules as they now stand.
func (rt *Router) confirmRemedyRiskChange(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, by, err := humanOnRiskChange(r, true)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.WriteProblem(w, r, domain.RiskChangeNotFound())
		return
	}
	set, err := rt.svc.ConfirmRemedyRiskChange(r.Context(), scope, id, by)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, remedyRiskRulesDTO(set), started)
}

// discardRemedyRiskChange serves POST /api/v1/remedy-risk-rules/changes/{id}/discard: any human
// withdrawing or refusing the pending change. `204`.
func (rt *Router) discardRemedyRiskChange(w http.ResponseWriter, r *http.Request) {
	scope, by, err := humanOnRiskChange(r, false)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.WriteProblem(w, r, domain.RiskChangeNotFound())
		return
	}
	if err := rt.svc.DiscardRemedyRiskChange(r.Context(), scope, id, by); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusNoContent, nil)
}
