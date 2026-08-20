package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/validate"
)

// MaxSlackHandleBytes bounds the denormalised handle. There is no DDL CHECK; the
// bound exists because the value comes from a foreign system.
const MaxSlackHandleBytes = 120

// SlackTeamID is a Slack workspace id (slack_identities_team_ck). The pattern
// lives in `platform/validate` and is reused rather than restated: a drift test
// asserts those strings stay byte-identical to the DDL.
type SlackTeamID struct{ v string }

// NewSlackTeamID parses a workspace id.
func NewSlackTeamID(s string) (SlackTeamID, error) {
	s = strings.TrimSpace(s)
	if !validate.SlackTeamIDRe.MatchString(s) {
		return SlackTeamID{}, errs.Validation("invalid_slack_team_id",
			"team_id must match "+validate.PatternSlackTeamID)
	}
	return SlackTeamID{v: s}, nil
}

// String renders the workspace id.
func (t SlackTeamID) String() string { return t.v }

// IsZero reports whether the workspace id is unset.
func (t SlackTeamID) IsZero() bool { return t.v == "" }

// SlackUserID is a Slack member id (slack_identities_user_ck): `U…` for humans,
// `W…` on Enterprise Grid.
type SlackUserID struct{ v string }

// NewSlackUserID parses a member id.
func NewSlackUserID(s string) (SlackUserID, error) {
	s = strings.TrimSpace(s)
	if !validate.SlackUserIDRe.MatchString(s) {
		return SlackUserID{}, errs.Validation("invalid_slack_user_id",
			"slack_user_id must match "+validate.PatternSlackUserID)
	}
	return SlackUserID{v: s}, nil
}

// String renders the member id.
func (u SlackUserID) String() string { return u.v }

// IsZero reports whether the member id is unset.
func (u SlackUserID) IsZero() bool { return u.v == "" }

// SlackIdentity maps a Slack workspace member onto an oto user, so that an ack
// pressed in Slack is attributable.
//
// An UNLINKED identity (UserID zero) is a first-class state, not a broken one:
// it still acks, and the timeline records the Slack handle as the actor label.
// Requiring a link before an ack could be recorded would mean the product
// silently loses acknowledgements from anybody who has not onboarded.
//
// ⚠️ SINCE MIGRATION 00074 IT IS ALSO A SHORT-LIVED STATE FOR ANYBODY WHO PRESSES
// A BUTTON. The first press mints a SHADOW member and links this row to it, because
// `idempotency_claims.principal_id` is NOT NULL and a press with no principal takes
// no claim — so a Slack redelivery applied it twice (git-bug `a74d6b2`). `UserID`
// zero therefore means "has never acted", not "has no account": a shadow carries no
// email and cannot log in, and a later genuine link ADOPTS it rather than minting a
// second member. The label above is still what the timeline shows.
type SlackIdentity struct {
	ID          uuid.UUID
	OrgID       uuid.UUID
	TeamID      SlackTeamID
	SlackUserID SlackUserID
	// Handle is the display handle AT LINK TIME. It is denormalised and allowed
	// to go stale: oto never reads Slack back (SPEC C9).
	Handle string
	// UserID is the linked oto user, zero when unlinked.
	UserID uuid.UUID
	// LinkedAt is set exactly when UserID is set; the pair is all-or-nothing
	// (slack_identities_link_ck).
	LinkedAt  *time.Time
	CreatedAt time.Time
}

// NewSlackIdentity builds an unlinked identity. Linking is a separate, explicit
// transition so that the all-or-nothing invariant has exactly one enforcement
// point.
func NewSlackIdentity(id, orgID uuid.UUID, teamID SlackTeamID, slackUserID SlackUserID, handle string) (SlackIdentity, error) {
	if id == uuid.Nil {
		return SlackIdentity{}, errs.Validation("invalid_slack_identity_id", "a slack identity needs an id")
	}
	if orgID == uuid.Nil {
		return SlackIdentity{}, errs.Validation("invalid_slack_identity_org",
			"a slack identity belongs to exactly one org")
	}
	if teamID.IsZero() {
		return SlackIdentity{}, errs.Validation("invalid_slack_team_id", "a slack identity needs a team")
	}
	if slackUserID.IsZero() {
		return SlackIdentity{}, errs.Validation("invalid_slack_user_id", "a slack identity needs a member id")
	}

	handle = strings.TrimSpace(strings.TrimPrefix(handle, "@"))
	if len(handle) > MaxSlackHandleBytes {
		handle = handle[:MaxSlackHandleBytes]
	}

	return SlackIdentity{
		ID:          id,
		OrgID:       orgID,
		TeamID:      teamID,
		SlackUserID: slackUserID,
		Handle:      handle,
	}, nil
}

// Linked reports whether this Slack member resolves to an oto user.
func (s SlackIdentity) Linked() bool { return s.UserID != uuid.Nil }

// Link binds the identity to an oto user, returning the updated value.
//
// It sets UserID and LinkedAt together, which is the Go half of
// slack_identities_link_ck. Nothing else in this package may write either field,
// so the pair cannot fall out of step.
func (s SlackIdentity) Link(userID uuid.UUID, at time.Time) (SlackIdentity, error) {
	if userID == uuid.Nil {
		return SlackIdentity{}, errs.Validation("invalid_slack_link", "linking needs a user")
	}
	if at.IsZero() {
		return SlackIdentity{}, errs.Validation("invalid_slack_link", "linking needs a time")
	}
	linked := at.UTC()
	s.UserID = userID
	s.LinkedAt = &linked
	return s, nil
}

// Unlink drops the binding, clearing both halves together.
func (s SlackIdentity) Unlink() SlackIdentity {
	s.UserID = uuid.Nil
	s.LinkedAt = nil
	return s
}

// ActorLabel is how this identity appears on a timeline when it is not linked to
// an oto user: the Slack handle, or the member id when even that is unknown.
// ACTOR METADATA on a signal, never a subject (CONTEXT.md §1b).
func (s SlackIdentity) ActorLabel() string {
	if s.Handle != "" {
		return "@" + s.Handle
	}
	return s.SlackUserID.String()
}
