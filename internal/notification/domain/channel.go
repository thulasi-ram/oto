package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ChannelType is the provider discriminator stored in `channels.type`
// (channels_type_ck).
//
// These two strings are the ONLY provider-specific tokens in this module, and
// they are here because they are a stored enum, not because any behaviour
// branches on them. Nothing in `notification` may ask which provider it is — the
// destination is described by its Capability bits, its Verbosity and its
// ThreadUpdates switch, and that is deliberately enough.
type ChannelType string

// The v1 channel types.
const (
	// ChannelTypeSlack is the full chat provider: threads, edit-in-place, buttons.
	ChannelTypeSlack ChannelType = "slack"
	// ChannelTypeWebhook is the generic JSON POST that proves the abstraction
	// holds. It MUST NEVER be given affordances the other one has.
	ChannelTypeWebhook ChannelType = "webhook"
)

// Valid reports whether t is one of the two.
func (t ChannelType) Valid() bool {
	return t == ChannelTypeSlack || t == ChannelTypeWebhook
}

// String renders the type as stored.
func (t ChannelType) String() string { return string(t) }

// HealthStatus mirrors `channels.health_status` (channels_hstatus_ck).
//
// The distinction between HealthDegraded and HealthAuthFailed is a PRODUCT
// FEATURE, not a taxonomy exercise: it is the difference between "the provider is
// flaky" and "your token was revoked three days ago and nobody noticed".
type HealthStatus string

// The health states.
const (
	// HealthHealthy means the last delivery worked.
	HealthHealthy HealthStatus = "healthy"
	// HealthDegraded means a permanent provider error that is not about
	// credentials: a deleted conversation, an archived channel.
	HealthDegraded HealthStatus = "degraded"
	// HealthAuthFailed means the credential is no longer usable. Never retried.
	HealthAuthFailed HealthStatus = "auth_failed"
	// HealthConfigInvalid means oto sent a payload the provider refused. This is
	// an oto bug and must be alerted on.
	HealthConfigInvalid HealthStatus = "config_invalid"
	// HealthUnknown is the initial state.
	HealthUnknown HealthStatus = "unknown"
)

// Channel is one CONFIGURED DESTINATION INSTANCE — "workspace T123,
// #sre-alerts" — not a channel type. It is the notification module's read model
// of a `channels` row: everything needed to decide what to send and how, and
// nothing needed to actually talk to the provider.
type Channel struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	Type  ChannelType
	Name  string
	// Config is the provider-specific settings blob, validated against the
	// provider's JSON Schema. This module never reads inside it.
	Config json.RawMessage
	// CredentialID names the sealed secret. This module never unseals it; that is
	// the credential store's job, behind a port.
	//
	// ⛔ IT IS NO LONGER A COLUMN ON `channels`. A channel now references a
	// connection, and the connection carries the credential — `repository`'s
	// channelColumns joins channel_connections to recover it, so this field
	// keeps the same meaning to every reader in this module without them
	// needing to know the join exists.
	CredentialID *uuid.UUID

	Capabilities Capability
	// Renderer is `channels.renderer`; "default" resolves to the provider's own.
	Renderer  string
	Verbosity Verbosity
	// ThreadUpdates false means update-in-place only: never post thread replies
	// to this destination.
	ThreadUpdates  bool
	ShowFieldEmoji bool

	Enabled      bool
	HealthStatus HealthStatus
	HealthError  string
	DeletedAt    *time.Time
}

// Live reports whether this destination may receive a delivery.
//
// A disabled or deleted channel does not make a notification vanish: it makes it
// a RECORDED suppression with `suppressed_reason = channel_disabled`, which is
// first in the §B.8.2 precedence chain precisely because it explains the absence
// of everything downstream.
func (c Channel) Live() bool { return c.Enabled && c.DeletedAt == nil }

// EffectiveVerbosity is the channel's verbosity with the schema default applied.
func (c Channel) EffectiveVerbosity() Verbosity { return c.Verbosity.Normalise() }
