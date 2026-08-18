package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// These are the tests for the CASE RETENTION WINDOW W (migration 00057): a case
// whose alert has resolved stays OPEN for W and closes only once the alert has
// stayed resolved for W, so a re-fire inside W lands in the still-open episode
// instead of opening the next `seq`.
//
// ⭐⭐ THE FIRST GROUP IS THE SAFETY ARGUMENT AND NOT A FEATURE TEST. The whole
// change ships on the claim that W=0 — the shipped default, and every deployment
// until an operator writes a `case_policy_config` row — is the pre-00057 close
// path unedited. That claim rests on the ORDER of three guards inside Apply's T5
// arm, so the tests below assert the emitted TRANSITION AND EVENT SEQUENCE at
// W=0, not the end state alone: an end state is reachable through the new
// branches as well as around them.
//
// ⛔ THEY NEED NO DATABASE, and that is deliberate: every property here is a
// property of the machine, and the machine takes its clock as a parameter.

// pendingClose puts the two `alert_cases` columns of a DELAYED CLOSE on a case
// under construction — `dueAt` is oto's clock (when the close falls due) and
// `endAt` is the stored UPSTREAM claim the close will stamp.
func pendingClose(dueAt, endAt time.Time) func(*CaseParams) {
	return func(p *CaseParams) {
		p.ResolvePendingAt = dueAt
		p.ResolvePendingEndAt = endAt
	}
}

// ackedBy marks a case acknowledged, so a test can prove an ack does not survive
// into the episode that succeeds it.
func ackedBy(label string, when time.Time) func(*CaseParams) {
	return func(p *CaseParams) {
		p.AckState = AckStateAcked
		p.AckedAt = when
		p.AckedByLabel = label
	}
}

// nextCaseIDFix is the id of the episode that SUCCEEDS the retained one, so a
// test can prove the second `seq` is a new row rather than the first one revived.
var nextCaseIDFix = uuid.MustParse("018f3a4b-0000-7000-8000-000000000107")

// TestTheDueCloseHasExactlyOneEdgeInTheTable closes a gap the shipped table pin
// leaves: `allTriggers` predates migration 00057, so
// TestTransitionTable_IsExactlySpecB3 and TestNeverFabricateAResolution never ask
// about `close_due` at all.
//
// ⛔⛔ ONE ROW IS THE WHOLE OF THE SAFETY ARGUMENT. A second actor on the T5 edge
// is only safe while it can reach ONE state from ONE phase: widening it would let a
// sweep end an episode it has no receipt for. There is deliberately no `suppressed`
// twin either — `case_pending_supp_ck` keeps a pending close and a suppression
// reason off the same row, so a row holding a receipt is always in the firing
// phase.
func TestTheDueCloseHasExactlyOneEdgeInTheTable(t *testing.T) {
	for _, from := range allStates() {
		for _, to := range allStates() {
			want := from == StateFiring && to == StateResolved
			assert.Equal(t, want, CanTransition(from, to, TriggerCloseDue),
				"CanTransition(%q -> %q, close_due)", from, to)
		}
	}
	assert.Equal(t, []State{StateResolved}, TransitionsFrom(StateFiring, TriggerCloseDue))
	assert.Empty(t, TransitionsFrom(StateSuppressed, TriggerCloseDue),
		"a row holding a receipt cannot be suppressed")

	// The trigger is part of the closed set, and spelled exactly once.
	tr, err := NewTrigger("close_due")
	require.NoError(t, err)
	assert.Equal(t, TriggerCloseDue, tr)
	_, err = NewTrigger("close-due")
	requireKind(t, err, errs.KindValidation, "enum")
}

// -------------------------------------------------- W=0 is the pre-00057 path

