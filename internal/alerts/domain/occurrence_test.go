package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

func validOccurrenceParams() OccurrenceParams {
	return OccurrenceParams{
		ID:             occID,
		OrgID:          orgA,
		AlertID:        alertID,
		GroupID:        groupIDFix,
		Seq:            1,
		State:          StateFiring,
		StartedAt:      t0,
		LastObservedAt: t0,
		SourceStartsAt: t0,
		AckState:       AckStateUnacked,
	}
}

func TestNewOccurrence_RequiredFields(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*OccurrenceParams)
		kind errs.Kind
		code string
	}{
		{name: "no id", mut: func(p *OccurrenceParams) { p.ID = uuid.Nil }, kind: errs.KindValidation, code: "required"},
		{name: "no org", mut: func(p *OccurrenceParams) { p.OrgID = uuid.Nil }, kind: errs.KindValidation, code: "required"},
		{name: "no alert", mut: func(p *OccurrenceParams) { p.AlertID = uuid.Nil }, kind: errs.KindValidation, code: "required"},
		{name: "seq zero", mut: func(p *OccurrenceParams) { p.Seq = 0 }, kind: errs.KindValidation, code: "min"},
		{name: "seq negative", mut: func(p *OccurrenceParams) { p.Seq = -1 }, kind: errs.KindValidation, code: "min"},
		{name: "negative reopen count", mut: func(p *OccurrenceParams) { p.ReopenCount = -1 }, kind: errs.KindValidation, code: "min"},
		{name: "negative suppress count", mut: func(p *OccurrenceParams) { p.SuppressCount = -1 }, kind: errs.KindValidation, code: "min"},
		{name: "negative state version", mut: func(p *OccurrenceParams) { p.StateVersion = -1 }, kind: errs.KindValidation, code: "min"},
		{name: "no state", mut: func(p *OccurrenceParams) { p.State = State{} }, kind: errs.KindValidation, code: "required"},
		{name: "no started_at", mut: func(p *OccurrenceParams) { p.StartedAt = time.Time{} }, kind: errs.KindValidation, code: "required"},
		{name: "no last_observed_at", mut: func(p *OccurrenceParams) { p.LastObservedAt = time.Time{} }, kind: errs.KindValidation, code: "required"},
		{name: "no source_starts_at", mut: func(p *OccurrenceParams) { p.SourceStartsAt = time.Time{} }, kind: errs.KindValidation, code: "required"},
		{name: "reopens itself", mut: func(p *OccurrenceParams) { p.ReopenOf = p.ID }, kind: errs.KindValidation, code: "field_order"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validOccurrenceParams()
			tc.mut(&p)
			_, err := NewOccurrence(p)
			requireKind(t, err, tc.kind, tc.code)
		})
	}

	t.Run("happy path", func(t *testing.T) {
		o, err := NewOccurrence(validOccurrenceParams())
		require.NoError(t, err)
		assert.Equal(t, occID, o.ID())
		assert.Equal(t, orgA, o.OrgID())
		assert.Equal(t, alertID, o.AlertID())
		assert.Equal(t, groupIDFix, o.GroupID())
		assert.Equal(t, 1, o.Seq())
		assert.Equal(t, StateFiring, o.State())
		assert.Equal(t, AckStateUnacked, o.AckState())
		assert.True(t, o.IsOpen())
	})
}

// TestNewOccurrence_TerminalStateAndReasonAreBoundOneToOne is what stops oto ever
// claiming "resolved" when it means "expired".
func TestNewOccurrence_TerminalStateAndReasonAreBoundOneToOne(t *testing.T) {
	ended := t0.Add(time.Minute)

	tests := []struct {
		name    string
		state   State
		ended   time.Time
		reason  ResolveReason
		wantErr string
	}{
		{name: "resolved + upstream", state: StateResolved, ended: ended, reason: ResolveUpstream},
		{name: "expired + timeout", state: StateExpired, ended: ended, reason: ResolveTimeout},

		{name: "⛔ resolved + timeout", state: StateResolved, ended: ended, reason: ResolveTimeout, wantErr: "occurrence_resolve_map"},
		{name: "⛔ expired + upstream", state: StateExpired, ended: ended, reason: ResolveUpstream, wantErr: "occurrence_resolve_map"},
		{name: "resolved without a reason", state: StateResolved, ended: ended, wantErr: "occurrence_resolve_reason"},
		{name: "firing with a reason", state: StateFiring, reason: ResolveUpstream, wantErr: "occurrence_resolve_reason"},
		{name: "resolved without ended_at", state: StateResolved, reason: ResolveUpstream, wantErr: "occurrence_terminal_ended"},
		{name: "firing with an ended_at", state: StateFiring, ended: ended, wantErr: "occurrence_terminal_ended"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validOccurrenceParams()
			p.State = tc.state
			p.EndedAt = tc.ended
			p.ResolveReason = tc.reason

			o, err := NewOccurrence(p)
			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.False(t, o.IsOpen())
				assert.Equal(t, tc.reason, o.ResolveReason())
				return
			}
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, errs.KindInternal, e.Kind,
				"an impossible combination is a programming bug, not a caller error")
			assert.Equal(t, tc.wantErr, e.Code)
		})
	}
}

