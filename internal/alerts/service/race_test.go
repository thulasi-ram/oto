package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// TestReaperDoesNotExpireAnAlertAWebhookJustRefreshed is the regression test for
// the highest-value rule in the system: A RESOLUTION IS NEVER FABRICATED.
//
// The choreography is the one the sweep actually performs. `ReapCandidates` runs
// OUTSIDE any transaction, and `Reap` then makes two more round trips — source
// resolution and a health lookup — before `expire` writes anything. A webhook
// landing in that window pushes `source_ends_at` forward and proves the alert is
// still firing. Before the fix the reaper carried its stale snapshot all the way
// into `domain.Apply`, passed the §B.4 grace check against a `source_ends_at`
// that no longer existed, and stamped `expired`/`timeout` on a demonstrably live
// alert.
func TestReaperDoesNotExpireAnAlertAWebhookJustRefreshed(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	f := newFixture(t, now)
	ctx := t.Context()
	cfg := DefaultSettings()

	startsAt := now.Add(-2 * time.Hour)
	f.openFiring(startsAt, now.Add(-30*time.Minute))

	// 1. The sweep reads its candidates, outside any transaction.
	before := now.Add(-cfg.ResolveGrace)
	candidates, err := f.occurrences.ReapCandidates(ctx, f.scope, before, 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1, "the occurrence should be past source_ends_at + resolve_grace")
	stale := candidates[0]

	// 2. A webhook arrives while the sweep is resolving sources and health. T2
	//    folds it in and pushes source_ends_at into the future: the alert is
	//    demonstrably still firing.
	fresh := f.observation(domain.ObservedByIngest, "firing",
		now.Add(time.Second), startsAt, now.Add(5*time.Minute))
	_, err = f.svc.ObserveBatch(ctx, f.scope, []domain.Observation{fresh}, ObserveOptions{})
	require.NoError(t, err)

	refreshed := f.currentOccurrence()
	require.True(t, refreshed.SourceEndsAt().After(now),
		"the webhook should have moved source_ends_at into the future")

	// 3. The sweep now reaches `expire`, still holding the snapshot from step 1.
	expired, err := f.svc.expire(ctx, f.scope, stale, now, cfg)
	require.NoError(t, err)
	assert.False(t, expired, "the reaper must stand down, not expire a refreshed alert")

	// ⭐ THE ASSERTION THAT MATTERS.
	after := f.currentOccurrence()
	require.Equal(t, domain.StateFiring, after.State(),
		"a firing alert was expired from a stale read")
	require.True(t, after.EndedAt().IsZero(), "ended_at must not have been written")
	require.True(t, after.ResolveReason().IsZero(), "resolve_reason must not have been written")
	require.Zero(t, f.countEvents(domain.EventOccurrenceExpired.String()),
		"no occurrence.expired may be appended for an alert that is still firing")
}

