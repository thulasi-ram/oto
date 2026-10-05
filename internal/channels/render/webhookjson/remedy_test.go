package webhookjson_test

// A REMEDY FACT ON THE WIRE (ADR 0054 §2, git-bug 4148256): `incident.remedy` carries the
// transition, the exact command first — the write Tool and the EXACT arguments as the JSON
// object itself, with their hash — or `no_tool`, the sentence, and no Tool; then the target, the
// Investigator's description, the approvals so far and who made the transition. A fact, never
// a command.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/channels/domain"
)

const wireArgs = `{"namespace":"checkout","deployment":"api","generation":12345678901234567890}`

func remedyView(reason string, withTool bool) *domain.NotificationView {
	v := incidentView(reason)
	r := &domain.IncidentRemedyView{
		RemedyID: "0199a1b2-c3d4-7e5f-8a9b-00000000e1d1", InvestigationID: "0199a1b2-c3d4-7e5f-8a9b-00000000f1d1",
		State: strings.TrimPrefix(reason, "remedy_"), From: "proposed",
		Target: "Deployment checkout/api", Description: "Restart the api to pick up the reverted config.",
		ProposedBy: "Investigator firstlook v2", RequiredApprovals: 2,
		Approvals: []domain.IncidentRemedyApprovalView{
			{Label: "Ada Lovelace", ApprovedAt: renderedAt.Add(-3 * 60e9)},
			{Label: "Grace Hopper", ApprovedAt: renderedAt.Add(-60e9)},
		},
		ActorKind: "user", ActorLabel: "Grace Hopper", At: renderedAt.Add(-60e9), ExpiresAt: renderedAt.Add(3600e9),
	}
	if withTool {
		r.ToolServer, r.Tool, r.Arguments = "k8s-write", "rollout_restart", wireArgs
		r.ArgumentsSHA256 = "1e0d0b0f00000000000000000000000000000000000000000000000000000000"
	} else {
		r.NoTool = "no configured Tool can carry this out"
	}
	v.Incident.Remedy = r
	return v
}

func remedyOf(t *testing.T, payload []byte) map[string]json.RawMessage {
	t.Helper()
	var env struct {
		Incident struct {
			Remedy map[string]json.RawMessage `json:"remedy"`
		} `json:"incident"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	return env.Incident.Remedy
}

func TestARemedyFactCarriesTheExactCommandFirst(t *testing.T) {
	t.Parallel()
	payload := render(t, remedyView("remedy_approved", true)).Payload
	rm := remedyOf(t, payload)
	if rm == nil {
		t.Fatalf("a Remedy fact carries no incident.remedy: %s", payload)
	}
	var tool struct {
		ToolServer      string          `json:"tool_server"`
		Tool            string          `json:"tool"`
		Arguments       json.RawMessage `json:"arguments"`
		ArgumentsSHA256 string          `json:"arguments_sha256"`
	}
	if err := json.Unmarshal(rm["tool"], &tool); err != nil {
		t.Fatal(err)
	}
	// ⭐ THE ARGUMENTS ARE THE OBJECT ITSELF, BYTE FOR BYTE: key order and the 20-digit number
	// a float would round are exactly as the Investigator wrote them.
	var compact bytes.Buffer
	if err := json.Compact(&compact, tool.Arguments); err != nil || compact.String() != wireArgs {
		t.Fatalf("arguments on the wire = %s, want %s", tool.Arguments, wireArgs)
	}
	if tool.Tool != "rollout_restart" || tool.ToolServer != "k8s-write" || tool.ArgumentsSHA256 == "" {
		t.Fatalf("tool = %+v", tool)
	}
	if _, ok := rm["no_tool"]; ok {
		t.Fatal("a Remedy with a Tool says no Tool can carry it out")
	}
	var approvals []map[string]any
	if err := json.Unmarshal(rm["approvals"], &approvals); err != nil || len(approvals) != 2 {
		t.Fatalf("approvals = %s", rm["approvals"])
	}
	if !strings.Contains(string(payload), `"summary":"[INCIDENT] #4 a Remedy was approved (k8s-write__rollout_restart on Deployment checkout/api) by Grace Hopper`) {
		t.Fatalf("summary does not name the command: %s", payload)
	}
	lower := strings.ToLower(string(payload))
	for _, command := range []string{"resolve", "closed_by", "mitigat", "status"} {
		if strings.Contains(lower, command) {
			t.Errorf("a Remedy envelope says %q — oto declares facts and never commands an incident tool", command)
		}
	}
}

func TestARemedyWithNoToolSaysSoOnTheWire(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"remedy_proposed", "remedy_declined", "remedy_expired"} {
		rm := remedyOf(t, render(t, remedyView(reason, false)).Payload)
		var noTool string
		if err := json.Unmarshal(rm["no_tool"], &noTool); err != nil || noTool != "no configured Tool can carry this out" {
			t.Fatalf("%s: no_tool = %s", reason, rm["no_tool"])
		}
		if _, ok := rm["tool"]; ok {
			t.Fatalf("%s: a Remedy with no Tool names one: %s", reason, rm["tool"])
		}
	}
}

func TestEveryRemedyFactRendersAndValidates(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"remedy_proposed", "remedy_approved", "remedy_declined", "remedy_expired",
		"remedy_executed", "remedy_failed"} {
		v := remedyView(reason, true)
		if reason == "remedy_failed" {
			v.Incident.Remedy.FailureReason, v.Incident.Remedy.Detail = "tool_error", "forbidden"
		}
		rm := remedyOf(t, render(t, v).Payload)
		var state string
		if err := json.Unmarshal(rm["state"], &state); err != nil || "remedy_"+state != reason {
			t.Fatalf("%s: state = %s", reason, rm["state"])
		}
	}
}

// TestARemedyFactSaysWhatSetItsTier — `required_approvals` travels with what set it and the
// rule's name (git-bug eb4f21b), and both are absent when nothing was recorded.
func TestARemedyFactSaysWhatSetItsTier(t *testing.T) {
	t.Parallel()
	v := remedyView("remedy_proposed", true)
	v.Incident.Remedy.RequiredApprovals = 1
	v.Incident.Remedy.ApprovalsSetBy, v.Incident.Remedy.ApprovalsRule = "rule", "restart-payments"
	rm := remedyOf(t, render(t, v).Payload)
	if string(rm["required_approvals"]) != "1" || string(rm["approvals_set_by"]) != `"rule"` ||
		string(rm["approvals_rule"]) != `"restart-payments"` {
		t.Fatalf("remedy = %s %s %s", rm["required_approvals"], rm["approvals_set_by"], rm["approvals_rule"])
	}
	rm = remedyOf(t, render(t, remedyView("remedy_proposed", false)).Payload)
	if _, ok := rm["approvals_set_by"]; ok {
		t.Fatalf("a Remedy with no risk record says what set its tier: %s", rm["approvals_set_by"])
	}
}
