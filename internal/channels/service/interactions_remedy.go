package service

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// A REMEDY IS APPROVED OR DECLINED FROM ITS SLACK CARD (ADR 0054 §2, §4; git-bug ac9b492).
//
// ⭐⭐ THE SAME APPROVAL THE UI MAKES, BY THE SAME PERSON. A press is applied as the oto user
// the Slack member is LINKED to, through `investigator/service.ApproveRemedy` /
// `DeclineRemedy` — the calls `POST /remedies/{id}/approve|decline` make — so the grant on the
// Remedy's ToolServer (`RequireRemedyApprover`), its window, its Tool's availability and the
// count of DIFFERENT approvers are all that service's, and one person approving in Slack and
// again in the UI is one user id approving twice: refused, and counted once.
//
// ⛔ AN UNLINKED MEMBER IS REFUSED, AND THIS IS THE ONE PLACE ON THIS SURFACE THAT REFUSES ONE.
// An acknowledgement records what a Slack member saw and stands on its own; an approval is a
// grant holder's, and a grant belongs to an oto user. A member oto has only ever seen press a
// button — no link, or a link to the SHADOW member a press mints (migration 00074) — names no
// one who can hold a grant, so the press is answered with a one-time LINK CODE (git-bug a556a5c)
// the member enters on their own Account page in oto, and nothing is written. A shadow is refused
// for decline too: "linked" means a person who gave oto an address.
//
// ⛔ WHO PRESSED COMES FROM THE VERIFIED ENVELOPE AND NOWHERE ELSE. `Handle` copies `user.id`
// out of the payload whose HMAC the transport checked; the button's value is the Remedy's id
// and is resolved under the tenant the CHANNEL resolves to, so another org's Remedy is a 404.
//
// ⭐ A PARTIAL APPROVAL IS TOLD TO THE APPROVER, AND NOTHING ELSE IS. The card is the feedback
// everywhere else on this surface, and a completed approval or a decline is a transition, which
// goes outbound like every other and posts its own reply. An approval that leaves the Remedy
// waiting for a second, different person moves no state and sends no fact, so the person who
// gave it would otherwise hear nothing.

// Remedies is a Remedy's two decisions, made by one oto user. A PORT DECLARED BY THE CONSUMER,
// in primitives: `channels` may not learn `investigator`'s types (§I.1).
type Remedies interface {
	// ApproveRemedy approves one Remedy as this user, through the approval the UI makes. Its
	// refusals are typed errors — `remedy_approver_required` (403) for a user without the grant,
	// `remedy_already_approved` for a second approval by the same user, and the rest of the
	// approval's Conflicts — and a Remedy this tenant does not have is NotFound.
	//
	// ⭐ THE LABEL IS THE DIRECTORY'S, NOT THIS SURFACE'S. The adapter names the user as the UI
	// names a signed-in one, so an approval reads the same on the Remedy whichever surface gave
	// it; and a user it finds disabled is refused (`slack_member_disabled`).
	ApproveRemedy(ctx context.Context, s db.TenantScope, remedyID, userID uuid.UUID) (RemedyApproval, error)
	// DeclineRemedy declines one Remedy as this user, through the decline the UI makes.
	DeclineRemedy(ctx context.Context, s db.TenantScope, remedyID, userID uuid.UUID) error
}

// SlackLinkCodes issues the one-time code an unlinked Slack member enters in oto to link themselves
// (git-bug a556a5c). A PORT DECLARED BY THE CONSUMER, in primitives, for `Remedies`' reason.
//
// ⚠️ ITS CALLER HAS VERIFIED SLACK'S SIGNATURE. The code is bound to the member the VERIFIED
// envelope names, inside the org the CHANNEL resolves to, which is the whole reason the flow starts
// here: only Slack can prove a Slack identity.
type SlackLinkCodes interface {
	// IssueSlackLinkCode mints a code for this member, replacing any code they had. Its refusals
	// are typed — a member already linked to a real user is a Conflict, a deployment that cannot
	// link is Unavailable — and anything else is worth retrying.
	IssueSlackLinkCode(ctx context.Context, s db.TenantScope, teamID, slackUserID string) (SlackLinkCode, error)
}

// SlackLinkCode is an issued code, in the form a person reads it.
type SlackLinkCode struct {
	// Code is `ABCDE-FGHJK`. ⛔ It is a credential for its few minutes: it goes into the ephemeral
	// and nowhere else — not a log line, not a job arg, not an error.
	Code      string
	ExpiresAt time.Time
}

// RemedyApproval is where one approval left its Remedy.
type RemedyApproval struct {
	// Approvals counts the DIFFERENT people who have approved it, this one included.
	Approvals int
	// Required is how many it needs.
	Required int
	// Approved reports whether this approval completed it.
	Approved bool
}

