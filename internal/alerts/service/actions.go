package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// MaxCommentBytes bounds a human comment.
//
// SPEC §E.2 declares the endpoint and §D.4 gives `alert_events.summary` a
// 500-character ceiling, but neither bounds the comment BODY, which lands in the
// event payload. This mirrors MaxAckNoteBytes, the nearest bounded sibling: both
// are free text a human types onto a signal's timeline.
const MaxCommentBytes = domain.MaxAckNoteBytes

// ⛔ THE ONLY THREE HUMAN VERBS (§E.1.1).
//
//	ack / unack — a RECEIPT. "A human has seen this", never "this is mine" and
//	              never "this is over".
//	comment     — an ANNOTATION on the timeline.
//	snooze      — a fact about OTO'S NOTIFICATIONS, never about the signal.
//
// There is no Resolve, no Close, no Merge, no Dismiss, no Reopen and no Ignore in
// this file, and there never will be. A human declaring a signal resolved is the
// exact lie §B.2 exists to prevent, and it would kill the system-of-record claim
// that is the entire product.

// Acknowledge records that a human has seen the Alert's current episode (T9).
//
// Acknowledgement is ORTHOGONAL to state: an acked alert is still firing, still
// whatever severity it was, and every surface keeps rendering it that way.
// Acknowledging an Alert with no open episode is a precondition failure — the
// request is well-formed, there is simply nothing running to acknowledge.
func (s *Service) Acknowledge(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, actor domain.Actor, note string,
) (domain.Occurrence, error) {
	return s.setAck(ctx, scope, alertID, actor, note, true, domain.UnackReasonManual)
}

// Unacknowledge drops an acknowledgement a human placed (T10, reason `manual`).
func (s *Service) Unacknowledge(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, actor domain.Actor,
) (domain.Occurrence, error) {
	return s.setAck(ctx, scope, alertID, actor, "", false, domain.UnackReasonManual)
}

func (s *Service) setAck(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, actor domain.Actor,
	note string, ack bool, reason string,
) (domain.Occurrence, error) {
	if actor.IsZero() || !actor.Kind().IsHuman() {
		return domain.Occurrence{}, errs.Validation("actor_required",
			"an acknowledgement requires a human actor")
	}

	var out domain.Occurrence
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		alert, err := s.alerts.GetByID(ctx, scope, alertID)
		if err != nil {
			return err
		}
		occ, ok, err := s.occurrences.GetOpenByAlert(ctx, scope, alertID)
		if err != nil {
			return err
		}
		if !ok {
			return errs.Precondition("no_open_occurrence",
				"this alert has no open occurrence to acknowledge")
		}

		now := s.Now()
		at, err := domain.NewObservationTime(now, now)
		if err != nil {
			return err
		}
		cmd := domain.AckCommand{
			Actor:   actor,
			At:      at,
			EventID: id.New(),
			Note:    note,
			Reason:  reason,
		}

		var (
			next domain.Occurrence
			evs  []domain.Event
		)
		if ack {
			next, evs, err = occ.Acknowledge(cmd)
		} else {
			next, evs, err = occ.Unacknowledge(cmd)
		}
		if err != nil {
			return err
		}

		change := domain.AckChange{
			To:     next.AckState(),
			At:     next.AckedAt(),
			Reason: reason,
		}
		if ack {
			change.By = nilID(next.AckedBy())
			label := next.AckedByLabel()
			change.ByLabel = &label
			if note != "" {
				change.Note = &note
			}
		}
		// The version `occ` was read at. If the episode resolved or expired while
		// the human was deciding, this fails with a conflict rather than stamping
		// an acknowledgement on a closed episode and rewinding the alert
		// projection to its pre-resolution state.
		if err := s.occurrences.SetAck(ctx, scope, next.ID(), change, occ.StateVersion()); err != nil {
			return err
		}

		// The ack projection moves ack_state and NOTHING else — not state, not
		// snoozed_until (§B.1).
		if err := s.alerts.SetProjection(ctx, scope, alert.ID(), domain.AlertProjection{
			State:               alert.State(),
			CurrentOccurrenceID: ptr(next.ID()),
			AckState:            next.AckState(),
			SnoozedUntil:        nilTime(alert.SnoozedUntil()),
			LastSeenAt:          alert.LastSeenAt(),
			LastStateChangeAt:   alert.LastStateChangeAt(),
			TotalOccurrences:    alert.TotalOccurrences(),
		}); err != nil {
			return err
		}

		if _, err := s.appendEvents(ctx, scope, evs); err != nil {
			return err
		}
		if err := s.publishOccurrence(ctx, scope, next); err != nil {
			return err
		}
		if err := s.publishAlert(ctx, scope, alert.ID(), map[string]any{
			"ack": next.AckState().String(),
		}); err != nil {
			return err
		}

		notifyReason := reasonAcked
		if !ack {
			notifyReason = reasonUnacked
		}
		if _, err := s.enqueueNotify(ctx, scope, []notifyRequest{{
			groupID:      next.GroupID(),
			reason:       notifyReason,
			alertID:      ptr(alert.ID()),
			occurrenceID: ptr(next.ID()),
			actor:        actor.Label(),
		}}, nil); err != nil {
			return err
		}

		out = next
		return nil
	})
	if err != nil {
		return domain.Occurrence{}, err
	}
	return out, nil
}

