package alerthistory_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/alerthistory"
	"github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// alert.history answers the question a responder asks before any other: "is this
// new, or has it been doing this all week?" — and it answers it about the
// SIGNAL, never about the people responding to it. There is no MTTR here and
// there never will be (SPEC §A.1, R8).

var baseTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// store is the narrow read port, with the three answers a database can give:
// a row, no row, and a refusal.
type store struct {
	stats alerthistory.Stats
	err   error

	calls   int
	sawNow  time.Time
	sawID   uuid.UUID
	honours bool // return ctx.Err() when the budget is gone
}

func (s *store) AlertHistory(
	ctx context.Context, _ db.TenantScope, alertID uuid.UUID, now time.Time,
) (alerthistory.Stats, error) {
	s.calls++
	s.sawNow, s.sawID = now, alertID
	if s.honours {
		if err := ctx.Err(); err != nil {
			return alerthistory.Stats{}, err
		}
	}
	if s.err != nil {
		return alerthistory.Stats{}, s.err
	}
	return s.stats, nil
}

var alertID = id.New()

func subject() *domain.Subject {
	return &domain.Subject{
		OrgID:       id.NewString(),
		SubjectKind: domain.SubjectOccurrence,
		SubjectID:   id.NewString(),
		Alert: domain.AlertSnapshot{
			ID:        alertID.String(),
			AlertName: "HighErrorRate",
			Severity:  "critical",
		},
		Occurrence: domain.OccurrenceSnapshot{ID: id.NewString(), StartedAt: baseTime},
	}
}

// scoped is the context an enricher is really called with: the pipeline puts the
// caller's TenantScope into it, because Enrich has no scope parameter.
func scoped(t *testing.T) context.Context {
	t.Helper()
	s, err := db.NewTenantScope(id.New())
	require.NoError(t, err)
	return service.WithScope(context.Background(), s)
}

// expired is "the budget is already gone", expressed without waiting for it.
func expired(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(scoped(t), baseTime.Add(-time.Hour))
	t.Cleanup(cancel)
	return ctx
}

func payloadOf(t *testing.T, res domain.Result) alerthistory.Payload {
	t.Helper()
	p, ok := res.Payload.(alerthistory.Payload)
	require.True(t, ok, "the payload is the enricher's own typed struct")
	return p
}

// ------------------------------------------------------------------ the ports

func TestTheRegistryContractIsStable(t *testing.T) {
	t.Parallel()

	e := alerthistory.New(&store{}, clock.NewFake(baseTime))

	assert.Equal(t, "alert.history", e.Name())
	assert.True(t, domain.ValidEnricherName(e.Name()))
	assert.Equal(t, 1, e.Version())
	assert.Equal(t, domain.PhaseInline, e.Phase(),
		"\"this has fired 40 times today\" is the context most likely to change what the reader does next")
	assert.Equal(t, 200*time.Millisecond, e.Timeout(), "one indexed query")
	assert.Equal(t, 60*time.Second, alerthistory.CacheTTL,
		"a moving number needs a short TTL or it answers about the past while claiming to answer about now")
}

func TestApplicableRequiresAnAlertIdentityToCountAgainst(t *testing.T) {
	t.Parallel()

	e := alerthistory.New(&store{}, clock.NewFake(baseTime))
	assert.True(t, e.Applicable(subject()))
	assert.False(t, e.Applicable(nil))

	noID := subject()
	noID.Alert.ID = ""
	assert.False(t, e.Applicable(noID))

	assert.False(t, alerthistory.New(nil, clock.NewFake(baseTime)).Applicable(subject()),
		"an enricher with no store cannot answer and must not be dispatched")
}

// TestCacheSeedIsTheAlertIdentity: the result is shared across every occurrence
// of the same alert within the TTL, which is exactly what a storm produces.
func TestCacheSeedIsTheAlertIdentity(t *testing.T) {
	t.Parallel()

	e := alerthistory.New(&store{}, clock.NewFake(baseTime))

	assert.Equal(t, alertID.String(), e.CacheSeed(subject()))
	assert.Empty(t, e.CacheSeed(nil))

	// The seed carries no occurrence, so two fires of the same alert hit.
	first, second := subject(), subject()
	second.Occurrence.ID = id.NewString()
	assert.Equal(t, e.CacheSeed(first), e.CacheSeed(second))
}

