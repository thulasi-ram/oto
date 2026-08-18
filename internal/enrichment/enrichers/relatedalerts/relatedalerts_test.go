package relatedalerts_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/relatedalerts"
	"github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// alert.related states that these alerts OVERLAPPED IN TIME AND IN ONE LABEL
// DIMENSION. It does not say they share a cause, because it does not know —
// machine-derived correlation with a stated algorithm is the deferred
// `correlation` module, and a weaker version of it here under a friendlier name
// is the scope creep the boundary document exists to prevent.

var baseTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

type store struct {
	found  []relatedalerts.Related
	counts map[string]int
	err    error

	calls   int
	sawQ    relatedalerts.Query
	honours bool
}

func (s *store) RelatedAlerts(
	ctx context.Context, _ db.TenantScope, q relatedalerts.Query,
) ([]relatedalerts.Related, map[string]int, error) {
	s.calls++
	s.sawQ = q
	if s.honours {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
	}
	if s.err != nil {
		return nil, nil, s.err
	}
	return append([]relatedalerts.Related(nil), s.found...), s.counts, nil
}

var (
	alertID = id.New()
	caseID  = id.New()
)

func subject() *domain.Subject {
	return &domain.Subject{
		OrgID:       id.NewString(),
		SubjectKind: domain.SubjectCase,
		SubjectID:   caseID.String(),
		Alert: domain.AlertSnapshot{
			ID:        alertID.String(),
			AlertName: "HighErrorRate",
			Namespace: "payments",
			Severity:  "critical",
		},
		Case: domain.CaseSnapshot{ID: caseID.String(), StartedAt: baseTime},
	}
}

func scoped(t *testing.T) context.Context {
	t.Helper()
	s, err := db.NewTenantScope(id.New())
	require.NoError(t, err)
	return service.WithScope(context.Background(), s)
}

func expired(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(scoped(t), baseTime.Add(-time.Hour))
	t.Cleanup(cancel)
	return ctx
}

func payloadOf(t *testing.T, res domain.Result) relatedalerts.Payload {
	t.Helper()
	p, ok := res.Payload.(relatedalerts.Payload)
	require.True(t, ok)
	return p
}

func related(relation, name string, startedAt time.Time) relatedalerts.Related {
	return relatedalerts.Related{
		Relation:  relation,
		AlertID:   id.NewString(),
		AlertKey:  "ak_" + name,
		AlertName: name,
		State:     "firing",
		CaseID:    id.NewString(),
		StartedAt: startedAt,
	}
}

// ------------------------------------------------------------------ the ports

func TestTheRegistryContractIsStable(t *testing.T) {
	t.Parallel()

	e := relatedalerts.New(&store{}, clock.NewFake(baseTime))

	assert.Equal(t, "alert.related", e.Name())
	assert.True(t, domain.ValidEnricherName(e.Name()))
	assert.Equal(t, 1, e.Version())
	assert.Equal(t, domain.PhaseAsync, e.Phase(),
		"a three-way scan over a hot table is not worth pre-notification budget")
	assert.Equal(t, 2*time.Second, e.Timeout(),
		"looser than the inline enrichers', because only the amendment waits on it")
	assert.Equal(t, time.Hour, relatedalerts.Window)
	assert.Equal(t, 30*time.Second, relatedalerts.CacheTTL,
		"\"what else is firing\" is a statement about right now")
}

func TestApplicableNeedsAnIdentityAndSomethingToSearchOn(t *testing.T) {
	t.Parallel()

	e := relatedalerts.New(&store{}, clock.NewFake(baseTime))

	assert.True(t, e.Applicable(subject()))
	assert.False(t, e.Applicable(nil))

	noID := subject()
	noID.Alert.ID = ""
	assert.False(t, e.Applicable(noID))

	noRelations := subject()
	noRelations.Alert.AlertName, noRelations.Alert.Namespace = "", ""
	assert.False(t, e.Applicable(noRelations), "no alertname and no namespace: nothing to relate on")

	onlyNamespace := subject()
	onlyNamespace.Alert.AlertName = ""
	assert.True(t, e.Applicable(onlyNamespace), "one relation is enough")

	assert.False(t, relatedalerts.New(nil, clock.NewFake(baseTime)).Applicable(subject()))
}