// TestAZeroWindowClosesTheCaseExactlyAsItClosedBeforeTheWindowExisted is
// migration 00057's done-when 4, executable.
//
// ⭐⭐ IT COMPARES THE WHOLE POST-IMAGE, not the state letter. `res.Case` is
// asserted against a case built independently from CaseParams, so a stray
// `resolve_pending_at`, a moved `ended_at` or any other field the new branches
// could touch fails here rather than in production. The event sequence is pinned
// beside it: the pre-00057 T5 appended exactly one `case.resolved`, and a T5 that
// deferred would append none.
func TestAZeroWindowClosesTheCaseExactlyAsItClosedBeforeTheWindowExisted(t *testing.T) {
	o := caseIn(t, StateFiring)
	occurred := t0.Add(5 * time.Minute)
	recorded := occurred.Add(time.Second)

	res, err := Apply(o, TransitionCommand{
		Trigger:       TriggerObserveResolved,
		Actor:         actor(t, ActorIngest),
		At:            at(t, occurred, recorded),
		EventID:       eventIDFix,
		CaseRetention: 0, // the shipped default, spelled out
	})
	require.NoError(t, err)

	assert.Equal(t, TransitionT5, res.ID)
	assert.Equal(t, StateFiring, res.From)
	assert.Equal(t, StateResolved, res.To, "at W=0 the close happens on the resolve")
	assert.False(t, res.CloseDeferred, "there is no deferral branch to take at W=0")
	assert.False(t, res.OpensNewCase)
	assert.False(t, res.Clamped)

	want := caseIn(t, StateResolved, func(p *CaseParams) {
		p.EndedAt = occurred
		p.LastObservedAt = recorded
	})
	assert.Equal(t, want, res.Case,
		"the whole post-image is the pre-00057 one: nothing about W may touch a field")
	assert.Equal(t, o, res.Before, "the pre-image the verdict was reached against")

	require.Len(t, res.Events, 1, "one case.resolved, exactly as before W existed")
	assert.Equal(t, EventCaseResolved, res.Events[0].Type())
	assert.Equal(t, "Case resolved upstream", res.Events[0].Summary())
	assert.Equal(t, "case:"+caseID.String()+":resolved", res.Events[0].DedupeKey())
}

// TestNoEdgeWritesAPendingCloseAtAZeroWindow is the same claim read over the
// WHOLE table rather than over T5 alone: on a deployment that has configured no
// window, no §B.3 edge can leave a receipt on a row, so `case_close_due_idx` is
// empty and the sweep has nothing to find.
//
// ⭐ THE `close_due` ROW IS THE POINT OF THE TABLE. At W=0 the delayed close is
// not merely unused, it is UNREACHABLE — the trigger refuses the edge because no
// row carries the receipt only a configured window can write. That refusal is
// what makes "a resolution is never fabricated" survive a second actor on the T5
// edge.
func TestNoEdgeWritesAPendingCloseAtAZeroWindow(t *testing.T) {
	when := t0.Add(30 * time.Minute) // past source_ends_at + the default grace
	reapable := func(p *CaseParams) { p.SourceEndsAt = t0.Add(time.Minute) }

	tests := []struct {
		name    string
		from    State
		params  []func(*CaseParams)
		trigger Trigger
		kind    ActorKind
		mutate  func(*TransitionCommand)
		wantID  TransitionID
		wantTo  State
		wantErr string
	}{
		{
			name: "T2 — a repeat observation", from: StateFiring,
			trigger: TriggerObserveFiring, kind: ActorIngest,
			wantID: TransitionT2, wantTo: StateFiring,
		},
		{
			name: "T3 — suppression begins", from: StateFiring,
			trigger: TriggerObserveSuppressed, kind: ActorReconciler,
			mutate: func(c *TransitionCommand) { c.SuppressionReason = SuppressionSilence },
			wantID: TransitionT3, wantTo: StateSuppressed,
		},
		{
			name: "T4 — suppression ends", from: StateSuppressed,
			trigger: TriggerObserveFiring, kind: ActorIngest,
			wantID: TransitionT4, wantTo: StateFiring,
		},
		{
			name: "T5 — an upstream resolve", from: StateFiring,
			trigger: TriggerObserveResolved, kind: ActorIngest,
			wantID: TransitionT5, wantTo: StateResolved,
		},
		{
			name: "T5 — an upstream resolve while suppressed", from: StateSuppressed,
			trigger: TriggerObserveResolved, kind: ActorIngest,
			wantID: TransitionT5, wantTo: StateResolved,
		},
		{
			name: "T6 — the reaper", from: StateFiring, params: []func(*CaseParams){reapable},
			trigger: TriggerReap, kind: ActorReaper,
			mutate: func(c *TransitionCommand) { c.SourceHealthy = true },
			wantID: TransitionT6, wantTo: StateExpired,
		},
		{
			name: "T7 — a re-fire out of resolved", from: StateResolved,
			trigger: TriggerObserveFiring, kind: ActorIngest,
			wantID: TransitionT7, wantTo: StateFiring,
		},
		{
			name: "T7 — a re-fire out of expired", from: StateExpired,
			trigger: TriggerObserveFiring, kind: ActorIngest,
			wantID: TransitionT7, wantTo: StateFiring,
		},
		{
			name: "the delayed close is unreachable — no row carries a receipt",
			from: StateFiring, trigger: TriggerCloseDue, kind: ActorReaper,
			wantErr: "no_pending_close",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := caseIn(t, tc.from, tc.params...)
			cmd := TransitionCommand{
				Trigger:       tc.trigger,
				Actor:         actor(t, tc.kind),
				At:            at(t, when, when),
				EventID:       eventIDFix,
				CaseRetention: 0,
			}
			if tc.mutate != nil {
				tc.mutate(&cmd)
			}

			res, err := Apply(o, cmd)
			if tc.wantErr != "" {
				requireKind(t, err, errs.KindPrecondition, tc.wantErr)
				return
			}
			require.NoError(t, err)

			assert.Equal(t, tc.wantID, res.ID)
			assert.Equal(t, tc.wantTo, res.To)
			assert.False(t, res.CloseDeferred, "nothing defers at W=0")
			assert.False(t, res.Case.ClosePending(),
				"no edge may leave a pending close on a deployment with no window")
			assert.True(t, res.Case.ResolvePendingAt().IsZero())
			assert.True(t, res.Case.ResolvePendingEndAt().IsZero())
		})
	}
}