// --------------------------------------------------------------------- found

func TestAFullHistoryIsSummarised(t *testing.T) {
	t.Parallel()

	st := &store{stats: alerthistory.Stats{
		Count24h:               4,
		Count7d:                11,
		Count30d:               19,
		TotalOccurrences:       57,
		FlapScore:              0.42,
		IsFlapping:             true,
		FirstSeenAt:            baseTime.Add(-30 * 24 * time.Hour),
		LastSeenAt:             baseTime.Add(-time.Hour),
		FiringDurationsSeconds: []float64{300, 120, 60, 900},
	}}
	clk := clock.NewFake(baseTime)

	res, err := alerthistory.New(st, clk).Enrich(scoped(t), subject())
	require.NoError(t, err)

	assert.Equal(t, domain.StatusOK, res.Status)
	assert.Equal(t, alerthistory.CacheTTL, res.TTL)
	assert.Empty(t, res.Warnings)

	p := payloadOf(t, res)
	assert.Equal(t, 4, p.Count24h)
	assert.Equal(t, 11, p.Count7d)
	assert.Equal(t, 19, p.Count30d)
	assert.Equal(t, 57, p.TotalOccurrences)
	assert.InDelta(t, 0.42, p.FlapScore, 1e-9)
	assert.True(t, p.IsFlapping)
	assert.False(t, p.Noisy, "19 in thirty days is under the threshold")

	// "How long did it last last time?" is the number an operator actually asks
	// for, and the store returns newest first.
	assert.InDelta(t, 300, p.LastFiringDurationS, 1e-9)

	d := p.FiringDuration
	assert.Equal(t, 4, d.Samples)
	assert.InDelta(t, 60, d.MinS, 1e-9)
	assert.InDelta(t, 900, d.MaxS, 1e-9)
	assert.InDelta(t, 345, d.MeanS, 1e-9)

	// Nearest rank, not interpolation: p50 of a real set of durations is a
	// duration that really happened. Sorted, the sample is [60 120 300 900], so
	// the nearest rank at 0.50 is index ceil(0.5*4)-1 = 1.
	assert.InDelta(t, 120, d.P50S, 1e-9)
	assert.InDelta(t, 900, d.P90S, 1e-9)
	assert.Contains(t, []float64{60, 120, 300, 900}, d.P50S,
		"an interpolated percentile invents a value that did not occur")

	// The clock is the injected one, not the wall clock.
	assert.Equal(t, baseTime, st.sawNow)
	assert.Equal(t, alertID, st.sawID)
}

func TestTheTimestampsAreNormalisedToUTC(t *testing.T) {
	t.Parallel()

	zone := time.FixedZone("IST", 5*3600+1800)
	st := &store{stats: alerthistory.Stats{
		FirstSeenAt:            baseTime.In(zone),
		LastSeenAt:             baseTime.In(zone),
		FiringDurationsSeconds: []float64{10},
	}}

	res, err := alerthistory.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
	require.NoError(t, err)

	p := payloadOf(t, res)
	assert.Equal(t, time.UTC, p.FirstSeenAt.Location())
	assert.Equal(t, time.UTC, p.LastSeenAt.Location())
	assert.True(t, p.FirstSeenAt.Equal(baseTime))
}

func TestANoisyAlertIsFlaggedAsARenderingHint(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		count int
		noisy bool
	}{
		{count: alerthistory.NoisyThreshold30d - 1, noisy: false},
		{count: alerthistory.NoisyThreshold30d, noisy: true},
		{count: alerthistory.NoisyThreshold30d + 1, noisy: true},
	} {
		st := &store{stats: alerthistory.Stats{
			Count30d:               tc.count,
			FiringDurationsSeconds: []float64{60},
		}}
		res, err := alerthistory.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
		require.NoError(t, err)
		assert.Equal(t, tc.noisy, payloadOf(t, res).Noisy,
			"more than one fire a day for a month is a rule worth revisiting")
	}
}

