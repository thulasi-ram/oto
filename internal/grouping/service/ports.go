package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/grouping/repository"
	"github.com/thulasiram/oto/internal/platform/db"
)

// The ports `grouping/service` declares for itself (SPEC §F.5, CONTEXT.md §5.4).
//
// Cross-domain access is service → service through an interface declared by the
// CONSUMER, and grouping is the consumer of everything below. The alerts-side
// ports are typed in terms of `alerts/service` because that package is the seam
// grouping reaches the shared kernel through: `alert_events`, the §C.4 identity
// function and the member actions all have exactly one implementation, and it is
// not in this module.

// GroupRepository owns `alert_groups`.
type GroupRepository interface {
	GetByID(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Group, error)
	GetOpenByKey(ctx context.Context, s db.TenantScope, groupKey string) (domain.Group, bool, error)
	OpenGeneration(ctx context.Context, s db.TenantScope, in repository.NewGeneration) (domain.Group, error)
	// The three writers below are COMPARE-AND-SET on `alert_groups.state_version`:
	// `fromVersion` is the version the caller READ, and a write whose version has
	// moved returns errs.KindConflict rather than clobbering the winner. The
	// column already existed for §C.7's idempotency key; using it as the
	// optimistic lock as well keeps one answer to "has this generation changed".
	SetRollup(ctx context.Context, s db.TenantScope, g domain.Group, fromVersion int) error
	SetStorm(ctx context.Context, s db.TenantScope, g domain.Group, fromVersion int) error
	Close(ctx context.Context, s db.TenantScope, g domain.Group, fromVersion int) error
	Touch(ctx context.Context, s db.TenantScope, groupID uuid.UUID, at time.Time) error
	SetNotificationReason(ctx context.Context, s db.TenantScope, groupID uuid.UUID, reason string) error
	StateVersion(ctx context.Context, s db.TenantScope, groupID uuid.UUID) (int, error)
	// List takes the WHOLE filter and the sort key, because a filter applied
	// after pagination is not a filter (see domain.GroupFilter).
	List(ctx context.Context, s db.TenantScope, f domain.GroupFilter, sort string, p db.Keyset) ([]domain.Group, db.Cursor, error)
	CloseCandidates(ctx context.Context, s db.TenantScope, idleBefore time.Time, limit int) ([]domain.Group, error)
}

// MemberRepository READS a generation's membership off `alert_cases`.
//
// ⛔ THERE IS NO WRITE ON THIS PORT AND THERE MUST NOT BE ONE. Membership is not
// an event: since ADR 0038 derived the group key from the alert's own labels, an
// episode belongs to exactly one generation and `alert_cases.group_id` — set
// once, by the alerts module, when the episode opens — is the record. Migration
// 00051 dropped the `alert_group_members` table this port used to write through.
//
// There were a `Join` and a `Leave` here. `Join` inserted the row that made the
// group's rollup mean anything; `Leave` set the `left_at` that made "current
// members" current, and HAD NO PRODUCTION CALLER at any point in its life — three
// layers of implementation and an event type, reachable only from its own test.
// That is the defect `tools/lintreach` exists to catch, and the fix was not to
// wire it up: a human does not end an episode's membership of a group, the episode
// ending is what ends it.
type MemberRepository interface {
	// ListCurrentMembers is the READ PATH's view of a generation's current
	// members, and it is bounded. There was an unbounded `CurrentMembers` beside
	// it until the detail page's twenty-row preview stopped fetching a storm to
	// render twenty of it; a port that offers both is a port whose next caller
	// picks the wrong one.
	//
	// ⚠️ It is not the only read of current members — `CurrentMemberAlerts` below
	// is the WRITE path's, and this comment used to claim otherwise while an
	// unbounded sibling sat three lines under it. Both are bounded now, which is
	// what makes the claim safe to make about the port as a whole.
	ListCurrentMembers(ctx context.Context, s db.TenantScope, groupID uuid.UUID, p db.Keyset) ([]domain.Member, db.Cursor, error)
	AllMembers(ctx context.Context, s db.TenantScope, groupID uuid.UUID) ([]domain.Member, error)
	// MembersAt takes the INSTANT, because the alternative is AllMembers plus a
	// loop, and that is a filter the database was asked to skip.
	MembersAt(ctx context.Context, s db.TenantScope, groupID uuid.UUID, at time.Time) ([]domain.Member, error)
	GroupsForAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID, limit int) ([]domain.Member, error)
	DistinctJoinsSince(ctx context.Context, s db.TenantScope, groupID uuid.UUID, since time.Time) (int, time.Time, error)
	Rollup(ctx context.Context, s db.TenantScope, groupID uuid.UUID) (domain.Counts, string, error)
	// SnoozeRollup is the §B.8.6 result of the group snooze fan-out, for a whole
	// page of generations at once. It is derived at read time rather than stored,
	// because whether a snooze is active is a question about the clock and a
	// stored count would be stale after every expiry.
	SnoozeRollup(ctx context.Context, s db.TenantScope, groupIDs []uuid.UUID, now time.Time) (map[uuid.UUID]domain.SnoozeRollup, error)
	// CurrentMemberAlerts is the FAN-OUT's candidate read, and `limit` is not
	// optional decoration: one member is one write transaction, so the number of
	// rows this returns is the number of transactions the caller is about to
	// open. It is bounded in SQL rather than sliced afterwards.
	CurrentMemberAlerts(ctx context.Context, s db.TenantScope, groupID uuid.UUID, limit int) ([]repository.MemberAlert, error)
	// CountCurrentMembers lets a fan-out that stopped at its ceiling say how many
	// members it did not reach. It is asked only when the candidate read came
	// back full.
	CountCurrentMembers(ctx context.Context, s db.TenantScope, groupID uuid.UUID) (int, error)
}