// TestReaperDoesNotClobberAGenuineResolution stages the narrower window that the
// in-transaction re-read alone cannot close, and which only the compare-and-set
// catches.
//
// Ingest resolves the occurrence but has not committed. The reaper's re-read is a
// plain SELECT at READ COMMITTED, so it still sees `firing` and proceeds; its
// UPDATE then blocks on ingest's row lock. When ingest commits and the reaper
// wakes, the pre-fix predicate — `WHERE org_id AND id` — still matched, and
// `expired`/`timeout` went straight over an `occurrence.resolved` that Alertmanager
// had explicitly stated. The append-only timeline and the projection then
// disagreed permanently.
func TestReaperDoesNotClobberAGenuineResolution(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	f := newFixture(t, now)
	ctx := t.Context()
	cfg := DefaultSettings()

	startsAt := now.Add(-2 * time.Hour)
	f.openFiring(startsAt, now.Add(-30*time.Minute))

	candidates, err := f.occurrences.ReapCandidates(ctx, f.scope, now.Add(-cfg.ResolveGrace), 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	// Ingest resolves the episode and holds the transaction open.
	resolved := f.observation(domain.ObservedByIngest, "resolved",
		now, startsAt, now.Add(-time.Minute))
	staged, release, ingestDone := holdOpen(t, f.pool, func(ctx context.Context) error {
		_, err := f.svc.ObserveBatch(ctx, f.scope, []domain.Observation{resolved}, ObserveOptions{})
		return err
	})
	<-staged

	// The reaper runs against the uncommitted world: it reads `firing` and parks
	// on ingest's row lock.
	type reapResult struct {
		expired bool
		err     error
	}
	reaped := make(chan reapResult, 1)
	go func() {
		ok, err := f.svc.expire(context.Background(), f.scope, candidates[0], now, cfg)
		reaped <- reapResult{ok, err}
	}()

	waitForBlockedWriter(t, f.pool)
	close(release)

	require.NoError(t, <-ingestDone)
	got := <-reaped
	require.NoError(t, got.err)
	assert.False(t, got.expired, "the reaper must abandon a transition it lost")

	after := f.currentOccurrence()
	require.Equal(t, domain.StateResolved, after.State(),
		"an upstream resolution was overwritten with a fabricated expiry")
	require.Equal(t, domain.ResolveUpstream, after.ResolveReason())
	require.False(t, after.EndedAt().IsZero())
	require.Zero(t, f.countEvents(domain.EventOccurrenceExpired.String()))
	require.Equal(t, 1, f.countEvents(domain.EventOccurrenceResolved.String()))
}

// TestReconcilerT3DoesNotResurrectAResolvedEpisode is the §B.3 T3-versus-T5 race.
//
// Both witnesses read the occurrence as `firing`. Ingest's T5 commits `resolved`
// with an `ended_at`. The reconciler's T3, decided against the same pre-image,
// then lands — and before the fix it wrote `state='suppressed'` with a NULL
// `ended_at`, because `transitionOf` passes nil for a non-terminal state. A closed
// episode silently returned to `suppressed` with its end time erased, and
// occ_one_open_idx counted it open again.
func TestReconcilerT3DoesNotResurrectAResolvedEpisode(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	f := newFixture(t, now)
	ctx := t.Context()

	startsAt := now.Add(-time.Hour)
	f.openFiring(startsAt, now.Add(30*time.Minute))

	// The snapshot BOTH witnesses hold.
	pre := f.currentOccurrence()
	require.Equal(t, domain.StateFiring, pre.State())

	// Ingest wins the race and commits T5.
	res := f.observation(domain.ObservedByIngest, "resolved", now, startsAt, now)
	_, err := f.svc.ObserveBatch(ctx, f.scope, []domain.Observation{res}, ObserveOptions{})
	require.NoError(t, err)

	won := f.currentOccurrence()
	require.Equal(t, domain.StateResolved, won.State())
	endedAt := won.EndedAt()
	require.False(t, endedAt.IsZero())

	// The reconciler's T3, computed against `pre`, arrives late.
	r, err := domain.Apply(pre, suppressCommand(t, now.Add(time.Second)))
	require.NoError(t, err)
	require.Equal(t, domain.StateSuppressed, r.To)

	err = f.svc.occurrences.Transition(ctx, f.scope, r.Occurrence.ID(),
		transitionOf(r, domain.SuppressedBy{SilencedBy: []string{"sil-1"}}))
	assert.Error(t, err, "a transition decided against a superseded row must not succeed")
	assert.True(t, errs.IsKind(err, errs.KindConflict),
		"expected a conflict, got %v (%s)", err, errs.KindOf(err))

	after := f.currentOccurrence()
	require.Equal(t, domain.StateResolved, after.State(), "a resolved episode was resurrected")
	require.Equal(t, domain.ResolveUpstream, after.ResolveReason())
	require.WithinDuration(t, endedAt, after.EndedAt(), 0, "ended_at was erased")
	require.True(t, after.SuppressionReason().IsZero())
}

// TestTwoConcurrentReconcilerPassesAppendOneSuppressedEvent covers F9 together
// with the compare-and-set.
//
// Two reconcile passes over one occurrence both read it as `firing` and both
// decide T3 — the shape two HA replicas of one Alertmanager produce, since
// reconciliation is scoped by cluster and every replica reports the same alert
// set. They process a moment apart on oto's own clock.
//
// Before the fix that was two `occurrence.suppressed` events for one suppression:
// the §C.8 dedupe key was built from `lastObservedAt`, which is `cmd.At.RecordedAt()`
// — the instant oto happened to run — so the two passes minted two different keys,
// and nothing stopped the second unguarded UPDATE from landing either.
func TestTwoConcurrentReconcilerPassesAppendOneSuppressedEvent(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	f := newFixture(t, now)
	ctx := t.Context()

	startsAt := now.Add(-time.Hour)
	f.openFiring(startsAt, now.Add(30*time.Minute))
	pre := f.currentOccurrence()

	// Both passes decide against `pre`, at two different processing instants.
	pass := func(at time.Time) error {
		return db.Tx(ctx, f.pool, func(ctx context.Context) error {
			r, err := domain.Apply(pre, suppressCommand(t, at))
			if err != nil {
				return err
			}
			if err := f.svc.occurrences.Transition(ctx, f.scope, r.Occurrence.ID(),
				transitionOf(r, domain.SuppressedBy{SilencedBy: []string{"sil-1"}})); err != nil {
				return err
			}
			_, err = f.svc.appendEvents(ctx, f.scope, r.Events)
			return err
		})
	}

	require.NoError(t, pass(now.Add(time.Second)))

	err := pass(now.Add(4 * time.Second))
	assert.Error(t, err, "the second pass must not commit a decision from a superseded row")
	assert.True(t, errs.IsKind(err, errs.KindConflict),
		"expected a conflict, got %v (%s)", err, errs.KindOf(err))

	require.Equal(t, 1, f.countEvents(domain.EventOccurrenceSuppressed.String()),
		"one suppression must append exactly one occurrence.suppressed event (§C.8)")

	after := f.currentOccurrence()
	require.Equal(t, domain.StateSuppressed, after.State())
	require.Equal(t, domain.SuppressionSilence, after.SuppressionReason())
}

// TestSuppressionDedupeKeyIsACounterNotAClock isolates F9 from the
// compare-and-set: it asks the domain machine alone whether two passes over the
// same pre-image agree on the §C.8 key.
//
// They must, and a genuinely SECOND suppression of the same episode must still
// differ, or the fix would trade a duplicated timeline entry for a lost one.
func TestSuppressionDedupeKeyIsACounterNotAClock(t *testing.T) {
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	occ := firingOccurrence(t, base)

	first, err := domain.Apply(occ, suppressCommandAt(t, base.Add(time.Second), base))
	require.NoError(t, err)
	require.Len(t, first.Events, 1)
	require.Equal(t, 1, first.Occurrence.SuppressCount())

	// The same pre-image, processed three seconds later by another pass.
	second, err := domain.Apply(occ, suppressCommandAt(t, base.Add(4*time.Second), base))
	require.NoError(t, err)
	require.Len(t, second.Events, 1)

	require.Equal(t, first.Events[0].DedupeKey(), second.Events[0].DedupeKey(),
		"two passes over one pre-image must mint one §C.8 key")
	require.NotContains(t, first.Events[0].DedupeKey(), base.Format("2006"),
		"the key must not embed a timestamp at all")

	// A genuinely SECOND suppression of this episode is a different fact.
	unsuppressed, err := domain.Apply(first.Occurrence, unsuppressCommand(t, base.Add(time.Minute)))
	require.NoError(t, err)
	require.Equal(t, 1, unsuppressed.Occurrence.SuppressCount(),
		"T4 ends a suppression, it does not start one")

	again, err := domain.Apply(unsuppressed.Occurrence, suppressCommandAt(t, base.Add(2*time.Minute), base))
	require.NoError(t, err)
	require.Len(t, again.Events, 1)
	require.Equal(t, 2, again.Occurrence.SuppressCount())
	require.NotEqual(t, first.Events[0].DedupeKey(), again.Events[0].DedupeKey(),
		"a second suppression is a different fact and must not be collapsed")
}

// TestRepeatedSuppressionInOneEpisodeIsCounted is the same claim end to end: T3,
// T4, T3 inside ONE occurrence leaves suppress_count = 2 and three distinct
// timeline facts.
//
// This is what the interim `sourceUpdatedAt` key could not promise and what a
// `lastObservedAt` key promised falsely.
func TestRepeatedSuppressionInOneEpisodeIsCounted(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	f := newFixture(t, now)
	ctx := t.Context()

	startsAt := now.Add(-time.Hour)
	f.openFiring(startsAt, now.Add(30*time.Minute))

	step := func(cmd domain.TransitionCommand) {
		t.Helper()
		pre := f.currentOccurrence()
		require.NoError(t, db.Tx(ctx, f.pool, func(ctx context.Context) error {
			r, err := domain.Apply(pre, cmd)
			if err != nil {
				return err
			}
			if err := f.svc.occurrences.Transition(ctx, f.scope, r.Occurrence.ID(),
				transitionOf(r, domain.SuppressedBy{SilencedBy: []string{"sil-1"}})); err != nil {
				return err
			}
			_, err = f.svc.appendEvents(ctx, f.scope, r.Events)
			return err
		}))
	}

	step(suppressCommand(t, now.Add(time.Minute)))
	require.Equal(t, 1, f.currentOccurrence().SuppressCount())

	step(unsuppressCommand(t, now.Add(2*time.Minute)))
	require.Equal(t, 1, f.currentOccurrence().SuppressCount())

	step(suppressCommand(t, now.Add(3*time.Minute)))

	after := f.currentOccurrence()
	require.Equal(t, 2, after.SuppressCount(),
		"two suppressions of one episode must be counted, not collapsed")
	require.Equal(t, domain.StateSuppressed, after.State())
	require.Equal(t, 2, f.countEvents(domain.EventOccurrenceSuppressed.String()))
	require.Equal(t, 1, f.countEvents(domain.EventOccurrenceUnsuppressed.String()))
}

// TestOutOfOrderWebhookCannotRewindSourceEndsAt is the T2 rewind regression.
//
// `source_ends_at` is the entire input to the §B.4 grace check, and T2 was the
// one edge that wrote it with no guard at all. Alertmanager delivers
// at-least-once from an HA pair with no ordering guarantee between replicas, so
// the second replica's copy of an older payload arriving after the first
// replica's newer one is ordinary traffic. Assigning it verbatim rewound
// `source_ends_at` into the past, the occurrence became a reap candidate on the
// next tick, and a firing alert was expired — the same fabricated resolution,
// reached without ever racing anybody.
func TestOutOfOrderWebhookCannotRewindSourceEndsAt(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	f := newFixture(t, now)
	ctx := t.Context()
	cfg := DefaultSettings()

	startsAt := now.Add(-2 * time.Hour)
	f.openFiring(startsAt, now.Add(-30*time.Minute))

	// Replica A's payload: the alert is valid for another half hour.
	ahead := f.observation(domain.ObservedByIngest, "firing",
		now, startsAt, now.Add(30*time.Minute))
	_, err := f.svc.ObserveBatch(ctx, f.scope, []domain.Observation{ahead}, ObserveOptions{})
	require.NoError(t, err)
	require.True(t, f.currentOccurrence().SourceEndsAt().After(now))

	// Replica B's copy of an OLDER payload, delivered late. It arrives on oto's
	// clock AFTER the newer one, which is exactly why plain assignment loses.
	behind := f.observation(domain.ObservedByIngest, "firing",
		now, startsAt, now.Add(-30*time.Minute))
	_, err = f.svc.ObserveBatch(ctx, f.scope, []domain.Observation{behind}, ObserveOptions{})
	require.NoError(t, err)

	rewound := f.currentOccurrence()
	assert.True(t, rewound.SourceEndsAt().After(now),
		"a stale delivery rewound source_ends_at into the past")

	// Now let the reaper run exactly as the sweep would.
	candidates, err := f.occurrences.ReapCandidates(ctx, f.scope, now.Add(-cfg.ResolveGrace), 10)
	require.NoError(t, err)
	for _, c := range candidates {
		_, err := f.svc.expire(ctx, f.scope, c, now, cfg)
		require.NoError(t, err)
	}

	after := f.currentOccurrence()
	require.Equal(t, domain.StateFiring, after.State(),
		"a firing alert was expired because a late webhook rewound source_ends_at")
	require.True(t, after.EndedAt().IsZero())
	require.Zero(t, f.countEvents(domain.EventOccurrenceExpired.String()))
}

// TestAcknowledgeCannotLandOnAnEpisodeThatEnded is the SetAck ruling.
//
// The domain refuses to acknowledge a terminal occurrence, but it can only refuse
// against the snapshot it read. A resolve committing between that read and the
// ack write used to stamp `acked` on a closed episode AND rewind the alert
// projection: the ack path writes `alert.State()` and `current_occurrence_id`
// from the alert IT read, so the list went back to showing `firing` for an alert
// Alertmanager had explicitly resolved.
func TestAcknowledgeCannotLandOnAnEpisodeThatEnded(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	f := newFixture(t, now)
	ctx := t.Context()

	startsAt := now.Add(-time.Hour)
	f.openFiring(startsAt, now.Add(30*time.Minute))

	// What the human's request read.
	pre := f.currentOccurrence()
	require.Equal(t, domain.StateFiring, pre.State())

	// Upstream resolves while the human is deciding.
	res := f.observation(domain.ObservedByIngest, "resolved", now, startsAt, now)
	_, err := f.svc.ObserveBatch(ctx, f.scope, []domain.Observation{res}, ObserveOptions{})
	require.NoError(t, err)

	label := "ram"
	err = f.svc.occurrences.SetAck(ctx, f.scope, pre.ID(), domain.AckChange{
		To:      domain.AckStateAcked,
		At:      now.Add(time.Second),
		ByLabel: &label,
		Reason:  domain.UnackReasonManual,
	}, pre.StateVersion())
	require.Error(t, err, "an ack decided against a live episode must not land on a closed one")
	require.True(t, errs.IsKind(err, errs.KindConflict),
		"expected a conflict, got %v (%s)", err, errs.KindOf(err))

	after := f.currentOccurrence()
	require.Equal(t, domain.StateResolved, after.State())
	require.Equal(t, domain.AckStateUnacked, after.AckState(),
		"a resolved episode was acknowledged")
}

// ------------------------------------------------------------------- helpers

// suppressCommand is a reconciler T3 whose UPSTREAM facts are fixed and whose
// only moving part is oto's processing clock.
func suppressCommand(t *testing.T, recordedAt time.Time) domain.TransitionCommand {
	t.Helper()
	return suppressCommandAt(t, recordedAt, time.Date(2026, 8, 8, 11, 59, 0, 0, time.UTC))
}

func suppressCommandAt(t *testing.T, recordedAt, upstreamAt time.Time) domain.TransitionCommand {
	t.Helper()
	actor, err := domain.SystemActor(domain.ActorReconciler)
	require.NoError(t, err)
	at, err := domain.NewObservationTime(upstreamAt, recordedAt)
	require.NoError(t, err)
	return domain.TransitionCommand{
		Trigger:           domain.TriggerObserveSuppressed,
		Actor:             actor,
		At:                at,
		EventID:           id.New(),
		SuppressionReason: domain.SuppressionSilence,
		SourceUpdatedAt:   upstreamAt,
		Payload:           map[string]any{"silenced_by": []string{"sil-1"}},
	}
}

// unsuppressCommand is the T4 the reconciler drives when a silence lapses.
func unsuppressCommand(t *testing.T, recordedAt time.Time) domain.TransitionCommand {
	t.Helper()
	actor, err := domain.SystemActor(domain.ActorReconciler)
	require.NoError(t, err)
	at, err := domain.NewObservationTime(recordedAt, recordedAt)
	require.NoError(t, err)
	return domain.TransitionCommand{
		Trigger: domain.TriggerObserveFiring,
		Actor:   actor,
		At:      at,
		EventID: id.New(),
	}
}

// firingOccurrence builds an in-memory open episode, for the domain-only test.
func firingOccurrence(t *testing.T, now time.Time) domain.Occurrence {
	t.Helper()
	o, err := domain.NewOccurrence(domain.OccurrenceParams{
		ID:             id.New(),
		OrgID:          id.New(),
		AlertID:        id.New(),
		Seq:            1,
		State:          domain.StateFiring,
		StartedAt:      now.Add(-time.Hour),
		LastObservedAt: now.Add(-time.Minute),
		SourceStartsAt: now.Add(-time.Hour),
		SourceEndsAt:   now.Add(time.Hour),
		AckState:       domain.AckStateUnacked,
	})
	require.NoError(t, err)
	return o
}
