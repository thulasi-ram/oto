package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/rules/domain"
)

// snapAt is a snapshot of one text captured at one instant.
func snapAt(expr string, at time.Time) domain.Snapshot {
	s := snap(expr, 0, 0, nil, nil)
	s.CapturedAt = at
	return s
}

func TestNewHistoryNumbersOldestFirst(t *testing.T) {
	t.Parallel()

	t0 := capturedAt
	v1 := snapAt("a > 1", t0)
	v2 := snapAt("a > 2", t0.Add(time.Hour))
	v3 := snapAt("a > 3", t0.Add(2*time.Hour))

	// Deliberately shuffled: the caller's order must not decide the numbering.
	h := domain.NewHistory(validKey(), []domain.Snapshot{v3, v1, v2})

	require.Equal(t, 3, h.Len())
	assert.Equal(t, validKey(), h.Key)

	assert.Equal(t, 1, h.Versions[0].Number)
	assert.Equal(t, "a > 1", h.Versions[0].Snapshot.Expr)
	assert.Equal(t, 2, h.Versions[0].SupersededBy)

	assert.Equal(t, 2, h.Versions[1].Number)
	assert.Equal(t, "a > 2", h.Versions[1].Snapshot.Expr)
	assert.Equal(t, 3, h.Versions[1].SupersededBy)

	assert.Equal(t, 3, h.Versions[2].Number)
	assert.Equal(t, "a > 3", h.Versions[2].Snapshot.Expr)
	assert.Equal(t, 0, h.Versions[2].SupersededBy, "the newest version is superseded by nothing")
}

// TestNewHistoryTieBreaksOnTheContentAddress: a history that renumbers itself
// between two reads is worse than no history, so two rows captured in the same
// instant still order deterministically.
func TestNewHistoryTieBreaksOnTheContentAddress(t *testing.T) {
	t.Parallel()

	a := snapAt("a > 1", capturedAt)
	b := snapAt("a > 2", capturedAt)
	c := snapAt("a > 3", capturedAt)

	first := domain.NewHistory(validKey(), []domain.Snapshot{a, b, c})
	for _, order := range [][]domain.Snapshot{
		{c, b, a}, {b, a, c}, {a, c, b}, {c, a, b},
	} {
		got := domain.NewHistory(validKey(), order)
		require.Equal(t, first, got)
	}

	// And the tiebreak really is the fingerprint, ascending.
	for i := 1; i < first.Len(); i++ {
		assert.Less(t, first.Versions[i-1].Snapshot.Fingerprint, first.Versions[i].Snapshot.Fingerprint)
	}
}

func TestNewHistoryDoesNotMutateItsInput(t *testing.T) {
	t.Parallel()

	in := []domain.Snapshot{
		snapAt("a > 3", capturedAt.Add(2*time.Hour)),
		snapAt("a > 1", capturedAt),
	}
	before := []domain.Snapshot{in[0], in[1]}

	domain.NewHistory(validKey(), in)
	assert.Equal(t, before, in, "the caller's slice must survive the call unchanged")
}

func TestNewHistoryOfNothing(t *testing.T) {
	t.Parallel()

	for _, in := range [][]domain.Snapshot{nil, {}} {
		h := domain.NewHistory(validKey(), in)
		assert.Equal(t, 0, h.Len())
		_, ok := h.Latest()
		assert.False(t, ok)
		_, ok = h.At(1)
		assert.False(t, ok)
		_, ok = h.ByFingerprint("anything")
		assert.False(t, ok)
		assert.False(t, h.Drifted("anything"))
	}
}

func TestHistoryLatest(t *testing.T) {
	t.Parallel()

	h := domain.NewHistory(validKey(), []domain.Snapshot{
		snapAt("a > 1", capturedAt),
		snapAt("a > 2", capturedAt.Add(time.Hour)),
	})

	v, ok := h.Latest()
	require.True(t, ok)
	assert.Equal(t, 2, v.Number)
	assert.Equal(t, "a > 2", v.Snapshot.Expr)
}

func TestHistoryAt(t *testing.T) {
	t.Parallel()

	h := domain.NewHistory(validKey(), []domain.Snapshot{
		snapAt("a > 1", capturedAt),
		snapAt("a > 2", capturedAt.Add(time.Hour)),
		snapAt("a > 3", capturedAt.Add(2*time.Hour)),
	})

	cases := []struct {
		name   string
		number int
		ok     bool
		expr   string
	}{
		{name: "zero is below the 1-based floor", number: 0},
		{name: "negative", number: -1},
		{name: "the first version", number: 1, ok: true, expr: "a > 1"},
		{name: "the last version", number: 3, ok: true, expr: "a > 3"},
		{name: "one past the end", number: 4},
		{name: "far past the end", number: 99},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, ok := h.At(tc.number)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.number, v.Number)
				assert.Equal(t, tc.expr, v.Snapshot.Expr)
			} else {
				assert.Equal(t, domain.Version{}, v)
			}
		})
	}
}

