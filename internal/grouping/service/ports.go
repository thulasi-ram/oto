package service

import (
	"context"
	"time"

	"github.com/google/uuid"

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
	SetRollup(ctx context.Context, s db.TenantScope, g domain.Group) error
	SetStorm(ctx context.Context, s db.TenantScope, g domain.Group) error
	Close(ctx context.Context, s db.TenantScope, g domain.Group) error
	Touch(ctx context.Context, s db.TenantScope, groupID uuid.UUID, at time.Time) error
	SetNotificationReason(ctx context.Context, s db.TenantScope, groupID uuid.UUID, reason string) error
	StateVersion(ctx context.Context, s db.TenantScope, groupID uuid.UUID) (int, error)
	List(ctx context.Context, s db.TenantScope, states []string, p db.Keyset) ([]domain.Group, db.Cursor, error)
	CloseCandidates(ctx context.Context, s db.TenantScope, idleBefore time.Time, limit int) ([]domain.Group, error)
}

// MemberRepository owns `alert_group_members`. Membership is history: there is no
// Delete here and there never will be.
type MemberRepository interface {
	Join(ctx context.Context, s db.TenantScope, groupID, occurrenceID, alertID uuid.UUID, at time.Time) (bool, error)
	Leave(ctx context.Context, s db.TenantScope, groupID, occurrenceID uuid.UUID, at time.Time) (bool, error)
	CurrentMembers(ctx context.Context, s db.TenantScope, groupID uuid.UUID) ([]domain.Member, error)
	AllMembers(ctx context.Context, s db.TenantScope, groupID uuid.UUID) ([]domain.Member, error)
	GroupsForAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID, limit int) ([]domain.Member, error)
	DistinctJoinsSince(ctx context.Context, s db.TenantScope, groupID uuid.UUID, since time.Time) (int, time.Time, error)
	Rollup(ctx context.Context, s db.TenantScope, groupID uuid.UUID) (domain.Counts, string, error)
	CurrentMemberAlerts(ctx context.Context, s db.TenantScope, groupID uuid.UUID) ([]repository.MemberAlert, error)
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
	AppendGroupEvent(ctx context.Context, s db.TenantScope, in alerts.GroupEventRequest) error
}

// TimelineReader reads the merged group timeline — §D.12(b), the signature view.
type TimelineReader interface {
	GroupTimeline(ctx context.Context, s db.TenantScope, groupID uuid.UUID, w db.TimeWindow, p db.Keyset) (alerts.TimelineResult, error)
}

// MemberActions is the fan-out of the human verbs over a group's CURRENTLY-JOINED
// members (§E.2, §B.8.3).
//
// ⛔ There are exactly three, and they are the same three that exist on one alert:
// ack, comment and snooze. A group has no verbs of its own — "ack this group" is
// a receipt written once per member signal, never a claim over a set of them
// (§E.1.1).
type MemberActions interface {
	AcknowledgeAs(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actorKind, actorID, actorLabel, note string) error
	CommentAs(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actorKind, actorID, actorLabel, body string) error
	SnoozeAs(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actorKind, actorID, actorLabel string, until time.Time, note string) error
	UnsnoozeAs(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actorKind, actorID, actorLabel string) error
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
