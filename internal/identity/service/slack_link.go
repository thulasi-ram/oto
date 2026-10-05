package service

// A SLACK MEMBER LINKS THEMSELVES TO AN oto USER WITH A CODE oto SHOWED ONLY THEM (git-bug a556a5c;
// the ruling of 2026-10-05: SELF-SERVICE IN THE UI). The why is in `domain/slack_link_code.go`; this
// file is the four acts — issue, preview, confirm, unlink — and the one read.
//
// ⛔⛔ NOTHING HERE TAKES A USER ID FROM ITS CALLER. Preview, confirm, unlink and the list each take
// the PRINCIPAL the authenticator resolved, and the only user they ever link, unlink or list is
// `p.UserID`. There is no parameter through which a request could name somebody else, so "no route
// links an arbitrary user" is a property of these signatures and not of a check someone must keep.
//
// ⛔ AND THE WRITES DEMAND A SESSION, here as well as at the router. A link decides whose approval a
// Slack click counts as, so a leaked PAT that could link would let its holder make THEIR Slack
// clicks count as the PAT's owner — the router's session-only group refuses that on the header, and
// this refuses it again so a future mount that forgot cannot open it.
//
// ⭐ EVERY REFUSAL IS FAIL-CLOSED. No store: 503. A code that is not live, for any reason: one 422,
// counted against the user. Five of those in fifteen minutes: 429 before the code is even read. A
// Slack identity that already resolves to another real user: 409, never moved — on preview so the
// screen says so before anybody confirms, and again on confirm under a row lock.

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// IssuedSlackLinkCode is a freshly minted code, in the form Slack shows it. It exists in this
// process for one ephemeral reply and is never stored, logged or put in an error.
type IssuedSlackLinkCode struct {
	// Code is the display form, `ABCDE-FGHJK`.
	Code      string
	ExpiresAt time.Time
}

// SlackLinkPreview is what a code WOULD link, read without consuming it.
type SlackLinkPreview struct {
	Identity  domain.SlackIdentity
	ExpiresAt time.Time
	// AlreadyYours is true when the member already resolves to the signed-in user, so confirming
	// changes nothing.
	AlreadyYours bool
}

func slackLinksUnavailable() error {
	return errs.Unavailable("slack_link_unavailable",
		"linking a Slack account is not available in this deployment", 0)
}

// IssueSlackLinkCode mints the one live code for a Slack member, replacing any code they had.
//
// ⚠️ ITS CALLER MUST ALREADY HAVE VERIFIED SLACK'S SIGNATURE: it is how the code comes to be bound
// to a member that member proved they are. Its one caller is the Remedy press, behind the
// interaction transport's HMAC check, with the org resolved from the CHANNEL.
//
// A member who already resolves to a REAL user gets no code — `slack_identity_linked_elsewhere` —
// because entering one could only be refused; a member linked to a shadow, or to nothing, gets one.
func (s *Service) IssueSlackLinkCode(
	ctx context.Context, scope db.TenantScope, rawTeam, rawMember string,
) (IssuedSlackLinkCode, error) {
	if s.links == nil {
		return IssuedSlackLinkCode{}, slackLinksUnavailable()
	}
	team, err := domain.NewSlackTeamID(rawTeam)
	if err != nil {
		return IssuedSlackLinkCode{}, err
	}
	member, err := domain.NewSlackUserID(rawMember)
	if err != nil {
		return IssuedSlackLinkCode{}, err
	}
	si, err := s.slack.GetBySlackUser(ctx, scope, team, member)
	if err != nil {
		return IssuedSlackLinkCode{}, err
	}
	if real, err := s.linkedToARealUser(ctx, scope, si); err != nil {
		return IssuedSlackLinkCode{}, err
	} else if real {
		return IssuedSlackLinkCode{}, domain.SlackIdentityLinkedElsewhere()
	}

	code, err := domain.NewSlackLinkCode(rand.Reader)
	if err != nil {
		return IssuedSlackLinkCode{}, err
	}
	hash, err := code.Hash()
	if err != nil {
		return IssuedSlackLinkCode{}, err
	}
	now := s.clk.Now()
	expires := now.Add(domain.SlackLinkCodeTTL)
	if err := s.links.IssueCode(ctx, scope, si.ID, hash, now, expires); err != nil {
		return IssuedSlackLinkCode{}, err
	}
	// ⛔ The code is not in this line, and must never be.
	s.log.InfoContext(ctx, "identity: issued a Slack link code",
		"org_id", scope.OrgID(), "slack_identity_id", si.ID, "expires_at", expires)
	return IssuedSlackLinkCode{Code: code.Display(), ExpiresAt: expires.UTC()}, nil
}

