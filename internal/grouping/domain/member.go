package domain

import (
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Member is the membership of ONE AlertOccurrence in ONE AlertGroup generation.
//
// ⭐ Membership is HISTORY, NOT A BOOLEAN. `left_at` is nullable rather than a
// row being deleted, so the group card can be replayed at any past instant —
// "what was in this group when the thread was posted?" is a question the timeline
// must be able to answer, and a deleted row cannot answer it.
type Member struct {
	groupID      uuid.UUID
	occurrenceID uuid.UUID
	orgID        uuid.UUID
	alertID      uuid.UUID
	joinedAt     time.Time
	leftAt       time.Time
}

// MemberParams is the constructor and rehydration shape.
type MemberParams struct {
	GroupID      uuid.UUID
	OccurrenceID uuid.UUID
	OrgID        uuid.UUID
	AlertID      uuid.UUID
	JoinedAt     time.Time
	LeftAt       time.Time
}

// NewMember builds a membership record, enforcing the §D.5 invariants.
func NewMember(p MemberParams) (Member, error) {
	if err := requireID("group_id", p.GroupID); err != nil {
		return Member{}, err
	}
	if err := requireID("occurrence_id", p.OccurrenceID); err != nil {
		return Member{}, err
	}
	if err := requireID("org_id", p.OrgID); err != nil {
		return Member{}, err
	}
	if err := requireID("alert_id", p.AlertID); err != nil {
		return Member{}, err
	}
	if p.JoinedAt.IsZero() {
		return Member{}, errs.New(errs.KindValidation, "required", "joined_at is required")
	}
	// gm_order_ck
	if !p.LeftAt.IsZero() && p.LeftAt.Before(p.JoinedAt) {
		return Member{}, errs.New(errs.KindValidation, "field_order",
			"left_at must be >= joined_at")
	}
	return Member{
		groupID:      p.GroupID,
		occurrenceID: p.OccurrenceID,
		orgID:        p.OrgID,
		alertID:      p.AlertID,
		joinedAt:     p.JoinedAt.UTC(),
		leftAt:       utcOrZero(p.LeftAt),
	}, nil
}

// GroupID is the generation this membership is in.
func (m Member) GroupID() uuid.UUID { return m.groupID }

// OccurrenceID is the episode that joined.
func (m Member) OccurrenceID() uuid.UUID { return m.occurrenceID }

// OrgID is the tenant.
func (m Member) OrgID() uuid.UUID { return m.orgID }

// AlertID is the episode's Alert, denormalised so "which groups has this alert
// been part of" needs no join.
func (m Member) AlertID() uuid.UUID { return m.alertID }

// JoinedAt is when the episode joined the generation.
func (m Member) JoinedAt() time.Time { return m.joinedAt }

// LeftAt is when it left, zero while it is still a member.
func (m Member) LeftAt() time.Time { return m.leftAt }

// IsCurrent reports whether the episode is still a member.
func (m Member) IsCurrent() bool { return m.leftAt.IsZero() }

// WasMemberAt replays membership at a past instant, which is what makes a group
// card reproducible.
func (m Member) WasMemberAt(t time.Time) bool {
	t = t.UTC()
	if t.Before(m.joinedAt) {
		return false
	}
	return m.leftAt.IsZero() || t.Before(m.leftAt)
}
