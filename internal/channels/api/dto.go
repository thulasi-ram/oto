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
	// can perturb a byte of it.
	ConfigSchema json.RawMessage `json:"config_schema"`
	// CredentialKinds is what this provider accepts. Secrets are write-only
	// everywhere in this API, so this names kinds and never values.
	CredentialKinds []string `json:"credential_kinds"`
	// Capabilities are negotiated centrally by oto's dispatcher, never asserted by
	// a provider at send time.
	Capabilities   []string `json:"capabilities"`
	Renderers      []string `json:"renderers"`
	RateLimitClass string   `json:"rate_limit_class,omitempty"`
}

// ChannelDTO is a CONFIGURED DESTINATION INSTANCE — "Slack workspace T123,
// channel #sre-alerts" — and not a channel type.
type ChannelDTO struct {
	ID   uuid.UUID `json:"id"`
	Type string    `json:"type"`
	Name string    `json:"name"`
	// Config is non-secret provider configuration, validated against this
	// provider's `config_schema`. Credentials are never part of it and never
	// appear in a response.
	Config json.RawMessage `json:"config"`

	// CredentialKind and CredentialRotatedAt are the ONLY things this API ever
	// says about a secret: which kind is attached, and when it was last rotated.
	CredentialKind      *string    `json:"credential_kind"`
	CredentialRotatedAt *time.Time `json:"credential_rotated_at"`

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
	Kind string `json:"kind" validate:"required,oneof=slack_bot_token slack_app_token slack_signing_secret basic bearer none"`
	// Values is sealed with AES-256-GCM before it touches disk.
	Values map[string]string `json:"values,omitempty" validate:"omitempty,max=8,dive,max=4096"`
}

// CreateChannelRequest creates a destination instance.
//
// `config` is deliberately `json.RawMessage` and carries NO validator tag: it is
// validated against the PROVIDER'S PUBLISHED JSON SCHEMA (§L.5), which is the one
// source of truth the settings form was generated from. A second set of Go tags
// here would be a second copy of those rules, and the two would drift.
type CreateChannelRequest struct {
	Type   string          `json:"type"   validate:"required,oneof=slack webhook"`
	Name   string          `json:"name"   validate:"required,notblank,min=1,max=120"`
	Config json.RawMessage `json:"config" validate:"required"`

	Credential *CredentialInputDTO `json:"credential,omitempty"`

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
// supplying `credential` rotates the secret.
type UpdateChannelRequest struct {
	Name   *string          `json:"name,omitempty"   validate:"omitempty,notblank,min=1,max=120"`
	Config *json.RawMessage `json:"config,omitempty"`

	Credential *CredentialInputDTO `json:"credential,omitempty"`

	Renderer  *string `json:"renderer,omitempty"  validate:"omitempty,oneof=default slack.default webhook.json"`
	Verbosity *string `json:"verbosity,omitempty" validate:"omitempty,oneof=all status_changes firing_and_resolved firing_only"`

	ThreadUpdates  *bool `json:"thread_updates,omitempty"`
	ShowFieldEmoji *bool `json:"show_field_emoji,omitempty"`
	Enabled        *bool `json:"enabled,omitempty"`
}

// IsEmpty reports whether the request asks for nothing. The contract marks the
// body `minProperties: 1`.
func (r UpdateChannelRequest) IsEmpty() bool {
	return r.Name == nil && r.Config == nil && r.Credential == nil && r.Renderer == nil &&
		r.Verbosity == nil && r.ThreadUpdates == nil && r.ShowFieldEmoji == nil && r.Enabled == nil
}