// Comment adds a human note to an Alert's timeline (T14).
//
// It is an ANNOTATION, never a state change: nothing in this method touches
// state, ack_state, severity or the snooze projection.
func (s *Service) Comment(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, actor domain.Actor, body string,
) (domain.Event, error) {
	if actor.IsZero() || !actor.Kind().IsHuman() {
		return domain.Event{}, errs.Validation("actor_required", "a comment requires a human actor")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return domain.Event{}, errs.Validation("comment_required", "a comment must not be blank")
	}
	if len(body) > MaxCommentBytes {
		return domain.Event{}, errs.Newf(errs.KindValidation, "max_length",
			"a comment must have at most %d characters", MaxCommentBytes)
	}

	var out domain.Event
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		alert, err := s.alerts.GetByID(ctx, scope, alertID)
		if err != nil {
			return err
		}
		occ, hasOpen, err := s.occurrences.GetOpenByAlert(ctx, scope, alertID)
		if err != nil {
			return err
		}

		now := s.Now()
		at, err := domain.NewObservationTime(now, now)
		if err != nil {
			return err
		}

		params := domain.EventParams{
			ID:      id.New(),
			OrgID:   scope.OrgID(),
			AlertID: alert.ID(),
			Type:    domain.EventCommentAdded,
			At:      at,
			Actor:   actor,
			Summary: commentSummary(actor.Label(), body),
			Payload: map[string]any{"body": body},
			// Two comments a second apart are two facts; the key exists so a
			// retried request is one.
			DedupeKey: "comment:" + alert.ID().String() + ":" + now.Format(time.RFC3339Nano),
		}
		if hasOpen {
			params.OccurrenceID = occ.ID()
			params.GroupID = occ.GroupID()
		}

		ev, err := domain.NewEvent(params)
		if err != nil {
			return err
		}
		if _, err := s.appendEvents(ctx, scope, []domain.Event{ev}); err != nil {
			return err
		}
		if hasOpen {
			if _, err := s.enqueueNotify(ctx, scope, []notifyRequest{{
				groupID:      occ.GroupID(),
				reason:       reasonComment,
				alertID:      ptr(alert.ID()),
				occurrenceID: ptr(occ.ID()),
				actor:        actor.Label(),
			}}, nil); err != nil {
				return err
			}
		}
		out = ev
		return nil
	})
	if err != nil {
		return domain.Event{}, err
	}
	return out, nil
}

// commentSummary renders the timeline one-liner once, at write time, so reading a
// timeline never needs the renderer.
func commentSummary(who, body string) string {
	line := body
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	summary := who + " commented: " + line
	if len(summary) > domain.MaxEventSummaryBytes {
		summary = summary[:domain.MaxEventSummaryBytes-1] + "…"
	}
	return summary
}

// ------------------------------------------------------------------- snooze

