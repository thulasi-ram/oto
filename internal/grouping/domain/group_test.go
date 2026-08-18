package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// baseTime is the fixed instant every test in this package hangs off. The domain
// never calls time.Now(), so nothing here needs a clock.
var baseTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// validGroupKey is the canonical shape of groups_key_ck: "gk_" + 26 base32hex.
const validGroupKey = "gk_abcdefghijklmnopqrstuv0123"

func mustGroupKey(t *testing.T) domain.GroupKey {
	t.Helper()
	k, err := domain.NewGroupKey(validGroupKey)
	require.NoError(t, err)
	return k
}

// validGroupParams is a generation that satisfies every §D.5 invariant. Each
// rejection test mutates exactly one field so the failure is unambiguous.
func validGroupParams(t *testing.T) domain.GroupParams {
	t.Helper()
	return domain.GroupParams{
		ID:             uuid.New(),
		OrgID:          uuid.New(),
		SourceID:       uuid.New(),
		ClusterID:      uuid.New(),
		ClusterKey:     "prod-eu-1",
		Key:            mustGroupKey(t),
		Generation:     1,
		SourceGroupKey: `{}:{alertname="KubePodCrashLooping"}`,
		Receiver:       "sre-slack",
		GroupLabels:    map[string]string{"alertname": "KubePodCrashLooping"},
		Title:          "KubePodCrashLooping",
		State:          domain.StateOpen,
		Severity:       "critical",
		StateVersion:   1,
		Counts:         domain.Counts{Firing: 1, Total: 1},
		FirstSeenAt:    baseTime,
		LastActivityAt: baseTime,
	}
}

func mustGroup(t *testing.T, mutate func(p *domain.GroupParams)) domain.Group {
	t.Helper()
	p := validGroupParams(t)
	if mutate != nil {
		mutate(&p)
	}
	g, err := domain.NewGroup(p)
	require.NoError(t, err)
	return g
}

// requireValidationCode asserts the error is a KindValidation errs.Error with the
// given stable code. §L.2.3 makes the code part of the contract, not an internal.
func requireValidationCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	require.True(t, errors.Is(err, errs.ErrValidation), "want a validation error, got %v", err)
	var e *errs.Error
	require.True(t, errors.As(err, &e))
	assert.Equal(t, code, e.Code)
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

func TestNewState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "open", in: "open", want: "open", ok: true},
		{name: "closed", in: "closed", want: "closed", ok: true},
		{name: "empty is not a state", in: ""},
		{name: "case is significant", in: "Open"},
		{name: "a case state is not a group state", in: "firing"},
		{name: "resolved is not a group state", in: "resolved"},
		{name: "whitespace is not trimmed", in: " open"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.NewState(tc.in)
			if !tc.ok {
				requireValidationCode(t, err, "enum")
				assert.True(t, got.IsZero())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.String())
			assert.False(t, got.IsZero())
		})
	}
}

func TestStateOpenness(t *testing.T) {
	t.Parallel()

	assert.True(t, domain.StateOpen.IsOpen())
	assert.False(t, domain.StateClosed.IsOpen())
	// The zero State is not open — an unset state must never read as "accepting
	// members".
	assert.False(t, domain.State{}.IsOpen())
	assert.True(t, domain.State{}.IsZero())
}

// ---------------------------------------------------------------------------
// GroupKey
// ---------------------------------------------------------------------------

func TestNewGroupKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{name: "canonical", in: validGroupKey, ok: true},
		{name: "all zeros", in: "gk_" + strings.Repeat("0", 26), ok: true},
		{name: "top of the base32hex alphabet", in: "gk_" + strings.Repeat("v", 26), ok: true},
		{name: "empty"},
		{name: "no prefix", in: strings.Repeat("a", 26)},
		{name: "alert key prefix", in: "ak_" + strings.Repeat("a", 26)},
		{name: "one character short", in: "gk_" + strings.Repeat("a", 25)},
		{name: "one character long", in: "gk_" + strings.Repeat("a", 27)},
		{name: "uppercase is outside the alphabet", in: "gk_" + strings.Repeat("A", 26)},
		{name: "w is outside base32hex", in: "gk_" + strings.Repeat("w", 26)},
		{name: "z is outside base32hex", in: "gk_" + strings.Repeat("z", 26)},
		{name: "leading whitespace", in: " gk_" + strings.Repeat("a", 26)},
		{name: "trailing newline is not anchored away", in: "gk_" + strings.Repeat("a", 26) + "\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.NewGroupKey(tc.in)
			if !tc.ok {
				requireValidationCode(t, err, "group_key")
				assert.True(t, got.IsZero())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.in, got.String())
			assert.False(t, got.IsZero())
		})
	}
}

