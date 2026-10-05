package service

// A REMEDY'S APPROVE AND DECLINE FROM SLACK (ADR 0054 §2, §4; git-bug ac9b492): a linked user
// decides through the port the UI's service stands behind; anybody else is told how to get
// linked, and nothing is written.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

var remedyOne = uuid.MustParse("019fe297-d84f-7599-b5b2-1f23174910e1")

type remedyCall struct {
	verb     string
	scope    db.TenantScope
	remedyID uuid.UUID
	userID   uuid.UUID
}

// fakeRemedies stands in for the investigator's approval and decline. It counts approvals by
// user id, as the service does, so the same person twice is refused.
type fakeRemedies struct {
	required int
	approved map[uuid.UUID]bool
	// refuse, when set, is the answer every call gets.
	refuse error
	calls  []remedyCall
}

func (f *fakeRemedies) ApproveRemedy(_ context.Context, s db.TenantScope, remedyID, userID uuid.UUID) (RemedyApproval, error) {
	f.calls = append(f.calls, remedyCall{"approve", s, remedyID, userID})
	if f.refuse != nil {
		return RemedyApproval{}, f.refuse
	}
	if f.approved == nil {
		f.approved = map[uuid.UUID]bool{}
	}
	if f.approved[userID] {
		return RemedyApproval{}, errs.Conflict("remedy_already_approved", "you approved this Remedy already")
	}
	f.approved[userID] = true
	return RemedyApproval{Approvals: len(f.approved), Required: f.required, Approved: len(f.approved) >= f.required}, nil
}

func (f *fakeRemedies) DeclineRemedy(_ context.Context, s db.TenantScope, remedyID, userID uuid.UUID) error {
	f.calls = append(f.calls, remedyCall{"decline", s, remedyID, userID})
	return f.refuse
}

func remedyArgs(action string) jobs.SlackInteractionArgs {
	a := ackArgs()
	a.ActionID, a.Value = action, remedyOne.String()
	return a
}

func remedyService(t *testing.T, actors SlackActors, remedies Remedies, notice SlackNotice) *InteractionService {
	t.Helper()
	return newServiceWith(t, InteractionOptions{
		Conversations: alphaConversations(), Actors: actors, Cases: &fakeCases{},
		Remedies: remedies, Notice: notice,
	})
}

func linked() *fakeActors {
	return &fakeActors{actor: SlackActor{UserID: userRAM, Label: "ram@example.com"}}
}

// The two buttons are routed like the four writing actions: one job each, carrying the member
// the VERIFIED envelope names.
func TestHandleEnqueuesARemedyPressWithTheMemberTheEnvelopeNames(t *testing.T) {
	for _, action := range []string{ActionRemedyApprove, ActionRemedyDecline} {
		enq := &fakeEnqueuer{}
		s := newService(t, alphaConversations(), &fakeActors{}, &fakeCases{}, enq, &fakeNotice{})
		if err := s.Handle(context.Background(), envelope(action, remedyOne.String())); err != nil {
			t.Fatalf("%s: Handle: %v", action, err)
		}
		if len(enq.reqs) != 1 {
			t.Fatalf("%s enqueued %d jobs, want 1", action, len(enq.reqs))
		}
		got := enq.reqs[0].Args.(jobs.SlackInteractionArgs)
		if got.ActionID != action || got.Value != remedyOne.String() || got.SlackUserID != "U0123456789" ||
			got.TeamID != "T9TK3CUKW" {
			t.Fatalf("%s enqueued %+v", action, got)
		}
	}
}

func TestALinkedHolderApprovesAndDeclinesFromSlack(t *testing.T) {
	t.Run("an approval that completes the Remedy says nothing; its transition is the reply", func(t *testing.T) {
		rem, notice := &fakeRemedies{required: 1}, &fakeNotice{}
		s := remedyService(t, linked(), rem, notice)
		if err := s.Apply(context.Background(), remedyArgs(ActionRemedyApprove)); err != nil {
			t.Fatal(err)
		}
		if len(rem.calls) != 1 || rem.calls[0].verb != "approve" || rem.calls[0].userID != userRAM ||
			rem.calls[0].remedyID != remedyOne || rem.calls[0].scope.OrgID() != orgAlpha {
			t.Fatalf("calls = %+v", rem.calls)
		}
		if len(notice.sent) != 0 {
			t.Fatalf("a completed approval was answered with %q", notice.sent)
		}
	})
	t.Run("an approval that leaves it waiting tells the approver where it stands", func(t *testing.T) {
		rem, notice := &fakeRemedies{required: 2}, &fakeNotice{}
		s := remedyService(t, linked(), rem, notice)
		if err := s.Apply(context.Background(), remedyArgs(ActionRemedyApprove)); err != nil {
			t.Fatal(err)
		}
		if got := notice.only(); !strings.Contains(got, "1 of 2") || !strings.Contains(got, "1 more holder") {
			t.Fatalf("the approver was told %q", got)
		}
	})
	t.Run("a decline", func(t *testing.T) {
		rem, notice := &fakeRemedies{}, &fakeNotice{}
		s := remedyService(t, linked(), rem, notice)
		if err := s.Apply(context.Background(), remedyArgs(ActionRemedyDecline)); err != nil {
			t.Fatal(err)
		}
		if len(rem.calls) != 1 || rem.calls[0].verb != "decline" || rem.calls[0].userID != userRAM {
			t.Fatalf("calls = %+v", rem.calls)
		}
		if len(notice.sent) != 0 {
			t.Fatalf("a decline was answered with %q", notice.sent)
		}
	})
}

