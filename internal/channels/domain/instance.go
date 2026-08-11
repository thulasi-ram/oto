package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ⚠️ WHY THIS TYPE EXISTS.
//
// `Channel` in ports.go is the DELIVERY PORT — the interface a provider mints to
// move bytes to a destination. It is not the stored row. The `channels` table
// holds a CONFIGURED DESTINATION INSTANCE ("Slack workspace T123, #sre-alerts"),
// and both `channels/api` and `channels/repository` need to name it: the API to
// build its DTO, the repository to return one. Neither may import the other
// (CONTEXT.md §5.1), so the entity lives here, in the module's pure domain, which
// is the one package both are permitted to name.
//
// It is called `Instance` and not `Channel` precisely because `Channel` already
// means the port. Two things called "channel" in one package is how "Channel" and
// "channel type" became confusable in the first place (SPEC §A).

// Bounds mirrored from the `channels` CHECK constraints, so a bad value comes
// back as a field violation rather than a 23514 an operator must decode (R9).
const (
	// MaxInstanceNameLength is channels_name_ck.
	MaxInstanceNameLength = 120
	// MaxCapabilityBits is the ceiling of channels_caps_ck's non-negative bitmask.
	MaxCapabilityBits = 1<<31 - 1
)

// InstanceHealth mirrors `channels.health_status` (channels_hstatus_ck).
//
// The distinction between `degraded` and `auth_failed` is a PRODUCT FEATURE, not
// a taxonomy exercise: it is the difference between "the provider is flaky" and
// "your token was revoked three days ago and nobody noticed" (SPEC §G.6).
type InstanceHealth string

// The health states.
const (
	// InstanceHealthy means the last delivery worked.
	InstanceHealthy InstanceHealth = "healthy"
	// InstanceDegraded means a permanent provider error that is not about the
	// credential: an archived conversation, a deleted channel.
	InstanceDegraded InstanceHealth = "degraded"
	// InstanceAuthFailed means the credential is no longer usable. Never retried.
	InstanceAuthFailed InstanceHealth = "auth_failed"
	// InstanceConfigInvalid means oto sent a payload the provider refused. That is
	// an oto bug and raises a banner.
	InstanceConfigInvalid InstanceHealth = "config_invalid"
	// InstanceHealthUnknown is the initial state.
	InstanceHealthUnknown InstanceHealth = "unknown"
)

// Valid reports whether h is in the closed set.
func (h InstanceHealth) Valid() bool {
	switch h {
	case InstanceHealthy, InstanceDegraded, InstanceAuthFailed,
		InstanceConfigInvalid, InstanceHealthUnknown:
		return true
	default:
		return false
	}
}

// NeedsDetail reports whether channels_health_ck demands a `health_error`.
func (h InstanceHealth) NeedsDetail() bool {
	return h != InstanceHealthy && h != InstanceHealthUnknown
}

// Valid reports whether v is one of the four verbosity levels.
func (v Verbosity) Valid() bool {
	switch v {
	case VerbosityAll, VerbosityStatusChanges, VerbosityFiringAndResolved, VerbosityFiringOnly:
		return true
	default:
		return false
	}
}

// Normalise applies the schema default to an unset verbosity.
func (v Verbosity) Normalise() Verbosity {
	if v == "" {
		return VerbosityStatusChanges
	}
	return v
}

// Valid reports whether t is one of the two v1 provider types.
func (t Type) Valid() bool { return t == TypeSlack || t == TypeWebhook }

// Instance is one CONFIGURED DESTINATION INSTANCE — a row of `channels`.
//
// ⛔ It carries a `CredentialID` and NEVER credential material. The secret lives
// sealed in `channel_credentials` and is unsealed only at the moment a provider
// is opened, which is why no field here could hold one even by accident.
type Instance struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	Type  Type
	Name  string

	// Config is the non-secret, provider-specific settings blob. It is validated
	// against the provider's published JSON Schema on every write (§L.5) and is
	// served back verbatim.
	Config json.RawMessage

	// CredentialID names the sealed secret, or is nil. channels_cred_ck makes it
	// MANDATORY for a `slack` instance.
	CredentialID *uuid.UUID
	// CredentialKind and CredentialRotatedAt are the safe-to-show half of the
	// credential, joined in for the detail view.
	CredentialKind      string
	CredentialRotatedAt *time.Time

	Capabilities Capability
	// Renderer is `channels.renderer`; "default" resolves to the provider's own.
	Renderer RendererID
	// Verbosity gates thread REPLIES only. Root updates are never gated: an
	// operator must always be able to trust the card in front of them.
	Verbosity Verbosity
	// ThreadUpdates false means update-in-place only — never post thread replies.
	ThreadUpdates  bool
	ShowFieldEmoji bool

	Enabled         bool
	Health          InstanceHealth
	HealthError     string
	HealthCheckedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// Live reports whether this destination may receive a delivery. A disabled or