// TestCacheSeedBucketsOnTheCaseStartNotOnNow.
//
// Two cases of the same alert minutes apart genuinely have different
// neighbourhoods; a seed built from `now` would make the cache either useless or
// wrong depending on which way it rounded.
func TestCacheSeedBucketsOnTheCaseStartNotOnNow(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(baseTime)
	e := relatedalerts.New(&store{}, clk)

	sameBucket := subject()
	sameBucket.Case.StartedAt = baseTime.Add(20 * time.Minute)
	assert.Equal(t, e.CacheSeed(subject()), e.CacheSeed(sameBucket),
		"two fires inside one window share a neighbourhood, so they share a seed")

	nextBucket := subject()
	nextBucket.Case.StartedAt = baseTime.Add(90 * time.Minute)
	assert.NotEqual(t, e.CacheSeed(subject()), e.CacheSeed(nextBucket),
		"an hour later is a different neighbourhood")

	// Advancing the wall clock must not move the bucket.
	before := e.CacheSeed(subject())
	clk.Advance(45 * time.Minute)
	assert.Equal(t, before, e.CacheSeed(subject()))

	assert.Empty(t, e.CacheSeed(nil))
	noID := subject()
	noID.Alert.ID = ""
	assert.Empty(t, e.CacheSeed(noID))
}

// --------------------------------------------------------------------- found

func TestTheNeighbourhoodIsReportedStrongestRelationFirst(t *testing.T) {
	t.Parallel()

	st := &store{
		found: []relatedalerts.Related{
			related(relatedalerts.RelationNamespace, "DiskFilling", baseTime.Add(-10*time.Minute)),
			related(relatedalerts.RelationAlertName, "HighErrorRate", baseTime.Add(-time.Minute)),
			related(relatedalerts.RelationGroup, "LatencyHigh", baseTime.Add(-30*time.Minute)),
			related(relatedalerts.RelationAlertName, "HighErrorRate", baseTime.Add(-20*time.Minute)),
		},
		counts: map[string]int{
			relatedalerts.RelationGroup:     1,
			relatedalerts.RelationAlertName: 2,
			relatedalerts.RelationNamespace: 1,
		},
	}

	res, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
	require.NoError(t, err)

	assert.Equal(t, domain.StatusOK, res.Status)
	assert.Equal(t, relatedalerts.CacheTTL, res.TTL)

	p := payloadOf(t, res)
	assert.Equal(t, 3600, p.WindowSeconds)
	assert.False(t, p.Truncated)

	relations := make([]string, 0, len(p.Alerts))
	for _, a := range p.Alerts {
		relations = append(relations, a.Relation)
	}
	assert.Equal(t, []string{
		relatedalerts.RelationGroup,
		relatedalerts.RelationAlertName,
		relatedalerts.RelationAlertName,
		relatedalerts.RelationNamespace,
	}, relations,
		"same_group is the strongest signal available and the only one oto did not invent")

	// Within a relation: newest first.
	assert.True(t, p.Alerts[1].StartedAt.After(p.Alerts[2].StartedAt))

	assert.Equal(t, map[string]int{
		relatedalerts.RelationGroup:     1,
		relatedalerts.RelationAlertName: 2,
		relatedalerts.RelationNamespace: 1,
	}, p.Counts, "the count is the honest number; the list is a sample")
}

func TestTheQueryIsWindowedAroundTheCaseStart(t *testing.T) {
	t.Parallel()

	st := &store{found: []relatedalerts.Related{related(relatedalerts.RelationGroup, "X", baseTime)}}

	_, err := relatedalerts.New(st, clock.NewFake(baseTime.Add(9*time.Hour))).Enrich(scoped(t), subject())
	require.NoError(t, err)

	assert.Equal(t, baseTime.Add(-time.Hour), st.sawQ.From)
	assert.Equal(t, baseTime.Add(time.Hour), st.sawQ.To,
		"the window is centred on the FIRE, not on when the enricher happened to run")
	assert.Equal(t, alertID, st.sawQ.AlertID)
	assert.Equal(t, caseID, st.sawQ.CaseID, "the subject is excluded from its own results")
	assert.Equal(t, "HighErrorRate", st.sawQ.AlertName)
	assert.Equal(t, "payments", st.sawQ.Namespace)
	assert.Equal(t, relatedalerts.MaxPerRelation, st.sawQ.Limit)
}

