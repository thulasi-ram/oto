package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

var (
	caseID      = uuid.MustParse("018f3a4b-0000-7000-8000-000000000101")
	alertID     = uuid.MustParse("018f3a4b-0000-7000-8000-000000000102")
	groupIDFix  = uuid.MustParse("018f3a4b-0000-7000-8000-000000000103")
	eventIDFix  = uuid.MustParse("018f3a4b-0000-7000-8000-000000000104")
	snapshotFix = uuid.MustParse("018f3a4b-0000-7000-8000-000000000106")

	t0 = time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
)

// allStates is every value State can hold, including the zero one.
func allStates() []State {
	return []State{StateNone, StateFiring, StateSuppressed, StateResolved, StateExpired}
}

func allTriggers() []Trigger {
	return []Trigger{TriggerObserveFiring, TriggerObserveSuppressed, TriggerObserveResolved, TriggerReap}
}

// caseIn builds a Case that an ALERT in `state` would have.
//
// ⭐ IT TAKES THE FOUR-WAY STATE AND STORES THE TWO-WAY ONE, which is ADR 0040's
// derivation exercised on every call: the Case row holds `open` or `closed`, and
// `suppression_reason` / `resolve_reason` carry the rest. `AlertState()` reads
// the four-way word back out, and every assertion below that names one is
// therefore a round-trip through both halves.
func caseIn(t *testing.T, state State, mut ...func(*CaseParams)) Case {
	t.Helper()
	p := CaseParams{
		ID:             caseID,
		OrgID:          orgA,
		AlertID:        alertID,
		Seq:            1,
		State:          state.CaseState(),
		StartedAt:      t0,
		LastObservedAt: t0,
		SourceStartsAt: t0,
		AckState:       AckStateUnacked,
		StateVersion:   7,
	}
	switch state {
	case StateSuppressed:
		p.SuppressionReason = SuppressionSilence
	case StateResolved:
		p.EndedAt = t0.Add(time.Minute)
		p.ResolveReason = ResolveUpstream
	case StateExpired:
		p.EndedAt = t0.Add(time.Minute)
		p.ResolveReason = ResolveTimeout
	}
	for _, m := range mut {
		m(&p)
	}
	o, err := NewCase(p)
	require.NoError(t, err)
	return o
}

func at(t *testing.T, occurred, recorded time.Time) ObservationTime {
	t.Helper()
	ot, err := NewObservationTime(occurred, recorded)
	require.NoError(t, err)
	return ot
}

func actor(t *testing.T, kind ActorKind) Actor {
	t.Helper()
	a, err := SystemActor(kind)
	require.NoError(t, err)
	return a
}

func humanActor(t *testing.T, id, label string) Actor {
	t.Helper()
	a, err := NewActor(ActorUser, id, label)
	require.NoError(t, err)
	return a
}

func requireKind(t *testing.T, err error, kind errs.Kind, code string) {
	t.Helper()
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, kind, e.Kind)
	assert.Equal(t, code, e.Code)
}

// ------------------------------------------------------- the shape of the table

// TestTransitionTable_IsExactlySpecB3 pins the whole edge set. Any edge added to
// or removed from transitionTable without a SPEC amendment fails here.
func TestTransitionTable_IsExactlySpecB3(t *testing.T) {
	type edge struct {
		from, to State
		trigger  Trigger
	}
	legal := map[edge]struct{}{
		// T1 — the first sighting.
		{StateNone, StateFiring, TriggerObserveFiring}: {},
		// T2 — a repeat observation.
		{StateFiring, StateFiring, TriggerObserveFiring}: {},
		// T3 — suppression begins.
		{StateFiring, StateSuppressed, TriggerObserveSuppressed}: {},
		// T4 — suppression ends.
		{StateSuppressed, StateFiring, TriggerObserveFiring}: {},
		// T5 — an explicit upstream resolution.
		{StateFiring, StateResolved, TriggerObserveResolved}:     {},
		{StateSuppressed, StateResolved, TriggerObserveResolved}: {},
		// T6 — the reaper.
		{StateFiring, StateExpired, TriggerReap}:     {},
		{StateSuppressed, StateExpired, TriggerReap}: {},
		// T7 / T8 — a re-fire out of a terminal state.
		{StateResolved, StateFiring, TriggerObserveFiring}: {},
		{StateExpired, StateFiring, TriggerObserveFiring}:  {},
	}

	for _, from := range allStates() {
		for _, to := range allStates() {
			for _, tr := range allTriggers() {
				_, want := legal[edge{from, to, tr}]
				got := CanTransition(from, to, tr)
				assert.Equal(t, want, got,
					"CanTransition(%q -> %q, %q)", from, to, tr)
			}
		}
	}
}

// TestNeverFabricateAResolution is CONTEXT.md §3's first rule, executable:
// `resolved` requires an explicit upstream status="resolved" and nothing else can
// produce it.
func TestNeverFabricateAResolution(t *testing.T) {
	for _, from := range allStates() {
		for _, tr := range allTriggers() {
			if tr == TriggerObserveResolved {
				continue
			}
			assert.False(t, CanTransition(from, StateResolved, tr),
				"only an explicit upstream observation may resolve (%q under %q)", from, tr)
		}
	}
	// And only from an OPEN state: a terminal case cannot be re-resolved.
	assert.False(t, CanTransition(StateResolved, StateResolved, TriggerObserveResolved))
	assert.False(t, CanTransition(StateExpired, StateResolved, TriggerObserveResolved))
}

// TestExpiredIsNotResolved_NoEdgeBetweenThem is the same rule read the other way:
// losing sight of an alert is not the alert resolving, and there is no path that
// silently converts one into the other.
func TestExpiredIsNotResolved_NoEdgeBetweenThem(t *testing.T) {
	for _, tr := range allTriggers() {
		assert.False(t, CanTransition(StateExpired, StateResolved, tr),
			"an expired case must never become resolved")
		assert.False(t, CanTransition(StateResolved, StateExpired, tr),
			"a resolved case must never become expired")
	}
	// `expired` is reachable ONLY by the reaper's trigger.
	for _, from := range allStates() {
		for _, tr := range allTriggers() {
			if tr == TriggerReap {
				continue
			}
			assert.False(t, CanTransition(from, StateExpired, tr),
				"expired came from %q under %q", from, tr)
		}
	}
}

// TestSuppressedIsInvisibleToWebhooks — CONTEXT.md §3: only the reconciler can
// ENTER suppressed; either witness may LEAVE it. The asymmetry is deliberate.
func TestSuppressedIsOnlyEnteredUnderItsOwnTrigger(t *testing.T) {
	for _, from := range allStates() {
		for _, tr := range allTriggers() {
			if tr == TriggerObserveSuppressed {
				continue
			}
			assert.False(t, CanTransition(from, StateSuppressed, tr))
		}
	}
	assert.True(t, CanTransition(StateFiring, StateSuppressed, TriggerObserveSuppressed))
	// Only from firing: a terminal case cannot be suppressed.
	assert.False(t, CanTransition(StateResolved, StateSuppressed, TriggerObserveSuppressed))
	assert.False(t, CanTransition(StateExpired, StateSuppressed, TriggerObserveSuppressed))
	assert.False(t, CanTransition(StateSuppressed, StateSuppressed, TriggerObserveSuppressed))
}