// TestOnlyAConfiguredWindowDefersTheClose pins the `cmd.CaseRetention > 0` guard
// itself: the deferral is entered by a configured window and by nothing else.
//
// ⛔ REMOVING THE GUARD MAKES THIS FAIL AT W=0 — the row would be left open with
// `resolve_pending_at = recorded_at + 0`, which is a non-zero instant, so the
// episode would stop closing on its resolve and the timeline would lose its
// `case.resolved` until a sweep came back for it.
func TestOnlyAConfiguredWindowDefersTheClose(t *testing.T) {
	occurred := t0.Add(4 * time.Minute)
	recorded := occurred.Add(2 * time.Second)

	tests := []struct {
		name   string
		window time.Duration
	}{
		{name: "no row configured", window: 0},
		{name: "a row configured with W=0 on purpose", window: MinCaseRetentionWindow},
	}
	for _, tc := range tests {
		t.Run(tc.name+" closes on the resolve", func(t *testing.T) {
			res, err := Apply(caseIn(t, StateFiring), TransitionCommand{
				Trigger:       TriggerObserveResolved,
				Actor:         actor(t, ActorIngest),
				At:            at(t, occurred, recorded),
				EventID:       eventIDFix,
				CaseRetention: tc.window,
			})
			require.NoError(t, err)
			assert.Equal(t, StateResolved, res.To)
			assert.False(t, res.Case.IsOpen())
			assert.Equal(t, ResolveUpstream, res.Case.ResolveReason())
			assert.False(t, res.Case.ClosePending())
			assert.False(t, res.CloseDeferred)
			require.Len(t, res.Events, 1)
		})
	}

	t.Run("a configured window records the resolve and closes nothing", func(t *testing.T) {
		const w = 10 * time.Minute
		res, err := Apply(caseIn(t, StateFiring), TransitionCommand{
			Trigger:       TriggerObserveResolved,
			Actor:         actor(t, ActorIngest),
			At:            at(t, occurred, recorded),
			EventID:       eventIDFix,
			CaseRetention: w,
		})
		require.NoError(t, err)

		assert.Equal(t, TransitionT5, res.ID)
		assert.Equal(t, StateFiring, res.From)
		assert.Equal(t, StateFiring, res.To, "a deferred close moves no §B.2 state")
		assert.True(t, res.CloseDeferred)
		assert.True(t, res.Case.IsOpen(), "the episode stays OPEN for W")
		assert.True(t, res.Case.ResolveReason().IsZero(), "nothing has resolved yet")
		assert.True(t, res.Case.EndedAt().IsZero())

		assert.True(t, res.Case.ClosePending())
		assert.Equal(t, recorded.Add(w), res.Case.ResolvePendingAt(),
			"the due time is oto's clock plus W")
		assert.Equal(t, occurred, res.Case.ResolvePendingEndAt(),
			"the receipt carries the UPSTREAM claim, which is what the close will stamp")

		assert.Empty(t, res.Events,
			"a deferred close appends nothing: `case.resolved` belongs to the real close")
	})
}

