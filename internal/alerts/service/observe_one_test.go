package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/test/harness"
)

// These drive ONE observation at a time through `observeOne` — the per-observation
// seam — against a REAL POSTGRES. The routing is pinned as a value in
// `alerts/domain`'s decide_test.go; what is pinned here is the other half: what
// the verdict WRITES, and in particular that the two rows which open an episode
// go through one function and can no longer disagree.
//
// ⚠️ THEY OBSERVE AT harness.Epoch, like everything else on the harness. They
// used to observe at a `partitionedNow()` derived from the wall clock, because
// `alert_events` has NO default partition on purpose and the manager built its
// months around the DATABASE's `now()`, so an append stamped at Epoch failed with
// SQLSTATE 23514 "no partition of relation alert_events found for row". The
// harness template builds a window around Epoch too now (git-bug 6547228), so the
// helper is gone from here and from `grouping/service`'s fanout_test.go, which
// carried a copy of it.

// observeOnce runs ONE observation through the seam, with no batch around it.
//
// ⭐ THIS IS WHAT THE REFACTOR BOUGHT. Reaching a single §B.3 edge used to mean
// assembling an observation slice and calling `ObserveBatch`, so an assertion
// about one edge was always an assertion about a batch. Here the accumulator is
// the test's, and it holds exactly what this one observation decided.
func (f *fixture) observeOnce(o domain.Observation, opt ObserveOptions) *observeAccum {
	f.t.Helper()
	ctx := f.t.Context()

	upserts, err := buildUpserts([]domain.Observation{o})
	require.NoError(f.t, err)
	results, err := f.svc.alerts.UpsertBatch(ctx, f.scope, upserts)
	require.NoError(f.t, err)
	require.Len(f.t, results, 1)

	latest, err := f.svc.latestCases(ctx, f.scope, []uuid.UUID{results[0].Alert.ID()})
	require.NoError(f.t, err)

	acc := &observeAccum{
		latest:     latest,
		newEpisode: map[uuid.UUID]int{},
	}
	require.NoError(f.t, f.svc.observeOne(ctx, f.scope, o, results[0], domain.Alert{},
		opt, DefaultSettings(), acc))
	return acc
}

// eventTypes names the timeline entries an observation accumulated, in order.
func eventTypes(acc *observeAccum) []string {
	out := make([]string, 0, len(acc.events))
	for _, e := range acc.events {
		out = append(out, e.Type().String())
	}
	return out
}

// notifyReasons names the §H.6 intents an observation accumulated, in order.
func notifyReasons(acc *observeAccum) []string {
	out := make([]string, 0, len(acc.notifies))
	for _, n := range acc.notifies {
		out = append(out, n.reason)
	}
	return out
}

// TestObserveOne_T1_OpensTheFirstEpisode is the opening row seen from the effects
// side: one episode, one enrichment, one `fired`, and NO unack — there was no
// acknowledgement to drop.
func TestObserveOne_T1_OpensTheFirstEpisode(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	opt := ObserveOptions{}

	obs := f.observation(domain.ObservedByIngest, "firing", now, now.Add(-time.Minute), time.Time{})
	acc := f.observeOnce(obs, opt)

	require.Len(t, acc.outcomes, 1)
	out := acc.outcomes[0]
	assert.Equal(t, domain.TransitionT1.String(), out.Transition)
	assert.True(t, out.CaseOpened)
	assert.True(t, out.AlertCreated)
	// ⭐ EMPTY IS THE HONEST `From` FOR T1 AND ONLY FOR T1: there was no episode
	// to come from. See the T7 test, where it is `resolved`.
	assert.Empty(t, out.From)
	assert.Equal(t, domain.StateFiring.String(), out.To)

	ac := f.currentCase()
	assert.Equal(t, ac.ID(), out.CaseID)
	// ⭐ `seq` 1 IS WHAT SAYS THIS EPISODE SUCCEEDS NOTHING, now that `reopen_of` is
	// gone: the episode a case follows is the row at `seq - 1`, and for the first
	// there is none. Its own state is `open`; `firing` is the ALERT's reading of it.
	assert.Equal(t, 1, ac.Seq())
	assert.Equal(t, domain.CaseOpen, ac.State())
	assert.Equal(t, domain.StateFiring, ac.AlertState())

	assert.Equal(t, []uuid.UUID{ac.ID()}, acc.enrichIDs)
	assert.Equal(t, 1, acc.newEpisode[out.AlertID])
	assert.Equal(t, []string{
		domain.EventAlertCreated.String(),
		domain.EventCaseOpened.String(),
	}, eventTypes(acc))
	assert.Equal(t, []string{reasonFired}, notifyReasons(acc))
	// ⭐ THE INTENT NAMES THE CASE, because the Case IS the conversation
	// (git-bug `7570090`). It used to name the generation the batch carried in.
	assert.Equal(t, ac.ID(), acc.notifies[0].caseID)
}