func TestNoEdgeReturnsToStateNone(t *testing.T) {
	for _, from := range allStates() {
		for _, tr := range allTriggers() {
			assert.False(t, CanTransition(from, StateNone, tr),
				"a case never un-happens")
		}
	}
}

func TestTransitionsFrom(t *testing.T) {
	tests := []struct {
		from    State
		trigger Trigger
		want    []State
	}{
		{from: StateNone, trigger: TriggerObserveFiring, want: []State{StateFiring}},
		{from: StateNone, trigger: TriggerReap},
		{from: StateFiring, trigger: TriggerObserveFiring, want: []State{StateFiring}},
		{from: StateFiring, trigger: TriggerObserveSuppressed, want: []State{StateSuppressed}},
		{from: StateFiring, trigger: TriggerObserveResolved, want: []State{StateResolved}},
		{from: StateFiring, trigger: TriggerReap, want: []State{StateExpired}},
		{from: StateSuppressed, trigger: TriggerObserveFiring, want: []State{StateFiring}},
		{from: StateSuppressed, trigger: TriggerObserveSuppressed},
		{from: StateResolved, trigger: TriggerObserveFiring, want: []State{StateFiring}},
		{from: StateExpired, trigger: TriggerObserveFiring, want: []State{StateFiring}},
		{from: StateExpired, trigger: TriggerObserveResolved},
	}
	for _, tc := range tests {
		t.Run(tc.from.String()+"/"+tc.trigger.String(), func(t *testing.T) {
			got := TransitionsFrom(tc.from, tc.trigger)
			assert.ElementsMatch(t, tc.want, got)
			assert.Len(t, got, len(tc.want), "TransitionsFrom must not repeat a state (T7 and T8 share one)")
		})
	}
}

func TestNewTrigger(t *testing.T) {
	for _, in := range []string{"observe_firing", "observe_suppressed", "observe_resolved", "reap"} {
		got, err := NewTrigger(in)
		require.NoError(t, err)
		assert.Equal(t, in, got.String())
		assert.False(t, got.IsZero())
	}
	for _, in := range []string{"", "resolve", "snooze", "ack", "observe", "Reap"} {
		got, err := NewTrigger(in)
		requireKind(t, err, errs.KindValidation, "enum")
		assert.True(t, got.IsZero())
	}
}

// ------------------------------------------------------------- Apply validation

func TestApply_RequiresACompleteCommand(t *testing.T) {
	o := caseIn(t, StateFiring)
	good := TransitionCommand{
		Trigger: TriggerObserveFiring,
		Actor:   actor(t, ActorIngest),
		At:      at(t, t0, t0),
	}

	tests := []struct {
		name string
		mut  func(*TransitionCommand)
	}{
		{name: "no trigger", mut: func(c *TransitionCommand) { c.Trigger = Trigger{} }},
		{name: "no actor", mut: func(c *TransitionCommand) { c.Actor = Actor{} }},
		{name: "no observation time", mut: func(c *TransitionCommand) { c.At = ObservationTime{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := good
			tc.mut(&cmd)
			_, err := Apply(o, cmd)
			requireKind(t, err, errs.KindValidation, "required")
		})
	}
}

func TestApply_IllegalEdgeIsPrecondition(t *testing.T) {
	// "The request is valid but the entity is in the wrong state" — 412, not 500.
	tests := []struct {
		name    string
		state   State
		trigger Trigger
		actor   ActorKind
	}{
		{name: "suppress a resolved case", state: StateResolved, trigger: TriggerObserveSuppressed, actor: ActorReconciler},
		{name: "resolve an expired case", state: StateExpired, trigger: TriggerObserveResolved, actor: ActorIngest},
		{name: "resolve an already resolved case", state: StateResolved, trigger: TriggerObserveResolved, actor: ActorIngest},
		{name: "reap a resolved case", state: StateResolved, trigger: TriggerReap, actor: ActorReaper},
		{name: "reap an expired case", state: StateExpired, trigger: TriggerReap, actor: ActorReaper},
		{name: "suppress an already suppressed case", state: StateSuppressed, trigger: TriggerObserveSuppressed, actor: ActorReconciler},
		{name: "reap a never-opened case", state: StateNone, trigger: TriggerReap, actor: ActorReaper},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := caseIn(t, tc.state, func(p *CaseParams) {
				if tc.state == StateNone {
					p.State = CaseOpen // NewCase requires a real state
				}
			})
			if tc.state == StateNone {
				// The zero Case IS "no episode": AlertState reads StateNone off a
				// zero CaseState, which is the state T1's row comes from.
				o.state = CaseState{}
			}
			_, err := Apply(o, TransitionCommand{
				Trigger: tc.trigger,
				Actor:   actor(t, tc.actor),
				At:      at(t, t0.Add(time.Hour), t0.Add(time.Hour)),
			})
			requireKind(t, err, errs.KindPrecondition, "illegal_transition")
		})
	}
}

// TestApply_WrongActorIsInternal — an edge driven by the wrong actor is a
// PROGRAMMING BUG, not a caller error (§L.4 invariant 2).
func TestApply_WrongActorIsInternal(t *testing.T) {
	tests := []struct {
		name    string
		state   State
		trigger Trigger
		actor   ActorKind
		mut     func(*TransitionCommand)
	}{
		{
			name:  "⭐ ingest may not ENTER suppressed: MuteStage drops suppressed alerts before the webhook",
			state: StateFiring, trigger: TriggerObserveSuppressed, actor: ActorIngest,
			mut: func(c *TransitionCommand) { c.SuppressionReason = SuppressionSilence },
		},
		{
			name:  "the reaper may not suppress",
			state: StateFiring, trigger: TriggerObserveSuppressed, actor: ActorReaper,
			mut: func(c *TransitionCommand) { c.SuppressionReason = SuppressionSilence },
		},
		{name: "ingest may not reap", state: StateFiring, trigger: TriggerReap, actor: ActorIngest},
		{name: "the reconciler may not reap", state: StateFiring, trigger: TriggerReap, actor: ActorReconciler},
		{name: "a human may not reap", state: StateFiring, trigger: TriggerReap, actor: ActorUser},
		{name: "the reconciler may not resolve", state: StateFiring, trigger: TriggerObserveResolved, actor: ActorReconciler},
		{name: "the reaper may not resolve", state: StateFiring, trigger: TriggerObserveResolved, actor: ActorReaper},
		{name: "a human may not observe", state: StateFiring, trigger: TriggerObserveFiring, actor: ActorUser},
		{name: "the notifier may not observe", state: StateFiring, trigger: TriggerObserveFiring, actor: ActorNotifier},
		{name: "the reaper may not unsuppress", state: StateSuppressed, trigger: TriggerObserveFiring, actor: ActorReaper},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := caseIn(t, tc.state)
			cmd := TransitionCommand{
				Trigger: tc.trigger,
				Actor:   actorOfKind(t, tc.actor),
				At:      at(t, t0, t0),
				EventID: eventIDFix,
			}
			if tc.mut != nil {
				tc.mut(&cmd)
			}
			_, err := Apply(o, cmd)
			requireKind(t, err, errs.KindInternal, "wrong_actor")
		})
	}
}

func actorOfKind(t *testing.T, k ActorKind) Actor {
	t.Helper()
	if k.IsHuman() {
		return humanActor(t, uuid.New().String(), "Ram")
	}
	return actor(t, k)
}