// ---------------------------------------------------------------------------
// Counts
// ---------------------------------------------------------------------------

func TestCountsValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   domain.Counts
		code string
	}{
		{name: "all zero is a legal empty generation", in: domain.Counts{}},
		{name: "acked equal to total is the boundary", in: domain.Counts{Total: 3, Acked: 3}},
		{name: "negative firing", in: domain.Counts{Firing: -1}, code: "min"},
		{name: "negative suppressed", in: domain.Counts{Suppressed: -1}, code: "min"},
		{name: "negative resolved", in: domain.Counts{Resolved: -1}, code: "min"},
		{name: "negative expired", in: domain.Counts{Expired: -1}, code: "min"},
		{name: "negative total", in: domain.Counts{Total: -1}, code: "min"},
		{name: "negative acked", in: domain.Counts{Acked: -1}, code: "min"},
		{name: "acked one over total", in: domain.Counts{Total: 3, Acked: 4}, code: "field_order"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.in.Validate()
			if tc.code == "" {
				require.NoError(t, err)
				return
			}
			requireValidationCode(t, err, tc.code)
		})
	}
}

func TestCountsLive(t *testing.T) {
	t.Parallel()

	// Live is `Firing`, and ONLY `Firing`, since ADR 0041. A resolved or expired
	// member is not a reason to hold a generation open.
	assert.Equal(t, 0, domain.Counts{Resolved: 9, Expired: 9, Total: 18}.Live())

	// ⭐ `Suppressed` IS A SUBSET OF `Firing` AND MUST NOT BE ADDED TWICE. Three
	// members, two of them silenced: `Firing` already counts all three, because a
	// silenced alert is still firing. The old `Firing + Suppressed` would report
	// five live members out of three and hold the generation open on two that do
	// not exist.
	assert.Equal(t, 3, domain.Counts{Firing: 3, Suppressed: 2, Resolved: 5, Total: 8}.Live())

	// And a generation whose live members are ALL silenced is still live.
	assert.Equal(t, 2, domain.Counts{Firing: 2, Suppressed: 2, Total: 2}.Live())
}

func TestCountsEqualDecidesMateriality(t *testing.T) {
	t.Parallel()

	a := domain.Counts{Firing: 1, Total: 1}
	assert.True(t, a.Equal(domain.Counts{Firing: 1, Total: 1}))
	// Every dimension participates: a rollup that moved only `acked` is still a
	// material change, because a receipt is news.
	assert.False(t, a.Equal(domain.Counts{Firing: 1, Total: 1, Acked: 1}))
	assert.False(t, a.Equal(domain.Counts{Firing: 1, Suppressed: 1, Total: 1}))
	assert.False(t, a.Equal(domain.Counts{}))
}

// ---------------------------------------------------------------------------
// NewGroup
// ---------------------------------------------------------------------------

func TestNewGroupRejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(p *domain.GroupParams)
		code   string
	}{
		{
			name:   "nil id",
			mutate: func(p *domain.GroupParams) { p.ID = uuid.Nil },
			code:   "required",
		},
		{
			name:   "nil org_id",
			mutate: func(p *domain.GroupParams) { p.OrgID = uuid.Nil },
			code:   "required",
		},
		{
			name:   "nil source_id",
			mutate: func(p *domain.GroupParams) { p.SourceID = uuid.Nil },
			code:   "required",
		},
		{
			name:   "nil cluster_id",
			mutate: func(p *domain.GroupParams) { p.ClusterID = uuid.Nil },
			code:   "required",
		},
		{
			name:   "zero group key",
			mutate: func(p *domain.GroupParams) { p.Key = domain.GroupKey{} },
			code:   "required",
		},
		{
			name:   "generation zero",
			mutate: func(p *domain.GroupParams) { p.Generation = 0 },
			code:   "min",
		},
		{
			name:   "generation negative",
			mutate: func(p *domain.GroupParams) { p.Generation = -1 },
			code:   "min",
		},
		{
			name:   "blank title",
			mutate: func(p *domain.GroupParams) { p.Title = "" },
			code:   "not_blank",
		},
		{
			name:   "whitespace-only title",
			mutate: func(p *domain.GroupParams) { p.Title = "   \t\n " },
			code:   "not_blank",
		},
		{
			name:   "title one byte over the bound",
			mutate: func(p *domain.GroupParams) { p.Title = strings.Repeat("t", domain.MaxTitleBytes+1) },
			code:   "max_length",
		},
		{
			name:   "zero state",
			mutate: func(p *domain.GroupParams) { p.State = domain.State{} },
			code:   "required",
		},
		{
			name:   "state_version zero",
			mutate: func(p *domain.GroupParams) { p.StateVersion = 0 },
			code:   "min",
		},
		{
			name:   "state_version negative",
			mutate: func(p *domain.GroupParams) { p.StateVersion = -3 },
			code:   "min",
		},
		{
			name:   "invalid counts propagate",
			mutate: func(p *domain.GroupParams) { p.Counts = domain.Counts{Total: 1, Acked: 2} },
			code:   "field_order",
		},
		{
			name:   "missing first_seen_at",
			mutate: func(p *domain.GroupParams) { p.FirstSeenAt = time.Time{} },
			code:   "required",
		},
		{
			name:   "last_activity_at before first_seen_at",
			mutate: func(p *domain.GroupParams) { p.LastActivityAt = baseTime.Add(-time.Nanosecond) },
			code:   "field_order",
		},
		{
			name: "closed without closed_at",
			mutate: func(p *domain.GroupParams) {
				p.State = domain.StateClosed
				p.Counts = domain.Counts{Resolved: 1, Total: 1}
			},
			code: "field_order",
		},
		{
			name:   "open with closed_at",
			mutate: func(p *domain.GroupParams) { p.ClosedAt = baseTime },
			code:   "field_order",
		},
		{
			name: "closed_at before first_seen_at",
			mutate: func(p *domain.GroupParams) {
				p.State = domain.StateClosed
				p.Counts = domain.Counts{Resolved: 1, Total: 1}
				p.ClosedAt = baseTime.Add(-time.Hour)
				p.LastActivityAt = baseTime
			},
			code: "field_order",
		},
		// ⛔ TWO CASES WERE HERE AND ARE DELETED WITH `groups_storm_ck`: "storm_mode
		// without storm_since" and its mirror. The constraint paired two columns that
		// migration 00059 dropped, so there is no half-set state left to refuse.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := validGroupParams(t)
			tc.mutate(&p)
			g, err := domain.NewGroup(p)
			requireValidationCode(t, err, tc.code)
			assert.Equal(t, domain.Group{}, g, "a rejected constructor must return the zero Group")
		})
	}
}

func TestNewGroupAcceptances(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(p *domain.GroupParams)
	}{
		{
			name:   "title exactly at the bound",
			mutate: func(p *domain.GroupParams) { p.Title = strings.Repeat("t", domain.MaxTitleBytes) },
		},
		{
			name:   "generation one is the floor",
			mutate: func(p *domain.GroupParams) { p.Generation = 1 },
		},
		{
			name:   "state_version one is the floor",
			mutate: func(p *domain.GroupParams) { p.StateVersion = 1 },
		},
		{
			name: "a closed generation with closed_at equal to first_seen_at",
			mutate: func(p *domain.GroupParams) {
				p.State = domain.StateClosed
				p.Counts = domain.Counts{Resolved: 1, Total: 1}
				p.ClosedAt = baseTime
			},
		},
		{
			name:   "a reconciler-sourced group has no receiver",
			mutate: func(p *domain.GroupParams) { p.Receiver = "" },
		},
		{
			name:   "cluster_key may be empty in flight, before the join has run",
			mutate: func(p *domain.GroupParams) { p.ClusterKey = "" },
		},
		{
			name:   "no group labels at all",
			mutate: func(p *domain.GroupParams) { p.GroupLabels = nil },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := validGroupParams(t)
			tc.mutate(&p)
			_, err := domain.NewGroup(p)
			require.NoError(t, err)
		})
	}
}

func TestNewGroupTrimsTitleAndNormalisesToUTC(t *testing.T) {
	t.Parallel()

	tokyo := time.FixedZone("JST", 9*3600)
	p := validGroupParams(t)
	p.Title = "  KubePodCrashLooping  "
	p.FirstSeenAt = baseTime.In(tokyo)
	p.LastActivityAt = baseTime.Add(time.Minute).In(tokyo)
	g, err := domain.NewGroup(p)
	require.NoError(t, err)

	assert.Equal(t, "KubePodCrashLooping", g.Title())
	assert.Equal(t, time.UTC, g.FirstSeenAt().Location())
	assert.Equal(t, time.UTC, g.LastActivityAt().Location())
	assert.True(t, g.FirstSeenAt().Equal(baseTime))

	// An unset instant stays unset rather than becoming the UTC epoch.
	assert.True(t, g.ClosedAt().IsZero())
}