// TestTheSweepsTriggerIsReadBeforeTheWindowIs pins the ORDER of the two branches
// in the T5 arm — `Trigger == TriggerCloseDue` first, `CaseRetention > 0` second.
//
// ⛔ SWAPPING THEM MAKES THIS FAIL, and the failure it describes is an episode
// that never closes: the sweep would arrive with the window still configured, take
// the deferral branch, push `resolve_pending_at` another W into the future and
// hand itself the same row on the next tick, for ever. The command below carries
// BOTH a due-close trigger and a live window, which is the only input that can
// tell the two orders apart.
func TestTheSweepsTriggerIsReadBeforeTheWindowIs(t *testing.T) {
	const w = 10 * time.Minute
	dueAt := t0.Add(12 * time.Minute)
	upstreamEnd := t0.Add(2 * time.Minute)
	sweptAt := dueAt.Add(30 * time.Second)

	res, err := Apply(caseIn(t, StateFiring, pendingClose(dueAt, upstreamEnd)), TransitionCommand{
		Trigger:       TriggerCloseDue,
		Actor:         actor(t, ActorReaper),
		At:            at(t, sweptAt, sweptAt),
		EventID:       eventIDFix,
		CaseRetention: w, // still configured — the sweep does not un-configure it
	})
	require.NoError(t, err)

	assert.Equal(t, TransitionT5, res.ID)
	assert.Equal(t, StateResolved, res.To, "the due close completes; it does not re-defer")
	assert.False(t, res.CloseDeferred)
	assert.False(t, res.Case.ClosePending(), "the receipt is spent, not renewed")
	assert.Equal(t, upstreamEnd, res.Case.EndedAt())
	require.Len(t, res.Events, 1)
	assert.Equal(t, EventCaseResolved, res.Events[0].Type())
}

// TestAnImmediateCloseSpendsAReceiptLeftByANarrowedWindow covers the trailing
// `o.ClosePending()` branch — the one that sits AFTER the pre-existing statements
// of the T5 arm and is false on every W=0 deployment.
//
// ⭐ THE ONE SEQUENCE THAT REACHES IT is an operator narrowing W to 0 between a
// resolve and the next observation: the row is carrying a receipt and the close is
// now due immediately. ⛔ DELETING THE BRANCH MAKES THIS FAIL LOUDLY rather than
// silently — the post-image would be a CLOSED case still carrying a pending
// close, which `case_pending_open_ck` and `Case.check` both refuse, so Apply
// returns an internal error and the ingest batch dies.
func TestAnImmediateCloseSpendsAReceiptLeftByANarrowedWindow(t *testing.T) {
	stored := t0.Add(4 * time.Minute)
	o := caseIn(t, StateFiring, pendingClose(t0.Add(14*time.Minute), stored))

	occurred := t0.Add(6 * time.Minute)
	recorded := occurred.Add(time.Second)

	res, err := Apply(o, TransitionCommand{
		Trigger:       TriggerObserveResolved,
		Actor:         actor(t, ActorIngest),
		At:            at(t, occurred, recorded),
		EventID:       eventIDFix,
		CaseRetention: 0, // the operator narrowed the window
	})
	require.NoError(t, err, "a closed case carrying a receipt is a state no CHECK admits")

	assert.Equal(t, StateResolved, res.To)
	assert.False(t, res.CloseDeferred)
	assert.Equal(t, occurred, res.Case.EndedAt(),
		"the FRESH upstream claim closes it, not the stored receipt")
	assert.False(t, res.Case.ClosePending())
	assert.True(t, res.Case.ResolvePendingEndAt().IsZero())
	require.Len(t, res.Events, 1)
	assert.Equal(t, EventCaseResolved, res.Events[0].Type())
}

// ------------------------------------------------- a re-fire inside the window

