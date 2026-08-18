package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Snooze is SPEC §B.8 — oto's quiet button.
//
// > Snooze suppresses OTO'S OWN NOTIFICATIONS for one `alert_key` until a fixed
// > time T. It changes nothing in the cluster, nothing upstream, and nothing
// > about the signal's state.
//
// ⭐⭐ SNOOZE IS NOW THE ONLY ONE. Storm collapse and the flap digest were oto's
// AUTOMATIC defences against its own noise (§B.6) and both are gone: the flap damper
// moved to case formation (migration 00057) and storm damping was removed outright.
// What survives is the MANUAL one, and that is the right survivor — a human asking for
// quiet is an explanation an operator can act on, where "oto decided this firing was not
// worth mentioning" makes a suppressed notification indistinguishable from a signal that
// never fired. Without snooze the only quiet button a user has is muting the Slack
// channel, which also hides the real incident.
//
// ⛔ THE THREE THINGS SNOOZE IS NOT, and each is a bug that has been written
// before:
//
//  1. Snooze is NOT a State. `State` must never gain a `snoozed` value. Snooze is
//     the THIRD ORTHOGONAL AXIS beside `state` and `ack_state` (§B.1). An alert
//     can be firing AND acked AND snoozed at once, and all three are displayed.
//  2. Snooze is NOT a SuppressionReason. `alert_cases.suppression_reason`
//     mirrors ALERTMANAGER'S FOUR REASONS and nothing else. Adding `snoozed` to
//     it would make oto report "Alertmanager is suppressing this" when the truth
//     is "a human asked oto to be quiet" — a lie about the world, in the one
//     table whose job is to mirror the world. Snooze records itself as a
//     NOTIFICATION suppressed_reason instead (see SuppressorPrecedence).
//  3. Snooze NEVER alters severity, colour or state on any surface. A snoozed
//     firing critical is still `#a30200` / `:rotating_light:` (§B.8.6). Colouring
//     it calm would be the same lie §E.1.1 exists to prevent.
//
// Both facts — "Alertmanager is suppressing this" and "oto is being quiet about
// this" — must always be representable SIMULTANEOUSLY, because they are facts
// about two different systems and neither overrides the other.
type Snooze struct {
	id       uuid.UUID
	orgID    uuid.UUID
	alertID  uuid.UUID
	alertKey AlertKey

	snoozedAt      time.Time
	snoozedUntil   time.Time
	snoozedBy      uuid.UUID
	snoozedByLabel string
	note           string

	endedAt      time.Time
	endedReason  SnoozeEndReason
	endedBy      uuid.UUID
	endedByLabel string
}

// Snooze bounds, binding (§B.8.3) and byte-identical to the alert_snoozes
// CHECKs. A bound lives in three places — DTO tag, domain constructor, DDL
// CHECK — and they must be IDENTICAL (R9).
const (
	// MinSnoozeDuration mirrors alert_snoozes_min_ck. Five minutes, because a
	// shorter snooze is a click that achieves nothing.
	MinSnoozeDuration = 5 * time.Minute
	// MaxSnoozeDuration mirrors alert_snoozes_max_ck. THERE IS NO INDEFINITE
	// SNOOZE: an unexpiring snooze is a mute, and mutes are how channels die.
	MaxSnoozeDuration = 30 * 24 * time.Hour
	// MaxSnoozeNoteBytes mirrors alert_snoozes_note_ck.
	MaxSnoozeNoteBytes = 2000
	// MaxSnoozeLabelBytes mirrors alert_snoozes_label_ck.
	MaxSnoozeLabelBytes = 120
)

// SnoozePresets are the durations the Slack buttons and the UI menu offer
// (§B.8.3). There is deliberately no "indefinite" entry, and adding one requires
// a SPEC amendment, not a UI change.
func SnoozePresets() []time.Duration {
	return []time.Duration{
		30 * time.Minute,
		1 * time.Hour,
		4 * time.Hour,
		24 * time.Hour,
		7 * 24 * time.Hour,
	}
}