// TestNewGroupCopiesGroupLabels proves the group owns its labels. A caller that
// keeps mutating the map it passed in must not be able to rewrite history.
func TestNewGroupCopiesGroupLabels(t *testing.T) {
	t.Parallel()

	labels := map[string]string{"alertname": "X", "cluster": "prod"}
	p := validGroupParams(t)
	p.GroupLabels = labels

	g, err := domain.NewGroup(p)
	require.NoError(t, err)

	labels["alertname"] = "TAMPERED"
	delete(labels, "cluster")
	assert.Equal(t, map[string]string{"alertname": "X", "cluster": "prod"}, g.GroupLabels())

	// The accessor also hands out a copy.
	got := g.GroupLabels()
	got["alertname"] = "TAMPERED"
	assert.Equal(t, "X", g.GroupLabels()["alertname"])
}

func TestGroupAccessorsRoundTripTheConstructor(t *testing.T) {
	t.Parallel()

	p := validGroupParams(t)
	g, err := domain.NewGroup(p)
	require.NoError(t, err)

	assert.Equal(t, p.ID, g.ID())
	assert.Equal(t, p.OrgID, g.OrgID())
	assert.Equal(t, p.SourceID, g.SourceID())
	assert.Equal(t, p.ClusterID, g.ClusterID())
	assert.Equal(t, p.ClusterKey, g.ClusterKey())
	assert.Equal(t, p.Key, g.Key())
	assert.Equal(t, p.Generation, g.Generation())
	assert.Equal(t, p.SourceGroupKey, g.SourceGroupKey())
	assert.Equal(t, p.Receiver, g.Receiver())
	assert.Equal(t, p.State, g.State())
	assert.Equal(t, p.Severity, g.Severity())
	assert.Equal(t, p.StateVersion, g.StateVersion())
	assert.Equal(t, p.Counts, g.Counts())
	assert.Equal(t, p.LastNotificationReason, g.LastNotificationReason())
	assert.True(t, g.IsOpen())
}

// TestClusterKeyIsNotAGroupLabel pins the ⛔ in the doc comment: ClusterKey is
// first-class and does not read out of group_labels["cluster"].
func TestClusterKeyIsNotAGroupLabel(t *testing.T) {
	t.Parallel()

	g := mustGroup(t, func(p *domain.GroupParams) {
		p.ClusterKey = "prod-eu-1"
		p.GroupLabels = map[string]string{"alertname": "X", "cluster": "something-else"}
	})
	assert.Equal(t, "prod-eu-1", g.ClusterKey())

	// Removing the label — which is what a `group_by` edit does — leaves the
	// cluster key intact.
	g2 := mustGroup(t, func(p *domain.GroupParams) {
		p.ClusterKey = "prod-eu-1"
		p.GroupLabels = map[string]string{"alertname": "X"}
	})
	assert.Equal(t, "prod-eu-1", g2.ClusterKey())
}

// ---------------------------------------------------------------------------
// CanCloseAt
// ---------------------------------------------------------------------------

