package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/grouping/domain"
)

func TestDefaultStormPolicy(t *testing.T) {
	t.Parallel()

	p := domain.DefaultStormPolicy()
	assert.Equal(t, domain.DefaultStormThreshold, p.Threshold)
	assert.Equal(t, domain.DefaultStormWindow, p.Window)
	assert.Equal(t, domain.DefaultStormCooldown, p.Cooldown)
	assert.Equal(t, domain.DefaultGroupCloseDelay, p.CloseDelay)

	// The §D.1 numbers themselves. Storm collapse is ON BY DEFAULT and there is
	// no "off" — a zero threshold would collapse every group on its first member.
	//
	// The three storm numbers are the group-level damper's own and answer a
	// question nothing else asks: how many DISTINCT alerts joining one generation
	// inside a minute counts as one event rather than many. They were never part
	// of ADR 0026's derivation and did not move.
	assert.Equal(t, 25, domain.DefaultStormThreshold)
	assert.Equal(t, 60*time.Second, domain.DefaultStormWindow)
	assert.Equal(t, 10*time.Minute, domain.DefaultStormCooldown)

	// ⛔ `group_close_delay` is NOT a storm number. It only lives in StormPolicy
	// because it is the same kind of clock, and it MIRRORS
	// `identity/domain.DefaultGroupCloseDelay`, which ADR 0026 pins EQUAL to
	// `refire_grace`. 20m, not the 5m this test used to transcribe: closing a
	// generation freezes its Slack thread, so the next observation opens N+1 with
	// a brand-new root card (ADR 0005, §B.5). At 5m against a 10m grace the
	// generation closed halfway through the grace and the whole second half bought
	// an occurrence reopen that posted a new card anyway — the mismatch defeated
	// the grace. Equality is safe rather than racy because this clock runs from the
	// group's last ACTIVITY (the resolve as oto observed it) and the grace runs
	// from the upstream `ended_at`, which is the same instant or earlier.
	//
	// If this assertion fails, the fix is almost certainly in `identity/domain`
	// and not here; `identity/domain/defaults_derivation_test.go` is what keeps
	// the two in step.
	assert.Equal(t, 20*time.Minute, domain.DefaultGroupCloseDelay)
}

func TestStormPolicyNormalise(t *testing.T) {
	t.Parallel()

	d := domain.DefaultStormPolicy()

	cases := []struct {
		name string
		in   domain.StormPolicy
		want domain.StormPolicy
	}{
		{
			name: "a wholly unconfigured org gets the defaults",
			in:   domain.StormPolicy{},
			want: d,
		},
		{
			name: "a zero threshold can never collapse on the first member",
			in:   domain.StormPolicy{Window: time.Second, Cooldown: time.Second, CloseDelay: time.Second},
			want: domain.StormPolicy{
				Threshold: d.Threshold, Window: time.Second,
				Cooldown: time.Second, CloseDelay: time.Second,
			},
		},
		{
			name: "negative values are treated as unset",
			in: domain.StormPolicy{
				Threshold: -1, Window: -time.Second,
				Cooldown: -time.Second, CloseDelay: -time.Second,
			},
			want: d,
		},
		{
			name: "a fully configured org is left alone",
			in: domain.StormPolicy{
				Threshold: 3, Window: 5 * time.Second,
				Cooldown: 2 * time.Minute, CloseDelay: time.Minute,
			},
			want: domain.StormPolicy{
				Threshold: 3, Window: 5 * time.Second,
				Cooldown: 2 * time.Minute, CloseDelay: time.Minute,
			},
		},
		{
			name: "a threshold of one is configuration, not absence",
			in:   domain.StormPolicy{Threshold: 1},
			want: domain.StormPolicy{
				Threshold: 1, Window: d.Window,
				Cooldown: d.Cooldown, CloseDelay: d.CloseDelay,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.in.Normalise())
		})
	}
}

func TestStormPolicyNormaliseIsIdempotent(t *testing.T) {
	t.Parallel()

	once := domain.StormPolicy{Threshold: 4}.Normalise()
	assert.Equal(t, once, once.Normalise())
}

// storming is an open generation already collapsed into storm mode.
func storming(t *testing.T, since time.Time) domain.Group {
	t.Helper()
	return mustGroup(t, func(p *domain.GroupParams) {
		p.Counts = domain.Counts{Firing: 40, Total: 40}
		p.StormMode = true
		p.StormSince = since
	})
}