// Snooze asks oto to be quiet about ONE Alert until a fixed time (§B.8.3).
//
// ⛔ It changes nothing in the cluster, nothing upstream and nothing about the
// signal. The alert stays firing, stays whatever severity it was, and every
// surface keeps rendering it that way — snoozed alerts are NOT hidden from the
// default list (§B.8.6).
//
// In one transaction: close any active snooze as `superseded`, insert the new
// row, write the `alerts.snoozed_until` projection, append `alert.snoozed`, and
// enqueue `notify.evaluate(reason=snoozed)` so the channel is TOLD it is going
// quiet. A snooze that does not announce itself is the silent suppression §B.6
// forbids, which is why that last step is not optional.
func (s *Service) Snooze(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, actor domain.Actor,
	until time.Time, note string,
) (domain.Snooze, error) {
	if actor.IsZero() || !actor.Kind().IsHuman() {
		return domain.Snooze{}, errs.Validation("actor_required",
			"a snooze requires a human actor: it is always attributed")
	}

	var out domain.Snooze
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		alert, err := s.alerts.GetByID(ctx, scope, alertID)
		if err != nil {
			return err
		}
		now := s.Now()
		at, err := domain.NewObservationTime(now, now)
		if err != nil {
			return err
		}

		var events []domain.Event

		// Exactly one active snooze per alert. The partial unique index
		// alert_snoozes_active_idx enforces it; this closes the incumbent in the
		// SAME transaction so a 23505 here is a concurrency bug, never a race.
		if active, ok, err := s.snoozes.GetActive(ctx, scope, alertID); err != nil {
			return err
		} else if ok {
			_, evs, err := s.endSnooze(ctx, scope, active, actor, domain.SnoozeEndedSuperseded, at)
			if err != nil {
				return err
			}
			events = append(events, evs...)
		}

		snoozeID := id.New()
		// The domain factory proves the §B.8.3 bounds (5 minutes to 30 days,
		// never indefinite) and mints the event whose payload names the snooze.
		// The id is minted here so the row and the event agree.
		_, evs, err := domain.StartSnooze(alert, domain.SnoozeCommand{
			ID:      snoozeID,
			Actor:   actor,
			At:      at,
			Until:   until,
			Note:    note,
			EventID: id.New(),
		})
		if err != nil {
			return err
		}

		created, err := s.createSnooze(ctx, scope, snoozeID, domain.SnoozeRequest{
			AlertID: alertID,
			Until:   until,
			By:      nilID(actorUUID(actor)),
			ByLabel: actor.Label(),
			Note:    strPtr(note),
		})
		if err != nil {
			return err
		}
		events = append(events, evs...)

		if err := s.writeSnoozeProjection(ctx, scope, alert, ptr(created.SnoozedUntil())); err != nil {
			return err
		}
		if _, err := s.appendEvents(ctx, scope, events); err != nil {
			return err
		}
		if err := s.publishAlert(ctx, scope, alert.ID(), map[string]any{
			"snoozed_until": created.SnoozedUntil(),
		}); err != nil {
			return err
		}
		if err := s.notifySnoozeChange(ctx, scope, alert.ID(), reasonSnoozed, actor.Label()); err != nil {
			return err
		}

		out = created
		return nil
	})
	if err != nil {
		return domain.Snooze{}, err
	}
	return out, nil
}

// Unsnooze ends an active snooze early (§B.8.3).
func (s *Service) Unsnooze(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, actor domain.Actor,
) (domain.Snooze, error) {
	if actor.IsZero() || !actor.Kind().IsHuman() {
		return domain.Snooze{}, errs.Validation("actor_required", "an unsnooze requires a human actor")
	}

	var out domain.Snooze
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		alert, err := s.alerts.GetByID(ctx, scope, alertID)
		if err != nil {
			return err
		}
		active, ok, err := s.snoozes.GetActive(ctx, scope, alertID)
		if err != nil {
			return err
		}
		if !ok {
			return errs.Precondition("not_snoozed", "this alert is not snoozed")
		}

		now := s.Now()
		at, err := domain.NewObservationTime(now, now)
		if err != nil {
			return err
		}

		ended, evs, err := s.endSnooze(ctx, scope, active, actor, domain.SnoozeEndedManual, at)
		if err != nil {
			return err
		}
		if err := s.writeSnoozeProjection(ctx, scope, alert, nil); err != nil {
			return err
		}
		if _, err := s.appendEvents(ctx, scope, evs); err != nil {
			return err
		}
		if err := s.publishAlert(ctx, scope, alert.ID(), map[string]any{"snoozed_until": nil}); err != nil {
			return err
		}
		if err := s.notifySnoozeChange(ctx, scope, alert.ID(), reasonUnsnoozed, actor.Label()); err != nil {
			return err
		}

		out = ended
		return nil
	})
	if err != nil {
		return domain.Snooze{}, err
	}
	return out, nil
}

