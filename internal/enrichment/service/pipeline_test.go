package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// The pipeline has exactly one hard commitment, and every test in this file is
// either that commitment or a consequence of it:
//
//	ENRICHMENT MUST NEVER DELAY OR FAIL A NOTIFICATION.
//
// An alert that fired is already real and already worth telling someone about;
// context is a bonus. So a failing enricher degrades to a RECORDED failure with
// an empty payload, never to a missing field and never to an error that reaches
// the caller.

// ------------------------------------------------------------- construction

func TestNewRequiresTheCollaboratorsItCannotWorkWithout(t *testing.T) {
	t.Parallel()

	reg, err := service.NewRegistry()
	require.NoError(t, err)

	tests := []struct {
		name string
		opts service.Options
		code string
	}{
		{
			name: "no registry",
			opts: service.Options{Repo: &fakeRepo{}, Subjects: &fakeSubjects{}},
			code: service.CodeMissingRegistry,
		},
		{
			name: "no repository",
			opts: service.Options{Registry: reg, Subjects: &fakeSubjects{}},
			code: service.CodeMissingRepository,
		},
		{
			name: "no subject loader",
			opts: service.Options{Registry: reg, Repo: &fakeRepo{}},
			code: service.CodeMissingSubjects,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc, err := service.New(tc.opts)
			require.Error(t, err)
			assert.Nil(t, svc)
			assert.Equal(t, tc.code, errs.CodeOf(err))
		})
	}
}

// TestNewTreatsTheOptionalPortsAsOptional pins what a minimal deployment gets:
// results are still computed, still stored and still provenanced when there is
// no cache, no notifier, no timeline and no queue.
func TestNewTreatsTheOptionalPortsAsOptional(t *testing.T) {
	t.Parallel()

	alpha := &stubEnricher{name: "test.alpha"}
	e := newEnv(t, func(o *service.Options) {
		o.Cache, o.Notifier, o.Events, o.Enqueuer = nil, nil, nil, nil
	}, alpha)

	out := e.run(t, domain.PhaseInline)

	require.Len(t, out.Results, 1)
	assert.Equal(t, domain.StatusOK, out.Results[0].Status)
	assert.False(t, out.Released, "there is nothing to release the card to")
	assert.Equal(t, 1, e.repo.writes(), "the provenanced record is written regardless")
}

func TestRunRequiresAnOccurrence(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.alpha"})

	_, err := e.svc.Run(context.Background(), e.scope, service.RunRequest{})
	require.Error(t, err)
	assert.Equal(t, service.CodeNoOccurrence, errs.CodeOf(err))
	assert.Equal(t, errs.KindValidation, errs.KindOf(err))
}

func TestRunDefaultsAnUnknownPhaseToInline(t *testing.T) {
	t.Parallel()

	inline := &stubEnricher{name: "test.inline", phase: domain.PhaseInline}
	e := newEnv(t, nil, inline)

	out, err := e.svc.Run(context.Background(), e.scope, service.RunRequest{
		OccurrenceID: e.occurrenceID,
		Phase:        domain.Phase(42),
	})
	require.NoError(t, err)
	assert.Equal(t, domain.PhaseInline, out.Phase,
		"an unreadable phase means the pass that blocks the notification")
	require.Len(t, out.Results, 1)
	assert.Equal(t, domain.PhaseInline, out.Results[0].Phase)
}

// ---------------------------------------------------- isolation between enrichers

// TestOneFailingEnricherDoesNotTakeTheOthersDown is the fan-out guarantee.
//
// Four enrichers, three of which misbehave in a different way, and the fourth
// must come back with its answer intact.
func TestOneFailingEnricherDoesNotTakeTheOthersDown(t *testing.T) {
	t.Parallel()

	boom := &stubEnricher{name: "test.boom", fn: failing("prometheus refused the connection")}
	crash := &stubEnricher{name: "test.crash", fn: panicking("nil map write")}
	slow := &stubEnricher{name: "test.slow", timeout: time.Nanosecond, fn: blocking()}
	good := &stubEnricher{name: "test.good"}

	e := newEnv(t, nil, boom, crash, slow, good)
	out := e.run(t, domain.PhaseInline)

	got := byName(out.Results)
	require.Len(t, got, 4, "every enricher is accounted for, however it ended")

	assert.Equal(t, domain.StatusFailed, got["test.boom"].Status)
	assert.Contains(t, got["test.boom"].Error, "prometheus refused")

	assert.Equal(t, domain.StatusFailed, got["test.crash"].Status)
	assert.Contains(t, got["test.crash"].Error, "panicked",
		"a panicking enricher is a recorded failure, not a dead process")

	assert.Equal(t, domain.StatusTimeout, got["test.slow"].Status)

	assert.Equal(t, domain.StatusOK, got["test.good"].Status,
		"the healthy enricher's answer survives its neighbours")
	assert.Equal(t, map[string]any{"who": "test.good"}, got["test.good"].Payload)
	assert.Empty(t, got["test.good"].Error)

	assert.Equal(t, 1, out.Succeeded(), "one of four produced usable content")
}

