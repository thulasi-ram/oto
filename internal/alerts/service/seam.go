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
// module. `rules` and `enrichment` record their `rule.*` and `enrichment.*` facts
// through this seam. ⛔ `grouping` WAS THE THIRD CALLER AND ITS `group.*` APPENDS
// ARE DELETED WITH IT (git-bug `7570090`); the enum still CONTAINS those types
// because rows already carry them and must still be read back.
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
// ⚠️ THE CALLERS DO NOT ALL ARRIVE THE SAME WAY, and the difference is CONTEXT.md
// §4 rather than taste. `enrichment` has no `──► alerts` module edge and `rules`
// must not grow a second one for a narration, so both declare an `EventRecorder`
// port of their own and `internal/app/adapters.go`'s timelineRecorder satisfies it
// over this method. One writer either way; what changes is who has to know it.
//
// ⛔ THE OTHER SHAPE IS GONE WITH ITS ONLY USER: `grouping ──► alerts` WAS a declared
// module edge, so grouping named `TimelineEventRequest` directly instead of
// declaring a port. Both remaining callers go through a port, which is why this
// request type is now reached only from `internal/app`.
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
//
// ⛔ `GroupID uuid.UUID` WAS A FIELD HERE AND IS DELETED (git-bug `7570090`). It was
// the third subject a cross-module fact could name, and the only writers that ever
// set it were `grouping`'s own `group.opened`/`group.closed` appends — deleted with
// the module. The two callers that remain, `timelineRecorder`'s rule and enrichment
// narrators in `internal/app`, have never set it: both facts are about an alert or
// an episode, which is the whole reason they are narrated at all.
type TimelineEventRequest struct {
	Type domain.EventType
	// AlertID and CaseID name the alert or episode the fact is about. At least one
	// of them is required (ev_subject_ck).
	//
	// ⚠️ `ev_subject_ck` ITSELF STILL ADMITS `group_id` AS A THIRD SUBJECT, because
	// the column and its CHECK are dropped by a migration that has not landed. What
	// changed is that no path through THIS seam can produce one.
	AlertID uuid.UUID
	CaseID  uuid.UUID
	Summary string
	Payload map[string]any
	// DedupeKey makes the append idempotent through `alert_event_keys` (C.8).
	DedupeKey string
	// ActorKind defaults to `system`. A human actor additionally requires an id
	// and a label (ev_actor_ck).
	ActorKind  string
	ActorID    string
	ActorLabel string

	// ⛔ `OccurredAt time.Time` WAS HERE AND IS DELETED (git-bug `7570090`). It was
	// the UPSTREAM claim about when the fact happened, with "zero means use oto's
	// clock" as its documented default, and `grouping/service` was the only caller in
	// the tree that ever passed a non-zero one: it dated a `group.opened` to the
	// observation time of the batch that created the generation rather than to the
	// moment the row was written. No other cross-module narrator has an upstream
	// clock to quote — a rule change and an enrichment both HAPPEN when oto does
	// them — so every request through this seam took the default, and the field was
	// a parameter with exactly one value.
	//
	// ⭐ THE DISTINCTION IT ENCODED SURVIVES AND IS NOT THIS FIELD. §C12 is that
	// `occurred_at` is the upstream claim and `recorded_at` is always oto's clock and
	// is what the timeline ORDERS BY; `domain.ObservationTime` still carries both and
	// the state-machine path — which does have an upstream clock, on the Observation
	// — still supplies a real `occurred_at`. Only the cross-module seam, which never
	// did, stopped pretending it might.
}