// -------------------------------------------------------------------- T1 and T7

func TestApply_T1IsNotAnEdgeOnAnExistingCase(t *testing.T) {
	o := caseIn(t, StateFiring)
	o.state = CaseState{} // the zero Case: no episode at all, which AlertState reads as StateNone

	_, err := Apply(o, TransitionCommand{
		Trigger: TriggerObserveFiring,
		Actor:   actor(t, ActorIngest),
		At:      at(t, t0, t0),
	})
	requireKind(t, err, errs.KindPrecondition, "no_open_case")
	assert.Contains(t, err.Error(), "OpenCase")
}

// ---------------------------------------------------------------------------- T2

func TestApply_T2_SilentUnlessSomethingMaterialChanged(t *testing.T) {
	o := caseIn(t, StateFiring)
	later := t0.Add(30 * time.Second)

	res, err := Apply(o, TransitionCommand{
		Trigger: TriggerObserveFiring,
		Actor:   actor(t, ActorIngest),
		At:      at(t, later, later),
		EventID: eventIDFix,
	})
	require.NoError(t, err)

	assert.Equal(t, TransitionT2, res.ID)
	assert.Equal(t, StateFiring, res.From)
	assert.Equal(t, StateFiring, res.To)
	assert.Empty(t, res.Events, "a repeat observation that changed nothing is silent")
	assert.Equal(t, later, res.Case.LastObservedAt())
	assert.Equal(t, DetectedByWebhook, res.DetectedBy)
	assert.False(t, res.Clamped)
	assert.Equal(t, o, res.Before, "Before is the pre-image the verdict was reached against")
	assert.Equal(t, o.StateVersion(), PreconditionFor(res.Before).StateVersion)
}

func TestApply_T2_MaterialChangeEmitsAlertMutated(t *testing.T) {
	o := caseIn(t, StateFiring)
	later := t0.Add(30 * time.Second)

	res, err := Apply(o, TransitionCommand{
		Trigger:        TriggerObserveFiring,
		Actor:          actor(t, ActorReconciler),
		At:             at(t, later, later),
		EventID:        eventIDFix,
		MaterialChange: true,
	})
	require.NoError(t, err)

	require.Len(t, res.Events, 1)
	ev := res.Events[0]
	assert.Equal(t, EventAlertMutated, ev.Type())
	assert.Equal(t, "Alert details changed", ev.Summary())
	assert.Equal(t, eventIDFix, ev.ID())
	assert.Equal(t, alertID, ev.AlertID())
	assert.Equal(t, caseID, ev.CaseID())
	assert.Empty(t, ev.DedupeKey(), "T2 has no §C.8 dedupe key")
	assert.Equal(t, DetectedByReconciler, res.DetectedBy)
}

func TestApply_T2_ObservationFieldsAreFoldedInNeverCleared(t *testing.T) {
	endsAt := t0.Add(10 * time.Minute)
	o := caseIn(t, StateFiring, func(p *CaseParams) {
		p.SourceEndsAt = endsAt
		p.SourceUpdatedAt = t0
	})
	later := t0.Add(time.Minute)
	value := 42.5

	// A payload that supplies nothing must PRESERVE what is already known:
	// clearing source_ends_at would silently disable the reaper for this row.
	res, err := Apply(o, TransitionCommand{
		Trigger: TriggerObserveFiring,
		Actor:   actor(t, ActorIngest),
		At:      at(t, later, later),
	})
	require.NoError(t, err)
	assert.Equal(t, endsAt, res.Case.SourceEndsAt(), "a zero endsAt means 'unknown', never 'forget'")
	assert.Equal(t, t0, res.Case.SourceUpdatedAt())
	assert.Nil(t, res.Case.Value())

	// A payload that DOES supply them moves them.
	newEnds := t0.Add(20 * time.Minute)
	res, err = Apply(o, TransitionCommand{
		Trigger:         TriggerObserveFiring,
		Actor:           actor(t, ActorIngest),
		At:              at(t, later, later),
		SourceEndsAt:    newEnds,
		SourceUpdatedAt: later,
		Value:           &value,
		ObservedSkew:    3 * time.Second,
	})
	require.NoError(t, err)
	assert.Equal(t, newEnds, res.Case.SourceEndsAt())
	assert.Equal(t, later, res.Case.SourceUpdatedAt())
	require.NotNil(t, res.Case.Value())
	assert.Equal(t, 42.5, *res.Case.Value())
	assert.Equal(t, 3*time.Second, res.Case.ObservedSkew())
}

// ---------------------------------------------------------------------------- T3

func TestApply_T3_SuppressRequiresAReasonAndCountsTheEpisode(t *testing.T) {
	o := caseIn(t, StateFiring)
	later := t0.Add(time.Minute)

	// A reason is required: case_suppress_ck ties it to the state.
	_, err := Apply(o, TransitionCommand{
		Trigger: TriggerObserveSuppressed,
		Actor:   actor(t, ActorReconciler),
		At:      at(t, later, later),
		EventID: eventIDFix,
	})
	requireKind(t, err, errs.KindValidation, "required")

	for _, reason := range []SuppressionReason{
		SuppressionSilence, SuppressionInhibition,
		SuppressionMuteTimeInterval, SuppressionActiveTimeInterval,
	} {
		t.Run(reason.String(), func(t *testing.T) {
			res, err := Apply(o, TransitionCommand{
				Trigger:           TriggerObserveSuppressed,
				Actor:             actor(t, ActorReconciler),
				At:                at(t, later, later),
				EventID:           eventIDFix,
				SuppressionReason: reason,
			})
			require.NoError(t, err)

			assert.Equal(t, TransitionT3, res.ID)
			assert.Equal(t, StateSuppressed, res.To)
			assert.Equal(t, reason, res.Case.SuppressionReason())
			assert.Equal(t, 1, res.Case.SuppressCount(), "a suppression is a COUNTED fact")
			assert.True(t, res.Case.IsOpen(), "suppressed is an OPEN state, not a terminal one")

			require.Len(t, res.Events, 1)
			assert.Equal(t, EventCaseSuppressed, res.Events[0].Type())
			assert.Equal(t, "case:"+caseID.String()+":suppressed:1", res.Events[0].DedupeKey(),
				"⛔ T3's dedupe key is a COUNTER, never a clock: two concurrent reconciler passes must mint the same key")
		})
	}
}