// TestHistoryByFingerprint is how an occurrence's bound snapshot is located
// within the history.
func TestHistoryByFingerprint(t *testing.T) {
	t.Parallel()

	s1 := snapAt("a > 1", capturedAt)
	s2 := snapAt("a > 2", capturedAt.Add(time.Hour))
	h := domain.NewHistory(validKey(), []domain.Snapshot{s1, s2})

	v, ok := h.ByFingerprint(s1.Fingerprint)
	require.True(t, ok)
	assert.Equal(t, 1, v.Number)

	v, ok = h.ByFingerprint(s2.Fingerprint)
	require.True(t, ok)
	assert.Equal(t, 2, v.Number)

	_, ok = h.ByFingerprint("deadbeef")
	assert.False(t, ok)

	_, ok = h.ByFingerprint("")
	assert.False(t, ok)
}

// TestHistoryDrifted is SPEC §C.6's definition of drift: has the rule been
// edited since the version an occurrence was bound to?
func TestHistoryDrifted(t *testing.T) {
	t.Parallel()

	s1 := snapAt("a > 1", capturedAt)
	s2 := snapAt("a > 2", capturedAt.Add(time.Hour))

	one := domain.NewHistory(validKey(), []domain.Snapshot{s1})
	two := domain.NewHistory(validKey(), []domain.Snapshot{s1, s2})

	assert.False(t, one.Drifted(s1.Fingerprint), "the only version cannot have drifted")
	assert.True(t, two.Drifted(s1.Fingerprint), "an older text is drift")
	assert.False(t, two.Drifted(s2.Fingerprint), "the newest text is not drift")

	// An occurrence with no bound snapshot has nothing to have drifted from, so
	// oto must not claim an edit it cannot evidence.
	assert.False(t, two.Drifted(""))
	assert.False(t, domain.History{}.Drifted(s1.Fingerprint))
}

// TestHistoryDriftedIsAboutContentNotCount: a rule reverted to an earlier text
// reuses the earlier row, so the newest version can carry an old fingerprint.
func TestHistoryDriftedIsAboutContentNotCount(t *testing.T) {
	t.Parallel()

	s1 := snapAt("a > 1", capturedAt)
	s2 := snapAt("a > 2", capturedAt.Add(time.Hour))
	reverted := snapAt("a > 1", capturedAt.Add(2*time.Hour))
	require.Equal(t, s1.Fingerprint, reverted.Fingerprint)

	h := domain.NewHistory(validKey(), []domain.Snapshot{s1, s2, reverted})
	assert.False(t, h.Drifted(s1.Fingerprint),
		"an occurrence bound to the text the rule now has again has not drifted")
	assert.True(t, h.Drifted(s2.Fingerprint))
}

func TestHistoryDiffVersions(t *testing.T) {
	t.Parallel()

	h := domain.NewHistory(validKey(), []domain.Snapshot{
		snapAt("a > 1", capturedAt),
		snapAt("a > 2", capturedAt.Add(time.Hour)),
		snapAt("a > 3", capturedAt.Add(2*time.Hour)),
	})

	t.Run("compares oldest-first whichever order the caller asked in", func(t *testing.T) {
		t.Parallel()
		forward, ok := h.DiffVersions(1, 3)
		require.True(t, ok)
		backward, ok := h.DiffVersions(3, 1)
		require.True(t, ok)
		assert.Equal(t, forward, backward)

		assert.Equal(t, "a > 1", forward.From.Expr)
		assert.Equal(t, "a > 3", forward.To.Expr)
		assert.True(t, forward.Changed)
		assert.True(t, forward.SameRule)
		assert.Equal(t, []domain.NumberChange{{Index: 0, Old: 1, New: 3}}, forward.ExprNumbers)
	})

	t.Run("a version against itself is empty", func(t *testing.T) {
		t.Parallel()
		d, ok := h.DiffVersions(2, 2)
		require.True(t, ok)
		assert.True(t, d.Empty())
		assert.False(t, d.Changed)
	})

	t.Run("an out-of-range version is refused, not clamped", func(t *testing.T) {
		t.Parallel()
		for _, pair := range [][2]int{{0, 2}, {1, 4}, {-1, 1}, {5, 9}, {0, 0}} {
			d, ok := h.DiffVersions(pair[0], pair[1])
			assert.False(t, ok, "pair %v", pair)
			assert.Equal(t, domain.Diff{}, d)
		}
	})
}

// TestVersionNumberingSurvivesDeduplication: rule_snapshots is deduplicated by
// content, so the rows for one key already are its distinct texts. Numbering
// them at read time is what keeps the number in step with the rows.
func TestVersionNumberingIsContiguous(t *testing.T) {
	t.Parallel()

	snaps := make([]domain.Snapshot, 0, 10)
	for i := 0; i < 10; i++ {
		snaps = append(snaps, snapAt("a > "+string(rune('0'+i)), capturedAt.Add(time.Duration(i)*time.Hour)))
	}
	h := domain.NewHistory(validKey(), snaps)

	require.Equal(t, 10, h.Len())
	for i, v := range h.Versions {
		assert.Equal(t, i+1, v.Number)
		if i+1 < h.Len() {
			assert.Equal(t, i+2, v.SupersededBy)
		} else {
			assert.Equal(t, 0, v.SupersededBy)
		}
		got, ok := h.At(v.Number)
		require.True(t, ok)
		assert.Equal(t, v, got)
	}
}