// SnoozeEndReason says how a snooze stopped (alert_snoozes_endmap_ck).
type SnoozeEndReason struct{ s string }

// The closed SnoozeEndReason set (§B.8.3).
var (
	// SnoozeEndedExpired is the snooze.expire job finding the clock has run out.
	// Its actor is `system`; every other reason has a human behind it (§B.8.5).
	SnoozeEndedExpired = SnoozeEndReason{"expired"}
	// SnoozeEndedManual is a human unsnoozing.
	SnoozeEndedManual = SnoozeEndReason{"manual"}
	// SnoozeEndedSuperseded is an older snooze being closed because a new one
	// replaced it. Exactly one snooze per alert is active, and the partial unique
	// index alert_snoozes_active_idx — not application code — is what enforces it.
	SnoozeEndedSuperseded = SnoozeEndReason{"superseded"}
)

// NewSnoozeEndReason parses a persisted end reason.
func NewSnoozeEndReason(s string) (SnoozeEndReason, error) {
	switch s {
	case SnoozeEndedExpired.s, SnoozeEndedManual.s, SnoozeEndedSuperseded.s:
		return SnoozeEndReason{s: s}, nil
	default:
		return SnoozeEndReason{}, errs.Newf(errs.KindValidation, "enum",
			"snooze ended_reason must be one of: expired, manual, superseded (got %q)", s)
	}
}

// String renders the end reason.
func (r SnoozeEndReason) String() string { return r.s }

// IsZero reports whether the snooze has not ended.
func (r SnoozeEndReason) IsZero() bool { return r.s == "" }

// SnoozeParams is the full constructor input for a Snooze. It is also the
// rehydration shape: a repository maps a row into it and the constructor
// re-proves every invariant.
type SnoozeParams struct {
	ID       uuid.UUID
	OrgID    uuid.UUID
	AlertID  uuid.UUID
	AlertKey AlertKey

	SnoozedAt    time.Time
	SnoozedUntil time.Time
	// SnoozedBy is the acting user, or uuid.Nil when the row survived the user
	// being deleted (ON DELETE SET NULL). The LABEL is what history reads from.
	SnoozedBy      uuid.UUID
	SnoozedByLabel string
	Note           string

	EndedAt      time.Time
	EndedReason  SnoozeEndReason
	EndedBy      uuid.UUID
	EndedByLabel string
}

