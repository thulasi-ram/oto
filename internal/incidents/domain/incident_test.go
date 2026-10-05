package domain

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// TestStateIsReadOffTheOpenMembers is ADR 0052 §3 in one table: active while any
// current member Case is open, quiet otherwise — including the Incident whose every
// member was removed, which has nothing open and therefore nothing to be active
// about.
func TestStateIsReadOffTheOpenMembers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		open int
		want State
	}{
		{"one open member", 1, StateActive},
		{"several open members", 7, StateActive},
		{"every member closed", 0, StateQuiet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, StateOf(tc.open))
			assert.Equal(t, tc.want, Incident{OpenMemberCount: tc.open, MemberCount: tc.open + 2}.State())
		})
	}

	// The membership size does not enter into it: ten closed members are quiet.
	assert.Equal(t, StateQuiet, Incident{MemberCount: 10}.State())
	assert.Equal(t, []State{StateActive, StateQuiet}, AllStates())
}

// TestAnAttributionNamesExactlyOneAuthor — ADR 0052 §2's "two authors, and a model
// is not one of them", as the constructor and the row restorer enforce it.
func TestAnAttributionNamesExactlyOneAuthor(t *testing.T) {
	t.Parallel()

	user := uuid.New()
	h, err := Human(user, "  alice  ")
	require.NoError(t, err)
	assert.True(t, h.IsHuman())
	assert.Equal(t, "alice", h.Label(), "the label is trimmed before it is frozen")
	assert.Equal(t, user, h.UserID())
	assert.False(t, h.IsZero())

	_, err = Human(user, "   ")
	require.Error(t, err, "a decision nobody signed is not attributable")
	assert.Equal(t, errs.KindValidation, errs.KindOf(err))

	long, err := Human(user, strings.Repeat("é", MaxActorLabelBytes))
	require.NoError(t, err, "a long name is clipped, never refused: the person still acted")
	assert.LessOrEqual(t, len(long.Label()), MaxActorLabelBytes)
	assert.True(t, strings.HasSuffix(long.Label(), "é"), "clipping must not split a rune")

	corr := uuid.New()
	c, err := Restore(uuid.Nil, "", corr)
	require.NoError(t, err)
	assert.False(t, c.IsHuman())
	assert.Equal(t, corr, c.CorrelatorID())

	_, err = Restore(user, "alice", corr)
	require.Error(t, err, "both authors at once is a row incidents_drawn_by_ck refuses")
	_, err = Restore(uuid.Nil, "", uuid.Nil)
	require.Error(t, err, "no author at all is a row incidents_drawn_by_ck refuses")

	// A user deleted since keeps the decision: the label survives the nulled id.
	gone, err := Restore(uuid.Nil, "alice", uuid.Nil)
	require.NoError(t, err)
	assert.True(t, gone.IsHuman())
	assert.Equal(t, "alice", gone.Label())
}

// TestTheAtMostOneRefusalPointsAtTheMove — ADR 0052 §4. The refusal is a 409 the
// caller can act on: it names the holding Incident and the exact move request.
func TestTheAtMostOneRefusalPointsAtTheMove(t *testing.T) {
	t.Parallel()

	caseID := uuid.New()
	err := CaseInIncident(CaseRef{ID: caseID, Number: 12}, Ref{ID: uuid.New(), Number: 4})
	assert.Equal(t, errs.KindConflict, errs.KindOf(err))
	assert.Equal(t, "case_in_incident", errs.CodeOf(err))
	assert.Contains(t, err.Error(), "Case #12 is already in Incident #4")
	assert.Contains(t, err.Error(), "POST /api/v1/incidents/4/cases/"+caseID.String()+"/move")

	// A lost race says the same thing in the same code, so a client branches once.
	assert.Equal(t, "case_in_incident", errs.CodeOf(RaceLostToAnotherIncident()))

	assert.Equal(t, "already_a_member", errs.CodeOf(AlreadyAMember(CaseRef{Number: 12}, Ref{Number: 4})))
	assert.Equal(t, errs.KindNotFound, errs.KindOf(NotFound()))
	assert.Equal(t, errs.KindNotFound, errs.KindOf(MemberNotFound()))
	assert.Equal(t, errs.KindValidation, errs.KindOf(MoveToSelf()))
}

// TestAMemberIsCurrentUntilItIsTombstoned — a removed Case stays recorded, as a
// tombstone, rather than disappearing.
func TestAMemberIsCurrentUntilItIsTombstoned(t *testing.T) {
	t.Parallel()

	m := Member{CaseNumber: 12}
	assert.True(t, m.Current())
	m.RemovedAt = m.AddedAt.Add(1)
	m.RemovedByLabel = "alice"
	assert.False(t, m.Current())
}

// TestTheStateEdgesAreTheOnlyStateFacts — ADR 0052 §5. `quiet` and `active_again`
// are declared exactly when the derived state MOVES, and a membership change that
// leaves it where it was declares nothing about state.
func TestTheStateEdgesAreTheOnlyStateFacts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		before, after int
		want          Fact
		moved         bool
	}{
		{"the last open member left or closed", 1, 0, FactQuiet, true},
		{"an open member joined a quiet Incident", 0, 1, FactActiveAgain, true},
		{"one of several open members left", 3, 2, "", false},
		{"a closed member joined a quiet Incident", 0, 0, "", false},
		{"an open member joined an active Incident", 1, 2, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, moved := Transition(tc.before, tc.after)
			assert.Equal(t, tc.moved, moved)
			assert.Equal(t, tc.want, got)
		})
	}

	// ⛔ No fact means resolve or close.
	for _, f := range []Fact{FactDrawn, FactCaseAdded, FactCaseRemoved, FactQuiet, FactActiveAgain} {
		assert.NotContains(t, string(f), "resolve")
		assert.NotContains(t, string(f), "close")
	}
}