func TestAnCaseWithNoStartFallsBackToTheClock(t *testing.T) {
	t.Parallel()

	st := &store{found: []relatedalerts.Related{related(relatedalerts.RelationGroup, "X", baseTime)}}
	s := subject()
	s.Case.StartedAt = time.Time{}

	_, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), s)
	require.NoError(t, err)

	assert.Equal(t, baseTime.Add(-time.Hour), st.sawQ.From, "the injected clock, never the machine's")
}

func TestTheWindowIsOverridable(t *testing.T) {
	t.Parallel()

	st := &store{found: []relatedalerts.Related{related(relatedalerts.RelationGroup, "X", baseTime)}}
	e := relatedalerts.New(st, clock.NewFake(baseTime)).WithWindow(10 * time.Minute)

	res, err := e.Enrich(scoped(t), subject())
	require.NoError(t, err)

	assert.Equal(t, baseTime.Add(-10*time.Minute), st.sawQ.From)
	assert.Equal(t, 600, payloadOf(t, res).WindowSeconds, "the payload states the window it used")

	// A non-positive override is ignored rather than producing a zero window.
	assert.Equal(t, 600, payloadOf(t, res).WindowSeconds)
	e.WithWindow(0)
	_, err = e.Enrich(scoped(t), subject())
	require.NoError(t, err)
	assert.Equal(t, baseTime.Add(-10*time.Minute), st.sawQ.From)
}

// TestAStormIsSampledAndSaysSo. A card listing a thousand related alerts is
// unreadable; a count plus the newest few is not.
func TestAStormIsSampledAndSaysSo(t *testing.T) {
	t.Parallel()

	found := make([]relatedalerts.Related, 0, relatedalerts.MaxTotal+5)
	for i := 0; i < relatedalerts.MaxTotal+5; i++ {
		found = append(found, related(relatedalerts.RelationNamespace, "Other",
			baseTime.Add(-time.Duration(i)*time.Minute)))
	}
	st := &store{found: found, counts: map[string]int{relatedalerts.RelationNamespace: 900}}

	res, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
	require.NoError(t, err)

	p := payloadOf(t, res)
	assert.Len(t, p.Alerts, relatedalerts.MaxTotal)
	assert.True(t, p.Truncated)
	assert.Equal(t, 900, p.Counts[relatedalerts.RelationNamespace],
		"the total is reported honestly even though the list is capped")
}

func TestTruncationIsReportedWhenTheCountsExceedTheSample(t *testing.T) {
	t.Parallel()

	st := &store{
		found:  []relatedalerts.Related{related(relatedalerts.RelationGroup, "X", baseTime)},
		counts: map[string]int{relatedalerts.RelationGroup: 40},
	}

	res, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
	require.NoError(t, err)

	p := payloadOf(t, res)
	assert.Len(t, p.Alerts, 1)
	assert.True(t, p.Truncated, "one of forty is a sample, and the card must say so")
}

func TestTheOrderIsStableAcrossRuns(t *testing.T) {
	t.Parallel()

	shared := baseTime.Add(-5 * time.Minute)
	st := &store{found: []relatedalerts.Related{
		{Relation: relatedalerts.RelationGroup, AlertID: "bbbb", AlertName: "B", StartedAt: shared},
		{Relation: relatedalerts.RelationGroup, AlertID: "aaaa", AlertName: "A", StartedAt: shared},
	}}

	first, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
	require.NoError(t, err)

	for i := 0; i < 10; i++ {
		again, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
		require.NoError(t, err)
		require.Equal(t, payloadOf(t, first), payloadOf(t, again),
			"an amended card that reshuffles between two identical runs churns for no reason")
	}
	assert.Equal(t, "aaaa", payloadOf(t, first).Alerts[0].AlertID,
		"an exact tie on relation and instant is broken by id")
}

// ----------------------------------------------------------------- not found

// TestAnIsolatedAlertIsSkippedRatherThanAnnounced.
//
// "Nothing nearby" is a genuine, useful answer, but it is not worth amending a
// card for, so it is recorded as skipped and contributes nothing to the
// coalesced reply.
func TestAnIsolatedAlertIsSkippedRatherThanAnnounced(t *testing.T) {
	t.Parallel()

	st := &store{}

	res, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
	require.NoError(t, err)

	assert.Equal(t, domain.StatusSkipped, res.Status)
	assert.False(t, res.Status.Succeeded(), "so the async pass has nothing to announce")

	p := payloadOf(t, res)
	assert.Empty(t, p.Alerts)
	assert.False(t, p.Truncated)
	assert.NotNil(t, p.Counts, "an absent count map is rendered as {}, never as null")
	assert.Empty(t, p.Counts)
}

