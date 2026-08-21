package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// The DTOs of the Channels tag. They live in `api` and NOWHERE ELSE
// (CONTEXT.md §5.5).

// ---------------------------------------------------------------- responses

// ChannelTypeDTO is a static provider descriptor.
//
// ⛔ `ConfigSchema` is served VERBATIM and is the same bytes the server validates
// against. The settings form renders and pre-validates itself from it, which is
// why adding a provider requires a schema file and no UI code at all (§L.5).
type ChannelTypeDTO struct {
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	// ConfigSchema is JSON Schema draft 2020-12, embedded raw so no re-encoding
	// can perturb a byte of it. It describes one CHANNEL's own settings.
	ConfigSchema json.RawMessage `json:"config_schema"`
	// CredentialKinds is what a CHANNEL of this type accepts directly. v1 has
	// none that do — every credential now lives on the connection — but the
	// field stays for a future provider that needs one per destination rather
	// than one per connection.
	CredentialKinds []string `json:"credential_kinds"`
	// ConnectionConfigSchema describes the org-wide CONNECTION's settings —
	// e.g. a Slack workspace's team_id — the same schema-driven way
	// ConfigSchema does for a channel.
	ConnectionConfigSchema json.RawMessage `json:"connection_config_schema"`
	// ConnectionCredentialKinds is what a CONNECTION of this type accepts.
	ConnectionCredentialKinds []string `json:"connection_credential_kinds"`
	// Capabilities are negotiated centrally by oto's dispatcher, never asserted by
	// a provider at send time.
	Capabilities   []string `json:"capabilities"`
	Renderers      []string `json:"renderers"`
	RateLimitClass string   `json:"rate_limit_class,omitempty"`
}

