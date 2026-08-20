package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/idempotency"
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

// Acknowledge records that a human has seen ONE FIRING EPISODE (T9).
//
// ⭐⭐ IT IS ADDRESSED BY CASE ID, AND THAT IS THE WHOLE POINT OF THE VERB. An
// acknowledgement is a receipt for one contiguous firing of one alert; it is
// written on `alert_cases`, it is cleared when the next episode opens (T10,
// `reason: new_case`), and the Alert carries no ack column for it to land on
// (§B.1, 00049). Taking an alert id and looking up "whatever episode happens to
// be open right now" made the SUBJECT of the receipt a race: an operator reading
// a case that resolved a second ago, pressing Acknowledge, and signing for the
// episode that replaced it.
//
// Acknowledgement is ORTHOGONAL to state: an acked case is still firing, still
// whatever severity it was, and every surface keeps rendering it that way.
// Acknowledging an episode that has already ended is a PRECONDITION failure
// (`case_terminal`) — the request is well-formed, the entity is simply in the
// wrong state — and an id that names no case in this tenant is a 404.
func (s *Service) Acknowledge(
	ctx context.Context, scope db.TenantScope, caseID uuid.UUID, actor domain.Actor, note string,
) (domain.Case, error) {
	return s.setAck(ctx, scope, caseID, actor, note, true, domain.UnackReasonManual)
}

// Unacknowledge drops an acknowledgement a human placed (T10, reason `manual`).
//
// `note` is the human's explanation for the withdrawal. It does NOT go back onto
// the case — `ack_note` describes the acknowledgement being removed and is
// cleared by the transition — it goes onto the timeline, in the
// `case.unacknowledged` event payload.
func (s *Service) Unacknowledge(
	ctx context.Context, scope db.TenantScope, caseID uuid.UUID, actor domain.Actor, note string,
) (domain.Case, error) {
	return s.setAck(ctx, scope, caseID, actor, note, false, domain.UnackReasonManual)
}

// setAck is the body of both verbs.
//
// ⭐ THE CASE IS READ FIRST AND THE ALERT IS REACHED THROUGH IT. The episode is
// the subject; the alert is read only because the projection written at the end
// of this transaction (`state`, `current_case_id`) is a fact about the identity
// and has to be re-stated from a row that was read under the same lock. Reading
// the case by id also means the tenancy check happens once, on the row that is
// about to be written, rather than on a parent that could own a different one.
func (s *Service) setAck(
	ctx context.Context, scope db.TenantScope, caseID uuid.UUID, actor domain.Actor,
	note string, ack bool, reason string,
) (domain.Case, error) {
	if actor.IsZero() || !actor.Kind().IsHuman() {
		return domain.Case{}, errs.Validation("actor_required",
			"an acknowledgement requires a human actor")
	}

	var out domain.Case
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		ac, err := s.cases.GetByID(ctx, scope, caseID)
		if err != nil {
			return err
		}
		alert, err := s.alerts.GetByID(ctx, scope, ac.AlertID())
		if err != nil {
			return err
		}
		// ⛔ AN ENDED EPISODE REFUSES BOTH VERBS, AND THE GUARD IS HERE BECAUSE
		// THE DOMAIN ONLY CARRIES HALF OF IT. `Case.Acknowledge` refuses a
		// terminal state on its own; `Case.Unacknowledge` deliberately does not,
		// because T10's AUTOMATIC withdrawal has to run on the episode that is
		// closing (lifecycle.go). A human withdrawal is the other caller, and
		// letting it act on a finished episode would re-state
		// `alerts.current_case_id` — which the projection below writes
		// unconditionally — back onto a case the alert has already moved past.
		if !ac.IsOpen() {
			return errs.Precondition("case_terminal",
				"this episode has ended; its acknowledgement is now a record of what happened")
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
			next domain.Case
			evs  []domain.Event
		)
		if ack {
			next, evs, err = ac.Acknowledge(cmd)
		} else {
			next, evs, err = ac.Unacknowledge(cmd)
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
		// The version `ac` was read at. If the episode resolved or expired while
		// the human was deciding, this fails with a conflict rather than stamping
		// an acknowledgement on a closed episode and rewinding the alert
		// projection to its pre-resolution state.
		if err := s.cases.SetAck(ctx, scope, next.ID(), change, ac.StateVersion()); err != nil {
			return err
		}

		// ⭐ THE ACK ITSELF IS ALREADY WRITTEN, on the case, by SetAck above.
		// This projection re-states `state` and `current_case_id` and moves
		// NOTHING else: `alerts` carries no ack column, because a receipt for one
		// firing must not outlive that firing, and no snooze column, because a
		// quiet period outlives every one of them (§B.1).
		if err := s.alerts.SetProjection(ctx, scope, alert.ID(), domain.AlertProjection{
			State:             alert.State(),
			CurrentCaseID:     ptr(next.ID()),
			LastSeenAt:        alert.LastSeenAt(),
			LastStateChangeAt: alert.LastStateChangeAt(),
			TotalCases:        alert.TotalCases(),
		}); err != nil {
			return err
		}

		if _, err := s.appendEvents(ctx, scope, evs); err != nil {
			return err
		}
		if err := s.publishCase(ctx, scope, next); err != nil {
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
			reason:  notifyReason,
			alertID: ptr(alert.ID()),
			caseID:  next.ID(),
			actor:   actor.Label(),
		}}, nil); err != nil {
			return err
		}

		out = next
		return nil
	})
	if err != nil {
		return domain.Case{}, err
	}
	return out, nil
}

