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

func validCaseParams() CaseParams {
	return CaseParams{
		ID:             caseID,
		OrgID:          orgA,
		AlertID:        alertID,
		Seq:            1,
		State:          CaseOpen,
		StartedAt:      t0,
		LastObservedAt: t0,
		SourceStartsAt: t0,
		AckState:       AckStateUnacked,
	}
}

func TestNewCase_RequiredFields(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*CaseParams)
		kind errs.Kind
		code string
	}{
		{name: "no id", mut: func(p *CaseParams) { p.ID = uuid.Nil }, kind: errs.KindValidation, code: "required"},
		{name: "no org", mut: func(p *CaseParams) { p.OrgID = uuid.Nil }, kind: errs.KindValidation, code: "required"},
		{name: "no alert", mut: func(p *CaseParams) { p.AlertID = uuid.Nil }, kind: errs.KindValidation, code: "required"},
		{name: "seq zero", mut: func(p *CaseParams) { p.Seq = 0 }, kind: errs.KindValidation, code: "min"},
		{name: "seq negative", mut: func(p *CaseParams) { p.Seq = -1 }, kind: errs.KindValidation, code: "min"},
		{name: "negative suppress count", mut: func(p *CaseParams) { p.SuppressCount = -1 }, kind: errs.KindValidation, code: "min"},
		{name: "negative state version", mut: func(p *CaseParams) { p.StateVersion = -1 }, kind: errs.KindValidation, code: "min"},
		{name: "no state", mut: func(p *CaseParams) { p.State = CaseState{} }, kind: errs.KindValidation, code: "required"},
		{name: "no started_at", mut: func(p *CaseParams) { p.StartedAt = time.Time{} }, kind: errs.KindValidation, code: "required"},
		{name: "no last_observed_at", mut: func(p *CaseParams) { p.LastObservedAt = time.Time{} }, kind: errs.KindValidation, code: "required"},
		{name: "no source_starts_at", mut: func(p *CaseParams) { p.SourceStartsAt = time.Time{} }, kind: errs.KindValidation, code: "required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validCaseParams()
			tc.mut(&p)
			_, err := NewCase(p)
			requireKind(t, err, tc.kind, tc.code)
		})
	}

	t.Run("happy path", func(t *testing.T) {
		o, err := NewCase(validCaseParams())
		require.NoError(t, err)
		assert.Equal(t, caseID, o.ID())
		assert.Equal(t, orgA, o.OrgID())
		assert.Equal(t, alertID, o.AlertID())
		assert.Equal(t, 1, o.Seq())
		assert.Equal(t, CaseOpen, o.State())
		assert.Equal(t, StateFiring, o.AlertState(),
			"an open episode with no suppression reason reads back as a firing alert")
		assert.Equal(t, AckStateUnacked, o.AckState())
		assert.True(t, o.IsOpen())
	})
}

