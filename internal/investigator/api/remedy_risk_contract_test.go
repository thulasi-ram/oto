package api

// THE REMEDY RISK RULES' TRANSPORT, CHECKED AGAINST THE CONTRACT (ADR 0054 §3, git-bug
// eb4f21b).
//
//   - read and replace answer the shape the contract declares, and a fresh org has no rules —
//     oto ships none, and every Remedy then needs two;
//   - a rule oto cannot apply is a 422 naming its field, and nothing reaches the service;
//   - a system principal cannot write them;
//   - a Remedy carries `risk`: what set its tier, or null for one that names no Tool.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/schema"
)

// fxRisk is the fake's per-client rules, behind its own lock, for fxClasses's reason.
var fxRisk = struct {
	mu   sync.Mutex
	sets map[*fakeInvestigators]domain.RemedyRiskSettings
	by   map[*fakeInvestigators]domain.Requester
}{sets: map[*fakeInvestigators]domain.RemedyRiskSettings{}, by: map[*fakeInvestigators]domain.Requester{}}

func (f *fakeInvestigators) RemedyRisk(_ context.Context, s db.TenantScope) (domain.RemedyRiskSettings, error) {
	if !mine(s) {
		return domain.RemedyRiskSettings{}, nil
	}
	fxRisk.mu.Lock()
	defer fxRisk.mu.Unlock()
	return fxRisk.sets[f], nil
}

func (f *fakeInvestigators) ReplaceRemedyRisk(_ context.Context, s db.TenantScope, set domain.RemedyRiskSettings, by domain.Requester) (domain.RemedyRiskSettings, error) {
	f.record("replaceRemedyRiskRules")
	if !mine(s) {
		return domain.RemedyRiskSettings{}, errs.Internal("wrong_org", nil)
	}
	fxRisk.mu.Lock()
	defer fxRisk.mu.Unlock()
	set.WrittenByLabel, set.WrittenAt = by.Label, fxEpoch
	fxRisk.sets[f], fxRisk.by[f] = set, by
	return set, nil
}

func TestTheRiskRulesAreReadAndReplacedWholeAndShipEmpty(t *testing.T) {
	t.Parallel()
	f, c := newClient(t)

	resp := c.GET("/remedy-risk-rules").MustStatus(t, http.StatusOK)
	schema.Assert(t, "getRemedyRiskRules", http.StatusOK, resp.Body())
	data := resp.JSON(t)["data"].(map[string]any)
	if got := data["rules"].([]any); len(got) != 0 || data["risk_model_provider_id"] != nil {
		t.Fatalf("a fresh org has rules %v or a risk model — oto ships none", data)
	}
	if verbs := data["reversible_verbs"].([]any); len(verbs) == 0 {
		t.Fatalf("the reversible verbs are not said")
	}

	model := uuid.New()
	resp = c.PUT(t, "/remedy-risk-rules", map[string]any{
		"rules": []map[string]any{
			{"name": "restart-payments", "verbs": []string{"Rollout Restart"}, "kinds": []string{"deploy"},
				"namespaces": []string{"payments"}, "approvals": 1},
			{"name": "secrets-need-two", "tool": "k8s-write__kubectl", "kinds": []string{"secrets"},
				"reversibility": "irreversible", "approvals": 2},
		},
		"risk_model_provider_id": model.String(),
	}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "replaceRemedyRiskRules", http.StatusOK, resp.Body())
	data = resp.JSON(t)["data"].(map[string]any)
	rules := data["rules"].([]any)
	first := rules[0].(map[string]any)
	if len(rules) != 2 || first["name"] != "restart-payments" || first["verbs"].([]any)[0] != "rollout restart" ||
		first["kinds"].([]any)[0] != "deployment" || first["tool"] != nil {
		t.Fatalf("the rules came back as %v, want the operator's two, normalised, in order", rules)
	}
	if data["risk_model_provider_id"] != model.String() || data["written_by_label"] == nil {
		t.Fatalf("the risk model or the writer was lost: %v", data)
	}
	fxRisk.mu.Lock()
	by := fxRisk.by[f]
	fxRisk.mu.Unlock()
	if by.Label == "" {
		t.Fatalf("the service was not told who wrote the rules")
	}

	// No rules, no model: every Remedy needs two again.
	resp = c.PUT(t, "/remedy-risk-rules", map[string]any{"rules": []any{}}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "replaceRemedyRiskRules", http.StatusOK, resp.Body())
	if data := resp.JSON(t)["data"].(map[string]any); data["risk_model_provider_id"] != nil {
		t.Fatalf("an absent risk model kept one: %v", data)
	}
}

func TestARiskRuleOtoCannotApplyIsRefusedByName(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		rule  map[string]any
		field string
	}{
		"no condition":        {map[string]any{"name": "everything", "approvals": 1}, "rules/0"},
		"three approvals":     {map[string]any{"name": "x", "verbs": []string{"delete"}, "approvals": 3}, "approvals"},
		"a capital name":      {map[string]any{"name": "Restart", "verbs": []string{"delete"}, "approvals": 1}, "rules/0/name"},
		"a verb with a pipe":  {map[string]any{"name": "x", "verbs": []string{"get | sh"}, "approvals": 1}, "rules/0/verbs/0"},
		"a wildcard ns":       {map[string]any{"name": "x", "namespaces": []string{"*"}, "approvals": 1}, "rules/0/namespaces/0"},
		"an unqualified tool": {map[string]any{"name": "x", "tool": "kubectl", "approvals": 1}, "rules/0/tool"},
		"maybe reversible":    {map[string]any{"name": "x", "verbs": []string{"delete"}, "reversibility": "maybe", "approvals": 1}, "reversibility"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, c := newClient(t)
			resp := c.PUT(t, "/remedy-risk-rules", map[string]any{"rules": []any{tc.rule}}).
				MustStatus(t, http.StatusUnprocessableEntity)
			schema.AssertProblem(t, "replaceRemedyRiskRules", http.StatusUnprocessableEntity, resp.Body())
			if body := string(resp.Body()); !strings.Contains(body, tc.field) {
				t.Fatalf("the 422 does not name %s: %s", tc.field, body)
			}
			if n := f.callCount(); n != 0 {
				t.Fatalf("a refused rule reached the service (%d call(s))", n)
			}
		})
	}
	f, c := newClient(t)
	c.PUT(t, "/remedy-risk-rules", map[string]any{}).MustStatus(t, http.StatusUnprocessableEntity)
	if f.callCount() != 0 {
		t.Fatal("a body naming no rules reached the service")
	}
}

func TestARemedyCarriesWhatSetItsTier(t *testing.T) {
	t.Parallel()
	_, c := newClient(t)
	resp := c.GET("/investigations/"+fxInvestigation.String()+"/remedies").MustStatus(t, http.StatusOK)
	list := resp.JSON(t)["data"].([]any)
	tool, none := list[0].(map[string]any), list[1].(map[string]any)
	risk, ok := tool["risk"].(map[string]any)
	if !ok || risk["set_by"] != "risk_model" || risk["rule"] != "scale-checkout" || risk["risk_model_check"] != "raised" ||
		risk["detail"] == nil || risk["risk_model_tokens"] != float64(135) {
		t.Fatalf("risk = %v", tool["risk"])
	}
	if v, ok := none["risk"]; !ok || v != nil {
		t.Fatalf("a Remedy with no Tool answered risk %v (present %v), want null", v, ok)
	}
}
