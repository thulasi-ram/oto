package service

import (
	"context"

	chdomain "github.com/thulasiram/oto/internal/channels/domain"
)

// THIS FILE IS THE ENTIRE SEAM BETWEEN `notification` AND `channels`.
//
// It is the ONLY file in this module that names `internal/channels/domain`.
// Everything else in `notification` speaks in the local names declared below, so
// the coupling is one import in one file that a reviewer can read in a minute,
// rather than a dependency scattered across a package.
//
// ⚠ KNOWN CONFLICT, DELIBERATE. `.golangci.yml`'s
// `notification-must-not-reach-into-other-domains` rule denies
// `internal/channels/**` and re-allows only `internal/channels/service`, which
// is currently an empty package. SPEC §F.1 and §F.2 are explicit and binding in
// the other direction:
//
//   - §F.1 places `Channel`, `Renderer`, `RenderedMessage` and `Mode` in
//     `internal/channels/domain`, as PORTS;
//   - §F.2 states that `NotificationView` lives in `internal/channels/domain`
//     and is "built once per delivery, at claim time, by
//     `notification/service.ViewService`" — this package, by name.
//
// A port is meant to be imported by its consumer; that is what makes it a port.
// The rule as written would require either duplicating a 160-field read model or
// routing every renderer call through a facade that does not exist. The SPEC
// wins (it outranks the tooling in the stated precedence order), the import is
// confined to this file, and the resolution is one line: add
// `internal/channels/domain` to that rule's `allow` list beside
// `internal/channels/service`. That is a shared-config change and therefore not
// this module's to make unilaterally.
//
// What the rule is actually protecting is still honoured: nothing below reaches
// into a channels IMPLEMENTATION. There is no provider, no registry concrete, no
// SDK — only the interfaces and value types §F.1 declares as the contract.

// The channel port's value types, under local names.
type (
	// NotificationView is the channel-agnostic read model a Renderer consumes.
	NotificationView = chdomain.NotificationView
	// RenderOptions is the per-delivery rendering context.
	RenderOptions = chdomain.RenderOptions
	// RenderedMessage is provider-native bytes plus the two strings every
	// provider needs.
	RenderedMessage = chdomain.RenderedMessage
	// MessageRef is a foreign system's primary key. Immutable, never reshaped.
	MessageRef = chdomain.MessageRef
	// DeliverRequest is one delivery attempt on one destination.
	DeliverRequest = chdomain.DeliverRequest
	// DeliverResult is what the destination said.
	DeliverResult = chdomain.DeliverResult
	// TargetConfig is the validated, non-secret configuration of one destination.
	TargetConfig = chdomain.ChannelConfig
	// TargetCredential is an already-unsealed secret.
	TargetCredential = chdomain.Credential
	// ProviderType is the provider discriminator.
	ProviderType = chdomain.Type
	// RendererID names a Renderer in the registry.
	RendererID = chdomain.RendererID
	// RenderMode is the §H.6 delivery decision, as the renderer sees it.
	RenderMode = chdomain.Mode
	// RenderVerbosity is the destination's verbosity, as the renderer sees it.
	RenderVerbosity = chdomain.Verbosity
	// ProviderError is a classified provider failure.
	ProviderError = chdomain.Error
)

// The view's sub-types, under local names.
type (
	// OrgRef identifies the tenant a notification belongs to.
	OrgRef = chdomain.OrgRef
	// GroupView is one generation of one notification group.
	GroupView = chdomain.GroupView
	// AlertView is one Alert as a renderer sees it.
	AlertView = chdomain.AlertView
	// CaseView is one firing episode as a renderer sees it.
	CaseView = chdomain.CaseView
	// DigestView is a count and the span it was counted over — what a digest has
	// instead of a GroupView (git-bug `78388fb`). It is aliased here for the same
	// reason every other view type is: `ViewService.digest` builds one and this is
	// the only file in the module allowed to name `internal/channels/domain`.
	DigestView = chdomain.DigestView
	// RuleView is what the alerting rule said when the case fired.
	RuleView = chdomain.RuleView
	// RuleChangeView is what changed in the rule since the previous case.
	RuleChangeView = chdomain.RuleChangeView
	// EnrichmentView is one Enricher's provenanced result.
	EnrichmentView = chdomain.EnrichmentView
	// ActorView is who caused the fact being communicated.
	ActorView = chdomain.ActorView
	// Action is one interactive affordance on a card.
	Action = chdomain.Action
	// ActionOption is one choice inside a menu-shaped Action (§B.8.3's presets).
	ActionOption = chdomain.ActionOption
	// PreviousState is the state the card showed before this delivery (§H.4).
	PreviousState = chdomain.PreviousState
	// TrailEntry is one transition on the card's state trail (§H.4).
	TrailEntry = chdomain.TrailEntry
	// Links are the deep links a card offers.
	Links = chdomain.Links
)

// snoozePresets is the closed list of durations a card may offer to go quiet for
// (§B.8.3: 30 m · 1 h · 4 h · 24 h · 7 d, and no indefinite option).
//
// ⛔ IT IS FORWARDED THROUGH THIS FILE RATHER THAN IMPORTED WHERE IT IS USED,
// because of the sentence at the top: this is the ONLY file in the module that may
// name `internal/channels/domain`, and the value is needed by `ViewService.actions`
// two files away. A `var` holding the function is how a FUNCTION crosses a seam
// that only has room for type aliases.
//
// ⭐ WHY THE LIST LIVES OVER THERE AT ALL. `channels/service` decodes the token
// that comes back off the press, and this module mints the token that goes out. One
// table read from both ends is the only shape in which the menu cannot offer a
// choice the handler is unable to decode — which would present as a button that
// does nothing, the exact defect the snooze affordance was filed against
// (git-bug `0a8ca4a`).
var snoozePresets = chdomain.SnoozePresets

// snoozeValueSeparator joins a preset token to the alert id in one snooze option's
// value. Forwarded for the same reason snoozePresets is, and owned over there so
// that the module which splits the value cannot disagree with the module that
// builds it.
const snoozeValueSeparator = chdomain.SnoozeValueSeparator

// Target is one opened destination: the delivery port.
//
// It knows NOTHING about alerts. It moves rendered bytes somewhere and reports
// what that somewhere said. Every provider-specific concern — Block Kit, rate
// limits, error-code classification — lives behind it.
type Target = chdomain.Channel

// MessageRenderer is a PURE FUNCTION from a NotificationView to provider-native
// bytes. No I/O: it runs at claim time inside a delivery's budget.
type MessageRenderer = chdomain.Renderer

// ChannelRegistry is the narrow view this module takes of the channels registry:
// resolve a renderer, and open a destination.
//
// Note what is absent. There is no "post", no "update", no "thread" and no
// provider name. Everything this module does to a destination goes through
// Target, which is the only reason the §H.6 decision table can be trusted — a
// table with an "unless it is this provider" branch is not a table.
type ChannelRegistry interface {
	Renderer(t ProviderType, id RendererID) (MessageRenderer, error)
	Open(ctx context.Context, t ProviderType, cfg TargetConfig, cred TargetCredential) (Target, error)
}
