package domain

import (
	"maps"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/validate"
)

// Bounds on an AlertEvent, mirroring the §D.4 CHECKs.
const (
	// MaxEventSummaryBytes bounds the pre-rendered timeline one-liner (ev_summary_ck).
	MaxEventSummaryBytes = 500
	// MaxDedupeKeyBytes bounds an event dedupe key (ev_dedupe_ck).
	MaxDedupeKeyBytes = 200
)

// EventType is the closed enum of alert_events.type (SPEC §D.4.1).
//
// Adding a type requires a SPEC amendment. Implementers MUST NOT invent types:
// the timeline is the product, and an unrecognised type renders as nothing.
type EventType struct{ s string }

// The closed EventType set (SPEC §D.4.1).
var (
	// EventAlertCreated records the first sighting of an alert_key.
	EventAlertCreated = EventType{"alert.created"}
	// EventAlertMutated records a material change on a repeat observation.
	EventAlertMutated = EventType{"alert.mutated"}
	// EventAlertFlappingStarted records the Alert crossing flap_threshold.
	EventAlertFlappingStarted = EventType{"alert.flapping_started"}
	// EventAlertFlappingEnded records the Alert settling.
	EventAlertFlappingEnded = EventType{"alert.flapping_ended"}

	// EventOccurrenceOpened records a new firing episode (T1, T7).
	EventOccurrenceOpened = EventType{"occurrence.opened"}
	// EventOccurrenceReopened records a re-fire inside refire_grace (T8).
	EventOccurrenceReopened = EventType{"occurrence.reopened"}
	// EventOccurrenceSuppressed records the reconciler seeing suppression (T3).
	EventOccurrenceSuppressed = EventType{"occurrence.suppressed"}
	// EventOccurrenceUnsuppressed records suppression lifting (T4).
	EventOccurrenceUnsuppressed = EventType{"occurrence.unsuppressed"}
	// EventOccurrenceResolved records an explicit upstream resolution (T5).
	EventOccurrenceResolved = EventType{"occurrence.resolved"}
	// EventOccurrenceExpired records the reaper sweeping an occurrence (T6).
	EventOccurrenceExpired = EventType{"occurrence.expired"}
	// EventOccurrenceAcknowledged records a human taking the occurrence (T9).
	EventOccurrenceAcknowledged = EventType{"occurrence.acknowledged"}
	// EventOccurrenceUnacknowledged records an ack being dropped (T10).
	EventOccurrenceUnacknowledged = EventType{"occurrence.unacknowledged"}

	// EventGroupOpened records a new AlertGroup generation.
	EventGroupOpened = EventType{"group.opened"}
	// EventGroupClosed records a generation closing after group_close_delay.
	EventGroupClosed = EventType{"group.closed"}
	// EventGroupMemberJoined records an occurrence joining a generation.
	EventGroupMemberJoined = EventType{"group.member_joined"}
	// EventGroupMemberLeft records an occurrence leaving a generation.
	EventGroupMemberLeft = EventType{"group.member_left"}
	// EventGroupStormStarted records a generation entering storm mode.
	EventGroupStormStarted = EventType{"group.storm_started"}
	// EventGroupStormEnded records storm mode ending after storm_cooldown.
	EventGroupStormEnded = EventType{"group.storm_ended"}

	// EventRuleSnapshotCaptured records a RuleSnapshot being bound to an occurrence.
	EventRuleSnapshotCaptured = EventType{"rule.snapshot_captured"}
	// EventRuleDefinitionChanged records rule drift — the headline differentiator.
	EventRuleDefinitionChanged = EventType{"rule.definition_changed"}
	// EventRuleLookupFailed records a rule fetch that did not succeed.
	EventRuleLookupFailed = EventType{"rule.lookup_failed"}

	// EventEnrichmentCompleted records an Enricher producing a result.
	EventEnrichmentCompleted = EventType{"enrichment.completed"}
	// EventEnrichmentFailed records an Enricher failing or timing out.
	EventEnrichmentFailed = EventType{"enrichment.failed"}

	// EventNotificationCreated records an intent to communicate one fact.
	EventNotificationCreated = EventType{"notification.created"}
	// EventNotificationSuppressed records a notification deliberately not sent.
	EventNotificationSuppressed = EventType{"notification.suppressed"}

	// EventDeliverySent records a message landing on a Channel.
	EventDeliverySent = EventType{"delivery.sent"}
	// EventDeliveryUpdated records a message being amended in place.
	EventDeliveryUpdated = EventType{"delivery.updated"}
	// EventDeliveryFailed records a retryable delivery failure.
	EventDeliveryFailed = EventType{"delivery.failed"}
	// EventDeliverySkipped records a delivery deliberately skipped.
	EventDeliverySkipped = EventType{"delivery.skipped"}
	// EventDeliveryDead records a delivery abandoned. oto's silence must never be
	// indistinguishable from "no alert", so this is a first-class timeline fact.
	EventDeliveryDead = EventType{"delivery.dead"}

	// EventCommentAdded records a human comment.
	EventCommentAdded = EventType{"comment.added"}

	// EventSourceUnreachable records an AlertSource going dark. While it is dark
	// the reaper is BLOCKED (§B.4).
	EventSourceUnreachable = EventType{"source.unreachable"}
	// EventSourceRecovered records an AlertSource coming back.
	EventSourceRecovered = EventType{"source.recovered"}
	// EventSourceClockSkew records measured upstream clock skew (C12).
	EventSourceClockSkew = EventType{"source.clock_skew"}
)