func TestEvaluateStormEntersOnlyAboveTheThreshold(t *testing.T) {
	t.Parallel()

	policy := domain.StormPolicy{Threshold: 25, Window: time.Minute, Cooldown: 10 * time.Minute, CloseDelay: time.Minute}
	now := baseTime.Add(time.Hour)

	cases := []struct {
		name          string
		distinctJoins int
		want          domain.StormAction
	}{
		{name: "one under the threshold", distinctJoins: 24, want: domain.StormUnchanged},
		{name: "exactly at the threshold is not MORE THAN", distinctJoins: 25, want: domain.StormUnchanged},
		{name: "one over the threshold collapses", distinctJoins: 26, want: domain.StormStart},
		{name: "far over the threshold", distinctJoins: 400, want: domain.StormStart},
		{name: "no joins at all", distinctJoins: 0, want: domain.StormUnchanged},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := mustGroup(t, func(p *domain.GroupParams) {
				p.Counts = domain.Counts{Firing: 1, Total: 1}
			})
			d := domain.EvaluateStorm(g, tc.distinctJoins, time.Time{}, now, policy)
			assert.Equal(t, tc.want, d.Action)
			assert.Equal(t, tc.distinctJoins, d.DistinctJoins)
			if tc.want == domain.StormStart {
				assert.True(t, d.Since.Equal(now), "a start records when it began")
			} else {
				assert.True(t, d.Since.IsZero())
			}
		})
	}
}

// TestEvaluateStormEchoesThePolicyInForce: the timeline must record the policy
// that decided, not the one configured when somebody reads the event later.
func TestEvaluateStormEchoesThePolicyInForce(t *testing.T) {
	t.Parallel()

	g := mustGroup(t, nil)

	custom := domain.StormPolicy{Threshold: 3, Window: 5 * time.Second, Cooldown: time.Minute, CloseDelay: time.Minute}
	d := domain.EvaluateStorm(g, 4, time.Time{}, baseTime, custom)
	assert.Equal(t, domain.StormStart, d.Action)
	assert.Equal(t, 3, d.Threshold)
	assert.Equal(t, 5*time.Second, d.Window)

	// An unconfigured policy is normalised before it is echoed, so the event never
	// claims a threshold of zero.
	z := domain.EvaluateStorm(g, 26, time.Time{}, baseTime, domain.StormPolicy{})
	assert.Equal(t, domain.StormStart, z.Action)
	assert.Equal(t, domain.DefaultStormThreshold, z.Threshold)
	assert.Equal(t, domain.DefaultStormWindow, z.Window)
}

func TestEvaluateStormNormalisesNowToUTC(t *testing.T) {
	t.Parallel()

	tokyo := time.FixedZone("JST", 9*3600)
	g := mustGroup(t, nil)
	d := domain.EvaluateStorm(g, 100, time.Time{}, baseTime.In(tokyo), domain.DefaultStormPolicy())
	require.Equal(t, domain.StormStart, d.Action)
	assert.Equal(t, time.UTC, d.Since.Location())
	assert.True(t, d.Since.Equal(baseTime))
}

func TestEvaluateStormEndsOnlyAfterCooldownWithoutANewMember(t *testing.T) {
	t.Parallel()

	policy := domain.StormPolicy{Threshold: 25, Window: time.Minute, Cooldown: 10 * time.Minute, CloseDelay: time.Minute}
	since := baseTime
	lastJoin := baseTime.Add(30 * time.Minute)

	cases := []struct {
		name     string
		lastJoin time.Time
		now      time.Time
		want     domain.StormAction
	}{
		{
			name:     "a storm that is still growing is still a storm",
			lastJoin: lastJoin,
			now:      lastJoin.Add(time.Minute),
			want:     domain.StormUnchanged,
		},
		{
			name:     "one nanosecond before the cooldown elapses",
			lastJoin: lastJoin,
			now:      lastJoin.Add(10*time.Minute - time.Nanosecond),
			want:     domain.StormUnchanged,
		},
		{
			name:     "exactly at the cooldown",
			lastJoin: lastJoin,
			now:      lastJoin.Add(10 * time.Minute),
			want:     domain.StormEnd,
		},
		{
			name:     "well past the cooldown",
			lastJoin: lastJoin,
			now:      lastJoin.Add(time.Hour),
			want:     domain.StormEnd,
		},
		{
			name: "with no join recorded the cooldown runs from storm_since",
			now:  since.Add(10 * time.Minute),
			want: domain.StormEnd,
		},
		{
			name: "with no join recorded and inside the cooldown",
			now:  since.Add(time.Minute),
			want: domain.StormUnchanged,
		},
		{
			name:     "the cooldown is measured from the LAST join, not the first",
			lastJoin: lastJoin,
			now:      since.Add(20 * time.Minute),
			want:     domain.StormUnchanged,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := storming(t, since)
			d := domain.EvaluateStorm(g, 40, tc.lastJoin, tc.now, policy)
			assert.Equal(t, tc.want, d.Action)
			assert.True(t, d.Since.Equal(since),
				"an end echoes the existing storm_since so the event can say how long it ran")
		})
	}
}