func TestCanCloseAt(t *testing.T) {
	t.Parallel()

	const delay = 5 * time.Minute

	cases := []struct {
		name   string
		mutate func(p *domain.GroupParams)
		now    time.Time
		want   bool
	}{
		{
			name:   "a firing member blocks the close",
			mutate: func(p *domain.GroupParams) { p.Counts = domain.Counts{Firing: 1, Total: 1} },
			now:    baseTime.Add(time.Hour),
		},
		{
			// ADR 0041: a silenced member is counted in BOTH, because it is firing
			// and nobody is being told. A silence never closes a generation.
			name: "a suppressed member blocks the close",
			mutate: func(p *domain.GroupParams) {
				p.Counts = domain.Counts{Firing: 1, Suppressed: 1, Total: 1}
			},
			now: baseTime.Add(time.Hour),
		},
		{
			name: "an already-closed generation cannot close again",
			mutate: func(p *domain.GroupParams) {
				p.State = domain.StateClosed
				p.Counts = domain.Counts{Resolved: 1, Total: 1}
				p.ClosedAt = baseTime
			},
			now: baseTime.Add(time.Hour),
		},
		{
			name:   "one nanosecond before the delay elapses",
			mutate: func(p *domain.GroupParams) { p.Counts = domain.Counts{Resolved: 1, Total: 1} },
			now:    baseTime.Add(delay - time.Nanosecond),
		},
		{
			name:   "exactly at the delay",
			mutate: func(p *domain.GroupParams) { p.Counts = domain.Counts{Resolved: 1, Total: 1} },
			now:    baseTime.Add(delay),
			want:   true,
		},
		{
			name:   "well past the delay",
			mutate: func(p *domain.GroupParams) { p.Counts = domain.Counts{Expired: 1, Total: 1} },
			now:    baseTime.Add(time.Hour),
			want:   true,
		},
		{
			name:   "an empty generation past the delay",
			mutate: func(p *domain.GroupParams) { p.Counts = domain.Counts{} },
			now:    baseTime.Add(delay),
			want:   true,
		},
		{
			name:   "a clock behind last_activity_at cannot close",
			mutate: func(p *domain.GroupParams) { p.Counts = domain.Counts{Resolved: 1, Total: 1} },
			now:    baseTime.Add(-time.Hour),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := mustGroup(t, tc.mutate)
			assert.Equal(t, tc.want, g.CanCloseAt(tc.now, delay))
		})
	}
}

// TestCanCloseAtIgnoresTheCallersZone proves the comparison is on the instant,
// not the wall clock: `now` in Tokyo must decide the same as `now` in UTC.
func TestCanCloseAtIgnoresTheCallersZone(t *testing.T) {
	t.Parallel()

	g := mustGroup(t, func(p *domain.GroupParams) { p.Counts = domain.Counts{Resolved: 1, Total: 1} })
	at := baseTime.Add(5 * time.Minute)
	tokyo := time.FixedZone("JST", 9*3600)

	assert.True(t, g.CanCloseAt(at, 5*time.Minute))
	assert.True(t, g.CanCloseAt(at.In(tokyo), 5*time.Minute))
}

// ---------------------------------------------------------------------------
// WithRollup
// ---------------------------------------------------------------------------

func TestWithRollupMateriality(t *testing.T) {
	t.Parallel()

	t.Run("an identical rollup is not news", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, func(p *domain.GroupParams) {
			p.Counts = domain.Counts{Firing: 2, Total: 2}
			p.Severity = "critical"
			p.StateVersion = 7
		})

		next, material, err := g.WithRollup(domain.Counts{Firing: 2, Total: 2}, "critical",
			baseTime.Add(time.Hour))
		require.NoError(t, err)
		assert.False(t, material)
		assert.Equal(t, 7, next.StateVersion(), "an immaterial change must not mint a Notification")
		assert.True(t, next.LastActivityAt().Equal(baseTime),
			"a repeat observation must not restart the close clock")
	})

	t.Run("moved counts bump state_version", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, func(p *domain.GroupParams) {
			p.Counts = domain.Counts{Firing: 2, Total: 2}
			p.StateVersion = 7
		})

		next, material, err := g.WithRollup(domain.Counts{Firing: 1, Resolved: 1, Total: 2},
			"critical", baseTime.Add(time.Hour))
		require.NoError(t, err)
		assert.True(t, material)
		assert.Equal(t, 8, next.StateVersion())
		assert.True(t, next.LastActivityAt().Equal(baseTime.Add(time.Hour)))
		assert.Equal(t, domain.Counts{Firing: 1, Resolved: 1, Total: 2}, next.Counts())
	})

	t.Run("a severity change alone is material", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, func(p *domain.GroupParams) {
			p.Counts = domain.Counts{Firing: 2, Total: 2}
			p.Severity = "warning"
			p.StateVersion = 3
		})

		next, material, err := g.WithRollup(domain.Counts{Firing: 2, Total: 2}, "critical",
			baseTime.Add(time.Minute))
		require.NoError(t, err)
		assert.True(t, material)
		assert.Equal(t, 4, next.StateVersion())
		assert.Equal(t, "critical", next.Severity())
	})

	t.Run("an acked-only change is material", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, func(p *domain.GroupParams) {
			p.Counts = domain.Counts{Firing: 2, Total: 2}
			p.StateVersion = 1
		})

		next, material, err := g.WithRollup(domain.Counts{Firing: 2, Total: 2, Acked: 2},
			"critical", baseTime.Add(time.Minute))
		require.NoError(t, err)
		assert.True(t, material)
		assert.Equal(t, 2, next.StateVersion())
	})

	t.Run("an invalid rollup is rejected and nothing is returned", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, nil)
		next, material, err := g.WithRollup(domain.Counts{Total: 1, Acked: 5}, "critical", baseTime)
		requireValidationCode(t, err, "field_order")
		assert.False(t, material)
		assert.Equal(t, domain.Group{}, next)
	})

	t.Run("a backward clock never rewinds last_activity_at", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, func(p *domain.GroupParams) {
			p.Counts = domain.Counts{Firing: 2, Total: 2}
			p.LastActivityAt = baseTime.Add(time.Hour)
		})
		next, material, err := g.WithRollup(domain.Counts{Firing: 1, Total: 2}, "critical", baseTime)
		require.NoError(t, err)
		assert.True(t, material)
		assert.True(t, next.LastActivityAt().Equal(baseTime.Add(time.Hour)))
	})

	t.Run("a closed generation stays closed", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, func(p *domain.GroupParams) {
			p.State = domain.StateClosed
			p.Counts = domain.Counts{Resolved: 1, Total: 1}
			p.ClosedAt = baseTime
		})
		next, _, err := g.WithRollup(domain.Counts{Firing: 1, Total: 1}, "critical", baseTime)
		require.NoError(t, err)
		assert.Equal(t, domain.StateClosed, next.State(),
			"a rollup must never re-open a generation; re-opening mints a new one")
		assert.False(t, next.IsOpen())
	})

	t.Run("the receiver is not mutated", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, func(p *domain.GroupParams) { p.StateVersion = 4 })
		_, _, err := g.WithRollup(domain.Counts{Firing: 9, Total: 9}, "warning", baseTime)
		require.NoError(t, err)
		assert.Equal(t, 4, g.StateVersion())
		assert.Equal(t, "critical", g.Severity())
	})
}