// Comment adds a human note to an Alert's timeline (T14).
//
// It is an ANNOTATION, never a state change: nothing in this method touches
// state, ack_state, severity or the snooze projection.
//
// ⭐⭐ A KEYED RETRY REPLAYS THE ORIGINAL ANNOTATION INSTEAD OF APPENDING A
// SECOND, and the second return value is which of the two happened. A comment is
// an APPEND and has no state machine to refuse a repeat, so before ticket a6cc834
// it was one of exactly three operations in oto that genuinely duplicated a side
// effect on retry — and the §C.8 key it already carried could not catch one,
// because it was minted from the wall clock and a retry arrives at a different
// nanosecond.
func (s *Service) Comment(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, actor domain.Actor,
	body string, idem Idempotency,
) (domain.Event, bool, error) {
	if actor.IsZero() || !actor.Kind().IsHuman() {
		return domain.Event{}, false, errs.Validation("actor_required", "a comment requires a human actor")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return domain.Event{}, false, errs.Validation("comment_required", "a comment must not be blank")
	}
	if len(body) > MaxCommentBytes {
		return domain.Event{}, false, errs.Newf(errs.KindValidation, "max_length",
			"a comment must have at most %d characters", MaxCommentBytes)
	}
	if err := idempotency.Require(idem, s.claims, s.tx); err != nil {
		return domain.Event{}, false, err
	}

	// The keyed §C.8 key is derived OUTSIDE the transaction because the replay
	// read below happens outside it too: a replay rolls this whole unit of work
	// back, and the key is the handle that finds what the first attempt wrote.
	keyedDedupe := ""
	if idem.Keyed {
		keyedDedupe = commentDedupeKey(alertID, idem)
	}

	var out domain.Event
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		alert, err := s.alerts.GetByID(ctx, scope, alertID)
		if err != nil {
			return err
		}
		ac, hasOpen, err := s.cases.GetOpenByAlert(ctx, scope, alertID)
		if err != nil {
			return err
		}

		now := s.Now()
		at, err := domain.NewObservationTime(now, now)
		if err != nil {
			return err
		}

		// ⭐ THE CLOCK KEY IS THE UNKEYED FORM AND ONLY THE UNKEYED FORM. It gives
		// the append a §C.8 claim of its own so that two comments minted inside
		// the same nanosecond cannot collide, and it does NOTHING for a retry: a
		// second HTTP request reads a different `now`, computes a different key,
		// and `alert_event_keys` sees no repeat.
		//
		// ⛔ NEITHER FORM IS A HASH OF THE BODY. Two people who type "restarted
		// it" ten minutes apart wrote two facts, and a content key would silently
		// discard the second — losing a human's words is worse than keeping one
		// too many. Retry safety belongs to the caller's `Idempotency-Key`, which
		// is a handle on ONE gesture rather than on a sentence, and which is what
		// keyedDedupe is derived from.
		dedupe := "comment:" + alert.ID().String() + ":" + now.Format(time.RFC3339Nano)
		if keyedDedupe != "" {
			dedupe = keyedDedupe
		}

		params := domain.EventParams{
			ID:        id.New(),
			OrgID:     scope.OrgID(),
			AlertID:   alert.ID(),
			Type:      domain.EventCommentAdded,
			At:        at,
			Actor:     actor,
			Summary:   commentSummary(actor.Label(), body),
			Payload:   map[string]any{"body": body},
			DedupeKey: dedupe,
		}
		if hasOpen {
			params.CaseID = ac.ID()
		}

		ev, err := domain.NewEvent(params)
		if err != nil {
			return err
		}
		if idem.Keyed {
			// ⭐ BEFORE THE APPEND, because a replay must not have appended
			// anything to roll back — and because the claim records what this call
			// creates, which is the event id the caller's `201` would have carried.
			if _, err := idempotency.Resolve(ctx, s.claims, scope, idem,
				idempotency.Replay, ev.ID(), s.Now()); err != nil {
				return err
			}
		}
		if _, err := s.appendEvents(ctx, scope, []domain.Event{ev}); err != nil {
			return err
		}
		if hasOpen {
			if _, err := s.enqueueNotify(ctx, scope, []notifyRequest{{
				reason:  reasonComment,
				alertID: ptr(alert.ID()),
				caseID:  ac.ID(),
				actor:   actor.Label(),
			}}, nil); err != nil {
				return err
			}
		}
		out = ev
		return nil
	})
	if errors.Is(err, idempotency.ErrReplay) {
		// The transaction above is rolled back, so this read sees the world the
		// FIRST attempt committed and nothing this one did.
		ev, found, readErr := s.events.GetByDedupeKey(ctx, scope, keyedDedupe)
		if readErr != nil {
			return domain.Event{}, false, readErr
		}
		if !found {
			// The claim exists and the annotation it names does not — the event
			// aged out of `alert_events`, or its key row was pruned ahead of it.
			// Refusing is the honest answer and carries the same code, so a client
			// still learns that its first attempt succeeded.
			return domain.Event{}, false, errs.Conflict(idempotency.CodeReuse,
				"this Idempotency-Key was already used and that comment was appended; "+
					"it is no longer readable, so it cannot be returned a second time")
		}
		return ev, true, nil
	}
	if err != nil {
		return domain.Event{}, false, err
	}
	return out, false, nil
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
// row, append `alert.snoozed`, and
// enqueue `notify.evaluate(reason=snoozed)` so the channel is TOLD it is going
// quiet. A snooze that does not announce itself is the silent suppression §B.6
// forbids, which is why that last step is not optional.
//
// ⭐⭐ A KEYED RETRY REPLAYS THE SNOOZE IT ALREADY GRANTED, and the second return
// value is which of the two happened. Nothing here ever asked "have I already
// granted this snooze": a retry found its own incumbent, ended it as
// `superseded`, inserted a second row, and announced the quiet period AGAIN — one
// user click, two rows and two outbound Slack messages. That was the sharpest of
// the three genuine duplications ticket a6cc834 names, because it fires during
// exactly the network conditions the header exists for.
func (s *Service) Snooze(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, actor domain.Actor,
	until time.Time, note string, idem Idempotency,
) (domain.Snooze, bool, error) {
	if actor.IsZero() || !actor.Kind().IsHuman() {
		return domain.Snooze{}, false, errs.Validation("actor_required",
			"a snooze requires a human actor: it is always attributed")
	}
	if err := idempotency.Require(idem, s.claims, s.tx); err != nil {
		return domain.Snooze{}, false, err
	}

	var (
		out      domain.Snooze
		replayOf uuid.UUID
	)
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

		// The id is minted here rather than beside the domain command below
		// because the claim has to name it, and the claim has to be taken BEFORE
		// the incumbent is superseded: a replay that discovered itself afterwards
		// would already have ended the very snooze it is about to replay, and the
		// rollback that undoes that is a promise about the database rather than
		// about the notification `endSnooze` would have enqueued.
		snoozeID := id.New()
		if idem.Keyed {
			ref, err := idempotency.Resolve(ctx, s.claims, scope, idem,
				idempotency.Replay, snoozeID, s.Now())
			if err != nil {
				replayOf = ref
				return err
			}
		}

		var events []domain.Event

		// Exactly one active snooze per alert. The partial unique index
		// alert_snoozes_active_idx enforces it; this closes the incumbent in the
		// SAME transaction so a 23505 here is a concurrency bug, never a race.
		if active, ok, err := s.snoozes.GetActive(ctx, scope, alertID); err != nil {
			return err
		} else if ok {
			// No note: the incumbent is being replaced, not woken, and the note the
			// human typed belongs to the snooze they are creating.
			_, evs, err := s.endSnooze(ctx, scope, active, actor, domain.SnoozeEndedSuperseded, at, "")
			if err != nil {
				return err
			}
			events = append(events, evs...)
		}

		// The domain factory proves the §B.8.3 bounds (5 minutes to 30 days,
		// never indefinite) and mints the event whose payload names the snooze.
		// The id was minted above so the row, the event and the claim all agree.
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
	if errors.Is(err, idempotency.ErrReplay) {
		// The transaction above is rolled back, so the incumbent this read finds
		// is the snooze the FIRST attempt granted. Its id has to match the one the
		// claim recorded: an alert that has since been unsnoozed and snoozed again
		// has a DIFFERENT live snooze, and handing that one back as the caller's
		// own would be a lie about which quiet period they are holding.
		active, ok, readErr := s.snoozes.GetActive(ctx, scope, alertID)
		if readErr != nil {
			return domain.Snooze{}, false, readErr
		}
		if ok && active.ID() == replayOf {
			return active, true, nil
		}
		return domain.Snooze{}, false, errs.Conflict(idempotency.CodeReuse,
			"this Idempotency-Key was already used and that snooze was granted; it has since "+
				"ended, so it cannot be returned. Retry with a new key if you meant a new snooze")
	}
	if err != nil {
		return domain.Snooze{}, false, err
	}
	return out, false, nil
}