// deleted instance does not make a notification vanish — it makes it a RECORDED
// suppression with `channel_disabled`.
func (i Instance) Live() bool { return i.Enabled && i.DeletedAt == nil }

// Deleted reports whether the instance is soft deleted.
func (i Instance) Deleted() bool { return i.DeletedAt != nil }

// ToChannelConfig projects the stored instance onto the value a Provider takes
// when it opens a Channel. It is deliberately a projection and not an embed:
// `Open` must not be able to see the health columns or the credential id.
func (i Instance) ToChannelConfig() ChannelConfig {
	return ChannelConfig{
		ChannelID:      i.ID,
		OrgID:          i.OrgID,
		Name:           i.Name,
		Raw:            i.Config,
		Renderer:       i.Renderer,
		Verbosity:      i.Verbosity.Normalise(),
		ThreadUpdates:  i.ThreadUpdates,
		ShowFieldEmoji: i.ShowFieldEmoji,
	}
}

// ⚠️ WHY THE WRITE COMMANDS LIVE HERE AND NOT IN `repository`.
//
// `channels/api` declares the port it needs and `channels/repository` satisfies
// it, and neither may import the other (CONTEXT.md §5.1). The command types
// therefore have to live in the one package both are permitted to name.

// NewInstance is the create command.
//
// It is not an `Instance` because half of that struct is server-owned — ids,
// timestamps, health — and a create that accepted those would let a caller
// assert them.
type NewInstance struct {
	Type   Type
	Name   string
	Config json.RawMessage
	// CredentialID is nil when the provider needs no secret. channels_cred_ck
	// makes it MANDATORY for `slack`, and that rule is the database's.
	CredentialID *uuid.UUID
	Capabilities Capability
	Renderer     RendererID
	Verbosity    Verbosity

	ThreadUpdates  bool
	ShowFieldEmoji bool
	Enabled        bool
}

// InstancePatch is the partial update.
//
// Every field is a pointer so that "absent" and "set to the zero value" are
// different requests: a PATCH that could not tell them apart would silently
// disable a destination that only meant to be renamed.
//
// ⛔ `Type` is deliberately absent. A channel's PROVIDER IS ITS IDENTITY — a
// Slack destination reinterpreted as a webhook would carry a Slack thread
// pointer that means nothing, and oto never reads the provider back to find out
// (C9). The immutability is an absent field rather than a runtime check, because
// a field that cannot be set cannot be set by accident.
type InstancePatch struct {
	Name   *string
	Config *json.RawMessage
	// CredentialID is a double pointer: nil leaves it, a pointer to nil detaches
	// it, a pointer to a pointer attaches a new one.
	CredentialID   **uuid.UUID
	Capabilities   *Capability
	Renderer       *RendererID
	Verbosity      *Verbosity
	ThreadUpdates  *bool
	ShowFieldEmoji *bool
	Enabled        *bool
}

// IsEmpty reports whether the patch would change nothing.
func (p InstancePatch) IsEmpty() bool {
	return p.Name == nil && p.Config == nil && p.CredentialID == nil &&
		p.Capabilities == nil && p.Renderer == nil && p.Verbosity == nil &&
		p.ThreadUpdates == nil && p.ShowFieldEmoji == nil && p.Enabled == nil
}

// TestResult is the outcome of sending one synthetic alert card.
//
// The card is rendered through the SAME renderer, the same outbound validator
// and the same transport a real notification uses, so a test that passes proves
// the real path works rather than a simplified one (§L.6).
type TestResult struct {
	OK                     bool
	ProviderConversationID string
	ProviderMessageID      string
	// Permalink is a URL a human can click, and `ChannelTestDTO.permalink` is
	// `format: uri` in the contract.
	//
	//oto:reachable-ok no provider can produce one yet, and the value that used to be put here — MessageRef.ProviderKey, which is documented as opaque and is `channel:ts` for Slack and a UUID for webhook — is not a URL, so every successful channel test answered 200 with a body its own contract rejected (found by gate G2). Null is the honest answer until Slack's chat.getPermalink is called; the member stays because the contract publishes it.
	Permalink string
	// Error is the human, always-safe-to-render failure. Never a raw payload and
	// never a credential.
	Error string
	// ErrorClass is oto's own classification, never the provider's raw code.
	ErrorClass ErrorClass
	CheckedAt  time.Time
}