// ---------------------------------------------------------------------------
// Close and Touch
// ---------------------------------------------------------------------------

func TestClose(t *testing.T) {
	t.Parallel()

	t.Run("closes, freezes and bumps state_version", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, func(p *domain.GroupParams) {
			p.Counts = domain.Counts{Resolved: 2, Total: 2}
			p.StateVersion = 4
		})
		at := baseTime.Add(10 * time.Minute)

		next, err := g.Close(at)
		require.NoError(t, err)
		assert.Equal(t, domain.StateClosed, next.State())
		assert.False(t, next.IsOpen())
		assert.True(t, next.ClosedAt().Equal(at))
		assert.True(t, next.LastActivityAt().Equal(at))
		assert.Equal(t, 5, next.StateVersion())
	})

	// ⛔ "storm mode does not survive the close" WAS HERE. It proved `Close` cleared
	// the storm pair, because a storm on a closed generation is a storm nobody can
	// act on. Migration 00059 dropped both columns, so there is nothing to clear.

	t.Run("closed_at is clamped forward to first_seen_at", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, func(p *domain.GroupParams) {
			p.Counts = domain.Counts{Resolved: 1, Total: 1}
		})
		next, err := g.Close(baseTime.Add(-time.Hour))
		require.NoError(t, err)
		assert.True(t, next.ClosedAt().Equal(baseTime),
			"closed_at must never precede first_seen_at (groups_closed order)")
	})

	t.Run("a live member blocks the close", func(t *testing.T) {
		t.Parallel()
		for _, c := range []domain.Counts{
			{Firing: 1, Total: 1},
			// ADR 0041: silenced members are counted in `Firing` too.
			{Firing: 1, Suppressed: 1, Total: 1},
		} {
			g := mustGroup(t, func(p *domain.GroupParams) { p.Counts = c })
			_, err := g.Close(baseTime.Add(time.Hour))
			require.Error(t, err)
			require.True(t, errors.Is(err, errs.ErrPrecondition))
			var e *errs.Error
			require.True(t, errors.As(err, &e))
			assert.Equal(t, "group_has_live_members", e.Code)
		}
	})

	t.Run("closing twice is a precondition failure", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, func(p *domain.GroupParams) {
			p.State = domain.StateClosed
			p.Counts = domain.Counts{Resolved: 1, Total: 1}
			p.ClosedAt = baseTime
		})
		_, err := g.Close(baseTime.Add(time.Hour))
		require.Error(t, err)
		require.True(t, errors.Is(err, errs.ErrPrecondition))
		var e *errs.Error
		require.True(t, errors.As(err, &e))
		assert.Equal(t, "group_already_closed", e.Code)
	})

	t.Run("the closed generation still satisfies its own constructor", func(t *testing.T) {
		t.Parallel()
		g := mustGroup(t, func(p *domain.GroupParams) {
			p.Counts = domain.Counts{Resolved: 1, Total: 1}
		})
		next, err := g.Close(baseTime.Add(time.Minute))
		require.NoError(t, err)

		// groups_closed_ck, re-proved on the produced value. `groups_storm_ck` was
		// re-proved on the next line and is gone with its columns (migration 00059).
		assert.Equal(t, next.State() == domain.StateClosed, !next.ClosedAt().IsZero())
		assert.False(t, next.LastActivityAt().Before(next.FirstSeenAt()))
		assert.False(t, next.ClosedAt().Before(next.FirstSeenAt()))
	})
}