// ⛔ AN UNLINKED MEMBER IS REFUSED WITH HOW TO GET LINKED, AND NOTHING IS WRITTEN — whether oto
// has never linked them or linked them only to the shadow a press mints.
func TestAnUnlinkedSlackMemberIsRefusedWithHowToLink(t *testing.T) {
	for name, actor := range map[string]SlackActor{
		"never linked":         {},
		"a shadow member":      {UserID: uuid.New(), Label: "@ram", Shadow: true},
		"a link with no label": {UserID: uuid.Nil, Label: "ram@example.com"},
	} {
		for _, action := range []string{ActionRemedyApprove, ActionRemedyDecline} {
			rem, notice := &fakeRemedies{required: 1}, &fakeNotice{}
			s := remedyService(t, &fakeActors{actor: actor}, rem, notice)
			if err := s.Apply(context.Background(), remedyArgs(action)); err != nil {
				t.Fatalf("%s %s: %v", name, action, err)
			}
			if len(rem.calls) != 0 {
				t.Fatalf("%s %s reached the Remedy: %+v", name, action, rem.calls)
			}
			got := notice.only()
			for _, want := range []string{"not linked", "`U0123456789`", "`T9TK3CUKW`", "decide it there"} {
				if !strings.Contains(got, want) {
					t.Errorf("%s %s: the reply %q does not say %q", name, action, got, want)
				}
			}
		}
	}
	// No actor port at all is nobody linked.
	rem, notice := &fakeRemedies{required: 1}, &fakeNotice{}
	if err := remedyService(t, nil, rem, notice).Apply(context.Background(), remedyArgs(ActionRemedyApprove)); err != nil {
		t.Fatal(err)
	}
	if len(rem.calls) != 0 || !strings.Contains(notice.only(), "not linked") {
		t.Fatalf("with no actor port: calls %+v, said %q", rem.calls, notice.sent)
	}
}

// A linked member who does not hold the grant is refused by the service — the same 403 the UI
// gets — and told where a grant comes from.
func TestALinkedMemberWithoutTheGrantIsRefused(t *testing.T) {
	rem := &fakeRemedies{refuse: errs.New(errs.KindForbidden, "remedy_approver_required", "needs the grant")}
	notice := &fakeNotice{}
	if err := remedyService(t, linked(), rem, notice).Apply(context.Background(), remedyArgs(ActionRemedyApprove)); err != nil {
		t.Fatalf("a refusal is an outcome, not a retry: %v", err)
	}
	if got := notice.only(); !strings.Contains(got, "do not hold the approval grant") ||
		!strings.Contains(got, "oto grant remedy-approver") {
		t.Fatalf("the member was told %q", got)
	}
}

// One person approving twice — here twice from Slack; once in the UI and once in Slack is the
// same user id at the same service — counts once, and is told so.
func TestTheSamePersonApprovingTwiceCountsOnce(t *testing.T) {
	rem, notice := &fakeRemedies{required: 2}, &fakeNotice{}
	s := remedyService(t, linked(), rem, notice)
	for range 2 {
		if err := s.Apply(context.Background(), remedyArgs(ActionRemedyApprove)); err != nil {
			t.Fatal(err)
		}
	}
	if len(rem.approved) != 1 {
		t.Fatalf("%d approvals counted for one person", len(rem.approved))
	}
	if len(notice.sent) != 2 || !strings.Contains(notice.sent[1], "one person counts once") {
		t.Fatalf("the second press was answered with %q", notice.sent)
	}
}

// What is refused is told, and what is broken is retried.
func TestARemedyPressRetriesOnlyWhatIsWorthRetrying(t *testing.T) {
	boom := errors.New("connection refused")
	if err := remedyService(t, linked(), &fakeRemedies{refuse: boom}, &fakeNotice{}).
		Apply(context.Background(), remedyArgs(ActionRemedyApprove)); !errors.Is(err, boom) {
		t.Fatalf("a failed approval returned %v; it must be retried", err)
	}
	rem := &fakeRemedies{required: 1}
	if err := remedyService(t, &fakeActors{err: boom}, rem, &fakeNotice{}).
		Apply(context.Background(), remedyArgs(ActionRemedyApprove)); !errors.Is(err, boom) || len(rem.calls) != 0 {
		t.Fatalf("a failed directory read returned %v and made %d calls; an approval needs a person", err, len(rem.calls))
	}

	for code, want := range map[string]string{
		"remedy_has_no_tool":    "no configured Tool can carry it out",
		"remedy_not_found":      "can no longer find that Remedy",
		"slack_member_disabled": "is disabled",
		"remedy_expired":        "This Remedy expired at",
	} {
		notice := &fakeNotice{}
		refusal := errs.Conflict(code, "this Remedy expired at 2026-10-05T10:00:00Z and can no longer be decided")
		if code == "remedy_not_found" {
			refusal = errs.NotFound(code, "no such Remedy")
		}
		if err := remedyService(t, linked(), &fakeRemedies{refuse: refusal}, notice).
			Apply(context.Background(), remedyArgs(ActionRemedyDecline)); err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		if got := notice.only(); !strings.Contains(got, want) {
			t.Errorf("%s was answered with %q, want it to say %q", code, got, want)
		}
	}
}

// A value that is not an id, and a deployment with no Remedy port, are answered — never silence.
func TestARemedyPressThatCannotBeReadIsAnswered(t *testing.T) {
	a := remedyArgs(ActionRemedyApprove)
	a.Value = "not-a-remedy"
	notice := &fakeNotice{}
	if err := remedyService(t, linked(), &fakeRemedies{}, notice).Apply(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notice.only(), "could not read that button") {
		t.Fatalf("said %q", notice.sent)
	}
	notice = &fakeNotice{}
	if err := remedyService(t, linked(), nil, notice).Apply(context.Background(), remedyArgs(ActionRemedyDecline)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notice.only(), "in this deployment yet") {
		t.Fatalf("said %q", notice.sent)
	}
}