// NewSnooze builds a Snooze, enforcing every §D.8b invariant. If you can
// construct it, it is valid — there is no optional Validate() in this package.
func NewSnooze(p SnoozeParams) (Snooze, error) {
	if err := requireID("snooze id", p.ID); err != nil {
		return Snooze{}, err
	}
	if err := requireID("org_id", p.OrgID); err != nil {
		return Snooze{}, err
	}
	if err := requireID("alert_id", p.AlertID); err != nil {
		return Snooze{}, err
	}
	if p.AlertKey.IsZero() {
		return Snooze{}, errs.New(errs.KindValidation, "required", "alert_key is required")
	}
	if p.SnoozedAt.IsZero() {
		return Snooze{}, errs.New(errs.KindValidation, "required", "snoozed_at is required")
	}
	if p.SnoozedUntil.IsZero() {
		// alert_snoozes.snoozed_until is NOT NULL, deliberately (§B.8.3).
		return Snooze{}, errs.New(errs.KindValidation, "required",
			"snoozed_until is required: there is no indefinite snooze")
	}
	if err := checkSnoozeWindow(p.SnoozedAt, p.SnoozedUntil); err != nil {
		return Snooze{}, err
	}

	label := strings.TrimSpace(p.SnoozedByLabel)
	if label == "" {
		return Snooze{}, errs.New(errs.KindValidation, "not_blank",
			"snoozed_by_label is required: a snooze is always attributed and visible")
	}
	if len(label) > MaxSnoozeLabelBytes {
		return Snooze{}, errs.Newf(errs.KindValidation, "max_length",
			"snoozed_by_label must have at most %d characters", MaxSnoozeLabelBytes)
	}
	if len(p.Note) > MaxSnoozeNoteBytes {
		return Snooze{}, errs.Newf(errs.KindValidation, "max_length",
			"snooze note must have at most %d characters", MaxSnoozeNoteBytes)
	}

	// ended_at and ended_reason are all-or-nothing (alert_snoozes_end_ck).
	if p.EndedAt.IsZero() != p.EndedReason.IsZero() {
		return Snooze{}, errs.New(errs.KindValidation, "field_order",
			"ended_at and ended_reason are set together or not at all")
	}
	if !p.EndedAt.IsZero() && p.EndedAt.Before(p.SnoozedAt) {
		return Snooze{}, errs.New(errs.KindValidation, "field_order",
			"ended_at must be >= snoozed_at")
	}
	if p.EndedBy != uuid.Nil && strings.TrimSpace(p.EndedByLabel) == "" {
		return Snooze{}, errs.New(errs.KindValidation, "required",
			"ended_by requires ended_by_label")
	}
	if len(p.EndedByLabel) > MaxSnoozeLabelBytes {
		return Snooze{}, errs.Newf(errs.KindValidation, "max_length",
			"ended_by_label must have at most %d characters", MaxSnoozeLabelBytes)
	}

	return Snooze{
		id:             p.ID,
		orgID:          p.OrgID,
		alertID:        p.AlertID,
		alertKey:       p.AlertKey,
		snoozedAt:      p.SnoozedAt.UTC(),
		snoozedUntil:   p.SnoozedUntil.UTC(),
		snoozedBy:      p.SnoozedBy,
		snoozedByLabel: label,
		note:           p.Note,
		endedAt:        utcOrZero(p.EndedAt),
		endedReason:    p.EndedReason,
		endedBy:        p.EndedBy,
		endedByLabel:   strings.TrimSpace(p.EndedByLabel),
	}, nil
}

// checkSnoozeWindow enforces alert_snoozes_order_ck, _min_ck and _max_ck as one
// predicate, so layer 3 and layer 6 cannot disagree about where the edges are.
func checkSnoozeWindow(from, until time.Time) error {
	d := until.Sub(from)
	switch {
	case d <= 0:
		return errs.New(errs.KindValidation, "field_order",
			"snoozed_until must be after snoozed_at")
	case d < MinSnoozeDuration:
		return errs.Newf(errs.KindValidation, "min",
			"a snooze must last at least %s", MinSnoozeDuration)
	case d > MaxSnoozeDuration:
		return errs.Newf(errs.KindValidation, "max",
			"a snooze must last at most %s: there is no indefinite snooze", MaxSnoozeDuration)
	default:
		return nil
	}
}

// ID is the Snooze's uuidv7.
func (s Snooze) ID() uuid.UUID { return s.id }

// OrgID is the tenant this Snooze belongs to.
func (s Snooze) OrgID() uuid.UUID { return s.orgID }

// AlertID is the Alert whose notifications are quiet.
func (s Snooze) AlertID() uuid.UUID { return s.alertID }

// AlertKey is the denormalised alert identity, kept so the audit trail survives.
func (s Snooze) AlertKey() AlertKey { return s.alertKey }

// SnoozedAt is when the quiet began, on oto's clock.
func (s Snooze) SnoozedAt() time.Time { return s.snoozedAt }

// SnoozedUntil is when oto's notifications resume. Never zero.
func (s Snooze) SnoozedUntil() time.Time { return s.snoozedUntil }

// SnoozedBy is the acting user, or uuid.Nil if that user has since been deleted.
func (s Snooze) SnoozedBy() uuid.UUID { return s.snoozedBy }