func TestTouchOnlyMovesForward(t *testing.T) {
	t.Parallel()

	g := mustGroup(t, func(p *domain.GroupParams) { p.LastActivityAt = baseTime.Add(time.Hour) })

	assert.True(t, g.Touch(baseTime.Add(2*time.Hour)).LastActivityAt().Equal(baseTime.Add(2*time.Hour)))
	assert.True(t, g.Touch(baseTime).LastActivityAt().Equal(baseTime.Add(time.Hour)),
		"a skewed upstream clock must not rewind the close clock")

	// Touch does not move the rollup or the version.
	next := g.Touch(baseTime.Add(2 * time.Hour))
	assert.Equal(t, g.StateVersion(), next.StateVersion())
	assert.Equal(t, g.Counts(), next.Counts())
}

// ---------------------------------------------------------------------------
// Title
// ---------------------------------------------------------------------------

func TestTitle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		labels   map[string]string
		fallback string
		want     string
	}{
		{
			name: "no labels and no fallback",
			want: "Alert group",
		},
		{
			name:     "no labels falls back to the receiver",
			fallback: "sre-slack",
			want:     "sre-slack",
		},
		{
			name:     "a blank fallback is still the generic name",
			fallback: "   ",
			want:     "Alert group",
		},
		{
			name:   "the alertname IS the title",
			labels: map[string]string{"alertname": "KubePodCrashLooping"},
			want:   "KubePodCrashLooping",
		},
		{
			name: "labels the card renders elsewhere are dropped, not repeated",
			labels: map[string]string{
				"alertname": "KubePodCrashLooping",
				"cluster":   "prod-eu-1",
				"namespace": "payments",
				"service":   "checkout",
			},
			want: "KubePodCrashLooping",
		},
		{
			name: "anything left over is genuinely distinguishing and is appended",
			labels: map[string]string{
				"alertname": "HighLatency",
				"cluster":   "prod",
				"route":     "/checkout",
				"env":       "eu",
			},
			want: "HighLatency, env=eu, route=/checkout",
		},
		{
			name: "leftover labels are byte-sorted, not map-ordered",
			labels: map[string]string{
				"alertname": "X",
				"zeta":      "1",
				"Alpha":     "2",
				"beta":      "3",
			},
			want: "X, Alpha=2, beta=3, zeta=1",
		},
		{
			name:   "an empty-valued leftover label is not rendered",
			labels: map[string]string{"alertname": "X", "team": "", "env": "eu"},
			want:   "X, env=eu",
		},
		{
			name:   "without an alertname the honest fallback is the k=v dump",
			labels: map[string]string{"cluster": "prod", "job": "node"},
			want:   "cluster=prod, job=node",
		},
		{
			name:     "a blank alertname is no alertname",
			labels:   map[string]string{"alertname": "  ", "job": "node"},
			fallback: "sre",
			want:     "alertname=  , job=node",
		},
		{
			name:   "an empty label map with a nil check falls through to the fallback",
			labels: map[string]string{},
			want:   "Alert group",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, domain.Title(tc.labels, tc.fallback))
		})
	}
}

// TestTitleIsDeterministic guards the property that makes the Slack card and the
// UI agree: the same labels must render the same title every time, whatever
// order Go's map iteration hands them back.
func TestTitleIsDeterministic(t *testing.T) {
	t.Parallel()

	labels := map[string]string{
		"alertname": "HighLatency",
		"env":       "eu",
		"route":     "/checkout",
		"shard":     "7",
		"tier":      "gold",
	}
	first := domain.Title(labels, "sre")
	for i := 0; i < 200; i++ {
		assert.Equal(t, first, domain.Title(labels, "sre"))
	}
}

