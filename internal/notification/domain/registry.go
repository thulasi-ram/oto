package domain

import (
	"time"

	"github.com/google/uuid"
)

// ⚠️ WHY THE CONFIG-SIDE COMMANDS AND FILTERS LIVE IN `domain`.
//
// `internal/notification/api` declares the ports it needs and the repository
// satisfies them, and neither may import the other (CONTEXT.md §5.1). The shared
// vocabulary therefore has to live in the one package both are permitted to name.

// ⛔ BINDING (SCOPE-BOUNDARY §5.3, SPEC §G.9.1). Nothing in this file may gain a
// `UserIDs`, `TeamIDs`, `ScheduleID`, `Rotation`, `TimeOfDay`, `DaysOfWeek` or
// `Timezone` field, and `UnackedReminderAfter` may never become a slice. A policy
// routes a FACT to a DESTINATION; a policy that routes to a PERSON is a rota, and
// a rota is how oto stops being a flight recorder (FR-1, H-1).

// PolicyDraft creates one routing rule.
//
// It is not a `Policy` because id and timestamps are server-owned. Every other
// field is the operator's, and the pointers mark the ones with a published
// default: absent means "the documented default", which is not always the Go
// zero value — a policy created silently disabled would notify nobody and look
// fine.
type PolicyDraft struct {
	// ID lets the caller NAME the row before it exists. Zero means the repository
	// mints one.
	//
	// ⭐ IT EXISTS FOR THE `Idempotency-Key` CLAIM, which has to record the id of
	// what a create made in the SAME transaction as the insert — and therefore
	// before the insert, because a retry that inserted first would hit
	// `policies_name_uniq` and be answered with a name conflict rather than with
	// the policy the caller already created (ticket a6cc834).
	ID   uuid.UUID
	Name string
	// Priority orders evaluation, LOWER FIRST. Nil means the DDL default of 100.
	Priority *int
	// Enabled defaults to true.
	Enabled *bool

	Matchers []Matcher
	Reasons  []Reason
	// ChannelIDs references `channels` and NOTHING ELSE.
	ChannelIDs []uuid.UUID
	Throttle   *Throttle

	// Subjects is `subject_kinds` (migration `00072`). Nil AND empty both mean the
	// column stays at its `'{}'` default, which is "every subject kind" — the
	// shipped behaviour and the state of every row written before 00072.
	//
	// ⭐ IT IS A SLICE AND NOT A POINTER TO ONE, unlike the two digest scalars
	// below, because it has no third state. A digest window distinguishes "leave the
	// column alone" from "set it to NULL"; an empty binding and an absent binding are
	// the SAME instruction — claim every altitude — so a pointer would introduce a
	// distinction the column cannot hold and the wire would have to invent a spelling
	// for.
	Subjects SubjectBinding

	// CountMin and CountWindow are the two halves of `CountOverWindow` (migration
	// `00072`). Nil means the column stays NULL, which is "this policy carries no
	// count condition" — the shipped default.
	//
	// ⭐ TWO SCALARS AND NOT A `*CountOverWindow`, for the reason the digest gives
	// below: the wire is two scalars, the columns are two nullable columns, and
	// `Policy.Validate` reports violations against those two field paths. A nested
	// command object would have to be flattened at the boundary and the flattening is
	// where the two spellings would drift.
	CountMin    *int
	CountWindow *time.Duration

	// DigestWindow and DigestFloor are the two halves of `Digest` (migration
	// 00058). Nil means the column stays NULL, which is "this policy sends no
	// digest" — the shipped default.
	//
	// ⭐ THEY ARE TWO SCALARS AND NOT A `*Digest`, because the wire is two scalars
	// (`digest_window_seconds`, `digest_floor`), the columns are two nullable
	// columns, and `Policy.Validate` reports violations against those two field
	// paths. A nested command object would have to be flattened at the boundary and
	// the flattening is where the two spellings would drift.
	//
	// ⛔ NEITHER IS A SCHEDULE. The window selects which FACTS a summary covers
	// (digest.go's binding block); it has no timezone and never will.
	DigestWindow *time.Duration
	DigestFloor  *int
}

