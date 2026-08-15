package service

import (
	"context"
	"errors"
	"time"

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

	view := MeView{Principal: p, Org: org.WithDeclarative(s.declarative)}
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
//
// ⭐ THIS IS THE HOT PATH FOR EVERY TUNING KNOB. `internal/app`'s `orgSettings`
// adapter calls it once per lifecycle evaluation and once per storm evaluation,
// and it goes STRAIGHT TO POSTGRES: there is no memo, no TTL and no process-local
// copy anywhere between here and the row. That is what makes a settings change
// take effect on the very next evaluation, in every pod, with no restart and no
// invalidation message — and it is a property worth defending. If a cache is ever
// added here it MUST carry a bounded TTL, because the failure it would introduce
// is the one nobody notices: an operator raises `storm_threshold` during an
// incident, watches nothing change, and has no way to tell whether the setting is
// wrong or merely stale.
//
// ⭐ IT OVERLAYS THE DECLARATIVE LAYER, AND EVERY CALLER GETS THE OVERLAY. That
// is the whole reason the overlay lives here rather than in the api layer: the
// notify worker and the settings screen read the same Org through the same
// method, so a value the screen labels "managed by configuration" is by
// construction the value the hot path is using.
func (s *Service) GetOrg(ctx context.Context, scope db.TenantScope) (domain.Org, error) {
	org, err := s.orgs.Get(ctx, scope)
	if err != nil {
		return domain.Org{}, err
	}
	return org.WithDeclarative(s.declarative), nil
}

// retentionPageSize bounds one ListLive query in the MaxRetention walk. It is
// the repository's own page ceiling, stated once here: with the walk ending
// only on an EMPTY page, the number is a round-trip cost and never a
// correctness input, so it cannot reproduce the truncation defect the walk's
// own comment describes.
const retentionPageSize = 200

// MaxRetention reports the widest retention window any LIVE org has asked for:
// the maximum effective RawRetention and EventRetention over every tenant this
// process serves. Zero orgs answer zero durations, which every caller treats
// as "no tenant asks for anything" and floors with its own configuration.
//
// ⭐ IT LIVES HERE, IN IDENTITY, BECAUSE THE DECLARATIVE OVERLAY DOES. This
// service overlays the deployment's Declarative onto EVERY org read and
// RECOMPUTES the effective settings from it (`Org.WithDeclarative`), and the
// declarative value BEATS the org's own. An aggregate a caller wrote over the
// raw `orgs.settings` column would skip that overlay, so on an install where
// configuration forces a retention key it would compute a maximum over numbers
// nobody is using. The reduce has to run where the overlay is applied, so each
// row gets the same `WithDeclarative` the settings screen and the hot path get.
//
// ⛔ THE REDUCE IS EXACT OR IT IS AN ERROR, NEVER A PARTIAL ANSWER. Its caller
// drops partitions on this number, a maximum that missed the tenant with the
// longest window drops that tenant's rows early, and retention is the one
// setting pair whose wrong value is unrecoverable. So any failure mid-walk is
// returned as an error — the caller's documented response is to WIDEN — and
// the walk ends only when a page comes back EMPTY: a short page is not trusted
// as the end of the table, because a limit clamped anywhere between here and
// the SQL would otherwise truncate the reduce into a silently narrower answer.
// The extra empty read costs one round trip an hour.
func (s *Service) MaxRetention(ctx context.Context) (raw, event time.Duration, err error) {
	after := uuid.Nil
	for {
		orgs, lerr := s.orgs.ListLive(ctx, after, retentionPageSize)
		if lerr != nil {
			return 0, 0, lerr
		}
		if len(orgs) == 0 {
			return raw, event, nil
		}
		for _, org := range orgs {
			eff := org.WithDeclarative(s.declarative).Settings
			if eff.RawRetention > raw {
				raw = eff.RawRetention
			}
			if eff.EventRetention > event {
				event = eff.EventRetention
			}
		}
		after = orgs[len(orgs)-1].ID
	}
}

// UpdateOrgSettings applies a partial write to this org's tuning and returns the
// org as stored.
//
// ⛔ THE BOUNDS ARE ENFORCED HERE, and this is the only door. Validation runs on
// the MERGED patch rather than on the incoming fragment, so a write cannot slip a
// value past by relying on a key it did not send — and the result is written only
// if the whole merged state is legal. A UI that also checks is a nicety; a
// request from `curl` gets the same answer.
//
// ⛔⛔ A KEY THE DEPLOYMENT MANAGES IS REFUSED WITH 409, NAMING THE CONFIG KEY.
// It is not accepted-and-ignored and it is not accepted-and-overridden-on-read:
// both of those store a number the operator will see in the database, never see
// in force, and have no way to explain. The 409 says which line of which values
// file to edit instead, which is the only useful answer.
func (s *Service) UpdateOrgSettings(
	ctx context.Context, scope db.TenantScope, next domain.SettingsPatch, reset []domain.SettingKey,
) (domain.Org, error) {
	current, err := s.orgs.Get(ctx, scope)
	if err != nil {
		return domain.Org{}, err
	}
	if !current.Live() {
		return domain.Org{}, unauthenticated()
	}

	if err := s.refuseDeclarative(next, reset); err != nil {
		return domain.Org{}, err
	}

	merged := current.Overrides.Clear(reset...).Merge(next)
	if err := merged.Validate(); err != nil {
		return domain.Org{}, err
	}
	org, err := s.orgs.UpdateSettings(ctx, scope, merged)
	if err != nil {
		return domain.Org{}, err
	}
	return org.WithDeclarative(s.declarative), nil
}

// CodeSettingManagedByConfig is the problem `code` a write to a
// declaratively-managed key receives. It is stable and distinguishable so a UI
// can react to it without parsing prose.
const CodeSettingManagedByConfig = "setting_managed_by_config"

// refuseDeclarative rejects a write — or a reset — that names a key the
// deployment is managing.
//
// A RESET IS REFUSED TOO, and for the same reason a write is. "Return this key to
// oto's default" cannot happen while configuration is forcing a value, so
// accepting it would report success for something that did not occur; and unlike
// a write, a reset also DESTROYS the shadowed override underneath, which is the
// one thing that would come back if the config key were removed.
//
// Every offending key is reported in one response, with its config key. Refusing
// them one at a time is how a caller with four managed keys learns about them
// over four round trips.
func (s *Service) refuseDeclarative(next domain.SettingsPatch, reset []domain.SettingKey) error {
	if s.declarative.Empty() {
		return nil
	}

	named := map[domain.SettingKey]bool{}
	for _, k := range next.Overridden() {
		named[k] = true
	}
	for _, k := range reset {
		named[k] = true
	}

	var v []errs.Violation
	for _, k := range domain.AllSettingKeys() {
		if !named[k] || !s.declarative.Manages(k) {
			continue
		}
		v = append(v, errs.Violation{
			Field: string(k), Code: "managed_by_config",
			Message: "this value is set by " + s.declarative.ConfigKey(k) +
				" and the deployment's configuration wins; change it there, or remove that key to hand the setting back to this org",
		})
	}
	if len(v) == 0 {
		return nil
	}
	return errs.Conflict(CodeSettingManagedByConfig,
		"one or more of these settings are set by this deployment's configuration and cannot be changed over the API").
		WithViolations(v...)
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