func TestApply_T3andT4_DedupeKeysPairPerEpisode(t *testing.T) {
	// One suppression episode: T3 then T4 carry the SAME ordinal, so they read as
	// the two halves of one period of silence.
	firing := caseIn(t, StateFiring)
	t1 := t0.Add(time.Minute)

	suppressed, err := Apply(firing, TransitionCommand{
		Trigger: TriggerObserveSuppressed, Actor: actor(t, ActorReconciler),
		At: at(t, t1, t1), EventID: eventIDFix, SuppressionReason: SuppressionSilence,
	})
	require.NoError(t, err)
	assert.Equal(t, "case:"+caseID.String()+":suppressed:1", suppressed.Events[0].DedupeKey())

	t2 := t0.Add(2 * time.Minute)
	unsuppressed, err := Apply(suppressed.Case, TransitionCommand{
		Trigger: TriggerObserveFiring, Actor: actor(t, ActorReconciler),
		At: at(t, t2, t2), EventID: eventIDFix,
	})
	require.NoError(t, err)
	assert.Equal(t, "case:"+caseID.String()+":unsuppressed:1", unsuppressed.Events[0].DedupeKey())
	assert.Equal(t, 1, unsuppressed.Case.SuppressCount(), "T4 leaves the counter alone")

	// A genuine second suppression inside the same episode produces 2, so both
	// facts are recorded rather than collapsed.
	t3 := t0.Add(3 * time.Minute)
	again, err := Apply(unsuppressed.Case, TransitionCommand{
		Trigger: TriggerObserveSuppressed, Actor: actor(t, ActorReconciler),
		At: at(t, t3, t3), EventID: eventIDFix, SuppressionReason: SuppressionInhibition,
	})
	require.NoError(t, err)
	assert.Equal(t, "case:"+caseID.String()+":suppressed:2", again.Events[0].DedupeKey())
}

// ---------------------------------------------------------------------------- T4

// TestApply_T4_IsAsymmetricWithT3 — §B.3.1. A webhook arrival is POSITIVE PROOF
// of non-suppression, so ingest may leave `suppressed` even though it can never
// enter it.
func TestApply_T4_IsAsymmetricWithT3(t *testing.T) {
	tests := []struct {
		actor      ActorKind
		detectedBy string
	}{
		{actor: ActorReconciler, detectedBy: DetectedByReconciler},
		{actor: ActorIngest, detectedBy: DetectedByWebhook},
	}
	for _, tc := range tests {
		t.Run(tc.actor.String(), func(t *testing.T) {
			o := caseIn(t, StateSuppressed, func(p *CaseParams) {
				p.SuppressCount = 4
				p.SuppressedBy = SuppressedBy{SilencedBy: []string{"sil-1"}}
			})
			later := t0.Add(time.Minute)

			res, err := Apply(o, TransitionCommand{
				Trigger: TriggerObserveFiring,
				Actor:   actor(t, tc.actor),
				At:      at(t, later, later),
				EventID: eventIDFix,
			})
			require.NoError(t, err)

			assert.Equal(t, TransitionT4, res.ID)
			assert.Equal(t, StateSuppressed, res.From)
			assert.Equal(t, StateFiring, res.To)
			assert.True(t, res.Case.SuppressionReason().IsZero(),
				"suppression_reason exists only while suppressed")
			assert.True(t, res.Case.SuppressedBy().IsZero(),
				"the witnesses are not left behind on an alert that is demonstrably firing")
			assert.Equal(t, tc.detectedBy, res.DetectedBy)

			require.Len(t, res.Events, 1)
			ev := res.Events[0]
			assert.Equal(t, EventCaseUnsuppressed, ev.Type())
			assert.Equal(t, tc.detectedBy, ev.Payload()["detected_by"],
				"the event records WHICH witness proved suppression had ended")
			assert.Equal(t, "case:"+caseID.String()+":unsuppressed:4", ev.DedupeKey())
		})
	}
}

func TestApply_T4_MachineComputedPayloadKeysWinOverTheCallers(t *testing.T) {
	o := caseIn(t, StateSuppressed)
	later := t0.Add(time.Minute)

	res, err := Apply(o, TransitionCommand{
		Trigger: TriggerObserveFiring,
		Actor:   actor(t, ActorIngest),
		At:      at(t, later, later),
		EventID: eventIDFix,
		Payload: map[string]any{"detected_by": "a lie", "extra": "kept"},
	})
	require.NoError(t, err)

	payload := res.Events[0].Payload()
	assert.Equal(t, DetectedByWebhook, payload["detected_by"],
		"`detected_by` is a fact the machine derived, not a hint a caller may contradict")
	assert.Equal(t, "kept", payload["extra"])
}

// ---------------------------------------------------------------------------- T5

func TestApply_T5_ResolvedOnlyFromAnExplicitUpstreamObservation(t *testing.T) {
	for _, from := range []State{StateFiring, StateSuppressed} {
		t.Run("from "+from.String(), func(t *testing.T) {
			o := caseIn(t, from)
			endedAt := t0.Add(5 * time.Minute)

			res, err := Apply(o, TransitionCommand{
				Trigger: TriggerObserveResolved,
				Actor:   actor(t, ActorIngest),
				At:      at(t, endedAt, endedAt.Add(time.Second)),
				EventID: eventIDFix,
			})
			require.NoError(t, err)

			assert.Equal(t, TransitionT5, res.ID)
			assert.Equal(t, StateResolved, res.To)
			assert.Equal(t, ResolveUpstream, res.Case.ResolveReason(),
				"resolved is bound one-to-one to resolve_reason=upstream")
			assert.Equal(t, endedAt, res.Case.EndedAt(), "ended_at comes from the UPSTREAM claim")
			assert.True(t, res.Case.SuppressionReason().IsZero())
			assert.False(t, res.Case.IsOpen())
			assert.False(t, res.Clamped)

			require.Len(t, res.Events, 1)
			assert.Equal(t, EventCaseResolved, res.Events[0].Type())
			assert.Equal(t, "Case resolved upstream", res.Events[0].Summary())
			assert.Equal(t, "case:"+caseID.String()+":resolved", res.Events[0].DedupeKey())
		})
	}
}

// TestApply_T5_ClampNeverReject is §B.3.2 and ADR 0021's wedge path 3: a
// backward-skewed upstream clock is clamped and MEASURED, never a reason to
// abort the ingest transaction.
func TestApply_T5_ClampNeverReject(t *testing.T) {
	o := caseIn(t, StateFiring)
	skewed := t0.Add(-90 * time.Second) // upstream claims it ended before it started

	res, err := Apply(o, TransitionCommand{
		Trigger: TriggerObserveResolved,
		Actor:   actor(t, ActorIngest),
		At:      at(t, skewed, t0.Add(time.Minute)),
		EventID: eventIDFix,
	})
	require.NoError(t, err, "a customer's NTP problem must never drop a batch")

	assert.Equal(t, StateResolved, res.To)
	assert.Equal(t, t0, res.Case.EndedAt(), "ended_at = max(occurred_at, started_at)")
	assert.True(t, res.Clamped)
	assert.Equal(t, 90*time.Second, res.ClampSkew)

	payload := res.Events[0].Payload()
	assert.Equal(t, true, payload["clamped"])
	assert.Equal(t, int64(90_000), payload["clock_skew_ms"])
	assert.Equal(t, skewed.Format(time.RFC3339Nano), payload["source_ends_at"],
		"the UNMODIFIED upstream value stays on the timeline")
}

// ---------------------------------------------------------------------------- T6