// PolicyPatch is the partial update.
//
// Every field is a pointer so that "absent" and "set to the zero value" are
// different requests. `Throttle` and `UnackedReminderAfter` are double pointers
// because the contract types both as nullable: a pointer to nil CLEARS, which is
// how an operator turns a damper off.
type PolicyPatch struct {
	Name       *string
	Priority   *int
	Enabled    *bool
	Matchers   *[]Matcher
	Reasons    *[]Reason
	ChannelIDs *[]uuid.UUID

	Throttle **Throttle
	// DigestWindow and DigestFloor are double pointers for the reason Throttle is:
	// the contract types both as nullable and a pointer to nil CLEARS the column,
	// which is how an operator turns the summary — or just its floor — off.
	DigestWindow **time.Duration
	DigestFloor  **int

	// Subjects is a SINGLE pointer where its neighbours are double, and the
	// asymmetry is the column's rather than an oversight. `subject_kinds` is
	// `NOT NULL DEFAULT '{}'` — there is no NULL to set — so an empty slice already
	// spells "claim every altitude" and a second level of pointer would be a way to
	// ask for a state that does not exist. `{"subject_kinds": []}` is how an operator
	// removes a binding, and `{"subject_kinds": null}` is refused by the contract
	// rather than silently meaning the same thing.
	Subjects *SubjectBinding

	// CountMin and CountWindow are double pointers for the reason the digest's two
	// halves are: the contract types both as nullable and a pointer to nil CLEARS the
	// column, which is how an operator turns the condition off.
	//
	// ⚠️ THEY ARE SEPARATELY NULLABLE AND THAT IS NOT A LICENCE TO CLEAR ONE.
	// `policies_count_pair_ck` is symmetric — neither half means anything alone — so
	// clearing exactly one is a 23514 the merged validation catches first. They are
	// two pointers because the WIRE is two fields, not because half a condition is a
	// state.
	CountMin    **int
	CountWindow **time.Duration
}

// IsEmpty reports whether the patch would change nothing.
func (p PolicyPatch) IsEmpty() bool {
	return p.Name == nil && p.Priority == nil && p.Enabled == nil &&
		p.Matchers == nil && p.Reasons == nil && p.ChannelIDs == nil &&
		p.Throttle == nil &&
		p.DigestWindow == nil && p.DigestFloor == nil &&
		p.Subjects == nil && p.CountMin == nil && p.CountWindow == nil
}

// DefaultPolicyPriority mirrors the `notification_policies.priority` DDL default.
const DefaultPolicyPriority = 100

// NotificationFilter narrows the intent audit (§E.2, `GET /notifications`).
//
// Every dimension is optional and an empty value means "no constraint", which is
// what makes the default list the whole world rather than an accidental subset.
type NotificationFilter struct {
	Statuses []Status
	Reasons  []Reason
	// SuppressedReasons IMPLIES Statuses = [suppressed]: asking why something was
	// suppressed and being shown delivered rows would be nonsense.
	SuppressedReasons []SuppressedReason

	GroupID  uuid.UUID
	AlertID  uuid.UUID
	PolicyID uuid.UUID

	Since time.Time
	Until time.Time

	// FilterHash binds a keyset cursor to the filter it was minted under (§E.1).
	FilterHash string
}

// DeliveryFilter narrows the delivery audit (§E.2, `GET /deliveries`).
type DeliveryFilter struct {
	Statuses     []DeliveryStatus
	ErrorClasses []ErrorClass
	Modes        []Mode

	ChannelID      uuid.UUID
	NotificationID uuid.UUID

	// Ambiguous is tri-state: nil means "both". `true` finds the messages oto
	// re-sent after a crash and deliberately labelled as possible duplicates.
	Ambiguous *bool

	Since time.Time
	Until time.Time

	FilterHash string
}

// DeliveryContext is the joined-in description of where a delivery went.
//
// The delivery row stores only `channel_id`, and a deliveries list that showed a
// column of UUIDs would be unreadable. It is a separate struct rather than fields
// on `Delivery` because it is a JOIN, not part of the entity: the delivery does
// not own the channel's name and must not appear to.
type DeliveryContext struct {
	ChannelName string
	ChannelType ChannelType
}