// Unsnooze ends an active snooze early (§B.8.3).
//
// `note` is recorded with the wake-up, in the `alert.unsnoozed` event payload.
func (s *Service) Unsnooze(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, actor domain.Actor, note string,
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

		ended, evs, err := s.endSnooze(ctx, scope, active, actor, domain.SnoozeEndedManual, at, note)
		if err != nil {
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

// UnsnoozeOutcome is what a bulk wake concluded about ONE alert.
type UnsnoozeOutcome struct {
	AlertID uuid.UUID
	// Woken is true when this alert had an active snooze and now does not.
	Woken bool
	// Code is the stable errs code of the refusal, and empty when Woken.
	//
	// ⭐ IT IS THE CODE AND NOT A BOOLEAN, for the reason grouping's
	// FanOutResult.SkippedCodes gives: "nothing happened" has more than one honest
	// explanation, and a surface that has to tell a person which one it was cannot
	// get it from a count. `not_snoozed` means somebody already woke this alert;
	// `alert_not_found` means no such alert in this org.
	Code string
}

// UnsnoozeManyResult is the account of a bulk wake.
//
// ⛔ IT IS AN ACCOUNT AND NOT A COUNT, and for the same reason FanOutResult is:
// a partial result is the NORMAL one here. An operator waking a page of quiet
// alerts routinely finds some of them already awake, and a bare "3" cannot tell
// them which two were left and why.
type UnsnoozeManyResult struct {
	// Outcomes carries one entry per DISTINCT requested alert, in request order.
	Outcomes []UnsnoozeOutcome
}

// Woken is how many alerts this call actually put back on the air.
func (r UnsnoozeManyResult) Woken() int {
	n := 0
	for _, o := range r.Outcomes {
		if o.Woken {
			n++
		}
	}
	return n
}

// Skipped is how many refused, for any reason.
func (r UnsnoozeManyResult) Skipped() int { return len(r.Outcomes) - r.Woken() }

// UnsnoozeMany serves `POST /api/v1/alerts/unsnooze`: end the active snooze on
// each alert the CALLER NAMED.
//
// ⛔⛔ IT TAKES A LIST AND IT WILL NEVER TAKE A FILTER. A filter-scoped bulk wake
// is evaluated on the server against rows the caller never saw: one press would
// resume thousands of alerts whose extent the person pressing it cannot see, and
// every one of them would start notifying channels nobody agreed to wake. The
// caller must name what it is waking. That is a bound the server can check, and a
// record a person can read back afterwards.
//
// ⛔ THERE IS NO SnoozeMany BESIDE IT AND THERE MUST NOT BE. This is the UNDO of a
// gesture somebody made deliberately, one alert at a time. Going quiet in bulk is
// the blindfold §B.8.3 refuses on the group verb — a mute over alerts nobody has
// looked at — and the undo carries no such risk: its failure mode is oto talking
// more, which is the direction §B.6 wants a mistake to fall.
//
// ⭐ IT IS A FAN-OUT OF THE PRIMITIVE, exactly as the group verbs are: each id
// goes through `Unsnooze` verbatim, so the row, the `alert.unsnoozed` event, the
// projection and the resume notification are the SAME ones the single-alert route
// writes. That is what makes "consistent with the single endpoint" a property of
// the code rather than a promise in a comment — there is only one implementation.
//
// ⛔ IT ADDS NOTHING TO THE REPOSITORY, AND THE ABSENCE IS THE DESIGN. The group
// fan-out needed `CurrentMemberAlerts` because it had to DISCOVER its subjects;
// this one is handed them, so there is no candidate read to add and no batch
// UPDATE to write. One write transaction per alert is what §E.1.1's verbs are, and
// a single statement over a hundred rows would have to skip the domain, the event
// and the notification to be faster than the thing it replaced.
//
// ⛔ THE LIST'S LENGTH IS BOUNDED AT THE EDGE AND NOT HERE, like every other
// list-valued input in this API: `api.MaxUnsnoozeAlertIDs` is a `validate` tag on
// the request DTO and a `maxItems` in the contract, which is where a caller can be
// told which field was too long.
//
// ⚠️ THE ACCOUNT SURVIVES THE ERROR. A hard failure partway returns the partial
// result ALONGSIDE the error rather than a zero value: the alerts already woken
// are committed and are not coming back, and a result that forgot them would be
// the only record of them lost. A caller that retries reaches the same end state —
// the alerts that woke report `not_snoozed` on the second pass — which is why this
// verb needs no idempotency key to be safe to repeat.
func (s *Service) UnsnoozeMany(
	ctx context.Context, scope db.TenantScope, alertIDs []uuid.UUID,
	actor domain.Actor, note string,
) (UnsnoozeManyResult, error) {
	if actor.IsZero() || !actor.Kind().IsHuman() {
		return UnsnoozeManyResult{}, errs.Validation("actor_required",
			"an unsnooze requires a human actor")
	}

	res := UnsnoozeManyResult{Outcomes: make([]UnsnoozeOutcome, 0, len(alertIDs))}
	// The request DTO already refuses a repeated id, so this never fires in
	// production. It stays because the verb is about the SIGNAL and must be applied
	// once per signal whatever the caller sent — the same argument grouping's
	// fanOut makes about its own candidate read. Applying it twice would report one
	// alert as both `woken` and `not_snoozed`, which is an account that does not add
	// up.
	seen := make(map[uuid.UUID]struct{}, len(alertIDs))

	for _, alertID := range alertIDs {
		if _, dup := seen[alertID]; dup {
			continue
		}
		seen[alertID] = struct{}{}

		if _, err := s.Unsnooze(ctx, scope, alertID, actor, note); err != nil {
			if errs.IsKind(err, errs.KindPrecondition) || errs.IsKind(err, errs.KindNotFound) {
				// ⭐ A REFUSAL IS RECORDED, NOT RAISED. An alert that was not snoozed
				// is SKIPPED: refusing the other ninety-nine because one had already
				// woken makes the button unusable in exactly the situation it exists
				// for, which is the rule the group unsnooze already follows.
				//
				// ⛔ `alert_not_found` IS A SKIP FOR THE SAME REASON IT IS A 404 ON THE
				// SINGLE ROUTE. Every read is scoped by db.TenantScope, so another
				// org's id is not refused — it simply is not there. Answering
				// differently for "absent" and "somebody else's" would be an existence
				// oracle, and one that a hundred ids per request could be walked
				// through quickly.
				code := errs.CodeOf(err)
				if code == "" {
					code = "refused"
				}
				res.Outcomes = append(res.Outcomes, UnsnoozeOutcome{AlertID: alertID, Code: code})
				continue
			}
			return res, err
		}
		res.Outcomes = append(res.Outcomes, UnsnoozeOutcome{AlertID: alertID, Woken: true})
	}
	return res, nil
}

// endSnooze closes one snooze row and returns the `alert.unsnoozed` event.
//
// The domain decides whether the actor and the reason agree — an `expired` end is
// the system's and never a human's — and this method only persists what it
// approved.
func (s *Service) endSnooze(
	ctx context.Context, scope db.TenantScope, snz domain.Snooze, actor domain.Actor,
	reason domain.SnoozeEndReason, at domain.ObservationTime, note string,
) (domain.Snooze, []domain.Event, error) {
	next, evs, err := snz.End(domain.UnsnoozeCommand{
		Actor:   actor,
		At:      at,
		Reason:  reason,
		Note:    note,
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

// ⛔ `writeSnoozeProjection` IS GONE, AND ITS ABSENCE IS THE POINT OF THIS
// CHANGE. It wrote `alerts.snoozed_until` from THREE places — Snooze, Unsnooze
// and the 60-second expiry sweep — so three transactions had to remember to keep
// a mirror in step with the row they had just written. The notification path then
// read the mirror rather than the row, which is how "should oto be quiet?" came
// to be answered by a bare timestamp that cannot name who asked, what they wrote,
// or how the quiet period ended.
//
// The row is now the only answer. `alert_snoozes` is written once per verb, the
// unique partial index `alert_snoozes_active_idx` enforces at most one live
// snooze per alert, and every reader — the list's two tabs, the group card, the
// suppression decision — joins to it.

// notifySnoozeChange announces a snooze beginning or ending.
//
// ⭐ `snoozed` and `unsnoozed` are the ONLY two notification reasons a snooze does
// not itself suppress (§B.8.4). A snooze that cannot announce its own beginning
// and end is the silent suppression §B.6 forbids.
func (s *Service) notifySnoozeChange(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, reason, actor string,
) error {
	ac, ok, err := s.cases.GetOpenByAlert(ctx, scope, alertID)
	if err != nil {
		return err
	}
	if !ok {
		// Nothing is running, so there is no group card to amend. The
		// alert_snoozes row and the timeline event still record the fact.
		return nil
	}
	_, err = s.enqueueNotify(ctx, scope, []notifyRequest{{
		reason:  reason,
		alertID: ptr(alertID),
		caseID:  ac.ID(),
		actor:   actor,
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