var eventTypes = map[string]struct{}{}

func init() {
	for _, t := range AllEventTypes() {
		eventTypes[t.s] = struct{}{}
	}
}

// AllEventTypes returns the closed enum in declaration order. The timeline
// renderer and the DDL CHECK are both generated from this list.
func AllEventTypes() []EventType {
	return []EventType{
		EventAlertCreated, EventAlertMutated, EventAlertFlappingStarted, EventAlertFlappingEnded,
		EventOccurrenceOpened, EventOccurrenceReopened, EventOccurrenceSuppressed,
		EventOccurrenceUnsuppressed, EventOccurrenceResolved, EventOccurrenceExpired,
		EventOccurrenceAcknowledged, EventOccurrenceUnacknowledged,
		EventGroupOpened, EventGroupClosed, EventGroupMemberJoined, EventGroupMemberLeft,
		EventGroupStormStarted, EventGroupStormEnded,
		EventRuleSnapshotCaptured, EventRuleDefinitionChanged, EventRuleLookupFailed,
		EventEnrichmentCompleted, EventEnrichmentFailed,
		EventNotificationCreated, EventNotificationSuppressed,
		EventDeliverySent, EventDeliveryUpdated, EventDeliveryFailed,
		EventDeliverySkipped, EventDeliveryDead,
		EventCommentAdded,
		EventSourceUnreachable, EventSourceRecovered, EventSourceClockSkew,
	}
}

// NewEventType parses a persisted event type against the closed set.
func NewEventType(s string) (EventType, error) {
	if _, ok := eventTypes[s]; !ok {
		return EventType{}, errs.Newf(errs.KindValidation, "enum",
			"%q is not a known alert_events.type; adding one requires a SPEC amendment", s)
	}
	return EventType{s: s}, nil
}

// String renders the event type.
func (t EventType) String() string { return t.s }

// IsZero reports whether the event type is unset.
func (t EventType) IsZero() bool { return t.s == "" }

// Event is an AlertEvent: an immutable record of one thing that happened at one
// instant. It is never updated and never deleted — it is aged out by dropping a
// partition. This is the timeline, and it is what makes oto's history honest:
// current state is a projection, never the only record.
type Event struct {
	id           uuid.UUID
	orgID        uuid.UUID
	alertID      uuid.UUID
	occurrenceID uuid.UUID
	groupID      uuid.UUID
	typ          EventType
	at           ObservationTime
	actor        Actor
	summary      string
	payload      map[string]any
	dedupeKey    string
}

// EventParams is the full constructor input for an AlertEvent. A zero UUID in
// AlertID, OccurrenceID or GroupID means "not about that subject"; at least one
// must be set (ev_subject_ck).
type EventParams struct {
	ID           uuid.UUID
	OrgID        uuid.UUID
	AlertID      uuid.UUID
	OccurrenceID uuid.UUID
	GroupID      uuid.UUID
	Type         EventType
	At           ObservationTime
	Actor        Actor
	Summary      string
	Payload      map[string]any

	// DedupeKey makes an append idempotent through the unpartitioned
	// alert_event_keys table (C.8) — for example "occ:{occurrence_id}:opened".
	// It is optional; an empty key means "always append".
	DedupeKey string
}

