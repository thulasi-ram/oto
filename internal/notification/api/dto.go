package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// The DTOs of the Notification tag. They live in `api` and NOWHERE ELSE
// (CONTEXT.md §5.5).

// ---------------------------------------------------------------- responses

// MatcherDTO is one label matcher.
//
// ⛔ IT MATCHES LABELS ONLY. There is no matcher on a time of day, on a weekday
// or on who is on call, and there never will be: a policy whose outcome depends
// on WHEN it is evaluated is a schedule (SCOPE-BOUNDARY §4.8).
type MatcherDTO struct {
	Name  string `json:"name"  validate:"required,labelname,max=1024"`
	Op    string `json:"op"    validate:"required,matcherop"`
	Value string `json:"value" validate:"max=4096"`
}

// ThrottleDTO is a per-subject rate cap.
//
// Hitting it produces a notification RECORDED AS SUPPRESSED with a reason, never
// a silent drop.
type ThrottleDTO struct {
	Max           int32 `json:"max"            validate:"required,min=1,max=1000"`
	WindowSeconds int32 `json:"window_seconds" validate:"required,min=60,max=86400"`
}

// PolicyDTO is one routing rule: matchers → channels → reasons.
//
// ⛔ A policy decides WHETHER and WHERE, never how a message is rendered, and
// never WHO. It has no `user_ids`, `team_ids`, `schedule_id` or `time_of_day`
// field and must never gain one (SCOPE-BOUNDARY §5.3).
type PolicyDTO struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Priority int32     `json:"priority"`
	Enabled  bool      `json:"enabled"`

	Matchers []MatcherDTO `json:"matchers"`
	Reasons  []string     `json:"reasons"`
	// ChannelIDs references `channels` and NOTHING ELSE.
	ChannelIDs []uuid.UUID  `json:"channel_ids"`
	Throttle   *ThrottleDTO `json:"throttle"`

	// UnackedReminderAfterSeconds is oto's ONE reminder stage, in seconds. Wire,
	// column (`unacked_reminder_after_s`, migration 00019), domain field and
	// contract all spell it the same way; the older spelling named a LADDER that
	// ends at a PERSON, and this is a SCALAR that ends at a CHANNEL
	// (CONTEXT.md §3, SPEC §P-20).
	//
	// ⛔ IT IS AND STAYS A SCALAR. ONE STAGE, FOREVER (SPEC §G.9.1). The moment it
	// is an array, oto is an on-call product and FR-1 has been crossed.
	UnackedReminderAfterSeconds *int32 `json:"unacked_reminder_after_seconds"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DeliverySummaryDTO is the fan-out health of one intent, so that "was anybody
// told about this?" is answerable without a second request.
type DeliverySummaryDTO struct {
	Total int32 `json:"total"`
	// Sent counts `skipped` too. A coalesced no-op update means the destination
	// already shows exactly this content, and reporting it as anything but
	// delivered would make a healthy quiet thread look broken.
	Sent   int32 `json:"sent"`
	Failed int32 `json:"failed"`
	Dead   int32 `json:"dead"`
	// Skipped is the subset of Sent that was a deliberate no-op, broken out so
	// "nothing was actually posted" is answerable without a second request.
	Skipped int32 `json:"skipped"`
	// Pending is queued plus in flight. `pending` and `sending` both count: from
	// the outside they are the same answer — nobody has been told YET.
	Pending int32 `json:"pending"`

	// LastErrorClass is the class of the most recent delivery error in this
	// fan-out, absent when nothing failed. It is what turns "one died" into "one
	// died because the token expired", which is the difference between a number
	// and an action.
	LastErrorClass *string `json:"last_error_class,omitempty"`
	// LastSentAt is when anything in this fan-out last reached a destination.
	LastSentAt *time.Time `json:"last_sent_at,omitempty"`
}

// NotificationDTO is the channel-agnostic INTENT to communicate one fact.
//
// It is NOT a message. A message is a NotificationDelivery. The internal
// idempotency hash is deliberately not exposed: it is a write-path mechanism, not
// a client-facing identifier.
type NotificationDTO struct {
	ID           uuid.UUID  `json:"id"`
	SubjectKind  string     `json:"subject_kind"`
	SubjectID    uuid.UUID  `json:"subject_id"`
	GroupID      uuid.UUID  `json:"group_id"`
	AlertID      *uuid.UUID `json:"alert_id"`
	OccurrenceID *uuid.UUID `json:"occurrence_id"`

	Reason       string     `json:"reason"`
	PolicyID     *uuid.UUID `json:"policy_id"`
	StateVersion int32      `json:"state_version"`
	Status       string     `json:"status"`
	// SuppressedReason is non-null IF AND ONLY IF status is `suppressed`.
	// Suppression is always recorded and always visible — oto's silence must never
	// be indistinguishable from "no alert".
	SuppressedReason *string `json:"suppressed_reason"`

	DeliverySummary *DeliverySummaryDTO `json:"delivery_summary,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NotificationDetailDTO is one intent with every materialisation of it.