// TxRunner runs a unit of work inside ONE transaction.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// EventAppender writes a `group.*` fact onto the append-only timeline.
//
// ⭐ It is a port and not a table. `alert_events` has exactly ONE writer, in the
// alerts module, which is where the C.8 idempotency claim and the closed EventType
// enum live. A second writer would mean a second idempotency mechanism, and two
// idempotency mechanisms are none.
type EventAppender interface {
	AppendTimelineEvent(ctx context.Context, s db.TenantScope, in alerts.TimelineEventRequest) error
}

// TimelineReader reads the merged group timeline — §D.12(b), the signature view.
type TimelineReader interface {
	GroupTimeline(ctx context.Context, s db.TenantScope, groupID uuid.UUID, w db.TimeWindow, p db.Keyset) (alerts.TimelineResult, error)
}

// MemberActions is the fan-out of the human verbs over a group's CURRENTLY-JOINED
// members (§E.2, §B.8.3).
//
// ⛔ There are exactly three VERBS, and they are the same three that exist on one
// alert: ack, comment and snooze. Two of them have a withdrawal (unack, unsnooze),
// which is the same verb read backwards and not a fourth one. A group has no verbs
// of its own — "ack this group" is a receipt written once per member signal, never
// a claim over a set of them (§E.1.1).
//
// ⚠️ UNACK IS HERE BECAUSE ACK IS. A group ack an operator could not take back is
// a one-way gesture over forty signals, and for a while it was exactly that: the
// group surface offered `ack` with no counterpart, so the only route back was
// opening each member alert and withdrawing its receipt one at a time.
type MemberActions interface {
	AcknowledgeAs(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actorKind, actorID, actorLabel, note string) error
	// UnacknowledgeAs is AcknowledgeAs read backwards, and the note lands in a
	// different place: an ack note is a property of the acknowledgement and is
	// stored on the case, while a withdrawal's note has nothing left to hang on and
	// goes onto the timeline in the `case.unacknowledged` payload.
	UnacknowledgeAs(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actorKind, actorID, actorLabel, note string) error
	// CommentAs returns the APPENDED EVENT, not just an error. The group comment
	// endpoint answers `201` with the event it wrote, and reading it back off the
	// timeline afterwards — which is what the handler used to do — is a second
	// query that can return a different row than the one just appended.
	//
	// ⭐ THE SECOND BOOL IS "THIS WAS A REPLAY". The caller's `Idempotency-Key`
	// travels with the intent and is claimed inside the MEMBER'S OWN transaction —
	// which is the only transaction a fan-out has, since it is deliberately one per
	// member (see fanOut). A replay means the whole gesture already landed, and the
	// fan-out must stop rather than annotate the remaining members a second time.
	CommentAs(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actorKind, actorID, actorLabel, body string, idem alerts.Idempotency) (kernel.Event, bool, error)
	SnoozeAs(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actorKind, actorID, actorLabel string, until time.Time, note string, idem alerts.Idempotency) (bool, error)
	UnsnoozeAs(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actorKind, actorID, actorLabel, note string) error
}

// StreamAppender publishes a `group.upserted` frame onto the SSE spine, inside
// the caller's transaction (§E.4).
type StreamAppender interface {
	Append(ctx context.Context, s db.TenantScope, kind string, resourceID uuid.UUID, payload []byte) error
}

// StreamGroupUpserted is the `ui_events.kind` this module publishes.
const StreamGroupUpserted = "group.upserted"

// SettingsReader reads one org's storm and close tuning from `orgs.settings`
// (§D.1). Implemented by `identity/service`.
type SettingsReader interface {
	Storm(ctx context.Context, s db.TenantScope) (domain.StormPolicy, error)
}