// SnoozedByLabel is the immutable display name. Attribution is not optional: a
// snooze is always visible and always attributed (§B.8.1).
func (s Snooze) SnoozedByLabel() string { return s.snoozedByLabel }

// Note is the free-text justification, possibly empty.
func (s Snooze) Note() string { return s.note }

// EndedAt is when the snooze stopped, or the zero time while it is active.
func (s Snooze) EndedAt() time.Time { return s.endedAt }

// EndedReason is why it stopped, or the zero reason while it is active.
func (s Snooze) EndedReason() SnoozeEndReason { return s.endedReason }

// EndedBy is who ended it, or uuid.Nil for an expiry.
func (s Snooze) EndedBy() uuid.UUID { return s.endedBy }

// EndedByLabel is the immutable display name of whoever ended it.
func (s Snooze) EndedByLabel() string { return s.endedByLabel }

// Duration is how long the snooze was asked to last.
func (s Snooze) Duration() time.Duration { return s.snoozedUntil.Sub(s.snoozedAt) }

// IsOpen reports whether the row is the alert's active snooze — which is exactly
// what alert_snoozes_active_idx is partial on. It is a fact about the ROW, not
// about the clock: an open row whose `snoozed_until` has passed is one the
// snooze.expire job has not swept yet.
func (s Snooze) IsOpen() bool { return s.endedAt.IsZero() }

// IsActiveAt reports whether oto is holding its tongue at the given instant: the
// row is open AND the clock has not run out. The instant is a PARAMETER — the
// domain never calls time.Now().
func (s Snooze) IsActiveAt(now time.Time) bool {
	return s.IsOpen() && s.snoozedUntil.After(now.UTC())
}

// HasExpiredAt reports whether the snooze.expire job should sweep this row.
func (s Snooze) HasExpiredAt(now time.Time) bool {
	return s.IsOpen() && !s.snoozedUntil.After(now.UTC())
}

// RemainingAt is how much quiet is left, floored at zero, for the `:zzz:`
// countdown badge.
func (s Snooze) RemainingAt(now time.Time) time.Duration {
	if !s.IsActiveAt(now) {
		return 0
	}
	return s.snoozedUntil.Sub(now.UTC())
}

// SnoozeCommand asks oto to be quiet about one Alert until a fixed time.
type SnoozeCommand struct {
	// ID is the uuidv7 for the new alert_snoozes row.
	ID uuid.UUID
	// Actor must be a human: snooze is a deliberate act, and the row records who.
	Actor Actor
	// At carries both clocks. RecordedAt is `snoozed_at`.
	At ObservationTime
	// Until is the wake-up instant, bounded by Min/MaxSnoozeDuration.
	Until time.Time
	// Note is the optional justification.
	Note string
	// EventID is the uuidv7 for the `alert.snoozed` event.
	EventID uuid.UUID
	Payload map[string]any
}