// TestApply_T6_ReaperGuard is CONTEXT.md §3 and ADR 0021's wedge path 2 — the
// highest-value correctness rule in the system.
func TestApply_T6_ReaperGuard(t *testing.T) {
	endsAt := t0.Add(time.Minute)
	reapAt := t0.Add(30 * time.Minute)

	base := func(p *CaseParams) { p.SourceEndsAt = endsAt }

	t.Run("blocked while the AlertSource is not healthy", func(t *testing.T) {
		o := caseIn(t, StateFiring, base)
		_, err := Apply(o, TransitionCommand{
			Trigger:       TriggerReap,
			Actor:         actor(t, ActorReaper),
			At:            at(t, reapAt, reapAt),
			EventID:       eventIDFix,
			SourceHealthy: false,
		})
		requireKind(t, err, errs.KindPrecondition, "source_not_healthy")
		assert.Contains(t, err.Error(), "held, never expired")
	})

	t.Run("a case with no upstream end time cannot expire", func(t *testing.T) {
		o := caseIn(t, StateFiring)
		_, err := Apply(o, TransitionCommand{
			Trigger: TriggerReap, Actor: actor(t, ActorReaper),
			At: at(t, reapAt, reapAt), EventID: eventIDFix, SourceHealthy: true,
		})
		requireKind(t, err, errs.KindPrecondition, "no_source_ends_at")
	})

	t.Run("resolve_grace must have elapsed", func(t *testing.T) {
		o := caseIn(t, StateFiring, base)
		// endsAt + default 5m grace = t0+6m. Exactly at the boundary is NOT after.
		boundary := endsAt.Add(DefaultResolveGrace)
		_, err := Apply(o, TransitionCommand{
			Trigger: TriggerReap, Actor: actor(t, ActorReaper),
			At: at(t, boundary, boundary), EventID: eventIDFix, SourceHealthy: true,
		})
		requireKind(t, err, errs.KindPrecondition, "resolve_grace_not_elapsed")

		// One nanosecond later it is permitted.
		just := boundary.Add(time.Nanosecond)
		_, err = Apply(o, TransitionCommand{
			Trigger: TriggerReap, Actor: actor(t, ActorReaper),
			At: at(t, just, just), EventID: eventIDFix, SourceHealthy: true,
		})
		require.NoError(t, err)
	})

	t.Run("a configured resolve_grace overrides the default", func(t *testing.T) {
		o := caseIn(t, StateFiring, base)
		when := endsAt.Add(20 * time.Minute)
		_, err := Apply(o, TransitionCommand{
			Trigger: TriggerReap, Actor: actor(t, ActorReaper),
			At: at(t, when, when), EventID: eventIDFix, SourceHealthy: true,
			ResolveGrace: time.Hour,
		})
		requireKind(t, err, errs.KindPrecondition, "resolve_grace_not_elapsed")
	})

	for _, from := range []State{StateFiring, StateSuppressed} {
		t.Run("expires from "+from.String(), func(t *testing.T) {
			o := caseIn(t, from, base)
			res, err := Apply(o, TransitionCommand{
				Trigger: TriggerReap, Actor: actor(t, ActorReaper),
				At: at(t, reapAt, reapAt), EventID: eventIDFix, SourceHealthy: true,
			})
			require.NoError(t, err)

			assert.Equal(t, TransitionT6, res.ID)
			assert.Equal(t, StateExpired, res.To)
			assert.Equal(t, ResolveTimeout, res.Case.ResolveReason(),
				"expired is bound one-to-one to resolve_reason=timeout — never `upstream`")
			assert.NotEqual(t, ResolveUpstream, res.Case.ResolveReason())
			assert.Equal(t, reapAt, res.Case.EndedAt(), "expiry is stamped with OTO's clock")
			assert.True(t, res.Case.SuppressionReason().IsZero())

			require.Len(t, res.Events, 1)
			assert.Equal(t, EventCaseExpired, res.Events[0].Type())
			assert.Equal(t, "Case expired: oto stopped hearing about it", res.Events[0].Summary(),
				"the timeline never claims a resolution it did not observe")
			assert.NotContains(t, strings.ToLower(res.Events[0].Summary()), "resolved")
			assert.Equal(t, "case:"+caseID.String()+":expired", res.Events[0].DedupeKey())
		})
	}
}

// ------------------------------------------------------------------------- T7

// TestApply_EveryRefireOpensANewEpisodeWhateverTheClockSays is what replaced
// TestApply_T7vsT8_RefireGrace, and the replacement IS the assertion.
//
// ⭐⭐ THE CLOCK USED TO DECIDE THIS AND NO LONGER APPEARS. A re-fire inside
// `refire_grace` took T8 — the closed episode's `ended_at` was cleared, it ran
// again, and any acknowledgement taken on it carried across the gap in the
// firing. ADR 0040 reversed that: a Case is strictly terminal, so every re-fire
// takes T7 and opens the next `seq`, unacknowledged. The table below therefore
// walks the same instants the old one did — one second after the close, at what
// used to be the boundary, and an hour later — and demands ONE answer.
//
// ⛔ AND T7 LEAVES THE CLOSED EPISODE EXACTLY AS IT FOUND IT. That was already
// true and is now the only behaviour, which is why `res.Case` is compared whole:
// a future edit that let this branch touch the terminal row would be reviving an
// episode by another name.
func TestApply_EveryRefireOpensANewEpisodeWhateverTheClockSays(t *testing.T) {
	endedAt := t0.Add(time.Minute)

	tests := []struct {
		name   string
		from   State
		refire time.Time
	}{
		{name: "one second after the close", from: StateResolved, refire: endedAt.Add(time.Second)},
		{name: "five minutes after, once inside the grace", from: StateResolved, refire: endedAt.Add(5 * time.Minute)},
		{name: "at what used to be the grace boundary", from: StateResolved, refire: endedAt.Add(20 * time.Minute)},
		{name: "an hour after, once outside it", from: StateResolved, refire: endedAt.Add(time.Hour)},
		{name: "an expired episode, immediately", from: StateExpired, refire: endedAt.Add(time.Minute)},
		{name: "an expired episode, much later", from: StateExpired, refire: endedAt.Add(time.Hour)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := caseIn(t, tc.from, func(p *CaseParams) { p.EndedAt = endedAt })

			res, err := Apply(o, TransitionCommand{
				Trigger: TriggerObserveFiring,
				Actor:   actor(t, ActorIngest),
				At:      at(t, tc.refire, tc.refire),
				EventID: eventIDFix,
			})
			require.NoError(t, err)

			assert.Equal(t, TransitionT7, res.ID)
			assert.Equal(t, StateFiring, res.To)
			assert.True(t, res.OpensNewCase)

			assert.Equal(t, o, res.Case, "T7 does not touch the episode it succeeds")
			assert.Equal(t, CaseClosed, res.Case.State())
			assert.Equal(t, tc.from, res.Case.AlertState())
			assert.Equal(t, endedAt, res.Case.EndedAt())
			assert.Empty(t, res.Events, "the `case.opened` event comes from OpenNewCase")
		})
	}
}

// TestApply_ACloseIsTerminalAndNothingReopensIt is the negative half: there is no
// longer any command that puts a closed episode back into an open one.
func TestApply_ACloseIsTerminalAndNothingReopensIt(t *testing.T) {
	for _, from := range []State{StateResolved, StateExpired} {
		t.Run(from.String(), func(t *testing.T) {
			o := caseIn(t, from, func(p *CaseParams) { p.EndedAt = t0.Add(time.Minute) })

			for _, trigger := range []Trigger{
				TriggerObserveFiring, TriggerObserveSuppressed,
				TriggerObserveResolved, TriggerReap,
			} {
				res, err := Apply(o, TransitionCommand{
					Trigger:       trigger,
					Actor:         actor(t, ActorIngest),
					At:            at(t, t0.Add(time.Hour), t0.Add(time.Hour)),
					EventID:       eventIDFix,
					SourceHealthy: true,
				})
				if err != nil {
					continue // the table has no edge at all, which is stronger still
				}
				assert.True(t, res.OpensNewCase,
					"%s out of %s must open a NEW episode, never revive this one", trigger, from)
				assert.Equal(t, CaseClosed, res.Case.State())
				assert.False(t, res.Case.EndedAt().IsZero(),
					"nothing may clear ended_at on a closed episode")
			}
		})
	}
}