// ChannelDTO is a CONFIGURED DESTINATION INSTANCE — "channel #sre-alerts under
// the Acme Slack workspace connection" — and not a channel type.
type ChannelDTO struct {
	ID   uuid.UUID `json:"id"`
	Type string    `json:"type"`
	Name string    `json:"name"`
	// Config is non-secret provider configuration, validated against this
	// provider's `config_schema`. Credentials are never part of it and never
	// appear in a response.
	Config json.RawMessage `json:"config"`

	// ConnectionID names the org-wide connection this destination opens
	// through. The connection carries the credential; nothing about it appears
	// here beyond the id.
	ConnectionID uuid.UUID `json:"connection_id"`

	Renderer  string `json:"renderer"`
	Verbosity string `json:"verbosity"`
	// ThreadUpdates false collapses every delivery mode to an in-place root
	// update.
	ThreadUpdates  bool `json:"thread_updates"`
	ShowFieldEmoji bool `json:"show_field_emoji"`
	Enabled        bool `json:"enabled"`

	HealthStatus string `json:"health_status"`
	// HealthError is non-null unless the status is healthy or unknown. A revoked
	// token surfaces here as `auth_failed` within one delivery attempt, and oto
	// stops retrying rather than hammering.
	HealthError     *string    `json:"health_error"`
	HealthCheckedAt *time.Time `json:"health_checked_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ChannelTestDTO is the result of sending one synthetic alert card.
//
// A `200` with `ok: false` means the test RAN and the provider rejected it —
// `error_class` says which kind of rejection, and `auth_expired` in particular is
// how "your token was revoked three days ago" stops being invisible.
type ChannelTestDTO struct {
	OK                     bool      `json:"ok"`
	ProviderConversationID *string   `json:"provider_conversation_id"`
	ProviderMessageID      *string   `json:"provider_message_id"`
	Permalink              *string   `json:"permalink"`
	Error                  *string   `json:"error"`
	ErrorClass             *string   `json:"error_class"`
	CheckedAt              time.Time `json:"checked_at"`
}

// ----------------------------------------------------------------- requests

// CredentialInputDTO is secret material for a destination.
//
// ⛔ WRITE-ONLY. No endpoint in this API ever returns it and nothing logs it.
type CredentialInputDTO struct {
	Kind string `json:"kind" validate:"required,oneof=slack_bot_token slack_app_token slack_signing_secret basic bearer webhook_signing_secret none"`
	// Values is sealed with AES-256-GCM before it touches disk.
	Values map[string]string `json:"values,omitempty" validate:"omitempty,max=8,dive,max=4096"`
}

// CreateChannelRequest creates a destination instance under a connection.
//
// `config` is deliberately `json.RawMessage` and carries NO validator tag: it is
// validated against the PROVIDER'S PUBLISHED JSON SCHEMA (§L.5), which is the one
// source of truth the settings form was generated from. A second set of Go tags
// here would be a second copy of those rules, and the two would drift.
//
// ⛔ THERE IS NO `credential` FIELD HERE ANY MORE. A destination no longer
// carries its own secret — `connection_id` names the org-wide connection that
// does, and `createChannel` checks that connection's type matches `type`
// (no CHECK constraint can see across the two tables).
type CreateChannelRequest struct {
	Type         string          `json:"type"          validate:"required,oneof=slack webhook"`
	Name         string          `json:"name"          validate:"required,notblank,min=1,max=120"`
	Config       json.RawMessage `json:"config"        validate:"required"`
	ConnectionID uuid.UUID       `json:"connection_id" validate:"required"`

	Renderer  *string `json:"renderer,omitempty"  validate:"omitempty,oneof=default slack.default webhook.json"`
	Verbosity *string `json:"verbosity,omitempty" validate:"omitempty,oneof=all status_changes firing_and_resolved firing_only"`

	ThreadUpdates  *bool `json:"thread_updates,omitempty"`
	ShowFieldEmoji *bool `json:"show_field_emoji,omitempty"`
	Enabled        *bool `json:"enabled,omitempty"`
}

// UpdateChannelRequest is the partial update.
//
// `type` is absent because a channel's PROVIDER IS ITS IDENTITY. Supplying
// `config` replaces it wholesale and re-validates against the provider schema;
// supplying `connection_id` re-points the destination at a different
// connection, and `updateChannel` re-checks the type match.
type UpdateChannelRequest struct {
	Name         *string          `json:"name,omitempty"   validate:"omitempty,notblank,min=1,max=120"`
	Config       *json.RawMessage `json:"config,omitempty"`
	ConnectionID *uuid.UUID       `json:"connection_id,omitempty"`

	Renderer  *string `json:"renderer,omitempty"  validate:"omitempty,oneof=default slack.default webhook.json"`
	Verbosity *string `json:"verbosity,omitempty" validate:"omitempty,oneof=all status_changes firing_and_resolved firing_only"`

	ThreadUpdates  *bool `json:"thread_updates,omitempty"`
	ShowFieldEmoji *bool `json:"show_field_emoji,omitempty"`
	Enabled        *bool `json:"enabled,omitempty"`
}

// IsEmpty reports whether the request asks for nothing. The contract marks the
// body `minProperties: 1`.
func (r UpdateChannelRequest) IsEmpty() bool {
	return r.Name == nil && r.Config == nil && r.ConnectionID == nil && r.Renderer == nil &&
		r.Verbosity == nil && r.ThreadUpdates == nil && r.ShowFieldEmoji == nil && r.Enabled == nil
}

// ------------------------------------------------------------- connections

// ChannelConnectionDTO is one org-wide provider setup — a Slack workspace's
// bot token, or a webhook receiver family's shared credential.
type ChannelConnectionDTO struct {
	ID   uuid.UUID `json:"id"`
	Type string    `json:"type"`
	Name string    `json:"name"`
	// Config is non-secret, connection-level configuration — e.g. a Slack
	// workspace's team_id.
	Config json.RawMessage `json:"config"`

	// CredentialKind and CredentialRotatedAt are the ONLY things this API ever
	// says about a secret: which kind is attached, and when it was last rotated.
	CredentialKind      *string    `json:"credential_kind"`
	CredentialRotatedAt *time.Time `json:"credential_rotated_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateChannelConnectionRequest creates a connection.
type CreateChannelConnectionRequest struct {
	Type   string          `json:"type"   validate:"required,oneof=slack webhook"`
	Name   string          `json:"name"   validate:"required,notblank,min=1,max=120"`
	Config json.RawMessage `json:"config" validate:"required"`

	Credential *CredentialInputDTO `json:"credential,omitempty"`
}

// UpdateChannelConnectionRequest is the partial update. `type` is absent for
// the same reason it is on UpdateChannelRequest: a connection's provider is
// its identity.
type UpdateChannelConnectionRequest struct {
	Name   *string          `json:"name,omitempty"`
	Config *json.RawMessage `json:"config,omitempty"`

	Credential *CredentialInputDTO `json:"credential,omitempty"`
}

// IsEmpty reports whether the request asks for nothing.
func (r UpdateChannelConnectionRequest) IsEmpty() bool {
	return r.Name == nil && r.Config == nil && r.Credential == nil
}

// ResolveConversationRequest asks "what is the other half of this Slack
// channel" — exactly one of Name or ConversationID is expected.
type ResolveConversationRequest struct {
	Name           *string `json:"name,omitempty"`
	ConversationID *string `json:"conversation_id,omitempty"`
}

// ResolveConversationDTO is the answer: both halves, filled in.
type ResolveConversationDTO struct {
	ConversationID   string `json:"conversation_id"`
	ConversationName string `json:"conversation_name"`
}

// ---------------------------------------------------------------- wordings

// MatcherDTO is one term of a Wording's `when` clause.
//
// ⚠️ IT IS A DELIBERATE TWIN OF notification/api.MatcherDTO, NOT AN IMPORT, for
// the same reason `channels/domain.Matcher` twins the notification domain's: this
// module does not depend on that one. Both marshal to the ONE `MatcherDTO`
// schema, and gate G1 checks both against it, which is what stops the copies
// drifting from each other.
type MatcherDTO struct {
	Name  string `json:"name"  validate:"required,labelname,max=1024"`
	Op    string `json:"op"    validate:"required,matcherop"`
	Value string `json:"value" validate:"max=4096"`
}

// WordingDTO is one customer-authored template for one Stanza (ADR 0037).
//
// ⛔ IT CHOOSES WORDS AND NOTHING ELSE. There is no colour, no block, no
// destination and no mention on this shape, and there never will be: structure
// stays oto's, which is what makes it impossible for a Wording to mark a delivery
// dead.
type WordingDTO struct {
	ID uuid.UUID `json:"id"`
	// ChannelID is null for the org-wide house voice. A Wording naming one
	// destination is more specific and WINS over an org-wide one (ADR 0049).
	ChannelID *uuid.UUID `json:"channel_id"`
	Stanza    string     `json:"stanza"`
	Template  string     `json:"template"`
	// Matchers and Reasons are the `when` clause. An empty clause matches
	// everything, which is what makes a single org-wide row the natural way to set
	// a house voice.
	Matchers []MatcherDTO `json:"matchers"`
	Reasons  []string     `json:"reasons"`
	Priority int          `json:"priority"`
	Enabled  bool         `json:"enabled"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// DeletedAt is non-null only on a row `include_deleted` asked for. The delete
	// is soft because a delivery's persisted wording set names the rows that
	// produced a card, and a hard delete would make an old card unexplainable.
	DeletedAt *time.Time `json:"deleted_at"`
}

// WordingProblemDTO is one reason a template was refused, in words meant for the
// person who typed it.
//
// ⭐ IT IS THE SAME field/code/message TRIPLE A `422` CARRIES IN `violations`,
// deliberately. The preview endpoint answers `200` with the problems listed
// rather than refusing, so the same client control that highlights a save failure
// highlights a preview failure without a second renderer.
type WordingProblemDTO struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WordingSpellingDTO is one fixture's text as ONE provider writes it.
//
// ⭐⭐ SEEING TWO OF THESE SIDE BY SIDE IS THE WHOLE AUTHORING LESSON (ADR 0048).
// `{{ service | code }}` produces neither a backtick nor a visible mark — it
// produces a neutral mark that only a Dialect resolves — so an author who is
// shown one spelling learns that markup is theirs to write, and an author shown
// both learns that it is not.
type WordingSpellingDTO struct {
	Dialect string `json:"dialect"`
	Text    string `json:"text"`
	// Error is the render failure for this fixture, in which case a real delivery
	// would fall back to oto's own Go text for the Stanza rather than die.
	Error *string `json:"error"`
}

// WordingRenderingDTO is one fixture of the shipped corpus, spelled by every
// Dialect.
type WordingRenderingDTO struct {
	Fixture string `json:"fixture"`
	// Representative marks the ORDINARY cards. A template that renders empty on
	// one of these is refused at save time; rendering empty on a hostile fixture
	// is expected and degrades one Stanza at delivery.
	Representative bool                 `json:"representative"`
	Spellings      []WordingSpellingDTO `json:"spellings"`
}

// WordingPreviewDTO is what a candidate template would say, and why it would be
// refused, in one round trip.
type WordingPreviewDTO struct {
	Stanza string `json:"stanza"`
	// Template is the SANITISED source, not the bytes the caller sent. `sanitise`
	// strips the private-use area, the other-format codepoints where the bidi
	// overrides live, and the control characters — invisibly, because forging one
	// of oto's own marks is how a customer would otherwise get raw markup past the
	// sink. Echoing the result is the only way an author ever learns something was
	// removed.
	Template string              `json:"template"`
	Problems []WordingProblemDTO `json:"problems"`
	// Renderings is empty when the template did not compile: there is nothing to
	// show, and `problems` says why.
	Renderings []WordingRenderingDTO `json:"renderings"`
}

// CreateWordingRequest creates one Wording.
//
// ⛔ `stanza` CARRIES NO `oneof` TAG, AND THAT IS THE POINT. All eight SPEC §H.7
// names are accepted by the binder so that the four which take no Wording are
// refused by `service.ValidateWording` with the SENTENCE saying which kind of
// structure they are. A generic `must be one of: …` would teach nobody why
// `fields` is not on the list.
type CreateWordingRequest struct {
	// ChannelID absent or null is the org-wide house voice.
	ChannelID *uuid.UUID `json:"channel_id,omitempty"`
	Stanza    string     `json:"stanza"   validate:"required,max=32"`
	// Template carries no `max` tag for the same reason `stanza` carries no
	// `oneof`: the domain's own bound answers with "a wording is one line of
	// prose, and the limit is 2048 bytes", which names the rule rather than the
	// number.
	Template string       `json:"template" validate:"required"`
	Matchers []MatcherDTO `json:"matchers,omitempty" validate:"omitempty,max=32,dive"`
	Reasons  []string     `json:"reasons,omitempty"  validate:"omitempty,max=32,unique,dive,max=64"`
	Priority *int         `json:"priority,omitempty" validate:"omitempty,min=0,max=100000"`
	Enabled  *bool        `json:"enabled,omitempty"`
}

// UpdateWordingRequest is the partial update.
//
// ⛔ `stanza` IS ABSENT AND IS NOT AN OVERSIGHT. Moving a Wording from `body` to
// `title` is not an edit of that Wording, it is a different Wording — the read
// set differs, the budget differs, and the row's history would claim it had
// always been the new one. Delete and create.
//
// ⛔ `channel_id` IS ABSENT FOR THE SAME KIND OF REASON. Re-binding an org-wide
// house voice to one destination silently changes which cards it wins on, and the
// resolution order is the one thing an operator must be able to read off the row.
type UpdateWordingRequest struct {
	Template *string       `json:"template,omitempty"`
	Matchers *[]MatcherDTO `json:"matchers,omitempty" validate:"omitempty,max=32,dive"`
	Reasons  *[]string     `json:"reasons,omitempty"  validate:"omitempty,max=32,unique,dive,max=64"`
	Priority *int          `json:"priority,omitempty" validate:"omitempty,min=0,max=100000"`
	Enabled  *bool         `json:"enabled,omitempty"`
}

// IsEmpty reports whether the request asks for nothing. The contract marks the
// body `minProperties: 1`.
func (r UpdateWordingRequest) IsEmpty() bool {
	return r.Template == nil && r.Matchers == nil && r.Reasons == nil &&
		r.Priority == nil && r.Enabled == nil
}

// PreviewWordingRequest asks what a template would say, without saving anything.
//
// It carries no `when` clause: selection decides WHICH card a Wording writes on,
// and the preview is about WHAT it writes. Matchers would change nothing in the
// answer and would invite reading the fixture corpus as a routing rehearsal.
type PreviewWordingRequest struct {
	Stanza   string `json:"stanza"   validate:"required,max=32"`
	Template string `json:"template" validate:"required"`
}