// applyRemedy is one press of Approve or Decline. Tenant (in Apply), subject, human, decision.
func (s *InteractionService) applyRemedy(
	ctx context.Context, logger *slog.Logger, scope db.TenantScope, args jobs.SlackInteractionArgs,
) error {
	approve := args.ActionID == ActionRemedyApprove

	// ---- 2. THE SUBJECT ------------------------------------------------
	remedyID, err := uuid.Parse(args.Value)
	if err != nil || remedyID == uuid.Nil {
		logger.Warn("channels: a Remedy button carried a value that is not an id")
		s.tell(ctx, args, "oto could not read that button. Open the Remedy in oto and decide it there.")
		return nil
	}
	logger = logger.With(slog.String("remedy_id", remedyID.String()))
	if s.remedies == nil {
		logger.Warn("channels: a Remedy press arrived at a deployment with no Remedy port")
		s.tell(ctx, args, "oto cannot approve or decline a Remedy from Slack in this deployment yet. "+
			"Open it in oto and decide it there.")
		return nil
	}

	// ---- 3. THE HUMAN: A LINKED oto USER, OR NOBODY ---------------------
	if s.actors == nil {
		s.tell(ctx, args, unlinkedRemedyText(args, SlackLinkCode{}, 0))
		return nil
	}
	who, err := s.actors.SlackActor(ctx, scope, args.TeamID, args.SlackUserID, args.SlackUserName)
	switch {
	case errs.IsKind(err, errs.KindValidation):
		// A team or member id oto cannot read names nobody it could have linked — and nobody it
		// could bind a link code to.
		s.tell(ctx, args, unlinkedRemedyText(args, SlackLinkCode{}, 0))
		return nil
	case err != nil:
		// ⛔ UNLIKE AN ACK, A DIRECTORY FAILURE IS NOT DEGRADED TO "THE SLACK MEMBER DID IT":
		// an approval needs a person, so the press is retried rather than recorded without one.
		return err
	}
	if who.UserID == uuid.Nil || who.Shadow {
		logger.Info("channels: an unlinked Slack member pressed a Remedy button")
		return s.offerLinkCode(ctx, logger, scope, args)
	}
	logger = logger.With(slog.String("user_id", who.UserID.String()))

	// ---- 4. THE DECISION -----------------------------------------------
	if !approve {
		err := s.remedies.DeclineRemedy(ctx, scope, remedyID, who.UserID)
		if err == nil {
			logger.Info("channels: declined a Remedy from Slack")
			return nil
		}
		return s.remedyRefused(ctx, logger, args, err)
	}
	got, err := s.remedies.ApproveRemedy(ctx, scope, remedyID, who.UserID)
	if err != nil {
		return s.remedyRefused(ctx, logger, args, err)
	}
	logger.Info("channels: approved a Remedy from Slack",
		slog.Int("approvals", got.Approvals), slog.Int("required", got.Required), slog.Bool("approved", got.Approved))
	if !got.Approved {
		s.tell(ctx, args, partialApprovalText(got))
	}
	return nil
}

// remedyRefused answers a decision the service refused with a sentence, and returns any other
// error so the job retries it — a database that is down is worth retrying, a Remedy that has
// expired is not.
func (s *InteractionService) remedyRefused(
	ctx context.Context, logger *slog.Logger, args jobs.SlackInteractionArgs, err error,
) error {
	switch errs.KindOf(err) {
	case errs.KindNotFound, errs.KindForbidden, errs.KindConflict, errs.KindPrecondition, errs.KindValidation:
		logger.Info("channels: a Slack Remedy decision was refused", slog.String("refusal", errs.CodeOf(err)))
		s.tell(ctx, args, remedyRefusalText(err))
		return nil
	default:
		return err
	}
}