// ⛔ BUG (low severity). TransitionResult.DetectedBy documents itself as "set on
// every edge so a caller never has to re-derive it", but the T7 branch returns
// early (lifecycle.go:443-450) without populating it, so a T7 result carries "".
// It is currently harmless because T7's only permitted actor is ingest and
// internal/alerts/service/lifecycle.go:581 treats anything that is not
// "reconciler" as ingest — i.e. the field is load-bearing and the correct answer
// is reached by accident. Adding a second permitted actor to T7 would make it a
// silent mis-attribution on the timeline.
func TestApply_T7_SetsDetectedBy_BUG(t *testing.T) {
	// Regression: T7 returned early from Apply without DetectedBy, leaving the
	// field empty on the one edge that opens a new episode.

	o := caseIn(t, StateResolved, func(p *CaseParams) { p.EndedAt = t0.Add(time.Minute) })
	refire := t0.Add(2 * time.Hour)

	res, err := Apply(o, TransitionCommand{
		Trigger: TriggerObserveFiring,
		Actor:   actor(t, ActorIngest),
		At:      at(t, refire, refire),
		EventID: eventIDFix,
	})
	require.NoError(t, err)
	require.Equal(t, TransitionT7, res.ID)
	assert.Equal(t, DetectedByWebhook, res.DetectedBy)
}

// -------------------------------------------------------------- OpenNewCase

func TestOpenNewCase(t *testing.T) {
	p := OpenCaseParams{
		ID:      caseID,
		OrgID:   orgA,
		AlertID: alertID,
		Seq:     1,
		Actor:   actor(t, ActorIngest),
		At:      at(t, t0.Add(-time.Minute), t0),
		EventID: eventIDFix,
	}

	o, events, err := OpenNewCase(p)
	require.NoError(t, err)

	assert.Equal(t, CaseOpen, o.State())
	assert.Equal(t, StateFiring, o.AlertState())
	assert.Equal(t, AckStateUnacked, o.AckState(), "a new case always starts unacked (T10)")
	assert.Equal(t, t0, o.StartedAt(), "started_at is OTO's clock")
	assert.Equal(t, t0, o.LastObservedAt())
	assert.Equal(t, t0.Add(-time.Minute), o.SourceStartsAt(),
		"with no upstream startsAt, the upstream CLAIM is used")
	assert.True(t, o.IsOpen())
	assert.Equal(t, 1, o.StateVersion())

	require.Len(t, events, 1)
	assert.Equal(t, EventCaseOpened, events[0].Type())
	assert.Equal(t, "Case opened", events[0].Summary())
	assert.Equal(t, "case:"+caseID.String()+":opened", events[0].DedupeKey())
}

// TestOpenNewCase_SeqIsWhatNamesT7 — `reopen_of` used to say which episode a new
// one succeeded, and `seq` said the same thing one column over. ADR 0040 kept the
// one that was already unique, gapless and indexed.
func TestOpenNewCase_SeqIsWhatNamesT7(t *testing.T) {
	o, events, err := OpenNewCase(OpenCaseParams{
		ID: caseID, OrgID: orgA, AlertID: alertID, Seq: 2,
		Actor: actor(t, ActorIngest), At: at(t, t0, t0), EventID: eventIDFix,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, o.Seq())
	assert.Equal(t, AckStateUnacked, o.AckState(),
		"the episode this one succeeds may have been acked; this one is not")
	require.Len(t, events, 1)
	assert.Equal(t, EventCaseOpened, events[0].Type(), "T7 still appends `case.opened`")
	assert.Equal(t, "Case opened", events[0].Summary())
}

func TestOpenNewCase_Rejects(t *testing.T) {
	good := OpenCaseParams{
		ID: caseID, OrgID: orgA, AlertID: alertID, Seq: 1,
		Actor: actor(t, ActorIngest), At: at(t, t0, t0), EventID: eventIDFix,
	}

	tests := []struct {
		name string
		mut  func(*OpenCaseParams)
		kind errs.Kind
		code string
	}{
		{name: "no actor", mut: func(p *OpenCaseParams) { p.Actor = Actor{} }, kind: errs.KindValidation, code: "required"},
		{name: "no observation time", mut: func(p *OpenCaseParams) { p.At = ObservationTime{} }, kind: errs.KindValidation, code: "required"},
		{name: "the reaper may not open", mut: func(p *OpenCaseParams) { p.Actor = actor(t, ActorReaper) }, kind: errs.KindInternal, code: "wrong_actor"},
		{name: "a human may not open", mut: func(p *OpenCaseParams) { p.Actor = humanActor(t, uuid.New().String(), "Ram") }, kind: errs.KindInternal, code: "wrong_actor"},
		{name: "the notifier may not open", mut: func(p *OpenCaseParams) { p.Actor = actor(t, ActorNotifier) }, kind: errs.KindInternal, code: "wrong_actor"},
		{name: "seq below 1", mut: func(p *OpenCaseParams) { p.Seq = 0 }, kind: errs.KindValidation, code: "min"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := good
			tc.mut(&p)
			_, _, err := OpenNewCase(p)
			requireKind(t, err, tc.kind, tc.code)
		})
	}
}

// -------------------------------------------------------------- T9 / T10 (ack)

// TestAcknowledge_AnAckedAlertIsStillFiring is CONTEXT.md §3: ack_state is an
// ORTHOGONAL AXIS, not a state.
func TestAcknowledge_AnAckedAlertIsStillFiring(t *testing.T) {
	for _, from := range []State{StateFiring, StateSuppressed} {
		t.Run("from "+from.String(), func(t *testing.T) {
			o := caseIn(t, from)
			ackAt := t0.Add(time.Minute)
			userID := uuid.New()

			next, events, err := o.Acknowledge(AckCommand{
				Actor:   humanActor(t, userID.String(), "Ram"),
				At:      at(t, ackAt, ackAt),
				EventID: eventIDFix,
				Note:    "looking at it",
			})
			require.NoError(t, err)

			// ⭐ ADR 0041: the loop's `from` is the MACHINE'S phase, which still folds
			// suppression in; `AlertState` no longer does, so it is asserted against
			// `firing` for both cases — which is the ADR's point, since an acked
			// silenced alert is firing and every counter must say so.
			assert.Equal(t, from, next.lifecyclePhase(), "⭐ acknowledging does not move the state")
			assert.Equal(t, from, o.lifecyclePhase(), "and the receiver is untouched")
			assert.Equal(t, StateFiring, next.AlertState(),
				"and the stored state is `firing` whether or not a silence is in force")
			assert.Equal(t, CaseOpen, next.State(), "and the episode is still running")
			assert.Equal(t, AckStateAcked, next.AckState())
			assert.Equal(t, ackAt, next.AckedAt())
			assert.Equal(t, userID, next.AckedBy())
			assert.Equal(t, "Ram", next.AckedByLabel())
			assert.Equal(t, "looking at it", next.AckNote())
			assert.True(t, next.IsOpen(), "an acked alert is still firing")
			assert.Equal(t, next.SuppressionReason(), o.SuppressionReason(),
				"ack never touches the suppression axis")

			require.Len(t, events, 1)
			assert.Equal(t, EventCaseAcknowledged, events[0].Type())
			assert.Equal(t, "Acknowledged by Ram", events[0].Summary())
			assert.Contains(t, events[0].DedupeKey(), ":acknowledged:")
		})
	}
}