func TestNewOccurrence_SuppressionReasonExistsOnlyWhileSuppressed(t *testing.T) {
	tests := []struct {
		name    string
		state   State
		reason  SuppressionReason
		wantErr bool
	}{
		{name: "suppressed with a reason", state: StateSuppressed, reason: SuppressionSilence},
		{name: "firing without one", state: StateFiring},
		{name: "suppressed without a reason", state: StateSuppressed, wantErr: true},
		{name: "firing with a reason", state: StateFiring, reason: SuppressionSilence, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validOccurrenceParams()
			p.State = tc.state
			p.SuppressionReason = tc.reason

			o, err := NewOccurrence(p)
			if !tc.wantErr {
				require.NoError(t, err)
				assert.Equal(t, tc.reason, o.SuppressionReason())
				return
			}
			requireKind(t, err, errs.KindInternal, "occurrence_suppression")
		})
	}
}

func TestNewOccurrence_TimeOrdering(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*OccurrenceParams)
		code string
	}{
		{
			name: "ended_at before started_at",
			mut: func(p *OccurrenceParams) {
				p.State = StateResolved
				p.ResolveReason = ResolveUpstream
				p.EndedAt = t0.Add(-time.Second)
			},
			code: "occurrence_order",
		},
		{
			name: "last_observed_at before started_at",
			mut:  func(p *OccurrenceParams) { p.LastObservedAt = t0.Add(-time.Second) },
			code: "occurrence_observed_order",
		},
		{
			name: "source_ends_at before source_starts_at",
			mut:  func(p *OccurrenceParams) { p.SourceEndsAt = t0.Add(-time.Second) },
			code: "occurrence_source_order",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validOccurrenceParams()
			tc.mut(&p)
			_, err := NewOccurrence(p)
			requireKind(t, err, errs.KindInternal, tc.code)
		})
	}
}

func TestNewOccurrence_AckFieldsAreAllOrNothing(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*OccurrenceParams)
		kind errs.Kind
		code string
	}{
		{
			name: "acked without an acked_at",
			mut:  func(p *OccurrenceParams) { p.AckState = AckStateAcked },
			kind: errs.KindInternal, code: "occurrence_ack",
		},
		{
			name: "acked_at without an ack state",
			mut:  func(p *OccurrenceParams) { p.AckedAt = t0 },
			kind: errs.KindInternal, code: "occurrence_ack",
		},
		{
			name: "acked without a label",
			mut: func(p *OccurrenceParams) {
				p.AckState = AckStateAcked
				p.AckedAt = t0
			},
			kind: errs.KindInternal, code: "occurrence_ack_label",
		},
		{
			name: "acked_at before started_at",
			mut: func(p *OccurrenceParams) {
				p.AckState = AckStateAcked
				p.AckedAt = t0.Add(-time.Second)
				p.AckedByLabel = "Ram"
			},
			kind: errs.KindInternal, code: "occurrence_ack_order",
		},
		{
			name: "ack note over the bound",
			mut: func(p *OccurrenceParams) {
				p.AckState = AckStateAcked
				p.AckedAt = t0
				p.AckedByLabel = "Ram"
				p.AckNote = strings.Repeat("n", MaxAckNoteBytes+1)
			},
			kind: errs.KindValidation, code: "max_length",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validOccurrenceParams()
			tc.mut(&p)
			_, err := NewOccurrence(p)
			requireKind(t, err, tc.kind, tc.code)
		})
	}

	t.Run("a whitespace-only label is treated as absent", func(t *testing.T) {
		p := validOccurrenceParams()
		p.AckState = AckStateAcked
		p.AckedAt = t0
		p.AckedByLabel = "   "
		_, err := NewOccurrence(p)
		requireKind(t, err, errs.KindInternal, "occurrence_ack_label")
	})
}

func TestNewOccurrence_Defaults(t *testing.T) {
	p := validOccurrenceParams()
	p.AckState = AckState{}
	p.StateVersion = 0

	o, err := NewOccurrence(p)
	require.NoError(t, err)
	assert.Equal(t, AckStateUnacked, o.AckState(), "an unset ack state rehydrates as unacked")
	assert.Equal(t, 1, o.StateVersion(), "a zero state_version rehydrates as the column DEFAULT")
	assert.Equal(t, 1, PreconditionFor(o).StateVersion)
}