// TestNewCase_AClosedCaseAlwaysSaysWhyItClosed is what stops oto ever claiming
// "resolved" when it means "expired".
//
// ⭐⭐ THE GUARANTEE MOVED, IT DID NOT WEAKEN (ADR 0040). It used to be a MAP:
// `state='resolved'` was locked to `resolve_reason='upstream'` and
// `state='expired'` to `'timeout'`, two columns spelling one fact and a CHECK
// keeping them in step. `alert_cases.state` says only `closed` now, so there is
// no second spelling to disagree with — and `resolve_reason` becomes REQUIRED on
// every closed episode, which is what makes `AlertState()` able to answer at all.
// A closed case that said nothing about why would be the exact failure the old
// map existed to prevent, so the cases below prove it cannot be built.
func TestNewCase_AClosedCaseAlwaysSaysWhyItClosed(t *testing.T) {
	ended := t0.Add(time.Minute)

	tests := []struct {
		name    string
		state   CaseState
		ended   time.Time
		reason  ResolveReason
		want    State
		wantErr string
	}{
		{name: "closed + upstream reads as resolved", state: CaseClosed, ended: ended, reason: ResolveUpstream, want: StateResolved},
		{name: "closed + timeout reads as expired", state: CaseClosed, ended: ended, reason: ResolveTimeout, want: StateExpired},

		{name: "⛔ closed without a reason", state: CaseClosed, ended: ended, wantErr: "case_resolve_reason"},
		{name: "⛔ open with a reason", state: CaseOpen, reason: ResolveUpstream, wantErr: "case_resolve_reason"},
		{name: "⛔ closed without ended_at", state: CaseClosed, reason: ResolveUpstream, wantErr: "case_terminal_ended"},
		{name: "⛔ open with an ended_at", state: CaseOpen, ended: ended, wantErr: "case_terminal_ended"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validCaseParams()
			p.State = tc.state
			p.EndedAt = tc.ended
			p.ResolveReason = tc.reason

			o, err := NewCase(p)
			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.False(t, o.IsOpen())
				assert.Equal(t, tc.reason, o.ResolveReason())
				assert.Equal(t, tc.want, o.AlertState(),
					"resolve_reason is the SOLE record of resolved-versus-expired now")
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

// TestNewCase_SuppressionReasonExistsOnlyWhileTheEpisodeIsOpen is the surviving
// half of the old biconditional, and the reading that replaced the other half.
//
// ⭐ `suppressed` IS NOT A STATE ANY MORE, IT IS A READING. An open episode with a
// suppression reason IS a suppressed alert — that is the definition `AlertState`
// applies — so "open implies a reason" was never true and "suppressed implies a
// reason" became a tautology. What can still be false, and is still checked, is a
// reason left behind on an episode that has ENDED: that would make oto keep
// saying "silenced by <id>" about a firing that is over.
//
// ⚠️ The DDL says the same thing and no more: `case_suppress_ck` is
// `suppression_reason IS NULL OR state = 'open'` since migration 00054.
func TestNewCase_SuppressionReasonExistsOnlyWhileTheEpisodeIsOpen(t *testing.T) {
	ended := t0.Add(time.Minute)

	tests := []struct {
		name    string
		state   CaseState
		reason  SuppressionReason
		want    State
		wantErr bool
	}{
		// ⭐ ADR 0041: AlertState reads `firing` WITH OR WITHOUT a reason, because
		// suppression is an axis and not a state. `SuppressionReason()` is where the
		// silence is read from, and the two are asserted independently below.
		{name: "open with a reason still reads as firing", state: CaseOpen, reason: SuppressionSilence, want: StateFiring},
		{name: "open without one reads as firing", state: CaseOpen, want: StateFiring},
		{name: "⛔ closed with a reason", state: CaseClosed, reason: SuppressionSilence, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validCaseParams()
			p.State = tc.state
			p.SuppressionReason = tc.reason
			if tc.state.IsClosed() {
				p.EndedAt = ended
				p.ResolveReason = ResolveUpstream
			}

			o, err := NewCase(p)
			if !tc.wantErr {
				require.NoError(t, err)
				assert.Equal(t, tc.reason, o.SuppressionReason())
				assert.Equal(t, tc.want, o.AlertState())
				return
			}
			requireKind(t, err, errs.KindInternal, "case_suppression")
		})
	}
}

func TestNewCase_TimeOrdering(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*CaseParams)
		code string
	}{
		{
			name: "ended_at before started_at",
			mut: func(p *CaseParams) {
				p.State = CaseClosed
				p.ResolveReason = ResolveUpstream
				p.EndedAt = t0.Add(-time.Second)
			},
			code: "case_order",
		},
		{
			name: "last_observed_at before started_at",
			mut:  func(p *CaseParams) { p.LastObservedAt = t0.Add(-time.Second) },
			code: "case_observed_order",
		},
		{
			name: "source_ends_at before source_starts_at",
			mut:  func(p *CaseParams) { p.SourceEndsAt = t0.Add(-time.Second) },
			code: "case_source_order",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validCaseParams()
			tc.mut(&p)
			_, err := NewCase(p)
			requireKind(t, err, errs.KindInternal, tc.code)
		})
	}
}

func TestNewCase_AckFieldsAreAllOrNothing(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*CaseParams)
		kind errs.Kind
		code string
	}{
		{
			name: "acked without an acked_at",
			mut:  func(p *CaseParams) { p.AckState = AckStateAcked },
			kind: errs.KindInternal, code: "case_ack",
		},
		{
			name: "acked_at without an ack state",
			mut:  func(p *CaseParams) { p.AckedAt = t0 },
			kind: errs.KindInternal, code: "case_ack",
		},
		{
			name: "acked without a label",
			mut: func(p *CaseParams) {
				p.AckState = AckStateAcked
				p.AckedAt = t0
			},
			kind: errs.KindInternal, code: "case_ack_label",
		},
		{
			name: "acked_at before started_at",
			mut: func(p *CaseParams) {
				p.AckState = AckStateAcked
				p.AckedAt = t0.Add(-time.Second)
				p.AckedByLabel = "Ram"
			},
			kind: errs.KindInternal, code: "case_ack_order",
		},
		{
			name: "ack note over the bound",
			mut: func(p *CaseParams) {
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
			p := validCaseParams()
			tc.mut(&p)
			_, err := NewCase(p)
			requireKind(t, err, tc.kind, tc.code)
		})
	}

	t.Run("a whitespace-only label is treated as absent", func(t *testing.T) {
		p := validCaseParams()
		p.AckState = AckStateAcked
		p.AckedAt = t0
		p.AckedByLabel = "   "
		_, err := NewCase(p)
		requireKind(t, err, errs.KindInternal, "case_ack_label")
	})
}