// NewEvent builds an immutable AlertEvent, enforcing every §D.4 invariant.
func NewEvent(p EventParams) (Event, error) {
	if err := requireID("event id", p.ID); err != nil {
		return Event{}, err
	}
	if err := requireID("org_id", p.OrgID); err != nil {
		return Event{}, err
	}
	if p.Type.IsZero() {
		return Event{}, errs.New(errs.KindValidation, "required", "event type is required")
	}
	if p.AlertID == uuid.Nil && p.OccurrenceID == uuid.Nil && p.GroupID == uuid.Nil {
		return Event{}, errs.New(errs.KindValidation, "required",
			"an event must name at least one of alert, occurrence or group")
	}
	if p.At.IsZero() {
		return Event{}, errs.New(errs.KindValidation, "required",
			"an event carries both occurred_at and recorded_at")
	}
	if p.Actor.IsZero() {
		return Event{}, errs.New(errs.KindValidation, "required", "event actor is required")
	}

	summary := strings.TrimSpace(p.Summary)
	if summary == "" {
		return Event{}, errs.New(errs.KindValidation, "not_blank", "event summary must not be blank")
	}
	if len(summary) > MaxEventSummaryBytes {
		return Event{}, errs.Newf(errs.KindValidation, "max_length",
			"event summary must have at most %d characters", MaxEventSummaryBytes)
	}
	if l := len(p.DedupeKey); l > MaxDedupeKeyBytes {
		return Event{}, errs.Newf(errs.KindValidation, "max_length",
			"dedupe_key must have at most %d characters", MaxDedupeKeyBytes)
	}
	if !validate.EventTypeRe.MatchString(p.Type.s) {
		return Event{}, errs.Newf(errs.KindInternal, "event_type_shape",
			"event type %q does not match %s", p.Type.s, validate.PatternEventType)
	}

	return Event{
		id:           p.ID,
		orgID:        p.OrgID,
		alertID:      p.AlertID,
		occurrenceID: p.OccurrenceID,
		groupID:      p.GroupID,
		typ:          p.Type,
		at:           p.At,
		actor:        p.Actor,
		summary:      summary,
		payload:      maps.Clone(p.Payload),
		dedupeKey:    p.DedupeKey,
	}, nil
}

// ID is the event's uuidv7 — time-sortable, so it is also the ordering tiebreak.
func (e Event) ID() uuid.UUID { return e.id }

// OrgID is the tenant this event belongs to.
func (e Event) OrgID() uuid.UUID { return e.orgID }

// AlertID is the Alert this event is about, or uuid.Nil.
func (e Event) AlertID() uuid.UUID { return e.alertID }

// OccurrenceID is the AlertOccurrence this event is about, or uuid.Nil.
func (e Event) OccurrenceID() uuid.UUID { return e.occurrenceID }

// GroupID is the AlertGroup generation this event is about, or uuid.Nil.
func (e Event) GroupID() uuid.UUID { return e.groupID }

// Type is the closed-enum kind of fact this event records.
func (e Event) Type() EventType { return e.typ }

// At carries both clocks: display OccurredAt, order by RecordedAt (C12).
func (e Event) At() ObservationTime { return e.at }

// OccurredAt is the upstream claim. The UI displays this.
func (e Event) OccurredAt() time.Time { return e.at.occurredAt }

// RecordedAt is oto's clock. Timelines order by this.
func (e Event) RecordedAt() time.Time { return e.at.recordedAt }

// Actor is who or what caused the event.
func (e Event) Actor() Actor { return e.actor }

// Summary is the pre-rendered one-liner the timeline shows.
func (e Event) Summary() string { return e.summary }

// Payload is a copy of the event's structured detail.
func (e Event) Payload() map[string]any { return maps.Clone(e.payload) }

// DedupeKey is the idempotency key for the append, or "" for none (C.8).
func (e Event) DedupeKey() string { return e.dedupeKey }