// PreviewSlackLink says which Slack member a code would link to the signed-in user, WITHOUT
// consuming it — the confirmation screen's "clicks from this Slack account will count as you".
// It spends one of the code's presentations.
func (s *Service) PreviewSlackLink(
	ctx context.Context, scope db.TenantScope, p authn.Principal, rawCode string,
) (SlackLinkPreview, error) {
	userID, err := s.slackLinkSubject(ctx, scope, p)
	if err != nil {
		return SlackLinkPreview{}, err
	}
	hash, err := s.presentedCode(ctx, scope, userID, rawCode)
	if err != nil {
		return SlackLinkPreview{}, err
	}
	identityID, expires, err := s.links.PresentCode(ctx, scope, hash, s.clk.Now())
	if err != nil {
		return SlackLinkPreview{}, s.wrongCode(ctx, scope, userID, err)
	}
	si, err := s.slack.GetByID(ctx, scope, identityID)
	if err != nil {
		return SlackLinkPreview{}, err
	}
	if si.UserID == userID {
		return SlackLinkPreview{Identity: si, ExpiresAt: expires, AlreadyYours: true}, nil
	}
	if real, err := s.linkedToARealUser(ctx, scope, si); err != nil {
		return SlackLinkPreview{}, err
	} else if real {
		return SlackLinkPreview{}, domain.SlackIdentityLinkedElsewhere()
	}
	return SlackLinkPreview{Identity: si, ExpiresAt: expires}, nil
}

// ConfirmSlackLink uses a code up and links its Slack member to the signed-in user, through the
// same re-point and shadow retirement `LinkSlackIdentity` performs, and records the fact.
//
// ⭐ THE CODE, THE LOCK, THE CHECK, THE LINK AND THE FACT ARE ONE TRANSACTION. A refusal rolls the
// consumption back with everything else, so a code refused because its member belongs to somebody
// else is not burned by the refusal — and a link is never written without its fact.
func (s *Service) ConfirmSlackLink(
	ctx context.Context, scope db.TenantScope, p authn.Principal, rawCode string,
) (domain.SlackIdentity, error) {
	userID, err := s.slackLinkSubject(ctx, scope, p)
	if err != nil {
		return domain.SlackIdentity{}, err
	}
	hash, err := s.presentedCode(ctx, scope, userID, rawCode)
	if err != nil {
		return domain.SlackIdentity{}, err
	}

	var linked domain.SlackIdentity
	err = s.inTx(ctx, func(ctx context.Context) error {
		now := s.clk.Now()
		identityID, err := s.links.ConsumeCode(ctx, scope, hash, userID, now)
		if err != nil {
			return err
		}
		incumbent, err := s.slack.LockByID(ctx, scope, identityID)
		if err != nil {
			return err
		}
		if incumbent.UserID == userID {
			// Already this person's: the code is spent and nothing else changes, so there is no
			// fact to record.
			linked = incumbent
			return nil
		}
		if real, err := s.linkedToARealUser(ctx, scope, incumbent); err != nil {
			return err
		} else if real {
			return domain.SlackIdentityLinkedElsewhere()
		}
		linked, err = s.linkSlackIdentity(ctx, scope, incumbent.ID, userID)
		if err != nil {
			return err
		}
		var displaced uuid.UUID
		if incumbent.Linked() {
			displaced = incumbent.UserID
		}
		fact, err := domain.NewSlackLinkFact(id.New(), linked, domain.SlackLinkLinked, userID, displaced, userID, now)
		if err != nil {
			return err
		}
		return s.links.RecordFact(ctx, scope, fact)
	})
	if err != nil {
		if errs.CodeOf(err) == domain.SlackLinkCodeInvalidCode {
			return domain.SlackIdentity{}, s.wrongCode(ctx, scope, userID, err)
		}
		return domain.SlackIdentity{}, err
	}
	s.log.InfoContext(ctx, "identity: a user linked a Slack member to themselves",
		"org_id", scope.OrgID(), "user_id", userID, "slack_identity_id", linked.ID)
	return linked, nil
}

// UnlinkSlackIdentity drops the link of a Slack identity linked to the signed-in user, and records
// the fact. Any other identity — somebody else's, unlinked, another org's — is a 404.
//
// ⚠️ THE MEMBER'S NEXT PRESS MINTS A FRESH SHADOW. `ResolveSlackPresser` treats an unlinked identity
// as one that has never acted; that is the honest reading now, since its acts from here on are no
// longer this person's.
func (s *Service) UnlinkSlackIdentity(
	ctx context.Context, scope db.TenantScope, p authn.Principal, identityID uuid.UUID,
) error {
	userID, err := s.slackLinkSubject(ctx, scope, p)
	if err != nil {
		return err
	}
	if identityID == uuid.Nil {
		return errs.NotFound("slack_identity_not_found", "no such slack identity")
	}
	err = s.inTx(ctx, func(ctx context.Context) error {
		si, err := s.slack.Unlink(ctx, scope, identityID, userID)
		if err != nil {
			return err
		}
		fact, err := domain.NewSlackLinkFact(id.New(), si, domain.SlackLinkUnlinked, userID, uuid.Nil, userID, s.clk.Now())
		if err != nil {
			return err
		}
		return s.links.RecordFact(ctx, scope, fact)
	})
	if err != nil {
		return err
	}
	s.log.InfoContext(ctx, "identity: a user unlinked a Slack member from themselves",
		"org_id", scope.OrgID(), "user_id", userID, "slack_identity_id", identityID)
	return nil
}