// TestTitleOutputIsAcceptedByNewGroup is the contract between the two halves of
// this package: the service computes a title with Title() and hands it straight
// to the constructor via the repository. A title the constructor rejects is a
// group that cannot be opened.
func TestTitleOutputIsAcceptedByNewGroup(t *testing.T) {
	t.Parallel()

	labels := map[string]string{"alertname": "KubePodCrashLooping", "env": "eu"}
	title := domain.Title(labels, "sre")

	p := validGroupParams(t)
	p.GroupLabels = labels
	p.Title = title
	_, err := domain.NewGroup(p)
	require.NoError(t, err)
}

// TestBUGTitleTruncationExceedsMaxTitleBytes demonstrates a genuine defect.
//
// truncateTitle cuts at MaxTitleBytes-1 BYTES and appends "…", which is three
// bytes in UTF-8. The result is 502 bytes long. NewGroup measures the title with
// len() — bytes — and rejects anything over MaxTitleBytes, so Title() produces a
// value its own package's constructor refuses. Any group whose rendered title
// exceeds 500 bytes therefore cannot be opened at all.
//
// The same byte/character confusion also breaks CONTEXT.md §5b's "a bound lives
// in three places and they must be identical": groups_title_ck is
// `length(btrim(title)) BETWEEN 1 AND 500`, and Postgres `length()` counts
// CHARACTERS. A 400-character CJK title is legal in the DDL and rejected in Go.
func TestBUGTitleTruncationExceedsMaxTitleBytes(t *testing.T) {
	// Regression: truncateTitle used to cut at MaxTitleBytes-1 and append a
	// 3-byte '…', producing a 502-byte title that NewGroup rejected — so no
	// group with an over-long rendered title could be opened.

	labels := map[string]string{"alertname": strings.Repeat("A", 600)}
	title := domain.Title(labels, "sre")

	assert.LessOrEqual(t, len(title), domain.MaxTitleBytes,
		"a truncated title must fit the bound it was truncated to")

	p := validGroupParams(t)
	p.GroupLabels = labels
	p.Title = title
	_, err := domain.NewGroup(p)
	assert.NoError(t, err, "the title this package rendered must be constructible by this package")
}

// TestTitleTruncationLandsOnARuneBoundary is the other half of the same cut.
//
// MaxTitleBytes is a BYTE bound and the elided prefix is sliced by byte, so a
// multibyte title will straddle the cut point. A title sliced mid-rune is
// invalid UTF-8, which Postgres `text` refuses outright — the row would fail on
// INSERT rather than in the constructor, which is the worst place to find it.
func TestTitleTruncationLandsOnARuneBoundary(t *testing.T) {
	t.Parallel()

	// U+8B66 is 3 bytes, so 600 of them is 1800 bytes and no multiple of 3
	// lands on 497 — the cut is forced off a boundary unless it walks back.
	for _, r := range []string{"警", "é", "🔥"} {
		labels := map[string]string{"alertname": strings.Repeat(r, 600)}
		title := domain.Title(labels, "sre")

		assert.LessOrEqual(t, len(title), domain.MaxTitleBytes, "%q: over the byte bound", r)
		assert.True(t, utf8.ValidString(title), "%q: truncation produced invalid UTF-8", r)

		p := validGroupParams(t)
		p.GroupLabels = labels
		p.Title = title
		_, err := domain.NewGroup(p)
		assert.NoError(t, err, "%q: rendered title must be constructible", r)
	}
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

func TestGenerationLabel(t *testing.T) {
	t.Parallel()

	k := mustGroupKey(t)
	assert.Equal(t, validGroupKey+" #3", domain.GenerationLabel(k, 3))
	assert.Equal(t, " #1", domain.GenerationLabel(domain.GroupKey{}, 1))
}

func TestSnoozeRollupCarriesCountAndLatestWake(t *testing.T) {
	t.Parallel()

	// The type exists to keep "one of forty is muted" distinguishable from "all
	// forty are"; both dimensions must survive a round trip.
	r := domain.SnoozeRollup{Count: 3, Until: baseTime.Add(time.Hour)}
	assert.Equal(t, 3, r.Count)
	assert.True(t, r.Until.Equal(baseTime.Add(time.Hour)))

	// Zero count carries no wake-up instant.
	assert.True(t, domain.SnoozeRollup{}.Until.IsZero())
}
