package api

// REMEDY RISK RULES (ADR 0054 §3; git-bug eb4f21b): the org's rules over a Remedy's command and
// its risk model, READ at `GET /api/v1/remedy-risk-rules`; and, on every RemedyDTO, how its
// tier was set.
//
// ⛔⛔ READ-ONLY (owner ruling 2026-10-05). The rules are applied whole from the host shell by
// `oto remedy-rules apply --org SLUG -f rules.yaml` (internal/app/remedyrules.go), like an
// approval grant: a rule saying one lets one grant holder approve alone, so any member who could
// write one over HTTP could approve alone. No route, request DTO or handler here writes them.

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
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
	httpx.Data(w, r, http.StatusOK, remedyRiskRulesDTO(set), started)
}
