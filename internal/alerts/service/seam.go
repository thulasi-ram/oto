package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// This file is the seam the modules that do not own `alert_events` call across.
//
// ⭐ WHAT IT IS FOR. `alert_events` has exactly ONE writer, and it is in this
// module: the C.8 idempotency claim and the closed EventType enum both live here,
// and a second appender would be the duplication §C.9 exists to forbid. `grouping`
// therefore records its `group.*` facts through this seam, and so do `rules` and
// `enrichment` for their `rule.*` and `enrichment.*` ones.
//
// ⚠️ THE THREE CALLERS DO NOT ARRIVE THE SAME WAY, and the difference is CONTEXT.md
// §4 rather than taste. `grouping ──► alerts` is a declared module edge, so grouping
// names `TimelineEventRequest` directly. `enrichment` has no such edge and `rules`
// must not grow a second one for a narration, so both declare an `EventRecorder`
// port of their own and `internal/app/adapters.go`'s timelineRecorder satisfies it
// over this method. One writer either way; what changes is who has to know it.
//
// ⛔ WHAT IT IS NOT FOR. It is not a shim around the shared domain kernel. Value
// objects and the §C identity functions live in `internal/alerts/domain`, which
// depguard RULE K sanctions every domain importing directly (SPEC §C.9,
// CONTEXT.md §5.2b). A `GroupKeyFor` wrapper used to sit here purely because the
// linter had granted that import to `ingestion` alone; the rule is now uniform and
// the wrapper is gone. Re-exporting a kernel function through a service is how a
// pure identity function acquires a fake dependency on a database.
//
// Every signature here is deliberately built from primitives, uuid.UUID and
// db.TenantScope, so the caller needs no type from the alerts kernel.

// TimelineEventRequest is one entry for the append-only timeline, described in
// primitives.
//
// The type is the CLOSED §D.4.1 set — `group.*` from grouping, `rule.*` from rules,
// `enrichment.*` from enrichment. Anything else is rejected by the kernel's
// EventType enum, because inventing a type produces a timeline row the renderer
// shows as nothing.
type TimelineEventRequest struct {
	Type    string
	GroupID uuid.UUID
	// AlertID and OccurrenceID name the alert or episode the fact is about; a
	// membership event names the member. At least one of the three subjects is
	// required (ev_subject_ck).
	AlertID      uuid.UUID
	OccurrenceID uuid.UUID
	Summary      string
	Payload      map[string]any
	// DedupeKey makes the append idempotent through `alert_event_keys` (C.8).
	DedupeKey string
	// ActorKind defaults to `system`. A human actor additionally requires an id
	// and a label (ev_actor_ck).
	ActorKind  string
	ActorID    string
	ActorLabel string
	// OccurredAt is the upstream claim; zero means "use oto's clock". RecordedAt
	// is always oto's clock and is what the timeline orders by (C12).
	OccurredAt time.Time
}

// AppendTimelineEvent writes one fact onto the timeline, inside the caller's
// transaction when there is one.
//
// It exists so that no other module opens `alert_events` itself. One writer, one
// idempotency mechanism, one place where an event's shape is proved.
func (s *Service) AppendTimelineEvent(ctx context.Context, scope db.TenantScope, in TimelineEventRequest) error {
	typ, err := domain.NewEventType(in.Type)
	if err != nil {
		return err
	}
	kindName := in.ActorKind
	if kindName == "" {
		kindName = domain.ActorSystem.String()
	}
	kind, err := domain.NewActorKind(kindName)
	if err != nil {
		return err
	}
	actor, err := domain.NewActor(kind, in.ActorID, in.ActorLabel)
	if err != nil {
		return err
	}

	now := s.Now()
	occurred := in.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	at, err := domain.NewObservationTime(occurred, now)
	if err != nil {
		return err
	}

	ev, err := domain.NewEvent(domain.EventParams{
		ID:           id.New(),
		OrgID:        scope.OrgID(),
		AlertID:      in.AlertID,
		OccurrenceID: in.OccurrenceID,
		GroupID:      in.GroupID,
		Type:         typ,
		At:           at,
		Actor:        actor,
		Summary:      in.Summary,
		Payload:      in.Payload,
		DedupeKey:    in.DedupeKey,
	})
	if err != nil {
		return err
	}
	_, err = s.appendEvents(ctx, scope, []domain.Event{ev})
	return err
}