// TestEveryFailurePathDegradesToAnExplicitNotAvailable is the assertion that
// separates "the card lacks a field" from "the card states something wrong".
//
// A failed, timed-out or panicking enricher must leave an EMPTY payload and a
// stated reason. A half-filled payload would render as a fact.
func TestEveryFailurePathDegradesToAnExplicitNotAvailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stub   *stubEnricher
		status domain.Status
	}{
		{
			name:   "an upstream error",
			stub:   &stubEnricher{name: "test.alpha", fn: failing("connection refused")},
			status: domain.StatusFailed,
		},
		{
			name:   "a panic",
			stub:   &stubEnricher{name: "test.alpha", fn: panicking("index out of range")},
			status: domain.StatusFailed,
		},
		{
			name:   "a timeout",
			stub:   &stubEnricher{name: "test.alpha", timeout: time.Nanosecond, fn: blocking()},
			status: domain.StatusTimeout,
		},
		{
			// An enricher that returns an error AND a payload it wants kept
			// still may not present it as complete unless it says `partial`.
			name: "an error carrying a payload it did not label partial",
			stub: &stubEnricher{name: "test.alpha", fn: func(context.Context, *domain.Subject) (domain.Result, error) {
				return domain.Result{Status: domain.StatusOK, Payload: map[string]any{"expr": "up == 0"}},
					stubErr("the upstream hung up mid-read")
			}},
			status: domain.StatusFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e := newEnv(t, nil, tc.stub)
			out := e.run(t, domain.PhaseInline)

			require.Len(t, out.Results, 1)
			rec := out.Results[0]

			assert.Equal(t, tc.status, rec.Status)
			assert.False(t, rec.Status.Succeeded(), "a failure never counts as content")
			assert.Equal(t, map[string]any{}, rec.Payload,
				"a failed enrichment carries an EMPTY payload, never a partly-filled one")
			assert.NotEmpty(t, rec.Error,
				"enrichments_err_ck: a failure that cannot say why is a rumour")
			assert.False(t, rec.FromCache)
			assert.NoError(t, rec.Validate(), "and the row is still storable")
		})
	}
}

// TestAPartialAnswerAlongsideAnErrorIsKept is the one exception, and it is
// explicit: the enricher itself must label the result `partial`.
func TestAPartialAnswerAlongsideAnErrorIsKept(t *testing.T) {
	t.Parallel()

	half := &stubEnricher{name: "test.half", fn: func(context.Context, *domain.Subject) (domain.Result, error) {
		return domain.Result{
			Status:   domain.StatusPartial,
			Payload:  map[string]any{"count_24h": 3},
			Warnings: []string{"only_one_window_available"},
		}, stubErr("the 30-day window query timed out")
	}}

	e := newEnv(t, nil, half)
	out := e.run(t, domain.PhaseInline)

	require.Len(t, out.Results, 1)
	rec := out.Results[0]
	assert.Equal(t, domain.StatusPartial, rec.Status)
	assert.Equal(t, map[string]any{"count_24h": 3}, rec.Payload)
	assert.Empty(t, rec.Error, "a partial answer is not a failure, so it needs no error string")
	assert.Contains(t, rec.Warnings, "only_one_window_available")
	assert.Contains(t, strings.Join(rec.Warnings, "|"), "30-day window",
		"the reason is demoted to a warning rather than dropped")
	assert.NoError(t, rec.Validate())
}

// TestSkippedEnricherIsRecordedNotDropped: a missing enrichment and one that
// declined must be distinguishable in the UI.
func TestSkippedEnricherIsRecordedNotDropped(t *testing.T) {
	t.Parallel()

	declines := &stubEnricher{name: "test.declines", notApplicable: true}
	e := newEnv(t, nil, declines)

	out := e.run(t, domain.PhaseInline)

	require.Len(t, out.Results, 1)
	assert.Equal(t, domain.StatusSkipped, out.Results[0].Status)
	assert.Equal(t, map[string]any{}, out.Results[0].Payload)
	assert.Empty(t, out.Results[0].Error, "declining is not failing")
	assert.Zero(t, declines.callCount(), "Applicable said no, so Enrich was never called")
}

// -------------------------------------------------------- the load-bearing one

// TestEnrichmentFailureNeverBlocksTheNotification is the most important
// assertion in this package.
//
// Every enricher in the registry misbehaves — one errors, one panics, one runs
// out of budget — and the deferred first notification is STILL released, in the
// same pass, with the same coordinates. The difference this test defends is the
// difference between "the card lacks a field" and "the page never arrived".
func TestEnrichmentFailureNeverBlocksTheNotification(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil,
		&stubEnricher{name: "test.boom", fn: failing("prometheus is down")},
		&stubEnricher{name: "test.crash", fn: panicking("segfault in a parser")},
		&stubEnricher{name: "test.slow", timeout: time.Nanosecond, fn: blocking()},
	)

	out, err := e.svc.Run(context.Background(), e.scope, service.RunRequest{
		OccurrenceID: e.occurrenceID,
		Phase:        domain.PhaseInline,
	})

	require.NoError(t, err, "a phase in which EVERY enricher failed is not a failed run")
	assert.Zero(t, out.Succeeded(), "nothing was learned")
	assert.True(t, out.Released, "and the card goes out anyway")

	released := e.notifier.releaseCalls()
	require.Len(t, released, 1, "exactly one release, whatever the enrichers did")
	assert.Equal(t, e.groupID, released[0].GroupID)
	assert.Equal(t, e.alertID, released[0].AlertID)
	assert.Equal(t, e.occurrenceID, released[0].OccurrenceID)
	assert.Equal(t, 7, released[0].StateVersion,
		"pinned to the group state the evaluation was minted against (SPEC §C.7)")
}

// TestTheCardIsReleasedEvenWhenTheReleaseItselfFails: a failure to release is
// logged and swallowed, because `alerts` scheduled a backstop evaluation at the
// far end of the budget. The degradation is a card a second later, never no
// card.
func TestTheCardIsReleasedEvenWhenTheReleaseItselfFails(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.alpha"})
	e.notifier.releaseErr = stubErr("the queue is unreachable")

	out, err := e.svc.Run(context.Background(), e.scope, service.RunRequest{
		OccurrenceID: e.occurrenceID,
		Phase:        domain.PhaseInline,
	})

	require.NoError(t, err, "a failed release must not fail the enrichment run")
	assert.False(t, out.Released, "and the run says honestly that it did not release")
	assert.Len(t, e.notifier.releaseCalls(), 1, "it was attempted")
}

