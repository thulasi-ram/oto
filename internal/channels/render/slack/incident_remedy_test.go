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