// TestEvaluateStormCountsAlertsNotObservations: the same distinct-join count
// decides regardless of how firing the members are. One alert re-firing forty
// times is a flap (damped by flap_score), not a storm.
func TestEvaluateStormCountsDistinctJoins(t *testing.T) {
	t.Parallel()

	policy := domain.StormPolicy{Threshold: 25, Window: time.Minute, Cooldown: 10 * time.Minute, CloseDelay: time.Minute}

	noisyOne := mustGroup(t, func(p *domain.GroupParams) {
		p.Counts = domain.Counts{Firing: 1, Total: 1}
	})
	d := domain.EvaluateStorm(noisyOne, 1, baseTime, baseTime, policy)
	assert.Equal(t, domain.StormUnchanged, d.Action)
	assert.Equal(t, 1, d.DistinctJoins)

	manyQuiet := mustGroup(t, func(p *domain.GroupParams) {
		p.Counts = domain.Counts{Firing: 40, Total: 40}
	})
	d2 := domain.EvaluateStorm(manyQuiet, 40, baseTime, baseTime, policy)
	assert.Equal(t, domain.StormStart, d2.Action)
	assert.Equal(t, 40, d2.DistinctJoins)
}

func TestEvaluateStormOnAClosedGeneration(t *testing.T) {
	t.Parallel()

	policy := domain.DefaultStormPolicy()

	closedStorming := mustGroup(t, func(p *domain.GroupParams) {
		p.State = domain.StateClosed
		p.Counts = domain.Counts{Resolved: 40, Total: 40}
		p.ClosedAt = baseTime.Add(time.Hour)
		p.LastActivityAt = baseTime.Add(time.Hour)
		p.StormMode = true
		p.StormSince = baseTime
	})
	// A closed generation has no thread to collapse: the storm is unconditionally
	// wound up, without waiting for a cooldown.
	d := domain.EvaluateStorm(closedStorming, 999, baseTime.Add(time.Hour), baseTime.Add(time.Hour+time.Second), policy)
	assert.Equal(t, domain.StormEnd, d.Action)
	assert.True(t, d.Since.Equal(baseTime))

	closedCalm := mustGroup(t, func(p *domain.GroupParams) {
		p.State = domain.StateClosed
		p.Counts = domain.Counts{Resolved: 40, Total: 40}
		p.ClosedAt = baseTime.Add(time.Hour)
		p.LastActivityAt = baseTime.Add(time.Hour)
	})
	// A closed generation never ENTERS storm mode, however many joins are claimed.
	d2 := domain.EvaluateStorm(closedCalm, 9999, baseTime, baseTime.Add(time.Hour), policy)
	assert.Equal(t, domain.StormUnchanged, d2.Action)
}

func TestEvaluateStormDoesNotRestartAnActiveStorm(t *testing.T) {
	t.Parallel()

	policy := domain.StormPolicy{Threshold: 25, Window: time.Minute, Cooldown: 10 * time.Minute, CloseDelay: time.Minute}
	g := storming(t, baseTime)

	d := domain.EvaluateStorm(g, 4000, baseTime.Add(time.Minute), baseTime.Add(2*time.Minute), policy)
	assert.Equal(t, domain.StormUnchanged, d.Action)
	assert.True(t, d.Since.Equal(baseTime), "storm_since must not be rewritten mid-storm")
}