func TestAcknowledge_Rejects(t *testing.T) {
	tests := []struct {
		name  string
		state State
		mut   func(*CaseParams)
		cmd   func(*AckCommand)
		kind  errs.Kind
		code  string
	}{
		{
			name: "a machine actor cannot acknowledge", state: StateFiring,
			cmd:  func(c *AckCommand) { c.Actor = actor(t, ActorIngest) },
			kind: errs.KindValidation, code: "required",
		},
		{
			name: "no actor", state: StateFiring,
			cmd:  func(c *AckCommand) { c.Actor = Actor{} },
			kind: errs.KindValidation, code: "required",
		},
		{
			name: "no observation time", state: StateFiring,
			cmd:  func(c *AckCommand) { c.At = ObservationTime{} },
			kind: errs.KindValidation, code: "required",
		},
		{
			name: "a resolved case cannot be acknowledged", state: StateResolved,
			kind: errs.KindPrecondition, code: "case_terminal",
		},
		{
			name: "an expired case cannot be acknowledged", state: StateExpired,
			kind: errs.KindPrecondition, code: "case_terminal",
		},
		{
			name: "already acked", state: StateFiring,
			mut: func(p *CaseParams) {
				p.AckState = AckStateAcked
				p.AckedAt = t0
				p.AckedByLabel = "Someone"
			},
			kind: errs.KindPrecondition, code: "already_acked",
		},
		{
			name: "note over the bound", state: StateFiring,
			cmd:  func(c *AckCommand) { c.Note = strings.Repeat("n", MaxAckNoteBytes+1) },
			kind: errs.KindValidation, code: "max_length",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var muts []func(*CaseParams)
			if tc.mut != nil {
				muts = append(muts, tc.mut)
			}
			o := caseIn(t, tc.state, muts...)
			cmd := AckCommand{
				Actor:   humanActor(t, uuid.New().String(), "Ram"),
				At:      at(t, t0.Add(time.Minute), t0.Add(time.Minute)),
				EventID: eventIDFix,
			}
			if tc.cmd != nil {
				tc.cmd(&cmd)
			}
			_, _, err := o.Acknowledge(cmd)
			requireKind(t, err, tc.kind, tc.code)
		})
	}
}

func TestAcknowledge_AckedAtIsClampedToStartedAt(t *testing.T) {
	o := caseIn(t, StateFiring)
	next, _, err := o.Acknowledge(AckCommand{
		Actor:   humanActor(t, uuid.New().String(), "Ram"),
		At:      at(t, t0.Add(-time.Hour), t0.Add(-time.Hour)),
		EventID: eventIDFix,
	})
	require.NoError(t, err)
	assert.Equal(t, t0, next.AckedAt(), "case_ackorder_ck: acked_at >= started_at")
}

func TestAcknowledge_NonUUIDActorIDLeavesAckedByNil(t *testing.T) {
	// A Slack user id is carried on the event's actor_id, not on the case's
	// FK column.
	o := caseIn(t, StateFiring)
	slack, err := NewActor(ActorSlack, "U012ABCDEF", "ram")
	require.NoError(t, err)

	next, events, err := o.Acknowledge(AckCommand{
		Actor: slack, At: at(t, t0, t0), EventID: eventIDFix,
	})
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, next.AckedBy())
	assert.Equal(t, "ram", next.AckedByLabel(), "the LABEL is what history reads from")
	assert.Equal(t, "U012ABCDEF", events[0].Actor().ID())
}

func TestUnacknowledge(t *testing.T) {
	acked := func(t *testing.T) Case {
		return caseIn(t, StateFiring, func(p *CaseParams) {
			p.AckState = AckStateAcked
			p.AckedAt = t0
			p.AckedBy = uuid.New()
			p.AckedByLabel = "Ram"
			p.AckNote = "on it"
		})
	}

	t.Run("manual by default", func(t *testing.T) {
		o := acked(t)
		next, events, err := o.Unacknowledge(AckCommand{
			Actor:   humanActor(t, uuid.New().String(), "Ram"),
			At:      at(t, t0.Add(time.Minute), t0.Add(time.Minute)),
			EventID: eventIDFix,
		})
		require.NoError(t, err)

		assert.Equal(t, AckStateUnacked, next.AckState())
		assert.True(t, next.AckedAt().IsZero())
		assert.Equal(t, uuid.Nil, next.AckedBy())
		assert.Empty(t, next.AckedByLabel())
		assert.Empty(t, next.AckNote(), "the ack note describes the ack being removed")
		assert.Equal(t, StateFiring, next.AlertState(), "un-acking does not move the state either")

		require.Len(t, events, 1)
		assert.Equal(t, EventCaseUnacknowledged, events[0].Type())
		assert.Equal(t, UnackReasonManual, events[0].Payload()["reason"])
		assert.Equal(t, "Acknowledgement removed (manual)", events[0].Summary())
	})

	t.Run("the withdrawal note lands on the timeline", func(t *testing.T) {
		o := acked(t)
		_, events, err := o.Unacknowledge(AckCommand{
			Actor:   humanActor(t, uuid.New().String(), "Ram"),
			At:      at(t, t0, t0),
			EventID: eventIDFix,
			Note:    "Un-acking, it's back",
		})
		require.NoError(t, err)
		assert.Equal(t, "Un-acking, it's back", events[0].Payload()["note"],
			"the case has nowhere left to keep it: ack_note is cleared")
	})

	t.Run("new_case is machine-driven", func(t *testing.T) {
		o := acked(t)
		_, events, err := o.Unacknowledge(AckCommand{
			Actor:   actor(t, ActorSystem),
			At:      at(t, t0, t0),
			EventID: eventIDFix,
			Reason:  UnackReasonNewCase,
		})
		require.NoError(t, err)
		assert.Equal(t, UnackReasonNewCase, events[0].Payload()["reason"])
	})

	t.Run("rejects", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			o    func(*testing.T) Case
			cmd  func(*AckCommand)
			kind errs.Kind
			code string
		}{
			{name: "not acked", o: func(t *testing.T) Case { return caseIn(t, StateFiring) },
				kind: errs.KindPrecondition, code: "not_acked"},
			{name: "no actor", o: acked, cmd: func(c *AckCommand) { c.Actor = Actor{} },
				kind: errs.KindValidation, code: "required"},
			{name: "no time", o: acked, cmd: func(c *AckCommand) { c.At = ObservationTime{} },
				kind: errs.KindValidation, code: "required"},
			{name: "unknown reason", o: acked, cmd: func(c *AckCommand) { c.Reason = "closed" },
				kind: errs.KindValidation, code: "enum"},
			{name: "note over the bound", o: acked, cmd: func(c *AckCommand) { c.Note = strings.Repeat("n", MaxAckNoteBytes+1) },
				kind: errs.KindValidation, code: "max_length"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cmd := AckCommand{
					Actor:   humanActor(t, uuid.New().String(), "Ram"),
					At:      at(t, t0, t0),
					EventID: eventIDFix,
				}
				if tc.cmd != nil {
					tc.cmd(&cmd)
				}
				_, _, err := tc.o(t).Unacknowledge(cmd)
				requireKind(t, err, tc.kind, tc.code)
			})
		}
	})
}

