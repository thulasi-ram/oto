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
// ⭐ WHAT IT IS FOR. It is the ONE appender that goes through `domain.NewEvent`,
// and therefore the one place every §D.4 invariant is proved before a row exists.
// The C.8 idempotency claim and the closed EventType enum both live in this
// module. `grouping` records its `group.*` facts through this seam, and so do
// `rules` and `enrichment` for their `rule.*` and `enrichment.*` ones.
//
// ⛔ IT IS NOT THE ONLY WRITER OF `alert_events`, AND SAYING SO WAS A LIE THIS
// COMMENT TOLD FOR LONGER THAN IT SHOULD HAVE. `notification/repository/events.go`
// inserts its own `notification.*` and `delivery.*` rows — it is a second appender,
// with its own copy of the C.8 key claim, that never calls `NewEvent`. The
// duplication §C.9 forbids is therefore REAL and still open; what this seam can
// still guarantee, and now does by its signature, is that no caller may name a
// type the closed enum does not contain. Routing that second writer through here
// would need a `notification ──► alerts` module edge that CONTEXT.md §4 does not
// draw, which is a separate decision and a separate change.
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
// ⚠️ THE SIGNATURES ARE PRIMITIVES EXCEPT WHERE THE KERNEL IS THE POINT. Actors,
// times and ids travel as `string`, `time.Time`, `uuid.UUID` and `db.TenantScope`
// so a caller needs no alerts type to describe WHO or WHEN. The event TYPE is the
// deliberate exception: it is `domain.EventType`, and that is the whole property
// this seam exists to give. An earlier version of this comment claimed the caller
// "needs no type from the alerts kernel" — it was never true (`grouping/service`
// has always imported the kernel for `NewLabels` and `ComputeGroupKey`), and RULE K
// (§5.2b, `.golangci.yml`) grants every domain that import uniformly, so pretending
// otherwise bought nothing and cost the compile-time check below.

// TimelineEventRequest is one entry for the append-only timeline.
//
// The type is the CLOSED §D.4.1 set — `group.*` from grouping, `rule.*` from rules,
// `enrichment.*` from enrichment.
//
// ⭐ IT IS `domain.EventType` AND NOT A `string`, AND THAT IS THE DIFFERENCE
// BETWEEN A CLOSED ENUM AND AN OPEN ONE WITH EXTRA STEPS. As a string, a typo in a
// caller was a runtime KindValidation error from `NewEventType` — found when the
// fact it was recording had already happened, on the timeline that is the product.
// As the kernel's value object, whose only field is unexported and whose only
// parser is `NewEventType`, it is a compile error, and the six `group.*` constants
// this file used to re-export as bare strings have nowhere left to be re-added.
type TimelineEventRequest struct {
	Type    domain.EventType
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
	// No parse of in.Type: it arrives already proved. `domain.EventType` cannot be
	// constructed outside the kernel except through `NewEventType`, so the only
	// value that can reach here without being one of the closed 36 is the zero
	// value, which `NewEvent` rejects as "event type is required".
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
		Type:         in.Type,
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

// ⛔ THE SIX `group.*` STRING CONSTANTS THAT USED TO BE HERE ARE GONE, AND MUST NOT
// COME BACK. They were a second spelling of `domain.EventGroupOpened` and its five
// siblings, re-exported "so that `grouping` can name the fact it is recording
// without importing the kernel" — a reason that was never load-bearing, because
// RULE K grants `grouping` that import and `grouping/service` had already taken it.
// What the copies actually bought was a `Type string` field, and with it a class of
// typo the compiler could not see. `grouping` now names
// `alerts/domain.EventGroup*` directly. `test/arch/eventtype_test.go` is what
// notices if a package re-declares one.