// TestARefireInsideTheWindowLandsInTheSameCase is the ticket's own sentence: "a
// re-fire inside W finds the case still open". It is what turns six flaps into one
// case, one notification and one thread reply.
func TestARefireInsideTheWindowLandsInTheSameCase(t *testing.T) {
	dueAt := t0.Add(10 * time.Minute)
	o := caseIn(t, StateFiring, pendingClose(dueAt, t0.Add(3*time.Minute)))
	refire := t0.Add(5 * time.Minute) // inside W

	cmd := TransitionCommand{
		Trigger: TriggerObserveFiring,
		Actor:   actor(t, ActorIngest),
		At:      at(t, refire, refire),
		EventID: eventIDFix,
	}

	d, err := Decide(o, cmd)
	require.NoError(t, err)
	assert.Equal(t, TransitionT2, d.ID, "the case is still open, so this is a repeat observation")
	assert.False(t, d.OpensEpisode, "no new case is opened inside the window")
	assert.Zero(t, d.Seq)
	assert.False(t, d.DropsAck)

	res, err := Apply(o, cmd)
	require.NoError(t, err)

	assert.Equal(t, TransitionT2, res.ID)
	assert.Equal(t, StateFiring, res.From)
	assert.Equal(t, StateFiring, res.To)
	assert.False(t, res.OpensNewCase)
	assert.Equal(t, o.ID(), res.Case.ID(), "the SAME case")
	assert.Equal(t, 1, res.Case.Seq(), "no next seq is minted")
	assert.Equal(t, o.StartedAt(), res.Case.StartedAt())
	assert.True(t, res.Case.IsOpen())

	assert.False(t, res.Case.ClosePending(),
		"the re-fire cancels the pending close: the alert is demonstrably on fire")
	assert.False(t, res.Case.CloseDue(dueAt.Add(time.Hour)),
		"no clock can make a cleared receipt due")
	assert.Empty(t, res.Events, "a repeat observation that changed nothing is silent")
}

// TestASuppressedObservationDropsAHeldResolve is the same clearing on T3. A
// suppressed observation says the alert is PRESENT upstream and muted, so the
// resolve on the row is stale.
//
// ⛔ IT ALSO GUARDS `case_pending_supp_ck`: a receipt left behind here would sit
// beside a `suppression_reason`, which is oto saying "silenced by <id>" about an
// episode upstream has already called resolved. Apply would fail internally
// rather than write it.
func TestASuppressedObservationDropsAHeldResolve(t *testing.T) {
	o := caseIn(t, StateFiring, pendingClose(t0.Add(10*time.Minute), t0.Add(3*time.Minute)))
	seen := t0.Add(6 * time.Minute)

	res, err := Apply(o, TransitionCommand{
		Trigger:           TriggerObserveSuppressed,
		Actor:             actor(t, ActorReconciler),
		At:                at(t, seen, seen),
		EventID:           eventIDFix,
		SuppressionReason: SuppressionSilence,
	})
	require.NoError(t, err)

	assert.Equal(t, TransitionT3, res.ID)
	assert.Equal(t, StateSuppressed, res.To)
	assert.False(t, res.Case.ClosePending(), "a stale receipt does not survive a suppression")
	assert.Equal(t, SuppressionSilence, res.Case.SuppressionReason())
}

