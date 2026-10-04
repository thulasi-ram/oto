package slack_test

import (
	"strings"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// ------------------------------------------------------------------- the Incident

// ADR 0052 §6 — AN INCIDENT MAY BE A CONVERSATION (git-bug bf5fc7e).
//
// The renderer used to REFUSE an Incident view outright, because no threaded
// channel could carry one. A Correlator may now say its Incidents are
// conversations, and three messages exist that did not: the Incident's card (the
// root of its thread, whichever fact posts or amends it), an Incident fact as a
// reply under it, and the pointer posted into a member Case's own thread. A Case
// fact replying in the Incident's thread is the Case reply with the Case named in
// front. Each has a golden, so the next change to any of them is a diff rather than
// a discovery.

const incidentCaseLink = "http://localhost:8080/cases/019fe297-d84f-7599-b5b2-1f23174910c2"

// incidentView is a Correlator-drawn Incident over three spells: one Case open, one
// closed, and one a human took out — which the card must not list as a member.
func incidentView() *domain.NotificationView {
	drawn := upstreamStart
	return &domain.NotificationView{
		Reason: "drawn",
		Incident: &domain.IncidentView{
			ID: "019fe297-d84f-7599-b5b2-1f23174910c1", Number: 12, State: "active",
			DrawnAt: drawn,
			DrawnBy: domain.IncidentAuthorView{CorrelatorID: "019fe297-d84f-7599-b5b2-1f23174910c9"},
			Link:    "http://localhost:8080/incidents/12",
			Members: []domain.IncidentMemberView{
				{
					CaseID: "019fe297-d84f-7599-b5b2-1f23174910c2", CaseNumber: 412, CaseState: "open",
					AlertID: "019fe297-d84f-7599-b5b2-1f23174910a1", AlertName: "OtoSmokeTest",
					AddedAt: drawn, AddedBy: domain.IncidentAuthorView{CorrelatorID: "019fe297-d84f-7599-b5b2-1f23174910c9"},
					Link: incidentCaseLink,
				},
				{
					CaseID: "019fe297-d84f-7599-b5b2-1f23174910c3", CaseNumber: 413, CaseState: "closed",
					AlertID: "019fe297-d84f-7599-b5b2-1f23174910a2", AlertName: "OtoSmokeTestLatency",
					AddedAt: drawn.Add(time.Minute), AddedBy: domain.IncidentAuthorView{Label: "Priya R."},
					Link: "http://localhost:8080/cases/019fe297-d84f-7599-b5b2-1f23174910c3",
				},
				{
					CaseID: "019fe297-d84f-7599-b5b2-1f23174910c4", CaseNumber: 409, CaseState: "closed",
					AlertID: "019fe297-d84f-7599-b5b2-1f23174910a3", AlertName: "OtoSmokeTest",
					AddedAt: drawn, AddedBy: domain.IncidentAuthorView{CorrelatorID: "019fe297-d84f-7599-b5b2-1f23174910c9"},
					RemovedAt: drawn.Add(2 * time.Minute), RemovedByLabel: "Priya R.",
					Link: "http://localhost:8080/cases/019fe297-d84f-7599-b5b2-1f23174910c4",
				},
			},
		},
		RenderedAt: renderedAt,
	}
}

func TestGoldenIncidentRootCard(t *testing.T) {
	t.Parallel()
	msg := renderView(t, incidentView(), domain.ModePostRoot)
	golden(t, "incident_root.golden.json", msg.Payload)
}

func TestGoldenIncidentFactReply(t *testing.T) {
	t.Parallel()
	v := incidentView()
	v.Reason = "case_added"
	msg := renderView(t, v, domain.ModeThreadReply)
	golden(t, "incident_reply_case_added.golden.json", msg.Payload)
}

func TestGoldenIncidentPointerInACasesOwnThread(t *testing.T) {
	t.Parallel()
	v := incidentView()
	v.Reason = "case_added"
	v.Incident.PointsFrom = "019fe297-d84f-7599-b5b2-1f23174910c2"
	msg := renderView(t, v, domain.ModeThreadReply)
	golden(t, "incident_pointer.golden.json", msg.Payload)
}

func TestGoldenACaseReplyInAnIncidentsThread(t *testing.T) {
	t.Parallel()
	v := resolvedView()
	v.InIncident = &domain.InIncidentView{
		Number: 12, CaseNumber: 412,
	}
	msg := renderView(t, v, domain.ModeThreadReply)
	golden(t, "reply_all_resolved_in_incident.golden.json", msg.Payload)
}

// TestAnIncidentCardListsItsCurrentMembersAndOffersNothingToPress — the members
// are the story's current Cases, each linked to its own; a removed one is history
// the Incident's page keeps; and there is no button, because every action acts on a
// signal and the card is about the story.
func TestAnIncidentCardListsItsCurrentMembersAndOffersNothingToPress(t *testing.T) {
	t.Parallel()
	msg := renderView(t, incidentView(), domain.ModePostRoot)
	body := string(msg.Payload)

	for _, want := range []string{"Case #412", "Case #413", "<" + incidentCaseLink + "|Case #412>", "Incident #12"} {
		if !strings.Contains(body, want) {
			t.Errorf("the card does not carry %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Case #409") {
		t.Errorf("a Case a human removed is listed as a member:\n%s", body)
	}
	if strings.Contains(body, `"type":"actions"`) {
		t.Errorf("the Incident card offers an action; every action acts on ONE signal:\n%s", body)
	}
	if got := fieldValue(t, msg.Payload, "Open"); got != "1" {
		t.Errorf("Open = %q, want 1", got)
	}
	if got := decodeCard(t, msg.Payload).Attachments[0].Color; got != "#a30200" {
		t.Errorf("an active Incident's bar = %q, want the firing colour", got)
	}
	if strings.Contains(topLevelText(t, msg.Payload), "<!date") {
		t.Error("a <!date> token in the top-level text does not render in a push (S5)")
	}
}

// TestAQuietIncidentIsNotDrawnAsResolved — quiet is every member Case closed, and
// whether the response is over is the incident tool's fact (ADR 0052 §3). The bar is
// the neutral one, not the resolved green, and a zero is not a field (S11).
func TestAQuietIncidentIsNotDrawnAsResolved(t *testing.T) {
	t.Parallel()
	v := incidentView()
	v.Incident.State = "quiet"
	v.Incident.Members[0].CaseState = "closed"

	msg := renderView(t, v, domain.ModeUpdateRoot)
	if got := decodeCard(t, msg.Payload).Attachments[0].Color; got == "#2eb886" {
		t.Error("a quiet Incident wears the resolved green; quiet is not resolved")
	}
	if got := fieldValue(t, msg.Payload, "Open"); got != "" {
		t.Errorf("a quiet Incident renders Open = %q; S11 suppresses a zero", got)
	}
	if !strings.Contains(topLevelText(t, msg.Payload), "is quiet") {
		t.Errorf("the push text does not say quiet: %q", topLevelText(t, msg.Payload))
	}
	// The head says quiet with the neutral circle, never the resolved tick — and a
	// closed member Case keeps its tick, because that Case did close.
	const quietMark, resolvedMark = ":white_circle: *Quiet*", ":white_check_mark: *Quiet*"
	card := string(msg.Payload)
	if !strings.Contains(card, quietMark) || strings.Contains(card, resolvedMark) {
		t.Errorf("the quiet head is not drawn with the neutral circle:\n%s", card)
	}
	if !strings.Contains(card, ":white_check_mark: ") {
		t.Errorf("the closed member Case lost its resolved tick:\n%s", card)
	}

	reply := *v
	reply.Reason = "quiet"
	body := string(renderView(t, &reply, domain.ModeThreadReply).Payload)
	if !strings.Contains(body, quietMark) || strings.Contains(body, resolvedMark) {
		t.Errorf("the quiet fact in the thread is not drawn with the neutral circle:\n%s", body)
	}
}

// TestAnIncidentIsDrawnAsWhatTheModeAndTheThreadSay — a root mode is the card, a
// reply in the Incident's thread is a one-section fact, and the pointer is the
// pointer in any mode, because a pointer that a lost root turned into `post_root`
// is still in the Case's thread.
func TestAnIncidentIsDrawnAsWhatTheModeAndTheThreadSay(t *testing.T) {
	t.Parallel()
	for _, mode := range []domain.Mode{domain.ModePostRoot, domain.ModeUpdateRoot} {
		if body := string(renderView(t, incidentView(), mode).Payload); !strings.Contains(body, "oto_incidentfacts_") {
			t.Errorf("%s did not draw the Incident's card:\n%s", mode, body)
		}
	}
	reply := incidentView()
	reply.Reason = "quiet"
	if body := string(renderView(t, reply, domain.ModeThreadReply).Payload); !strings.Contains(body, "oto_incidentreply_") {
		t.Errorf("a thread reply did not draw the fact:\n%s", body)
	}
	for _, mode := range []domain.Mode{domain.ModeThreadReply, domain.ModePostRoot} {
		v := incidentView()
		v.Incident.PointsFrom = "019fe297-d84f-7599-b5b2-1f23174910c2"
		if body := string(renderView(t, v, mode).Payload); !strings.Contains(body, "oto_incidentpointer_") {
			t.Errorf("the pointer in %s was not drawn as the pointer:\n%s", mode, body)
		}
	}
}

// TestAReplyInAnIncidentsThreadNamesItsCase — forty Cases share that thread, so the
// reply says which one it is about, in the body and in the push text. Outside it,
// nothing changes: the Case's own thread IS the Case.
func TestAReplyInAnIncidentsThreadNamesItsCase(t *testing.T) {
	t.Parallel()
	v := resolvedView()
	v.InIncident = &domain.InIncidentView{Number: 12, CaseNumber: 412}
	msg := renderView(t, v, domain.ModeThreadReply)
	if !strings.Contains(string(msg.Payload), "|Case #412>*") {
		t.Errorf("the reply body does not name its Case:\n%s", msg.Payload)
	}
	if !strings.Contains(topLevelText(t, msg.Payload), "(case #412 in Incident #12)") {
		t.Errorf("the push text does not name its Case: %q", topLevelText(t, msg.Payload))
	}

	own := string(renderView(t, resolvedView(), domain.ModeThreadReply).Payload)
	if strings.Contains(own, "Incident #") {
		t.Errorf("a reply in the Case's own thread names an Incident:\n%s", own)
	}
}

// TestTheIncidentCardLinksTheExternalIncident is ADR 0052 §5's outbound mapping on
// the card (git-bug 506ff21): an incident tool that echoed its own incident gets a
// link in the context line beside oto's, labelled with the tool's id — and an id
// with no link is still shown, because it is what a reader searches the tool for.
// The card's state is not read off it: an Incident with an external link is still
// active while a member Case is open.
func TestTheIncidentCardLinksTheExternalIncident(t *testing.T) {
	t.Parallel()
	v := incidentView()
	v.Incident.External = []domain.IncidentExternalView{
		{Destination: "incident-tool", URL: "https://tool.example/incidents/42", ID: "INC-42"},
		{Destination: "bridge", ID: "B-7"},
	}
	payload := string(renderView(t, v, domain.ModePostRoot).Payload)

	for _, want := range []string{
		":link: <https://tool.example/incidents/42|INC-42>",
		":link: `B-7`",
		"*Active*",
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("the Incident card does not carry %q:\n%s", want, payload)
		}
	}

	// A tool that echoed nothing leaves the card byte-identical to its golden.
	bare := renderView(t, incidentView(), domain.ModePostRoot)
	if strings.Contains(string(bare.Payload), ":link:") {
		t.Errorf("a card with no external incident grew a link:\n%s", bare.Payload)
	}
}

// TestAnIncidentsFindingIsDrawnAsWhatWasSeenAtT — ADR 0053 §4 (git-bug 74ea849): the
// latest Finding on an Incident is on its card, and the `finding` fact is a reply that
// carries it. Every time it says the Finding it says WHO concluded it and WHEN, and a
// budget-cut one says "partial" first. A card with no Finding draws no Finding block.
func TestAnIncidentsFindingIsDrawnAsWhatWasSeenAtT(t *testing.T) {
	t.Parallel()
	if body := string(renderView(t, incidentView(), domain.ModePostRoot).Payload); strings.Contains(body, "oto_incidentfinding_") {
		t.Fatalf("an Incident with no Finding drew a Finding block:\n%s", body)
	}

	v := incidentView()
	v.Incident.Finding = &domain.IncidentFindingView{
		InvestigationID: "019fe297-d84f-7599-b5b2-1f23174910f1", Investigator: "firstlook", Version: 2,
		Summary:     "The deploy at 09:00 doubled the error rate.",
		ConcludedAt: renderedAt.Add(-time.Minute),
	}
	root := string(renderView(t, v, domain.ModePostRoot).Payload)
	for _, want := range []string{"oto_incidentfinding_", "*Finding* by `firstlook v2`", "as seen at", ">The deploy at 09:00 doubled the error rate."} {
		if !strings.Contains(root, want) {
			t.Errorf("the card does not carry %q:\n%s", want, root)
		}
	}

	v.Reason = "finding"
	v.Incident.Finding.Partial = true
	msg := renderView(t, v, domain.ModeThreadReply)
	reply := string(msg.Payload)
	if !strings.Contains(reply, "oto_incidentreply_") || !strings.Contains(reply, "*Partial Finding* by `firstlook v2`") {
		t.Errorf("the finding fact is not a reply carrying the partial Finding:\n%s", reply)
	}
	if got := topLevelText(t, msg.Payload); !strings.Contains(got, "has a new Finding by firstlook v2") {
		t.Errorf("the push text does not say who concluded it: %q", got)
	}
}
