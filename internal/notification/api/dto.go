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
	ChannelIDs []uuid.UUID `json:"channel_ids"`
	// TemplateID is the NotificationTemplate these deliveries are rendered with.
	// Absent is oto's built-in card.
	TemplateID *uuid.UUID   `json:"template_id,omitempty"`
	Throttle   *ThrottleDTO `json:"throttle"`

	// SubjectKinds is `subject_kinds` (migration 00072): which altitude of fact
	// this policy is about, drawn from the `alert | case | digest` vocabulary
	// `notifications.subject_kind` holds.
	//
	// ⭐ AN EMPTY ARRAY MEANS EVERY KIND AND IT IS NEVER `null`. It is a plain
	// slice rather than a pointer for the reason `matchers` and `reasons` are: the
	// column is `NOT NULL DEFAULT '{}'` so there is no third state, and "claims
	// every altitude" is a real answer rather than an absence. A nullable field
	// would offer clients two spellings of one fact.
	//
	// ⚠️ IT OVERLAPS `reasons` AND THE CONTRACT SAYS SO RATHER THAN HIDING IT. Each
	// Reason is about exactly one subject kind, so as a filter this narrows nothing
	// a shorter `reasons` list could not. It earns its place as the count
	// condition's UNIT — `count_min` requires exactly one kind — and as a
	// declaration an operator can read without knowing the Reason-to-subject map.
	SubjectKinds []string `json:"subject_kinds"`

	// CountMin and CountWindowSeconds are `count_min` and `count_window_s`
	// (migration 00072): stay silent until at least CountMin facts about this
	// policy's bound subject kind have happened inside the window. `null` on either
	// means the policy carries no count condition, which is what every policy
	// written before 00072 says.
	//
	// ⭐ IT IS THE THROTTLE'S DUAL AND THE SAME TWO FIELDS — a floor where the
	// throttle is a ceiling — and a policy may carry both. The window is a SLIDING
	// lookback like the throttle's, not a tiled one like the digest's, so there is
	// no divisor rule on it and none is implied by its range.
	//
	// ⭐ IT SUPPRESSES, AS OF MIGRATION 00073, AND A UI MAY PROMISE THE BEHAVIOUR.
	// `NotificationService.suppressors` counts the facts already inside the window,
	// adds the one being evaluated (which is why the floor starts at 2), and records
	// `below_threshold` when the total is still under CountMin. The two migrations
	// landed in that order on purpose: 00072 shipped the columns and refused to widen
	// `notifications_suppmap_ck`, because a suppression reason nothing can write is a
	// dead enum entry an operator has to rule out; 00073 admits the reason together
	// with its writer.
	//
	// ⚠️ WHAT IT GATES IS AN ORDINARY NOTIFICATION ON THE ORDINARY EVALUATION PATH,
	// which is every policy bound to `alert` or to `case`. A policy bound to `digest`
	// has its count condition read by nothing — a digest is minted by the tick
	// against `digest_window_s`/`digest_floor`, its own floor over its own window, and
	// never passes the suppressor chain — and whether that pairing stays admissible at
	// all is being tightened. Do not build on either answer for that one binding.
	CountMin           *int32 `json:"count_min"`
	CountWindowSeconds *int32 `json:"count_window_seconds"`

	// DigestWindowSeconds and DigestFloor are `digest_window_s` and
	// `digest_floor` (migration 00058): summarise what matched me over a window,
	// and stay silent unless at least Floor Cases OPENED inside it. `null` on
	// either means the policy has no window and no floor, which is what every
	// policy written before 00058 says.
	//
	// ⛔ THE WINDOW IS NOT A SCHEDULE OF WHEN OTO MAY SPEAK (SCOPE-BOUNDARY §4.8).
	// It selects which FACTS a summary covers, is read by the digest tick and by
	// nothing on the evaluation path, and carries no timezone — windows are aligned
	// in UTC, because a per-policy timezone is the first half of quiet hours.
	//
	// ⚠️ THE DIVISOR RULE IS NOT EXPRESSIBLE HERE and is not duplicated here. The
	// window must divide 86400 (`policies_digest_window_ck`), which neither a
	// `validate` tag nor JSON Schema can state — `multipleOf` says the opposite —
	// so `domain.DigestWindowAligned` is the ONE place it is written and
	// `Policy.Validate` is what enforces it. Layer 1 carries the range only.
	DigestWindowSeconds *int32 `json:"digest_window_seconds"`
	DigestFloor         *int32 `json:"digest_floor"`

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
	ID          uuid.UUID `json:"id"`
	SubjectKind string    `json:"subject_kind"`
	SubjectID   uuid.UUID `json:"subject_id"`
	// ⛔ `GroupID *uuid.UUID` WAS HERE AND IS DELETED (git-bug `7570090`, migration
	// `00069`), AND ITS ARGUMENT OUTLIVES IT because it is about the WIRE, not about
	// groups. It became a pointer at `00058` for this reason, quoted so the next
	// optional id gets it right:
	//
	//   a value type would serialise the ZERO UUID and every client would read an id
	//   that resolves to nothing. oto's silence must never be indistinguishable from
	//   "no alert", and an id that names nothing is the same class of lie.
	//
	// `AlertID` and `CaseID` below are pointers under exactly that rule. The delivery
	// target is now the `(conversation_kind, conversation_id)` pair, and it is a value
	// type legitimately: every row names a conversation, so there is no absence to
	// express.
	AlertID *uuid.UUID `json:"alert_id"`
	CaseID  *uuid.UUID `json:"case_id"`

	Reason       string     `json:"reason"`
	PolicyID     *uuid.UUID `json:"policy_id"`
	StateVersion int32      `json:"state_version"`
	Status       string     `json:"status"`
	// SuppressedReason is non-null IF AND ONLY IF status is `suppressed`.
	// Suppression is always recorded and always visible — oto's silence must never
	// be indistinguishable from "no alert".
	SuppressedReason *string `json:"suppressed_reason"`

	// DeliverySummary is the fan-out roll-up, and it is OPTIONAL ON THIS SCHEMA.
	//
	// ⚠️ THE ORG-WIDE LIST NOW SENDS ONE, WHICH IT DID NOT. It reads the whole
	// page's deliveries in a single round trip rather than a query per row, which
	// is what made the roll-up unaffordable before. The key stays optional for two
	// honest reasons: the batched read is allowed to fail without taking the log
	// down with it, and the per-alert notification list in `alerts/api` is served
	// by a projection that has no deliveries to roll up at all. An absent key says
	// "not computed here" rather than inventing an all-zero fan-out, and the
	// difference matters — all-zero would read as "nobody was told".
	//
	// It is REQUIRED on `NotificationDetailDTO`, which declares its own non-pointer
	// field for exactly that reason.
	DeliverySummary *DeliverySummaryDTO `json:"delivery_summary,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the intent or its delivery roll-up last moved, or `null`
	// when the read model serving the response does not track one.
	//
	// ⛔ IT IS A POINTER BECAUSE THE SCHEMA IS NULLABLE, AND THE SCHEMA IS
	// NULLABLE BECAUSE ONE PRODUCER GENUINELY HAS NO VALUE. `notifications.
	// updated_at` is NOT NULL, so this module always sends a timestamp; the
	// per-alert notification list is served by `alerts/api` out of a projection
	// that does not read the column at all (`internal/app/adapters.go`), and it
	// sends `null`. `null` there says "unknown"; echoing `created_at` instead —
	// which is what it used to do — made "never changed" and "changed a minute
	// ago" indistinguishable on the wire, with no way for a caller to tell.
	//
	// Both Go structs behind the one `NotificationDTO` schema therefore have to
	// be shaped like the schema, or the schema is describing only one of them.
	UpdatedAt *time.Time `json:"updated_at"`
}