// TestAResolveRefireResolveInsideTheWindowIsOneCaseWithOneStartedAt is migration
// 00057's done-when 3 and ADR 0040's terminality, together: the flap is ONE
// episode with one `started_at`, one `case.resolved`, and a firing duration that
// is not charged the window.
//
// ⭐ THE DUE TIME MOVES FORWARD ON THE SECOND RESOLVE, which is what makes the
// rule "stayed resolved for W" rather than "resolved W ago".
func TestAResolveRefireResolveInsideTheWindowIsOneCaseWithOneStartedAt(t *testing.T) {
	const w = 10 * time.Minute
	ingest := actor(t, ActorIngest)
	opened := caseIn(t, StateFiring) // started at t0, seq 1

	resolveAt := func(o Case, when time.Time) TransitionResult {
		t.Helper()
		r, err := Apply(o, TransitionCommand{
			Trigger: TriggerObserveResolved, Actor: ingest,
			At: at(t, when, when), EventID: eventIDFix, CaseRetention: w,
		})
		require.NoError(t, err)
		return r
	}

	// 1. the first resolve, two minutes in — held, not performed.
	first := resolveAt(opened, t0.Add(2*time.Minute))
	assert.True(t, first.CloseDeferred)
	assert.True(t, first.Case.IsOpen())
	assert.Equal(t, t0.Add(12*time.Minute), first.Case.ResolvePendingAt())
	assert.Empty(t, first.Events)

	// 2. the re-fire, three minutes later and well inside the window.
	refire, err := Apply(first.Case, TransitionCommand{
		Trigger: TriggerObserveFiring, Actor: ingest,
		At: at(t, t0.Add(5*time.Minute), t0.Add(5*time.Minute)), EventID: eventIDFix,
	})
	require.NoError(t, err)
	assert.Equal(t, TransitionT2, refire.ID)
	assert.False(t, refire.Case.ClosePending())
	assert.Empty(t, refire.Events)

	// 3. the second resolve, eight minutes in. The window restarts from HERE.
	second := resolveAt(refire.Case, t0.Add(8*time.Minute))
	assert.True(t, second.CloseDeferred)
	assert.Equal(t, t0.Add(18*time.Minute), second.Case.ResolvePendingAt(),
		"the due time moved forward: the rule is `stayed resolved for W`")
	assert.False(t, second.Case.CloseDue(t0.Add(12*time.Minute)),
		"the first resolve's due time is no longer due — it was superseded")
	assert.True(t, second.Case.CloseDue(t0.Add(18*time.Minute)))
	assert.Empty(t, second.Events)

	// 4. the window elapses and the sweep spends the receipt.
	sweptAt := t0.Add(18 * time.Minute)
	closed, err := Apply(second.Case, TransitionCommand{
		Trigger: TriggerCloseDue, Actor: actor(t, ActorReaper),
		At: at(t, sweptAt, sweptAt), EventID: eventIDFix,
	})
	require.NoError(t, err)

	// ONE case, one started_at, one seq, through the whole flap.
	for _, r := range []TransitionResult{first, refire, second, closed} {
		assert.Equal(t, caseID, r.Case.ID(), "one case across the flap")
		assert.Equal(t, 1, r.Case.Seq(), "no re-fire inside the window minted a seq")
		assert.Equal(t, t0, r.Case.StartedAt(), "one started_at")
		assert.False(t, r.OpensNewCase)
	}

	// ONE case.resolved, at the real close.
	assert.Equal(t, StateResolved, closed.To)
	assert.Equal(t, ResolveUpstream, closed.Case.ResolveReason())
	require.Len(t, closed.Events, 1)
	assert.Equal(t, EventCaseResolved, closed.Events[0].Type())
	assert.Equal(t, "case:"+caseID.String()+":resolved", closed.Events[0].DedupeKey())

	// And the window is not charged to the signal (R8).
	assert.Equal(t, t0.Add(8*time.Minute), closed.Case.EndedAt(),
		"ended_at is the LAST upstream claim, never the sweep's clock")
	assert.Equal(t, 8*time.Minute, closed.Case.Duration(sweptAt),
		"the episode burned eight minutes, not eighteen")

	// It closes EXACTLY ONCE: the edge does not exist from a terminal state.
	_, err = Apply(closed.Case, TransitionCommand{
		Trigger: TriggerCloseDue, Actor: actor(t, ActorReaper),
		At: at(t, sweptAt.Add(time.Minute), sweptAt.Add(time.Minute)), EventID: eventIDFix,
	})
	requireKind(t, err, errs.KindPrecondition, "illegal_transition")
}

// ---------------------------------------------------- the close, and what next

