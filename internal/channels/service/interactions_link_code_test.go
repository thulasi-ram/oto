package service

// AN UNLINKED MEMBER'S REMEDY PRESS IS ANSWERED WITH A LINK CODE (git-bug a556a5c): bound to the
// member the VERIFIED envelope names, in the org the channel resolves to, shown only to them, and
// never shown when oto could not issue it.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

type linkCodeCall struct {
	scope        db.TenantScope
	team, member string
}

type fakeLinkCodes struct {
	code  SlackLinkCode
	err   error
	calls []linkCodeCall
}

func (f *fakeLinkCodes) IssueSlackLinkCode(_ context.Context, s db.TenantScope, team, member string) (SlackLinkCode, error) {
	f.calls = append(f.calls, linkCodeCall{s, team, member})
	return f.code, f.err
}

// The fake clock in newServiceWith reads 2026-08-09T12:00Z.
var linkCodeNow = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

func linkCodeService(t *testing.T, actors SlackActors, codes SlackLinkCodes, rem Remedies, notice SlackNotice) *InteractionService {
	t.Helper()
	return newServiceWith(t, InteractionOptions{
		Conversations: alphaConversations(), Actors: actors, Cases: &fakeCases{},
		Remedies: rem, LinkCodes: codes, Notice: notice,
	})
}

func TestAnUnlinkedPressIsAnsweredWithACodeBoundToTheVerifiedMember(t *testing.T) {
	for name, actor := range map[string]SlackActor{
		"a shadow member": {UserID: uuid.New(), Label: "@ram", Shadow: true},
		"never linked":    {},
	} {
		for _, action := range []string{ActionRemedyApprove, ActionRemedyDecline} {
			codes := &fakeLinkCodes{code: SlackLinkCode{Code: "ABCDE-FGHJK", ExpiresAt: linkCodeNow.Add(10 * time.Minute)}}
			rem, notice := &fakeRemedies{required: 1}, &fakeNotice{}
			s := linkCodeService(t, &fakeActors{actor: actor}, codes, rem, notice)
			if err := s.Apply(context.Background(), remedyArgs(action)); err != nil {
				t.Fatalf("%s %s: %v", name, action, err)
			}
			if len(rem.calls) != 0 {
				t.Fatalf("%s %s reached the Remedy: %+v", name, action, rem.calls)
			}
			if len(codes.calls) != 1 || codes.calls[0].team != "T9TK3CUKW" || codes.calls[0].member != "U0123456789" ||
				codes.calls[0].scope.OrgID() != orgAlpha {
				t.Fatalf("%s %s: issued %+v", name, action, codes.calls)
			}
			got := notice.only()
			for _, want := range []string{"`ABCDE-FGHJK`", "within 10 minutes", "Account", "credential",
				"count as theirs", "`U0123456789`", "`T9TK3CUKW`", "decide it there"} {
				if !strings.Contains(got, want) {
					t.Errorf("%s %s: the reply %q does not say %q", name, action, got, want)
				}
			}
			if strings.Contains(got, "whoever runs oto") {
				t.Errorf("the reply still sends the member to an operator: %q", got)
			}
		}
	}
}

func TestALinkedMemberIsNeverIssuedACode(t *testing.T) {
	codes := &fakeLinkCodes{code: SlackLinkCode{Code: "ABCDE-FGHJK"}}
	rem, notice := &fakeRemedies{required: 1}, &fakeNotice{}
	if err := linkCodeService(t, linked(), codes, rem, notice).Apply(context.Background(), remedyArgs(ActionRemedyApprove)); err != nil {
		t.Fatal(err)
	}
	if len(codes.calls) != 0 {
		t.Fatalf("a linked member was issued a code: %+v", codes.calls)
	}
	// ⭐ And the approval went through the same port the UI's service stands behind, as that user.
	if len(rem.calls) != 1 || rem.calls[0].userID != userRAM {
		t.Fatalf("calls = %+v", rem.calls)
	}
}

func TestAMemberOtoCannotReadIsIssuedNoCode(t *testing.T) {
	codes := &fakeLinkCodes{code: SlackLinkCode{Code: "ABCDE-FGHJK"}}
	notice := &fakeNotice{}
	actors := &fakeActors{err: errs.Validation("invalid_slack_user_id", "bad member")}
	if err := linkCodeService(t, actors, codes, &fakeRemedies{}, notice).Apply(context.Background(), remedyArgs(ActionRemedyApprove)); err != nil {
		t.Fatal(err)
	}
	if len(codes.calls) != 0 || strings.Contains(notice.only(), "ABCDE") {
		t.Fatalf("issued %+v, said %q", codes.calls, notice.sent)
	}
}

func TestACodeOtoCouldNotIssueIsNeverShown(t *testing.T) {
	for name, err := range map[string]error{
		"linked elsewhere": errs.Conflict("slack_identity_linked_elsewhere", "held"),
		"not wired":        errs.Unavailable("slack_link_unavailable", "no", 0),
		"no identity":      errs.NotFound("slack_identity_not_found", "no"),
	} {
		codes := &fakeLinkCodes{code: SlackLinkCode{Code: "ABCDE-FGHJK"}, err: err}
		notice := &fakeNotice{}
		shadow := &fakeActors{actor: SlackActor{UserID: uuid.New(), Shadow: true}}
		if aerr := linkCodeService(t, shadow, codes, &fakeRemedies{}, notice).Apply(context.Background(), remedyArgs(ActionRemedyApprove)); aerr != nil {
			t.Fatalf("%s: %v", name, aerr)
		}
		got := notice.only()
		if strings.Contains(got, "ABCDE") || !strings.Contains(got, "could not give you a link code") ||
			!strings.Contains(got, "decide it there") {
			t.Fatalf("%s: said %q", name, got)
		}
	}
	// An untyped failure is retried, and says nothing yet.
	codes := &fakeLinkCodes{err: errors.New("connection reset")}
	notice := &fakeNotice{}
	shadow := &fakeActors{actor: SlackActor{UserID: uuid.New(), Shadow: true}}
	if err := linkCodeService(t, shadow, codes, &fakeRemedies{}, notice).Apply(context.Background(), remedyArgs(ActionRemedyApprove)); err == nil {
		t.Fatal("a database failure issuing a code was swallowed")
	}
	if len(notice.sent) != 0 {
		t.Fatalf("a retried press was answered: %q", notice.sent)
	}
	// An empty answer is not a code.
	codes = &fakeLinkCodes{}
	notice = &fakeNotice{}
	if err := linkCodeService(t, shadow, codes, &fakeRemedies{}, notice).Apply(context.Background(), remedyArgs(ActionRemedyApprove)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(notice.only(), "``") {
		t.Fatalf("an empty code was shown: %q", notice.sent)
	}
}