// NotificationDetailDTO is one intent with every materialisation of it.
type NotificationDetailDTO struct {
	NotificationDTO
	// DeliverySummary SHADOWS the optional pointer on NotificationDTO with a
	// value, because the contract lists it among this schema's required members
	// and a pointer with `omitempty` cannot keep that promise structurally.
	//
	// ⛔ THIS IS THE AUDIT'S WORST FINDING, MADE UNREPEATABLE. The summary was
	// dropped from the detail response for exactly the intents where it matters
	// most: a SUPPRESSED notification has no deliveries at all, `summarise`
	// returned nil for an empty fan-out, and `omitempty` then deleted the key —
	// so "oto formed this intent and told nobody" was indistinguishable from a
	// server that never computed one. `summarise` now returns an all-zero
	// summary; this field is what stops a future nil from becoming an absent
	// key again.
	DeliverySummary DeliverySummaryDTO `json:"delivery_summary"`
	Deliveries      []DeliveryDTO      `json:"deliveries"`
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

	// The provider's own handles for the message oto posted. Together they are
	// what an in-place update addresses.
	//
	// ⛔ THERE IS NO `Permalink`. One was declared here and written by nothing:
	// oto stores no permalink column, `chat.postMessage` does not return one, and
	// building a Slack archive URL needs a workspace domain oto never asks for. A
	// `format: uri` field that is always null is a deep link that never links.
	ProviderMessageID      *string `json:"provider_message_id"`
	ProviderConversationID *string `json:"provider_conversation_id"`

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
	//
	// `max` is the SIZE OF THE ENUM, not a request-size judgement: `unique` over a
	// closed vocabulary makes an N+1'th element unreachable, and since 00046 the
	// column agrees — it refuses a duplicate too, so 32 stopped being a number any
	// row could reach. It moved from 18 to 19 when migration 00058 added `digest`
	// and back to 18 when migration 00060 deleted `storm`; the contract's
	// `maxItems` is held to the same number by
	// `test/contract/dto_schema_test.go`'s enum-ceiling gate.
	//
	// It moved to 17 when 00067 deleted `unacked_reminder` (git-bug bd0fb1d).
	Reasons []string `json:"reasons" validate:"required,min=1,max=17,unique"`
	// ChannelIDs references `channels` and NOTHING ELSE.
	ChannelIDs []uuid.UUID `json:"channel_ids" validate:"required,min=1,max=16,unique"`
	// TemplateID names a NotificationTemplate. Omit it for oto's built-in card.
	//
	// ⚠️ IT IS NOT CHECKED AGAINST THE PROVIDERS OF `channel_ids`. A policy fans
	// out to as many as sixteen destinations and they need not share a provider;
	// `card` and `text` render anywhere and `raw` degrades to oto's own card
	// elsewhere. Pairing them up is the owner's call.
	TemplateID *uuid.UUID   `json:"template_id,omitempty"`
	Throttle   *ThrottleDTO `json:"throttle,omitempty"`

	// SubjectKinds binds the policy to an altitude. OPTIONAL, and omitting it —
	// like sending `[]` — claims every kind, which is the shipped behaviour and what
	// every payload written before migration 00072 means.
	//
	// `max` is the SIZE OF THE VOCABULARY held against `MaxPolicySubjectKinds`, on
	// the rule `reasons` is held to: `unique` over a closed set makes a fourth
	// element unreachable, so any larger number would be one no request could reach.
	// The values are validated against the domain's closed vocabulary in the mapper
	// rather than by an `oneof` tag, because a duplicated list here would be the
	// second copy that drifts.
	SubjectKinds []string `json:"subject_kinds,omitempty" validate:"omitempty,max=3,unique"`

	// CountMin and CountWindowSeconds ask for the count condition. Both are
	// OPTIONAL, so every payload written before migration 00072 stays valid — the
	// contract evolves additively (openapi.yaml §evolution).
	//
	// The tags carry the RANGE from `policies_count_min_ck` and
	// `policies_count_window_ck` and nothing else. The symmetric pair rule
	// (`policies_count_pair_ck`) and the unit rule (`policies_count_subject_ck`,
	// which ties the condition to a one-element `subject_kinds`) are cross-field and
	// live in `domain.Policy.Validate` — the same division the digest makes.
	CountMin           *int32 `json:"count_min,omitempty"             validate:"omitempty,min=2,max=10000"`
	CountWindowSeconds *int32 `json:"count_window_seconds,omitempty"  validate:"omitempty,min=60,max=86400"`

	// DigestWindowSeconds and DigestFloor ask for the periodic summary. Both are
	// OPTIONAL, so every payload written before migration 00058 stays valid — the
	// contract evolves additively (openapi.yaml §evolution).
	//
	// The tags carry the RANGE from `policies_digest_window_ck` and
	// `policies_digest_floor_ck` and nothing else. The divisor rule, the pair rule
	// (`policies_digest_pair_ck`) and the reason rule (`policies_digest_reason_ck`)
	// are cross-field and live in `domain.Policy.Validate` — see PolicyDTO.
	DigestWindowSeconds *int32 `json:"digest_window_seconds,omitempty" validate:"omitempty,min=300,max=86400"`
	DigestFloor         *int32 `json:"digest_floor,omitempty"          validate:"omitempty,min=1,max=10000"`
}

