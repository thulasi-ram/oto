package api

import (
	"time"

	"github.com/google/uuid"
)

// The Identity DTOs of SPEC §E.2, one per schema in `api/openapi/openapi.yaml`.
//
// DTOs LIVE HERE AND NOWHERE ELSE (CONTEXT.md §5.5). They carry `json` tags and
// `validate` tags; no DTO embeds a domain type or a row type, and the compiler
// can tell all three model sets apart.
//
// ⚠️ Every `validate` bound below is one of THREE copies of the same rule — the
// DTO tag, the domain constructor, the DDL CHECK — and R9 requires them to be
// identical. Changing one without the other two is what turns a 422 into a 500.
//
// ⛔ THERE IS NO FIELD ON ANY RESPONSE DTO THAT CARRIES A SECRET, with exactly
// one deliberate exception: APITokenCreatedDTO.Secret, which is the one-time
// disclosure the contract mandates. No password hash, no token hash and no
// session id appears in any shape in this file.

// LoginRequest is the body of `POST /api/v1/auth/login`.
//
// Password is `min=8,max=1024`. The ceiling is not a strength limit — argon2id
// hashes whatever it is given, so an unbounded password is an unbounded amount
// of server work per request.
type LoginRequest struct {
	Email    string `json:"email"    validate:"required,email,max=254"`
	Password string `json:"password" validate:"required,min=8,max=1024"`
}

// CreateTokenRequest is the body of `POST /api/v1/api-tokens`.
type CreateTokenRequest struct {
	Name string `json:"name" validate:"required,notblank,min=1,max=120"`
	// ExpiresAt is optional; omitted means the token never expires. "In the
	// future" is checked by the service against the injected clock, not by a tag:
	// a validator cannot see what time it is.
	ExpiresAt *time.Time `json:"expires_at,omitempty" validate:"omitempty"`
}

// PageQuery is the keyset page request of every Identity collection (§E.2).
//
// It is a DTO even though nothing decodes a body into it: §L.2.1 puts the bounds
// on the DTO whatever filled it in, and `httpx.BindEmpty` runs layer 1 over a
// struct assembled from the query string.
//
// ⚠️ THERE IS NO `max` ON Limit, deliberately. SPEC §E.1 caps a page at 200, and
// `httpx.Params.Limit` applies that cap SILENTLY — a caller asking for more is a
// caller who will page anyway, and a 422 there breaks a UI for no benefit.
// Putting `max=200` here as well would give the same request two different
// answers depending on which door it came through.
type PageQuery struct {
	Limit  int    `json:"limit"  validate:"omitempty,min=1"`
	Cursor string `json:"cursor" validate:"omitempty,cursor"`
}

// OrgSettingsDTO is one org's tuning of the lifecycle machine (§D.1).
//
// Durations are rendered in SECONDS and retentions in their own units, exactly
// as `orgs.settings` stores them. The domain type is durations; this is the
// boundary where they become the numbers the contract names.
type OrgSettingsDTO struct {
	RefireGraceS        int `json:"refire_grace_s"`
	ResolveGraceS       int `json:"resolve_grace_s"`
	GroupCloseDelayS    int `json:"group_close_delay_s"`
	FlapThreshold       int `json:"flap_threshold"`
	FlapWindowS         int `json:"flap_window_s"`
	FlapDigestIntervalS int `json:"flap_digest_interval_s"`
	StormThreshold      int `json:"storm_threshold"`
	StormWindowS        int `json:"storm_window_s"`
	StormCooldownS      int `json:"storm_cooldown_s"`
	RawRetentionDays    int `json:"raw_retention_days"`
	EventRetentionMonth int `json:"event_retention_months"`

	// UnackedReminderAfterS is the org default a notification policy inherits when
	// it names no delay. ZERO MEANS "no org default", which is what shipped — not
	// "immediately". ⛔ ONE STAGE, FOREVER (§G.9.1).
	UnackedReminderAfterS int `json:"unacked_reminder_after_s"`
	// DefaultVerbosity is the fallback for a Channel that names no verbosity.
	DefaultVerbosity string `json:"default_verbosity"`
	// BroadcastOnResolved is ADR 0020's one configurable broadcast, default off.
	BroadcastOnResolved bool `json:"broadcast_on_resolved"`

	// UnackedReminderMention is WHO the one unacked reminder addresses:
	// none | here | channel | list. DEFAULT `none`, because Slack documents that
	// @here and @channel do not notify from inside a thread and oto's reminder is
	// a thread reply — a default of `here` would ship a control that does nothing.
	// ⛔ NOT A ROTA (§4.8): a fixed audience, never time-aware, never a ladder.
	UnackedReminderMention string `json:"unacked_reminder_mention"`
	// UnackedReminderMentionList is the explicit audience for mode `list`: Slack
	// user (`<@U...>`) and usergroup (`<!subteam^S...>`) ids, at most ten.
	UnackedReminderMentionList []string `json:"unacked_reminder_mention_list"`
	// UnackedReminderMentionMinSeverity gates it on severity. DEFAULT `critical`.
	UnackedReminderMentionMinSeverity string `json:"unacked_reminder_mention_min_severity"`
}

