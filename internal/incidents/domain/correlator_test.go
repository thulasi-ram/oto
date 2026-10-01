package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ADR 0052 §2 and §4 (git-bug 61eeddf), the pure half: what a Correlator accepts
// as configuration, what its matchers hold against, and the count condition's
// verdict on a window of Cases.

func TestACorrelatorMatchesWithTheNotificationPolicyGrammar(t *testing.T) {
	t.Parallel()
	k := Correlator{Matchers: []kernel.Matcher{
		{Name: "cluster", Op: kernel.OpEqual, Value: "prod"},
		{Name: "namespace", Op: kernel.OpMatch, Value: "pay.*"},
		{Name: "team", Op: kernel.OpNotEqual, Value: "web"},
	}}

	ok, err := k.Matches(map[string]string{"cluster": "prod", "namespace": "payments"})
	require.NoError(t, err)
	assert.True(t, ok, "every matcher holds, and a missing label is the empty string for `!=`")

	ok, err = k.Matches(map[string]string{"cluster": "prod", "namespace": "xpayments"})
	require.NoError(t, err)
	assert.False(t, ok, "`=~` is anchored, as a policy's is")

	ok, err = Correlator{}.Matches(map[string]string{"anything": "at all"})
	require.NoError(t, err)
	assert.True(t, ok, "no matchers is \"any Case\", as an empty policy matches every fact")
}

func TestACorrelatorIsValidatedAgainstEveryBoundTheTableHolds(t *testing.T) {
	t.Parallel()
	valid := Correlator{Name: "payments storm", Priority: 100, Enabled: true,
		Count: Count{Min: 5, Window: 600 * time.Second}}
	require.NoError(t, valid.Validate())

	cases := map[string]struct {
		mutate func(*Correlator)
		field  string
	}{
		"blank name":          {func(c *Correlator) { c.Name = "  " }, "name"},
		"priority too high":   {func(c *Correlator) { c.Priority = 10001 }, "priority"},
		"half a count":        {func(c *Correlator) { c.Count = Count{Min: 5} }, "count_min"},
		"a count of one":      {func(c *Correlator) { c.Count.Min = 1 }, "count_min"},
		"a window under 60 s": {func(c *Correlator) { c.Count.Window = 30 * time.Second }, "count_window_seconds"},
		"a broken regex": {func(c *Correlator) {
			c.Matchers = []kernel.Matcher{{Name: "a", Op: kernel.OpMatch, Value: "("}}
		}, "matchers.value"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := valid
			tc.mutate(&c)
			err := c.Validate()
			require.Error(t, err)
			assert.Equal(t, "correlator_invalid", errs.CodeOf(err))
			var fields []string
			for _, v := range errs.ViolationsOf(err) {
				fields = append(fields, v.Field)
			}
			assert.Contains(t, fields, tc.field)
		})
	}
}

func TestADisabledCountClearsEverythingAndAnEnabledOneCounts(t *testing.T) {
	t.Parallel()
	assert.True(t, Count{}.Clears(1), "no count condition draws on the first match")
	c := Count{Min: 5, Window: 10 * time.Minute}
	assert.False(t, c.Clears(4))
	assert.True(t, c.Clears(5))
}

func at(start time.Time, offset time.Duration) CaseAt {
	return CaseAt{CaseRef: CaseRef{ID: uuid.New()}, StartedAt: start.Add(offset)}
}

func TestTheSpanFindsTheFifthCaseWhicheverOrderTheJobsRan(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	c := Count{Min: 5, Window: 600 * time.Second}
	five := []CaseAt{at(t0, 0), at(t0, time.Minute), at(t0, 2*time.Minute), at(t0, 3*time.Minute), at(t0, 4*time.Minute)}

	// Whichever Case's job runs last — the first to start, the last to start, one
	// in the middle — it finds the other four recorded and the verdict is the same.
	for i := range five {
		anchor := five[i]
		var others []CaseAt
		for j := range five {
			if j != i {
				others = append(others, five[j])
			}
		}
		span, ok := c.Span(anchor, others)
		require.True(t, ok, "anchor %d: five Cases inside 600 s clear a count of five", i)
		assert.Len(t, span, 5)
	}

	// Four is below the count from any anchor.
	_, ok := c.Span(five[4], five[:3])
	assert.False(t, ok)
}

func TestTheSpanIsAWindowOfTheCorrelatorsLengthNotTwiceIt(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	c := Count{Min: 3, Window: 600 * time.Second}
	anchor := at(t0, 0)
	// One Case nine minutes before, one nine minutes after: three Cases inside
	// [anchor - W, anchor + W], but no 600-second window holds all three.
	before, after := at(t0, -9*time.Minute), at(t0, 9*time.Minute)
	_, ok := c.Span(anchor, []CaseAt{before, after})
	assert.False(t, ok, "a span through the anchor is at most W long")

	// Move the later one inside ten minutes of the earlier one and it clears.
	after = at(t0, time.Minute)
	span, ok := c.Span(anchor, []CaseAt{before, after})
	require.True(t, ok)
	assert.Equal(t, []uuid.UUID{before.ID, anchor.ID, after.ID},
		[]uuid.UUID{span[0].ID, span[1].ID, span[2].ID}, "drawn in start order")
}
