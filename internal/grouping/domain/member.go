package domain

import (
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// MemberPreviewLimit is how many currently-joined members the detail view shows.
// `top_alerts` is `maxItems: 20` in the contract, and this is that number.
//
// ⭐ IT IS THE ONLY COPY, and it lives in `domain` because that is the one place
// all three layers may name: `service.Get` passes it as the SQL LIMIT of the
// preview read, `api` renders exactly the rows that read returned and caps
// nothing of its own, and `repository`'s plan test EXPLAINs the statement at
// `MemberPreviewLimit + 1` — the extra row `ListCurrentMembers` reads to answer
// `has_more`. It cannot live in `service`: `service` imports `repository`, so a
// repository test naming it back would be an import cycle, which is exactly how
// the third hand-written 21 got there.
//
// Two literals could drift, and the direction they would drift in is the one
// where the endpoint fetches more rows than it can render.
//
// The full list is `GET /alert-groups/{id}/alerts`, which pages.
const MemberPreviewLimit = 20

// FanOutLimit is how many currently-joined members ONE press of a group verb —
// ack, comment, snooze, unsnooze — acts on.
//
// ⛔ IT IS NOT MemberPreviewLimit AND MUST NEVER BE. A preview renders a
// sample; a fan-out WRITES, and writing to a sample of a group would be a button
// that acks twenty of five thousand alerts and says nothing about the rest. The
// two numbers are unrelated and live apart on purpose.
//
// ⭐ WHY THERE IS A CEILING AT ALL. A fan-out is one full write transaction per
// member — a read of the alert, a read of the open case, a compare-and-set
// on the episode, a projection write, and a dedupe-key claim plus an event
// insert — applied in series. At the storm figure this module names for itself
// (`repository`'s member reads call it "a storm of five thousand") an unbounded
// fan-out is ~5 000 sequential commits and ~30 000 statements inside ONE HTTP
// request or ONE job. `internal/app/workers.go` says the rest of it: "A sweep
// that is not bounded is a sweep that becomes an outage the first time somebody
// has a bad night."
//
// ⭐ WHY 500. It is `sweepLimit`, deliberately: the same order of work oto
// already considers one safe unit for one tick. The binding deadline is tighter
// here than for a sweep — `slack.interaction` is registered with a FIFTEEN
// SECOND timeout (`internal/platform/jobs/registry.go`) because a human is
// watching the card, where a lifecycle sweep gets two minutes and repeats every
// sixty seconds. Five hundred member transactions fit inside fifteen seconds
// with room; five thousand do not, and a fan-out that outlives the timeout is
// retried from the beginning of the membership.
//
// ⚠️ A ceiling makes a big fan-out PARTIAL, and partial is only honest if it is
// counted: `service.FanOutResult.Unreached` is that count, and it is why this is
// a bound rather than a silent truncation.
const FanOutLimit = 500

// Member is the membership of ONE AlertCase in ONE AlertGroup generation.
//
// ⭐ Membership is HISTORY, NOT A BOOLEAN. `LeftAt` is zero rather than the
// membership being forgotten, so the group card can be replayed at any past
// instant — "what was in this group when the thread was posted?" is a question the
// timeline must be able to answer, and a forgotten membership cannot answer it.
//
// ⭐⭐ IT IS DERIVED, NOT RECORDED, AND THAT IS THE WHOLE OF MIGRATION 00051.
// There was an `alert_group_members` row behind this type; there is now an
// `alert_cases` row. Once the group key is computed from the alert's own
// labels (ADR 0038), an episode cannot belong to two generations, so membership is
// a FUNCTION OF THE EPISODE rather than an event that happened to it:
//
//	GroupID   the episode's own group_id, written once when it opens
//	JoinedAt  the episode's started_at — an episode joins by existing
//	LeftAt    the episode's ended_at — it leaves by ending
//
// The join table's `left_at` had no production writer, so `IsCurrent` was true of
// every membership that had ever been created and `WasMemberAt` could only ever
// show a generation growing. Both are honest now because `ended_at` is written by
// the §B.3 state machine and constrained by `case_terminal_ended`.
type Member struct {
	groupID  uuid.UUID
	caseID   uuid.UUID
	orgID    uuid.UUID
	alertID  uuid.UUID
	joinedAt time.Time
	leftAt   time.Time
}

// MemberParams is the constructor and rehydration shape.
type MemberParams struct {
	GroupID  uuid.UUID
	CaseID   uuid.UUID
	OrgID    uuid.UUID
	AlertID  uuid.UUID
	JoinedAt time.Time
	LeftAt   time.Time
}

// NewMember builds a membership record, enforcing the §D.5 invariants.
func NewMember(p MemberParams) (Member, error) {
	if err := requireID("group_id", p.GroupID); err != nil {
		return Member{}, err
	}
	if err := requireID("case_id", p.CaseID); err != nil {
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
	// case_order_ck: `ended_at IS NULL OR ended_at >= started_at`. It used to be
	// gm_order_ck on the join table, which said the same thing about the copy.
	if !p.LeftAt.IsZero() && p.LeftAt.Before(p.JoinedAt) {
		return Member{}, errs.New(errs.KindValidation, "field_order",
			"left_at must be >= joined_at")
	}
	return Member{
		groupID:  p.GroupID,
		caseID:   p.CaseID,
		orgID:    p.OrgID,
		alertID:  p.AlertID,
		joinedAt: p.JoinedAt.UTC(),
		leftAt:   utcOrZero(p.LeftAt),
	}, nil
}

// GroupID is the generation this membership is in.
func (m Member) GroupID() uuid.UUID { return m.groupID }

// CaseID is the episode that joined.
func (m Member) CaseID() uuid.UUID { return m.caseID }

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