func TestATruncatedSampleSaysSo(t *testing.T) {
	t.Parallel()

	durations := make([]float64, alerthistory.SampleLimit)
	for i := range durations {
		durations[i] = float64(i + 1)
	}
	st := &store{stats: alerthistory.Stats{FiringDurationsSeconds: durations}}

	res, err := alerthistory.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
	require.NoError(t, err)

	assert.Equal(t, domain.StatusOK, res.Status)
	assert.Contains(t, res.Warnings, "duration_sample_truncated",
		"a bounded query is honest about being bounded")
}

func TestNonsenseDurationsAreDroppedRatherThanAveraged(t *testing.T) {
	t.Parallel()

	st := &store{stats: alerthistory.Stats{
		FiringDurationsSeconds: []float64{100, -5, math.NaN(), math.Inf(1), 200},
	}}

	res, err := alerthistory.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
	require.NoError(t, err)

	d := payloadOf(t, res).FiringDuration
	assert.Equal(t, 2, d.Samples, "a negative duration and a NaN are not measurements")
	assert.InDelta(t, 150, d.MeanS, 1e-9)
	assert.False(t, math.IsNaN(d.MeanS), "one NaN must not poison the whole summary")
}

func TestSubSecondPrecisionIsTrimmed(t *testing.T) {
	t.Parallel()

	st := &store{stats: alerthistory.Stats{FiringDurationsSeconds: []float64{61.2345, 61.2345}}}

	res, err := alerthistory.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
	require.NoError(t, err)

	d := payloadOf(t, res).FiringDuration
	assert.InDelta(t, 61.2, d.P50S, 1e-9,
		"sub-100ms precision on a duration measured in minutes is noise pretending to be accuracy")
}

// ----------------------------------------------------------------- not found

// TestAFirstFireIsPartialRatherThanARowOfZeroes. Saying "there is no
// distribution yet" is better than rendering zeroes that read like a
// measurement.
func TestAFirstFireIsPartialRatherThanARowOfZeroes(t *testing.T) {
	t.Parallel()

	st := &store{stats: alerthistory.Stats{
		Count24h:         1,
		TotalOccurrences: 1,
		FirstSeenAt:      baseTime,
		LastSeenAt:       baseTime,
	}}

	res, err := alerthistory.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
	require.NoError(t, err)

	assert.Equal(t, domain.StatusPartial, res.Status)
	assert.Equal(t, []string{"no_closed_episodes_yet"}, res.Warnings)

	p := payloadOf(t, res)
	assert.Zero(t, p.FiringDuration.Samples,
		"Samples == 0 is the flag that says the zeroes below are not measurements")
	assert.Zero(t, p.FiringDuration.P50S)
	assert.Zero(t, p.LastFiringDurationS)
	assert.Equal(t, 1, p.Count24h, "the counts it DOES know are still reported")
}

// TestAnAlertWithNoIdentityIsSkippedNotFailed.
func TestAnAlertWithNoIdentityIsSkippedNotFailed(t *testing.T) {
	t.Parallel()

	st := &store{}
	s := subject()
	s.Alert.ID = "not-a-uuid"

	res, err := alerthistory.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), s)
	require.NoError(t, err, "an unusable subject is not an error, it is a declined enrichment")

	assert.Equal(t, domain.StatusSkipped, res.Status)
	assert.Equal(t, []string{"no_alert_id"}, res.Warnings)
	assert.Nil(t, res.Payload, "and it invents no payload")
	assert.Zero(t, st.calls, "the store is never asked about an alert that does not exist")
}

// ------------------------------------------------------------ upstream error

