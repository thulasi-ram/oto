package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Type is the provider discriminator. v1 ships exactly two (R5): Slack in full,
// and a trivial generic webhook whose only job is to prove the abstraction holds.
// The webhook provider MUST NOT be given Slack-specific affordances.
type Type string

// The v1 provider types.
const (
	// TypeSlack is the full Slack provider: Block Kit, threads, interactivity.
	TypeSlack Type = "slack"
	// TypeWebhook is the generic JSON POST provider.
	TypeWebhook Type = "webhook"
)

// Capability is a bitset of what a Channel can do. It is negotiated centrally by
// the dispatch service, never by a provider: the classification drives behaviour,
// so a provider cannot quietly opt itself out of a rule.
type Capability uint32

// The capability bits.
const (
	// CapThreading means replies attach to a parent message.
	CapThreading Capability = 1 << iota
	// CapAmend means an already-sent message can be edited in place. This is what
	// makes `chat.update` the primary mechanism rather than reposting (C8).
	CapAmend
	// CapRichLayout means structured blocks, not just text.
	CapRichLayout
	// CapInteractive means buttons that call back into oto.
	CapInteractive
	// CapBroadcast means a thread reply can be surfaced in-channel.
	CapBroadcast
	// CapDedupeKey means the provider does its own dedupe.
	CapDedupeKey
)

// RendererID names a Renderer in the registry.
type RendererID string

// The v1 renderers.
const (
	// RendererSlackDefault renders the Slack Block Kit card.
	RendererSlackDefault RendererID = "slack.default"
	// RendererWebhookJSON renders the generic `oto.notification.v1` envelope.
	RendererWebhookJSON RendererID = "webhook.json"
)

// Descriptor is static per Provider and is served by GET /api/v1/channel-types.
// Its ConfigSchema is the ONE source of truth for a provider's configuration: the
// same bytes validate on the server and render the settings form in the UI, so
// adding a provider changes no UI code (§L.5).
type Descriptor struct {
	Type            Type
	DisplayName     string
	ConfigSchema    json.RawMessage // JSON Schema draft 2020-12; drives the dynamic settings UI
	CredentialKinds []string
	Capabilities    Capability
	Renderers       []RendererID
	RateLimitClass  string // "slack" | "none"
}

// ChannelConfig is the validated, non-secret configuration of one Channel
// instance — "Slack workspace T123, channel #sre-alerts". A Channel is a
// configured DESTINATION, not a channel type.
type ChannelConfig struct {
	ChannelID uuid.UUID
	OrgID     uuid.UUID
	Name      string
	Raw       json.RawMessage
	Renderer  RendererID
	Verbosity Verbosity

	ThreadUpdates  bool
	ShowFieldEmoji bool
}

// Verbosity decides which thread replies a Channel receives (§H.6). Root updates
// are NEVER gated by verbosity: an operator must always be able to trust that the
// card in front of them is current.
type Verbosity string

// The verbosity levels.
const (
	// VerbosityAll delivers every reply type.
	VerbosityAll Verbosity = "all"
	// VerbosityStatusChanges is the default: acks, suppression, expiry, re-fires,
	// new alerts, all-resolved, rule changes, comments, escalation and storms.
	VerbosityStatusChanges Verbosity = "status_changes"
	// VerbosityFiringAndResolved drops the human-action replies.
	VerbosityFiringAndResolved Verbosity = "firing_and_resolved"
	// VerbosityFiringOnly is the quietest setting.
	VerbosityFiringOnly Verbosity = "firing_only"
)

// Credential is an already-unsealed secret. It NEVER crosses an API boundary and
// is never rendered, logged or persisted outside the sealed credential store.
type Credential struct {
	Kind   string
	Values map[string]string
}

// Provider is registered once at boot and mints Channels from stored config.
type Provider interface {
	Descriptor() Descriptor
	ValidateConfig(ctx context.Context, raw json.RawMessage) error
	Open(ctx context.Context, cfg ChannelConfig, cred Credential) (Channel, error)
}

// Channel is the delivery port. It knows NOTHING about alerts: it moves rendered
// bytes to a destination and reports what the destination said.
type Channel interface {
	Capabilities() Capability
	Deliver(ctx context.Context, req DeliverRequest) (DeliverResult, error)
	Amend(ctx context.Context, ref MessageRef, msg RenderedMessage) (DeliverResult, error)
	Probe(ctx context.Context) error
	Close() error
}

// Mode is the delivery decision produced by the §H.6 table.
type Mode string

// The delivery modes.
const (
	// ModePostRoot posts a new root message. It happens once per AlertGroup
	// generation and is the only genuinely at-risk operation on recovery (§G.5).
	ModePostRoot Mode = "post_root"
	// ModeUpdateRoot amends the root in place. This is the PRIMARY mechanism:
	// `repeat interval elapsed` is update-only, never a repost (C8).
	ModeUpdateRoot Mode = "update_root"
	// ModeThreadReply appends a reply to the group's thread. Replies are the
	// exception, not the rule.
	ModeThreadReply Mode = "thread_reply"
	// ModeBroadcastReply surfaces a thread reply in-channel (ADR 0020). It is for
	// the transitions an on-call engineer would be angry to have missed, and it is
	// IRREVERSIBLE: Slack documents nothing that un-broadcasts. Never an
	// escalation — oto has one reminder stage and it reminds a CHANNEL (§G.9.1).
	ModeBroadcastReply Mode = "broadcast_reply"
)