// offerLinkCode answers an unlinked member's press with a link code bound to the member the
// verified envelope names (git-bug a556a5c). Nothing about the Remedy is written.
//
// ⛔ A CODE oto COULD NOT ISSUE IS NEVER SHOWN, AND THE PRESS IS STILL ANSWERED. A typed refusal —
// no port, a deployment that cannot link, a member another real user holds — is answered with
// how to decide the Remedy in oto; anything else is returned so the job retries, which issues a
// fresh code (and so kills any code an earlier attempt stored but never delivered).
func (s *InteractionService) offerLinkCode(
	ctx context.Context, logger *slog.Logger, scope db.TenantScope, args jobs.SlackInteractionArgs,
) error {
	if s.linkCodes == nil {
		s.tell(ctx, args, unlinkedRemedyText(args, SlackLinkCode{}, 0))
		return nil
	}
	code, err := s.linkCodes.IssueSlackLinkCode(ctx, scope, args.TeamID, args.SlackUserID)
	if err != nil {
		switch errs.KindOf(err) {
		case errs.KindValidation, errs.KindNotFound, errs.KindConflict, errs.KindUnavailable, errs.KindForbidden:
			logger.Info("channels: no Slack link code was issued", slog.String("refusal", errs.CodeOf(err)))
			s.tell(ctx, args, unlinkedRemedyText(args, SlackLinkCode{}, 0))
			return nil
		default:
			return err
		}
	}
	if code.Code == "" {
		// A port that answered nothing is not a code; showing "``" would be worse than none.
		s.tell(ctx, args, unlinkedRemedyText(args, SlackLinkCode{}, 0))
		return nil
	}
	// ⛔ The code is not logged.
	logger.Info("channels: answered an unlinked Remedy press with a link code")
	s.tell(ctx, args, unlinkedRemedyText(args, code, code.ExpiresAt.Sub(s.clk.Now())))
	return nil
}

// unlinkedRemedyText is the reply to a press by a Slack member who is not linked to an oto user.
// With a code it says how to link — sign in, Account, enter the code — and that the code is a
// credential; without one it says how to decide the Remedy in oto instead. ⭐ IT NAMES THE MEMBER
// AND WORKSPACE IDS, read off the verified press, so the person can match them against what oto's
// confirmation screen shows before they confirm.
func unlinkedRemedyText(args jobs.SlackInteractionArgs, code SlackLinkCode, life time.Duration) string {
	head := "Approving or declining a Remedy from Slack needs your Slack account linked to your oto account, " +
		"and Slack member " + noticeCode(args.SlackUserID) + " in workspace " + noticeCode(args.TeamID) +
		" is not linked, so oto recorded nothing. "
	if code.Code == "" {
		return head + "oto could not give you a link code just now — press the button again for one, " +
			"or open the Remedy in oto and decide it there."
	}
	minutes := int(math.Round(life.Minutes()))
	if minutes < 1 {
		minutes = 1
	}
	return head + "To link it, sign in to oto, open *Account* from your menu and enter " + noticeCode(code.Code) +
		" within " + strconv.Itoa(minutes) + " " + plural(minutes, "minute", "minutes") +
		"; it works once. ⚠️ This code is a credential: whoever enters it in their own oto session makes your " +
		"Slack clicks count as theirs, so never share it. Or open the Remedy in oto and decide it there."
}

// partialApprovalText tells an approver their approval is recorded and the Remedy still waits.
func partialApprovalText(got RemedyApproval) string {
	more := got.Required - got.Approvals
	return "Your approval is recorded: " + strconv.Itoa(got.Approvals) + " of " + strconv.Itoa(got.Required) +
		" different people have approved. It runs once " + strconv.Itoa(more) + " more " +
		plural(more, "holder of the grant on its ToolServer approves", "holders of the grant on its ToolServer approve") + "."
}

// remedyRefusalText says which refusal a decision met, keyed on the codes
// `investigator/domain` and `identity/domain` answer with. A code with no sentence of its own
// is answered with the refusal's own message, which those modules write for a person to read.
func remedyRefusalText(err error) string {
	switch errs.CodeOf(err) {
	case "remedy_not_found":
		return "oto can no longer find that Remedy from this channel. It may belong to a different oto organisation."
	case "remedy_approver_required":
		return "Your Slack account is linked to oto, but you do not hold the approval grant on this Remedy's " +
			"ToolServer, so oto recorded no approval. The grant is given from the host shell by whoever runs oto " +
			"(`oto grant remedy-approver`), never from inside oto."
	case "remedy_already_approved":
		return "You have already approved this Remedy — from Slack or in oto — and one person counts once. " +
			"The next approval must come from a different holder of the grant."
	case "remedy_has_no_tool":
		return "This Remedy cannot be approved: no configured Tool can carry it out. Decline it, or make the change by hand."
	case "slack_member_disabled":
		return "The oto account your Slack account is linked to is disabled, so oto records no decision by it."
	}
	if e, ok := errs.As(err); ok && strings.TrimSpace(e.Message) != "" {
		return sentenceCase(e.Message)
	}
	return "oto could not record that decision. Open the Remedy in oto to see why."
}

// sentenceCase capitalises a refusal's message and ends it with a full stop.
func sentenceCase(s string) string {
	s = strings.TrimSpace(s)
	r, n := utf8.DecodeRuneInString(s)
	s = string(unicode.ToUpper(r)) + s[n:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}