// TestAnUngroupedOccurrenceReleasesNothing: no open group means no card to
// amend and nothing to release.
func TestAnUngroupedOccurrenceReleasesNothing(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.alpha"})
	e.subjects.loaded.GroupID = uuid.Nil

	out := e.run(t, domain.PhaseInline)

	assert.False(t, out.Released)
	assert.Empty(t, e.notifier.releaseCalls())
}

// TestTheCardIsReleasedBeforeTheAsyncWorkIsAsked pins the ordering that makes
// the rule snapshot land on the message a human actually reads: results are
// stored, the card is released, and only then does anything slow happen.
func TestTheCardIsReleasedOnTheInlinePassOnly(t *testing.T) {
	t.Parallel()

	async := &stubEnricher{name: "test.async", phase: domain.PhaseAsync}
	e := newEnv(t, nil, async)

	out := e.run(t, domain.PhaseAsync)

	assert.False(t, out.Released, "the async pass amends; it does not release the first card")
	assert.Empty(t, e.notifier.releaseCalls())
	assert.True(t, out.Notified)
}

// ------------------------------------------------------------------ timeouts

// TestATimedOutInlineEnricherIsDeferredAsOneAsyncJob pins SPEC §F.3.
//
// ONE job carries all the stragglers. A job per straggler would produce a
// notification per straggler, which is exactly the noise the coalescing rule
// exists to stop.
func TestATimedOutInlineEnricherIsDeferredAsOneAsyncJob(t *testing.T) {
	t.Parallel()

	slowA := &stubEnricher{name: "test.slowa", timeout: time.Nanosecond, fn: blocking()}
	slowB := &stubEnricher{name: "test.slowb", timeout: time.Nanosecond, fn: blocking()}
	quick := &stubEnricher{name: "test.quick"}

	e := newEnv(t, nil, slowA, slowB, quick)
	out := e.run(t, domain.PhaseInline)

	assert.ElementsMatch(t, []string{"test.slowa", "test.slowb"}, out.Deferred)

	enqueued := e.enqueuer.enqueued()
	require.Len(t, enqueued, 1, "one job for all the stragglers, never one each")

	args, ok := enqueued[0].(jobs.EnrichRunArgs)
	require.True(t, ok, "the deferral is an enrich.run job")
	assert.Equal(t, e.occurrenceID, args.OccurrenceID)
	assert.Equal(t, domain.PhaseNameAsync, args.Phase)
	assert.ElementsMatch(t, []string{"test.slowa", "test.slowb"}, args.Enrichers,
		"and it names only the ones that ran out of budget")
}

// TestNothingIsDeferredWhenNothingTimedOut.
func TestNothingIsDeferredWhenNothingTimedOut(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.alpha"})
	out := e.run(t, domain.PhaseInline)

	assert.Empty(t, out.Deferred)
	assert.Empty(t, e.enqueuer.enqueued())
}

// TestAFailedDeferralStillLeavesAnHonestRecord: all that is lost is the retry.
func TestAFailedDeferralStillLeavesAnHonestRecord(t *testing.T) {
	t.Parallel()

	slow := &stubEnricher{name: "test.slow", timeout: time.Nanosecond, fn: blocking()}
	e := newEnv(t, nil, slow)
	e.enqueuer.err = stubErr("the queue refused the insert")

	out := e.run(t, domain.PhaseInline)

	assert.Equal(t, []string{"test.slow"}, out.Deferred)
	require.Len(t, out.Results, 1)
	assert.Equal(t, domain.StatusTimeout, out.Results[0].Status,
		"the UI is honest either way; only the retry is lost")
	assert.True(t, out.Released, "and the card still goes out")
}

// TestThePhaseBudgetIsACeilingSharedByEveryone: one enricher that never returns
// must not hold the phase past the budget for the rest.
func TestThePhaseBudgetIsACeilingNotAWait(t *testing.T) {
	t.Parallel()

	wedged := &stubEnricher{name: "test.wedged", timeout: time.Hour, fn: blocking()}
	e := newEnv(t, func(o *service.Options) { o.InlineBudget = time.Nanosecond }, wedged)

	out := e.run(t, domain.PhaseInline)

	require.Len(t, out.Results, 1)
	assert.Equal(t, domain.StatusTimeout, out.Results[0].Status)
	assert.True(t, out.Released, "the notification did not wait for it")
}

// TestTheAsyncBudgetIsTheGenerousOne pins that the two phases read different
// ceilings.
func TestTheAsyncBudgetIsTheGenerousOne(t *testing.T) {
	t.Parallel()

	async := &stubEnricher{name: "test.async", phase: domain.PhaseAsync, timeout: time.Hour, fn: blocking()}
	e := newEnv(t, func(o *service.Options) {
		o.InlineBudget = time.Hour
		o.AsyncBudget = time.Nanosecond
	}, async)

	out := e.run(t, domain.PhaseAsync)

	require.Len(t, out.Results, 1)
	assert.Equal(t, domain.StatusTimeout, out.Results[0].Status,
		"the async pass is bounded too: a wedged enricher must not hold a worker forever")
	assert.False(t, out.Notified, "a timeout produced no content to announce")
}

// -------------------------------------------------------------------- caching

// TestACacheHitIsServedAndSaysSo. FromCache is PROVENANCE, not an optimisation
// detail: a reader must be able to tell a fresh answer from a reused one.
func TestACacheHitIsServedAndSaysSo(t *testing.T) {
	t.Parallel()

	alpha := &stubEnricher{name: "test.alpha", seed: "an-alert-key"}
	e := newEnv(t, nil, alpha)

	key := domain.CacheKey(e.orgID.String(), "test.alpha", 1, "an-alert-key")
	require.NotEmpty(t, key)
	e.cache.entries[key] = domain.CacheEntry{
		Key:        key,
		OrgID:      e.orgID.String(),
		Payload:    []byte(`{"cached":true}`),
		ComputedAt: baseTime.Add(-time.Minute),
		ExpiresAt:  baseTime.Add(time.Minute),
	}

	out := e.run(t, domain.PhaseInline)

	require.Len(t, out.Results, 1)
	rec := out.Results[0]
	assert.Equal(t, domain.StatusOK, rec.Status)
	assert.True(t, rec.FromCache, "provenance: this answer was reused, not recomputed")
	assert.Equal(t, baseTime.Add(time.Minute), rec.ExpiresAt, "and it inherits the entry's expiry")
	assert.Zero(t, alpha.callCount(), "a hit means the upstream was never called")
}

