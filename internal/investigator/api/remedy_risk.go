package api

// REMEDY RISK RULES (ADR 0054 §3; git-bug eb4f21b): the org's rules over a Remedy's command and
// its risk model, read and replaced whole at `/api/v1/remedy-risk-rules`; and, on every
// RemedyDTO, how its tier was set.
//
// ⭐ READ AND REPLACED WHOLE, like the Classification set: the rules are one list whose order
// names the rule that set a tier, and a partial edit of it is a list nobody wrote. Who wrote it
// last, and when, is recorded and shown.
//
// ⛔ A HUMAN WRITES THE RULES. A system principal is refused before the service is reached.

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
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
	for _, r := range rules {
		out.Rules = append(out.Rules, RemedyRiskRuleDTO{Name: r.Name, Tool: optString(r.Tool),
			Verbs: r.Verbs, Kinds: r.Kinds, Namespaces: r.Namespaces,
			Reversibility: optString(string(r.Reversibility)), Approvals: r.Approvals})
	}
	return out
}

// RemedyRiskRuleRequest is one rule in `ReplaceRemedyRiskRulesRequest`. What a verb, kind or
// namespace may be, and that a rule names at least one condition, are the domain's to refuse
// (domain.NewRiskRules), each with the field it is about.
type RemedyRiskRuleRequest struct {
	Name          string   `json:"name"                    validate:"required,min=1,max=63"`
	Tool          *string  `json:"tool,omitempty"          validate:"omitempty,max=160"`
	Verbs         []string `json:"verbs,omitempty"         validate:"max=20,dive,max=63"`
	Kinds         []string `json:"kinds,omitempty"         validate:"max=20,dive,max=63"`
	Namespaces    []string `json:"namespaces,omitempty"    validate:"max=20,dive,max=63"`
	Reversibility *string  `json:"reversibility,omitempty" validate:"omitempty,oneof=reversible irreversible"`
	Approvals     int      `json:"approvals"               validate:"required,oneof=1 2"`
}

// ReplaceRemedyRiskRulesRequest is the body of `PUT /api/v1/remedy-risk-rules`: the whole rule
// list, which replaces the old one, and the risk model (absent or null for none). An empty list
// is legal, and is how an operator makes every Remedy need two approvals again.
type ReplaceRemedyRiskRulesRequest struct {
	Rules               []RemedyRiskRuleRequest `json:"rules"                            validate:"required,max=100,dive"`
	RiskModelProviderID *uuid.UUID              `json:"risk_model_provider_id,omitempty"`
}

func (dto ReplaceRemedyRiskRulesRequest) toDomain() (domain.RemedyRiskSettings, error) {
	rules := make([]domain.RiskRule, 0, len(dto.Rules))
	for _, r := range dto.Rules {
		rule := domain.RiskRule{Name: r.Name, Verbs: r.Verbs, Kinds: r.Kinds, Namespaces: r.Namespaces, Approvals: r.Approvals}
		if r.Tool != nil {
			rule.Tool = *r.Tool
		}
		if r.Reversibility != nil {
			rule.Reversibility = domain.Reversibility(*r.Reversibility)
		}
		rules = append(rules, rule)
	}
	rs, err := domain.NewRiskRules(rules)
	if err != nil {
		return domain.RemedyRiskSettings{}, err
	}
	out := domain.RemedyRiskSettings{Rules: rs}
	if dto.RiskModelProviderID != nil {
		out.RiskModelProviderID = *dto.RiskModelProviderID
	}
	return out, nil
}

// RemedyRiskDTO renders `RemedyRiskDTO`: how a Remedy's required approvals were set, shown
// under its exact command. `set_by` is rule, no_rule, unparseable, risk_model (it raised the
// tier) or risk_model_failed.
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
	httpx.Data(w, r, http.StatusOK, remedyRiskRulesDTO(set), started)
}

// replaceRemedyRiskRules serves PUT /api/v1/remedy-risk-rules: the whole list and the risk
// model, replacing the old ones. ⛔ No Remedy already proposed is re-tiered.
func (rt *Router) replaceRemedyRiskRules(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if kind, err := kernel.NewActorKind(p.ActorKind()); err != nil || !kind.IsHuman() {
		httpx.WriteProblem(w, r, errs.Forbidden("forbidden", "writing the Remedy risk rules requires a human actor"))
		return
	}
	by, err := domain.NewRequester(p.UserID, p.ActorLabel())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[ReplaceRemedyRiskRulesRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	set, err := dto.toDomain()
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	stored, err := rt.svc.ReplaceRemedyRisk(r.Context(), scope, set, by)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, remedyRiskRulesDTO(stored), started)
}