// TestThePendingCloseStampsTheStoredUpstreamClaimAndNotTheSweepClock is the
// reason the receipt is TWO columns and not one: `resolve_pending_at` is oto's
// clock and `resolve_pending_end_at` is the `ended_at` the close will stamp, so
// the window is oto's own damper and is never charged to the signal (R8).
func TestThePendingCloseStampsTheStoredUpstreamClaimAndNotTheSweepClock(t *testing.T) {
	dueAt := t0.Add(10 * time.Minute)
	upstreamEnd := t0.Add(90 * time.Second)
	sweptAt := dueAt.Add(37 * time.Second) // the sweep runs when it runs

	o := caseIn(t, StateFiring, pendingClose(dueAt, upstreamEnd))

	t.Run("it closes on the stored claim", func(t *testing.T) {
		res, err := Apply(o, TransitionCommand{
			Trigger: TriggerCloseDue, Actor: actor(t, ActorReaper),
			At: at(t, sweptAt, sweptAt), EventID: eventIDFix,
		})
		require.NoError(t, err)

		assert.Equal(t, TransitionT5, res.ID, "the delayed half of T5, not an edge of its own")
		assert.Equal(t, StateResolved, res.To)
		assert.Equal(t, ResolveUpstream, res.Case.ResolveReason(),
			"upstream, because an explicit upstream resolve is what wrote the receipt")
		assert.Equal(t, upstreamEnd, res.Case.EndedAt(),
			"ended_at is the STORED upstream claim")
		assert.NotEqual(t, sweptAt, res.Case.EndedAt(), "and never the sweep's clock")
		assert.Equal(t, 90*time.Second, res.Case.Duration(sweptAt),
			"the signal is not charged the window")

		assert.False(t, res.Case.IsOpen())
		assert.False(t, res.Case.ClosePending(), "the receipt is spent in the same write")
		assert.True(t, res.Case.ResolvePendingEndAt().IsZero())
		assert.False(t, res.CloseDeferred)
		assert.False(t, res.Clamped, "the receipt was clamped when it was written")

		require.Len(t, res.Events, 1)
		assert.Equal(t, EventCaseResolved, res.Events[0].Type())
		assert.Equal(t, "Case resolved upstream", res.Events[0].Summary())
	})

	t.Run("the close is due at the instant the window elapses", func(t *testing.T) {
		assert.False(t, o.CloseDue(dueAt.Add(-time.Nanosecond)))
		assert.True(t, o.CloseDue(dueAt), "the boundary itself is due")
		assert.True(t, o.CloseDue(dueAt.Add(time.Nanosecond)))
	})

	t.Run("it cannot mint a resolution it was not handed", func(t *testing.T) {
		_, err := Apply(caseIn(t, StateFiring), TransitionCommand{
			Trigger: TriggerCloseDue, Actor: actor(t, ActorReaper),
			At: at(t, sweptAt, sweptAt), EventID: eventIDFix,
		})
		requireKind(t, err, errs.KindPrecondition, "no_pending_close")
	})

	t.Run("only the reaper drives it", func(t *testing.T) {
		for _, kind := range []ActorKind{ActorIngest, ActorReconciler, ActorSystem} {
			_, err := Apply(o, TransitionCommand{
				Trigger: TriggerCloseDue, Actor: actor(t, kind),
				At: at(t, sweptAt, sweptAt), EventID: eventIDFix,
			})
			requireKind(t, err, errs.KindInternal, "wrong_actor")
		}
	})
}

