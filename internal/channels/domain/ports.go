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
	// ⛔ `CapBroadcast` WAS HERE AND IS DELETED (git-bug 7570090). It meant "a
	// thread reply can be surfaced in-channel", and oto no longer surfaces
	// anything in a channel: `BroadcastPolicy.Warrants` returned true for exactly
	// `refired` — which ADR 0040 left with no producer — and `all_resolved`, which
	// was opt-in and default-off. A capability that nothing can ever ask for is a
	// negotiation nobody runs, so the mechanism was deleted rather than disabled.
	//
	// ⛔⛔ THE BLANK IS LOAD-BEARING AND MUST NOT BE TIDIED AWAY. These positions
	// are a STORED WIRE CONTRACT: `channels.capabilities` is a BIGINT holding this
	// exact bitmask for every configured channel
	// (`db/migrations/00011_notification.sql:56`, `channels_caps_ck` at `:75`).
	// Closing the gap would move `CapDedupeKey` from 32 down to 16 and silently
	// re-label every row already in the database — a data corruption with no error
	// message. Bit 4 is RETIRED, not freed: the next capability takes 64.
	_
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
//
// ⭐ TWO SCHEMAS, TWO LEVELS. ConfigSchema is a CHANNEL's own settings — one
// destination, e.g. `conversation_id`. ConnectionConfigSchema is the ORG-WIDE
// setup every channel of this type shares, e.g. a Slack workspace's `team_id` —
// set up once in Settings, referenced by many channels. The same schema-driven
// argument applies to both: neither schema is hand-copied into a DTO.
type Descriptor struct {
	Type            Type
	DisplayName     string
	ConfigSchema    json.RawMessage // JSON Schema draft 2020-12; drives the dynamic settings UI
	CredentialKinds []string
	// ConnectionConfigSchema is the JSON Schema for this provider's Connection —
	// the org-wide setup, not one destination's.
	ConnectionConfigSchema json.RawMessage
	// ConnectionCredentialKinds is what a Connection of this type accepts. A
	// slack connection requires CredBotToken (channel_connections_cred_ck); a
	// webhook connection may have none at all.
	ConnectionCredentialKinds []string
	Capabilities              Capability
	Renderers                 []RendererID
	RateLimitClass            string // "slack" | "none"
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
	// ValidateConnectionConfig validates a Connection's config against
	// Descriptor().ConnectionConfigSchema — the org-wide setup, not one
	// channel's.
	ValidateConnectionConfig(ctx context.Context, raw json.RawMessage) error
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

// ConversationQuery names a conversation by whichever half the operator typed.
// Exactly one of Name or ID is set; the caller fills the other in.
type ConversationQuery struct {
	// Name is a channel display name, with or without a leading '#'.
	Name string
	// ID is the provider's own identifier ("C0123456" for Slack).
	ID string
}

// ConversationResult is what a resolve answered: both halves, filled in.
type ConversationResult struct {
	ID   string
	Name string
}

// ConversationResolver is an OPTIONAL capability a Provider may implement, so
// the settings UI can turn "the channel named #sre-alerts" and "the channel
// with id C0123456" into each other. It is not part of Provider itself: a
// provider with nothing to resolve — the generic webhook — need not fake one.
//
// ⭐ THIS RE-OPENS A DELIBERATELY CLOSED SCOPE DECISION. The Slack `API`
// interface in `providers/slack/channel.go` used to be exactly three methods,
// and `conversations.info` was removed for having zero callers. This interface
// is that caller, reintroduced on purpose — see the ADR that supersedes the
// "cut it, nothing calls it" reasoning.
type ConversationResolver interface {
	ResolveConversation(ctx context.Context, cred Credential, query ConversationQuery) (ConversationResult, error)
}

// Mode is the delivery decision produced by the §H.6 table.
type Mode string

// The delivery modes.
const (
	// ModePostRoot posts a new root message. It happens once per THREAD, and a
	// thread is keyed by its SUBJECT — an alert, a case or a group since migration
	// 00056, and always the AlertGroup generation in v1. It is the only genuinely
	// at-risk operation on recovery (§G.5).
	ModePostRoot Mode = "post_root"
	// ModeUpdateRoot amends the root in place. This is the PRIMARY mechanism:
	// `repeat interval elapsed` is update-only, never a repost (C8).
	ModeUpdateRoot Mode = "update_root"
	// ModeThreadReply appends a reply to the SUBJECT's thread — the group's, while
	// v1 keys every thread by the AlertGroup generation. Replies are the exception,
	// not the rule.
	ModeThreadReply Mode = "thread_reply"
	// ⛔ `ModeBroadcastReply` WAS HERE AND IS DELETED IN FULL (git-bug 7570090).
	// It surfaced a thread reply in-channel with Slack's `reply_broadcast` (ADR
	// 0020), and it is gone with the mechanism: `BroadcastPolicy.Warrants` returned
	// true for `refired` alone — which ADR 0040 retired T8 and left with no producer
	// — and for `all_resolved`, which was opt-in and off by default. oto therefore
	// broadcast nothing out of the box and had no way to be asked to, so the owner
	// ruled the mechanism removed rather than disabled.
	//
	// ⭐ WHAT THE MODE ARGUED THAT STILL HOLDS. Broadcast was NEVER AN ESCALATION —
	// oto has zero reminder stages (git-bug `bd0fb1d`) and sends nothing unprompted
	// — and `escalation` remains a banned word (CONTEXT.md) with no replacement.
	// That is now a property of the whole surface rather than of one mode: every
	// delivery oto makes lands in a thread, on the same trigger as every other.
	//
	// ⛔ THE VALUE IS NOT RETIRED, IT IS DELETED. `deliveries_mode_ck` in migration
	// 00011 still admits `'broadcast_reply'`; a new migration drops it from the
	// constraint and strips it from existing rows by hand, the way 00060 and 00067
	// did. Nothing may write it in the meantime.
)

// DeliverRequest is one delivery attempt on one Channel.
type DeliverRequest struct {
	Message    RenderedMessage
	Mode       Mode
	ReplyTo    *MessageRef // required for thread_reply
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
	// Template is the NotificationTemplate this delivery was routed with, ALREADY
	// SELECTED: the policy that matched named it, so the renderer reads a foreign
	// key's worth of decision rather than running a search of its own.
	//
	// ⭐ IT IS SOURCE RATHER THAN A COMPILED OBJECT ON PURPOSE. This package cannot
	// import the template package — that package imports this one for
	// NotificationView — and a string is also exactly what gets written onto the
	// delivery row beside the rendered payload, so "why did my card read like
	// that" is answerable from one row rather than from a config that may since
	// have changed. The renderer keeps its own compiled cache.
	//
	// ⚠️ ITS FORMAT NEED NOT SUIT THIS CHANNEL'S PROVIDER, AND THAT IS THE OWNER'S
	// CALL. A policy fans out to as many as sixteen channels and they need not
	// share a provider. `card` and `text` are portable and render anywhere; `raw`
	// is Slack's Block Kit and means nothing elsewhere. A renderer that cannot use
	// what it was given builds its own card instead — a mismatch is a degraded
	// message, never a dropped alert.
	Template *TemplateRef
	// ⛔ `Mentions` WAS HERE AND IS DELETED (git-bug bd0fb1d). It carried the org's
	// resolved mention audience for the ONE unacked reminder, gated on severity.
	// The owner withdrew the reminder and ruled the mention goes with it: a mention
	// was never a property of delivery in general, it was the audience half of that
	// one fact and had no other producer.
	//
	// ⭐ ADR 0013 §4.8 AND H-1 GET STRONGER, NOT WEAKER. The field carried a standing
	// refusal — a fixed audience from configuration, never a rota, nothing on the
	// path allowed to read a clock. With no mention surface at all there is nowhere
	// left for oto to name a responder.
}

// A TemplateRef is the NotificationTemplate a delivery was routed with, in the
// only two fields a renderer needs: what shape it is, and what it says.
type TemplateRef struct {
	// ID and Version identify it on the delivery row, so a card can be traced to
	// the exact revision that produced it even after the template is edited.
	ID      string
	Version int
	// Format is `card`, `text` or `raw`.
	Format string
	// Source is the template body, already sanitised at save time.
	Source string
}