func TestNewCase_Defaults(t *testing.T) {
	p := validCaseParams()
	p.AckState = AckState{}
	p.StateVersion = 0

	o, err := NewCase(p)
	require.NoError(t, err)
	assert.Equal(t, AckStateUnacked, o.AckState(), "an unset ack state rehydrates as unacked")
	assert.Equal(t, 1, o.StateVersion(), "a zero state_version rehydrates as the column DEFAULT")
	assert.Equal(t, 1, PreconditionFor(o).StateVersion)
}

func TestNewCase_NormalisesToUTC(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	p := validCaseParams()
	p.State = CaseClosed
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

	o, err := NewCase(p)
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

// TestCase_SuppressedByIsGatedOnTheState — witnesses left behind on an
// case that is demonstrably firing would make oto keep saying
// "silenced by <id>" about an alert nobody is silencing.
func TestCase_SuppressedByIsGatedOnTheState(t *testing.T) {
	witnesses := SuppressedBy{SilencedBy: []string{"sil-1"}, MutedBy: []string{"mute-2"}}

	suppressed := caseIn(t, StateSuppressed, func(p *CaseParams) {
		p.SuppressedBy = witnesses
	})
	assert.Equal(t, witnesses, suppressed.SuppressedBy())

	for _, state := range []State{StateFiring, StateResolved, StateExpired} {
		t.Run("hidden while "+state.String(), func(t *testing.T) {
			o := caseIn(t, state, func(p *CaseParams) { p.SuppressedBy = witnesses })
			assert.True(t, o.SuppressedBy().IsZero(),
				"a row written before the persistence path cleared the column must read the same way")
		})
	}
}

func TestCase_ValueIsCopiedOut(t *testing.T) {
	v := 3.14
	o := caseIn(t, StateFiring, func(p *CaseParams) { p.Value = &v })

	got := o.Value()
	require.NotNil(t, got)
	assert.Equal(t, 3.14, *got)

	*got = 99
	assert.Equal(t, 3.14, *o.Value(), "the accessor hands out a copy")

	assert.Nil(t, caseIn(t, StateFiring).Value())
}

func TestCase_Duration(t *testing.T) {
	open := caseIn(t, StateFiring)
	assert.Equal(t, 30*time.Minute, open.Duration(t0.Add(30*time.Minute)),
		"an open episode is measured to the clock reading the caller supplies")

	closed := caseIn(t, StateResolved, func(p *CaseParams) {
		p.EndedAt = t0.Add(7 * time.Minute)
	})
	assert.Equal(t, 7*time.Minute, closed.Duration(t0.Add(time.Hour)),
		"a closed episode ignores the asOf reading entirely")
}

// ⛔ THIS WAS `TestCase_WithGroupAndRuleSnapshot` AND ITS GROUP HALF IS DELETED
// (git-bug `7570090`). That half proved three things about `WithGroup`: that a Case
// could be built unbound, that binding returned a new Case carrying the generation
// id, and that binding to `uuid.Nil` was REFUSED. All three were statements about a
// late binding the ingest orchestrator performed, and both the method and the field
// it set are gone — a Case IS the conversation and joins nothing.
//
// ⭐ WHAT THE HALF THAT REMAINS PROVES IS THE PART THAT WAS NEVER ABOUT THE GROUP:
// `With*` binders do not mutate their receiver, and they refuse a nil id rather
// than storing one. `WithRuleSnapshot` is the surviving binder and it carries both
// claims on its own, which is why deleting the group half costs no coverage.
func TestCase_WithRuleSnapshot(t *testing.T) {
	o := caseIn(t, StateFiring)

	withRule, err := o.WithRuleSnapshot(snapshotFix)
	require.NoError(t, err)
	assert.Equal(t, snapshotFix, withRule.RuleSnapshotID())
	assert.Equal(t, uuid.Nil, o.RuleSnapshotID(), "the receiver is not mutated")

	_, err = o.WithRuleSnapshot(uuid.Nil)
	requireKind(t, err, errs.KindValidation, "required")
}

func TestCase_IsOpenTracksEndedAtNotState(t *testing.T) {
	assert.True(t, caseIn(t, StateFiring).IsOpen())
	assert.True(t, caseIn(t, StateSuppressed).IsOpen(),
		"suppressed is an OPEN state: the alert is still active, merely muted upstream")
	assert.False(t, caseIn(t, StateResolved).IsOpen())
	assert.False(t, caseIn(t, StateExpired).IsOpen())
}