// StartSnooze creates a snooze on an Alert and the `alert.snoozed` event to
// append (§B.8.3, T15).
//
// The CALLER must, in the SAME TRANSACTION: close any active snooze on this
// alert as `superseded` (Snooze.End), and enqueue
// `notify.evaluate(reason=snoozed)` — so the channel is TOLD it is going quiet.
// A snooze that does not announce itself is the silent suppression §B.6 forbids.
//
// ⭐ THERE IS NO PROJECTION TO WRITE ANY MORE. This row is the whole record of
// the quiet period; nothing mirrors it onto `alerts`, so no caller can forget to
// keep a mirror in step and no reader can consult one instead of this.
//
// It does not touch the Alert's state, ack state, severity or flap score, and
// there is no code path here by which it could.
func StartSnooze(a Alert, cmd SnoozeCommand) (Snooze, []Event, error) {
	if cmd.Actor.IsZero() || !cmd.Actor.Kind().IsHuman() {
		return Snooze{}, nil, errs.New(errs.KindValidation, "required",
			"a snooze requires a human actor: it is always attributed")
	}
	if cmd.At.IsZero() {
		return Snooze{}, nil, errs.New(errs.KindValidation, "required",
			"a snooze carries both occurred_at and recorded_at")
	}

	at := cmd.At.RecordedAt()
	s, err := NewSnooze(SnoozeParams{
		ID:             cmd.ID,
		OrgID:          a.OrgID(),
		AlertID:        a.ID(),
		AlertKey:       a.Key(),
		SnoozedAt:      at,
		SnoozedUntil:   cmd.Until,
		SnoozedBy:      actorUUID(cmd.Actor),
		SnoozedByLabel: cmd.Actor.Label(),
		Note:           cmd.Note,
	})
	if err != nil {
		return Snooze{}, nil, err
	}

	payload := mergePayload(cmd.Payload, map[string]any{
		"snooze_id":        s.id.String(),
		"until":            s.snoozedUntil.Format(time.RFC3339Nano),
		"note":             s.note,
		"duration_seconds": int64(s.Duration().Seconds()),
	})

	ev, err := NewEvent(EventParams{
		ID:        cmd.EventID,
		OrgID:     s.orgID,
		AlertID:   s.alertID,
		Type:      EventAlertSnoozed,
		At:        cmd.At,
		Actor:     cmd.Actor,
		Summary:   "Notifications snoozed by " + cmd.Actor.Label(),
		Payload:   payload,
		DedupeKey: "snooze:" + s.id.String() + ":started",
	})
	if err != nil {
		return Snooze{}, nil, err
	}
	return s, []Event{ev}, nil
}

// UnsnoozeCommand ends a snooze (§B.8.3, T15).
type UnsnoozeCommand struct {
	// Actor is the human who unsnoozed, or the system actor for an expiry.
	Actor Actor
	At    ObservationTime
	// Reason is manual, expired or superseded.
	Reason SnoozeEndReason
	// EventID is the uuidv7 for the `alert.unsnoozed` event.
	EventID uuid.UUID
	// Note is the free-text note recorded with a MANUAL wake-up, at most
	// MaxSnoozeNoteBytes. It lands in the `alert.unsnoozed` event payload:
	// `alert_snoozes` has a `note` column, but that one belongs to the snooze
	// that was requested, and overwriting it would rewrite the reason the quiet
	// period was asked for with the reason it was ended.
	//
	// An `expired` end has no human behind it and therefore no note.
	Note    string
	Payload map[string]any
}