// TestACacheMissComputesAndWritesBack, with the key derived by the DOMAIN and
// not by the enricher: the org and the version are in it whether the enricher
// remembered them or not.
func TestACacheMissComputesAndWritesBack(t *testing.T) {
	t.Parallel()

	alpha := &stubEnricher{
		name: "test.alpha", version: 3, seed: "an-alert-key",
		fn: func(context.Context, *domain.Subject) (domain.Result, error) {
			return domain.Result{
				Status:  domain.StatusOK,
				Payload: map[string]any{"fresh": true},
				TTL:     5 * time.Minute,
			}, nil
		},
	}
	e := newEnv(t, nil, alpha)

	out := e.run(t, domain.PhaseInline)

	require.Len(t, out.Results, 1)
	assert.False(t, out.Results[0].FromCache)
	assert.Equal(t, baseTime.Add(5*time.Minute), out.Results[0].ExpiresAt)
	assert.Equal(t, 1, alpha.callCount())

	keys := e.cache.keys()
	require.Len(t, keys, 1)
	assert.Contains(t, keys[0], e.orgID.String(),
		"the org is in the key: a cross-tenant hit would be a data leak, not a win")
	assert.Contains(t, keys[0], ":v3:",
		"the version is in the key: a bump must invalidate, or bumping does nothing")
}

// TestAnExpiredCacheEntryIsNeverServed. The entry is present and the repository
// hands it over; the pipeline still recomputes, because a stale answer
// presented as a fresh fact is the failure mode this whole module exists to
// avoid.
func TestAnExpiredCacheEntryIsNeverServed(t *testing.T) {
	t.Parallel()

	alpha := &stubEnricher{name: "test.alpha", seed: "an-alert-key"}
	e := newEnv(t, nil, alpha)

	key := domain.CacheKey(e.orgID.String(), "test.alpha", 1, "an-alert-key")
	e.cache.entries[key] = domain.CacheEntry{
		Key:        key,
		OrgID:      e.orgID.String(),
		Payload:    []byte(`{"stale":true}`),
		ComputedAt: baseTime.Add(-2 * time.Hour),
		ExpiresAt:  baseTime.Add(-time.Hour),
	}

	out := e.run(t, domain.PhaseInline)

	require.Len(t, out.Results, 1)
	assert.False(t, out.Results[0].FromCache, "an expired entry is a miss")
	assert.Equal(t, map[string]any{"who": "test.alpha"}, out.Results[0].Payload)
	assert.Equal(t, 1, alpha.callCount(), "so the upstream is consulted again")
}

// TestAnEntryThatExpiresWhileTheClockMovesStopsBeingServed drives the same
// boundary from the other side, through the injected clock.
func TestAnEntryStopsBeingServedTheInstantItExpires(t *testing.T) {
	t.Parallel()

	alpha := &stubEnricher{name: "test.alpha", seed: "an-alert-key"}
	e := newEnv(t, nil, alpha)

	key := domain.CacheKey(e.orgID.String(), "test.alpha", 1, "an-alert-key")
	e.cache.entries[key] = domain.CacheEntry{
		Key:        key,
		OrgID:      e.orgID.String(),
		Payload:    []byte(`{"cached":true}`),
		ComputedAt: baseTime,
		ExpiresAt:  baseTime.Add(time.Minute),
	}

	first := e.run(t, domain.PhaseInline)
	require.Len(t, first.Results, 1)
	require.True(t, first.Results[0].FromCache, "inside the TTL it is a hit")

	// Exactly at the expiry, not past it: Expired is `!ExpiresAt.After(now)`.
	e.clk.Advance(time.Minute)

	second := e.run(t, domain.PhaseInline)
	require.Len(t, second.Results, 1)
	assert.False(t, second.Results[0].FromCache, "at its expiry the entry is dead, not nearly dead")
	assert.Equal(t, 1, alpha.callCount())
}

// TestACacheThatIsDownIsASlowPipelineNeverABrokenOne.
func TestACacheThatIsDownIsASlowPipelineNeverABrokenOne(t *testing.T) {
	t.Parallel()

	alpha := &stubEnricher{
		name: "test.alpha", seed: "an-alert-key",
		fn: func(context.Context, *domain.Subject) (domain.Result, error) {
			return domain.Result{Status: domain.StatusOK, Payload: map[string]any{"ok": true}, TTL: time.Minute}, nil
		},
	}
	e := newEnv(t, nil, alpha)
	e.cache.getErr = stubErr("the pool is exhausted")
	e.cache.putErr = stubErr("the pool is exhausted")

	out := e.run(t, domain.PhaseInline)

	require.Len(t, out.Results, 1)
	assert.Equal(t, domain.StatusOK, out.Results[0].Status, "a read failure is a miss, not a failure")
	assert.Equal(t, 1, alpha.callCount())
	gets, puts := e.cache.counts()
	assert.Equal(t, 1, gets)
	assert.Equal(t, 1, puts, "the write was attempted, and its failure was swallowed")
	assert.True(t, out.Released)
}

// TestAnEnricherThatCannotNameItsInputsAlwaysComputes: only enrichers that
// implement CacheSeeder participate in the shared layer.
func TestAnEnricherThatCannotNameItsInputsAlwaysComputes(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, plainEnricher{name: "test.plain"})
	out := e.run(t, domain.PhaseInline)

	require.Len(t, out.Results, 1)
	assert.False(t, out.Results[0].FromCache)
	gets, puts := e.cache.counts()
	assert.Zero(t, gets, "no seed, no lookup")
	assert.Zero(t, puts)
}