// UpdatePolicyRequest is the partial update.
type UpdatePolicyRequest struct {
	Name     *string `json:"name,omitempty"     validate:"omitempty,notblank,min=1,max=120"`
	Priority *int32  `json:"priority,omitempty" validate:"omitempty,min=0,max=10000"`
	Enabled  *bool   `json:"enabled,omitempty"`

	Matchers   *[]MatcherDTO `json:"matchers,omitempty"    validate:"omitempty,max=32,dive"`
	Reasons    *[]string     `json:"reasons,omitempty"     validate:"omitempty,min=1,max=17,unique"`
	ChannelIDs *[]uuid.UUID  `json:"channel_ids,omitempty" validate:"omitempty,min=1,max=16,unique"`
	// TemplateID is nullable: `"template_id": null` CLEARS it and puts the policy
	// back on oto's built-in card, while omitting the key leaves it alone.
	TemplateID *NullableUUID `json:"template_id,omitempty"`

	// Throttle is nullable in the contract: an explicit `null` CLEARS the damper,
	// which is a different request from omitting the field. NullableThrottle keeps
	// that distinction while leaving `httpx.Bind` the only door a body comes through.
	Throttle NullableThrottle `json:"throttle,omitempty"`

	// The digest is nullable for the same reason the throttle is: an explicit
	// `null` TURNS THE SUMMARY OFF, which is a different request from omitting the
	// field. The two halves are separately nullable because the columns are —
	// clearing the floor while keeping the window is "send whenever the window was
	// not empty", which is a real instruction and not the same as clearing both.
	DigestWindowSeconds NullableInt32 `json:"digest_window_seconds,omitempty"`
	DigestFloor         NullableInt32 `json:"digest_floor,omitempty"`

	// SubjectKinds is the ONE new field on this request that is NOT nullable, and
	// the asymmetry is the column's rather than an oversight. `subject_kinds` is
	// `NOT NULL DEFAULT '{}'`, so there is no NULL to set: an EMPTY ARRAY is how an
	// operator removes a binding, and it is a `*[]string` for exactly the reason
	// `Reasons` is — absent means "leave it alone" and present means "this is the new
	// value". `{"subject_kinds": null}` is refused by the contract rather than
	// silently meaning the same thing as `[]`.
	SubjectKinds *[]string `json:"subject_kinds,omitempty" validate:"omitempty,max=3,unique"`

	// The count condition is nullable for the reason the digest and the throttle
	// are: an explicit `null` TURNS THE CONDITION OFF, which is a different request
	// from omitting the field.
	//
	// ⚠️ THE TWO HALVES ARE SEPARATELY NULLABLE AND THAT IS NOT A LICENCE TO CLEAR
	// ONE. `policies_count_pair_ck` is symmetric, unlike the digest's pair rule, so
	// clearing exactly one half is refused — `validateMerged` catches it as a
	// field-level 422 before the UPDATE can turn it into a 23514. They are two
	// nullable fields because the WIRE is two fields, not because half a condition is
	// a state a policy may be in.
	CountMin           NullableInt32 `json:"count_min,omitempty"`
	CountWindowSeconds NullableInt32 `json:"count_window_seconds,omitempty"`
}

