package domain

import (
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/tuning"
)

// Lifecycle defaults (SPEC §B.5, §D.1 org settings). A caller always passes the
// org's configured value; these are what the configuration defaults to.
//
// ⛔ THEY ARE REFERENCES, NOT COPIES, AND THAT IS THE WHOLE FIX. These two used to
// be spelled here as literals with a ⚠️ comment saying they MIRROR
// `identity/domain` and must move with it — which is a note, not a mechanism. ADR
// 0026 moved three of oto's tuning defaults at once and two mirrored copies were
// missed; the failure is silent, because a stale fallback is only reached by an
// org whose settings failed to load, so exactly that tenant runs the OLD
// arithmetic and nobody is told.
//
// This package may not import `identity/domain` — CONTEXT.md §5.4, enforced by
// depguard — so the numbers live one layer below both of us, in
// `platform/tuning`, which every module may import and which may import none.
// The derivation for each is stated there.
const (
	// DefaultResolveGrace is how long past `source_ends_at` the reaper waits
	// before a case may expire.
	DefaultResolveGrace = tuning.DefaultResolveGrace
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
	// "suppressed". It drives T3 and is the ONLY way a case becomes
	// suppressed: Alertmanager's MuteStage drops suppressed alerts before the
	// webhook, so ingest can never see this (C1).
	TriggerObserveSuppressed = Trigger{"observe_suppressed"}
	// TriggerObserveResolved is an explicit per-alert status == "resolved". It
	// drives T5 and is the ONLY way a case becomes resolved (C2).
	TriggerObserveResolved = Trigger{"observe_resolved"}
	// TriggerReap is the case.reap job finding a case oto has
	// stopped hearing about. It drives T6 and is the ONLY way a case
	// becomes expired.
	TriggerReap = Trigger{"reap"}
	// TriggerCloseDue is the sweep finding an episode whose CASE RETENTION WINDOW
	// has elapsed (migration 00057). It drives the delayed half of T5 and nothing
	// else.
	//
	// ⛔⛔ IT CANNOT FABRICATE A RESOLUTION, AND THE RECEIPT IS WHY. The row it runs
	// against already carries `resolve_pending_at`/`resolve_pending_end_at`, which
	// only an explicit upstream `status="resolved"` can write. This trigger SPENDS
	// that receipt; it cannot mint one. A case with no pending close refuses the
	// edge — see Apply's T5 arm — so "resolved means upstream said so" (C2, 00007)
	// survives a second actor on the edge with its meaning intact.
	//
	// ⭐ IT IS A SEPARATE TRIGGER RATHER THAN A SECOND ACTOR ON THE EXISTING T5
	// ROWS. Widening `actors` there would let the reaper resolve ANY firing case it
	// was handed; a trigger of its own makes the delayed close reachable only from
	// the one caller that has re-read the row and proved the receipt.
	TriggerCloseDue = Trigger{"close_due"}
)