// TestAnEmptySeedIsNotCacheable: a seeder that returns "" opts out per call,
// which is how an enricher declines to cache one particular subject.
func TestAnEmptySeedIsNotCacheable(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.alpha", seed: ""})
	e.run(t, domain.PhaseInline)

	gets, puts := e.cache.counts()
	assert.Zero(t, gets)
	assert.Zero(t, puts)
}

// TestAFailedResultIsNeverCached. Caching "we could not see it" would make the
// next fire inherit the failure for the whole TTL.
func TestAFailedResultIsNeverCached(t *testing.T) {
	t.Parallel()

	alpha := &stubEnricher{
		name: "test.alpha", seed: "an-alert-key",
		fn: func(context.Context, *domain.Subject) (domain.Result, error) {
			return domain.Result{TTL: time.Hour}, stubErr("prometheus is down")
		},
	}
	e := newEnv(t, nil, alpha)

	out := e.run(t, domain.PhaseInline)

	require.Len(t, out.Results, 1)
	assert.Equal(t, domain.StatusFailed, out.Results[0].Status)
	assert.Zero(t, out.Results[0].ExpiresAt, "a failure does not go stale, it is simply not reused")
	_, puts := e.cache.counts()
	assert.Zero(t, puts, "the next fire must try again rather than inherit the failure")
}

// TestARequestedTTLIsClampedIntoTheStorableRange.
func TestARequestedTTLIsClampedIntoTheStorableRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{name: "a one-second TTL is a cache that only costs", ttl: time.Second, want: domain.MinCacheTTL},
		{name: "a one-week TTL is a stale answer presented as a fact", ttl: 7 * 24 * time.Hour, want: domain.MaxCacheTTL},
		{name: "a sane TTL is honoured", ttl: 5 * time.Minute, want: 5 * time.Minute},
		{name: "no TTL means no caching", ttl: 0, want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			alpha := &stubEnricher{
				name: "test.alpha", seed: "seed",
				fn: func(context.Context, *domain.Subject) (domain.Result, error) {
					return domain.Result{Status: domain.StatusOK, Payload: map[string]any{}, TTL: tc.ttl}, nil
				},
			}
			e := newEnv(t, nil, alpha)
			out := e.run(t, domain.PhaseInline)

			require.Len(t, out.Results, 1)
			if tc.want == 0 {
				assert.Zero(t, out.Results[0].ExpiresAt)
				return
			}
			assert.Equal(t, baseTime.Add(tc.want), out.Results[0].ExpiresAt)
		})
	}
}

// ------------------------------------------------------------ re-run economy

// TestAStillFreshStoredResultIsSkippedRatherThanRecomputed. A retry of a phase
// is cheap by construction; that is what stops it paying for the same
// Prometheus call twice.
func TestAStillFreshStoredResultIsSkippedRatherThanRecomputed(t *testing.T) {
	t.Parallel()

	alpha := &stubEnricher{name: "test.alpha"}
	e := newEnv(t, nil, alpha)
	e.repo.existing = []domain.Enrichment{{
		OrgID:       e.orgID.String(),
		SubjectKind: domain.SubjectOccurrence,
		SubjectID:   e.occurrenceID.String(),
		Enricher:    "test.alpha",
		Version:     1,
		Phase:       domain.PhaseInline,
		Status:      domain.StatusOK,
		ComputedAt:  baseTime.Add(-time.Minute),
		ExpiresAt:   baseTime.Add(time.Hour),
	}}

	out := e.run(t, domain.PhaseInline)

	assert.Equal(t, []string{"test.alpha"}, out.Skipped)
	assert.Empty(t, out.Results, "a skipped enricher produces no new row")
	assert.Zero(t, alpha.callCount())
	assert.Zero(t, e.repo.writes(), "and nothing is written")
	assert.True(t, out.Released, "the card is still released")
}

func TestAStoredResultIsNotReusedWhenItShouldNotBe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing domain.Enrichment
	}{
		{
			name: "the enricher's version was bumped",
			existing: domain.Enrichment{
				Enricher: "test.alpha", Version: 1, Status: domain.StatusOK,
				ExpiresAt: baseTime.Add(time.Hour),
			},
		},
		{
			name: "the stored result is stale",
			existing: domain.Enrichment{
				Enricher: "test.alpha", Version: 2, Status: domain.StatusOK,
				ExpiresAt: baseTime.Add(-time.Second),
			},
		},
		{
			name: "the stored result was a failure",
			existing: domain.Enrichment{
				Enricher: "test.alpha", Version: 2, Status: domain.StatusFailed, Error: "boom",
				ExpiresAt: baseTime.Add(time.Hour),
			},
		},
		{
			name: "the stored result was a timeout",
			existing: domain.Enrichment{
				Enricher: "test.alpha", Version: 2, Status: domain.StatusTimeout, Error: "budget",
				ExpiresAt: baseTime.Add(time.Hour),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			alpha := &stubEnricher{name: "test.alpha", version: 2}
			e := newEnv(t, nil, alpha)
			e.repo.existing = []domain.Enrichment{tc.existing}

			out := e.run(t, domain.PhaseInline)

			assert.Empty(t, out.Skipped)
			require.Len(t, out.Results, 1)
			assert.Equal(t, 1, alpha.callCount(), "this one must be recomputed")
		})
	}
}