// TestARefireAfterTheWindowOpensTheNextCaseUnacknowledged is ADR 0040 surviving
// W intact: the window moves WHEN a case closes, never how many times. Once the
// close has happened the episode is terminal, and the next firing is the next
// `seq` — unacknowledged, because an acknowledgement is a receipt for one firing
// and this is not the one that was signed for.
func TestARefireAfterTheWindowOpensTheNextCaseUnacknowledged(t *testing.T) {
	dueAt := t0.Add(10 * time.Minute)
	upstreamEnd := t0.Add(2 * time.Minute)

	held := caseIn(t, StateFiring,
		pendingClose(dueAt, upstreamEnd),
		ackedBy("@ram", t0.Add(time.Minute)))

	closed, err := Apply(held, TransitionCommand{
		Trigger: TriggerCloseDue, Actor: actor(t, ActorReaper),
		At: at(t, dueAt, dueAt), EventID: eventIDFix,
	})
	require.NoError(t, err)
	require.False(t, closed.Case.IsOpen())

	// The re-fire arrives AFTER the window has been spent.
	refire := dueAt.Add(90 * time.Second)
	cmd := TransitionCommand{
		Trigger: TriggerObserveFiring, Actor: actor(t, ActorIngest),
		At: at(t, refire, refire), EventID: eventIDFix,
	}

	d, err := Decide(closed.Case, cmd)
	require.NoError(t, err)
	assert.Equal(t, TransitionT7, d.ID, "a closed episode is terminal: this opens the next one")
	assert.True(t, d.OpensEpisode)
	assert.Equal(t, 2, d.Seq)
	assert.True(t, d.DropsAck, "an ack does not survive into the succeeding episode")

	res, err := Apply(closed.Case, cmd)
	require.NoError(t, err)
	assert.Equal(t, TransitionT7, res.ID)
	assert.True(t, res.OpensNewCase)
	assert.Equal(t, closed.Case, res.Case, "T7 leaves the terminal episode exactly as it found it")
	assert.Empty(t, res.Events, "the new episode appends its own case.opened")

	next, evs, err := OpenNewCase(OpenCaseParams{
		ID: nextCaseIDFix, OrgID: orgA, AlertID: alertID, GroupID: groupIDFix, Seq: d.Seq,
		Actor: actor(t, ActorIngest), At: at(t, refire, refire), EventID: eventIDFix,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, next.Seq())
	assert.NotEqual(t, closed.Case.ID(), next.ID(), "a new row, never the old one revived")
	assert.Equal(t, AckStateUnacked, next.AckState(), "the next case starts unacknowledged")
	assert.True(t, next.AckedAt().IsZero())
	assert.Equal(t, refire, next.StartedAt())
	assert.False(t, next.ClosePending(), "a new episode carries no receipt")
	require.Len(t, evs, 1)
	assert.Equal(t, EventCaseOpened, evs[0].Type())
}

// ------------------------------------------- T6 may never overwrite a held resolve

// TestAHeldResolveIsNeverStampedExpired is the regression test for the latent bug
// migration 00057 would otherwise have introduced.
//
// ⛔⛔ THE BUG. A pending close leaves `ended_at` NULL, so an episode inside its
// retention window looks exactly like an episode the reaper is entitled to expire:
// open, with a `source_ends_at` whose `resolve_grace` has long elapsed. Expiring
// it would stamp `expired`/`timeout` over a resolve oto was HOLDING — oto claiming
// it stopped hearing about an alert whose resolution it had in hand, which is
// precisely the resolved-versus-expired fabrication 00007 calls the distinction it
// must never blur.
//
// ⭐ THE CONTROL IS THE ASSERTION. The two halves below run the SAME command
// against two cases that differ in one thing: the receipt. Without it the reaper
// expires the episode; with it the machine refuses. Delete the `o.ClosePending()`
// guard from Apply's T6 arm and the first half starts producing
// `expired`/`timeout`.
func TestAHeldResolveIsNeverStampedExpired(t *testing.T) {
	endsAt := t0.Add(time.Minute)
	reapAt := t0.Add(30 * time.Minute) // source_ends_at + the default grace, long past
	reapable := func(p *CaseParams) { p.SourceEndsAt = endsAt }

	cmd := TransitionCommand{
		Trigger:       TriggerReap,
		Actor:         actor(t, ActorReaper),
		At:            at(t, reapAt, reapAt),
		EventID:       eventIDFix,
		SourceHealthy: true,
	}

	t.Run("a case holding an upstream resolve refuses the reaper", func(t *testing.T) {
		held := caseIn(t, StateFiring, reapable,
			pendingClose(t0.Add(20*time.Minute), t0.Add(2*time.Minute)))

		_, err := Apply(held, cmd)
		requireKind(t, err, errs.KindPrecondition, "close_pending")
		assert.Contains(t, err.Error(), "never expired")
		assert.Contains(t, err.Error(), "closes as resolved")
	})

	t.Run("the same command expires the same case without the receipt", func(t *testing.T) {
		res, err := Apply(caseIn(t, StateFiring, reapable), cmd)
		require.NoError(t, err, "the control: nothing but the receipt is holding the reaper off")
		assert.Equal(t, TransitionT6, res.ID)
		assert.Equal(t, StateExpired, res.To)
		assert.Equal(t, ResolveTimeout, res.Case.ResolveReason())
	})

	t.Run("a receipt whose window has ALREADY elapsed is still not expirable", func(t *testing.T) {
		// The due close is the only edge that may end this row, so the reaper must
		// stand down even when it is racing a sweep that is entitled to run.
		held := caseIn(t, StateFiring, reapable,
			pendingClose(t0.Add(5*time.Minute), t0.Add(2*time.Minute)))
		require.True(t, held.CloseDue(reapAt))

		_, err := Apply(held, cmd)
		requireKind(t, err, errs.KindPrecondition, "close_pending")
	})

	t.Run("the receipt is read before the §B.4 guard and before the grace", func(t *testing.T) {
		// Order matters for the refusal a caller LOGS: "held while the source is
		// unhealthy" and "holding an upstream resolve" are different facts, and the
		// second is the one that must be reported about a row carrying a receipt.
		held := caseIn(t, StateFiring, pendingClose(t0.Add(20*time.Minute), t0.Add(2*time.Minute)))
		unhealthy := cmd
		unhealthy.SourceHealthy = false

		_, err := Apply(held, unhealthy)
		requireKind(t, err, errs.KindPrecondition, "close_pending")
	})
}
