package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// TestApply_T6ExpiresAsTheReasonItWasProvenFor — ADR 0056 §2–§4. T6 has three
// expiries now, and each is refused unless the row and the caller's proofs make
// ITS claim true: `silent` asks the source's health and the row's silence,
// `source_removed` asks only that no live source is left. None of them is ever a
// resolution, and every one reads as `expired`.
func TestApply_T6ExpiresAsTheReasonItWasProvenFor(t *testing.T) {
	maxSilence := 24 * time.Hour
	lastHeard := t0.Add(time.Hour)
	heard := func(p *CaseParams) { p.LastObservedAt = lastHeard }
	reap := func(when time.Time, mut func(*TransitionCommand)) TransitionCommand {
		cmd := TransitionCommand{
			Trigger: TriggerReap, Actor: actor(t, ActorReaper),
			At: at(t, when, when), EventID: eventIDFix,
		}
		mut(&cmd)
		return cmd
	}

	t.Run("silent: a healthy source past its max silence expires the case as silent", func(t *testing.T) {
		for _, from := range []State{StateFiring, StateSuppressed} {
			o := caseIn(t, from, heard)
			when := lastHeard.Add(maxSilence + time.Nanosecond)
			res, err := Apply(o, reap(when, func(c *TransitionCommand) {
				c.ExpireAs, c.MaxSilence, c.SourceHealthy = ResolveSilent, maxSilence, true
			}))
			require.NoError(t, err)
			assert.Equal(t, TransitionT6, res.ID)
			assert.Equal(t, ResolveSilent, res.Case.ResolveReason())
			assert.Equal(t, StateExpired, res.Case.AlertState(), "silent reads as expired, never resolved")
			assert.Equal(t, when, res.Case.EndedAt())
			require.Len(t, res.Events, 1)
			assert.Equal(t, EventCaseExpired, res.Events[0].Type())
			assert.Equal(t, "silent", res.Events[0].Payload()["resolve_reason"])
			assert.NotContains(t, strings.ToLower(res.Events[0].Summary()), "resolved")
		}
	})

	t.Run("silent: NOT before max silence has elapsed since last_observed_at", func(t *testing.T) {
		o := caseIn(t, StateFiring, heard)
		_, err := Apply(o, reap(lastHeard.Add(maxSilence), func(c *TransitionCommand) {
			c.ExpireAs, c.MaxSilence, c.SourceHealthy = ResolveSilent, maxSilence, true
		}))
		requireKind(t, err, errs.KindPrecondition, "max_silence_not_elapsed")
	})

	t.Run("silent: §B.4 holds it under a source that is not healthy", func(t *testing.T) {
		o := caseIn(t, StateFiring, heard)
		_, err := Apply(o, reap(lastHeard.Add(48*time.Hour), func(c *TransitionCommand) {
			c.ExpireAs, c.MaxSilence = ResolveSilent, maxSilence
		}))
		requireKind(t, err, errs.KindPrecondition, "source_not_healthy")
	})

	t.Run("silent: a source with max_silence_s off ends nothing", func(t *testing.T) {
		o := caseIn(t, StateFiring, heard)
		_, err := Apply(o, reap(lastHeard.Add(48*time.Hour), func(c *TransitionCommand) {
			c.ExpireAs, c.SourceHealthy = ResolveSilent, true
		}))
		requireKind(t, err, errs.KindPrecondition, "max_silence_off")
	})

	t.Run("source_removed: expires with no health to ask, once no live source is left", func(t *testing.T) {
		o := caseIn(t, StateFiring, heard)
		when := lastHeard.Add(time.Minute)
		res, err := Apply(o, reap(when, func(c *TransitionCommand) {
			c.ExpireAs, c.NoLiveSource = ResolveSourceRemoved, true
		}))
		require.NoError(t, err)
		assert.Equal(t, ResolveSourceRemoved, res.Case.ResolveReason())
		assert.Equal(t, StateExpired, res.Case.AlertState())
		assert.Equal(t, "Case expired: its source was removed", res.Events[0].Summary())
	})

	t.Run("source_removed: refused while a live source still feeds the cluster", func(t *testing.T) {
		o := caseIn(t, StateFiring, heard)
		_, err := Apply(o, reap(lastHeard.Add(time.Minute), func(c *TransitionCommand) {
			c.ExpireAs, c.SourceHealthy = ResolveSourceRemoved, true
		}))
		requireKind(t, err, errs.KindPrecondition, "live_source_remains")
	})

	t.Run("T6 cannot be asked for a resolution under any spelling", func(t *testing.T) {
		o := caseIn(t, StateFiring, heard)
		_, err := Apply(o, reap(lastHeard.Add(48*time.Hour), func(c *TransitionCommand) {
			c.ExpireAs, c.SourceHealthy, c.NoLiveSource = ResolveUpstream, true, true
		}))
		requireKind(t, err, errs.KindInternal, "expiry_reason_invalid")
	})

	t.Run("a case holding an upstream resolve is not expired by any reason", func(t *testing.T) {
		o := caseIn(t, StateFiring, heard, func(p *CaseParams) {
			p.ResolvePendingAt = lastHeard
			p.ResolvePendingEndAt = lastHeard
		})
		_, err := Apply(o, reap(lastHeard.Add(time.Minute), func(c *TransitionCommand) {
			c.ExpireAs, c.NoLiveSource = ResolveSourceRemoved, true
		}))
		requireKind(t, err, errs.KindPrecondition, "close_pending")
	})
}

// TestResolveReason_OnlyUpstreamIsAResolution pins ADR 0056 §4 at the enum.
func TestResolveReason_OnlyUpstreamIsAResolution(t *testing.T) {
	for _, s := range []string{"upstream", "timeout", "silent", "source_removed"} {
		r, err := NewResolveReason(s)
		require.NoError(t, err)
		assert.Equal(t, s != "upstream", r.IsExpiry(), s)
	}
	_, err := NewResolveReason("manual")
	require.Error(t, err, "a person never ends a Case (ADR 0056 Holds; verdict #34)")
}
