package api

// THE REMEDY RISK RULES' TRANSPORT, CHECKED AGAINST THE CONTRACT (ADR 0054 §3, git-bug
// eb4f21b).
//
//   - the read answers the shape the contract declares, and a fresh org has no rules — oto
//     ships none, and every Remedy then needs two;
//   - ⛔ no verb on the path writes them: `oto remedy-rules apply` does, from the host shell
//     (owner ruling 2026-10-05);
//   - a Remedy carries `risk`: what set its tier, or null for one that names no Tool.

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// fxRisk is the fake's per-client rules, behind its own lock, for fxClasses's reason. Only a
// test seeds them: nothing on the transport writes them.
var fxRisk = struct {
	mu   sync.Mutex
	sets map[*fakeInvestigators]domain.RemedyRiskSettings
}{sets: map[*fakeInvestigators]domain.RemedyRiskSettings{}}

func (f *fakeInvestigators) RemedyRisk(_ context.Context, s db.TenantScope) (domain.RemedyRiskSettings, error) {
	if !mine(s) {
		return domain.RemedyRiskSettings{}, nil
	}
	fxRisk.mu.Lock()
	defer fxRisk.mu.Unlock()
	return fxRisk.sets[f], nil
}

func TestTheRiskRulesAreReadAndShipEmpty(t *testing.T) {
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

	// What `oto remedy-rules apply` wrote reads back, in order, with its writer.
	rs, err := domain.NewRiskRules([]domain.RiskRule{
		{Name: "restart-payments", Verbs: []string{"Rollout Restart"}, Kinds: []string{"deploy"},
			Namespaces: []string{"payments"}, Approvals: 1},
		{Name: "secrets-need-two", Tool: "k8s-write__kubectl", Kinds: []string{"secrets"},
			Reversibility: domain.Irreversible, Approvals: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	model := uuid.New()
	fxRisk.mu.Lock()
	fxRisk.sets[f] = domain.RemedyRiskSettings{Rules: rs, RiskModelProviderID: model,
		WrittenByLabel: "oto remedy-rules apply", WrittenAt: fxEpoch}
	fxRisk.mu.Unlock()

	resp = c.GET("/remedy-risk-rules").MustStatus(t, http.StatusOK)
	schema.Assert(t, "getRemedyRiskRules", http.StatusOK, resp.Body())
	data = resp.JSON(t)["data"].(map[string]any)
	rules := data["rules"].([]any)
	first := rules[0].(map[string]any)
	if len(rules) != 2 || first["name"] != "restart-payments" || first["verbs"].([]any)[0] != "rollout restart" ||
		first["kinds"].([]any)[0] != "deployment" || first["tool"] != nil {
		t.Fatalf("the rules came back as %v, want the operator's two, normalised, in order", rules)
	}
	if data["risk_model_provider_id"] != model.String() || data["written_by_label"] != "oto remedy-rules apply" {
		t.Fatalf("the risk model or the writer was lost: %v", data)
	}
}

// TestNothingOnTheTransportWritesTheRiskRules — owner ruling 2026-10-05 on git-bug eb4f21b:
// every write verb on the path is refused before any service is reached, and the rules read
// back unchanged.
func TestNothingOnTheTransportWritesTheRiskRules(t *testing.T) {
	t.Parallel()
	f, c := newClient(t)
	body := map[string]any{"rules": []map[string]any{{"name": "everything-one", "verbs": []string{"delete"}, "approvals": 1}}}
	for _, resp := range []*apitest.Response{
		c.PUT(t, "/remedy-risk-rules", body),
		c.POST(t, "/remedy-risk-rules", body),
		c.PATCH(t, "/remedy-risk-rules", body),
		c.DELETE("/remedy-risk-rules"),
	} {
		if got := resp.Code(); got != http.StatusMethodNotAllowed && got != http.StatusNotFound {
			t.Fatalf("a write to the risk rules answered %d, want 405 or 404", got)
		}
	}
	if n := f.callCount(); n != 0 {
		t.Fatalf("a write to the risk rules reached the service (%d call(s))", n)
	}
	data := c.GET("/remedy-risk-rules").MustStatus(t, http.StatusOK).JSON(t)["data"].(map[string]any)
	if got := data["rules"].([]any); len(got) != 0 {
		t.Fatalf("a refused write left rules %v", got)
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
