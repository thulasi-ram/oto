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

// This file is the seam `internal/grouping` calls across.
//
// ⭐ WHAT IT IS FOR. `alert_events` has exactly ONE writer, and it is in this
// module: the C.8 idempotency claim and the closed EventType enum both live here,
// and a second appender would be the duplication §C.9 exists to forbid. `grouping`
// therefore records its `group.*` facts through this seam.
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

// GroupEventRequest is one `group.*` entry for the append-only timeline.
//
// The type is a closed set: group.opened, group.closed, group.member_joined,
// group.member_left, group.storm_started, group.storm_ended. Anything else is
// rejected by the kernel's EventType enum, because inventing a type produces a
// timeline row the renderer shows as nothing (§D.4.1).
type GroupEventRequest struct {
	Type    string
	GroupID uuid.UUID
	// AlertID and OccurrenceID name the member a membership event is about.
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

// AppendGroupEvent writes one group fact onto the timeline, inside the caller's
// transaction.
//
// It exists so that `grouping` never opens `alert_events` itself. One writer, one
// idempotency mechanism, one place where an event's shape is proved.
func (s *Service) AppendGroupEvent(ctx context.Context, scope db.TenantScope, in GroupEventRequest) error {
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

// MemberStates is the per-occurrence state of a group's members, keyed by
// occurrence id.
//
// It exists so that `grouping` can roll up `firing_count`, `acked_count` and the
// group's own state without reading `alert_occurrences` itself.
type MemberStates struct {
	// State is the occurrence state: firing | suppressed | resolved | expired.
	State map[uuid.UUID]string
	// Acked reports which members a human has taken. Orthogonal to State: an
	// acked alert is still firing (§B.1).
	Acked map[uuid.UUID]bool
	// Severity is the RAW upstream severity label of each member's Alert, kept
	// raw because users filter on their own vocabulary (§L.4.2).
	Severity map[uuid.UUID]string
}

// OccurrenceStates reads the current state of many episodes at once.
func (s *Service) OccurrenceStates(
	ctx context.Context, scope db.TenantScope, occurrenceIDs []uuid.UUID,
) (MemberStates, error) {
	out := MemberStates{
		State:    map[uuid.UUID]string{},
		Acked:    map[uuid.UUID]bool{},
		Severity: map[uuid.UUID]string{},
	}
	for _, oid := range occurrenceIDs {
		occ, err := s.occurrences.GetByID(ctx, scope, oid)
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				continue
			}
			return MemberStates{}, err
		}
		out.State[oid] = occ.State().String()
		out.Acked[oid] = occ.AckState().IsAcked()

		alert, err := s.alerts.GetByID(ctx, scope, occ.AlertID())
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				continue
			}
			return MemberStates{}, err
		}
		if raw, ok := alert.Labels().Get(domain.LabelSeverity); ok {
			out.Severity[oid] = raw
		}
	}
	return out, nil
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
	actorKind, actorID, actorLabel string,
) error {
	actor, err := humanActor(actorKind, actorID, actorLabel)
	if err != nil {
		return err
	}
	_, err = s.Unsnooze(ctx, scope, alertID, actor)
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
