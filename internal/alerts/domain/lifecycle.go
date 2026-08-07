package domain

import (
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Lifecycle defaults (SPEC §B.5, §D.1 org settings). A caller always passes the
// org's configured value; these are what the configuration defaults to.
const (
	// DefaultRefireGrace decides T7 from T8: a re-fire inside this window reopens
	// the existing AlertOccurrence, a re-fire after it opens a new one.
	DefaultRefireGrace = 10 * time.Minute
	// DefaultResolveGrace is how long past `source_ends_at` the reaper waits
	// before an occurrence may expire.
	DefaultResolveGrace = 5 * time.Minute
)

// Trigger is what happened to make the lifecycle machine run. There are exactly
// four, and together with the current State they select a row of the SPEC §B.3
// table.
type Trigger struct{ s string }

// The closed Trigger set.
var (
	// TriggerObserveFiring is an observation that the label set is active and not
	// suppressed. It drives T1, T2, T4, T7 and T8.
	TriggerObserveFiring = Trigger{"observe_firing"}
	// TriggerObserveSuppressed is the reconciler seeing status.state ==
	// "suppressed". It drives T3 and is the ONLY way an occurrence becomes
	// suppressed: Alertmanager's MuteStage drops suppressed alerts before the
	// webhook, so ingest can never see this (C1).
	TriggerObserveSuppressed = Trigger{"observe_suppressed"}
	// TriggerObserveResolved is an explicit per-alert status == "resolved". It
	// drives T5 and is the ONLY way an occurrence becomes resolved (C2).
	TriggerObserveResolved = Trigger{"observe_resolved"}
	// TriggerReap is the occurrence.reap job finding an occurrence oto has
	// stopped hearing about. It drives T6 and is the ONLY way an occurrence
	// becomes expired.
	TriggerReap = Trigger{"reap"}
)

// NewTrigger parses a trigger name.
func NewTrigger(s string) (Trigger, error) {
	switch s {
	case TriggerObserveFiring.s, TriggerObserveSuppressed.s,
		TriggerObserveResolved.s, TriggerReap.s:
		return Trigger{s: s}, nil
	default:
		return Trigger{}, errs.Newf(errs.KindValidation, "enum", "%q is not a lifecycle trigger", s)
	}
}

// String renders the trigger.
func (t Trigger) String() string { return t.s }

// IsZero reports whether the trigger is unset.
func (t Trigger) IsZero() bool { return t.s == "" }

// TransitionID names one row of the SPEC §B.3 transition table. It is carried on
// the result so that a caller, a log line and a test can all say which edge ran.
type TransitionID struct{ s string }

// The rows of SPEC §B.3 that the occurrence state machine owns. T9–T14 are ack,
// enrichment, rule, notification and comment facts; T9 and T10 are implemented as
// Acknowledge and Unacknowledge, the rest are not state transitions at all.
var (
	// TransitionT1 opens the first occurrence of an alert_key.
	TransitionT1 = TransitionID{"T1"}
	// TransitionT2 is a repeat observation of an already-firing occurrence.
	TransitionT2 = TransitionID{"T2"}
	// TransitionT3 suppresses a firing occurrence. Reconciler only.
	TransitionT3 = TransitionID{"T3"}
	// TransitionT4 unsuppresses a suppressed occurrence. Reconciler only.
	TransitionT4 = TransitionID{"T4"}
	// TransitionT5 resolves an occurrence on an explicit upstream observation.
	TransitionT5 = TransitionID{"T5"}
	// TransitionT6 expires an occurrence oto has stopped hearing about.
	TransitionT6 = TransitionID{"T6"}
	// TransitionT7 opens a NEW occurrence after a re-fire beyond refire_grace.
	TransitionT7 = TransitionID{"T7"}
	// TransitionT8 reopens the SAME occurrence after a re-fire inside refire_grace.
	TransitionT8 = TransitionID{"T8"}
	// TransitionT9 acknowledges an occurrence.
	TransitionT9 = TransitionID{"T9"}
	// TransitionT10 drops an acknowledgement.
	TransitionT10 = TransitionID{"T10"}
)

// String renders the transition id.
func (t TransitionID) String() string { return t.s }

// refirePolicy distinguishes the two rows that share (terminal → firing, observe
// firing): T8 inside refire_grace and T7 beyond it.
type refirePolicy int

const (
	refireNotApplicable refirePolicy = iota
	refireWithinGrace
	refireAfterGrace
)

// transitionRule is one row of SPEC §B.3.
type transitionRule struct {
	from    State
	to      State
	trigger Trigger
	id      TransitionID
	refire  refirePolicy
	// actors is the closed set of actors permitted to drive this edge. An actor
	// outside it is a programming error, not a caller error (§L.4 invariant 2).
	actors []ActorKind
	// event is the AlertEvent this edge appends; the zero EventType means the
	// edge appends nothing unless something material changed.
	event EventType
	// opensNewOccurrence marks T7: the current occurrence is untouched and the
	// caller must open a new one.
	opensNewOccurrence bool
}

// transitionTable IS SPEC §B.3. Adding an edge means editing this table and the
// SPEC in the same commit; there are no `if`s anywhere else that move a state.
var transitionTable = []transitionRule{
	{
		from: StateNone, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT1, actors: []ActorKind{ActorIngest, ActorReconciler},
		event: EventOccurrenceOpened,
	},
	{
		from: StateFiring, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT2, actors: []ActorKind{ActorIngest, ActorReconciler},
		// No event unless a material field changed, in which case alert.mutated.
	},
	{
		from: StateFiring, to: StateSuppressed, trigger: TriggerObserveSuppressed,
		id: TransitionT3, actors: []ActorKind{ActorReconciler},
		event: EventOccurrenceSuppressed,
	},
	{
		from: StateSuppressed, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT4, actors: []ActorKind{ActorReconciler},
		event: EventOccurrenceUnsuppressed,
	},
	{
		from: StateFiring, to: StateResolved, trigger: TriggerObserveResolved,
		id: TransitionT5, actors: []ActorKind{ActorIngest},
		event: EventOccurrenceResolved,
	},
	{
		from: StateSuppressed, to: StateResolved, trigger: TriggerObserveResolved,
		id: TransitionT5, actors: []ActorKind{ActorIngest},
		event: EventOccurrenceResolved,
	},
	{
		from: StateFiring, to: StateExpired, trigger: TriggerReap,
		id: TransitionT6, actors: []ActorKind{ActorReaper},
		event: EventOccurrenceExpired,
	},
	{
		from: StateSuppressed, to: StateExpired, trigger: TriggerReap,
		id: TransitionT6, actors: []ActorKind{ActorReaper},
		event: EventOccurrenceExpired,
	},
	{
		from: StateResolved, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT8, refire: refireWithinGrace, actors: []ActorKind{ActorIngest},
		event: EventOccurrenceReopened,
	},
	{
		from: StateExpired, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT8, refire: refireWithinGrace, actors: []ActorKind{ActorIngest},
		event: EventOccurrenceReopened,
	},
	{
		from: StateResolved, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT7, refire: refireAfterGrace, actors: []ActorKind{ActorIngest},
		opensNewOccurrence: true,
	},
	{
		from: StateExpired, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT7, refire: refireAfterGrace, actors: []ActorKind{ActorIngest},
		opensNewOccurrence: true,
	},
}

// CanTransition reports whether SPEC §B.3 has an edge from one state to another
// under a trigger. It answers the shape of the machine only: whether a PARTICULAR
// transition is legal right now also depends on the actor, the reaper guard and
// the re-fire grace, all of which Apply checks.
func CanTransition(from, to State, trigger Trigger) bool {
	for _, r := range transitionTable {
		if r.from == from && r.to == to && r.trigger == trigger {
			return true
		}
	}
	return false
}

// TransitionsFrom returns every state reachable from one state under a trigger.
func TransitionsFrom(from State, trigger Trigger) []State {
	var out []State
	for _, r := range transitionTable {
		if r.from == from && r.trigger == trigger && !containsState(out, r.to) {
			out = append(out, r.to)
		}
	}
	return out
}

func containsState(xs []State, s State) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// TransitionCommand is the complete input to the lifecycle machine. Every clock
// reading arrives as a parameter: the domain never calls time.Now().
type TransitionCommand struct {
	// Trigger and Actor together select and authorise the edge.
	Trigger Trigger
	Actor   Actor

	// At carries the upstream claim (occurred_at) and oto's clock (recorded_at).
	At ObservationTime

	// EventID is the uuidv7 for the AlertEvent this transition may append. Mint
	// one per call; it is unused when the edge appends nothing.
	EventID uuid.UUID

	// SuppressionReason is REQUIRED by T3 and forbidden on every other edge.
	SuppressionReason SuppressionReason

	// RefireGrace decides T8 from T7 (§B.5). Zero means DefaultRefireGrace.
	RefireGrace time.Duration

	// SourceEndsAt is Alertmanager's `endsAt` for this observation, zero when no
	// end time is known.
	SourceEndsAt time.Time
	// SourceUpdatedAt is Alertmanager's `updatedAt`, zero when unknown.
	SourceUpdatedAt time.Time

	// ResolveGrace is how long past SourceEndsAt the reaper waits before T6.
	// Zero means DefaultResolveGrace.
	ResolveGrace time.Duration

	// SourceHealthy gates T6 and nothing else. THE REAPER GUARD (§B.4) IS THE
	// HIGHEST-VALUE CORRECTNESS RULE IN THE SYSTEM: losing sight of an alert is
	// not the same as the alert resolving, so an occurrence whose AlertSource is
	// not healthy is held in its current state and never expired.
	SourceHealthy bool

	// MaterialChange reports whether a repeat observation changed severity, an
	// annotation, the generator URL or the bound rule fingerprint. Only then does
	// T2 append `alert.mutated` (§B.3).
	MaterialChange bool

	// Value is the sample value carried by the observation, when there is one.
	Value *float64
	// ObservedSkew is received_at - startsAt for this observation (C12).
	ObservedSkew time.Duration

	// Summary is the pre-rendered timeline one-liner. An empty Summary makes the
	// machine render a deterministic default.
	Summary string
	// Payload is the structured detail appended to the event.
	Payload map[string]any
}

// TransitionResult is what the lifecycle machine produced.
type TransitionResult struct {
	// ID names the SPEC §B.3 row that ran.
	ID TransitionID
	// From and To are the occurrence states either side of the edge.
	From State
	To   State
	// Occurrence is the updated AlertOccurrence. On T7 it is the UNCHANGED
	// terminal occurrence: T7 opens a new episode, it does not revive an old one.
	Occurrence Occurrence
	// OpensNewOccurrence marks T7. The caller must open a new occurrence with
	// seq+1 and ReopenOf set to Occurrence.ID(), which appends its own
	// `occurrence.opened` event.
	OpensNewOccurrence bool
	// Events are the AlertEvents to append, in order. At most one edge appends
	// more than nothing, so this is empty or a single event.
	Events []Event
}

// Apply runs the SPEC §B.3 state machine over one AlertOccurrence.
//
// It is a total function: every input either produces a result or an error, never
// a panic and never a silent no-op. An edge that does not exist in the table is
// errs.KindPrecondition — the request is valid but the entity is in the wrong
// state. An edge driven by the wrong actor is errs.KindInternal — `suppressed`
// set by ingest, or `expired` set by anything but the reaper, is a programming
// bug, not a caller error (§L.4 invariant 2).
func Apply(o Occurrence, cmd TransitionCommand) (TransitionResult, error) {
	if cmd.Trigger.IsZero() {
		return TransitionResult{}, errs.New(errs.KindValidation, "required", "trigger is required")
	}
	if cmd.Actor.IsZero() {
		return TransitionResult{}, errs.New(errs.KindValidation, "required", "actor is required")
	}
	if cmd.At.IsZero() {
		return TransitionResult{}, errs.New(errs.KindValidation, "required",
			"a transition carries both occurred_at and recorded_at")
	}

	rule, err := selectRule(o, cmd)
	if err != nil {
		return TransitionResult{}, err
	}
	if !permits(rule.actors, cmd.Actor.Kind()) {
		return TransitionResult{}, errs.Newf(errs.KindInternal, "wrong_actor",
			"%s may not drive %s (%s -> %s)", cmd.Actor.Kind(), rule.id, rule.from, rule.to)
	}

	from := o.state
	next := o
	switch rule.id {
	case TransitionT1:
		return TransitionResult{}, errs.New(errs.KindPrecondition, "no_open_occurrence",
			"T1 opens the first occurrence; call OpenOccurrence")

	case TransitionT2:
		next.lastObservedAt = cmd.At.RecordedAt()
		next.observe(cmd)

	case TransitionT3:
		if cmd.SuppressionReason.IsZero() {
			return TransitionResult{}, errs.New(errs.KindValidation, "required",
				"suppression_reason is required to suppress an occurrence")
		}
		next.state = StateSuppressed
		next.suppressionReason = cmd.SuppressionReason
		next.lastObservedAt = cmd.At.RecordedAt()
		next.observe(cmd)

	case TransitionT4:
		next.state = StateFiring
		next.suppressionReason = SuppressionReason{}
		next.lastObservedAt = cmd.At.RecordedAt()
		next.observe(cmd)

	case TransitionT5:
		// T5 sets ended_at from the UPSTREAM claim. A skewed upstream clock could
		// place it before started_at, which occ_order_ck forbids, so it is
		// clamped: skew is measured, never a reason to lose the resolution (C12).
		next.state = StateResolved
		next.resolveReason = ResolveUpstream
		next.suppressionReason = SuppressionReason{}
		next.endedAt = notBefore(cmd.At.OccurredAt(), o.startedAt)
		next.lastObservedAt = cmd.At.RecordedAt()
		next.observe(cmd)

	case TransitionT6:
		if !cmd.SourceHealthy {
			return TransitionResult{}, errs.New(errs.KindPrecondition, "source_not_healthy",
				"an occurrence is held, never expired, while its AlertSource is not healthy")
		}
		if o.sourceEndsAt.IsZero() {
			return TransitionResult{}, errs.New(errs.KindPrecondition, "no_source_ends_at",
				"an occurrence with no upstream end time cannot expire")
		}
		if !cmd.At.RecordedAt().After(o.sourceEndsAt.Add(resolveGrace(cmd))) {
			return TransitionResult{}, errs.New(errs.KindPrecondition, "resolve_grace_not_elapsed",
				"resolve_grace has not elapsed since source_ends_at")
		}
		next.state = StateExpired
		next.resolveReason = ResolveTimeout
		next.suppressionReason = SuppressionReason{}
		next.endedAt = notBefore(cmd.At.RecordedAt(), o.startedAt)

	case TransitionT7:
		// The terminal occurrence is untouched. The caller opens a new episode.
		return TransitionResult{
			ID: rule.id, From: from, To: rule.to,
			Occurrence:         o,
			OpensNewOccurrence: true,
		}, nil

	case TransitionT8:
		next.state = StateFiring
		next.resolveReason = ResolveReason{}
		next.suppressionReason = SuppressionReason{}
		next.endedAt = time.Time{}
		next.reopenCount = o.reopenCount + 1
		next.lastObservedAt = cmd.At.RecordedAt()
		next.observe(cmd)

	default:
		return TransitionResult{}, errs.Newf(errs.KindInternal, "unhandled_transition",
			"transition %s has no implementation", rule.id)
	}

	if err := next.check(); err != nil {
		return TransitionResult{}, err
	}

	res := TransitionResult{ID: rule.id, From: from, To: next.state, Occurrence: next}

	eventType := rule.event
	if rule.id == TransitionT2 {
		if !cmd.MaterialChange {
			return res, nil // a repeat observation that changed nothing is silent
		}
		eventType = EventAlertMutated
	}
	if eventType.IsZero() {
		return res, nil
	}

	ev, err := NewEvent(EventParams{
		ID:           cmd.EventID,
		OrgID:        next.orgID,
		AlertID:      next.alertID,
		OccurrenceID: next.id,
		GroupID:      next.groupID,
		Type:         eventType,
		At:           cmd.At,
		Actor:        cmd.Actor,
		Summary:      summaryOr(cmd.Summary, defaultSummary(rule.id, from, next.state)),
		Payload:      cmd.Payload,
		DedupeKey:    dedupeKeyFor(rule.id, next),
	})
	if err != nil {
		return TransitionResult{}, err
	}
	res.Events = []Event{ev}
	return res, nil
}

// selectRule finds the one §B.3 row that matches the occurrence and the command,
// resolving T7 against T8 by the re-fire grace.
func selectRule(o Occurrence, cmd TransitionCommand) (transitionRule, error) {
	want := refireNotApplicable
	if o.state.IsTerminal() && cmd.Trigger == TriggerObserveFiring {
		if withinRefireGrace(o, cmd) {
			want = refireWithinGrace
		} else {
			want = refireAfterGrace
		}
	}

	for _, r := range transitionTable {
		if r.from != o.state || r.trigger != cmd.Trigger {
			continue
		}
		if r.refire != want {
			continue
		}
		return r, nil
	}
	return transitionRule{}, errs.Newf(errs.KindPrecondition, "illegal_transition",
		"no transition from %q under trigger %q", o.state, cmd.Trigger)
}

// withinRefireGrace decides T8 from T7: a re-fire whose observation lands within
// refire_grace of the occurrence ending REOPENS that occurrence and reuses its
// Slack thread; a later one opens a new episode and a new AlertGroup generation
// (§B.5).
func withinRefireGrace(o Occurrence, cmd TransitionCommand) bool {
	if o.endedAt.IsZero() {
		return false
	}
	grace := cmd.RefireGrace
	if grace <= 0 {
		grace = DefaultRefireGrace
	}
	return !cmd.At.RecordedAt().After(o.endedAt.Add(grace))
}

func resolveGrace(cmd TransitionCommand) time.Duration {
	if cmd.ResolveGrace <= 0 {
		return DefaultResolveGrace
	}
	return cmd.ResolveGrace
}

func permits(actors []ActorKind, actor ActorKind) bool {
	for _, a := range actors {
		if a == actor {
			return true
		}
	}
	return false
}

// observe folds the upstream fields of an observation into the occurrence.
//
// A field the observation did not supply is PRESERVED, never cleared. §L.3.1
// says a zero `endsAt` means "no end time known" for that payload — it does not
// mean "forget the end time you already had", and clearing it would silently
// disable the reaper for that occurrence (occ_reap_idx only sees rows with a
// non-null source_ends_at).
func (o *Occurrence) observe(cmd TransitionCommand) {
	if !cmd.SourceEndsAt.IsZero() {
		o.sourceEndsAt = cmd.SourceEndsAt.UTC()
	}
	if !cmd.SourceUpdatedAt.IsZero() {
		o.sourceUpdatedAt = cmd.SourceUpdatedAt.UTC()
	}
	if cmd.Value != nil {
		v := *cmd.Value
		o.value = &v
	}
	if cmd.ObservedSkew != 0 {
		o.observedSkew = cmd.ObservedSkew
	}
}

func notBefore(t, floor time.Time) time.Time {
	t = t.UTC()
	if t.Before(floor) {
		return floor
	}
	return t
}

func summaryOr(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}

func defaultSummary(id TransitionID, from, to State) string {
	switch id {
	case TransitionT1, TransitionT7:
		return "Occurrence opened"
	case TransitionT2:
		return "Alert details changed"
	case TransitionT3:
		return "Occurrence suppressed"
	case TransitionT4:
		return "Occurrence no longer suppressed"
	case TransitionT5:
		return "Occurrence resolved upstream"
	case TransitionT6:
		return "Occurrence expired: oto stopped hearing about it"
	case TransitionT8:
		return "Occurrence reopened"
	default:
		return "Occurrence moved from " + from.String() + " to " + to.String()
	}
}

// dedupeKeyFor renders the C.8 idempotency key for an event, so that a job
// replayed at least once appends the fact exactly once.
func dedupeKeyFor(id TransitionID, o Occurrence) string {
	switch id {
	case TransitionT1, TransitionT7:
		return "occ:" + o.id.String() + ":opened"
	case TransitionT3:
		return "occ:" + o.id.String() + ":suppressed:" + o.lastObservedAt.UTC().Format(time.RFC3339Nano)
	case TransitionT4:
		return "occ:" + o.id.String() + ":unsuppressed:" + o.lastObservedAt.UTC().Format(time.RFC3339Nano)
	case TransitionT5:
		return "occ:" + o.id.String() + ":resolved"
	case TransitionT6:
		return "occ:" + o.id.String() + ":expired"
	case TransitionT8:
		return "occ:" + o.id.String() + ":reopened:" + strconv.Itoa(o.reopenCount)
	default:
		return ""
	}
}

// OpenOccurrenceParams opens a new AlertOccurrence — SPEC §B.3 T1 (the first
// sighting of an alert_key, or a firing observation with no open occurrence) and
// T7 (a re-fire beyond refire_grace).
type OpenOccurrenceParams struct {
	ID      uuid.UUID
	OrgID   uuid.UUID
	AlertID uuid.UUID
	// GroupID is the AlertGroup generation this occurrence joins, or uuid.Nil
	// until grouping resolves it.
	GroupID uuid.UUID
	// Seq is prev+1, or 1 for the first episode.
	Seq int

	Actor Actor
	At    ObservationTime

	SourceStartsAt  time.Time
	SourceEndsAt    time.Time
	SourceUpdatedAt time.Time

	// ReopenOf is the previous, terminal occurrence when this open follows a T7
	// re-fire. It is uuid.Nil for T1.
	ReopenOf uuid.UUID

	Value        *float64
	ObservedSkew time.Duration

	// EventID is the uuidv7 for the `occurrence.opened` event.
	EventID uuid.UUID
	Summary string
	Payload map[string]any
}

// OpenOccurrence opens a new firing episode and returns it with the
// `occurrence.opened` event to append. A new occurrence always starts unacked:
// T10 says an ack does not survive into a new episode.
func OpenOccurrence(p OpenOccurrenceParams) (Occurrence, []Event, error) {
	if p.Actor.IsZero() {
		return Occurrence{}, nil, errs.New(errs.KindValidation, "required", "actor is required")
	}
	if !permits([]ActorKind{ActorIngest, ActorReconciler}, p.Actor.Kind()) {
		return Occurrence{}, nil, errs.Newf(errs.KindInternal, "wrong_actor",
			"%s may not open an occurrence", p.Actor.Kind())
	}
	if p.At.IsZero() {
		return Occurrence{}, nil, errs.New(errs.KindValidation, "required",
			"opening an occurrence carries both occurred_at and recorded_at")
	}

	starts := p.SourceStartsAt
	if starts.IsZero() {
		starts = p.At.OccurredAt()
	}

	o, err := NewOccurrence(OccurrenceParams{
		ID:              p.ID,
		OrgID:           p.OrgID,
		AlertID:         p.AlertID,
		GroupID:         p.GroupID,
		Seq:             p.Seq,
		State:           StateFiring,
		StartedAt:       p.At.RecordedAt(),
		LastObservedAt:  p.At.RecordedAt(),
		SourceStartsAt:  starts,
		SourceEndsAt:    p.SourceEndsAt,
		SourceUpdatedAt: p.SourceUpdatedAt,
		ReopenOf:        p.ReopenOf,
		AckState:        AckStateUnacked,
		Value:           p.Value,
		ObservedSkew:    p.ObservedSkew,
	})
	if err != nil {
		return Occurrence{}, nil, err
	}

	id := TransitionT1
	if p.ReopenOf != uuid.Nil {
		id = TransitionT7
	}
	ev, err := NewEvent(EventParams{
		ID:           p.EventID,
		OrgID:        o.orgID,
		AlertID:      o.alertID,
		OccurrenceID: o.id,
		GroupID:      o.groupID,
		Type:         EventOccurrenceOpened,
		At:           p.At,
		Actor:        p.Actor,
		Summary:      summaryOr(p.Summary, defaultSummary(id, StateNone, StateFiring)),
		Payload:      p.Payload,
		DedupeKey:    dedupeKeyFor(id, o),
	})
	if err != nil {
		return Occurrence{}, nil, err
	}
	return o, []Event{ev}, nil
}

// AckCommand acknowledges or un-acknowledges an AlertOccurrence (§B.3 T9, T10).
// Acknowledgement is orthogonal to state: an acked alert is still firing.
type AckCommand struct {
	Actor Actor
	At    ObservationTime
	// EventID is the uuidv7 for the appended event.
	EventID uuid.UUID
	// Note is the free-text note left with an acknowledgement, at most
	// MaxAckNoteBytes.
	Note string
	// Reason explains an un-acknowledgement: "manual" or "new_occurrence".
	Reason  string
	Payload map[string]any
}

// Unacknowledge reasons (§B.3 T10).
const (
	// UnackReasonManual is a human dropping their own acknowledgement.
	UnackReasonManual = "manual"
	// UnackReasonNewOccurrence is an ack being dropped because a new episode opened.
	UnackReasonNewOccurrence = "new_occurrence"
)

// Acknowledge records that a human took this occurrence (T9).
//
// Acknowledging a terminal occurrence is errs.KindPrecondition: the request is
// well-formed, the entity is simply in the wrong state. Acknowledgement identity
// IS stored — it is operationally necessary — but oto exposes no per-person
// response-time metric anywhere (R8).
func (o Occurrence) Acknowledge(cmd AckCommand) (Occurrence, []Event, error) {
	if cmd.Actor.IsZero() || !cmd.Actor.Kind().IsHuman() {
		return Occurrence{}, nil, errs.New(errs.KindValidation, "required",
			"an acknowledgement requires a human actor")
	}
	if cmd.At.IsZero() {
		return Occurrence{}, nil, errs.New(errs.KindValidation, "required",
			"an acknowledgement carries both occurred_at and recorded_at")
	}
	if o.state.IsTerminal() {
		return Occurrence{}, nil, errs.Newf(errs.KindPrecondition, "occurrence_terminal",
			"a %s occurrence cannot be acknowledged", o.state)
	}
	if o.ackState.IsAcked() {
		return Occurrence{}, nil, errs.New(errs.KindPrecondition, "already_acked",
			"this occurrence is already acknowledged")
	}
	if len(cmd.Note) > MaxAckNoteBytes {
		return Occurrence{}, nil, errs.Newf(errs.KindValidation, "max_length",
			"ack note must have at most %d characters", MaxAckNoteBytes)
	}

	next := o
	next.ackState = AckStateAcked
	next.ackedAt = notBefore(cmd.At.RecordedAt(), o.startedAt)
	next.ackedByLabel = cmd.Actor.Label()
	next.ackNote = cmd.Note
	if id, err := uuid.Parse(cmd.Actor.ID()); err == nil {
		next.ackedBy = id
	}
	if err := next.check(); err != nil {
		return Occurrence{}, nil, err
	}

	ev, err := NewEvent(EventParams{
		ID:           cmd.EventID,
		OrgID:        next.orgID,
		AlertID:      next.alertID,
		OccurrenceID: next.id,
		GroupID:      next.groupID,
		Type:         EventOccurrenceAcknowledged,
		At:           cmd.At,
		Actor:        cmd.Actor,
		Summary:      "Acknowledged by " + cmd.Actor.Label(),
		Payload:      cmd.Payload,
		DedupeKey:    "occ:" + next.id.String() + ":acknowledged:" + next.ackedAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		return Occurrence{}, nil, err
	}
	return next, []Event{ev}, nil
}

// Unacknowledge drops an acknowledgement (T10), either because a human said so or
// because a new occurrence opened.
func (o Occurrence) Unacknowledge(cmd AckCommand) (Occurrence, []Event, error) {
	if cmd.Actor.IsZero() {
		return Occurrence{}, nil, errs.New(errs.KindValidation, "required", "actor is required")
	}
	if cmd.At.IsZero() {
		return Occurrence{}, nil, errs.New(errs.KindValidation, "required",
			"an un-acknowledgement carries both occurred_at and recorded_at")
	}
	if !o.ackState.IsAcked() {
		return Occurrence{}, nil, errs.New(errs.KindPrecondition, "not_acked",
			"this occurrence is not acknowledged")
	}
	reason := cmd.Reason
	switch reason {
	case UnackReasonManual, UnackReasonNewOccurrence:
	case "":
		reason = UnackReasonManual
	default:
		return Occurrence{}, nil, errs.Newf(errs.KindValidation, "enum",
			"unack reason must be one of: manual, new_occurrence (got %q)", reason)
	}

	next := o
	next.ackState = AckStateUnacked
	next.ackedAt = time.Time{}
	next.ackedBy = uuid.Nil
	next.ackedByLabel = ""
	next.ackNote = ""
	if err := next.check(); err != nil {
		return Occurrence{}, nil, err
	}

	payload := map[string]any{"reason": reason}
	for k, v := range cmd.Payload {
		payload[k] = v
	}

	ev, err := NewEvent(EventParams{
		ID:           cmd.EventID,
		OrgID:        next.orgID,
		AlertID:      next.alertID,
		OccurrenceID: next.id,
		GroupID:      next.groupID,
		Type:         EventOccurrenceUnacknowledged,
		At:           cmd.At,
		Actor:        cmd.Actor,
		Summary:      "Acknowledgement removed (" + reason + ")",
		Payload:      payload,
		DedupeKey:    "occ:" + next.id.String() + ":unacknowledged:" + cmd.At.RecordedAt().Format(time.RFC3339Nano),
	})
	if err != nil {
		return Occurrence{}, nil, err
	}
	return next, []Event{ev}, nil
}