func TestApplyStorm(t *testing.T) {
	t.Parallel()

	t.Run("a start turns on the visible state and bumps state_version", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, func(p *domain.GroupParams) { p.StateVersion = 4 })
		at := baseTime.Add(time.Hour)

		next, changed := domain.ApplyStorm(g, domain.StormDecision{
			Action: domain.StormStart, Since: at, DistinctJoins: 40,
		})
		require.True(t, changed)
		assert.True(t, next.StormMode())
		assert.True(t, next.StormSince().Equal(at))
		assert.Equal(t, time.UTC, next.StormSince().Location())
		assert.Equal(t, 5, next.StateVersion(),
			"entering storm mode is material, so the announcement is a new intent (§C.7)")
	})

	t.Run("an end turns it off and bumps state_version", func(t *testing.T) {
		t.Parallel()
		g := storming(t, baseTime)
		before := g.StateVersion()

		next, changed := domain.ApplyStorm(g, domain.StormDecision{
			Action: domain.StormEnd, Since: baseTime,
		})
		require.True(t, changed)
		assert.False(t, next.StormMode())
		assert.True(t, next.StormSince().IsZero(), "groups_storm_ck is all-or-nothing")
		assert.Equal(t, before+1, next.StateVersion())
	})

	t.Run("a start on an already-storming generation is a no-op", func(t *testing.T) {
		t.Parallel()
		g := storming(t, baseTime)
		next, changed := domain.ApplyStorm(g, domain.StormDecision{
			Action: domain.StormStart, Since: baseTime.Add(time.Hour),
		})
		assert.False(t, changed)
		assert.Equal(t, g.StateVersion(), next.StateVersion())
		assert.True(t, next.StormSince().Equal(baseTime))
	})

	t.Run("an end on a calm generation is a no-op", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, nil)
		next, changed := domain.ApplyStorm(g, domain.StormDecision{Action: domain.StormEnd})
		assert.False(t, changed)
		assert.Equal(t, g.StateVersion(), next.StateVersion())
		assert.False(t, next.StormMode())
	})

	t.Run("unchanged never moves state_version", func(t *testing.T) {
		t.Parallel()
		for _, g := range []domain.Group{mustGroup(t, nil), storming(t, baseTime)} {
			next, changed := domain.ApplyStorm(g, domain.StormDecision{Action: domain.StormUnchanged})
			assert.False(t, changed)
			assert.Equal(t, g.StateVersion(), next.StateVersion())
			assert.Equal(t, g.StormMode(), next.StormMode())
		}
	})
}

// TestStormRoundTripPreservesTheAllOrNothingInvariant walks a whole storm
// lifecycle through Evaluate → Apply and re-proves groups_storm_ck at every step.
// Half a storm renders as neither.
func TestStormRoundTripPreservesTheAllOrNothingInvariant(t *testing.T) {
	t.Parallel()

	policy := domain.StormPolicy{Threshold: 25, Window: time.Minute, Cooldown: 10 * time.Minute, CloseDelay: time.Minute}
	g := mustGroup(t, func(p *domain.GroupParams) {
		p.Counts = domain.Counts{Firing: 40, Total: 40}
	})
	assertStormInvariant := func(g domain.Group) {
		t.Helper()
		assert.Equal(t, g.StormMode(), !g.StormSince().IsZero())
	}
	assertStormInvariant(g)

	// Enter.
	start := baseTime.Add(time.Hour)
	d := domain.EvaluateStorm(g, 40, start, start, policy)
	require.Equal(t, domain.StormStart, d.Action)
	g, changed := domain.ApplyStorm(g, d)
	require.True(t, changed)
	assertStormInvariant(g)

	// Still growing: nothing moves.
	d = domain.EvaluateStorm(g, 60, start.Add(5*time.Minute), start.Add(6*time.Minute), policy)
	require.Equal(t, domain.StormUnchanged, d.Action)
	g, changed = domain.ApplyStorm(g, d)
	require.False(t, changed)
	assertStormInvariant(g)

	// Cooldown elapses with no new member.
	last := start.Add(5 * time.Minute)
	d = domain.EvaluateStorm(g, 60, last, last.Add(10*time.Minute), policy)
	require.Equal(t, domain.StormEnd, d.Action)
	g, changed = domain.ApplyStorm(g, d)
	require.True(t, changed)
	assertStormInvariant(g)
	assert.False(t, g.StormMode())
}