// ListMySlackIdentities is every Slack identity linked to the signed-in user.
func (s *Service) ListMySlackIdentities(
	ctx context.Context, scope db.TenantScope, p authn.Principal,
) ([]domain.SlackIdentity, error) {
	if p.UserID == uuid.Nil || (p.Kind != authn.KindSession && p.Kind != authn.KindPAT) {
		return nil, errs.Forbidden("slack_link_needs_a_person", "only a signed-in person has Slack accounts linked")
	}
	return s.slack.ListByUser(ctx, scope, p.UserID)
}

// slackLinkSubject is the ONE user a write may act on: the human behind a SESSION.
func (s *Service) slackLinkSubject(ctx context.Context, scope db.TenantScope, p authn.Principal) (uuid.UUID, error) {
	if s.links == nil {
		return uuid.Nil, slackLinksUnavailable()
	}
	if p.Kind != authn.KindSession || p.UserID == uuid.Nil {
		return uuid.Nil, errs.Forbidden("slack_link_needs_a_session",
			"a Slack account is linked or unlinked only from a signed-in browser session")
	}
	if p.OrgID != scope.OrgID() {
		return uuid.Nil, errs.Internal("slack_link_scope_mismatch", nil)
	}
	return p.UserID, nil
}

// presentedCode refuses a user who has spent their wrong-code budget BEFORE reading what they sent,
// then parses it. A malformed code is a wrong code and is counted as one.
func (s *Service) presentedCode(
	ctx context.Context, scope db.TenantScope, userID uuid.UUID, rawCode string,
) (domain.TokenHash, error) {
	now := s.clk.Now()
	n, err := s.links.CountWrongAttempts(ctx, scope, userID, now.Add(-domain.SlackLinkWrongAttemptWindow))
	if err != nil {
		return domain.TokenHash{}, err
	}
	if n >= domain.SlackLinkWrongAttemptLimit {
		return domain.TokenHash{}, errs.RateLimited(domain.SlackLinkAttemptsExhaustedCode,
			"too many wrong link codes; wait a few minutes, then press the Remedy button in Slack again for a new one",
			domain.SlackLinkWrongAttemptWindow)
	}
	code, err := domain.ParseSlackLinkCode(rawCode)
	if err != nil {
		return domain.TokenHash{}, s.wrongCode(ctx, scope, userID, err)
	}
	return code.Hash()
}

// wrongCode counts a refused code against the user and returns the refusal. Any error that is not
// the code refusal passes through uncounted: a database that is down is not the user's guess.
//
// ⚠️ IT RUNS OUTSIDE ANY TRANSACTION THE REFUSAL CAME FROM — ConfirmSlackLink calls it after its
// own has rolled back — because a count written inside a transaction that then rolls back is a
// count that never happened, and a limit that forgets is not one.
func (s *Service) wrongCode(ctx context.Context, scope db.TenantScope, userID uuid.UUID, refusal error) error {
	if errs.CodeOf(refusal) != domain.SlackLinkCodeInvalidCode {
		return refusal
	}
	now := s.clk.Now()
	if err := s.links.RecordWrongAttempt(ctx, scope, id.New(), userID, now,
		now.Add(-domain.SlackLinkWrongAttemptWindow)); err != nil {
		// ⛔ FAIL CLOSED: an attempt oto could not count is not answered as a mere wrong code,
		// because a guesser who can make the count fail would otherwise guess for free.
		return err
	}
	s.log.InfoContext(ctx, "identity: a wrong Slack link code was presented",
		"org_id", scope.OrgID(), "user_id", userID)
	return refusal
}

// linkedToARealUser reports whether an identity resolves to a user who is NOT a shadow member —
// a person, disabled or not, whose link this path must never move.
func (s *Service) linkedToARealUser(ctx context.Context, scope db.TenantScope, si domain.SlackIdentity) (bool, error) {
	if !si.Linked() {
		return false, nil
	}
	u, err := s.users.Get(ctx, scope, si.UserID)
	if err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			// A link to a row this org cannot read is stale, not a person's.
			return false, nil
		}
		return false, err
	}
	return !u.IsShadow(), nil
}
