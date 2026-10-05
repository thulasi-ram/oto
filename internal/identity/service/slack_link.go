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
// Slack identity that already resolves to another ENABLED person: 409, never moved — on preview so
// the screen says so before anybody confirms, and again on confirm under a row lock. One linked to
// a DISABLED person moves to the member's own code, and the fact names who was displaced (F6).

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
// to a member that member proved they are. Its callers are an unlinked member's Remedy press and
// Ack/Un-ack press (owner ruling F7), behind the interaction transport's HMAC check, with the org
// resolved from the CHANNEL.
//
// A member who already resolves to an ENABLED person gets no code — `slack_identity_linked_elsewhere`
// — because entering one could only be refused; a member linked to a shadow, to a DISABLED person
// (owner ruling F6), or to nothing, gets one.
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
	if held, err := s.heldByAnActiveUser(ctx, scope, si); err != nil {
		return IssuedSlackLinkCode{}, err
	} else if held {
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
	hash, attempt, err := s.presentedCode(ctx, scope, userID, rawCode)
	if err != nil {
		return SlackLinkPreview{}, err
	}
	preview, err := s.previewSlackLink(ctx, scope, userID, hash)
	return preview, s.settleAttempt(ctx, scope, userID, attempt, err)
}

func (s *Service) previewSlackLink(
	ctx context.Context, scope db.TenantScope, userID uuid.UUID, hash domain.TokenHash,
) (SlackLinkPreview, error) {
	identityID, expires, err := s.links.PresentCode(ctx, scope, hash, s.clk.Now())
	if err != nil {
		return SlackLinkPreview{}, err
	}
	si, err := s.slack.GetByID(ctx, scope, identityID)
	if err != nil {
		return SlackLinkPreview{}, err
	}
	if si.UserID == userID {
		return SlackLinkPreview{Identity: si, ExpiresAt: expires, AlreadyYours: true}, nil
	}
	if held, err := s.heldByAnActiveUser(ctx, scope, si); err != nil {
		return SlackLinkPreview{}, err
	} else if held {
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
	hash, attempt, err := s.presentedCode(ctx, scope, userID, rawCode)
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
		if held, err := s.heldByAnActiveUser(ctx, scope, incumbent); err != nil {
			return err
		} else if held {
			return domain.SlackIdentityLinkedElsewhere()
		}
		// ⭐ THE INCUMBENT MAY BE A DISABLED PERSON (owner ruling F6): the code proves the Slack
		// identity is the presser's, a disabled user cannot sign in to unlink it, and the fact
		// below names them as `displaced_user_id` — so the move is on the record, never silent.
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
	if err := s.settleAttempt(ctx, scope, userID, attempt, err); err != nil {
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
func (s *Service) slackLinkSubject(_ context.Context, scope db.TenantScope, p authn.Principal) (uuid.UUID, error) {
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

// presentedCode spends one attempt of the user's wrong-code budget BEFORE reading what they sent,
// then parses it. A malformed code is a wrong code: its attempt is already on record.
//
// ⛔⛔ THE BUDGET IS RESERVED, NOT COUNTED (review E2). It used to count, evaluate, and record a
// wrong code afterwards, outside any lock, so twenty parallel previews all counted under the limit
// and all were evaluated. `ReserveAttempt` now counts and records under a lock on the user, in a
// transaction committed BEFORE the code is evaluated — so the attempt is on record whatever the
// evaluation does, and an attempt oto cannot record is never evaluated (fail closed). A code that
// proves right gives its attempt back (`settleAttempt`).
func (s *Service) presentedCode(
	ctx context.Context, scope db.TenantScope, userID uuid.UUID, rawCode string,
) (domain.TokenHash, uuid.UUID, error) {
	now := s.clk.Now()
	attempt := id.New()
	var allowed bool
	err := s.inTx(ctx, func(ctx context.Context) error {
		var err error
		allowed, err = s.links.ReserveAttempt(ctx, scope, attempt, userID, now,
			now.Add(-domain.SlackLinkWrongAttemptWindow), domain.SlackLinkWrongAttemptLimit)
		return err
	})
	if err != nil {
		return domain.TokenHash{}, uuid.Nil, err
	}
	if !allowed {
		return domain.TokenHash{}, uuid.Nil, errs.RateLimited(domain.SlackLinkAttemptsExhaustedCode,
			"too many wrong link codes; wait a few minutes, then press an Ack or a Remedy button in Slack again for a new one",
			domain.SlackLinkWrongAttemptWindow)
	}
	code, err := domain.ParseSlackLinkCode(rawCode)
	if err != nil {
		s.logWrongCode(ctx, scope, userID)
		return domain.TokenHash{}, uuid.Nil, err
	}
	hash, err := code.Hash()
	if err != nil {
		return domain.TokenHash{}, uuid.Nil, s.settleAttempt(ctx, scope, userID, attempt, err)
	}
	return hash, attempt, nil
}

// settleAttempt decides what a reserved attempt was once the code has been evaluated: a WRONG code
// (`slack_link_code_invalid`) keeps it on the budget; anything else — the code proved right, or the
// evaluation failed for a reason that is not the user's guess — gives it back. The release is best
// effort and logged: an attempt left on the budget by a failed release costs the user one guess,
// never the guarantee. It returns `outcome` unchanged.
func (s *Service) settleAttempt(ctx context.Context, scope db.TenantScope, userID, attempt uuid.UUID, outcome error) error {
	if errs.CodeOf(outcome) == domain.SlackLinkCodeInvalidCode {
		s.logWrongCode(ctx, scope, userID)
		return outcome
	}
	if attempt == uuid.Nil {
		return outcome
	}
	if err := s.links.ReleaseAttempt(ctx, scope, attempt); err != nil {
		s.log.WarnContext(ctx, "identity: could not give back a Slack link attempt that was not a wrong code",
			"org_id", scope.OrgID(), "user_id", userID, "error", err.Error())
	}
	return outcome
}

func (s *Service) logWrongCode(ctx context.Context, scope db.TenantScope, userID uuid.UUID) {
	s.log.InfoContext(ctx, "identity: a wrong Slack link code was presented",
		"org_id", scope.OrgID(), "user_id", userID)
}

// heldByAnActiveUser reports whether an identity resolves to a user who is a PERSON (not a shadow
// member) and ENABLED — the one link this path must never move.
//
// ⭐ A DISABLED PERSON'S LINK MAY MOVE (owner ruling F6, 2026-10-05; review E3). It used to count
// as "real" too, and then a member linked to a disabled user could never relink: no code was
// issued, the disabled user cannot sign in to unlink, and there is no CLI. The member's own code
// proves the Slack identity is theirs; the confirm records the disabled user as displaced.
func (s *Service) heldByAnActiveUser(ctx context.Context, scope db.TenantScope, si domain.SlackIdentity) (bool, error) {
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
	return !u.IsShadow() && u.Active(), nil
}
