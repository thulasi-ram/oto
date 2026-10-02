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

	// The signing slot (migration 00088), said about the same way: whether one is
	// attached, when it was last rotated, and until when the secret that rotation
	// replaced keeps signing beside it. Never the secret.
	SigningCredentialKind      *string    `json:"signing_credential_kind"`
	SigningCredentialRotatedAt *time.Time `json:"signing_credential_rotated_at"`
	SigningOverlapUntil        *time.Time `json:"signing_overlap_until"`

	// PayloadMapping is a webhook connection's payload mapping (ADR 0055 §2), or
	// null when it sends the plain envelope. It holds no secret — a secret is named
	// in it as `secrets.<name>` — so it is returned whole, and is what the catalog
	// exports.
	PayloadMapping json.RawMessage `json:"payload_mapping"`
	// MappingSecretNames are the names the connection seals for its mapping, and
	// MappingSecretsRotatedAt when they were last replaced. Never a value.
	MappingSecretNames      []string   `json:"mapping_secret_names"`
	MappingSecretsRotatedAt *time.Time `json:"mapping_secrets_rotated_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateChannelConnectionRequest creates a connection.
type CreateChannelConnectionRequest struct {
	Type   string          `json:"type"   validate:"required,oneof=slack webhook"`
	Name   string          `json:"name"   validate:"required,notblank,min=1,max=120"`
	Config json.RawMessage `json:"config" validate:"required"`

	Credential *CredentialInputDTO `json:"credential,omitempty"`
	// SigningCredential is a webhook connection's outbound signing secret, kind
	// `webhook_signing_secret`, beside — never instead of — `credential`.
	SigningCredential *CredentialInputDTO `json:"signing_credential,omitempty"`

	// PayloadMapping is a webhook connection's payload mapping (ADR 0055 §2). It is
	// rendered against an envelope for every fact before it is stored, and refused
	// with the failing fact named.
	PayloadMapping json.RawMessage `json:"payload_mapping,omitempty"`
	// MappingSecrets are the secrets the mapping references, name → value, sealed
	// into one credential. ⛔ WRITE-ONLY, like every credential: only the names come
	// back.
	MappingSecrets map[string]string `json:"mapping_secrets,omitempty" validate:"omitempty,max=16"`
}

// UpdateChannelConnectionRequest is the partial update. `type` is absent for
// the same reason it is on UpdateChannelRequest: a connection's provider is
// its identity.
type UpdateChannelConnectionRequest struct {
	Name   *string          `json:"name,omitempty"`
	Config *json.RawMessage `json:"config,omitempty"`

	Credential *CredentialInputDTO `json:"credential,omitempty"`
	// SigningCredential rotates the signing secret, keeping the old one signing
	// beside it for domain.SigningSecretOverlap; kind `none` detaches it.
	SigningCredential *CredentialInputDTO `json:"signing_credential,omitempty"`

	// PayloadMapping replaces the mapping; JSON `null` removes it, and the connection
	// sends the plain envelope from the next claim on.
	PayloadMapping json.RawMessage `json:"payload_mapping,omitempty"`
	// MappingSecrets REPLACES the whole set of mapping secrets — nothing outside the
	// send path can unseal the current ones to add to — and `{}` removes them.
	MappingSecrets map[string]string `json:"mapping_secrets,omitempty" validate:"omitempty,max=16"`
}

// IsEmpty reports whether the request asks for nothing.
func (r UpdateChannelConnectionRequest) IsEmpty() bool {
	return r.Name == nil && r.Config == nil && r.Credential == nil && r.SigningCredential == nil &&
		len(r.PayloadMapping) == 0 && r.MappingSecrets == nil
}

// PayloadMappingCatalogEntryDTO is one file of the payload-mapping catalog (ADR
// 0055 §2): a tool's mapping, the docs its field names were checked against, and
// what an importer must do. Importing COPIES `mapping` into a webhook connection's
// own `payload_mapping`; nothing links the two afterwards.
type PayloadMappingCatalogEntryDTO struct {
	ID        string   `json:"id"`
	Vendor    string   `json:"vendor"`
	Title     string   `json:"title"`
	Summary   string   `json:"summary"`
	Docs      []string `json:"docs"`
	CheckedOn string   `json:"checked_on"`
	Setup     []string `json:"setup"`
	// Commands are the tool's command fields and the values the catalog never sends
	// in them (ADR 0055 §4) — what the import tells the operator they are NOT getting.
	Commands []PayloadMappingCommandDTO `json:"commands"`
	// Choices are what the import must ask before it copies `mapping`: each is
	// written into the copy as the literal the operator picks, in place of its
	// `<<choose:<name>>>` placeholder. A copy that still holds one is refused at save.
	Choices []PayloadMappingChoiceDTO `json:"choices"`
	// Secrets are the mapping secrets the connection must seal before the copy can
	// be saved, by name. Never a value: a catalog file holds none.
	Secrets []string        `json:"secrets"`
	Mapping json.RawMessage `json:"mapping"`
}

// PayloadMappingCommandDTO is one command field of a tool's request body and the
// values of it that would turn a fact into a command.
type PayloadMappingCommandDTO struct {
	Field     string   `json:"field"`
	Forbidden []string `json:"forbidden"`
}

// PayloadMappingChoiceDTO is one value a catalog entry leaves to the operator.
type PayloadMappingChoiceDTO struct {
	Name     string   `json:"name"`
	Question string   `json:"question"`
	Field    string   `json:"field"`
	Options  []string `json:"options"`
}

// TestConnectionMappingRequest asks for one fact to be sent through a mapped
// connection, by way of one of its channels — the connection holds the mapping and
// the channel holds the URL.
type TestConnectionMappingRequest struct {
	ChannelID uuid.UUID `json:"channel_id" validate:"required"`
	Fact      string    `json:"fact"       validate:"required,max=32"`
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

// -------------------------------------------------------------- templates

// NotificationTemplateDTO is one whole message an operator wrote.
//
// ⭐ IT CARRIES NO `when` CLAUSE, AND THAT ABSENCE IS THE DESIGN. Selection lives
// on the NotificationPolicy, which already has matchers, already has reasons and
// already chose the destinations — so there is one routing decision to read and
// one precedence rule to hold in your head.
type NotificationTemplateDTO struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Provider is the destination kind this was WRITTEN FOR. It is declared intent,
	// not an enforced constraint: `card` and `text` render anywhere, `raw` is
	// Slack-only and degrades to oto's built-in card elsewhere.
	Provider string `json:"provider"`
	Format   string `json:"format"`
	Source   string `json:"source"`
	// ReplySource is the body thread replies are rendered from, in `format`. null
	// means oto's own replies (ADR 0051).
	ReplySource *string `json:"reply_source"`
	// Version increments only when `source`, `reply_source` or `format` changes, because it exists
	// to attribute a rendered card to a revision — and a rename produced no
	// different bytes.
	Version int  `json:"version"`
	Enabled bool `json:"enabled"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// TemplateProblemDTO is one reason a template was refused, or one warning about a
