package api

// THE REMEDY RISK RULES' TRANSPORT, CHECKED AGAINST THE CONTRACT (ADR 0054 §3, git-bug
// eb4f21b).
//
//   - the read answers the shape the contract declares, and a fresh org has no rules — oto
//     ships none, and every Remedy then needs two;
//   - ⛔ no verb on the path writes them: `oto remedy-rules apply` does, from the host shell, and a
//     CONFIRMED change does (owner ruling O3, 2026-10-06). What is mounted is a proposal, which
//     changes no rule, a confirmation only a different member's browser SESSION may make, and a
//     discard;
//   - a Remedy carries `risk`: what set its tier, or null for one that names no Tool.

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
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

// fxChanges is the fake's per-client pending change, behind its own lock.
var fxChanges = struct {
	mu      sync.Mutex
	pending map[*fakeInvestigators]domain.RiskChange
}{pending: map[*fakeInvestigators]domain.RiskChange{}}

func (f *fakeInvestigators) ProposeRemedyRiskChange(_ context.Context, _ db.TenantScope, by domain.Requester, d domain.RiskChangeDraft) (domain.RiskChange, error) {
	f.record("proposeRemedyRiskChange")
	rules, err := domain.ValidateRiskChange(d)
	if err != nil {
		return domain.RiskChange{}, err
	}
	c := domain.RiskChange{ID: fxRiskChange, OrgID: apitest.OrgID, Rules: rules, RiskModelProviderID: d.RiskModelProviderID,
		Status: domain.RiskChangePending, ProposedBy: by, ProposedAt: fxEpoch}
	fxChanges.mu.Lock()
	fxChanges.pending[f] = c
	fxChanges.mu.Unlock()
	return c, nil
}

func (f *fakeInvestigators) PendingRemedyRiskChange(_ context.Context, s db.TenantScope) (domain.RiskChange, bool, error) {
	fxChanges.mu.Lock()
	defer fxChanges.mu.Unlock()
	c, ok := fxChanges.pending[f]
	return c, ok && mine(s), nil
}

func (f *fakeInvestigators) ConfirmRemedyRiskChange(_ context.Context, s db.TenantScope, id uuid.UUID, by domain.Requester) (domain.RemedyRiskSettings, error) {
	f.record("confirmRemedyRiskChange")
	fxChanges.mu.Lock()
	c, ok := fxChanges.pending[f]
	fxChanges.mu.Unlock()
	if !mine(s) || !ok || id != c.ID {
		return domain.RemedyRiskSettings{}, domain.RiskChangeNotFound()
	}
	if err := c.ConfirmableBy(by); err != nil {
		return domain.RemedyRiskSettings{}, err
	}
	return domain.RemedyRiskSettings{Rules: c.Rules, RiskModelProviderID: c.RiskModelProviderID,
		WrittenByLabel: by.Label, WrittenAt: fxEpoch}, nil
}

func (f *fakeInvestigators) DiscardRemedyRiskChange(_ context.Context, s db.TenantScope, id uuid.UUID, _ domain.Requester) error {
	f.record("discardRemedyRiskChange")
	fxChanges.mu.Lock()
	defer fxChanges.mu.Unlock()
	c, ok := fxChanges.pending[f]
	if !mine(s) || !ok || id != c.ID {
		return domain.RiskChangeNotFound()
	}
	delete(fxChanges.pending, f)
	return nil
}

var fxRiskChange = uuid.MustParse("22222222-2222-4222-8222-222222222222")

const oneRuleBody = `{"rules":[{"name":"restart-payments","tool":"k8s-write__kubectl","verbs":["rollout restart"],` +
	`"kinds":["deployment"],"namespaces":["payments"],"approvals":1}],"risk_model_provider_id":null}`

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
		{Name: "restart-payments", Tool: "k8s-write__kubectl", Verbs: []string{"Rollout Restart"}, Kinds: []string{"deploy"},
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
		first["kinds"].([]any)[0] != "deployment" || first["tool"] != "k8s-write__kubectl" {
		t.Fatalf("the rules came back as %v, want the operator's two, normalised, in order", rules)
	}
	if data["risk_model_provider_id"] != model.String() || data["written_by_label"] != "oto remedy-rules apply" {
		t.Fatalf("the risk model or the writer was lost: %v", data)
	}
}

