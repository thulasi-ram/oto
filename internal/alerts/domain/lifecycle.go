package domain

import (
	"maps"
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
	// TransitionT3 suppresses a firing occurrence. RECONCILER ONLY, and that
	// asymmetry with T4 is deliberate — see the note on the transition table.
	TransitionT3 = TransitionID{"T3"}
	// TransitionT4 unsuppresses a suppressed occurrence. Reconciler OR ingest —
	// see the asymmetry note on the transition table (§B.3.1).
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
	// ⭐ T4 IS ASYMMETRIC WITH T3 AND THAT IS DELIBERATE (§B.3.1). Do not "fix" it.
	//
	// T3 (firing -> suppressed) is RECONCILER ONLY, because ingest can never
	// observe suppression: Alertmanager's MuteStage runs before RetryStage and
	// DROPS suppressed alerts from the slice that continues down the pipeline
	// (research A6), so a suppressed alert never reaches oto's webhook at all.
	//
	// T4 (suppressed -> firing) is RECONCILER *OR* INGEST, for exactly the same
	// reason read the other way round: a webhook arrival is POSITIVE PROOF OF
	// NON-SUPPRESSION. If the alert were still suppressed it would never have been
	// sent. Ingest cannot see suppression begin, but arrival IS the evidence that
	// it ended.
	//
	// Making T4 reconciler-only left an occurrence stuck in `suppressed` for up to
	// a full reconcile interval after a silence expired, even though a webhook had
	// already proved it was firing again — and when group_interval is shorter than
	// the reconcile interval (the common case) oto rendered a live firing alert as
	// "silenced by @ram". That is a visible lie of precisely the kind §B.4 exists
	// to prevent.
	{
		from: StateSuppressed, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT4, actors: []ActorKind{ActorReconciler, ActorIngest},
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
	// Before is the occurrence EXACTLY AS THE MACHINE READ IT — the pre-image this
	// verdict was reached against.
	//
	// It exists so a caller can persist the edge as a compare-and-set without
	// having to carry the pre-image alongside the result and hope the two stay in
	// step. `PreconditionFor(r.Before)` is the guard, and it cannot name a row
	// other than the one the decision was made from.
	Before Occurrence
	// OpensNewOccurrence marks T7. The caller must open a new occurrence with
	// seq+1 and ReopenOf set to Occurrence.ID(), which appends its own
	// `occurrence.opened` event.
	OpensNewOccurrence bool
	// Events are the AlertEvents to append, in order. At most one edge appends
	// more than nothing, so this is empty or a single event.
	Events []Event

	// DetectedBy names the witness: "webhook" for ingest, "reconciler" for the
	// reconciler. It is what T4's `occurrence.unsuppressed` payload carries
	// (§B.3.1), and it is set on every edge so a caller never has to re-derive it.
	DetectedBy string

	// Clamped reports that §B.3.2 fired: the upstream clock ran backwards and
	// `ended_at` was pulled forward to `started_at` rather than violating
	// occ_order_ck and aborting the ingest transaction.
	Clamped bool
	// ClampSkew is how far backwards the upstream clock was, and is zero unless
	// Clamped. THE CALLER MUST ACCUMULATE IT into source_health.clock_skew_ms and
	// export it as oto_clock_skew_seconds: the skew is MEASURED AND SURFACED,
	// never rejected (C12).
	ClampSkew time.Duration
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
	// extra carries the keys the machine itself puts on the event payload:
	// `detected_by` on T4, and the §B.3.2 clamp record on T5 and T6. A caller's
	// own cmd.Payload is merged over nothing — these keys are the machine's, and
	// they are computed, not supplied.
	extra := map[string]any{}
	var clampDelta time.Duration
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
		// ⭐ A suppression is a COUNTED fact. suppress_count is reopen_count's twin
		// for the suppressed path, and it is what gives T3 and T4 §C.8 dedupe keys
		// that neither collapse two real suppressions nor split one across two
		// passes. See the note above dedupeKeyFor.
		next.suppressCount = o.suppressCount + 1
		next.lastObservedAt = cmd.At.RecordedAt()
		next.observe(cmd)

	case TransitionT4:
		// `detected_by` records WHICH of the two witnesses saw suppression end.
		// The reconciler saw status.state == "active"; ingest saw a webhook
		// arrive, which is positive proof of non-suppression (§B.3.1).
		next.state = StateFiring
		next.suppressionReason = SuppressionReason{}
		next.lastObservedAt = cmd.At.RecordedAt()
		next.observe(cmd)
		extra["detected_by"] = detectedBy(cmd.Actor.Kind())

	case TransitionT5:
		// T5 sets ended_at from the UPSTREAM claim. A skewed upstream clock could
		// place it before started_at, which occ_order_ck forbids, so it is
		// clamped: skew is measured, never a reason to lose the resolution (C12).
		next.state = StateResolved
		next.resolveReason = ResolveUpstream
		next.suppressionReason = SuppressionReason{}
		next.endedAt, clampDelta = clampEnd(cmd.At.OccurredAt(), o.startedAt)
		next.lastObservedAt = cmd.At.RecordedAt()
		next.observe(cmd)
		recordClamp(extra, cmd.At.OccurredAt(), clampDelta)

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
		next.endedAt, clampDelta = clampEnd(cmd.At.RecordedAt(), o.startedAt)
		recordClamp(extra, cmd.At.RecordedAt(), clampDelta)

	case TransitionT7:
		// The terminal occurrence is untouched. The caller opens a new episode.
		return TransitionResult{
			ID: rule.id, From: from, To: rule.to,
			Occurrence:         o,
			Before:             o,
			OpensNewOccurrence: true,
		}, nil

	case TransitionT8:
		// §B.3.2 names T8 alongside T5 and T6, but a reopen CLEARS ended_at rather
		// than setting it, so there is nothing to clamp here: the invariant
		// occ_order_ck guards (ended_at >= started_at) is vacuously true while
		// ended_at is NULL. The clamp reappears when this occurrence next ends.
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

	res := TransitionResult{
		ID: rule.id, From: from, To: next.state, Occurrence: next, Before: o,
		DetectedBy: detectedBy(cmd.Actor.Kind()),
		Clamped:    clampDelta > 0,
		ClampSkew:  clampDelta,
	}

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
		Payload:      mergePayload(cmd.Payload, extra),
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
	t, _ = clampEnd(t, floor)
	return t
}

// clampEnd implements ⭐ SPEC §B.3.2: `ended_at = max(occurred_at, started_at)`.
//
// Upstream clocks skew, sometimes backwards. A `resolved` observation whose
// `occurred_at` precedes the occurrence's `started_at` would violate
// occ_order_ck and ABORT THE INGEST TRANSACTION — turning a customer's NTP
// problem into oto dropping a whole batch. So the value is pulled forward to the
// floor and the distance is returned, to be recorded on the event payload and
// accumulated into source_health.clock_skew_ms. Measure the skew; never reject.
func clampEnd(t, floor time.Time) (time.Time, time.Duration) {
	t = t.UTC()
	if t.Before(floor) {
		return floor, floor.Sub(t)
	}
	return t, 0
}

// recordClamp writes the §B.3.2 evidence onto the event payload: the clamp flag
// and the UNMODIFIED upstream value, so the timeline can still show what the
// source actually claimed.
func recordClamp(payload map[string]any, raw time.Time, delta time.Duration) {
	if delta <= 0 {
		return
	}
	payload["clamped"] = true
	payload["source_ends_at"] = raw.UTC().Format(time.RFC3339Nano)
	payload["clock_skew_ms"] = delta.Milliseconds()
}

// Detection witnesses for `occurrence.unsuppressed` (§B.3.1, T4).
const (
	// DetectedByWebhook means ingest saw the alert arrive, which is positive
	// proof of non-suppression: Alertmanager would never have sent a suppressed
	// alert.
	DetectedByWebhook = "webhook"
	// DetectedByReconciler means the API v2 reconciler saw status.state=="active".
	DetectedByReconciler = "reconciler"
)

// detectedBy maps the driving actor onto the §B.3.1 witness vocabulary.
func detectedBy(k ActorKind) string {
	switch k {
	case ActorIngest:
		return DetectedByWebhook
	case ActorReconciler:
		return DetectedByReconciler
	default:
		return k.String()
	}
}

// mergePayload copies the caller's payload and lays the machine-computed keys
// over it. The machine wins: `clamped` and `detected_by` are facts it derived,
// not hints a caller may contradict.
func mergePayload(base, extra map[string]any) map[string]any {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	out := make(map[string]any, len(base)+len(extra))
	maps.Copy(out, base)
	maps.Copy(out, extra)
	return out
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
		return "occ:" + o.id.String() + ":suppressed:" + strconv.Itoa(o.suppressCount)
	case TransitionT4:
		// The count of the suppression this edge is ENDING, which T4 leaves
		// untouched — so the pair of keys for one suppression cycle carry the same
		// ordinal and read as the two halves of one episode of silence.
		return "occ:" + o.id.String() + ":unsuppressed:" + strconv.Itoa(o.suppressCount)
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

// ⛔ T3 AND T4 KEY OFF A COUNTER, NEVER A CLOCK.
//
// They used to be built from `lastObservedAt`, which Apply sets to
// `cmd.At.RecordedAt()` — the instant oto happened to process the observation.
// Two concurrent reconciler passes over one occurrence therefore minted two
// different keys and appended TWO `occurrence.suppressed` events for ONE
// suppression, so §C.8's "a job replayed at least once appends the fact exactly
// once" did not hold for the only two edges in the table that can repeat inside
// an episode. An interim fix keyed off `sourceUpdatedAt`, which was stable across
// concurrent passes but still a timestamp, and still upstream's to move.
//
// `suppress_count` (migration 00023) is the real answer, and it is the one T8 has
// always had in `reopen_count`: two passes decided from the same pre-image both
// compute the same ordinal, and a genuine T3 -> T4 -> T3 inside one episode
// produces 1 then 2 and records both facts.

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

// OpenNewOccurrence opens a new firing episode and returns it with the
// `occurrence.opened` event to append. A new occurrence always starts unacked:
// T10 says an ack does not survive into a new episode.
//
// The name carries the "New" because SPEC §F.5.2 gives the identifier
// `OpenOccurrence` to the repository PARAMETER STRUCT of the same name, and Go
// has one namespace for both.
func OpenNewOccurrence(p OpenOccurrenceParams) (Occurrence, []Event, error) {
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