// End closes a snooze and returns the `alert.unsnoozed` event to append.
//
// The CALLER must, in the SAME TRANSACTION — when the reason is not
// `superseded`, and the alert's case is still open — enqueue
// `notify.evaluate(reason=unsnoozed)`. Stamping `ended_at` on this row IS the
// wake-up: there is no second place the quiet period is recorded.
// Because deliveries are rendered at CLAIM time (C11), that wake-up notification
// reflects the alert's state NOW, not a replay of what was suppressed: an alert
// that fired and resolved entirely inside the window produces no stale card.
//
// Actor and reason must agree (§B.8.5): an `expired` end is the reaper's, and is
// `system`; manual and superseded ends have a human behind them.
func (s Snooze) End(cmd UnsnoozeCommand) (Snooze, []Event, error) {
	if cmd.Actor.IsZero() {
		return Snooze{}, nil, errs.New(errs.KindValidation, "required", "actor is required")
	}
	if cmd.At.IsZero() {
		return Snooze{}, nil, errs.New(errs.KindValidation, "required",
			"ending a snooze carries both occurred_at and recorded_at")
	}
	if cmd.Reason.IsZero() {
		return Snooze{}, nil, errs.New(errs.KindValidation, "required",
			"a snooze ends for a stated reason: expired, manual or superseded")
	}
	if !s.IsOpen() {
		return Snooze{}, nil, errs.Newf(errs.KindPrecondition, "snooze_already_ended",
			"this snooze already ended (%s)", s.endedReason)
	}
	switch {
	case cmd.Reason == SnoozeEndedExpired && cmd.Actor.Kind().IsHuman():
		return Snooze{}, nil, errs.New(errs.KindInternal, "wrong_actor",
			"an expired snooze is ended by the system, never by a human")
	case cmd.Reason != SnoozeEndedExpired && !cmd.Actor.Kind().IsHuman():
		return Snooze{}, nil, errs.Newf(errs.KindInternal, "wrong_actor",
			"a %s snooze end requires a human actor", cmd.Reason)
	}
	if len(cmd.Note) > MaxSnoozeNoteBytes {
		return Snooze{}, nil, errs.Newf(errs.KindValidation, "max_length",
			"unsnooze note must have at most %d characters", MaxSnoozeNoteBytes)
	}

	next := s
	next.endedAt = notBefore(cmd.At.RecordedAt(), s.snoozedAt)
	next.endedReason = cmd.Reason
	next.endedBy = actorUUID(cmd.Actor)
	next.endedByLabel = cmd.Actor.Label()

	base := map[string]any{
		"snooze_id": next.id.String(),
		"reason":    cmd.Reason.String(),
	}
	// The wake-up note, on the timeline. The contract calls it "Optional note
	// recorded with the wake-up"; it was bound and validated by the handler and
	// then discarded, so nothing was recorded anywhere.
	if cmd.Note != "" {
		base["note"] = cmd.Note
	}
	payload := mergePayload(cmd.Payload, base)

	ev, err := NewEvent(EventParams{
		ID:        cmd.EventID,
		OrgID:     next.orgID,
		AlertID:   next.alertID,
		Type:      EventAlertUnsnoozed,
		At:        cmd.At,
		Actor:     cmd.Actor,
		Summary:   unsnoozeSummary(cmd),
		Payload:   payload,
		DedupeKey: "snooze:" + next.id.String() + ":ended",
	})
	if err != nil {
		return Snooze{}, nil, err
	}
	return next, []Event{ev}, nil
}

func unsnoozeSummary(cmd UnsnoozeCommand) string {
	switch cmd.Reason {
	case SnoozeEndedExpired:
		return "Snooze expired: notifications resumed"
	case SnoozeEndedSuperseded:
		return "Snooze replaced by a new snooze from " + cmd.Actor.Label()
	default:
		return "Notifications resumed by " + cmd.Actor.Label()
	}
}

// actorUUID reads a human actor's id as a UUID, yielding uuid.Nil for a machine
// actor or a non-UUID identity (a Slack user id, for instance, which is carried
// on the event's actor_id, not on the snooze row's FK).
func actorUUID(a Actor) uuid.UUID {
	id, err := uuid.Parse(a.ID())
	if err != nil {
		return uuid.Nil
	}
	return id
}

// ------------------------------------------------- notification suppressors

// The `notifications.suppressed_reason` vocabulary — OTO'S OWN, not
// Alertmanager's (see the ⛔ note on Snooze).
//
// These are plain strings rather than a value object because the enum's HOME is
// the notification domain; what lives here is only the §B.8.2 ORDERING, because
// snooze's position in it is a snooze ruling.
const (
	// SuppressorChannelDisabled — the destination is switched off.
	SuppressorChannelDisabled = "channel_disabled"
	// SuppressorNoPolicy — nothing routed this fact anywhere.
	SuppressorNoPolicy = "no_policy"
	// SuppressorSnoozed — a human asked oto to be quiet (§B.8).
	SuppressorSnoozed = "snoozed"
	// SuppressorThrottled — the per-subject rate cap.
	SuppressorThrottled = "throttled"
	// SuppressorVerbosity — the channel does not want this class of fact.
	SuppressorVerbosity = "verbosity"
	// SuppressorDuplicateRender — the rendered payload is byte-identical to the
	// last one sent.
	SuppressorDuplicateRender = "duplicate_render"
)

