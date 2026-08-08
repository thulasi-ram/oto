package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// MeView is the answer to "who am I, which org am I in, and how is that org
// tuned".
//
// It is a SERVICE type, not a DTO and not a row: the api layer maps it onto
// MeDTO explicitly, and nothing in it carries a json tag (CONTEXT.md §5.5).
//
// User is nil only for a principal with no human behind it, which on this
// endpoint cannot happen — both credential kinds the contract admits are a
// person's. It is a pointer anyway so that the api layer renders `"user": null`
// rather than a zero-valued object if that ever changes.
type MeView struct {
	Principal authn.Principal
	Org       domain.Org
	User      *domain.User
	// SlackUserID is the linked Slack member id, empty when unlinked. It is a
	// display fact on UserDTO; nothing authorises on it.
	SlackUserID string
}

// Me assembles the current principal's view of itself.
func (s *Service) Me(ctx context.Context, scope db.TenantScope, p authn.Principal) (MeView, error) {
	org, err := s.orgs.Get(ctx, scope)
	if err != nil {
		return MeView{}, err
	}
	if !org.Live() {
		// A soft-deleted org is not a tenant any more. The credential resolvers
		// already exclude one; this is the second refusal, on the read path.
		return MeView{}, unauthenticated()
	}

	view := MeView{Principal: p, Org: org}
	if p.UserID == uuid.Nil {
		return view, nil
	}

	user, err := s.users.Get(ctx, scope, p.UserID)
	if err != nil {
		return MeView{}, err
	}
	view.User = &user

	// The Slack link is a display fact, and a missing one is the normal state for
	// anybody who has not linked their account. A not-found here must not fail
	// the whole endpoint.
	if si, err := s.slack.GetByUser(ctx, scope, p.UserID); err == nil {
		view.SlackUserID = si.SlackUserID.String()
	} else if !errors.Is(err, errs.ErrNotFound) {
		s.log.WarnContext(ctx, "identity: could not read the slack link",
			"user_id", p.UserID, "org_id", scope.OrgID(), "error", err.Error())
	}

	return view, nil
}

// GetOrg returns the caller's org.
func (s *Service) GetOrg(ctx context.Context, scope db.TenantScope) (domain.Org, error) {
	return s.orgs.Get(ctx, scope)
}

// GetUser returns one user within the caller's org. v1 has no RBAC: any member
// may read any member of their own org, and none of another (R2).
func (s *Service) GetUser(ctx context.Context, scope db.TenantScope, userID uuid.UUID) (domain.User, error) {
	return s.users.Get(ctx, scope, userID)
}

// GetUserByEmail returns one user within the caller's org.
func (s *Service) GetUserByEmail(ctx context.Context, scope db.TenantScope, raw string) (domain.User, error) {
	email, err := domain.NewEmail(raw)
	if err != nil {
		return domain.User{}, err
	}
	return s.users.GetByEmail(ctx, scope, email)
}

// ListMembers pages the org's live users. It exists for attribution lookups —
// "who acked this" — and carries no workload, rota or response-time field,
// because there is nowhere in this schema to put one (R8).
func (s *Service) ListMembers(ctx context.Context, scope db.TenantScope, k db.Keyset) ([]domain.User, db.Cursor, error) {
	return s.users.ListMembers(ctx, scope, k)
}

// ResolveSlackActor maps a Slack workspace member onto an actor for a timeline.
//
// ⚠️ It resolves the TENANT from the workspace, so it is unscoped, and its
// caller must already have verified Slack's HMAC signature (§H.8) — this
// function authenticates nothing.
//
// An UNLINKED identity is a success, not a failure: the ack still records, with
// the Slack handle as the actor label. Requiring a link first would silently
// lose acknowledgements from everybody who has not onboarded.
func (s *Service) ResolveSlackActor(ctx context.Context, rawTeam, rawMember string) (domain.SlackIdentity, error) {
	team, err := domain.NewSlackTeamID(rawTeam)
	if err != nil {
		return domain.SlackIdentity{}, err
	}
	member, err := domain.NewSlackUserID(rawMember)
	if err != nil {
		return domain.SlackIdentity{}, err
	}
	return s.slack.ResolveBySlackUser(ctx, team, member)
}

// LinkSlackIdentity binds a Slack member to an oto user within the caller's org.
func (s *Service) LinkSlackIdentity(
	ctx context.Context, scope db.TenantScope, identityID, userID uuid.UUID,
) (domain.SlackIdentity, error) {
	if identityID == uuid.Nil || userID == uuid.Nil {
		return domain.SlackIdentity{}, errs.Validation("invalid_slack_link",
			"linking needs a slack identity and a user")
	}
	return s.slack.Link(ctx, scope, identityID, userID, s.clk.Now())
}

// RecordSlackIdentity notes a sighting of a Slack member, refreshing the
// denormalised handle. It never touches the link, because
// `slack_identities_link_ck` makes that pair all-or-nothing.
func (s *Service) RecordSlackIdentity(
	ctx context.Context, scope db.TenantScope, rawTeam, rawMember, handle string,
) (domain.SlackIdentity, error) {
	team, err := domain.NewSlackTeamID(rawTeam)
	if err != nil {
		return domain.SlackIdentity{}, err
	}
	member, err := domain.NewSlackUserID(rawMember)
	if err != nil {
		return domain.SlackIdentity{}, err
	}
	si, err := domain.NewSlackIdentity(id.New(), scope.OrgID(), team, member, handle)
	if err != nil {
		return domain.SlackIdentity{}, err
	}
	return s.slack.Upsert(ctx, scope, si, s.clk.Now())
}
