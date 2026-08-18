package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// These are the §B.3 ROUTING tests: for one observation against one episode,
// which row runs, and — when the row opens an episode — with what.
//
// ⭐ THEY NEED NO DATABASE, WHICH IS THE POINT. `Service.observe` used to decide
// this inline in a 294-line loop, so exercising one edge of the table meant
// assembling an observation slice, a settings value, an options value and four
// repositories. The decision is a value now, and the table below is one row per
// edge of SPEC §B.3.

// refireGraceFix is the window that resolves T7 against T8 in these tests.
// `caseIn` ends a terminal episode at t0+1m, so `withinGrace` reopens and
// `beyondGrace` opens a new episode.
const refireGraceFix = 15 * time.Minute

var (
	withinGrace = t0.Add(2 * time.Minute)
	beyondGrace = t0.Add(2 * time.Hour)
)

// decideCase is one row of §B.3 as seen from the routing question.
type decideCase struct {
	name    string
	current Case
	trigger Trigger
	// actor is the one the row permits. Decide never reads it; Apply does, and
	// the agreement test below needs the edge to be authorised.
	actor    ActorKind
	recorded time.Time
	// suppressionReason and sourceHealthy are what APPLY needs beyond the routing
	// fields. Decide reads neither, and that asymmetry is the reason the fallible
	// half of a command is assembled only once a row has been named.
	suppressionReason SuppressionReason
	sourceHealthy     bool

	want    Decision
	wantErr string
}

func (c decideCase) cmd(t *testing.T) TransitionCommand {
	t.Helper()
	return TransitionCommand{
		Trigger:           c.trigger,
		Actor:             actor(t, c.actor),
		At:                at(t, c.recorded, c.recorded),
		EventID:           eventIDFix,
		RefireGrace:       refireGraceFix,
		ResolveGrace:      5 * time.Minute,
		SuppressionReason: c.suppressionReason,
		SourceHealthy:     c.sourceHealthy,
	}
}