// ------------------------------------------------------- machine-wide invariants

// TestApply_NeverLeavesTheClosedStateSet drives every legal (state, trigger,
// actor) combination and asserts the machine can only ever land on one of the
// four §B.2 states.
func TestApply_NeverLeavesTheClosedStateSet(t *testing.T) {
	closed := map[State]struct{}{
		StateFiring: {}, StateSuppressed: {}, StateResolved: {}, StateExpired: {},
	}
	actors := []ActorKind{
		ActorSystem, ActorIngest, ActorReconciler, ActorReaper,
		ActorEnricher, ActorNotifier, ActorUser, ActorSlack,
	}

	for _, from := range []State{StateFiring, StateSuppressed, StateResolved, StateExpired} {
		for _, tr := range allTriggers() {
			for _, ak := range actors {
				o := caseIn(t, from, func(p *CaseParams) {
					p.SourceEndsAt = t0.Add(time.Minute)
				})
				when := t0.Add(time.Hour)
				res, err := Apply(o, TransitionCommand{
					Trigger:           tr,
					Actor:             actorOfKind(t, ak),
					At:                at(t, when, when),
					EventID:           eventIDFix,
					SuppressionReason: SuppressionSilence,
					SourceHealthy:     true,
				})
				if err != nil {
					// Every rejection is a typed error, never a panic.
					var e *errs.Error
					require.ErrorAs(t, err, &e)
					require.Contains(t,
						[]errs.Kind{errs.KindValidation, errs.KindPrecondition, errs.KindInternal},
						e.Kind)
					continue
				}
				_, ok := closed[res.To]
				assert.True(t, ok, "Apply produced state %q from %q under %q/%q", res.To, from, tr, ak)
				assert.Equal(t, from, res.From)
				assert.Equal(t, o, res.Before)
				if res.OpensNewCase {
					// T7 leaves the terminal case alone; `To` describes the
					// NEW episode the caller must open.
					// ADR 0041: the table's states are the MACHINE'S phases, so the
					// comparison is against `lifecyclePhase`. `AlertState` is the
					// three-value column reading and is asserted elsewhere.
					assert.Equal(t, from, res.Case.lifecyclePhase())
					continue
				}
				assert.Equal(t, res.To, res.Case.lifecyclePhase())
			}
		}
	}
}

func TestDefaultSummariesAreWithinTheEventBound(t *testing.T) {
	for _, id := range []TransitionID{
		TransitionT1, TransitionT2, TransitionT3, TransitionT4,
		TransitionT5, TransitionT6, TransitionT7,
		TransitionT9, TransitionT10,
	} {
		s := defaultSummary(id, StateFiring, StateResolved)
		assert.NotEmpty(t, strings.TrimSpace(s))
		assert.LessOrEqual(t, len(s), MaxEventSummaryBytes)
	}
}

func TestSummaryOr_PrefersTheCallersText(t *testing.T) {
	assert.Equal(t, "custom", summaryOr("custom", "fallback"))
	assert.Equal(t, "fallback", summaryOr("", "fallback"))
	assert.Equal(t, "fallback", summaryOr("   \t\n", "fallback"))
}

func TestClampEnd(t *testing.T) {
	floor := t0
	tests := []struct {
		name      string
		in        time.Time
		want      time.Time
		wantDelta time.Duration
	}{
		{name: "after the floor is untouched", in: t0.Add(time.Minute), want: t0.Add(time.Minute)},
		{name: "at the floor is untouched", in: t0, want: t0},
		{name: "before the floor is pulled forward", in: t0.Add(-time.Minute), want: t0, wantDelta: time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, delta := clampEnd(tc.in, floor)
			assert.True(t, got.Equal(tc.want))
			assert.Equal(t, tc.wantDelta, delta)
			assert.Equal(t, time.UTC, got.Location())
		})
	}
}

func TestDetectedBy(t *testing.T) {
	assert.Equal(t, DetectedByWebhook, detectedBy(ActorIngest))
	assert.Equal(t, DetectedByReconciler, detectedBy(ActorReconciler))
	assert.Equal(t, "reaper", detectedBy(ActorReaper))
	assert.Equal(t, "", detectedBy(ActorKind{}))
}

func TestMergePayload(t *testing.T) {
	assert.Nil(t, mergePayload(nil, nil))
	assert.Nil(t, mergePayload(map[string]any{}, map[string]any{}))

	base := map[string]any{"a": 1, "shared": "caller"}
	out := mergePayload(base, map[string]any{"shared": "machine", "b": 2})
	assert.Equal(t, map[string]any{"a": 1, "b": 2, "shared": "machine"}, out)
	assert.Equal(t, "caller", base["shared"], "the caller's map is not mutated")
}

func TestLifecycleDefaults(t *testing.T) {
	// ⭐ ONE NUMBER LEFT, AND THE OTHER'S DEPARTURE IS THE POINT. This used to pin
	// `DefaultRefireGrace` too — the window inside which a re-fire took T8 and
	// reopened the closed episode. ADR 0040 retired T8, so the machine has no
	// grace to consult and no fallback copy of one; `refire_grace_s` survives as an
	// `identity` setting because `group_close_delay_s` and the ingest replay window
	// are derived against it, and `identity/domain` owns that number outright now.
	//
	// `resolve_grace` was never part of that arithmetic. It answers a different
	// question — how long past `source_ends_at` the reaper waits before a case may
	// expire (§B.4) — and is still the machine's fallback when no SettingsReader is
	// wired.
	assert.Equal(t, 5*time.Minute, DefaultResolveGrace)
}

// TestTransitionIDs — ⛔ THE NUMBERING HAS A HOLE IN IT, AND THE HOLE IS LOAD-
// BEARING. T8 was the re-fire-inside-the-grace reopen and ADR 0040 retired it;
// the ids either side keep their names because they are the SPEC §B.3 row labels
// every comment, event summary and ADR in the tree refers to, and renumbering
// them to close the gap would silently redirect a decade of prose. So this asserts
// each id spells itself, that they are unique, and that T8 is not among them.
func TestTransitionIDs(t *testing.T) {
	want := []string{"T1", "T2", "T3", "T4", "T5", "T6", "T7", "T9", "T10"}
	ids := []TransitionID{
		TransitionT1, TransitionT2, TransitionT3, TransitionT4, TransitionT5,
		TransitionT6, TransitionT7, TransitionT9, TransitionT10,
	}
	require.Len(t, ids, len(want))

	seen := map[string]struct{}{}
	for i, id := range ids {
		assert.Equal(t, want[i], id.String())
		_, dup := seen[id.String()]
		assert.False(t, dup)
		seen[id.String()] = struct{}{}
	}
	assert.NotContains(t, seen, "T8", "T8 is retired, not renumbered")

	for _, r := range transitionTable {
		assert.NotEqual(t, "T8", r.id.String(),
			"no row of the table may reintroduce the reopen edge")
	}
}