// DeliverRequest is one delivery attempt on one Channel.
type DeliverRequest struct {
	Message    RenderedMessage
	Mode       Mode
	ReplyTo    *MessageRef // required for thread_reply / broadcast_reply
	Target     *MessageRef // required for update_root
	DeliveryID uuid.UUID
	DedupeKey  string // used only if CapDedupeKey
}

// MessageRef is a FOREIGN SYSTEM'S PRIMARY KEY. It is immutable and never
// reshaped. oto's database is the memory of Slack; oto never reads Slack back to
// reconstruct its own state (C9).
type MessageRef struct {
	ConversationID string // Slack channel id, taken from the API RESPONSE, not the request
	MessageID      string // Slack ts. ALWAYS A STRING. NEVER A FLOAT.
	ThreadID       string // Slack root ts
	// ProviderKey is the provider's own composite handle — `channel:ts` for
	// Slack, the delivery id for the generic webhook.
	//
	//oto:reachable-ok its only reader used to be Tester, which published it as ChannelTestDTO.permalink; the contract declares that member `format: uri` and this value never is one, so gate G2 rejected every successful channel test. The field stays because a provider must be able to round-trip its own key to amend a message, and ConversationID/MessageID are what oto stores.
	ProviderKey string
}

// DeliverResult is what the destination said. Its Ref is the durable handle oto
// stores; its Raw is kept for debugging a provider that misbehaves.
type DeliverResult struct {
	Ref         MessageRef
	DeliveredAt time.Time
	Raw         json.RawMessage
}

// RenderedMessage is provider-native bytes plus the two strings every provider
// needs. It is persisted BEFORE the network call, so a crash mid-send leaves
// evidence of exactly what was sent (C11, §G.7).
type RenderedMessage struct {
	// Fallback is the top-level plain text: the push notification, the search
	// snippet, and the ONLY thing a screen reader reads. It is a complete sentence.
	Fallback string
	Summary  string          // short preview
	Payload  json.RawMessage // channel-native (Slack: {text,attachments,unfurl_*})
	Hash     string          // sha256 of Payload; skips no-op updates
	Metadata map[string]string
}

// ErrorClass drives retry policy (§G.6). THE CLASSIFICATION drives retry, never
// the provider: `config_invalid` and `auth_expired` never retry, because "your
// token was revoked three days ago and nobody noticed" is a product feature.
type ErrorClass string

// The error classes.
const (
	// ClassRetryable is a transient failure: exponential backoff, 12 attempts.
	ClassRetryable ErrorClass = "retryable"
	// ClassRateLimited honours the provider's Retry-After exactly.
	ClassRateLimited ErrorClass = "rate_limited"
	// ClassPermanent is dead immediately, unless it is a thread-pointer error (§H.9).
	ClassPermanent ErrorClass = "permanent"
	// ClassConfigInvalid is dead, and raises a UI banner on the Channel.
	ClassConfigInvalid ErrorClass = "config_invalid"
	// ClassAuthExpired is dead, and raises a UI banner on the Channel.
	ClassAuthExpired ErrorClass = "auth_expired"
)

// Error is a provider failure, classified. The Code is the provider's own error
// string, verbatim ("ratelimited", "channel_not_found"), so that a support
// question can be answered without a packet capture.
type Error struct {
	Class      ErrorClass
	RetryAfter time.Duration
	Provider   string
	Code       string
	Cause      error
}

// Error implements the error interface.
func (e *Error) Error() string { return e.Provider + ": " + e.Code }

// Unwrap exposes the underlying provider error.
func (e *Error) Unwrap() error { return e.Cause }

// Renderer is a PURE FUNCTION from a NotificationView to provider-native bytes.
// No I/O: a renderer must never query, because it runs at claim time inside a
// delivery's budget and is golden-file tested.
type Renderer interface {
	ID() RendererID
	Supports(Capability) bool
	Render(ctx context.Context, v *NotificationView, o RenderOptions) (RenderedMessage, error)
}

// RenderOptions is the per-delivery rendering context.
type RenderOptions struct {
	Mode           Mode
	Verbosity      Verbosity
	ShowFieldEmoji bool
	BaseURL        string // oto's public URL, for deep links
	MaxInstances   int    // default 10
	// Continued marks a post_root that REPLACES a card this destination already
	// had for this generation, rather than opening the conversation: the §H.9
	// thread-pointer recovery, the reply-ceiling re-root, or a §G.5 re-send whose
	// predecessor may still be sitting in the channel.
	//
	// It exists because oto's answer to an un-amendable card is to post a NEW one,
	// and a new card with no marker reads to everybody in the room as a SECOND
	// INCIDENT. A visibly continued card is a far better failure than a silently
	// stale one — a human can see the first and cannot see the second — but only
	// if it says so.
	Continued bool
	// Mentions is the ALREADY-RESOLVED audience for this delivery: the org's
	// mention policy after its severity gate, in the provider's own wire form.
	// Empty is the default and means "address nobody".
	//
	// ⛔⛔ IT BELONGS IN THE TOP-LEVEL `text`, NEVER INSIDE A BLOCK. §H.1 S3 puts
	// every oto block inside one attachment, and Slack strips attachments from the
	// in-channel `thread_broadcast` reference — so a mention in a block is not
	// merely un-notifying, it is INVISIBLE in the channel. A renderer that cannot
	// put it in the top-level text must drop it rather than hide it (ADR 0020).
	//
	// ⛔ NOT A ROTA (§4.8, ADR 0013). It is a fixed audience an operator chose in
	// configuration; nothing on this path knows the time of day and nothing on it
	// ever may.
	Mentions []string
}