func decideCases(t *testing.T) []decideCase {
	t.Helper()

	// `acked_at` travels with the state: NewCase refuses a params whose
	// ack state and timestamp disagree, which is the invariant that makes an
	// acknowledgement a fact somebody took at a moment rather than a flag.
	acked := func(p *CaseParams) {
		p.AckState = AckStateAcked
		p.AckedAt = t0.Add(30 * time.Second)
		p.AckedByLabel = "ada@example.com"
	}
	seq := func(n int) func(*CaseParams) {
		return func(p *CaseParams) { p.Seq = n }
	}
	reapable := func(p *CaseParams) { p.SourceEndsAt = t0.Add(time.Minute) }

	return []decideCase{
		// ---------------------------------------------------- no episode at all
		{
			// ⭐ THE ZERO Case IS "this Alert has no episode". Its state is
			// StateNone, which is the state T1's row comes from.
			name:     "a first sighting opens the first episode (T1)",
			current:  Case{},
			trigger:  TriggerObserveFiring,
			actor:    ActorIngest,
			recorded: t0,
			want: Decision{
				ID: TransitionT1, From: StateNone,
				OpensEpisode: true, Seq: 1, ReopenOf: uuid.Nil,
			},
		},
		{
			// Inventing an episode to close would be fabricating history.
			name:     "a resolved for an Alert never seen firing transitions nothing",
			current:  Case{},
			trigger:  TriggerObserveResolved,
			actor:    ActorIngest,
			recorded: t0,
			wantErr:  "illegal_transition",
		},
		{
			name:     "a suppressed for an Alert never seen firing transitions nothing",
			current:  Case{},
			trigger:  TriggerObserveSuppressed,
			actor:    ActorReconciler,
			recorded: t0,
			wantErr:  "illegal_transition",
		},

		// ------------------------------------------------- edges on a live episode
		{
			name:     "a repeat observation is T2",
			current:  caseIn(t, StateFiring),
			trigger:  TriggerObserveFiring,
			actor:    ActorIngest,
			recorded: withinGrace,
			want:     Decision{ID: TransitionT2, From: StateFiring},
		},
		{
			name:              "suppression begins (T3)",
			current:           caseIn(t, StateFiring),
			trigger:           TriggerObserveSuppressed,
			actor:             ActorReconciler,
			recorded:          withinGrace,
			suppressionReason: SuppressionSilence,
			want:              Decision{ID: TransitionT3, From: StateFiring},
		},
		{
			name:     "suppression ends (T4)",
			current:  caseIn(t, StateSuppressed),
			trigger:  TriggerObserveFiring,
			actor:    ActorIngest,
			recorded: withinGrace,
			want:     Decision{ID: TransitionT4, From: StateSuppressed},
		},
		{
			name:     "an explicit upstream resolution (T5)",
			current:  caseIn(t, StateFiring),
			trigger:  TriggerObserveResolved,
			actor:    ActorIngest,
			recorded: withinGrace,
			want:     Decision{ID: TransitionT5, From: StateFiring},
		},
		{
			name:     "a suppressed episode resolves (T5)",
			current:  caseIn(t, StateSuppressed),
			trigger:  TriggerObserveResolved,
			actor:    ActorIngest,
			recorded: withinGrace,
			want:     Decision{ID: TransitionT5, From: StateSuppressed},
		},
		{
			name:          "the reaper expires an episode it has stopped hearing about (T6)",
			current:       caseIn(t, StateFiring, reapable),
			trigger:       TriggerReap,
			actor:         ActorReaper,
			recorded:      beyondGrace,
			sourceHealthy: true,
			want:          Decision{ID: TransitionT6, From: StateFiring},
		},

		// ------------------------------------------------------------ the re-fire
		{
			name:     "a re-fire inside refire_grace reopens the SAME episode (T8)",
			current:  caseIn(t, StateResolved),
			trigger:  TriggerObserveFiring,
			actor:    ActorIngest,
			recorded: withinGrace,
			want:     Decision{ID: TransitionT8, From: StateResolved},
		},
		{
			name:     "a re-fire inside refire_grace out of an ACKED episode keeps the ack",
			current:  caseIn(t, StateResolved, acked),
			trigger:  TriggerObserveFiring,
			actor:    ActorIngest,
			recorded: withinGrace,
			// DropsAck is false: T8 is the SAME episode, and the acknowledgement
			// that was taken on it still applies to it.
			want: Decision{ID: TransitionT8, From: StateResolved},
		},
		{
			name:     "a re-fire beyond refire_grace opens a NEW episode (T7)",
			current:  caseIn(t, StateResolved, seq(3)),
			trigger:  TriggerObserveFiring,
			actor:    ActorIngest,
			recorded: beyondGrace,
			want: Decision{
				ID: TransitionT7, From: StateResolved,
				OpensEpisode: true, Seq: 4, ReopenOf: caseID,
			},
		},
		{
			name:     "an EXPIRED episode re-fires beyond refire_grace (T7)",
			current:  caseIn(t, StateExpired),
			trigger:  TriggerObserveFiring,
			actor:    ActorIngest,
			recorded: beyondGrace,
			want: Decision{
				ID: TransitionT7, From: StateExpired,
				OpensEpisode: true, Seq: 2, ReopenOf: caseID,
			},
		},
		{
			// T10 by the `new_case` road. It is decided HERE, once, for both
			// rows that open an episode — the two call sites that used to decide it
			// separately had already drifted apart on what to do about it.
			name:     "T7 out of an ACKED episode drops the acknowledgement",
			current:  caseIn(t, StateResolved, acked),
			trigger:  TriggerObserveFiring,
			actor:    ActorIngest,
			recorded: beyondGrace,
			want: Decision{
				ID: TransitionT7, From: StateResolved,
				OpensEpisode: true, Seq: 2, ReopenOf: caseID, DropsAck: true,
			},
		},

		// ------------------------------------------------------- nothing to do
		{
			name:     "a duplicate resolved on a resolved episode transitions nothing",
			current:  caseIn(t, StateResolved),
			trigger:  TriggerObserveResolved,
			actor:    ActorIngest,
			recorded: withinGrace,
			wantErr:  "illegal_transition",
		},
		{
			name:     "the reaper does not expire an episode that has already ended",
			current:  caseIn(t, StateResolved),
			trigger:  TriggerReap,
			actor:    ActorReaper,
			recorded: beyondGrace,
			wantErr:  "illegal_transition",
		},
		{
			name:     "an expired episode is not suppressible",
			current:  caseIn(t, StateExpired),
			trigger:  TriggerObserveSuppressed,
			actor:    ActorReconciler,
			recorded: withinGrace,
			wantErr:  "illegal_transition",
		},
	}
}