// AppendTimelineEvent writes one fact onto the timeline, inside the caller's
// transaction when there is one.
//
// It exists so that no other module opens `alert_events` itself: one CROSS-MODULE
// writer, one idempotency mechanism, one place where an event's shape is proved.
//
// ⚠️ IT IS NOT THE ONLY STATEMENT THAT INSERTS INTO `alert_events`, and reading
// it as one is how the guarantee below gets over-claimed. Two other paths write
// that table, both deliberately:
//
//   - `alerts/service.appendEvents` — this module's own lifecycle (actions.go,
//     lifecycle.go, sweep.go) hands it `domain.Event`s it BUILT, so it needs no
//     request shape and does not pass through here;
//   - `notification/repository/events.go` — INSERTs its own `notification.*` and
//     `delivery.*` rows with its own §C.8 key claim, and says so on itself.
//
// What is true, and what the refusal below rests on, is narrower: this is the
// only way a request-shaped event enters `alert_events` from OUTSIDE
// `alerts`, and it is where every cross-module caller — grouping, rules,
// enrichment — arrives.
func (s *Service) AppendTimelineEvent(ctx context.Context, scope db.TenantScope, in TimelineEventRequest) error {
	// No parse of in.Type: it arrives already proved. `domain.EventType` cannot be
	// constructed outside the kernel except through `NewEventType`, so the only
	// value that can reach here without being one of the closed 36 is the zero
	// value, which `NewEvent` rejects as "event type is required".
	//
	// ⛔ BUT PROVED IS NOT THE SAME AS PERMITTED. A RETIRED type parses — it must,
	// because rows already carry it and the timeline has to read them back — and it
	// still may not be appended. There are three: `group.member_joined`,
	// `group.member_left` and `case.reopened`, and the reasoning is on
	// `domain.retiredEventTypes`. It is `Internal` rather than `Validation` on
	// purpose: no request can ask for this, so reaching it means code asked for it.
	//
	// ⚠️ WHAT THIS REFUSAL COVERS, EXACTLY. The two `group.*` values are emitted
	// from a different module, so every caller that could ever emit one arrives
	// HERE — which is what makes a check at this one point sufficient for them. It
	// is not a guarantee about the table in general: see the two other write paths
	// named on this method.
	//
	// ⭐ AND `case.reopened` IS WHY `appendEvents` NOW CHECKS TOO (ADR 0040). It was
	// minted by THIS module's transition table, so it reached `alert_events` by the
	// in-module road and never came past this line — the comment here used to say
	// extending the check to `appendEvents` "would close the in-module half", and
	// `alerts/service.refuseRetired` is that extension. `notification`'s INSERT is
	// still outside both, and `test/arch`'s event-type gate is what covers the
	// table as a whole.
	if in.Type.Retired() {
		return errs.Newf(errs.KindInternal, "event_type_retired",
			"%q is a retired alert_events.type and may be read but never appended", in.Type)
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

	// ⛔ THIS READ `occurred := in.OccurredAt; if occurred.IsZero() { occurred = now }`
	// (git-bug `7570090`). With the field deleted the branch had one arm, so the
	// default is now stated once: a fact narrated across a module boundary occurred
	// when oto recorded it, because the narrator has no upstream clock to quote.
	now := s.Now()
	at, err := domain.NewObservationTime(now, now)
	if err != nil {
		return err
	}

	ev, err := domain.NewEvent(domain.EventParams{
		ID:        id.New(),
		OrgID:     scope.OrgID(),
		AlertID:   in.AlertID,
		CaseID:    in.CaseID,
		Type:      in.Type,
		At:        at,
		Actor:     actor,
		Summary:   in.Summary,
		Payload:   in.Payload,
		DedupeKey: in.DedupeKey,
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
//
// ⭐ IT TAKES A CASE ID, NOT AN ALERT ID, AND THE FAN-OUT ALREADY HAD ONE. The
// candidate read behind a group verb is `CurrentMemberAlerts`, which selects
// `(alert_id, id)` out of `alert_cases` — the episode is what makes an alert a
// member of a generation, so the id was being read, discarded, and looked up
// again from the alert one layer down. Handing it straight through removes the
// second read and, with it, the window in which the episode that made the alert
// a member and the episode that receives the receipt could be different rows.
func (s *Service) AcknowledgeAs(
	ctx context.Context, scope db.TenantScope, caseID uuid.UUID,
	actorKind, actorID, actorLabel, note string,
) error {
	actor, err := humanActor(actorKind, actorID, actorLabel)
	if err != nil {
		return err
	}
	_, err = s.Acknowledge(ctx, scope, caseID, actor, note)
	return err
}

// UnacknowledgeAs is Unacknowledge with the actor described in primitives.
//
// It is the fan-out entry point for `POST /api/v1/alert-groups/{id}/unack`, the
// exact inverse of AcknowledgeAs above: where the group ack means "every member
// carries a receipt", the group unack means NO member carries one. It is still one
// withdrawal per signal and never a claim over a set of them (§E.1.1), and the
// reason is `manual` — a deliberate withdrawal, not the automatic unack a new
// episode performs.
//
// ⛔ THE NOTE DOES NOT GO BACK ONTO THE CASE. `ack_note` describes the
// acknowledgement being removed and is cleared by the transition; the withdrawal's
// own explanation lands in the `case.unacknowledged` event payload, on each
// member's timeline. See Unacknowledge.
//
// It is addressed by CASE id, for the reason given on AcknowledgeAs.
func (s *Service) UnacknowledgeAs(
	ctx context.Context, scope db.TenantScope, caseID uuid.UUID,
	actorKind, actorID, actorLabel, note string,
) error {
	actor, err := humanActor(actorKind, actorID, actorLabel)
	if err != nil {
		return err
	}
	_, err = s.Unacknowledge(ctx, scope, caseID, actor, note)
	return err
}

// SnoozeAs is Snooze with the actor described in primitives.
//
// It is the fan-out entry point for `POST /api/v1/alert-groups/{id}/snooze`,
// which is a FAN-OUT OF THE SAME PRIMITIVE and not a new one: one snooze per
// CURRENTLY-JOINED member alert. Alerts that join the group later are NOT
// snoozed — a snooze is never predictive (§B.8.3).
//
// ⭐ THE INTENT TRAVELS WITH IT, and the second result says whether the claim
// REPLAYED. A fan-out that ignored that answer would carry on to members two
// through forty and grant each of them the second snooze the claim just refused
// on member one — which is the duplication the key was sent to prevent, moved
// one member to the right.
func (s *Service) SnoozeAs(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID,
	actorKind, actorID, actorLabel string, until time.Time, note string, idem Idempotency,
) (bool, error) {
	actor, err := humanActor(actorKind, actorID, actorLabel)
	if err != nil {
		return false, err
	}
	_, replayed, err := s.Snooze(ctx, scope, alertID, actor, until, note, idem)
	return replayed, err
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
//
// ⭐ THE INTENT TRAVELS WITH IT, and the third result says whether the claim
// REPLAYED — see SnoozeAs for why a fan-out has to stop when it does.
func (s *Service) CommentAs(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID,
	actorKind, actorID, actorLabel, body string, idem Idempotency,
) (domain.Event, bool, error) {
	actor, err := humanActor(actorKind, actorID, actorLabel)
	if err != nil {
		return domain.Event{}, false, err
	}
	return s.Comment(ctx, scope, alertID, actor, body, idem)
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