// template that was accepted.
//
// ⚠️ `kind: "warning"` DOES NOT REFUSE THE SAVE. Today there is exactly one: a
// card with no `{{ actions }}` carries no Acknowledge button. That is the owner's
// choice to make and oto does not overrule it — an alert stays acknowledgeable
// from the console and from `POST /api/v1/cases/{id}/ack` — but nobody should
// discover it in production, so it is said at the moment somebody can still change
// their mind.
type TemplateProblemDTO struct {
	Kind    string `json:"kind"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
	// Fixture names the example card this problem appeared on, when it appeared on
	// one. A template that works on a rich firing notification and breaks on a
	// resolved one with no rule has a problem worth naming precisely.
	Fixture string `json:"fixture,omitempty"`
}

// TemplateSpellingDTO is one example card as ONE provider writes it.
type TemplateSpellingDTO struct {
	Dialect string `json:"dialect"`
	Text    string `json:"text"`
	Error   string `json:"error,omitempty"`
}

// TemplateRenderingDTO is one fixture of the shipped corpus, spelled by every
// provider oto knows.
type TemplateRenderingDTO struct {
	Fixture        string `json:"fixture"`
	Representative bool   `json:"representative"`
	// HasActions records whether this rendering placed the button row, so the
	// editor can show the warning against the example that demonstrates it.
	HasActions bool                  `json:"has_actions"`
	Spellings  []TemplateSpellingDTO `json:"spellings"`
}

// TemplatePreviewDTO is what a candidate template would say, and why it would be
// refused.
//
// ⭐⭐ SHOWING TWO SPELLINGS SIDE BY SIDE IS THIS ENDPOINT'S WHOLE TEACHING JOB.
// One Markdown document compiles to Slack's `*bold*` and to a webhook's plain
// words, and an author shown only one concludes that markup is theirs to write. It
// is also the only way the portability claim is visible rather than asserted.
type TemplatePreviewDTO struct {
	Format string `json:"format"`
	Source string `json:"source"`
	// Problems is empty when the template is accepted, and may hold warnings even
	// then. Read `kind` before treating any of it as a refusal.
	Problems []TemplateProblemDTO `json:"problems"`
	// Renderings is empty when the template does not compile; `problems` says why.
	Renderings []TemplateRenderingDTO `json:"renderings"`
	// ReplyRenderings is the reply body against one example per reason, each
	// fixture named for its reason. A spelling with no text and no error is "this
	// body says nothing here, so oto's own reply". Empty when no reply body was sent.
	ReplyRenderings []TemplateRenderingDTO `json:"reply_renderings"`
}

// CreateNotificationTemplateRequest creates one template.
type CreateNotificationTemplateRequest struct {
	Name     string `json:"name"     validate:"required,max=120"`
	Provider string `json:"provider" validate:"required,max=32"`
	Format   string `json:"format"   validate:"required,oneof=card text raw"`
	Source   string `json:"source"   validate:"required,max=16384"`
	// ReplySource is optional; absent or "" means oto's own replies.
	ReplySource *string `json:"reply_source,omitempty" validate:"omitempty,max=16384"`
	Enabled     *bool   `json:"enabled,omitempty"`
}

// UpdateNotificationTemplateRequest patches one template. A nil field is untouched.
type UpdateNotificationTemplateRequest struct {
	Name     *string `json:"name,omitempty"     validate:"omitempty,max=120"`
	Provider *string `json:"provider,omitempty" validate:"omitempty,max=32"`
	Format   *string `json:"format,omitempty"   validate:"omitempty,oneof=card text raw"`
	Source   *string `json:"source,omitempty"   validate:"omitempty,max=16384"`
	// ReplySource set to "" clears the reply body, back to oto's own replies.
	ReplySource *string `json:"reply_source,omitempty" validate:"omitempty,max=16384"`
	Enabled     *bool   `json:"enabled,omitempty"`
}

// IsEmpty reports the "you asked for nothing" case, which the schema enforces as
// body `minProperties: 1`.
func (r UpdateNotificationTemplateRequest) IsEmpty() bool {
	return r.Name == nil && r.Provider == nil && r.Format == nil &&
		r.Source == nil && r.ReplySource == nil && r.Enabled == nil
}

// PreviewNotificationTemplateRequest asks what a template would say, without
// saving anything.
type PreviewNotificationTemplateRequest struct {
	Format string `json:"format" validate:"required,oneof=card text raw"`
	Source string `json:"source" validate:"required,max=16384"`
	// ReplySource, when present, is previewed against one example per reason.
	ReplySource *string `json:"reply_source,omitempty" validate:"omitempty,max=16384"`
}
