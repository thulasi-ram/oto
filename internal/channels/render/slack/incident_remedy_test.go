package slack_test

// A REMEDY FACT IN AN INCIDENT'S THREAD (ADR 0054 §2, §3; git-bug 4148256): the reply says the
// transition and who made it, then ⭐ THE EXACT COMMAND — the write Tool and its arguments, or
// that no configured Tool can carry it out — and only after it the Investigator's description.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

func TestARemedyReplySaysTheExactCommandBeforeTheDescription(t *testing.T) {
	t.Parallel()
	v := incidentView()
	v.Reason = "remedy_approved"
	v.Incident.Remedy = &domain.IncidentRemedyView{
		RemedyID: "019fe297-d84f-7599-b5b2-1f23174910e1", State: "approved", From: "proposed",
		ToolServer: "k8s-write", Tool: "rollout_restart",
		Arguments: `{"namespace":"checkout","deployment":"api"}`,
		Target:    "Deployment checkout/api", Description: "Restart the api to pick up the reverted config.",
		RequiredApprovals: 2,
		Approvals: []domain.IncidentRemedyApprovalView{
			{Label: "Ada Lovelace", ApprovedAt: renderedAt.Add(-2 * time.Minute)},
			{Label: "Grace Hopper", ApprovedAt: renderedAt.Add(-time.Minute)},
		},
		ActorKind: "user", ActorLabel: "Grace Hopper", At: renderedAt.Add(-time.Minute),
	}
	msg := renderView(t, v, domain.ModeThreadReply)
	var payload struct {
		Attachments []struct {
			Blocks []struct {
				Text struct {
					Text string `json:"text"`
				} `json:"text"`
			} `json:"blocks"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	body := payload.Attachments[0].Blocks[0].Text.Text
	command, args, why := strings.Index(body, "`k8s-write__rollout_restart`"),
		strings.Index(body, `{"namespace":"checkout","deployment":"api"}`), strings.Index(body, "Restart the api")
	if command < 0 || args < 0 || why < 0 || !(command < args && args < why) {
		t.Fatalf("the reply does not say the command, then its arguments, then the description:\n%s", body)
	}
	if !strings.Contains(body, "*Remedy approved* by Grace Hopper — 2 of 2 approvals") {
		t.Fatalf("the reply does not say who approved it and how many have:\n%s", body)
	}
	if got := topLevelText(t, msg.Payload); !strings.Contains(got, "was approved by Grace Hopper") {
		t.Errorf("the push text does not say the transition: %q", got)
	}

	v.Reason, v.Incident.Remedy.State = "remedy_proposed", "proposed"
	v.Incident.Remedy.ToolServer, v.Incident.Remedy.Tool, v.Incident.Remedy.Arguments = "", "", ""
	v.Incident.Remedy.NoTool = "no configured Tool can carry this out"
	if body := string(renderView(t, v, domain.ModeThreadReply).Payload); !strings.Contains(body, "_no configured Tool can carry this out_") {
		t.Errorf("a Remedy with no Tool does not say so:\n%s", body)
	}
}

// proposedRemedyView is a Remedy as its proposal fact carries it: no approvals yet, one
// needed, and the rule that said so.
func proposedRemedyView() *domain.NotificationView {
	v := incidentView()
	v.Reason = "remedy_proposed"
	v.Incident.Remedy = &domain.IncidentRemedyView{
		RemedyID: "019fe297-d84f-7599-b5b2-1f23174910e1", InvestigationID: "019fe297-d84f-7599-b5b2-1f23174910e2",
		State: "proposed", ToolServer: "k8s-write", Tool: "rollout_restart",
		Arguments:       `{"namespace":"checkout","deployment":"api"}`,
		ArgumentsSHA256: "0d7ac1c3d5a1d7b7f0a4c1c5e7c8a6b5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a9",
		Target:          "Deployment checkout/api", Description: "Restart the api to pick up the reverted config.",
		ProposedBy: "firstlook v3", RequiredApprovals: 1, ApprovalsSetBy: "rule", ApprovalsRule: "restart-is-reversible",
		ActorKind: "investigator", ActorLabel: "firstlook v3", At: renderedAt.Add(-time.Minute),
		ExpiresAt: renderedAt.Add(time.Hour),
	}
	return v
}

// remedyReply decodes a Remedy reply into its body text and its buttons.
type remedyButton struct {
	ActionID string `json:"action_id"`
	Value    string `json:"value"`
	Style    string `json:"style"`
	Confirm  *struct {
		Title struct {
			Text string `json:"text"`
		} `json:"title"`
		Text struct {
			Text string `json:"text"`
		} `json:"text"`
	} `json:"confirm"`
}

func remedyReply(t *testing.T, v *domain.NotificationView) (string, []remedyButton) {
	t.Helper()
	msg := renderView(t, v, domain.ModeThreadReply)
	var payload struct {
		Attachments []struct {
			Blocks []struct {
				Type string `json:"type"`
				Text struct {
					Text string `json:"text"`
				} `json:"text"`
				Elements []remedyButton `json:"elements"`
			} `json:"blocks"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	blocks := payload.Attachments[0].Blocks
	var buttons []remedyButton
	for _, b := range blocks {
		if b.Type == "actions" {
			buttons = append(buttons, b.Elements...)
		}
	}
	return blocks[0].Text.Text, buttons
}

// ⭐ THE GOLDEN OF THE ONE INCIDENT-THREAD MESSAGE WITH BUTTONS (git-bug ac9b492): the command
// first, then the target, the tier and the rule that set it, then the description — and the
// Approve and Decline row under it.
func TestGoldenARemedyProposalSaysTheCommandFirstAndOffersApproveAndDecline(t *testing.T) {
	t.Parallel()
	v := proposedRemedyView()
	msg := renderView(t, v, domain.ModeThreadReply)
	golden(t, "incident_reply_remedy_proposed.golden.json", msg.Payload)

	body, buttons := remedyReply(t, v)
	command := strings.Index(body, "`k8s-write__rollout_restart`")
	tier := strings.Index(body, "*Needs 1 approval* — set by the rule `restart-is-reversible`")
	why := strings.Index(body, "Restart the api")
	if command < 0 || tier < 0 || why < 0 || !(command < tier && tier < why) {
		t.Fatalf("the reply does not say the command, then the tier and its rule, then the description:\n%s", body)
	}
	if len(buttons) != 2 || buttons[0].ActionID != domain.ActionRemedyApprove || buttons[1].ActionID != domain.ActionRemedyDecline {
		t.Fatalf("a proposed Remedy with a Tool offers Approve then Decline, got %+v", buttons)
	}
	for _, b := range buttons {
		if b.Value != v.Incident.Remedy.RemedyID {
			t.Errorf("%s carries %q; a button's value is the Remedy's id and nothing else", b.ActionID, b.Value)
		}
	}
	if c := buttons[0].Confirm; c == nil || !strings.Contains(c.Text.Text, "k8s-write__rollout_restart") {
		t.Errorf("Approve does not ask first, naming the command: %+v", buttons[0].Confirm)
	}
	if buttons[1].Confirm != nil {
		t.Error("Decline asks for confirmation; saying no is the safe direction")
	}
}

// ADR 0054 §1: a Remedy no configured Tool can carry out cannot be approved, so its card offers
// Decline and nothing else.
func TestARemedyWithNoToolOffersNoApproveButton(t *testing.T) {
	t.Parallel()
	v := proposedRemedyView()
	r := v.Incident.Remedy
	r.ToolServer, r.Tool, r.Arguments, r.ArgumentsSHA256 = "", "", "", ""
	r.NoTool = "no configured Tool can carry this out"
	_, buttons := remedyReply(t, v)
	if len(buttons) != 1 || buttons[0].ActionID != domain.ActionRemedyDecline {
		t.Fatalf("a Remedy with no Tool offers %+v; it may only be declined", buttons)
	}
}

// An approval in Slack is an approval of what the card showed: arguments the reply had to cut
// are approved on the Remedy's page, and the card says so.
func TestARemedyWhoseArgumentsTheCardCutIsNotApprovedFromIt(t *testing.T) {
	t.Parallel()
	v := proposedRemedyView()
	v.Incident.Remedy.Arguments = `{"patch":"` + strings.Repeat("x", 2000) + `"}`
	body, buttons := remedyReply(t, v)
	if len(buttons) != 1 || buttons[0].ActionID != domain.ActionRemedyDecline {
		t.Fatalf("a Remedy whose arguments were cut offers %+v; only Decline", buttons)
	}
	if !strings.Contains(body, "so it is approved on the Remedy's page") {
		t.Errorf("the reply does not say where it is approved:\n%s", body)
	}
}

// Only the proposal offers a decision: a later transition's reply is a record, with no row.
func TestOnlyAProposalOffersButtons(t *testing.T) {
	t.Parallel()
	for _, step := range []struct{ reason, state string }{
		{"remedy_approved", "approved"}, {"remedy_declined", "declined"}, {"remedy_expired", "expired"},
		{"remedy_executed", "executed"}, {"remedy_proposed", "expired"},
	} {
		v := proposedRemedyView()
		v.Reason, v.Incident.Remedy.State = step.reason, step.state
		if _, buttons := remedyReply(t, v); len(buttons) != 0 {
			t.Errorf("%s (%s) offers %+v", step.reason, step.state, buttons)
		}
	}
}

// Who approved so far is said by name, and a tier the risk model raised says the rule's word too.
func TestARemedyReplySaysItsApprovalsSoFarAndWhatSetItsTier(t *testing.T) {
	t.Parallel()
	v := proposedRemedyView()
	v.Reason = "remedy_approved"
	r := v.Incident.Remedy
	r.State, r.RequiredApprovals, r.ApprovalsSetBy = "approved", 2, "risk_model"
	r.Approvals = []domain.IncidentRemedyApprovalView{{Label: "Ada Lovelace"}, {Label: "Grace Hopper"}}
	body, _ := remedyReply(t, v)
	for _, want := range []string{
		"*Needs 2 approvals from different people* — raised by the risk model; the rule `restart-is-reversible` said one",
		"Approved so far by Ada Lovelace, Grace Hopper",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the reply does not say %q:\n%s", want, body)
		}
	}
}