// TestObserveOne_T7_OutOfAnAckedEpisodeIsDecidedInOnePlace is the regression test
// for the divergence this refactor closed.
//
// T1 and T7 both open an episode, and they used to do it in two near-identical
// blocks forty lines apart inside one 294-line function. The blocks had drifted:
// one enqueued `unacked` beside the auto-unack event and the other enqueued
// nothing, and neither site said which was intended. SPEC §B.3 T10 is explicit —
// "Emit `case.unacknowledged` … enqueue `notify.evaluate(reason=unacked)`"
// — so the notifying half is the correct one, and it now happens in `applyOpen`,
// which is the ONLY place either row can be applied from.
func TestObserveOne_T7_OutOfAnAckedEpisodeIsDecidedInOnePlace(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	opt := ObserveOptions{}
	ctx := t.Context()

	// 1. The first episode, opened by the real path.
	firstSeen := now.Add(-3 * time.Hour)
	f.observeOnce(f.observation(domain.ObservedByIngest, "firing",
		firstSeen, firstSeen, time.Time{}), opt)
	first := f.currentCase()

	// 2. A human takes it. The ack is staged through the case repository
	//    because WHO acked is not this test's subject; that the ack does not
	//    survive into the next episode is.
	label := "ada@example.com"
	require.NoError(t, f.cases.SetAck(ctx, f.scope, first.ID(), domain.AckChange{
		To:      domain.AckStateAcked,
		At:      firstSeen.Add(time.Minute),
		ByLabel: &label,
		Reason:  domain.UnackReasonManual,
	}, first.StateVersion()))

	// 3. Alertmanager resolves it (T5). The acknowledgement stays on the record:
	//    state and ack_state are orthogonal axes (§B.1).
	resolvedAt := firstSeen.Add(30 * time.Minute)
	resolved := f.observeOnce(f.observation(domain.ObservedByIngest, "resolved",
		resolvedAt, firstSeen, resolvedAt), opt)
	require.Len(t, resolved.outcomes, 1)
	assert.Equal(t, domain.TransitionT5.String(), resolved.outcomes[0].Transition)
	require.True(t, f.currentCase().AckState().IsAcked())

	// 4. It fires again, hours later. A closed episode is strictly terminal since
	//    ADR 0040, so there is no window to be inside or beyond: every re-fire is
	//    T7 and opens the next `seq`.
	acc := f.observeOnce(f.observation(domain.ObservedByIngest, "firing",
		now, now, time.Time{}), opt)

	require.Len(t, acc.outcomes, 1)
	out := acc.outcomes[0]
	assert.Equal(t, domain.TransitionT7.String(), out.Transition)
	assert.True(t, out.CaseOpened)
	// ⭐ THE OTHER HALF OF THE DIVERGENCE. This edge came out of `resolved`, and
	// the branch that reported "" for it was describing a first sighting that had
	// not happened.
	assert.Equal(t, domain.StateResolved.String(), out.From)
	assert.Equal(t, domain.StateFiring.String(), out.To)

	opened := f.currentCase()
	assert.Equal(t, opened.ID(), out.CaseID)
	assert.NotEqual(t, first.ID(), opened.ID(), "T7 opens a NEW episode")
	// ⭐ `seq` IS THE WHOLE LINK BACK. `reopen_of` used to repeat it as a column;
	// the episode this one succeeds is the row at `seq - 1`, which is `first`.
	assert.Equal(t, 2, opened.Seq())
	assert.Equal(t, first.Seq()+1, opened.Seq())
	assert.False(t, opened.AckState().IsAcked(), "a new episode always starts unacked (T10)")

	// The timeline records the dropped acknowledgement against the NEW episode.
	assert.Equal(t, []string{
		domain.EventCaseOpened.String(),
		domain.EventCaseUnacknowledged.String(),
	}, eventTypes(acc))
	for _, e := range acc.events {
		assert.Equal(t, opened.ID(), e.CaseID())
	}

	// ⭐ AND IT NOTIFIES — the half the two branches disagreed about. `unacked` is
	// a root UPDATE with no reply (§H.6), so it costs a card edit and never a
	// repost; without it the only witness that a human's acknowledgement stopped
	// applying is a timeline entry nobody is told about.
	assert.Equal(t, []string{reasonUnacked, reasonFired}, notifyReasons(acc))
	for _, n := range acc.notifies {
		assert.Equal(t, opened.ID(), n.caseID, "both intents are about the NEW episode")
	}
	assert.Equal(t, []uuid.UUID{opened.ID()}, acc.enrichIDs)
	assert.Equal(t, 1, acc.newEpisode[out.AlertID])

	// ⛔ THE PREVIOUS EPISODE IS UNTOUCHED. Clearing its ack would erase who took
	// a closed episode; `acked_by_label` is denormalised precisely so the timeline
	// reads the same in a year.
	prev, err := f.cases.GetByID(ctx, f.scope, first.ID())
	require.NoError(t, err)
	assert.True(t, prev.AckState().IsAcked())
	assert.Equal(t, domain.CaseClosed, prev.State())
	assert.Equal(t, domain.StateResolved, prev.AlertState())
}

// TestObserveOne_NoLegalRowRecordsTheOutcomeAndNothingElse pins the give-up path:
// a `resolved` for an Alert oto has never seen firing resolves nothing, and
// inventing an episode to close would be fabricating history.
func TestObserveOne_NoLegalRowRecordsTheOutcomeAndNothingElse(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)

	acc := f.observeOnce(f.observation(domain.ObservedByIngest, "resolved",
		now, now.Add(-time.Minute), now), ObserveOptions{})

	require.Len(t, acc.outcomes, 1)
	out := acc.outcomes[0]
	assert.Empty(t, out.Transition)
	assert.False(t, out.CaseOpened)
	assert.Equal(t, uuid.Nil, out.CaseID)
	// The identity was still recorded: the Alert row and its `alert.created`
	// entry are in the transaction either way.
	assert.True(t, out.AlertCreated)
	assert.Equal(t, []string{domain.EventAlertCreated.String()}, eventTypes(acc))
	assert.Empty(t, acc.notifies)
	assert.Empty(t, acc.enrichIDs)

	_, ok, err := f.svc.cases.GetLatestByAlert(t.Context(), f.scope, out.AlertID)
	require.NoError(t, err)
	assert.False(t, ok, "no episode may be fabricated to close")
}