// SuppressorPrecedence is the FIXED order of SPEC §B.8.2: when several reasons
// apply, the FIRST MATCH is the one recorded.
//
//	channel_disabled -> no_policy -> snoozed
//	                 -> throttled -> verbosity -> duplicate_render
//
// Snooze outranked the automatic dampers because it is a DELIBERATE HUMAN ACT and
// therefore the most actionable explanation: "you snoozed this" tells a user what
// to do, where a damper only told them what happened. It sits below
// channel_disabled and no_policy because those two mean the message had nowhere
// to go at all, which is a truer account of why nothing was sent.
//
// ⛔ `storm` AND `flapping` HELD THE TWO RANKS BETWEEN `snoozed` AND `throttled`,
// AND BOTH ARE DELETED. Storm damping is removed (ADR 0042) and flap damping moved
// to case formation (migration 00057); migration 00059 narrowed
// `notifications_suppmap_ck` to the six that remain and migration 00060 does the
// same to `notifications_reason_ck`, neither rewriting a row. They were briefly
// kept as retired ranks so a binary meeting an older row could still SORT it — the
// maintainer has authorised the database reset that answers the 23514, so there is
// no such row and no such binary. What is left is settled rather than argued: every
// reason here is a human's request, an absent destination, a provider's rate limit
// or "nothing changed", and not one is oto's own judgement about a signal.
func SuppressorPrecedence() []string {
	return []string{
		SuppressorChannelDisabled,
		SuppressorNoPolicy,
		SuppressorSnoozed,
		SuppressorThrottled,
		SuppressorVerbosity,
		SuppressorDuplicateRender,
	}
}

// FirstSuppressor returns the §B.8.2 winner among the reasons that apply, or ""
// when nothing suppresses the notification and it should be delivered.
//
// Suppression is always RECORDED — `status='suppressed'` with this reason, and
// no deliveries — never silently dropped. The audit trail stays complete, which
// is the whole reason snooze is safe (§B.6: silence destroys trust).
func FirstSuppressor(applies map[string]bool) string {
	for _, r := range SuppressorPrecedence() {
		if applies[r] {
			return r
		}
	}
	return ""
}

// SnoozeSuppresses reports whether an active snooze silences a notification with
// the given `reason` (§B.8.4).
//
// Snooze suppresses EVERY reason for that alert_key — including `rule_changed`,
// which is otherwise never gated, because a partial mute is a confusing mute.
// THE TWO EXCEPTIONS ARE NECESSARY: `snoozed` and `unsnoozed` are themselves
// exempt, because a snooze must be able to announce its own beginning and end or
// it becomes the silent suppression §B.6 forbids.
func SnoozeSuppresses(reason string) bool {
	switch reason {
	case NotifyReasonSnoozed, NotifyReasonUnsnoozed:
		return false
	default:
		return true
	}
}

// The two notification reasons a snooze may never suppress (§B.8.4). They are
// declared here, next to the rule that exempts them, so the exemption cannot
// drift away from the names it exempts.
const (
	// NotifyReasonSnoozed announces that oto is going quiet.
	NotifyReasonSnoozed = "snoozed"
	// NotifyReasonUnsnoozed announces that oto is speaking again.
	NotifyReasonUnsnoozed = "unsnoozed"
)

// ------------------------------------------------------- repository contracts

// SnoozeRequest is the repository input for creating a snooze (§F.5.2).
type SnoozeRequest struct {
	AlertID uuid.UUID
	// Until is ALREADY validated to lie within [now+MinSnoozeDuration,
	// now+MaxSnoozeDuration]. The repository never validates a business rule.
	Until   time.Time
	By      *uuid.UUID
	ByLabel string
	Note    *string
}

// SnoozeEnd is the repository input for closing a snooze (§F.5.2).
type SnoozeEnd struct {
	SnoozeID uuid.UUID
	// Reason is "expired", "manual" or "superseded".
	Reason  string
	By      *uuid.UUID
	ByLabel *string
	At      time.Time
}
