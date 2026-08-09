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
	occID       = uuid.MustParse("018f3a4b-0000-7000-8000-000000000101")
	alertID     = uuid.MustParse("018f3a4b-0000-7000-8000-000000000102")
	groupIDFix  = uuid.MustParse("018f3a4b-0000-7000-8000-000000000103")
	eventIDFix  = uuid.MustParse("018f3a4b-0000-7000-8000-000000000104")
	prevOccID   = uuid.MustParse("018f3a4b-0000-7000-8000-000000000105")
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

func occurrenceIn(t *testing.T, state State, mut ...func(*OccurrenceParams)) Occurrence {
	t.Helper()
	p := OccurrenceParams{
		ID:             occID,
		OrgID:          orgA,
		AlertID:        alertID,
		GroupID:        groupIDFix,
		Seq:            1,
		State:          state,
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
	o, err := NewOccurrence(p)
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
	// And only from an OPEN state: a terminal occurrence cannot be re-resolved.
	assert.False(t, CanTransition(StateResolved, StateResolved, TriggerObserveResolved))
	assert.False(t, CanTransition(StateExpired, StateResolved, TriggerObserveResolved))
}

// TestExpiredIsNotResolved_NoEdgeBetweenThem is the same rule read the other way:
// losing sight of an alert is not the alert resolving, and there is no path that
// silently converts one into the other.
func TestExpiredIsNotResolved_NoEdgeBetweenThem(t *testing.T) {
	for _, tr := range allTriggers() {
		assert.False(t, CanTransition(StateExpired, StateResolved, tr),
			"an expired occurrence must never become resolved")
		assert.False(t, CanTransition(StateResolved, StateExpired, tr),
			"a resolved occurrence must never become expired")
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
	// Only from firing: a terminal occurrence cannot be suppressed.
	assert.False(t, CanTransition(StateResolved, StateSuppressed, TriggerObserveSuppressed))
	assert.False(t, CanTransition(StateExpired, StateSuppressed, TriggerObserveSuppressed))
	assert.False(t, CanTransition(StateSuppressed, StateSuppressed, TriggerObserveSuppressed))
}

func TestNoEdgeReturnsToStateNone(t *testing.T) {
	for _, from := range allStates() {
		for _, tr := range allTriggers() {
			assert.False(t, CanTransition(from, StateNone, tr),
				"an occurrence never un-happens")
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
	o := occurrenceIn(t, StateFiring)
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
		{name: "suppress a resolved occurrence", state: StateResolved, trigger: TriggerObserveSuppressed, actor: ActorReconciler},
		{name: "resolve an expired occurrence", state: StateExpired, trigger: TriggerObserveResolved, actor: ActorIngest},
		{name: "resolve an already resolved occurrence", state: StateResolved, trigger: TriggerObserveResolved, actor: ActorIngest},
		{name: "reap a resolved occurrence", state: StateResolved, trigger: TriggerReap, actor: ActorReaper},
		{name: "reap an expired occurrence", state: StateExpired, trigger: TriggerReap, actor: ActorReaper},
		{name: "suppress an already suppressed occurrence", state: StateSuppressed, trigger: TriggerObserveSuppressed, actor: ActorReconciler},
		{name: "reap a never-opened occurrence", state: StateNone, trigger: TriggerReap, actor: ActorReaper},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := occurrenceIn(t, tc.state, func(p *OccurrenceParams) {
				if tc.state == StateNone {
					p.State = StateFiring // NewOccurrence requires a real state
				}
			})
			if tc.state == StateNone {
				o.state = StateNone
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
			o := occurrenceIn(t, tc.state)
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

func TestApply_T1IsNotAnEdgeOnAnExistingOccurrence(t *testing.T) {
	o := occurrenceIn(t, StateFiring)
	o.state = StateNone

	_, err := Apply(o, TransitionCommand{
		Trigger: TriggerObserveFiring,
		Actor:   actor(t, ActorIngest),
		At:      at(t, t0, t0),
	})
	requireKind(t, err, errs.KindPrecondition, "no_open_occurrence")
	assert.Contains(t, err.Error(), "OpenOccurrence")
}

// ---------------------------------------------------------------------------- T2

func TestApply_T2_SilentUnlessSomethingMaterialChanged(t *testing.T) {
	o := occurrenceIn(t, StateFiring)
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
	assert.Equal(t, later, res.Occurrence.LastObservedAt())
	assert.Equal(t, DetectedByWebhook, res.DetectedBy)
	assert.False(t, res.Clamped)
	assert.Equal(t, o, res.Before, "Before is the pre-image the verdict was reached against")
	assert.Equal(t, o.StateVersion(), PreconditionFor(res.Before).StateVersion)
}

func TestApply_T2_MaterialChangeEmitsAlertMutated(t *testing.T) {
	o := occurrenceIn(t, StateFiring)
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
	assert.Equal(t, occID, ev.OccurrenceID())
	assert.Empty(t, ev.DedupeKey(), "T2 has no §C.8 dedupe key")
	assert.Equal(t, DetectedByReconciler, res.DetectedBy)
}

func TestApply_T2_ObservationFieldsAreFoldedInNeverCleared(t *testing.T) {
	endsAt := t0.Add(10 * time.Minute)
	o := occurrenceIn(t, StateFiring, func(p *OccurrenceParams) {
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
	assert.Equal(t, endsAt, res.Occurrence.SourceEndsAt(), "a zero endsAt means 'unknown', never 'forget'")
	assert.Equal(t, t0, res.Occurrence.SourceUpdatedAt())
	assert.Nil(t, res.Occurrence.Value())

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
	assert.Equal(t, newEnds, res.Occurrence.SourceEndsAt())
	assert.Equal(t, later, res.Occurrence.SourceUpdatedAt())
	require.NotNil(t, res.Occurrence.Value())
	assert.Equal(t, 42.5, *res.Occurrence.Value())
	assert.Equal(t, 3*time.Second, res.Occurrence.ObservedSkew())
}

// ---------------------------------------------------------------------------- T3

func TestApply_T3_SuppressRequiresAReasonAndCountsTheEpisode(t *testing.T) {
	o := occurrenceIn(t, StateFiring)
	later := t0.Add(time.Minute)

	// A reason is required: occ_suppress_ck ties it to the state.
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
			assert.Equal(t, reason, res.Occurrence.SuppressionReason())
			assert.Equal(t, 1, res.Occurrence.SuppressCount(), "a suppression is a COUNTED fact")
			assert.True(t, res.Occurrence.IsOpen(), "suppressed is an OPEN state, not a terminal one")

			require.Len(t, res.Events, 1)
			assert.Equal(t, EventOccurrenceSuppressed, res.Events[0].Type())
			assert.Equal(t, "occ:"+occID.String()+":suppressed:1", res.Events[0].DedupeKey(),
				"⛔ T3's dedupe key is a COUNTER, never a clock: two concurrent reconciler passes must mint the same key")
		})
	}
}

func TestApply_T3andT4_DedupeKeysPairPerEpisode(t *testing.T) {
	// One suppression episode: T3 then T4 carry the SAME ordinal, so they read as
	// the two halves of one period of silence.
	firing := occurrenceIn(t, StateFiring)
	t1 := t0.Add(time.Minute)

	suppressed, err := Apply(firing, TransitionCommand{
		Trigger: TriggerObserveSuppressed, Actor: actor(t, ActorReconciler),
		At: at(t, t1, t1), EventID: eventIDFix, SuppressionReason: SuppressionSilence,
	})
	require.NoError(t, err)
	assert.Equal(t, "occ:"+occID.String()+":suppressed:1", suppressed.Events[0].DedupeKey())

	t2 := t0.Add(2 * time.Minute)
	unsuppressed, err := Apply(suppressed.Occurrence, TransitionCommand{
		Trigger: TriggerObserveFiring, Actor: actor(t, ActorReconciler),
		At: at(t, t2, t2), EventID: eventIDFix,
	})
	require.NoError(t, err)
	assert.Equal(t, "occ:"+occID.String()+":unsuppressed:1", unsuppressed.Events[0].DedupeKey())
	assert.Equal(t, 1, unsuppressed.Occurrence.SuppressCount(), "T4 leaves the counter alone")

	// A genuine second suppression inside the same episode produces 2, so both
	// facts are recorded rather than collapsed.
	t3 := t0.Add(3 * time.Minute)
	again, err := Apply(unsuppressed.Occurrence, TransitionCommand{
		Trigger: TriggerObserveSuppressed, Actor: actor(t, ActorReconciler),
		At: at(t, t3, t3), EventID: eventIDFix, SuppressionReason: SuppressionInhibition,
	})
	require.NoError(t, err)
	assert.Equal(t, "occ:"+occID.String()+":suppressed:2", again.Events[0].DedupeKey())
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
			o := occurrenceIn(t, StateSuppressed, func(p *OccurrenceParams) {
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
			assert.True(t, res.Occurrence.SuppressionReason().IsZero(),
				"suppression_reason exists only while suppressed")
			assert.True(t, res.Occurrence.SuppressedBy().IsZero(),
				"the witnesses are not left behind on an alert that is demonstrably firing")
			assert.Equal(t, tc.detectedBy, res.DetectedBy)

			require.Len(t, res.Events, 1)
			ev := res.Events[0]
			assert.Equal(t, EventOccurrenceUnsuppressed, ev.Type())
			assert.Equal(t, tc.detectedBy, ev.Payload()["detected_by"],
				"the event records WHICH witness proved suppression had ended")
			assert.Equal(t, "occ:"+occID.String()+":unsuppressed:4", ev.DedupeKey())
		})
	}
}

func TestApply_T4_MachineComputedPayloadKeysWinOverTheCallers(t *testing.T) {
	o := occurrenceIn(t, StateSuppressed)
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
			o := occurrenceIn(t, from)
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
			assert.Equal(t, ResolveUpstream, res.Occurrence.ResolveReason(),
				"resolved is bound one-to-one to resolve_reason=upstream")
			assert.Equal(t, endedAt, res.Occurrence.EndedAt(), "ended_at comes from the UPSTREAM claim")
			assert.True(t, res.Occurrence.SuppressionReason().IsZero())
			assert.False(t, res.Occurrence.IsOpen())
			assert.False(t, res.Clamped)

			require.Len(t, res.Events, 1)
			assert.Equal(t, EventOccurrenceResolved, res.Events[0].Type())
			assert.Equal(t, "Occurrence resolved upstream", res.Events[0].Summary())
			assert.Equal(t, "occ:"+occID.String()+":resolved", res.Events[0].DedupeKey())
		})
	}
}

// TestApply_T5_ClampNeverReject is §B.3.2 and ADR 0021's wedge path 3: a
// backward-skewed upstream clock is clamped and MEASURED, never a reason to
// abort the ingest transaction.
func TestApply_T5_ClampNeverReject(t *testing.T) {
	o := occurrenceIn(t, StateFiring)
	skewed := t0.Add(-90 * time.Second) // upstream claims it ended before it started

	res, err := Apply(o, TransitionCommand{
		Trigger: TriggerObserveResolved,
		Actor:   actor(t, ActorIngest),
		At:      at(t, skewed, t0.Add(time.Minute)),
		EventID: eventIDFix,
	})
	require.NoError(t, err, "a customer's NTP problem must never drop a batch")

	assert.Equal(t, StateResolved, res.To)
	assert.Equal(t, t0, res.Occurrence.EndedAt(), "ended_at = max(occurred_at, started_at)")
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

	base := func(p *OccurrenceParams) { p.SourceEndsAt = endsAt }

	t.Run("blocked while the AlertSource is not healthy", func(t *testing.T) {
		o := occurrenceIn(t, StateFiring, base)
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

	t.Run("an occurrence with no upstream end time cannot expire", func(t *testing.T) {
		o := occurrenceIn(t, StateFiring)
		_, err := Apply(o, TransitionCommand{
			Trigger: TriggerReap, Actor: actor(t, ActorReaper),
			At: at(t, reapAt, reapAt), EventID: eventIDFix, SourceHealthy: true,
		})
		requireKind(t, err, errs.KindPrecondition, "no_source_ends_at")
	})

	t.Run("resolve_grace must have elapsed", func(t *testing.T) {
		o := occurrenceIn(t, StateFiring, base)
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
		o := occurrenceIn(t, StateFiring, base)
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
			o := occurrenceIn(t, from, base)
			res, err := Apply(o, TransitionCommand{
				Trigger: TriggerReap, Actor: actor(t, ActorReaper),
				At: at(t, reapAt, reapAt), EventID: eventIDFix, SourceHealthy: true,
			})
			require.NoError(t, err)

			assert.Equal(t, TransitionT6, res.ID)
			assert.Equal(t, StateExpired, res.To)
			assert.Equal(t, ResolveTimeout, res.Occurrence.ResolveReason(),
				"expired is bound one-to-one to resolve_reason=timeout — never `upstream`")
			assert.NotEqual(t, ResolveUpstream, res.Occurrence.ResolveReason())
			assert.Equal(t, reapAt, res.Occurrence.EndedAt(), "expiry is stamped with OTO's clock")
			assert.True(t, res.Occurrence.SuppressionReason().IsZero())

			require.Len(t, res.Events, 1)
			assert.Equal(t, EventOccurrenceExpired, res.Events[0].Type())
			assert.Equal(t, "Occurrence expired: oto stopped hearing about it", res.Events[0].Summary(),
				"the timeline never claims a resolution it did not observe")
			assert.NotContains(t, strings.ToLower(res.Events[0].Summary()), "resolved")
			assert.Equal(t, "occ:"+occID.String()+":expired", res.Events[0].DedupeKey())
		})
	}
}

// --------------------------------------------------------------------- T7 vs T8

func TestApply_T7vsT8_RefireGrace(t *testing.T) {
	endedAt := t0.Add(time.Minute)

	tests := []struct {
		name    string
		from    State
		grace   time.Duration
		refire  time.Time
		wantID  TransitionID
		wantNew bool
	}{
		{name: "inside the default grace reopens", from: StateResolved, refire: endedAt.Add(5 * time.Minute), wantID: TransitionT8},
		{name: "exactly at the grace boundary reopens", from: StateResolved, refire: endedAt.Add(DefaultRefireGrace), wantID: TransitionT8},
		{name: "one nanosecond past the boundary opens a new episode", from: StateResolved, refire: endedAt.Add(DefaultRefireGrace + time.Nanosecond), wantID: TransitionT7, wantNew: true},
		{name: "expired reopens too", from: StateExpired, refire: endedAt.Add(time.Minute), wantID: TransitionT8},
		{name: "expired past the grace opens a new episode", from: StateExpired, refire: endedAt.Add(time.Hour), wantID: TransitionT7, wantNew: true},
		{name: "a configured grace widens the window", from: StateResolved, grace: 2 * time.Hour, refire: endedAt.Add(time.Hour), wantID: TransitionT8},
		{name: "a configured grace narrows it", from: StateResolved, grace: time.Minute, refire: endedAt.Add(2 * time.Minute), wantID: TransitionT7, wantNew: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := occurrenceIn(t, tc.from, func(p *OccurrenceParams) { p.EndedAt = endedAt })

			res, err := Apply(o, TransitionCommand{
				Trigger:     TriggerObserveFiring,
				Actor:       actor(t, ActorIngest),
				At:          at(t, tc.refire, tc.refire),
				EventID:     eventIDFix,
				RefireGrace: tc.grace,
			})
			require.NoError(t, err)

			assert.Equal(t, tc.wantID, res.ID)
			assert.Equal(t, StateFiring, res.To)
			assert.Equal(t, tc.wantNew, res.OpensNewOccurrence)

			if tc.wantNew {
				// T7 leaves the terminal occurrence UNTOUCHED: it opens a new
				// episode, it does not revive an old one.
				assert.Equal(t, o, res.Occurrence)
				assert.Equal(t, tc.from, res.Occurrence.State())
				assert.Equal(t, endedAt, res.Occurrence.EndedAt())
				assert.Empty(t, res.Events, "the `occurrence.opened` event comes from OpenNewOccurrence")
				return
			}

			assert.Equal(t, StateFiring, res.Occurrence.State())
			assert.True(t, res.Occurrence.EndedAt().IsZero(), "a reopen CLEARS ended_at")
			assert.True(t, res.Occurrence.ResolveReason().IsZero())
			assert.Equal(t, 1, res.Occurrence.ReopenCount())
			assert.Equal(t, occID, res.Occurrence.ID(), "T8 reuses the SAME occurrence and its Slack thread")

			require.Len(t, res.Events, 1)
			assert.Equal(t, EventOccurrenceReopened, res.Events[0].Type())
			assert.Equal(t, "occ:"+occID.String()+":reopened:1", res.Events[0].DedupeKey())
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

	o := occurrenceIn(t, StateResolved, func(p *OccurrenceParams) { p.EndedAt = t0.Add(time.Minute) })
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

// -------------------------------------------------------------- OpenNewOccurrence

func TestOpenNewOccurrence(t *testing.T) {
	p := OpenOccurrenceParams{
		ID:      occID,
		OrgID:   orgA,
		AlertID: alertID,
		GroupID: groupIDFix,
		Seq:     1,
		Actor:   actor(t, ActorIngest),
		At:      at(t, t0.Add(-time.Minute), t0),
		EventID: eventIDFix,
	}

	o, events, err := OpenNewOccurrence(p)
	require.NoError(t, err)

	assert.Equal(t, StateFiring, o.State())
	assert.Equal(t, AckStateUnacked, o.AckState(), "a new occurrence always starts unacked (T10)")
	assert.Equal(t, t0, o.StartedAt(), "started_at is OTO's clock")
	assert.Equal(t, t0, o.LastObservedAt())
	assert.Equal(t, t0.Add(-time.Minute), o.SourceStartsAt(),
		"with no upstream startsAt, the upstream CLAIM is used")
	assert.True(t, o.IsOpen())
	assert.Equal(t, uuid.Nil, o.ReopenOf())
	assert.Equal(t, 1, o.StateVersion())

	require.Len(t, events, 1)
	assert.Equal(t, EventOccurrenceOpened, events[0].Type())
	assert.Equal(t, "Occurrence opened", events[0].Summary())
	assert.Equal(t, "occ:"+occID.String()+":opened", events[0].DedupeKey())
}

func TestOpenNewOccurrence_T7CarriesReopenOf(t *testing.T) {
	o, events, err := OpenNewOccurrence(OpenOccurrenceParams{
		ID: occID, OrgID: orgA, AlertID: alertID, Seq: 2,
		Actor: actor(t, ActorIngest), At: at(t, t0, t0), EventID: eventIDFix,
		ReopenOf: prevOccID,
	})
	require.NoError(t, err)

	assert.Equal(t, prevOccID, o.ReopenOf())
	assert.Equal(t, 2, o.Seq())
	require.Len(t, events, 1)
	assert.Equal(t, EventOccurrenceOpened, events[0].Type(), "T7 still appends `occurrence.opened`")
	assert.Equal(t, "Occurrence opened", events[0].Summary())
}

func TestOpenNewOccurrence_Rejects(t *testing.T) {
	good := OpenOccurrenceParams{
		ID: occID, OrgID: orgA, AlertID: alertID, Seq: 1,
		Actor: actor(t, ActorIngest), At: at(t, t0, t0), EventID: eventIDFix,
	}

	tests := []struct {
		name string
		mut  func(*OpenOccurrenceParams)
		kind errs.Kind
		code string
	}{
		{name: "no actor", mut: func(p *OpenOccurrenceParams) { p.Actor = Actor{} }, kind: errs.KindValidation, code: "required"},
		{name: "no observation time", mut: func(p *OpenOccurrenceParams) { p.At = ObservationTime{} }, kind: errs.KindValidation, code: "required"},
		{name: "the reaper may not open", mut: func(p *OpenOccurrenceParams) { p.Actor = actor(t, ActorReaper) }, kind: errs.KindInternal, code: "wrong_actor"},
		{name: "a human may not open", mut: func(p *OpenOccurrenceParams) { p.Actor = humanActor(t, uuid.New().String(), "Ram") }, kind: errs.KindInternal, code: "wrong_actor"},
		{name: "the notifier may not open", mut: func(p *OpenOccurrenceParams) { p.Actor = actor(t, ActorNotifier) }, kind: errs.KindInternal, code: "wrong_actor"},
		{name: "seq below 1", mut: func(p *OpenOccurrenceParams) { p.Seq = 0 }, kind: errs.KindValidation, code: "min"},
		{name: "cannot reopen itself", mut: func(p *OpenOccurrenceParams) { p.ReopenOf = occID }, kind: errs.KindValidation, code: "field_order"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := good
			tc.mut(&p)
			_, _, err := OpenNewOccurrence(p)
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
			o := occurrenceIn(t, from)
			ackAt := t0.Add(time.Minute)
			userID := uuid.New()

			next, events, err := o.Acknowledge(AckCommand{
				Actor:   humanActor(t, userID.String(), "Ram"),
				At:      at(t, ackAt, ackAt),
				EventID: eventIDFix,
				Note:    "looking at it",
			})
			require.NoError(t, err)

			assert.Equal(t, from, next.State(), "⭐ acknowledging does not move the state")
			assert.Equal(t, from, o.State(), "and the receiver is untouched")
			assert.Equal(t, AckStateAcked, next.AckState())
			assert.Equal(t, ackAt, next.AckedAt())
			assert.Equal(t, userID, next.AckedBy())
			assert.Equal(t, "Ram", next.AckedByLabel())
			assert.Equal(t, "looking at it", next.AckNote())
			assert.True(t, next.IsOpen(), "an acked alert is still firing")
			assert.Equal(t, next.SuppressionReason(), o.SuppressionReason(),
				"ack never touches the suppression axis")

			require.Len(t, events, 1)
			assert.Equal(t, EventOccurrenceAcknowledged, events[0].Type())
			assert.Equal(t, "Acknowledged by Ram", events[0].Summary())
			assert.Contains(t, events[0].DedupeKey(), ":acknowledged:")
		})
	}
}

func TestAcknowledge_Rejects(t *testing.T) {
	tests := []struct {
		name  string
		state State
		mut   func(*OccurrenceParams)
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
			name: "a resolved occurrence cannot be acknowledged", state: StateResolved,
			kind: errs.KindPrecondition, code: "occurrence_terminal",
		},
		{
			name: "an expired occurrence cannot be acknowledged", state: StateExpired,
			kind: errs.KindPrecondition, code: "occurrence_terminal",
		},
		{
			name: "already acked", state: StateFiring,
			mut: func(p *OccurrenceParams) {
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
			var muts []func(*OccurrenceParams)
			if tc.mut != nil {
				muts = append(muts, tc.mut)
			}
			o := occurrenceIn(t, tc.state, muts...)
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
	o := occurrenceIn(t, StateFiring)
	next, _, err := o.Acknowledge(AckCommand{
		Actor:   humanActor(t, uuid.New().String(), "Ram"),
		At:      at(t, t0.Add(-time.Hour), t0.Add(-time.Hour)),
		EventID: eventIDFix,
	})
	require.NoError(t, err)
	assert.Equal(t, t0, next.AckedAt(), "occ_ackorder_ck: acked_at >= started_at")
}

func TestAcknowledge_NonUUIDActorIDLeavesAckedByNil(t *testing.T) {
	// A Slack user id is carried on the event's actor_id, not on the occurrence's
	// FK column.
	o := occurrenceIn(t, StateFiring)
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
	acked := func(t *testing.T) Occurrence {
		return occurrenceIn(t, StateFiring, func(p *OccurrenceParams) {
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
		assert.Equal(t, StateFiring, next.State(), "un-acking does not move the state either")

		require.Len(t, events, 1)
		assert.Equal(t, EventOccurrenceUnacknowledged, events[0].Type())
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
			"the occurrence has nowhere left to keep it: ack_note is cleared")
	})

	t.Run("new_occurrence is machine-driven", func(t *testing.T) {
		o := acked(t)
		_, events, err := o.Unacknowledge(AckCommand{
			Actor:   actor(t, ActorSystem),
			At:      at(t, t0, t0),
			EventID: eventIDFix,
			Reason:  UnackReasonNewOccurrence,
		})
		require.NoError(t, err)
		assert.Equal(t, UnackReasonNewOccurrence, events[0].Payload()["reason"])
	})

	t.Run("rejects", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			o    func(*testing.T) Occurrence
			cmd  func(*AckCommand)
			kind errs.Kind
			code string
		}{
			{name: "not acked", o: func(t *testing.T) Occurrence { return occurrenceIn(t, StateFiring) },
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
				o := occurrenceIn(t, from, func(p *OccurrenceParams) {
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
				if res.OpensNewOccurrence {
					// T7 leaves the terminal occurrence alone; `To` describes the
					// NEW episode the caller must open.
					assert.Equal(t, from, res.Occurrence.State())
					continue
				}
				assert.Equal(t, res.To, res.Occurrence.State())
			}
		}
	}
}

func TestDefaultSummariesAreWithinTheEventBound(t *testing.T) {
	for _, id := range []TransitionID{
		TransitionT1, TransitionT2, TransitionT3, TransitionT4,
		TransitionT5, TransitionT6, TransitionT7, TransitionT8,
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
	assert.Equal(t, 10*time.Minute, DefaultRefireGrace)
	assert.Equal(t, 5*time.Minute, DefaultResolveGrace)
}

func TestTransitionIDs(t *testing.T) {
	ids := []TransitionID{
		TransitionT1, TransitionT2, TransitionT3, TransitionT4, TransitionT5,
		TransitionT6, TransitionT7, TransitionT8, TransitionT9, TransitionT10,
	}
	seen := map[string]struct{}{}
	for i, id := range ids {
		assert.Equal(t, "T"+itoa(i+1), id.String())
		_, dup := seen[id.String()]
		assert.False(t, dup)
		seen[id.String()] = struct{}{}
	}
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