func TestAnAlertWithNoIdentityIsSkippedNotFailed(t *testing.T) {
	t.Parallel()

	st := &store{}
	s := subject()
	s.Alert.ID = "not-a-uuid"

	res, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), s)
	require.NoError(t, err)

	assert.Equal(t, domain.StatusSkipped, res.Status)
	assert.Equal(t, []string{"no_alert_id"}, res.Warnings)
	assert.Nil(t, res.Payload)
	assert.Zero(t, st.calls)
}

// ------------------------------------------------------------ upstream error

func TestAnUpstreamRefusalIsAnErrorAndNothingElse(t *testing.T) {
	t.Parallel()

	st := &store{
		err:    errs.New(errs.KindInternal, "alerts_query_failed", "the pool is exhausted"),
		found:  []relatedalerts.Related{related(relatedalerts.RelationGroup, "X", baseTime)},
		counts: map[string]int{relatedalerts.RelationGroup: 1},
	}

	res, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())

	require.Error(t, err)
	assert.Equal(t, "alerts_query_failed", errs.CodeOf(err))
	assert.Equal(t, domain.Result{}, res,
		"never a neighbourhood that looks complete but was assembled from a failed scan")
}

func TestAnEnricherWithoutAScopeFailsRatherThanQueryingUnscoped(t *testing.T) {
	t.Parallel()

	st := &store{}

	res, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(context.Background(), subject())

	require.Error(t, err)
	assert.Equal(t, "enrichment_no_tenant_scope", errs.CodeOf(err))
	assert.Equal(t, domain.Result{}, res)
	assert.Zero(t, st.calls)
}

// ------------------------------------------------------------------ timeout

func TestAWithdrawnBudgetSurfacesAsADeadlineAndNoResult(t *testing.T) {
	t.Parallel()

	st := &store{
		honours: true,
		found:   []relatedalerts.Related{related(relatedalerts.RelationGroup, "X", baseTime)},
	}

	res, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(expired(t), subject())

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"a timeout earns a re-run in the async phase; a failure earns the job's retry policy")
	assert.Equal(t, domain.Result{}, res)
}

func TestACancelledRunProducesNoResultEither(t *testing.T) {
	t.Parallel()

	st := &store{honours: true}
	ctx, cancel := context.WithCancel(scoped(t))
	cancel()

	res, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(ctx, subject())

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	assert.Equal(t, domain.Result{}, res)
}

func TestANilClockFallsBackToTheSystemClock(t *testing.T) {
	t.Parallel()

	st := &store{}
	s := subject()
	s.Case.StartedAt = time.Time{}

	_, err := relatedalerts.New(st, nil).Enrich(scoped(t), s)
	require.NoError(t, err)
	assert.False(t, st.sawQ.From.IsZero())
}

// TestTheRelationSetIsClosed. Each member is a plain, checkable statement about
// labels — never a heuristic, and never a causal claim.
func TestTheRelationSetIsClosed(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "same_group", relatedalerts.RelationGroup)
	assert.Equal(t, "same_alertname", relatedalerts.RelationAlertName)
	assert.Equal(t, "same_namespace", relatedalerts.RelationNamespace)

	// An unknown relation sorts last rather than crashing or being dropped: a
	// store from a newer build must degrade, not disappear.
	st := &store{found: []relatedalerts.Related{
		{Relation: "invented_by_a_newer_store", AlertID: "z", StartedAt: baseTime},
		{Relation: relatedalerts.RelationNamespace, AlertID: "a", StartedAt: baseTime},
	}}

	res, err := relatedalerts.New(st, clock.NewFake(baseTime)).Enrich(scoped(t), subject())
	require.NoError(t, err)

	p := payloadOf(t, res)
	require.Len(t, p.Alerts, 2)
	assert.Equal(t, relatedalerts.RelationNamespace, p.Alerts[0].Relation)
	assert.Equal(t, "invented_by_a_newer_store", p.Alerts[1].Relation)
}
