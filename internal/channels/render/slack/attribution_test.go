package slack_test

import (
	"strings"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// ⛔⛔ WHO DID IT, AND WHAT THEY SAID.
//
// git-bug 56a9951: nothing in production ever wrote `NotificationView.Actor` or
// `.Comment`, so every attribution the renderer knows how to draw came out
// empty. `Acknowledged` named nobody, `Un-acknowledged` named nobody, and a
// `comment` — a notification that exists ONLY because a human typed something —
// rendered as a speech balloon with nothing after it.
//
// The renderer was never the bug, which is exactly why these tests exist now:
// the moment the view carries the facts, the rendering rules have to be RIGHT,
// and three of them were not.
//
//  1. an empty attribution silently dropped the clause, so "a machine did this"
//     and "oto has no idea who did this" rendered identically;
//  2. every attribution read `v.Actor` — the actor of the fact being ANNOUNCED —
//     including the Acknowledged field on a card announcing something else, which
//     credits one person's action to whoever spoke last;
//  3. a comment with no text rendered as an emoji rather than as a sentence.

const (
	slackMember  = "U0123456789"
	slackMention = "<@" + slackMember + ">"
	ackerLabel   = "ram@example.com"
)

var attributionAt = time.Date(2026, 8, 7, 17, 59, 9, 0, time.UTC)

// humanVerbView is one acknowledged, still-firing group — the card every human
// verb lands on.
func humanVerbView(reason string) *domain.NotificationView {
	return &domain.NotificationView{
		Org:    domain.OrgRef{ID: "o1", Slug: "acme", Name: "Acme"},
		Reason: reason,
		Group: domain.GroupView{
			ID: "g1", GroupKey: "gk_x", Generation: 1,
			Title: "CheckoutErrorRateHigh", Severity: "critical", State: "open",
			GroupLabels:    map[string]string{"alertname": "CheckoutErrorRateHigh"},
			FiringCount:    1,
			TotalCount:     1,
			AckedCount:     1,
			ClusterKey:     "eu-west-1",
			StartedAt:      attributionAt.Add(-10 * time.Minute),
			FirstSeenAt:    attributionAt.Add(-10 * time.Minute),
			LastActivityAt: attributionAt,
		},
		Occurrence: &domain.OccurrenceView{
			ID: "occ1", Seq: 1, State: "firing", AckState: "acked",
			StartedAt:    attributionAt.Add(-10 * time.Minute),
			AckedAt:      &attributionAt,
			AckedByLabel: ackerLabel,
		},
		Links:      domain.Links{Group: "https://oto.example.com/groups/g1"},
		RenderedAt: attributionAt.Add(time.Second),
	}
}

// slackActor is a human who acted through a Slack interaction: the kind the view
// carries for `alert_events.actor_kind = 'slack'`, whose id is a member id and
// therefore renders as a real mention.
func slackActor() *domain.ActorView {
	return &domain.ActorView{Kind: "slack_user", ID: slackMember, Label: "ram"}
}

// A HUMAN CAUSED IT: the card says so, in the thread and on the root, in the same
// words. The two disagreeing is what made the defect easy to miss — the root
// card's Acknowledged field named the acker while its own status line did not.
func TestAnAcknowledgementNamesTheHumanWhoTookIt(t *testing.T) {
	t.Parallel()

	v := humanVerbView("acked")
	v.Actor = slackActor()

	reply := string(renderView(t, v, domain.ModeThreadReply).Payload)
	if !strings.Contains(reply, "*Acknowledged* by "+slackMention) {
		t.Fatalf("the thread reply does not name the acker: %s", reply)
	}

	root := renderView(t, v, domain.ModeUpdateRoot)
	if status := fieldValue(t, root.Payload, "Status"); !strings.Contains(status, "by "+slackMention) {
		t.Fatalf("the root card's status line does not name the acker: %q", status)
	}
}

// ⛔ A SYSTEM TRANSITION IS NOT AN UNKNOWN ONE. T10 drops an acknowledgement when
// a new episode opens, and `autoUnackEvent` records that with actor_kind=system
// and no label at all. The card must say a machine did it rather than trailing
// off, because "somebody took their ack back" and "it fired again so your receipt
// no longer applies" are different things to be woken up by.
func TestASystemTransitionSaysItWasAutomaticInsteadOfNamingNobody(t *testing.T) {
	t.Parallel()

	v := humanVerbView("unacked")
	v.Group.AckedCount = 0
	v.Occurrence.AckState = "unacked"
	v.Occurrence.AckedAt = nil
	v.Occurrence.AckedByLabel = ""
	v.Occurrence.ReopenCount = 1
	// No id and no label: only a human actor is guaranteed either (ev_actor_ck).
	v.Actor = &domain.ActorView{Kind: "system"}

	body := string(renderView(t, v, domain.ModeThreadReply).Payload)

	if !strings.Contains(body, "*Un-acknowledged* automatically") {
		t.Fatalf("a system un-acknowledgement does not say it was automatic: %s", body)
	}
	if strings.Contains(body, "*Un-acknowledged* by") {
		t.Fatalf("a machine actor was rendered as a person: %s", body)
	}
}

// oto knowing NOTHING about the cause is a third answer, and it must not be
// dressed up as either of the other two.
func TestAnUnknownCauseIsLeftUnattributedRatherThanInvented(t *testing.T) {
	t.Parallel()

	v := humanVerbView("unacked")
	v.Group.AckedCount = 0
	v.Occurrence.AckState = "unacked"
	v.Occurrence.AckedAt = nil
	v.Occurrence.AckedByLabel = ""

	body := string(renderView(t, v, domain.ModeThreadReply).Payload)

	if !strings.Contains(body, "*Un-acknowledged*") {
		t.Fatalf("the reply lost its sentence: %s", body)
	}
	if strings.Contains(body, "automatically") || strings.Contains(body, "*Un-acknowledged* by") {
		t.Fatalf("an absent actor was rendered as an agent: %s", body)
	}
}

// ⛔ THE COMMENT IS THE MESSAGE. CONTEXT.md §6: a human's words live nowhere else
// once the timeline is gone, and this reply is the copy the people who were
// talking actually read.
func TestACommentReplyCarriesTheWordsSomebodyTyped(t *testing.T) {
	t.Parallel()

	v := humanVerbView("comment")
	v.Actor = slackActor()
	v.Comment = "Provider says their ETA is 20 minutes; not paging further."

	body := string(renderView(t, v, domain.ModeThreadReply).Payload)

	if !strings.Contains(body, "Provider says their ETA is 20 minutes") {
		t.Fatalf("the comment reply does not carry the comment: %s", body)
	}
	if !strings.Contains(body, ":speech_balloon: "+slackMention+": ") {
		t.Fatalf("the comment reply does not say who wrote it: %s", body)
	}
}

// A balloon with nothing after it is the failure this ticket is named for. If the
// text really is missing, the card has to SAY a comment was added and point at
// the one place it still exists.
func TestACommentWithNoTextSaysSoRatherThanRenderingABareEmoji(t *testing.T) {
	t.Parallel()

	v := humanVerbView("comment")
	v.Actor = slackActor()

	body := string(renderView(t, v, domain.ModeThreadReply).Payload)

	if !strings.Contains(body, "*A comment was added*") {
		t.Fatalf("an empty comment renders as an emoji and nothing else: %s", body)
	}
	if !strings.Contains(body, v.Links.Group) {
		t.Fatalf("an empty comment does not point at where the text lives: %s", body)
	}
}

// ⛔ THE ACTOR IS THE ACTOR OF THE FACT BEING ANNOUNCED, AND NOTHING ELSE. A
// resolved card amended by a comment carries the COMMENTER — and the receipt's
// Acknowledged field is about the acknowledgement, which somebody else took.
// Reading one for the other puts a name against an action that person never did.
func TestACommentDoesNotCreditTheAcknowledgementToWhoeverSpokeLast(t *testing.T) {
	t.Parallel()

	v := humanVerbView("comment")
	v.Comment = "closing the loop for the record"
	v.Actor = &domain.ActorView{Kind: "slack_user", ID: "U09ZZZZZZZZ", Label: "someone-else"}
	v.Group.FiringCount = 0
	v.Group.ResolvedCount = 1
	v.Occurrence.State = "resolved"
	v.Occurrence.EndedAt = &attributionAt

	acknowledged := fieldValue(t, renderView(t, v, domain.ModeUpdateRoot).Payload, "Acknowledged")

	if !strings.Contains(acknowledged, ackerLabel) {
		t.Fatalf("the receipt does not name the human who acknowledged: %q", acknowledged)
	}
	if strings.Contains(acknowledged, "U09ZZZZZZZZ") {
		t.Fatalf("the receipt credits the acknowledgement to the commenter: %q", acknowledged)
	}
}

// ⛔ AND IT DOES NOT CREDIT A MACHINE EITHER. The status line's impersonal branch
// used to be decided by `v.Actor` — the actor of the ANNOUNCED fact — while the
// name it was standing in for came from the ACK. A group with one silenced member
// and the rest firing-and-acked still derives an Acknowledged card; a comment on
// the silenced member makes that episode the focus, so its `AckedByLabel` is
// empty and the commenter is `v.Actor`. The card then read "Acknowledged
// automatically" about an acknowledgement humans took, because somebody typed a
// message. Saying nothing is the honest third answer: oto does not know.
func TestACommentDoesNotTurnAHumanAcknowledgementIntoAnAutomaticOne(t *testing.T) {
	t.Parallel()

	v := humanVerbView("comment")
	v.Comment = "silencing the payments one; the rest are already acked"
	v.Actor = &domain.ActorView{Kind: "slack_user", ID: "U09ZZZZZZZZ", Label: "someone-else"}
	v.Group.FiringCount = 2
	v.Group.AckedCount = 2
	v.Group.SuppressedCount = 1
	v.Group.TotalCount = 3
	// The focus is the SILENCED member, which nobody acknowledged.
	v.Occurrence.State = "suppressed"
	v.Occurrence.AckState = "unacked"
	v.Occurrence.AckedAt = nil
	v.Occurrence.AckedByLabel = ""

	status := fieldValue(t, renderView(t, v, domain.ModeUpdateRoot).Payload, "Status")

	// `Acked` is the status line's own word for this state; the prose above says
	// "Acknowledged" because that is what the card means, not what it prints.
	if !strings.Contains(status, "Acked") {
		t.Fatalf("the card lost its state label: %q", status)
	}
	if strings.Contains(status, "automatically") {
		t.Fatalf("a human acknowledgement was credited to a machine because somebody "+
			"commented: %q", status)
	}
	if strings.Contains(status, " by ") {
		t.Fatalf("the status line named somebody for an acknowledgement it has no "+
			"attribution for: %q", status)
	}
}

// oto has no write path into the cluster (R3) and only the reconciler can
// produce a suppression, so a silence oto did not record a human for was made
// upstream — never by oto, and never by nobody.
func TestASilenceIsAttributedUpstreamWhenNoHumanIsNamed(t *testing.T) {
	t.Parallel()

	v := humanVerbView("suppressed")
	v.Group.FiringCount = 0
	v.Group.AckedCount = 0
	v.Group.SuppressedCount = 1
	v.Occurrence.State = "suppressed"
	v.Occurrence.AckState = "unacked"
	v.Occurrence.AckedAt = nil
	v.Occurrence.AckedByLabel = ""
	v.Actor = &domain.ActorView{Kind: "reconciler"}

	body := string(renderView(t, v, domain.ModeThreadReply).Payload)

	if !strings.Contains(body, "*Silenced* upstream") {
		t.Fatalf("a silence nobody is named for is not attributed upstream: %s", body)
	}
}
