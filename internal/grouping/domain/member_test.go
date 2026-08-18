package domain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/grouping/domain"
)

func validMemberParams() domain.MemberParams {
	return domain.MemberParams{
		GroupID:  uuid.New(),
		CaseID:   uuid.New(),
		OrgID:    uuid.New(),
		AlertID:  uuid.New(),
		JoinedAt: baseTime,
	}
}

func TestNewMemberRejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(p *domain.MemberParams)
		code   string
	}{
		{
			name:   "nil group_id",
			mutate: func(p *domain.MemberParams) { p.GroupID = uuid.Nil },
			code:   "required",
		},
		{
			name:   "nil case_id",
			mutate: func(p *domain.MemberParams) { p.CaseID = uuid.Nil },
			code:   "required",
		},
		{
			name:   "nil org_id",
			mutate: func(p *domain.MemberParams) { p.OrgID = uuid.Nil },
			code:   "required",
		},
		{
			name:   "nil alert_id",
			mutate: func(p *domain.MemberParams) { p.AlertID = uuid.Nil },
			code:   "required",
		},
		{
			name:   "missing joined_at",
			mutate: func(p *domain.MemberParams) { p.JoinedAt = time.Time{} },
			code:   "required",
		},
		{
			name: "left_at one nanosecond before joined_at (case_order_ck)",
			mutate: func(p *domain.MemberParams) {
				p.LeftAt = baseTime.Add(-time.Nanosecond)
			},
			code: "field_order",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := validMemberParams()
			tc.mutate(&p)
			m, err := domain.NewMember(p)
			requireValidationCode(t, err, tc.code)
			assert.Equal(t, domain.Member{}, m)
		})
	}
}

func TestNewMemberAcceptances(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(p *domain.MemberParams)
	}{
		{
			name:   "still a member — no left_at",
			mutate: func(_ *domain.MemberParams) {},
		},
		{
			name:   "left_at equal to joined_at is the boundary",
			mutate: func(p *domain.MemberParams) { p.LeftAt = baseTime },
		},
		{
			name:   "left_at after joined_at",
			mutate: func(p *domain.MemberParams) { p.LeftAt = baseTime.Add(time.Hour) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := validMemberParams()
			tc.mutate(&p)
			_, err := domain.NewMember(p)
			require.NoError(t, err)
		})
	}
}

func TestNewMemberNormalisesToUTC(t *testing.T) {
	t.Parallel()

	tokyo := time.FixedZone("JST", 9*3600)
	p := validMemberParams()
	p.JoinedAt = baseTime.In(tokyo)
	p.LeftAt = baseTime.Add(time.Hour).In(tokyo)

	m, err := domain.NewMember(p)
	require.NoError(t, err)
	assert.Equal(t, time.UTC, m.JoinedAt().Location())
	assert.Equal(t, time.UTC, m.LeftAt().Location())
	assert.True(t, m.JoinedAt().Equal(baseTime))

	// An absent left_at stays absent rather than becoming the UTC epoch.
	p.LeftAt = time.Time{}
	m2, err := domain.NewMember(p)
	require.NoError(t, err)
	assert.True(t, m2.LeftAt().IsZero())
}

func TestMemberIsCurrent(t *testing.T) {
	t.Parallel()

	p := validMemberParams()
	m, err := domain.NewMember(p)
	require.NoError(t, err)
	assert.True(t, m.IsCurrent())

	p.LeftAt = baseTime.Add(time.Minute)
	left, err := domain.NewMember(p)
	require.NoError(t, err)
	assert.False(t, left.IsCurrent())
}

// TestWasMemberAt is the property that makes a group card reproducible:
// membership is a half-open interval [joined_at, left_at).
func TestWasMemberAt(t *testing.T) {
	t.Parallel()

	joined := baseTime
	left := baseTime.Add(time.Hour)

	cases := []struct {
		name   string
		leftAt time.Time
		at     time.Time
		want   bool
	}{
		{name: "before joining", leftAt: left, at: joined.Add(-time.Nanosecond)},
		{name: "exactly at joining", leftAt: left, at: joined, want: true},
		{name: "midway", leftAt: left, at: joined.Add(30 * time.Minute), want: true},
		{name: "one nanosecond before leaving", leftAt: left, at: left.Add(-time.Nanosecond), want: true},
		{name: "exactly at leaving is already gone", leftAt: left, at: left},
		{name: "after leaving", leftAt: left, at: left.Add(time.Hour)},
		{name: "still a member, long after joining", at: joined.Add(100 * time.Hour), want: true},
		{name: "still a member, before joining", at: joined.Add(-time.Second)},
		{
			name:   "joined and left in the same instant is never a member",
			leftAt: joined,
			at:     joined,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := validMemberParams()
			p.JoinedAt = joined
			p.LeftAt = tc.leftAt
			m, err := domain.NewMember(p)
			require.NoError(t, err)
			assert.Equal(t, tc.want, m.WasMemberAt(tc.at))
		})
	}
}

// TestWasMemberAtIgnoresTheCallersZone: replay asks about an instant, not a wall
// clock reading.
func TestWasMemberAtIgnoresTheCallersZone(t *testing.T) {
	t.Parallel()

	tokyo := time.FixedZone("JST", 9*3600)
	p := validMemberParams()
	p.JoinedAt = baseTime
	p.LeftAt = baseTime.Add(time.Hour)
	m, err := domain.NewMember(p)
	require.NoError(t, err)

	at := baseTime.Add(30 * time.Minute)
	assert.True(t, m.WasMemberAt(at))
	assert.True(t, m.WasMemberAt(at.In(tokyo)))
	assert.False(t, m.WasMemberAt(baseTime.Add(2*time.Hour).In(tokyo)))
}

func TestMemberAccessorsRoundTripTheConstructor(t *testing.T) {
	t.Parallel()

	p := validMemberParams()
	p.LeftAt = baseTime.Add(time.Hour)
	m, err := domain.NewMember(p)
	require.NoError(t, err)

	assert.Equal(t, p.GroupID, m.GroupID())
	assert.Equal(t, p.CaseID, m.CaseID())
	assert.Equal(t, p.OrgID, m.OrgID())
	assert.Equal(t, p.AlertID, m.AlertID())
	assert.True(t, m.JoinedAt().Equal(p.JoinedAt))
	assert.True(t, m.LeftAt().Equal(p.LeftAt))
}