// TestPriorCarriesWhatIsAlreadyKnownIntoTheRun is how an async enricher sees
// what the inline pass found.
func TestPriorCarriesWhatIsAlreadyKnownIntoTheRun(t *testing.T) {
	t.Parallel()

	async := &stubEnricher{name: "test.async", phase: domain.PhaseAsync}
	e := newEnv(t, nil, async)
	e.repo.existing = []domain.Enrichment{{
		Enricher: "test.inline",
		Version:  1,
		Status:   domain.StatusOK,
		Payload:  map[string]any{"expr": "up == 0"},
		Warnings: []string{"ambiguous_rule_match"},
	}}

	e.run(t, domain.PhaseAsync)

	prior := async.prior()
	require.Contains(t, prior, "test.inline")
	assert.Equal(t, domain.StatusOK, prior["test.inline"].Status)
	assert.Equal(t, map[string]any{"expr": "up == 0"}, prior["test.inline"].Payload)
	assert.Equal(t, []string{"ambiguous_rule_match"}, prior["test.inline"].Warnings)
}

// ------------------------------------------------------------- the async pass

// TestTheAsyncPassAnnouncesExactlyOnceHoweverManyEnrichersFinished is the
// coalescing rule in code. Five slow enrichers produce one amended card, never
// five replies.
func TestTheAsyncPassAnnouncesExactlyOnceHoweverManyEnrichersFinished(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil,
		&stubEnricher{name: "test.one", phase: domain.PhaseAsync},
		&stubEnricher{name: "test.two", phase: domain.PhaseAsync},
		&stubEnricher{name: "test.three", phase: domain.PhaseAsync},
	)

	out := e.run(t, domain.PhaseAsync)

	assert.True(t, out.Notified)
	notices := e.notifier.enrichedCalls()
	require.Len(t, notices, 1, "one call per pass, whatever the number of enrichers")
	assert.Equal(t, []string{"test.one", "test.three", "test.two"}, notices[0].Enrichers,
		"named in deterministic order, so the context line does not churn")
	assert.Equal(t, e.groupID, notices[0].GroupID)
	assert.Equal(t, 7, notices[0].StateVersion)
}

// TestTheAsyncPassSaysNothingWhenItLearnedNothing. Amending a card to report
// that nothing was learned is worse than silence.
func TestTheAsyncPassSaysNothingWhenItLearnedNothing(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil,
		&stubEnricher{name: "test.boom", phase: domain.PhaseAsync, fn: failing("down")},
		&stubEnricher{name: "test.none", phase: domain.PhaseAsync, notApplicable: true},
	)

	out := e.run(t, domain.PhaseAsync)

	assert.False(t, out.Notified)
	assert.Empty(t, e.notifier.enrichedCalls())
	assert.Len(t, out.Results, 2, "the results are still recorded")
}

// TestAFailedAnnouncementDoesNotFailTheRun.
func TestAFailedAnnouncementDoesNotFailTheRun(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.async", phase: domain.PhaseAsync})
	e.notifier.enrichedErr = stubErr("the queue refused the insert")

	out, err := e.svc.Run(context.Background(), e.scope, service.RunRequest{
		OccurrenceID: e.occurrenceID,
		Phase:        domain.PhaseAsync,
	})

	require.NoError(t, err)
	assert.False(t, out.Notified)
	assert.Len(t, out.Results, 1, "the enrichment is stored whether or not anyone is told")
}

// ------------------------------------------------------------------- storage

func TestAnUnloadableSubjectFailsTheRun(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.alpha"})
	e.subjects.err = stubErr("the occurrence has been deleted")

	_, err := e.svc.Run(context.Background(), e.scope, service.RunRequest{
		OccurrenceID: e.occurrenceID,
		Phase:        domain.PhaseInline,
	})
	require.Error(t, err, "a run about a subject that cannot be loaded is meaningless")
}

func TestAnUnreadableRecordFailsTheRun(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.alpha"})
	e.repo.listErr = stubErr("the pool is exhausted")

	_, err := e.svc.Run(context.Background(), e.scope, service.RunRequest{
		OccurrenceID: e.occurrenceID,
		Phase:        domain.PhaseInline,
	})
	require.Error(t, err)
}

// TestAStorageFailureIsReturnedWithTheResultsAlreadyComputed documents the ONE
// place where the notification is not released by this module.
//
// It is not a hole in the "never blocks" rule: `alerts` scheduled its own
// evaluation at the far end of the pre-notification budget as a backstop, so the
// card still arrives. It is recorded here so a future reader knows the ordering
// is deliberate — results are stored BEFORE the card is released, because a card
// that quotes an enrichment nobody stored is worse than a card a second later.
func TestAStorageFailureIsReturnedWithTheResultsAlreadyComputed(t *testing.T) {
	t.Parallel()

	alpha := &stubEnricher{name: "test.alpha"}
	e := newEnv(t, nil, alpha)
	e.repo.upsertErr = stubErr("could not store the enrichment results")

	out, err := e.svc.Run(context.Background(), e.scope, service.RunRequest{
		OccurrenceID: e.occurrenceID,
		Phase:        domain.PhaseInline,
	})

	require.Error(t, err, "a storage failure is one of the two things that make a run meaningless")
	assert.Len(t, out.Results, 1, "the computed results are still handed back")
	assert.False(t, out.Released,
		"the release is not reached; `alerts`' scheduled backstop is what sends the card")
	assert.Equal(t, 1, alpha.callCount())
}

// ------------------------------------------------------------------ timeline

func TestTheTimelineSaysWhetherAnythingWasLearned(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		enrichers []domain.Enricher
		wantType  string
	}{
		{
			name:      "something was learned",
			enrichers: []domain.Enricher{&stubEnricher{name: "test.alpha"}},
			wantType:  service.EventCompleted,
		},
		{
			name: "nothing could be produced",
			enrichers: []domain.Enricher{
				&stubEnricher{name: "test.alpha", fn: failing("down")},
			},
			wantType: service.EventFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e := newEnv(t, nil, tc.enrichers...)
			e.run(t, domain.PhaseInline)

			recorded := e.events.recorded()
			require.Len(t, recorded, 1)
			assert.Equal(t, tc.wantType, recorded[0].Type)
			assert.Equal(t, e.alertID, recorded[0].AlertID)
			assert.Equal(t, e.occurrenceID, recorded[0].OccurrenceID)
			assert.NotEmpty(t, recorded[0].Summary)
			assert.LessOrEqual(t, len(recorded[0].Summary), 500)
			assert.Contains(t, recorded[0].DedupeKey, e.occurrenceID.String(),
				"SPEC §C.8: the append is idempotent per (occurrence, phase)")
			assert.Contains(t, recorded[0].Payload, "test.alpha",
				"per-enricher status and duration, so the timeline is readable")
		})
	}
}