// TestDecide_IsTheB3RoutingTable pins one verdict per §B.3 row, from a value.
func TestDecide_IsTheB3RoutingTable(t *testing.T) {
	for _, c := range decideCases(t) {
		t.Run(c.name, func(t *testing.T) {
			d, err := Decide(c.current, c.cmd(t))

			if c.wantErr != "" {
				requireKind(t, err, errs.KindPrecondition, c.wantErr)
				assert.Equal(t, Decision{}, d, "a refused observation decides nothing")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, d)
		})
	}
}

// TestDecide_OpensAnEpisodeExactlyForT1AndT7 is the guard on the column the
// router reads. An edge that starts opening episodes without SPEC §B.3 saying so
// — or T7 quietly ceasing to — fails here rather than in production.
func TestDecide_OpensAnEpisodeExactlyForT1AndT7(t *testing.T) {
	opening := map[TransitionID]bool{TransitionT1: true, TransitionT7: true}

	for _, r := range transitionTable {
		assert.Equal(t, opening[r.id], r.opensNewCase,
			"%s: the table's opensNewCase column disagrees with §B.3", r.id)
	}

	for _, c := range decideCases(t) {
		if c.wantErr != "" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			d, err := Decide(c.current, c.cmd(t))
			require.NoError(t, err)
			require.Equal(t, opening[d.ID], d.OpensEpisode)
			if !d.OpensEpisode {
				assert.Zero(t, d.Seq, "a row that moves an episode opens none")
				assert.Equal(t, uuid.Nil, d.ReopenOf)
				assert.False(t, d.DropsAck)
				return
			}
			// ⭐ T1 AND T7 ARE DECIDED IDENTICALLY, and this is what that means:
			// the same three values, computed the same way, differing only in what
			// the previous episode was. A caller cannot treat them differently
			// because it is not told which one it is holding.
			assert.GreaterOrEqual(t, d.Seq, 1, "a new episode is numbered from 1")
			assert.Equal(t, d.From == StateNone, d.ReopenOf == uuid.Nil,
				"an episode succeeds one exactly when there was one")
			assert.Equal(t, c.current.AckState().IsAcked(), d.DropsAck,
				"an acknowledgement does not survive into a new episode (T10)")
		})
	}
}

// TestDecide_AndApply_NeverDisagreeAboutWhichRowRuns is the anti-divergence pin.
//
// The router and the machine read the SAME row of the SAME table, so a caller
// that asks one and applies the other can never act on two different verdicts.
// This is the property the old code lacked: `observe` answered the T7-versus-T8
// question itself, with its own grace handling, beside a machine that answered it
// again — and the two answers had nowhere to be compared.
func TestDecide_AndApply_NeverDisagreeAboutWhichRowRuns(t *testing.T) {
	for _, c := range decideCases(t) {
		t.Run(c.name, func(t *testing.T) {
			cmd := c.cmd(t)
			d, derr := Decide(c.current, cmd)
			r, aerr := Apply(c.current, cmd)

			if c.wantErr != "" {
				requireKind(t, derr, errs.KindPrecondition, c.wantErr)
				requireKind(t, aerr, errs.KindPrecondition, c.wantErr)
				return
			}
			require.NoError(t, derr)

			if d.OpensEpisode {
				if d.ID == TransitionT1 {
					// Apply refuses to open the first episode, which is the same
					// verdict said the other way round: opening is the caller's job.
					requireKind(t, aerr, errs.KindPrecondition, "no_open_case")
					return
				}
				require.NoError(t, aerr)
				assert.True(t, r.OpensNewCase)
				assert.Equal(t, d.ID, r.ID)
				assert.Equal(t, d.From, r.From)
				return
			}
			require.NoError(t, aerr)
			assert.False(t, r.OpensNewCase)
			assert.Equal(t, d.ID, r.ID)
			assert.Equal(t, d.From, r.From)
		})
	}
}