// IsEmpty reports whether the request asks for nothing.
func (r UpdatePolicyRequest) IsEmpty() bool {
	return r.Name == nil && r.Priority == nil && r.Enabled == nil &&
		r.Matchers == nil && r.Reasons == nil && r.ChannelIDs == nil &&
		r.TemplateID == nil &&
		!r.Throttle.Set &&
		!r.DigestWindowSeconds.Set && !r.DigestFloor.Set &&
		r.SubjectKinds == nil &&
		!r.CountMin.Set && !r.CountWindowSeconds.Set
}

// PolicyPreviewRequest describes the fact to dry-run.
//
// Exactly one subject is supplied. Supplying an inline `policy` evaluates that
// UNSAVED DRAFT in addition to the stored ones, which is what lets the settings
// form answer "who would this reach?" before anything is saved — and is the
// difference between a preview and a post-mortem.
type PolicyPreviewRequest struct {
	// ⛔ `alert_id` AND `group_id` WERE HERE AND ARE DELETED (git-bug `7570090`,
	// owner ruling: case_id only). `group_id` named a row that no longer exists.
	// `alert_id` looked harmless and was not: an Alert has MANY Cases, so the
	// endpoint had to pick one, and it picked by resolving the alert's group — which
	// is the confidently-wrong answer this endpoint's own comment refused. A Case is
	// the conversation, so naming the Case is the only unambiguous question.
	CaseID *uuid.UUID `json:"case_id,omitempty"`
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

// NullableUUID is a contract field typed as `string(uuid) | null`, where an
// explicit `null` means CLEAR and an omitted field means leave it alone.
//
// ⭐ THE DISTINCTION IS THE WHOLE REASON THE TYPE EXISTS. Putting a policy back
// on oto's built-in card is an operation an operator performs deliberately, and
// `"template_id": null` is how they say it. A plain `*uuid.UUID` cannot tell that
// request from one that never mentioned the field.
type NullableUUID struct {
	Set   bool
	Value *uuid.UUID
}

// UnmarshalJSON records presence as well as value.
func (n *NullableUUID) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var v uuid.UUID
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

// MarshalJSON renders the field back, for symmetry.
func (n NullableUUID) MarshalJSON() ([]byte, error) {
	if !n.Set || n.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*n.Value)
}