func TestNewOccurrence_NormalisesToUTC(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	p := validOccurrenceParams()
	p.State = StateResolved
	p.ResolveReason = ResolveUpstream
	p.StartedAt = t0.In(ist)
	p.LastObservedAt = t0.In(ist)
	p.SourceStartsAt = t0.In(ist)
	p.EndedAt = t0.Add(time.Minute).In(ist)
	p.SourceEndsAt = t0.Add(time.Minute).In(ist)
	p.SourceUpdatedAt = t0.In(ist)
	p.AckState = AckStateAcked
	p.AckedAt = t0.Add(time.Second).In(ist)
	p.AckedByLabel = "Ram"

	o, err := NewOccurrence(p)
	require.NoError(t, err)
	for name, got := range map[string]time.Time{
		"started_at":        o.StartedAt(),
		"ended_at":          o.EndedAt(),
		"last_observed_at":  o.LastObservedAt(),
		"source_starts_at":  o.SourceStartsAt(),
		"source_ends_at":    o.SourceEndsAt(),
		"source_updated_at": o.SourceUpdatedAt(),
		"acked_at":          o.AckedAt(),
	} {
		assert.Equal(t, time.UTC, got.Location(), "%s", name)
	}
}

// TestOccurrence_SuppressedByIsGatedOnTheState — witnesses left behind on an
// occurrence that is demonstrably firing would make oto keep saying
// "silenced by <id>" about an alert nobody is silencing.
func TestOccurrence_SuppressedByIsGatedOnTheState(t *testing.T) {
	witnesses := SuppressedBy{SilencedBy: []string{"sil-1"}, MutedBy: []string{"mute-2"}}

	suppressed := occurrenceIn(t, StateSuppressed, func(p *OccurrenceParams) {
		p.SuppressedBy = witnesses
	})
	assert.Equal(t, witnesses, suppressed.SuppressedBy())

	for _, state := range []State{StateFiring, StateResolved, StateExpired} {
		t.Run("hidden while "+state.String(), func(t *testing.T) {
			o := occurrenceIn(t, state, func(p *OccurrenceParams) { p.SuppressedBy = witnesses })
			assert.True(t, o.SuppressedBy().IsZero(),
				"a row written before the persistence path cleared the column must read the same way")
		})
	}
}

func TestOccurrence_ValueIsCopiedOut(t *testing.T) {
	v := 3.14
	o := occurrenceIn(t, StateFiring, func(p *OccurrenceParams) { p.Value = &v })

	got := o.Value()
	require.NotNil(t, got)
	assert.Equal(t, 3.14, *got)

	*got = 99
	assert.Equal(t, 3.14, *o.Value(), "the accessor hands out a copy")

	assert.Nil(t, occurrenceIn(t, StateFiring).Value())
}

func TestOccurrence_Duration(t *testing.T) {
	open := occurrenceIn(t, StateFiring)
	assert.Equal(t, 30*time.Minute, open.Duration(t0.Add(30*time.Minute)),
		"an open episode is measured to the clock reading the caller supplies")

	closed := occurrenceIn(t, StateResolved, func(p *OccurrenceParams) {
		p.EndedAt = t0.Add(7 * time.Minute)
	})
	assert.Equal(t, 7*time.Minute, closed.Duration(t0.Add(time.Hour)),
		"a closed episode ignores the asOf reading entirely")
}

func TestOccurrence_WithGroupAndRuleSnapshot(t *testing.T) {
	o := occurrenceIn(t, StateFiring, func(p *OccurrenceParams) { p.GroupID = uuid.Nil })
	assert.Equal(t, uuid.Nil, o.GroupID())

	bound, err := o.WithGroup(groupIDFix)
	require.NoError(t, err)
	assert.Equal(t, groupIDFix, bound.GroupID())
	assert.Equal(t, uuid.Nil, o.GroupID(), "the receiver is not mutated")

	_, err = o.WithGroup(uuid.Nil)
	requireKind(t, err, errs.KindValidation, "required")

	withRule, err := o.WithRuleSnapshot(snapshotFix)
	require.NoError(t, err)
	assert.Equal(t, snapshotFix, withRule.RuleSnapshotID())
	assert.Equal(t, uuid.Nil, o.RuleSnapshotID())

	_, err = o.WithRuleSnapshot(uuid.Nil)
	requireKind(t, err, errs.KindValidation, "required")
}

func TestOccurrence_IsOpenTracksEndedAtNotState(t *testing.T) {
	assert.True(t, occurrenceIn(t, StateFiring).IsOpen())
	assert.True(t, occurrenceIn(t, StateSuppressed).IsOpen(),
		"suppressed is an OPEN state: the alert is still active, merely muted upstream")
	assert.False(t, occurrenceIn(t, StateResolved).IsOpen())
	assert.False(t, occurrenceIn(t, StateExpired).IsOpen())
}