// OrgDTO is the tenant boundary.
type OrgDTO struct {
	ID       uuid.UUID      `json:"id"`
	Slug     string         `json:"slug"`
	Name     string         `json:"name"`
	Settings OrgSettingsDTO `json:"settings"`
}

// UserDTO is a human principal.
//
// ⛔ There is no password field, no hash field and no last-login field, and
// there is no per-person response-time field — those are unrepresentable in the
// schema beneath this DTO, not merely omitted from it (R8).
type UserDTO struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	// SlackUserID is the linked Slack identity, null when unlinked.
	SlackUserID *string `json:"slack_user_id"`
}

// SearchDTO reports what free-text alert search can do IN THIS DEPLOYMENT.
//
// It exists because the answer is not a constant: it depends on whether the
// operator has enabled Postgres's `pg_trgm` extension, which oto itself never
// does (see internal/platform/db/capabilities.go and
// docs/runbooks/alert-search-partial-match.md). The UI reads this to decide
// whether to advertise substring matching on alert names at all, rather than
// offering a search mode that silently never matches.
type SearchDTO struct {
	// PartialMatchEnabled is true when `pg_trgm` is enabled, which lets alert
	// search find a substring INSIDE a compound alertname (e.g. "error" inside
	// `CheckoutErrorRateHigh`) that word-based full-text search can never split.
	// False means alert search still works — full-text search over alertname,
	// summary and description — just without that one gap closed.
	PartialMatchEnabled bool `json:"partial_match_enabled"`
}

// MeDTO is the current principal, its org and that org's settings.
//
// PrincipalKind is `user` for a session and `pat` for a token — the contract's
// enum, not oto's internal authn.Kind, which also names credentials this
// endpoint can never see.
type MeDTO struct {
	PrincipalKind string   `json:"principal_kind"`
	User          *UserDTO `json:"user"`
	Org           OrgDTO   `json:"org"`
	// TokenID is present when PrincipalKind is `pat`. It names the credential,
	// which is safe: an id is not a secret and cannot be presented as one.
	TokenID *uuid.UUID `json:"token_id"`
	// SessionExpiresAt lets the UI warn before it is logged out rather than
	// discovering it mid-action.
	SessionExpiresAt *time.Time `json:"session_expires_at"`
	// Search is this deployment's alert-search capabilities. Nested rather than
	// flattened for the same reason Org's Settings is: it is one coherent fact
	// about the deployment, not a property of the principal.
	Search SearchDTO `json:"search"`
}

// APITokenDTO is a bearer credential WITHOUT its secret.
//
// Prefix is the only part of the credential that appears here, and it is
// deliberately public: it is what lets an operator tell two tokens apart in a
// list, and it reveals nothing that would help present either.
type APITokenDTO struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	SourceID   *uuid.UUID `json:"source_id"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// APITokenCreatedDTO is a newly minted token WITH its secret.
//
// ⚠️ THE ONLY RESPONSE SHAPE IN OTO THAT CARRIES A CREDENTIAL. The secret is
// returned exactly once, at creation, and there is no endpoint that can show it
// again — only its sha256 is stored, so a lost token is replaced rather than
// recovered.
type APITokenCreatedDTO struct {
	Token  APITokenDTO `json:"token"`
	Secret string      `json:"secret"`
}