// NewTrigger parses a trigger name.
func NewTrigger(s string) (Trigger, error) {
	switch s {
	case TriggerObserveFiring.s, TriggerObserveSuppressed.s,
		TriggerObserveResolved.s, TriggerReap.s, TriggerCloseDue.s:
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

// The rows of SPEC §B.3 that the case state machine owns. T9–T14 are ack,
// enrichment, rule, notification and comment facts; T9 and T10 are implemented as
// Acknowledge and Unacknowledge, the rest are not state transitions at all.
var (
	// TransitionT1 opens the first case of an alert_key.
	TransitionT1 = TransitionID{"T1"}
	// TransitionT2 is a repeat observation of an already-firing case.
	TransitionT2 = TransitionID{"T2"}
	// TransitionT3 suppresses a firing case. RECONCILER ONLY, and that
	// asymmetry with T4 is deliberate — see the note on the transition table.
	TransitionT3 = TransitionID{"T3"}
	// TransitionT4 unsuppresses a suppressed case. Reconciler OR ingest —
	// see the asymmetry note on the transition table (§B.3.1).
	TransitionT4 = TransitionID{"T4"}
	// TransitionT5 resolves a case on an explicit upstream observation.
	TransitionT5 = TransitionID{"T5"}
	// TransitionT6 expires a case oto has stopped hearing about.
	TransitionT6 = TransitionID{"T6"}
	// TransitionT7 opens a NEW case after a re-fire. It is the ONLY road out of a
	// closed episode (ADR 0040): T8, which used to reopen the same one inside
	// refire_grace, is retired and a closed Case is now strictly terminal.
	TransitionT7 = TransitionID{"T7"}
	// TransitionT9 acknowledges a case.
	TransitionT9 = TransitionID{"T9"}
	// TransitionT10 drops an acknowledgement.
	TransitionT10 = TransitionID{"T10"}
)

// String renders the transition id.
func (t TransitionID) String() string { return t.s }

// transitionRule is one row of SPEC §B.3.
type transitionRule struct {
	from    State
	to      State
	trigger Trigger
	id      TransitionID
	// actors is the closed set of actors permitted to drive this edge. An actor
	// outside it is a programming error, not a caller error (§L.4 invariant 2).
	actors []ActorKind
	// event is the AlertEvent this edge appends; the zero EventType means the
	// edge appends nothing unless something material changed.
	event EventType
	// opensNewCase marks the two rows that OPEN an episode rather than move
	// one: T1, where there is no case to move, and T7, where the terminal
	// one is left exactly as it is. `Decide` reads this column and nothing else to
	// route an observation, which is what keeps "which rows open an episode?" a
	// fact of the table rather than an `if` at a call site.
	opensNewCase bool
}

// transitionTable IS SPEC §B.3. Adding an edge means editing this table and the
// SPEC in the same commit; there are no `if`s anywhere else that move a state.
var transitionTable = []transitionRule{
	{
		from: StateNone, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT1, actors: []ActorKind{ActorIngest, ActorReconciler},
		event: EventCaseOpened,
		// T1 opens the FIRST episode, so it opens one exactly as T7 does — the
		// column says so here rather than being re-derived from the id anywhere
		// else. Apply still refuses this row: opening is OpenNewCase's job.
		opensNewCase: true,
	},
	{
		from: StateFiring, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT2, actors: []ActorKind{ActorIngest, ActorReconciler},
		// No event unless a material field changed, in which case alert.mutated.
	},
	{
		from: StateFiring, to: StateSuppressed, trigger: TriggerObserveSuppressed,
		id: TransitionT3, actors: []ActorKind{ActorReconciler},
		event: EventCaseSuppressed,
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
	// Making T4 reconciler-only left a case stuck in `suppressed` for up to
	// a full reconcile interval after a silence expired, even though a webhook had
	// already proved it was firing again — and when group_interval is shorter than
	// the reconcile interval (the common case) oto rendered a live firing alert as
	// "silenced by @ram". That is a visible lie of precisely the kind §B.4 exists
	// to prevent.
	{
		from: StateSuppressed, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT4, actors: []ActorKind{ActorReconciler, ActorIngest},
		event: EventCaseUnsuppressed,
	},
	{
		from: StateFiring, to: StateResolved, trigger: TriggerObserveResolved,
		id: TransitionT5, actors: []ActorKind{ActorIngest},
		event: EventCaseResolved,
	},
	{
		from: StateSuppressed, to: StateResolved, trigger: TriggerObserveResolved,
		id: TransitionT5, actors: []ActorKind{ActorIngest},
		event: EventCaseResolved,
	},
	// ⭐ THE DELAYED HALF OF T5 (migration 00057). Same edge, same event, same
	// `resolve_reason` — a different WITNESS. The upstream resolve arrived earlier
	// and was recorded on the row as a pending close; this row is the sweep coming
	// back for it once the retention window has elapsed.
	//
	// There is no `suppressed` twin, and there cannot be: `case_pending_supp_ck`
	// keeps a pending close and a suppression reason off the same row, so an
	// episode holding a receipt is always in the `firing` phase.
	{
		from: StateFiring, to: StateResolved, trigger: TriggerCloseDue,
		id: TransitionT5, actors: []ActorKind{ActorReaper},
		event: EventCaseResolved,
	},
	{
		from: StateFiring, to: StateExpired, trigger: TriggerReap,
		id: TransitionT6, actors: []ActorKind{ActorReaper},
		event: EventCaseExpired,
	},
	{
		from: StateSuppressed, to: StateExpired, trigger: TriggerReap,
		id: TransitionT6, actors: []ActorKind{ActorReaper},
		event: EventCaseExpired,
	},
	// ⭐⭐ ONE ROW PER TERMINAL STATE, NOT TWO, AND THAT IS ADR 0040's REVERSAL.
	// There used to be a T8 beside each of these, taken when the re-fire landed
	// inside `refire_grace`: it CLEARED `ended_at` on the closed episode and let
	// it run again, carrying its acknowledgement across a gap in the firing. A
	// Case is strictly terminal now. Every re-fire opens the next `seq`, UNACKED
	// — see Decision.DropsAck — because an acknowledgement is a receipt for one
	// firing and the second firing is not the one that was signed for.
	{
		from: StateResolved, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT7, actors: []ActorKind{ActorIngest},
		opensNewCase: true,
	},
	{
		from: StateExpired, to: StateFiring, trigger: TriggerObserveFiring,
		id: TransitionT7, actors: []ActorKind{ActorIngest},
		opensNewCase: true,
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
	// SuppressedBy is WHICH upstream objects are doing the suppressing, as
	// Alertmanager named them on the observation that caused the edge. It is read
	// on T3 and ignored everywhere else, because every other edge CLEARS the
	// witnesses.
	//
	// ⭐ IT TRAVELS THROUGH THE MACHINE RATHER THAN AROUND IT (ADR 0041). The
	// witnesses used to reach `alert_cases.suppressed_by` from the service, off
	// the observation, while the Case the machine returned carried none — so the
	// in-memory episode disagreed with its own row, and the ALERT projection,
	// which is built from that episode, wrote an empty witness set onto a signal
	// the database knew was silenced.
	SuppressedBy SuppressedBy

	// SourceEndsAt is Alertmanager's `endsAt` for this observation, zero when no
	// end time is known.
	SourceEndsAt time.Time
	// SourceUpdatedAt is Alertmanager's `updatedAt`, zero when unknown.
	SourceUpdatedAt time.Time

	// ResolveGrace is how long past SourceEndsAt the reaper waits before T6.
	// Zero means DefaultResolveGrace.
	ResolveGrace time.Duration

	// CaseRetention is W, the case retention window for this Alert's
	// (namespace, alertname) — `case_policy_config.retention_window_s`, migration
	// 00057. It is read by T5 and ignored on every other edge.
	//
	// ⭐⭐ ZERO IS NOT A SPECIAL CASE, IT IS THE ABSENCE OF ONE. Unlike ResolveGrace
	// above, a zero here does NOT mean "use a default": it means the operator has
	// configured no window, which is the shipped default and every deployment until
	// somebody writes a `case_policy_config` row. The T5 arm's deferral branch is
	// guarded on `> 0`, so at zero the machine executes the same statements in the
	// same order it executed before this field was added. That is what makes W
	// safe to ship: the zero-value path is not equivalent to the old path, it IS
	// the old path.
	CaseRetention time.Duration

	// SourceHealthy gates T6 and nothing else. THE REAPER GUARD (§B.4) IS THE
	// HIGHEST-VALUE CORRECTNESS RULE IN THE SYSTEM: losing sight of an alert is
	// not the same as the alert resolving, so a case whose AlertSource is
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
	// From and To are the case states either side of the edge.
	From State
	To   State
	// Case is the updated AlertCase. On T7 it is the UNCHANGED
	// terminal case: T7 opens a new episode, it does not revive an old one.
	Case Case
	// Before is the case EXACTLY AS THE MACHINE READ IT — the pre-image this
	// verdict was reached against.
	//
	// It exists so a caller can persist the edge as a compare-and-set without
	// having to carry the pre-image alongside the result and hope the two stay in
	// step. `PreconditionFor(r.Before)` is the guard, and it cannot name a row
	// other than the one the decision was made from.
	Before Case
	// OpensNewCase marks T7. The caller must open a new case at seq+1, which
	// appends its own `case.opened` event; `Case` above is the CLOSED episode,
	// untouched, and the new one succeeds it by being the next `seq` rather than
	// by naming it in a column (ADR 0040).
	OpensNewCase bool
	// Events are the AlertEvents to append, in order. At most one edge appends
	// more than nothing, so this is empty or a single event.
	Events []Event

	// DetectedBy names the witness: "webhook" for ingest, "reconciler" for the
	// reconciler. It is what T4's `case.unsuppressed` payload carries
	// (§B.3.1), and it is set on every edge so a caller never has to re-derive it.
	DetectedBy string

	// Clamped reports that §B.3.2 fired: the upstream clock ran backwards and
	// `ended_at` was pulled forward to `started_at` rather than violating
	// case_order_ck and aborting the ingest transaction.
	Clamped bool
	// ClampSkew is how far backwards the upstream clock was, and is zero unless
	// Clamped. THE CALLER MUST ACCUMULATE IT into source_health.clock_skew_ms and
	// export it as oto_clock_skew_seconds: the skew is MEASURED AND SURFACED,
	// never rejected (C12).
	ClampSkew time.Duration

	// CloseDeferred marks a T5 that RECORDED an upstream resolve without performing
	// the close, because the Alert's case retention window W has not elapsed
	// (migration 00057). `Case` is the still-OPEN episode carrying the receipt.
	//
	// ⛔ A CALLER MUST NOT ANNOUNCE A RESOLUTION ON THIS RESULT. `Events` is empty
	// and `From`/`To` are equal, so a caller that keys on those two alone is already
	// correct; the flag exists for the one that keys on the TRANSITION ID, because
	// `reasonFor(T5)` is `resolved` and delivering that here would put back exactly
	// the six pings W exists to prevent. It is false at W=0, where nothing about
	// this result differs from what it was before W existed.
	CloseDeferred bool
}

// Apply runs the SPEC §B.3 state machine over one AlertCase.
//
// It is a total function: every input either produces a result or an error, never
// a panic and never a silent no-op. An edge that does not exist in the table is
// errs.KindPrecondition — the request is valid but the entity is in the wrong
// state. An edge driven by the wrong actor is errs.KindInternal — `suppressed`
// set by ingest, or `expired` set by anything but the reaper, is a programming
// bug, not a caller error (§L.4 invariant 2).
func Apply(o Case, cmd TransitionCommand) (TransitionResult, error) {
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

	// ⭐ THE MACHINE'S PHASE, NOT THE ALERT'S STATE (ADR 0041). `lifecyclePhase`
	// still folds suppression in, because T3, T4 and the suppressed arms of T5 and
	// T6 are edges of THIS table and nothing else routes them. `AlertState` — what
	// `alerts.state` holds — no longer does.
	from := o.lifecyclePhase()
	next := o
	// extra carries the keys the machine itself puts on the event payload:
	// `detected_by` on T4, and the §B.3.2 clamp record on T5 and T6. A caller's
	// own cmd.Payload is merged over nothing — these keys are the machine's, and
	// they are computed, not supplied.
	extra := map[string]any{}
	var clampDelta time.Duration
	// deferred marks the one edge that RECORDS a resolve without performing it.
	// See the T5 arm and TransitionResult.CloseDeferred.
	var deferred bool
	switch rule.id {
	case TransitionT1:
		return TransitionResult{}, errs.New(errs.KindPrecondition, "no_open_case",
			"T1 opens the first case; call OpenCase")

	case TransitionT2:
		next.lastObservedAt = cmd.At.RecordedAt()
		next.observe(cmd)
		// ⭐⭐ THE RE-FIRE INSIDE W LANDS HERE, AND CLEARING THE RECEIPT IS WHAT
		// MAKES THAT SAFE. A firing observation is proof the alert is firing again,
		// so a pending close must not survive it: left behind, the sweep would come
		// back at the due time and close an episode that is demonstrably on fire.
		// This is also the single line that turns six cases into one — the case is
		// still open, so §B.3 routes the re-fire to T2 rather than T7, and no new
		// `seq`, root card, thread or ping is minted.
		//
		// ⚠️ IT IS CLEARED IN SQL TOO, and it has to be: T2 persists through
		// `Observe`, not `Transition` (alerts/service/lifecycle.go
		// persistTransition), so `observeSQL` carries the same clearing. Two writers
		// of one fact, because the two statements are two different UPDATEs.
		next.resolvePendingAt = time.Time{}
		next.resolvePendingEndAt = time.Time{}

	case TransitionT3:
		if cmd.SuppressionReason.IsZero() {
			return TransitionResult{}, errs.New(errs.KindValidation, "required",
				"suppression_reason is required to suppress a case")
		}
		next.state = CaseOpen
		next.suppressionReason = cmd.SuppressionReason
		next.suppressedBy = cmd.SuppressedBy
		// ⭐ A suppression is a COUNTED fact. suppress_count is reopen_count's twin
		// for the suppressed path, and it is what gives T3 and T4 §C.8 dedupe keys
		// that neither collapse two real suppressions nor split one across two
		// passes. See the note above dedupeKeyFor.
		next.suppressCount = o.suppressCount + 1
		next.lastObservedAt = cmd.At.RecordedAt()
		next.observe(cmd)
		// A suppressed observation says the alert is PRESENT upstream and muted, so
		// a pending resolve on the row is stale and is dropped exactly as a re-fire
		// drops it. False at W=0, where no row carries one.
		if o.ClosePending() {
			next.resolvePendingAt = time.Time{}
			next.resolvePendingEndAt = time.Time{}
		}

	case TransitionT4:
		// `detected_by` records WHICH of the two witnesses saw suppression end.
		// The reconciler saw status.state == "active"; ingest saw a webhook
		// arrive, which is positive proof of non-suppression (§B.3.1).
		next.state = CaseOpen
		next.suppressionReason = SuppressionReason{}
		next.suppressedBy = SuppressedBy{}
		next.lastObservedAt = cmd.At.RecordedAt()
		next.observe(cmd)
		extra["detected_by"] = detectedBy(cmd.Actor.Kind())

	case TransitionT5:
		// ⭐⭐ THE TWO WITNESSES OF ONE EDGE, AND THE ORDER OF THIS ARM IS THE WHOLE
		// SAFETY ARGUMENT FOR THE CASE RETENTION WINDOW.
		//
		// Both branches below are ENTERED ONLY BY A CONFIGURED WINDOW OR BY THE
		// SWEEP. `CaseRetention` is zero on every deployment that has written no
		// `case_policy_config` row, and `TriggerCloseDue` is reachable from one
		// caller. So at W=0 control falls straight through to the statements after
		// them, which are the pre-00057 T5 arm, unedited and in their original
		// order. There is no "W=0 branch that should behave the same"; there is no
		// W=0 branch at all.
		if cmd.Trigger == TriggerCloseDue {
			// THE DELAYED CLOSE COMPLETING. The resolve arrived earlier and is on
			// the row; this spends it. `ended_at` is the stored UPSTREAM claim and
			// not the sweep's clock, so the window is not charged to the signal's
			// firing duration (R8), and it needs no clamp because
			// `case_pending_order_ck` and `check` already hold it at or above
			// `started_at`.
			if !o.ClosePending() {
				return TransitionResult{}, errs.New(errs.KindPrecondition, "no_pending_close",
					"a due close requires a pending resolve on the row")
			}
			next.state = CaseClosed
			next.resolveReason = ResolveUpstream
			next.suppressionReason = SuppressionReason{}
			next.suppressedBy = SuppressedBy{}
			next.endedAt = o.resolvePendingEndAt
			next.resolvePendingAt = time.Time{}
			next.resolvePendingEndAt = time.Time{}
			break
		}
		// ⛔⛔ THE DEFERRAL IS REFUSED FROM THE SUPPRESSED ARM, BECAUSE A SILENT EDGE
		// MAY NOT MOVE THE SUPPRESSION AXIS.
		//
		// T5 has two `from` rows and the suppressed one is reachable: a SILENCED
		// episode whose alert resolves upstream. Deferring that resolve is not
		// possible without breaking one of two rules, neither of which is negotiable.
		//
		//   * KEEP the suppression across the deferral and the row holds a receipt AND
		//     a reason at once — which `case_pending_supp_ck` forbids in DDL and
		//     `Case.check` refuses in Go (`case_pending_supp`). The resolve would be
		//     lost as an internal error.
		//   * CLEAR it, which is what this arm used to do, and the case silently stops
		//     being suppressed: `From=suppressed`, `To=firing`, no `case.unsuppressed`,
		//     no notification, and — because `applyEdge` reads `From != To` as a state
		//     change — `alerts.suppression_reason` projected away to NULL. The UI drops
		//     the silence chip and shows the episode FIRING for the length of W.
		//     Suppression is an AXIS, not a state (ADR 0041 Amendment 1, §B.6), and the
		//     end of one is a fact T4 ANNOUNCES; §B.8.4 makes the same ruling for
		//     snooze, because a damper that cannot announce its own end is exactly the
		//     silent suppression §B.6 forbids.
		//
		// So the suppressed arm falls through to the immediate close below:
		// From=suppressed, To=resolved, one `case.resolved`, one notification — the
		// suppression ends in the same breath as the episode, visibly, exactly as it
		// did before W existed. Nothing W is for is lost by that. W damps a FLAP, and a
		// suppressed alert is not delivering the re-fires that would flap —
		// Alertmanager's MuteStage drops them before the webhook (C1) — so there is no
		// episode for a receipt to hold open.
		//
		// ⭐ IT IS ALSO WHAT MAKES THE TABLE'S OWN CLAIM TRUE BY CONSTRUCTION. The
		// `close_due` row says there is no `suppressed` twin and there cannot be,
		// because an episode holding a receipt is always in the `firing` phase. That
		// was asserted OF A CHECK CONSTRAINT, which can only refuse the row after the
		// machine has decided to write it. This guard is the machine keeping it.
		if cmd.CaseRetention > 0 && from != StateSuppressed {
			// THE DEFERRAL. The episode does NOT close: `state`, `resolve_reason`
			// and `ended_at` are left exactly as they are, and the resolve is
			// recorded as a receipt for the sweep to spend once the window has
			// elapsed. A re-fire inside the window therefore finds this case still
			// open and runs T2 — one case across the flap.
			//
			// ⭐ THE DUE TIME MOVES FORWARD ON EVERY RESOLVE, which is what makes
			// the rule "stayed resolved for W" rather than "resolved W ago": two
			// resolves nine minutes apart under a ten-minute window close once, ten
			// minutes after the SECOND one.
			//
			// ⛔ THIS EDGE APPENDS NOTHING AND NOTIFIES NOTHING, and that is the
			// point of the whole ticket. `case.resolved` and the resolved
			// notification are appended by the branch above, once, at the real
			// close. Emitting them here would put the six pings back and leave a
			// timeline claiming a resolve that had not happened yet.
			//
			// ⛔ IT TOUCHES NEITHER SUPPRESSION COLUMN, AND THAT IS THE GUARD ABOVE
			// READ FROM THE OTHER END. It used to clear both here "exactly as an
			// immediate T5 does" — but an immediate T5 clears them while ANNOUNCING
			// the close, and this edge announces nothing. With the suppressed arm
			// refused, `from` is `firing`, so `suppressionReason` is already zero by
			// `lifecyclePhase`'s own definition and there is nothing left to clear.
			next.resolvePendingAt = cmd.At.RecordedAt().Add(cmd.CaseRetention)
			next.resolvePendingEndAt, clampDelta = clampEnd(cmd.At.OccurredAt(), o.startedAt)
			next.lastObservedAt = cmd.At.RecordedAt()
			next.observe(cmd)
			recordClamp(extra, cmd.At.OccurredAt(), clampDelta)
			deferred = true
			break
		}
		// T5 sets ended_at from the UPSTREAM claim. A skewed upstream clock could
		// place it before started_at, which case_order_ck forbids, so it is
		// clamped: skew is measured, never a reason to lose the resolution (C12).
		next.state = CaseClosed
		next.resolveReason = ResolveUpstream
		next.suppressionReason = SuppressionReason{}
		next.suppressedBy = SuppressedBy{}
		next.endedAt, clampDelta = clampEnd(cmd.At.OccurredAt(), o.startedAt)
		next.lastObservedAt = cmd.At.RecordedAt()
		next.observe(cmd)
		recordClamp(extra, cmd.At.OccurredAt(), clampDelta)
		// ⭐ A FALSE BRANCH AT W=0, AND THAT IS THE POINT: `ClosePending` can only be
		// true on a row a CONFIGURED window wrote, so nothing above or below it
		// executes differently on a deployment that has set no W. It exists for the
		// one sequence that can reach here holding a receipt — an operator narrowing
		// W to 0 between the resolve and the next observation — where the close is
		// simply performed now, which is what W=0 means.
		if o.ClosePending() {
			next.resolvePendingAt = time.Time{}
			next.resolvePendingEndAt = time.Time{}
		}

	case TransitionT6:
		// ⛔⛔ AN EPISODE HOLDING AN UPSTREAM RESOLVE IS NOT ONE OTO HAS STOPPED
		// HEARING ABOUT. Expiring it would stamp `timeout` over a resolve already in
		// hand — oto claiming it lost sight of an alert whose resolution it was
		// holding, which is precisely the resolved-versus-expired fabrication 00007
		// calls the distinction it must never blur. The due close is the only edge
		// that may end such a row, so this refuses rather than races it. The sweep's
		// candidate scan and `unreapable` refuse it too; a state machine that trusts
		// its caller is not a guard.
		//
		// It is unreachable at W=0: no row can carry a pending close.
		if o.ClosePending() {
			return TransitionResult{}, errs.New(errs.KindPrecondition, "close_pending",
				"a case holding an upstream resolve closes as resolved, never expired")
		}
		if !cmd.SourceHealthy {
			return TransitionResult{}, errs.New(errs.KindPrecondition, "source_not_healthy",
				"a case is held, never expired, while its AlertSource is not healthy")
		}
		if o.sourceEndsAt.IsZero() {
			return TransitionResult{}, errs.New(errs.KindPrecondition, "no_source_ends_at",
				"a case with no upstream end time cannot expire")
		}
		if !cmd.At.RecordedAt().After(o.sourceEndsAt.Add(resolveGrace(cmd))) {
			return TransitionResult{}, errs.New(errs.KindPrecondition, "resolve_grace_not_elapsed",
				"resolve_grace has not elapsed since source_ends_at")
		}
		next.state = CaseClosed
		next.resolveReason = ResolveTimeout
		next.suppressionReason = SuppressionReason{}
		next.suppressedBy = SuppressedBy{}
		next.endedAt, clampDelta = clampEnd(cmd.At.RecordedAt(), o.startedAt)
		recordClamp(extra, cmd.At.RecordedAt(), clampDelta)

	case TransitionT7:
		// The terminal case is untouched. The caller opens a new episode.
		//
		// DetectedBy is set here even though T7's only permitted actor is ingest:
		// the early return skips the common construction below, and a caller that
		// trusts the field's "set on every edge" contract would otherwise read an
		// empty string and mis-attribute the new episode the moment a second actor
		// becomes permitted on this edge.
		return TransitionResult{
			ID: rule.id, From: from, To: rule.to,
			Case:         o,
			Before:       o,
			DetectedBy:   detectedBy(cmd.Actor.Kind()),
			OpensNewCase: true,
		}, nil

	default:
		return TransitionResult{}, errs.Newf(errs.KindInternal, "unhandled_transition",
			"transition %s has no implementation", rule.id)
	}

	if err := next.check(); err != nil {
		return TransitionResult{}, err
	}

	res := TransitionResult{
		ID: rule.id, From: from, To: next.lifecyclePhase(), Case: next, Before: o,
		DetectedBy:    detectedBy(cmd.Actor.Kind()),
		Clamped:       clampDelta > 0,
		ClampSkew:     clampDelta,
		CloseDeferred: deferred,
	}

	// ⛔ A DEFERRED CLOSE IS SILENT. The row moved — the receipt is on it — but no
	// §B.2 state changed and neither suppression column moved, so `From` and `To`
	// are both `firing` and there is nothing for `case.resolved` to be true about
	// yet. The event and the notification are appended once, by the due close.
	//
	// ⚠️ "BOTH `firing`" IS A CONSEQUENCE OF THE SUPPRESSED ARM'S REFUSAL, NOT AN
	// OBSERVATION ABOUT IT. It was written here while the deferral could still be
	// entered from `suppressed`, where it was false: that path returned
	// `From=suppressed`, `To=firing` — an unsuppression with no event, no
	// notification and a projection behind it. The T5 arm's `from != StateSuppressed`
	// guard is what makes the sentence true, and the two must move together.
	if deferred && from == StateSuppressed {
		return TransitionResult{}, errs.New(errs.KindInternal, "deferred_from_suppressed",
			"a deferred close may not be reached from a suppressed episode")
	}
	if deferred {
		return res, nil
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
		ID:        cmd.EventID,
		OrgID:     next.orgID,
		AlertID:   next.alertID,
		CaseID:    next.id,
		GroupID:   next.groupID,
		Type:      eventType,
		At:        cmd.At,
		Actor:     cmd.Actor,
		Summary:   summaryOr(cmd.Summary, defaultSummary(rule.id, from, next.lifecyclePhase())),
		Payload:   mergePayload(cmd.Payload, extra),
		DedupeKey: dedupeKeyFor(rule.id, next),
	})
	if err != nil {
		return TransitionResult{}, err
	}
	res.Events = []Event{ev}
	return res, nil
}

// Decision is the §B.3 verdict for ONE observation against ONE Alert's LATEST
// episode: which row of the table runs, and — for the two rows that open an
// episode rather than move one — everything that episode must be opened with.
//
// It is reached with no I/O, no clock and no repository, which is what makes
// every edge of the table addressable from a test that never opens a database.
type Decision struct {
	// ID names the §B.3 row that selectRule matched.
	ID TransitionID
	// From is the state the episode was in when the verdict was reached. It is
	// StateNone when the Alert has no episode at all, which is T1's whole meaning:
	// there is no state to come from.
	From State

	// OpensEpisode is true for exactly the rows that do not move the case
	// they were decided against — T1, where there is none, and T7, where the
	// terminal one is left exactly as it is (§B.5). The caller opens the new
	// episode with Seq; every other row is applied with Apply.
	OpensEpisode bool
	// Seq is the sequence number the new episode takes.
	Seq int
	// DropsAck is T10 by the `new_case` road: the episode being succeeded
	// was ACKED, and an acknowledgement does not survive into a new one. The
	// caller records that on the NEW episode and leaves the old one exactly as it
	// is — see autoUnackEvent.
	//
	// ⭐ SINCE ADR 0040 IT IS THE ONLY ROAD OUT OF A CLOSED EPISODE. T8 used to
	// carry the ack across a re-fire inside `refire_grace`; it is retired, so
	// every re-fire lands here and no acknowledgement ever survives one.
	DropsAck bool
}

// Decide names the one §B.3 row an observation runs, and is the ONLY place that
// question is answered.
//
// ⭐ AN ALERT WITH NO EPISODE IS THE ZERO Case, whose state is StateNone —
// the state T1's row already comes from. That is why this takes a Case and
// no "is there one?" flag: the absence of an episode is a state the table models,
// not a case a caller special-cases around it. The zero Case also carries
// seq 0 and a nil id, so the arithmetic below yields exactly the first episode's
// parameters without a branch.
//
// ⛔ IT READS ONLY THE ROUTING FIELDS of cmd — Trigger and At, which are what
// selectRule matches on. The rest of a TransitionCommand is what an edge
// is APPLIED with, and a caller may finish assembling it after this returns;
// Apply is what validates it.
//
// ⛔ IT DOES NOT CHECK rule.actors, AND THAT IS NOT AN OVERSIGHT. Apply checks
// them, because authorisation belongs to the edge that MUTATES a case
// (§L.4 invariant 2) — `suppressed` set by ingest, `expired` set by anything but
// the reaper. Opening an episode mutates none: the previous one is untouched. And
// the reconciler MUST be able to open one — "present upstream, absent in oto" is
// the recovery ADR 0006 promises, and refusing it because T7's actor column names
// only ingest would turn oto's own repair path into an internal error.
func Decide(current Case, cmd TransitionCommand) (Decision, error) {
	rule, err := selectRule(current, cmd)
	if err != nil {
		return Decision{}, err
	}
	d := Decision{ID: rule.id, From: current.lifecyclePhase()}
	if !rule.opensNewCase {
		return d, nil
	}
	d.OpensEpisode = true
	d.Seq = current.seq + 1
	d.DropsAck = current.ackState.IsAcked()
	return d, nil
}

// selectRule finds the one §B.3 row that matches the case and the command.
//
// ⭐ IT MATCHES ON lifecyclePhase, NOT ON THE CASE'S OWN STATE, and that is ADR
// 0040 read the right way round: the table's `from`/`to` columns have always been
// the four §B.2 values. The Case is what the edge is APPLIED to, and it carries
// enough to answer which of the four it was in, so the table did not have to
// change shape when `alert_cases.state` narrowed.
//
// ⛔ IT IS NOT AlertState, SINCE ADR 0041, AND SWAPPING THEM BREAKS FOUR EDGES.
// `AlertState` stopped folding suppression in, because it is what
// `alerts.state` stores and `suppressed` there hid firing alerts from every
// aggregate. The MACHINE still needs the four phases: match on AlertState and T3
// becomes firing → firing, T4 has no `from` to leave, and a Case can never record
// that it was muted at all.
//
// Since T8 was retired there is at most one row per (from, trigger) pair, so
// there is nothing left to disambiguate and no grace window to measure.
func selectRule(o Case, cmd TransitionCommand) (transitionRule, error) {
	from := o.lifecyclePhase()
	for _, r := range transitionTable {
		if r.from != from || r.trigger != cmd.Trigger {
			continue
		}
		return r, nil
	}
	return transitionRule{}, errs.Newf(errs.KindPrecondition, "illegal_transition",
		"no transition from %q under trigger %q", from, cmd.Trigger)
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

// observe folds the upstream fields of an observation into the case.
//
// A field the observation did not supply is PRESERVED, never cleared. §L.3.1
// says a zero `endsAt` means "no end time known" for that payload — it does not
// mean "forget the end time you already had", and clearing it would silently
// disable the reaper for that case (case_reap_idx only sees rows with a
// non-null source_ends_at).
func (o *Case) observe(cmd TransitionCommand) {
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
// `occurred_at` precedes the case's `started_at` would violate
// case_order_ck and ABORT THE INGEST TRANSACTION — turning a customer's NTP
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

// Detection witnesses for `case.unsuppressed` (§B.3.1, T4).
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
		return "Case opened"
	case TransitionT2:
		return "Alert details changed"
	case TransitionT3:
		return "Case suppressed"
	case TransitionT4:
		return "Case no longer suppressed"
	case TransitionT5:
		return "Case resolved upstream"
	case TransitionT6:
		return "Case expired: oto stopped hearing about it"
	default:
		return "Case moved from " + from.String() + " to " + to.String()
	}
}

// dedupeKeyFor renders the C.8 idempotency key for an event, so that a job
// replayed at least once appends the fact exactly once.
func dedupeKeyFor(id TransitionID, o Case) string {
	switch id {
	case TransitionT1, TransitionT7:
		return "case:" + o.id.String() + ":opened"
	case TransitionT3:
		return "case:" + o.id.String() + ":suppressed:" + strconv.Itoa(o.suppressCount)
	case TransitionT4:
		// The count of the suppression this edge is ENDING, which T4 leaves
		// untouched — so the pair of keys for one suppression cycle carry the same
		// ordinal and read as the two halves of one episode of silence.
		return "case:" + o.id.String() + ":unsuppressed:" + strconv.Itoa(o.suppressCount)
	case TransitionT5:
		return "case:" + o.id.String() + ":resolved"
	case TransitionT6:
		return "case:" + o.id.String() + ":expired"
	default:
		return ""
	}
}

// ⛔ T3 AND T4 KEY OFF A COUNTER, NEVER A CLOCK.
//
// They used to be built from `lastObservedAt`, which Apply sets to
// `cmd.At.RecordedAt()` — the instant oto happened to process the observation.
// Two concurrent reconciler passes over one case therefore minted two
// different keys and appended TWO `case.suppressed` events for ONE
// suppression, so §C.8's "a job replayed at least once appends the fact exactly
// once" did not hold for the only two edges in the table that can repeat inside
// an episode. An interim fix keyed off `sourceUpdatedAt`, which was stable across
// concurrent passes but still a timestamp, and still upstream's to move.
//
// `suppress_count` (migration 00023) is the real answer, and it is the one T8 has
// always had in `reopen_count`: two passes decided from the same pre-image both
// compute the same ordinal, and a genuine T3 -> T4 -> T3 inside one episode
// produces 1 then 2 and records both facts.

// OpenCaseParams opens a new AlertCase — SPEC §B.3 T1 (the first
// sighting of an alert_key, or a firing observation with no open case) and
// T7 (a re-fire beyond refire_grace).
type OpenCaseParams struct {
	ID      uuid.UUID
	OrgID   uuid.UUID
	AlertID uuid.UUID
	// GroupID is the AlertGroup generation this case joins, or uuid.Nil
	// until grouping resolves it.
	GroupID uuid.UUID
	// Seq is prev+1, or 1 for the first episode.
	Seq int

	Actor Actor
	At    ObservationTime

	SourceStartsAt  time.Time
	SourceEndsAt    time.Time
	SourceUpdatedAt time.Time

	Value        *float64
	ObservedSkew time.Duration

	// EventID is the uuidv7 for the `case.opened` event.
	EventID uuid.UUID
	Summary string
	Payload map[string]any
}

// OpenNewCase opens a new firing episode and returns it with the
// `case.opened` event to append. A new case always starts unacked:
// T10 says an ack does not survive into a new episode.
//
// The name carries the "New" because SPEC §F.5.2 gives the identifier
// `OpenCase` to the repository PARAMETER STRUCT of the same name, and Go
// has one namespace for both.
func OpenNewCase(p OpenCaseParams) (Case, []Event, error) {
	if p.Actor.IsZero() {
		return Case{}, nil, errs.New(errs.KindValidation, "required", "actor is required")
	}
	if !permits([]ActorKind{ActorIngest, ActorReconciler}, p.Actor.Kind()) {
		return Case{}, nil, errs.Newf(errs.KindInternal, "wrong_actor",
			"%s may not open a case", p.Actor.Kind())
	}
	if p.At.IsZero() {
		return Case{}, nil, errs.New(errs.KindValidation, "required",
			"opening a case carries both occurred_at and recorded_at")
	}

	starts := p.SourceStartsAt
	if starts.IsZero() {
		starts = p.At.OccurredAt()
	}

	o, err := NewCase(CaseParams{
		ID:              p.ID,
		OrgID:           p.OrgID,
		AlertID:         p.AlertID,
		GroupID:         p.GroupID,
		Seq:             p.Seq,
		State:           CaseOpen,
		StartedAt:       p.At.RecordedAt(),
		LastObservedAt:  p.At.RecordedAt(),
		SourceStartsAt:  starts,
		SourceEndsAt:    p.SourceEndsAt,
		SourceUpdatedAt: p.SourceUpdatedAt,
		AckState:        AckStateUnacked,
		Value:           p.Value,
		ObservedSkew:    p.ObservedSkew,
	})
	if err != nil {
		return Case{}, nil, err
	}

	// ⭐ `seq` IS WHAT NAMES THE ROW, NOW THAT `reopen_of` IS GONE. It is 1-based
	// and gapless (case_seq_uniq, case_seq_ck), so the first episode of an Alert
	// is T1 by construction and every one above it followed a close — which is
	// exactly what T7 means since ADR 0040 made a re-fire the only way out of a
	// terminal Case.
	id := TransitionT1
	if p.Seq > 1 {
		id = TransitionT7
	}
	ev, err := NewEvent(EventParams{
		ID:        p.EventID,
		OrgID:     o.orgID,
		AlertID:   o.alertID,
		CaseID:    o.id,
		GroupID:   o.groupID,
		Type:      EventCaseOpened,
		At:        p.At,
		Actor:     p.Actor,
		Summary:   summaryOr(p.Summary, defaultSummary(id, StateNone, StateFiring)),
		Payload:   p.Payload,
		DedupeKey: dedupeKeyFor(id, o),
	})
	if err != nil {
		return Case{}, nil, err
	}
	return o, []Event{ev}, nil
}

// AckCommand acknowledges or un-acknowledges an AlertCase (§B.3 T9, T10).
// Acknowledgement is orthogonal to state: an acked alert is still firing.
type AckCommand struct {
	Actor Actor
	At    ObservationTime
	// EventID is the uuidv7 for the appended event.
	EventID uuid.UUID
	// Note is the free-text note left with an acknowledgement OR with a manual
	// un-acknowledgement, at most MaxAckNoteBytes.
	//
	// The two land in different places, and that is not an accident. An ack note
	// is a property of the acknowledgement, so it is stored on the case
	// (`ack_note`) and cleared when the ack is dropped. An unack note describes a
	// withdrawal that leaves nothing behind to hang it on, so it lands in the
	// `case.unacknowledged` event payload — the timeline, which is the only
	// record that keeps a fact after the state it described is gone.
	Note string
	// Reason explains an un-acknowledgement: "manual" or "new_case".
	Reason  string
	Payload map[string]any
}

// Unacknowledge reasons (§B.3 T10).
const (
	// UnackReasonManual is a human dropping their own acknowledgement.
	UnackReasonManual = "manual"
	// UnackReasonNewCase is an ack being dropped because a new episode opened.
	UnackReasonNewCase = "new_case"
)

// Acknowledge records that a human took this case (T9).
//
// Acknowledging a terminal case is errs.KindPrecondition: the request is
// well-formed, the entity is simply in the wrong state. Acknowledgement identity
// IS stored — it is operationally necessary — but oto exposes no per-person
// response-time metric anywhere (R8).
func (o Case) Acknowledge(cmd AckCommand) (Case, []Event, error) {
	if cmd.Actor.IsZero() || !cmd.Actor.Kind().IsHuman() {
		return Case{}, nil, errs.New(errs.KindValidation, "required",
			"an acknowledgement requires a human actor")
	}
	if cmd.At.IsZero() {
		return Case{}, nil, errs.New(errs.KindValidation, "required",
			"an acknowledgement carries both occurred_at and recorded_at")
	}
	if o.state.IsClosed() {
		return Case{}, nil, errs.Newf(errs.KindPrecondition, "case_terminal",
			"a %s case cannot be acknowledged", o.state)
	}
	if o.ackState.IsAcked() {
		return Case{}, nil, errs.New(errs.KindPrecondition, "already_acked",
			"this case is already acknowledged")
	}
	if len(cmd.Note) > MaxAckNoteBytes {
		return Case{}, nil, errs.Newf(errs.KindValidation, "max_length",
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
		return Case{}, nil, err
	}

	ev, err := NewEvent(EventParams{
		ID:        cmd.EventID,
		OrgID:     next.orgID,
		AlertID:   next.alertID,
		CaseID:    next.id,
		GroupID:   next.groupID,
		Type:      EventCaseAcknowledged,
		At:        cmd.At,
		Actor:     cmd.Actor,
		Summary:   "Acknowledged by " + cmd.Actor.Label(),
		Payload:   cmd.Payload,
		DedupeKey: "case:" + next.id.String() + ":acknowledged:" + next.ackedAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		return Case{}, nil, err
	}
	return next, []Event{ev}, nil
}

// Unacknowledge drops an acknowledgement (T10), either because a human said so or
// because a new case opened.
func (o Case) Unacknowledge(cmd AckCommand) (Case, []Event, error) {
	if cmd.Actor.IsZero() {
		return Case{}, nil, errs.New(errs.KindValidation, "required", "actor is required")
	}
	if cmd.At.IsZero() {
		return Case{}, nil, errs.New(errs.KindValidation, "required",
			"an un-acknowledgement carries both occurred_at and recorded_at")
	}
	if !o.ackState.IsAcked() {
		return Case{}, nil, errs.New(errs.KindPrecondition, "not_acked",
			"this case is not acknowledged")
	}
	reason := cmd.Reason
	switch reason {
	case UnackReasonManual, UnackReasonNewCase:
	case "":
		reason = UnackReasonManual
	default:
		return Case{}, nil, errs.Newf(errs.KindValidation, "enum",
			"unack reason must be one of: manual, new_case (got %q)", reason)
	}
	if len(cmd.Note) > MaxAckNoteBytes {
		return Case{}, nil, errs.Newf(errs.KindValidation, "max_length",
			"unack note must have at most %d characters", MaxAckNoteBytes)
	}

	next := o
	next.ackState = AckStateUnacked
	next.ackedAt = time.Time{}
	next.ackedBy = uuid.Nil
	next.ackedByLabel = ""
	next.ackNote = ""
	if err := next.check(); err != nil {
		return Case{}, nil, err
	}

	payload := map[string]any{"reason": reason}
	// ⭐ The withdrawal note goes ON THE TIMELINE, because the case has
	// nowhere left to keep it: `ack_note` describes the acknowledgement that is
	// being removed and is cleared four lines above. "Un-acking, it's back" is
	// the most useful sentence anybody types at 3am, and it used to be bound,
	// length-validated by the handler and then dropped on the floor.
	if cmd.Note != "" {
		payload["note"] = cmd.Note
	}
	for k, v := range cmd.Payload {
		payload[k] = v
	}

	ev, err := NewEvent(EventParams{
		ID:        cmd.EventID,
		OrgID:     next.orgID,
		AlertID:   next.alertID,
		CaseID:    next.id,
		GroupID:   next.groupID,
		Type:      EventCaseUnacknowledged,
		At:        cmd.At,
		Actor:     cmd.Actor,
		Summary:   "Acknowledgement removed (" + reason + ")",
		Payload:   payload,
		DedupeKey: "case:" + next.id.String() + ":unacknowledged:" + cmd.At.RecordedAt().Format(time.RFC3339Nano),
	})
	if err != nil {
		return Case{}, nil, err
	}
	return next, []Event{ev}, nil
}