type NotificationDetailDTO struct {
	NotificationDTO
	Deliveries []DeliveryDTO `json:"deliveries"`
}

// DeliveryDTO is ONE MATERIALISATION of a Notification on ONE Channel.
//
// This is where failure becomes visible: `dead` with `auth_expired` is how "your
// token was revoked three days ago and nobody noticed" stops being invisible.
type DeliveryDTO struct {
	ID             uuid.UUID `json:"id"`
	NotificationID uuid.UUID `json:"notification_id"`
	ChannelID      uuid.UUID `json:"channel_id"`
	ChannelName    string    `json:"channel_name,omitempty"`
	ChannelType    string    `json:"channel_type,omitempty"`

	ThreadID *uuid.UUID `json:"thread_id"`
	// ThreadSeq is the FIFO position within the thread, allocated inside the
	// transaction that created the delivery. It is how oto guarantees the root
	// lands first and replies appear in lifecycle order.
	ThreadSeq *int32 `json:"thread_seq"`

	Mode          string     `json:"mode"`
	Status        string     `json:"status"`
	Attempts      int32      `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at"`

	ProviderMessageID      *string `json:"provider_message_id"`
	ProviderConversationID *string `json:"provider_conversation_id"`
	Permalink              *string `json:"permalink"`

	Error      *string `json:"error"`
	ErrorClass *string `json:"error_class"`
	// Ambiguous is the honest flag: oto crashed after the provider may have
	// accepted the message, and re-sent it. Exactly-once delivery does not exist,
	// and oto would rather show a labelled duplicate than under-deliver a firing
	// alert.
	Ambiguous bool `json:"ambiguous"`

	// RenderedFallback is the message's top-level plain text — the push
	// notification, the search snippet, and THE ONLY THING A SCREEN READER READS.
	RenderedFallback *string `json:"rendered_fallback"`

	SentAt    *time.Time `json:"sent_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// DeliveryDetailDTO is one delivery including the payload that was rendered.
type DeliveryDetailDTO struct {
	DeliveryDTO
	// Rendered is the exact provider-native payload, persisted BEFORE the network
	// call. When outbound validation rejects a payload the delivery goes straight
	// to `dead` with `config_invalid` and the payload is retrievable here — it is
	// never silently truncated and never sent.
	Rendered         json.RawMessage `json:"rendered"`
	RenderedHash     *string         `json:"rendered_hash"`
	ProviderResponse json.RawMessage `json:"provider_response"`
}

// PolicyPreviewResultDTO is what ONE destination would receive.
type PolicyPreviewResultDTO struct {
	PolicyID    uuid.UUID `json:"policy_id"`
	PolicyName  string    `json:"policy_name"`
	ChannelID   uuid.UUID `json:"channel_id"`
	ChannelName string    `json:"channel_name"`
	ChannelType string    `json:"channel_type"`
	Mode        string    `json:"mode"`
	// WouldSend is false when a verbosity gate, a disabled channel or the absence
	// of any matching policy would suppress it.
	WouldSend bool `json:"would_send"`
	// SuppressedReason names WHICH suppressor would apply, in the §B.8.2
	// precedence order. A preview that said only "nothing would be sent" would
	// answer the easy half of the question.
	SuppressedReason *string `json:"suppressed_reason"`

	RenderedFallback *string         `json:"rendered_fallback"`
	Rendered         json.RawMessage `json:"rendered"`
}

// PolicyPreviewDTO is a DRY RUN: "given this alert, who is told, where, and
// rendered how."
//
// ⛔ IT RUNS THE REAL MATCHER AND THE REAL RENDERER AND SENDS NOTHING. This is
// the answer to the question every alert-routing config raises and few tools
// answer: if this fires at 3am, who actually gets woken up?
type PolicyPreviewDTO struct {
	// Matched reports whether any enabled policy claimed the fact at all. False
	// means this fact would notify nobody.
	Matched bool                     `json:"matched"`
	Results []PolicyPreviewResultDTO `json:"results"`
	// Warnings explain the outcome in words: which policy won, what it SHADOWED,
	// which clause of a near-miss failed, and which suppressor would engage. The
	// shadowing line is usually the actual question being asked.
	Warnings []string `json:"warnings,omitempty"`
}

// ----------------------------------------------------------------- requests

// CreatePolicyRequest creates a routing policy.
type CreatePolicyRequest struct {
	Name     string `json:"name"               validate:"required,notblank,min=1,max=120"`
	Priority *int32 `json:"priority,omitempty" validate:"omitempty,min=0,max=10000"`
	Enabled  *bool  `json:"enabled,omitempty"`

	Matchers []MatcherDTO `json:"matchers,omitempty" validate:"omitempty,max=32,dive"`
	// Reasons is validated against the domain's closed vocabulary rather than an
	// `oneof` tag, because migration 00018 narrowed it and a duplicated list here
	// would be the second copy that drifts.
	Reasons []string `json:"reasons" validate:"required,min=1,max=32,unique"`
	// ChannelIDs references `channels` and NOTHING ELSE.
	ChannelIDs []uuid.UUID  `json:"channel_ids" validate:"required,min=1,max=16,unique"`
	Throttle   *ThrottleDTO `json:"throttle,omitempty"`

	// UnackedReminderAfterSeconds is the ONE unacked reminder stage, in seconds.
	// See PolicyDTO: scalar, one stage, forever.
	UnackedReminderAfterSeconds *int32 `json:"unacked_reminder_after_seconds,omitempty" validate:"omitempty,min=60,max=86400"`
}

// UpdatePolicyRequest is the partial update.
type UpdatePolicyRequest struct {
	Name     *string `json:"name,omitempty"     validate:"omitempty,notblank,min=1,max=120"`
	Priority *int32  `json:"priority,omitempty" validate:"omitempty,min=0,max=10000"`
	Enabled  *bool   `json:"enabled,omitempty"`

	Matchers   *[]MatcherDTO `json:"matchers,omitempty"    validate:"omitempty,max=32,dive"`
	Reasons    *[]string     `json:"reasons,omitempty"     validate:"omitempty,min=1,max=32,unique"`
	ChannelIDs *[]uuid.UUID  `json:"channel_ids,omitempty" validate:"omitempty,min=1,max=16,unique"`

	// Throttle and UnackedReminderAfterSeconds are nullable in the contract: an
	// explicit `null` CLEARS the damper, which is a different request from
	// omitting the field. NullableThrottle and NullableInt32 keep that
	// distinction while leaving `httpx.Bind` the only door a body comes through.
	Throttle                    NullableThrottle `json:"throttle,omitempty"`
	UnackedReminderAfterSeconds NullableInt32    `json:"unacked_reminder_after_seconds,omitempty"`
}

// IsEmpty reports whether the request asks for nothing.
func (r UpdatePolicyRequest) IsEmpty() bool {
	return r.Name == nil && r.Priority == nil && r.Enabled == nil &&
		r.Matchers == nil && r.Reasons == nil && r.ChannelIDs == nil &&
		!r.Throttle.Set && !r.UnackedReminderAfterSeconds.Set
}

// PolicyPreviewRequest describes the fact to dry-run.
//
// Exactly one subject is supplied. Supplying an inline `policy` evaluates that
// UNSAVED DRAFT in addition to the stored ones, which is what lets the settings
// form answer "who would this reach?" before anything is saved — and is the
// difference between a preview and a post-mortem.
type PolicyPreviewRequest struct {
	AlertID      *uuid.UUID `json:"alert_id,omitempty"`
	OccurrenceID *uuid.UUID `json:"occurrence_id,omitempty"`
	GroupID      *uuid.UUID `json:"group_id,omitempty"`
	// Reason defaults to `fired`.
	Reason string               `json:"reason,omitempty"`
	Policy *CreatePolicyRequest `json:"policy,omitempty"`
}

// NullableThrottle is a contract field typed as `ThrottleDTO | null`, where an
// explicit `null` means REMOVE THE THROTTLE and an omitted field means leave it
// alone. A plain pointer cannot express both.
type NullableThrottle struct {
	Set   bool
	Value *ThrottleDTO
}

// UnmarshalJSON records presence as well as value.
func (n *NullableThrottle) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var v ThrottleDTO
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

// MarshalJSON renders the field back, for symmetry.
func (n NullableThrottle) MarshalJSON() ([]byte, error) {
	if !n.Set || n.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(n.Value)
}

// NullableInt32 is a contract field typed as `integer | null`, where an explicit
// `null` means DISABLE and an omitted field means leave it alone.
type NullableInt32 struct {
	Set   bool
	Value *int32
}

// UnmarshalJSON records presence as well as value.
func (n *NullableInt32) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var v int32
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

// MarshalJSON renders the field back, for symmetry.
func (n NullableInt32) MarshalJSON() ([]byte, error) {
	if !n.Set || n.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*n.Value)
}
