package domain

import (
	"maps"
	"sort"
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

// DedupeKeyRetention is how long a claimed C.8 key stays claimed in
// `alert_event_keys` — the horizon SPEC §D.4 and the table's own comment have
// always named, and which nothing enforced until `retention.prune` did.
//
// ⭐ IT IS THE PRIMARY NUMBER, NOT A DERIVED ONE, and the direction matters
// because five places state the coupling the other way round.
// `tuning.DefaultRawRetention` is 30 days BECAUSE THIS IS: SPEC acceptance
// criterion 36 says replaying a stored `ingest_batch` after a parser fix must
// reproduce the same state without duplicate Slack messages, and that replay is
// idempotent only while the batch's event keys are still claimed here. A raw
// payload kept past this horizon is a payload that can no longer be used for the
// one thing it is kept for. Move this and `DefaultRawRetention` moves with it —
// ADR 0024 says so in as many words.
//
// ⛔ IT IS A FLOOR, AND THE SWEEP MAY ONLY WIDEN IT. `raw_retention_days` is a
// per-org setting bounded 1..365, and `partitions.manage` already drops raw
// partitions at the LONGEST window any tenant asked for (ADR 0024, "retention is
// a floor, never a ceiling"). So `retention.prune` deletes a key at the wider of
// this constant and that window: a tenant holding replayable payloads for a year
// must keep for a year the keys that make them replayable, or its every replay
// appends the timeline a second time and reports success while doing it. The
// floor is what stops the reverse — an operator who lowers raw retention to a day
// must not thereby unclaim the keys of episodes that are still open, whose
// transitions the reconciler re-applies and which nothing but this table dedupes.
const DedupeKeyRetention = 30 * 24 * time.Hour

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

	// EventCaseOpened records a new firing episode (T1, T7).
	EventCaseOpened = EventType{"case.opened"}
	// EventCaseReopened recorded a re-fire inside refire_grace (T8).
	// ⛔ RETIRED — see `retiredEventTypes`. Nothing appends it.
	EventCaseReopened = EventType{"case.reopened"}
	// EventCaseSuppressed records the reconciler seeing suppression (T3).
	EventCaseSuppressed = EventType{"case.suppressed"}
	// EventCaseUnsuppressed records suppression lifting (T4).
	EventCaseUnsuppressed = EventType{"case.unsuppressed"}
	// EventCaseResolved records an explicit upstream resolution (T5).
	EventCaseResolved = EventType{"case.resolved"}
	// EventCaseExpired records the reaper sweeping a case (T6).
	EventCaseExpired = EventType{"case.expired"}
	// EventCaseAcknowledged records a human taking the case (T9).
	EventCaseAcknowledged = EventType{"case.acknowledged"}
	// EventCaseUnacknowledged records an ack being dropped (T10).
	EventCaseUnacknowledged = EventType{"case.unacknowledged"}

	// EventAlertSnoozed records a human asking oto to be quiet about this Alert
	// until a fixed time (§B.8.5, T15). Payload: snooze_id, until, note,
	// duration_seconds. It is a fact about OTO'S NOTIFICATION BEHAVIOUR — the
	// signal's state, ack state and severity are untouched.
	EventAlertSnoozed = EventType{"alert.snoozed"}
	// EventAlertUnsnoozed records a snooze ending (§B.8.5, T15). Payload:
	// snooze_id, reason ∈ {manual, expired, superseded}. Actor is `user` for
	// manual and superseded, `system` for expired.
	EventAlertUnsnoozed = EventType{"alert.unsnoozed"}

	// EventGroupOpened records a new AlertGroup generation.
	EventGroupOpened = EventType{"group.opened"}
	// EventGroupClosed records a generation closing after group_close_delay.
	EventGroupClosed = EventType{"group.closed"}
	// EventGroupMemberJoined recorded a case joining a generation.
	// ⛔ RETIRED — see `retiredEventTypes`. Nothing appends it.
	EventGroupMemberJoined = EventType{"group.member_joined"}
	// EventGroupMemberLeft recorded a case leaving a generation.
	// ⛔ RETIRED — see `retiredEventTypes`. Nothing appends it, and nothing ever
	// did in production: `Leave` was implemented at three layers and called from
	// nowhere, so this type was declared, validated, and absent from every
	// timeline oto has ever rendered.
	EventGroupMemberLeft = EventType{"group.member_left"}
	// EventGroupStormStarted records a generation entering storm mode.
	EventGroupStormStarted = EventType{"group.storm_started"}
	// EventGroupStormEnded records storm mode ending after storm_cooldown.
	EventGroupStormEnded = EventType{"group.storm_ended"}

	// EventRuleSnapshotCaptured records a RuleSnapshot being bound to a case.
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

// retiredEventTypes are values `alert_events` still CONTAINS but which nothing
// may append any more.
//
// ⭐⭐ RETIRED IS NOT DELETED, AND THE DIFFERENCE IS THIRTEEN MONTHS LONG.
// Membership stopped being an event when the group key became derived (ADR 0038,
// migration 00051): `group.member_joined` is implied by `case.opened` and
// `group.member_left` by `case.resolved`/`.expired`, and both were facts
// about the EPISODE phrased as if the group were the actor. But `alert_events` is
// append-only, partitioned, and retained thirteen months, and rows carrying these
// two values already exist. Removing them from the closed set would mean
// `NewEventType` REJECTING HISTORY the moment it is read back — a timeline that
// errors rather than renders — so they stay parseable, and they stay in
// `AllEventTypes` and therefore in `components.schemas.AlertEventType`, because a
// value oto can still put on the wire must be a value its own generated client
// can accept.
//
// ⛔ WHAT MAKES "NEVER AGAIN" MECHANICAL RATHER THAN A COMMENT: `alerts/service`
// refuses a retired type at `AppendTimelineEvent`. A comment saying "do not emit
// this" is advice; a refusal at the write path is a guarantee.
//
// ⭐⭐ AND `case.reopened` JOINED THEM FOR THE SAME REASON AT A DIFFERENT LAYER
// (ADR 0040, migration 00054). A Case is strictly terminal now: a re-fire opens
// the NEXT episode, unacknowledged, so there is no T8 and nothing left for the
// value to record. Thirteen months of rows still spell it — in both the `case.`
// and the pre-ADR-0036 `occurrence.` form — so it is retired on exactly the same
// terms: parseable, canonicalising, in `AllEventTypes`, in
// `components.schemas.AlertEventType`, and unwritable.
//
// ⚠️ THE GUARANTEE IS EXACTLY AS WIDE AS THE WRITE PATHS THAT CHECK IT, AND
// `case.reopened` NEEDED A SECOND ONE. The two `group.*` values are emitted from
// ANOTHER MODULE, so every caller that could append one has to come through
// `AppendTimelineEvent`, and one check there covered them. `case.reopened` was
// emitted from INSIDE `alerts`, by the transition table, and those events reach
// the column through `alerts/service.appendEvents` — which `seam.go` documents as
// bypassing the seam. So the refusal is now made TWICE, once at each writer, and
// the transition rows that built the value are gone as well. The third writer,
// `notification/repository`, INSERTs only its own `notification.*`/`delivery.*`
// rows and can no more reach these values than it can invent one.
//
// They leave this file when the last partition holding them is dropped, and not
// before.
var retiredEventTypes = map[string]struct{}{
	EventGroupMemberJoined.s: {},
	EventGroupMemberLeft.s:   {},
	EventCaseReopened.s:      {},
}

// Retired reports whether this type may still be READ but no longer WRITTEN.
func (t EventType) Retired() bool {
	_, ok := retiredEventTypes[t.s]
	return ok
}

// legacySpellings maps the eight strings `alert_events.type` held before ADR 0036
// onto the value that fact has now. It is a translation table, not a vocabulary.
//
// ⭐⭐ A RENAME IS NOT A RETIREMENT, AND THE TWO NEED OPPOSITE TREATMENT.
// `group.member_joined` and `group.member_left` above named a fact that STOPPED
// EXISTING, so they stay on the contract as themselves — a client asking for the
// timeline of a generation from last spring must be able to name what it will get.
// These eight name the SAME fact under a new word: `occurrence.opened` and
// `case.opened` are one transition, T1, spelled twice. So they are canonicalised
// on the way in and never leave the process: `AllEventTypes` — and therefore
// `components.schemas.AlertEventType` — lists thirty-six values, all `case.*`, and
// a client is never asked to learn two names for one thing.
//
// ⛔ WHY THE ROWS WERE NOT REWRITTEN INSTEAD. `alert_events` is monthly-partitioned,
// append-only and retained thirteen months; an `UPDATE` across it inside a goose
// transaction rewrites every lifecycle row oto has, doubles the table under MVCC,
// and still cannot reach a partition detached for cold storage. Migration 00052
// sets that argument out in full and rewrites only the bounded tables.
//
// ⚠️ THE READ IS ONLY HALF OF IT. A predicate — `type IN (…)` — is planned with the
// statement and cannot call this map, so the three filters registered in
// `test/arch.eventTypeSQLSites` spell BOTH forms out, and that gate now judges a
// literal against `AllPersistedEventTypes` rather than `AllEventTypes` because the
// question it asks is "can this string be in the column", not "is it on the wire".
// `?type=` expands through `PersistedSpellings` for the same reason: a filter for
// `case.opened` that quietly stopped matching last year's rows would be the exact
// failure `TestEventTypeSQLNamesLiveValues` exists to prevent, one layer down.
var legacySpellings = map[string]EventType{
	// vocab:allow — strings ON DISK, not vocabulary. This map and the three registered SQL filters are the only places in `internal/` the pre-rename word survives, and it survives as data rather than as a name. The marker reaches two lines, which is why there are three of them rather than one.
	"occurrence.opened": EventCaseOpened, "occurrence.reopened": EventCaseReopened,
	"occurrence.suppressed": EventCaseSuppressed, "occurrence.unsuppressed": EventCaseUnsuppressed,
	// vocab:allow — as above.
	"occurrence.resolved": EventCaseResolved, "occurrence.expired": EventCaseExpired,
	"occurrence.acknowledged": EventCaseAcknowledged, "occurrence.unacknowledged": EventCaseUnacknowledged,
}

// spellingsByType is legacySpellings inverted: canonical value to the strings a
// persisted row may hold for it, canonical first.
var spellingsByType = map[string][]string{}

func init() {
	for legacy, t := range legacySpellings {
		if _, ok := spellingsByType[t.s]; !ok {
			spellingsByType[t.s] = []string{t.s}
		}
		spellingsByType[t.s] = append(spellingsByType[t.s], legacy)
	}
	for _, v := range spellingsByType {
		sort.Strings(v[1:])
	}
}

// PersistedSpellings returns every string `alert_events.type` may hold for this
// fact: the canonical value first, then any pre-rename spelling still on disk.
//
// ⛔ USE IT FOR EVERY PREDICATE ON `alert_events.type`, never `String()`. A filter
// built from the canonical value alone is valid SQL that silently stops matching
// everything written before ADR 0036 — and a read that returns no rows is
// indistinguishable from a fact that never happened.
func (t EventType) PersistedSpellings() []string {
	if all, ok := spellingsByType[t.s]; ok {
		return append([]string(nil), all...)
	}
	return []string{t.s}
}

// AllPersistedEventTypes is every string that may legally appear in
// `alert_events.type`: the closed contract enum plus the pre-rename spellings.
//
// ⚠️ IT IS DELIBERATELY LARGER THAN `AllEventTypes`, and the two answer different
// questions. `AllEventTypes` is what oto PUTS ON THE WIRE and what its generated
// clients must accept. This is what oto may READ OFF DISK, which is what a SQL
// filter has to cover and what `test/arch`'s event-type gate has to judge a
// literal against.
func AllPersistedEventTypes() []string {
	out := make([]string, 0, len(eventTypes)+len(legacySpellings))
	for _, t := range AllEventTypes() {
		out = append(out, t.s)
	}
	for legacy := range legacySpellings {
		out = append(out, legacy)
	}
	sort.Strings(out)
	return out
}

// AllEventTypes returns the closed enum in declaration order.
//
// ⛔ THE DDL IS NOT A SECOND COPY OF THIS LIST, AND MUST NOT BE MISTAKEN FOR
// ONE. `ev_type_ck` is `CHECK (type ~ '^[a-z_]+\.[a-z_]+$')` — a SHAPE, not a
// vocabulary. It admits any `<subject>.<fact>` string, so a value added here and
// nowhere else reaches the database, the timeline and the wire without one
// constraint objecting.
//
// The one other enumeration of these values is `components.schemas.AlertEventType`
// in `api/openapi/openapi.yaml`, which is what every generated client knows.
// Adding a type here means adding it there in the same commit, and both
// `?type=` ceilings — the contract's `maxItems` and the `validate:"max=N"` tags
// on `alerts/api.TimelineQuery` and `grouping/api.TimelineQuery` — are the size
// of this set. `TestContractEnumsMatchTheirDomainEnum` in `test/contract` is
// what fails if the two lists drift; without it the first thing to notice is
// somebody else's generated client refusing a value oto already writes.
//
// ⚠️ IT IS THE SET THAT MAY BE READ, WHICH IS LARGER THAN THE SET THAT MAY BE
// WRITTEN. Two entries — `group.member_joined` and `group.member_left` — are
// RETIRED: nothing appends them, and `AppendTimelineEvent` refuses them, but they
// stay here because thirteen months of `alert_events` still contain them. See
// `retiredEventTypes`.
func AllEventTypes() []EventType {
	return []EventType{
		EventAlertCreated, EventAlertMutated, EventAlertFlappingStarted, EventAlertFlappingEnded,
		EventCaseOpened, EventCaseReopened, EventCaseSuppressed,
		EventCaseUnsuppressed, EventCaseResolved, EventCaseExpired,
		EventCaseAcknowledged, EventCaseUnacknowledged,
		EventAlertSnoozed, EventAlertUnsnoozed,
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
//
// ⚠️ IT CANONICALISES. A row written before ADR 0036 spells the eight lifecycle
// facts `occurrence.*`; this returns the `case.*` value that IS that fact, so no
// caller — and no client — ever sees the earlier word. Rejecting those strings
// instead would make oto error on thirteen months of its own timeline.
func NewEventType(s string) (EventType, error) {
	if _, ok := eventTypes[s]; ok {
		return EventType{s: s}, nil
	}
	if t, ok := legacySpellings[s]; ok {
		return t, nil
	}
	return EventType{}, errs.Newf(errs.KindValidation, "enum",
		"%q is not a known alert_events.type; adding one requires a SPEC amendment", s)
}

// String renders the event type.
func (t EventType) String() string { return t.s }

// MarshalText renders the event type as the string it IS, for every encoder that
// is not `fmt`.
//
// ⛔ WITHOUT THIS, A LOG LINE SAYS `"type":{}`. `EventType`'s only field is
// unexported, and `String()` does not help outside `fmt`: `fmt` consults
// `Stringer`, `encoding/json` does not. So the production JSON handler — which
// hands an `any` attribute to `encoding/json` — encoded a struct with no exported
// fields and emitted an empty object. `rules/service`'s "could not record rule
// event" warning stopped naming WHICH fact was lost the day this became a struct,
// and nothing failed: a blanked log line is invisible to the compiler, to the
// linters and to every test in the tree.
//
// ⚠️ IT IS `MarshalText` AND NOT `MarshalJSON`, ONCE, FOR ALL FOUR CALLERS.
// `slog`'s TextHandler asks for `encoding.TextMarshaler` directly; its JSONHandler
// goes through `encoding/json`, which asks for `json.Marshaler` and then for this;
// `json.Marshal` on any DTO carrying the value does the same and renders it as a
// JSON string. A `MarshalJSON` would have to re-quote and re-escape by hand what
// `encoding/json` already does correctly from these bytes.
//
// ⚠️ THERE IS DELIBERATELY NO `UnmarshalText`. Rendering a closed value is safe;
// PARSING one is `NewEventType`'s job and must stay there, because a decoder that
// could mint an `EventType` from arbitrary wire bytes is the closed set opened
// again — this time from outside the process. Nothing in the tree decodes one: the
// wire and the row are `string` and are parsed at the boundary.
func (t EventType) MarshalText() ([]byte, error) { return []byte(t.s), nil }

// IsZero reports whether the event type is unset.
func (t EventType) IsZero() bool { return t.s == "" }

// Event is an AlertEvent: an immutable record of one thing that happened at one
// instant. It is never updated and never deleted — it is aged out by dropping a
// partition. This is the timeline, and it is what makes oto's history honest:
// current state is a projection, never the only record.
type Event struct {
	id        uuid.UUID
	orgID     uuid.UUID
	alertID   uuid.UUID
	caseID    uuid.UUID
	groupID   uuid.UUID
	typ       EventType
	at        ObservationTime
	actor     Actor
	summary   string
	payload   map[string]any
	dedupeKey string
}

// EventParams is the full constructor input for an AlertEvent. A zero UUID in
// AlertID, CaseID or GroupID means "not about that subject"; at least one
// must be set (ev_subject_ck).
type EventParams struct {
	ID      uuid.UUID
	OrgID   uuid.UUID
	AlertID uuid.UUID
	CaseID  uuid.UUID
	GroupID uuid.UUID
	Type    EventType
	At      ObservationTime
	Actor   Actor
	Summary string
	Payload map[string]any

	// DedupeKey makes an append idempotent through the unpartitioned
	// alert_event_keys table (C.8) — for example "case:{case_id}:opened".
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
	if p.AlertID == uuid.Nil && p.CaseID == uuid.Nil && p.GroupID == uuid.Nil {
		return Event{}, errs.New(errs.KindValidation, "required",
			"an event must name at least one of alert, case or group")
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
		id:        p.ID,
		orgID:     p.OrgID,
		alertID:   p.AlertID,
		caseID:    p.CaseID,
		groupID:   p.GroupID,
		typ:       p.Type,
		at:        p.At,
		actor:     p.Actor,
		summary:   summary,
		payload:   maps.Clone(p.Payload),
		dedupeKey: p.DedupeKey,
	}, nil
}

// ID is the event's uuidv7 — time-sortable, so it is also the ordering tiebreak.
func (e Event) ID() uuid.UUID { return e.id }

// OrgID is the tenant this event belongs to.
func (e Event) OrgID() uuid.UUID { return e.orgID }

// AlertID is the Alert this event is about, or uuid.Nil.
func (e Event) AlertID() uuid.UUID { return e.alertID }

// CaseID is the AlertCase this event is about, or uuid.Nil.
func (e Event) CaseID() uuid.UUID { return e.caseID }

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