// endSnooze closes one snooze row and returns the `alert.unsnoozed` event.
//
// The domain decides whether the actor and the reason agree — an `expired` end is
// the system's and never a human's — and this method only persists what it
// approved.
func (s *Service) endSnooze(
	ctx context.Context, scope db.TenantScope, snz domain.Snooze, actor domain.Actor,
	reason domain.SnoozeEndReason, at domain.ObservationTime,
) (domain.Snooze, []domain.Event, error) {
	next, evs, err := snz.End(domain.UnsnoozeCommand{
		Actor:   actor,
		At:      at,
		Reason:  reason,
		EventID: id.New(),
	})
	if err != nil {
		return domain.Snooze{}, nil, err
	}

	end := domain.SnoozeEnd{
		SnoozeID: next.ID(),
		Reason:   reason.String(),
		At:       next.EndedAt(),
	}
	if actor.Kind().IsHuman() {
		end.By = nilID(actorUUID(actor))
		label := actor.Label()
		end.ByLabel = &label
	}
	persisted, err := s.snoozes.End(ctx, scope, end)
	if err != nil {
		return domain.Snooze{}, nil, err
	}
	return persisted, evs, nil
}

// createSnooze prefers the id-carrying form so the row and the `alert.snoozed`
// event name the same snooze. A repository that mints its own id is still
// supported, at the cost of an event whose payload points at a different row —
// which is why the interface assertion is tried first.
func (s *Service) createSnooze(
	ctx context.Context, scope db.TenantScope, snoozeID uuid.UUID, req domain.SnoozeRequest,
) (domain.Snooze, error) {
	if withID, ok := s.snoozes.(interface {
		CreateWithID(ctx context.Context, s db.TenantScope, id uuid.UUID, in domain.SnoozeRequest) (domain.Snooze, error)
	}); ok {
		return withID.CreateWithID(ctx, scope, snoozeID, req)
	}
	return s.snoozes.Create(ctx, scope, req)
}

// writeSnoozeProjection writes `alerts.snoozed_until` and NOTHING else.
//
// It prefers the narrow port for exactly that reason: SetProjection could move
// state and ack_state too, and a snooze that could move them is a snooze that one
// day will.
func (s *Service) writeSnoozeProjection(
	ctx context.Context, scope db.TenantScope, alert domain.Alert, until *time.Time,
) error {
	if s.snoozeProj != nil {
		return s.snoozeProj.SetSnoozedUntil(ctx, scope, alert.ID(), until)
	}
	return s.alerts.SetProjection(ctx, scope, alert.ID(), domain.AlertProjection{
		State:               alert.State(),
		CurrentOccurrenceID: nilID(alert.CurrentOccurrenceID()),
		AckState:            alert.AckState(),
		SnoozedUntil:        until,
		LastSeenAt:          alert.LastSeenAt(),
		LastStateChangeAt:   alert.LastStateChangeAt(),
		TotalOccurrences:    alert.TotalOccurrences(),
	})
}

// notifySnoozeChange announces a snooze beginning or ending.
//
// ⭐ `snoozed` and `unsnoozed` are the ONLY two notification reasons a snooze does
// not itself suppress (§B.8.4). A snooze that cannot announce its own beginning
// and end is the silent suppression §B.6 forbids.
func (s *Service) notifySnoozeChange(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, reason, actor string,
) error {
	occ, ok, err := s.occurrences.GetOpenByAlert(ctx, scope, alertID)
	if err != nil {
		return err
	}
	if !ok {
		// Nothing is running, so there is no group card to amend. The
		// alert_snoozes row and the timeline event still record the fact.
		return nil
	}
	_, err = s.enqueueNotify(ctx, scope, []notifyRequest{{
		groupID:      occ.GroupID(),
		reason:       reason,
		alertID:      ptr(alertID),
		occurrenceID: ptr(occ.ID()),
		actor:        actor,
	}}, nil)
	return err
}

// actorUUID reads a human actor's id as a UUID, yielding uuid.Nil for a Slack
// user id or any other non-UUID identity — those live on the event's actor_id,
// not on a foreign key.
func actorUUID(a domain.Actor) uuid.UUID {
	v, err := uuid.Parse(a.ID())
	if err != nil {
		return uuid.Nil
	}
	return v
}