// ------------------------------------------------- primitive action wrappers

// AcknowledgeAs is Acknowledge with the actor described in primitives.
//
// It is the fan-out entry point for `POST /api/v1/alert-groups/{id}/ack`, which
// acks every OPEN member episode. Ack is a RECEIPT — "a human has seen this" —
// and acking a group is still one receipt per signal, never a claim of ownership
// over a set of them (§E.1.1).
func (s *Service) AcknowledgeAs(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID,
	actorKind, actorID, actorLabel, note string,
) error {
	actor, err := humanActor(actorKind, actorID, actorLabel)
	if err != nil {
		return err
	}
	_, err = s.Acknowledge(ctx, scope, alertID, actor, note)
	return err
}

// SnoozeAs is Snooze with the actor described in primitives.
//
// It is the fan-out entry point for `POST /api/v1/alert-groups/{id}/snooze`,
// which is a FAN-OUT OF THE SAME PRIMITIVE and not a new one: one snooze per
// CURRENTLY-JOINED member alert. Alerts that join the group later are NOT
// snoozed — a snooze is never predictive (§B.8.3).
func (s *Service) SnoozeAs(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID,
	actorKind, actorID, actorLabel string, until time.Time, note string,
) error {
	actor, err := humanActor(actorKind, actorID, actorLabel)
	if err != nil {
		return err
	}
	_, err = s.Snooze(ctx, scope, alertID, actor, until, note)
	return err
}

// UnsnoozeAs is Unsnooze with the actor described in primitives.
func (s *Service) UnsnoozeAs(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID,
	actorKind, actorID, actorLabel, note string,
) error {
	actor, err := humanActor(actorKind, actorID, actorLabel)
	if err != nil {
		return err
	}
	_, err = s.Unsnooze(ctx, scope, alertID, actor, note)
	return err
}

// CommentAs is Comment with the actor described in primitives, for
// `POST /api/v1/alert-groups/{id}/comments`.
//
// It returns the APPENDED EVENT rather than swallowing it. The group endpoint
// answers `201` with the event it wrote, and the only place that fact is known
// for certain is here, at the write: re-reading the timeline afterwards is a
// second query that can return a different row.
func (s *Service) CommentAs(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID,
	actorKind, actorID, actorLabel, body string,
) (domain.Event, error) {
	actor, err := humanActor(actorKind, actorID, actorLabel)
	if err != nil {
		return domain.Event{}, err
	}
	return s.Comment(ctx, scope, alertID, actor, body)
}

func humanActor(kindName, actorID, label string) (domain.Actor, error) {
	if kindName == "" {
		kindName = domain.ActorUser.String()
	}
	kind, err := domain.NewActorKind(kindName)
	if err != nil {
		return domain.Actor{}, err
	}
	if !kind.IsHuman() {
		return domain.Actor{}, errs.Validation("actor_required",
			"this action requires a human actor")
	}
	return domain.NewActor(kind, actorID, label)
}

// The `group.*` timeline types, re-exported as plain strings so that `grouping`
// can name the fact it is recording without importing the kernel.
const (
	// GroupEventOpened records a new AlertGroup generation.
	GroupEventOpened = "group.opened"
	// GroupEventClosed records a generation closing after group_close_delay.
	GroupEventClosed = "group.closed"
	// GroupEventMemberJoined records an occurrence joining a generation.
	GroupEventMemberJoined = "group.member_joined"
	// GroupEventMemberLeft records an occurrence leaving a generation.
	GroupEventMemberLeft = "group.member_left"
	// GroupEventStormStarted records a generation entering storm mode.
	GroupEventStormStarted = "group.storm_started"
	// GroupEventStormEnded records storm mode ending after storm_cooldown.
	GroupEventStormEnded = "group.storm_ended"
)