// TestAnUpstreamRefusalIsAnErrorAndNothingElse.
//
// The enricher's contract with the pipeline is that an error means NO RESULT.
// The pipeline converts it into a recorded `failed` row with an empty payload;
// what must never happen here is a half-filled Payload travelling alongside an
// error, because that is what turns a database hiccup into a wrong number on a
// Slack card.
func TestAnUpstreamRefusalIsAnErrorAndNothingElse(t *testing.T) {
	t.Parallel()

	st := &store{
		err: errs.New(errs.KindInternal, "alerts_query_failed", "the pool is exhausted"),
		// Non-zero stats, so a leak would be visible.
		stats: alerthistory.Stats{Count24h: 99, FiringDurationsSeconds: []float64{1, 2, 3}},
	}

	res, err := alerthistory.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())

	require.Error(t, err)
	assert.Equal(t, "alerts_query_failed", errs.CodeOf(err))
	assert.Equal(t, domain.Result{}, res,
		"an error carries the ZERO result: no status, no payload, nothing to render")
	assert.Nil(t, res.Payload)
	assert.Empty(t, res.Status)
}

// TestAnEnricherWithoutAScopeFailsRatherThanQueryingUnscoped: an unscoped query
// in a multi-tenant table is a cross-tenant read.
func TestAnEnricherWithoutAScopeFailsRatherThanQueryingUnscoped(t *testing.T) {
	t.Parallel()

	st := &store{stats: alerthistory.Stats{Count24h: 3}}

	res, err := alerthistory.New(st, clock.NewFake(baseTime)).Enrich(context.Background(), subject())

	require.Error(t, err)
	assert.Equal(t, "enrichment_no_tenant_scope", errs.CodeOf(err))
	assert.Equal(t, domain.Result{}, res)
	assert.Zero(t, st.calls, "and it never reaches the database")
}

// ------------------------------------------------------------------ timeout

// TestAWithdrawnBudgetSurfacesAsADeadlineAndNoResult.
//
// The store is handed the context, so the deadline the pipeline set is the
// deadline the query sees. What the pipeline needs back is an error it can
// recognise as `context.DeadlineExceeded` — that is what separates a `timeout`
// row (re-enqueued to the async phase) from a `failed` one (left to the job's
// own retry policy).
func TestAWithdrawnBudgetSurfacesAsADeadlineAndNoResult(t *testing.T) {
	t.Parallel()

	st := &store{honours: true, stats: alerthistory.Stats{Count24h: 99}}

	res, err := alerthistory.New(st, clock.NewFake(baseTime)).Enrich(expired(t), subject())

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"the pipeline tells a timeout from a failure by this, and they earn different remedies")
	assert.Equal(t, domain.Result{}, res, "a timed-out query produces no partial answer")
}

// TestACancelledRunProducesNoResultEither.
func TestACancelledRunProducesNoResultEither(t *testing.T) {
	t.Parallel()

	st := &store{honours: true, stats: alerthistory.Stats{Count24h: 99}}
	ctx, cancel := context.WithCancel(scoped(t))
	cancel()

	res, err := alerthistory.New(st, clock.NewFake(baseTime)).Enrich(ctx, subject())

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	assert.Equal(t, domain.Result{}, res)
}

// TestTheClockIsInjectedAndTheWindowsMoveWithIt: the store is asked about "now",
// and "now" is the pipeline's clock, never the machine's.
func TestTheClockIsInjectedAndTheWindowsMoveWithIt(t *testing.T) {
	t.Parallel()

	st := &store{stats: alerthistory.Stats{FiringDurationsSeconds: []float64{60}}}
	clk := clock.NewFake(baseTime)
	e := alerthistory.New(st, clk)

	_, err := e.Enrich(scoped(t), subject())
	require.NoError(t, err)
	assert.Equal(t, baseTime, st.sawNow)

	clk.Advance(36 * time.Hour)
	_, err = e.Enrich(scoped(t), subject())
	require.NoError(t, err)
	assert.Equal(t, baseTime.Add(36*time.Hour), st.sawNow,
		"the rolling windows are computed against the injected clock")
}

func TestANilClockFallsBackToTheSystemClock(t *testing.T) {
	t.Parallel()

	st := &store{stats: alerthistory.Stats{FiringDurationsSeconds: []float64{60}}}

	_, err := alerthistory.New(st, nil).Enrich(scoped(t), subject())
	require.NoError(t, err, "a nil clock must not be a nil dereference at 3am")
	assert.False(t, st.sawNow.IsZero())
}