// TestATimelineFailureNeverFailsAnEnrichment.
func TestATimelineFailureNeverFailsAnEnrichment(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.alpha"})
	e.events.err = stubErr("the partition is missing")

	out, err := e.svc.Run(context.Background(), e.scope, service.RunRequest{
		OccurrenceID: e.occurrenceID,
		Phase:        domain.PhaseInline,
	})
	require.NoError(t, err)
	assert.Len(t, out.Results, 1)
	assert.True(t, out.Released)
}

// TestNothingIsNarratedWhenNothingRan.
func TestNothingIsNarratedWhenNothingRan(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.async", phase: domain.PhaseAsync})
	e.run(t, domain.PhaseInline) // selects no enricher at all

	assert.Empty(t, e.events.recorded(), "a phase with no enrichers has nothing to say")
	assert.Zero(t, e.repo.writes())
}

// ------------------------------------------------------ what an enricher says

// TestAnEnricherThatMisreportsItselfIsCorrectedRatherThanTrusted.
func TestAnEnricherThatMisreportsItselfIsCorrectedRatherThanTrusted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		result  domain.Result
		wantSt  domain.Status
		wantErr bool
		wantPay any
	}{
		{
			name:    "an unknown status becomes ok",
			result:  domain.Result{Status: "brilliant", Payload: map[string]any{"a": 1}},
			wantSt:  domain.StatusOK,
			wantPay: map[string]any{"a": 1},
		},
		{
			name:    "a nil payload becomes an empty object",
			result:  domain.Result{Status: domain.StatusOK},
			wantSt:  domain.StatusOK,
			wantPay: map[string]any{},
		},
		{
			name:    "a self-declared failure with no reason is given one",
			result:  domain.Result{Status: domain.StatusFailed, Payload: map[string]any{}},
			wantSt:  domain.StatusFailed,
			wantErr: true,
			wantPay: map[string]any{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res := tc.result
			e := newEnv(t, nil, &stubEnricher{
				name: "test.alpha",
				fn: func(context.Context, *domain.Subject) (domain.Result, error) {
					return res, nil
				},
			})
			out := e.run(t, domain.PhaseInline)

			require.Len(t, out.Results, 1)
			rec := out.Results[0]
			assert.Equal(t, tc.wantSt, rec.Status)
			assert.Equal(t, tc.wantPay, rec.Payload)
			if tc.wantErr {
				assert.NotEmpty(t, rec.Error, "enrichments_err_ck is restated in Go")
			}
			assert.NoError(t, rec.Validate(), "whatever the enricher said, the row is storable")
		})
	}
}

// TestWarningsAreClampedBecauseTheColumnIsNotALog.
func TestWarningsAreClampedBecauseTheColumnIsNotALog(t *testing.T) {
	t.Parallel()

	noisy := make([]string, 0, domain.MaxWarnings*2)
	for i := 0; i < domain.MaxWarnings*2; i++ {
		noisy = append(noisy, "note")
	}
	noisy = append(noisy, "   ", "", strings.Repeat("x", 900))

	e := newEnv(t, nil, &stubEnricher{
		name: "test.alpha",
		fn: func(context.Context, *domain.Subject) (domain.Result, error) {
			return domain.Result{Status: domain.StatusOK, Payload: map[string]any{}, Warnings: noisy}, nil
		},
	})
	out := e.run(t, domain.PhaseInline)

	require.Len(t, out.Results, 1)
	assert.Len(t, out.Results[0].Warnings, domain.MaxWarnings,
		"an enricher emitting more than this is reporting noise")
	for _, w := range out.Results[0].Warnings {
		assert.NotEmpty(t, strings.TrimSpace(w), "blank warnings are dropped, not stored")
		assert.LessOrEqual(t, len(w), 500)
	}
}

// TestEachEnricherSeesItsOwnCopyOfTheSubject. Enrichers are documented as
// pure-ish, but "documented" is not "enforced", and a shared pointer across a
// fan-out is a data race waiting for a busy Tuesday.
func TestEachEnricherSeesItsOwnCopyOfTheSubject(t *testing.T) {
	t.Parallel()

	vandal := &stubEnricher{
		name: "test.vandal",
		fn: func(_ context.Context, s *domain.Subject) (domain.Result, error) {
			s.Alert.AlertName = "MUTATED"
			s.SubjectID = "MUTATED"
			return domain.Result{Status: domain.StatusOK, Payload: map[string]any{}}, nil
		},
	}
	witness := &stubEnricher{
		name: "test.witness",
		fn: func(_ context.Context, s *domain.Subject) (domain.Result, error) {
			return domain.Result{
				Status:  domain.StatusOK,
				Payload: map[string]any{"alertname": s.Alert.AlertName},
			}, nil
		},
	}

	e := newEnv(t, nil, vandal, witness)
	out := e.run(t, domain.PhaseInline)

	got := byName(out.Results)
	require.Contains(t, got, "test.witness")
	assert.Equal(t, map[string]any{"alertname": "HighErrorRate"}, got["test.witness"].Payload)
	assert.Equal(t, e.occurrenceID.String(), got["test.vandal"].SubjectID,
		"and the recorded row still names the real subject")
	assert.Equal(t, e.occurrenceID.String(), out.Subject.SubjectID)
}

// ----------------------------------------------------------------- ordering