// TestNoVerbOnTheRulesPathWritesThem — the rules themselves are still never written by a verb on
// their own path; a change is proposed at `/changes`, and applied only by a confirmation.
func TestNoVerbOnTheRulesPathWritesThem(t *testing.T) {
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

// TestAProposalIsPendingAndOnlyADifferentMemberConfirmsIt — ADR 0054 §3, owner ruling O3.
func TestAProposalIsPendingAndOnlyADifferentMemberConfirmsIt(t *testing.T) {
	t.Parallel()
	_, c := newClient(t)

	resp := c.Raw(http.MethodPost, "/remedy-risk-rules/changes", apitest.ContentTypeJSON, oneRuleBody).
		MustStatus(t, http.StatusCreated)
	schema.Assert(t, "proposeRemedyRiskChange", http.StatusCreated, resp.Body())
	if got := resp.JSON(t)["data"].(map[string]any); got["status"] != "pending" || got["proposed_by_you"] != true {
		t.Fatalf("a proposal answered %v, want pending and proposed by the caller", got)
	}

	// The rules are still the rules: a proposal changes nothing, and the read says what waits.
	read := c.GET("/remedy-risk-rules").MustStatus(t, http.StatusOK)
	schema.Assert(t, "getRemedyRiskRules", http.StatusOK, read.Body())
	data := read.JSON(t)["data"].(map[string]any)
	if got := data["rules"].([]any); len(got) != 0 {
		t.Fatalf("a proposal changed the rules: %v", got)
	}
	pending, ok := data["pending_change"].(map[string]any)
	if !ok || pending["proposed_by_you"] != true || len(pending["rules"].([]any)) != 1 {
		t.Fatalf("pending_change = %v", data["pending_change"])
	}

	// ⛔ The proposer cannot confirm their own change.
	self := c.Raw(http.MethodPost, "/remedy-risk-rules/changes/"+fxRiskChange.String()+"/confirm", "", "").
		MustStatus(t, http.StatusForbidden)
	if p := self.Problem(t); p.Code != "remedy_risk_change_needs_a_second_person" {
		t.Fatalf("the proposer's own confirmation answered %q", p.Code)
	}

	// A different member's session confirms it, and the rules come back as they now stand.
	other := apitest.Member()
	other.UserID = uuid.MustParse("99999999-9999-4999-8999-999999999999")
	other.DisplayName = "Grace Hopper"
	done := c.As(other).Raw(http.MethodPost, "/remedy-risk-rules/changes/"+fxRiskChange.String()+"/confirm", "", "").
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "confirmRemedyRiskChange", http.StatusOK, done.Body())
	got := done.JSON(t)["data"].(map[string]any)
	if rules := got["rules"].([]any); len(rules) != 1 || got["written_by_label"] != "Grace Hopper" {
		t.Fatalf("a confirmed change answered %v", got)
	}
}

// TestATokenNeitherProposesNorConfirmsButMayDiscard — a script holding two members' tokens must not
// be the two people, so both are session-only; saying no stays open to a token.
func TestATokenNeitherProposesNorConfirmsButMayDiscard(t *testing.T) {
	t.Parallel()
	f, c := newClient(t)
	pat := apitest.Member()
	pat.Kind = authn.KindPAT
	tok := c.As(pat)

	for _, resp := range []*apitest.Response{
		tok.Raw(http.MethodPost, "/remedy-risk-rules/changes", apitest.ContentTypeJSON, oneRuleBody),
		tok.Raw(http.MethodPost, "/remedy-risk-rules/changes/"+fxRiskChange.String()+"/confirm", "", ""),
	} {
		resp.MustStatus(t, http.StatusForbidden)
		if p := resp.Problem(t); p.Code != "remedy_risk_change_needs_a_session" {
			t.Fatalf("a token answered %q, want remedy_risk_change_needs_a_session", p.Code)
		}
	}
	if n := f.callCount(); n != 0 {
		t.Fatalf("a refused token reached the service (%d call(s))", n)
	}

	c.Raw(http.MethodPost, "/remedy-risk-rules/changes", apitest.ContentTypeJSON, oneRuleBody).MustStatus(t, http.StatusCreated)
	tok.Raw(http.MethodPost, "/remedy-risk-rules/changes/"+fxRiskChange.String()+"/discard", "", "").MustStatus(t, http.StatusNoContent)
}

// TestARuleThatSaysOneMustNameItsToolOnTheTransportToo — the CLI's validation, with the CLI's
// violations: nothing is stored for a rule the domain refuses.
func TestARuleThatSaysOneMustNameItsToolOnTheTransportToo(t *testing.T) {
	t.Parallel()
	_, c := newClient(t)
	resp := c.Raw(http.MethodPost, "/remedy-risk-rules/changes", apitest.ContentTypeJSON,
		`{"rules":[{"name":"loose","verbs":["delete"],"approvals":1}]}`).MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "proposeRemedyRiskChange", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "rules/0/tool")
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
