package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
)

// The DTO ↔ service/domain mappers (SPEC §E.5).
//
// Every one is explicit and field by field. Nothing here embeds, aliases or
// re-tags a domain type: a DTO that embedded domain.Org would make the wire
// format and the invariant model the same type, and the next change to either
// would silently change the other.
//
// ⚠️ A HANDLER NEVER SEES A REPOSITORY ROW MODEL. Those are unexported inside
// `identity/repository` and cannot be named from this package at all — the
// property is enforced by the compiler, not by review.

func toOrgSettingsDTO(s domain.Settings) OrgSettingsDTO {
	s = s.Normalise()
	return OrgSettingsDTO{
		RefireGraceS:        int(s.RefireGrace / time.Second),
		ResolveGraceS:       int(s.ResolveGrace / time.Second),
		GroupCloseDelayS:    int(s.GroupCloseDelay / time.Second),
		FlapThreshold:       s.FlapThreshold,
		FlapWindowS:         int(s.FlapWindow / time.Second),
		FlapDigestIntervalS: int(s.FlapDigestInterval / time.Second),
		StormThreshold:      s.StormThreshold,
		StormWindowS:        int(s.StormWindow / time.Second),
		StormCooldownS:      int(s.StormCooldown / time.Second),
		RawRetentionDays:    int(s.RawRetention / (24 * time.Hour)),
		// §D.1 stores a month count and oto reads a month as 30 days, uniformly.
		EventRetentionMonth: int(s.EventRetention / (30 * 24 * time.Hour)),

		UnackedReminderAfterS: int(s.UnackedReminderAfter / time.Second),
		DefaultVerbosity:      s.DefaultVerbosity,
		BroadcastOnResolved:   s.BroadcastOnResolved,

		UnackedReminderMention: s.UnackedReminderMention,
		// Never null on the wire: an empty audience is `[]`, not `null`, so a
		// client can render "nobody" without a special case.
		UnackedReminderMentionList:        nonNilStrings(s.UnackedReminderMentionList),
		UnackedReminderMentionMinSeverity: s.UnackedReminderMentionMinSeverity,
	}
}

func toOrgDTO(o domain.Org) OrgDTO {
	return OrgDTO{
		ID:       o.ID,
		Slug:     o.Slug,
		Name:     o.Name,
		Settings: toOrgSettingsDTO(o.Settings),
	}
}

// toUserDTO renders a user. slackUserID is passed in rather than read off the
// user because the link lives on a different table and is a display fact, not a
// property of the human.
func toUserDTO(u domain.User, slackUserID string) UserDTO {
	dto := UserDTO{
		ID:          u.ID,
		Email:       u.Email.String(),
		DisplayName: u.DisplayName,
	}
	if slackUserID != "" {
		v := slackUserID
		dto.SlackUserID = &v
	}
	return dto
}

// principalKind maps oto's internal authn.Kind onto the contract's two-value
// enum. The mapping is narrowing on purpose: `ingest`, `slack` and `system`
// principals exist, and none of them can reach `GET /me`, so none of them has a
// representation here to be rendered by accident.
func principalKind(k authn.Kind) string {
	if k == authn.KindPAT {
		return "pat"
	}
	return "user"
}

func toMeDTO(v service.MeView) MeDTO {
	dto := MeDTO{
		PrincipalKind: principalKind(v.Principal.Kind),
		Org:           toOrgDTO(v.Org),
	}
	if v.User != nil {
		u := toUserDTO(*v.User, v.SlackUserID)
		dto.User = &u
	}
	if v.Principal.Kind == authn.KindPAT && v.Principal.TokenID != uuid.Nil {
		id := v.Principal.TokenID
		dto.TokenID = &id
	}
	if !v.Principal.ExpiresAt.IsZero() {
		t := v.Principal.ExpiresAt.UTC()
		dto.SessionExpiresAt = &t
	}
	return dto
}

// toAPITokenDTO renders a token.
//
// ⚠️ THE HASH IS NOT MAPPED, and there is nowhere to map it to: APITokenDTO has
// no field for it. Adding one would be the single change that turns a token list
// into a credential dump, which is why the absence is stated here rather than
// left to be noticed.
func toAPITokenDTO(t domain.APIToken) APITokenDTO {
	dto := APITokenDTO{
		ID:         t.ID,
		Kind:       string(t.Kind),
		Name:       t.Name,
		Prefix:     t.Prefix.String(),
		LastUsedAt: utcPtr(t.LastUsedAt),
		ExpiresAt:  utcPtr(t.ExpiresAt),
		CreatedAt:  t.CreatedAt.UTC(),
		RevokedAt:  utcPtr(t.RevokedAt),
	}
	if t.SourceID != uuid.Nil {
		sid := t.SourceID
		dto.SourceID = &sid
	}
	return dto
}

func toAPITokenDTOs(ts []domain.APIToken) []APITokenDTO {
	out := make([]APITokenDTO, 0, len(ts))
	for _, t := range ts {
		out = append(out, toAPITokenDTO(t))
	}
	return out
}

func toAPITokenCreatedDTO(issued service.IssuedToken) APITokenCreatedDTO {
	return APITokenCreatedDTO{
		Token:  toAPITokenDTO(issued.Token),
		Secret: issued.Secret,
	}
}

// toDomain maps the login request onto the service command. It carries the
// User-Agent so the session row can record it; that value is stored for the
// "your sessions" screen and is NEVER used for authorisation.
func (r LoginRequest) toDomain(userAgent string) service.LoginCommand {
	return service.LoginCommand{
		Email:     r.Email,
		Password:  r.Password,
		UserAgent: domain.TruncateUserAgent(userAgent),
	}
}

func (r CreateTokenRequest) toDomain() service.CreateTokenCommand {
	cmd := service.CreateTokenCommand{Name: r.Name}
	if r.ExpiresAt != nil {
		t := r.ExpiresAt.UTC()
		cmd.ExpiresAt = &t
	}
	return cmd
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

// nonNilStrings renders an absent list as an empty JSON array rather than null.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