func TestResultsAreOrderedByEnricherName(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil,
		&stubEnricher{name: "test.zulu"},
		&stubEnricher{name: "test.alpha"},
		&stubEnricher{name: "test.mike"},
	)

	// Asked for in a deliberately unhelpful order.
	out := e.run(t, domain.PhaseInline, "test.mike", "test.zulu", "test.alpha")

	names := make([]string, 0, len(out.Results))
	for _, r := range out.Results {
		names = append(names, r.Enricher)
	}
	assert.Equal(t, []string{"test.alpha", "test.mike", "test.zulu"}, names,
		"the guarantee is independent of how Select was asked")
}

// ------------------------------------------------------------- the record

// TestEveryRecordedResultSatisfiesTheDomainInvariants runs the full matrix of
// enricher behaviours through the pipeline and re-checks each stored row
// against Enrichment.Validate.
//
// That method is exactly what EnrichmentRepository.UpsertMany calls before it
// queues a batch, so a row the pipeline can mint but Validate rejects would be
// a run that fails at the write with a validation error — a 3am CHECK-shaped
// surprise rather than a recorded failure.
func TestEveryRecordedResultSatisfiesTheDomainInvariants(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil,
		&stubEnricher{name: "test.ok"},
		&stubEnricher{name: "test.boom", fn: failing("connection refused")},
		&stubEnricher{name: "test.crash", fn: panicking("nil deref")},
		&stubEnricher{name: "test.slow", timeout: time.Nanosecond, fn: blocking()},
		&stubEnricher{name: "test.skip", notApplicable: true},
		&stubEnricher{name: "test.cached", seed: "seed", fn: func(context.Context, *domain.Subject) (domain.Result, error) {
			return domain.Result{Status: domain.StatusOK, Payload: map[string]any{}, TTL: time.Hour}, nil
		}},
		&stubEnricher{name: "test.liar", fn: func(context.Context, *domain.Subject) (domain.Result, error) {
			return domain.Result{Status: "nonsense"}, nil
		}},
	)

	out := e.run(t, domain.PhaseInline)
	require.Len(t, out.Results, 7)

	for _, rec := range out.Results {
		t.Run(rec.Enricher, func(t *testing.T) {
			require.NoError(t, rec.Validate(),
				"the pipeline may not mint a row the repository would refuse")
			assert.True(t, rec.Status.Valid())
			assert.NotNil(t, rec.Payload, "enrichments_payload_ck: never a nil payload")
			assert.GreaterOrEqual(t, rec.Duration, time.Duration(0))
			assert.NotEmpty(t, rec.ID)
			assert.Equal(t, e.orgID.String(), rec.OrgID)
			assert.Equal(t, domain.SubjectOccurrence, rec.SubjectKind)
		})
	}

	// And the same rows are what reached storage.
	assert.Len(t, e.repo.stored(), 7)
}

// TestTheRunFillsInTheSubjectCoordinatesTheLoaderOmitted.
func TestTheRunFillsInTheSubjectCoordinatesTheLoaderOmitted(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.alpha"})
	e.subjects.loaded.Subject.SubjectKind = ""
	e.subjects.loaded.Subject.SubjectID = ""
	e.subjects.loaded.Subject.OrgID = ""

	out := e.run(t, domain.PhaseInline)

	assert.Equal(t, domain.SubjectOccurrence, out.Subject.SubjectKind,
		"an enrichment is a fact about a FIRE, not about an identity")
	assert.Equal(t, e.occurrenceID.String(), out.Subject.SubjectID)
	assert.Equal(t, e.orgID.String(), out.Subject.OrgID)
	require.Len(t, out.Results, 1)
	assert.NoError(t, out.Results[0].Validate())
}

// -------------------------------------------------------------- cache.expire

func TestExpireCacheIsTheMaintenanceSweep(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.alpha"})
	e.cache.deleted = 41

	n, err := e.svc.ExpireCache(context.Background(), 100)
	require.NoError(t, err)
	assert.Equal(t, int64(41), n)
	assert.Equal(t, baseTime, e.cache.lastBefore, "the sweep reads the injected clock")
	assert.Equal(t, 100, e.cache.lastLimit)
}

func TestExpireCacheBoundsAnUnboundedRequest(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.alpha"})

	_, err := e.svc.ExpireCache(context.Background(), 0)
	require.NoError(t, err)
	assert.Equal(t, 10000, e.cache.lastLimit,
		"an unbounded DELETE takes a long lock and blocks the pipeline that depends on it")
}

func TestExpireCacheWithoutACacheIsANoOp(t *testing.T) {
	t.Parallel()

	e := newEnv(t, func(o *service.Options) { o.Cache = nil }, &stubEnricher{name: "test.alpha"})

	n, err := e.svc.ExpireCache(context.Background(), 10)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestExpireCacheReportsAFailedSweep(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil, &stubEnricher{name: "test.alpha"})
	e.cache.deleteErr = stubErr("the table is locked")

	_, err := e.svc.ExpireCache(context.Background(), 10)
	require.Error(t, err, "unlike a read, a failed sweep is worth reporting to the job runner")
}

// --------------------------------------------------------------------- scope

// TestTheScopeTravelsToTheEnricher. domain.Enricher.Enrich has no scope
// parameter and must not gain one, so the pipeline puts the caller's scope into
// the context it hands down.
func TestTheScopeTravelsToTheEnricher(t *testing.T) {
	t.Parallel()

	var seen db.TenantScope
	var seenErr error

	e := newEnv(t, nil, &stubEnricher{
		name: "test.alpha",
		fn: func(ctx context.Context, _ *domain.Subject) (domain.Result, error) {
			seen, seenErr = service.ScopeFrom(ctx)
			return domain.Result{Status: domain.StatusOK, Payload: map[string]any{}}, nil
		},
	})
	e.run(t, domain.PhaseInline)

	require.NoError(t, seenErr, "an enricher that cannot find a scope must fail its own result")
	assert.Equal(t, e.orgID, seen.OrgID())
	assert.True(t, seen.Valid())
}
