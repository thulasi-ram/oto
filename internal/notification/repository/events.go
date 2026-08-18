package repository

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
)

// ⛔ THE SEVEN `alert_events.type` STRING CONSTANTS THAT USED TO BE HERE ARE GONE.
//
// They were `eventNotificationCreated = "notification.created"` and six siblings —
// a second Go spelling of `alerts/domain.EventNotificationCreated` and its six, so
// the "closed" enum was closed over only one of the two ways to say each value.
// This file now names the kernel's values directly. RULE K (§5.2b,
// `.golangci.yml`, and `exemptReason` in `test/arch/arch_test.go`) grants every
// module `internal/alerts/domain`, and this module already took it in
// `notification/domain/idempotency.go`.
//
// ⚠️ THE KERNEL TYPE IS ALL THIS FILE BORROWS, AND IT DOES NOT MAKE THE SECOND
// WRITER LEGITIMATE. This repository still INSERTs into `alert_events` itself,
// with its own §C.8 key claim and without going through `domain.NewEvent`, so the
// §D.4 invariants that constructor enforces are still not enforced on this path —
// only the type is now proved. `alerts/service/seam.go` says the same thing from
// the other side. Routing this writer through the seam would need a
// `notification ──► alerts` module edge CONTEXT.md §4 does not draw; that is a
// separate decision and a separate change.

// actorNotifier is this module's `alert_events.actor_kind`. Every row it writes
// is caused by oto's own notification machinery, never by a human, even when the
// FACT being communicated was.
const actorNotifier = "notifier"

// EventRepository appends this module's facts to the shared timeline.
//
// Suppression is the reason this type exists. §B.6 is unambiguous: a suppressed
// notification is a RECORDED FACT, not a silent drop. An alerting tool that
// quietly decides not to tell you something, and leaves no trace that it decided
// anything, is a tool nobody can trust at 3am — and the trace has to live on the
// timeline, next to the transition that would have caused the message, or nobody
// will ever find it.
type EventRepository struct {
	q   db.Querier
	clk clock.Clock
}

// NewEventRepository builds the repository over a fallback querier.
func NewEventRepository(q db.Querier, clk clock.Clock) *EventRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &EventRepository{q: q, clk: clk}
}

func (r *EventRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// Event is one timeline row this module writes.
type Event struct {
	// Type is the closed §D.4.1 enum, not a string: the value reaching the INSERT
	// can only have come from `alerts/domain`, which is the one property this
	// second writer can offer without being folded into the seam.
	Type    kernel.EventType
	AlertID *uuid.UUID
	CaseID  *uuid.UUID
	GroupID *uuid.UUID
	Summary string
	Payload map[string]any
	// DedupeKey makes the append idempotent across an at-least-once job (§C.8).
	// Empty means "append unconditionally", which is right for facts that
	// genuinely can recur, such as a retry failing again.
	DedupeKey string
	At        time.Time
}

const insertEventKeySQL = `
INSERT INTO alert_event_keys (org_id, dedupe_key, event_id, created_at)
VALUES ($1,$2,$3,$4)
ON CONFLICT DO NOTHING`

const insertEventSQL = `
INSERT INTO alert_events (
  id, org_id, alert_id, case_id, group_id, type, occurred_at, recorded_at,
  actor_kind, actor_label, summary, payload, dedupe_key)
VALUES ($1,$2,$3,$4,$5,$6,$7,$7,$8,$9,$10,$11,$12)`

// Append writes one timeline row, idempotently when a dedupe key is supplied.
//
// The key is inserted into the UNPARTITIONED `alert_event_keys` first, in the
// same transaction; zero rows affected means the event already exists and the
// `alert_events` insert is skipped (§C.8). The partitioned parent cannot enforce
// this itself, because a unique index on it would have to include `recorded_at`
// and the entire point is to suppress a second write at a DIFFERENT time.
func (r *EventRepository) Append(ctx context.Context, s db.TenantScope, e Event) error {
	if e.Summary == "" {
		e.Summary = e.Type.String()
	}
	if e.At.IsZero() {
		return mapErr(errNoEventTime, "event_not_found", "append a timeline event")
	}

	id := uuid.New()
	q := r.db(ctx)

	var key *string
	if e.DedupeKey != "" {
		tag, err := q.Exec(ctx, insertEventKeySQL, s.OrgID(), e.DedupeKey, id, e.At)
		if err != nil {
			return mapErr(err, "event_not_found", "reserve a timeline event key")
		}
		if tag.RowsAffected() == 0 {
			// The event already exists. This is the idempotency mechanism working.
			return nil
		}
		key = &e.DedupeKey
	}

	payload := []byte(`{}`)
	if len(e.Payload) > 0 {
		encoded, err := json.Marshal(e.Payload)
		if err != nil {
			return mapErr(err, "event_not_found", "encode a timeline event payload")
		}
		payload = encoded
	}

	_, err := q.Exec(ctx, insertEventSQL,
		// `.String()` is the ONE place the value object becomes a `TEXT` bind
		// parameter, which is what CONTEXT.md §6 means by "TEXT + CHECK in SQL,
		// closed value-object types in Go": the string exists at the driver
		// boundary and nowhere above it.
		id, s.OrgID(), e.AlertID, e.CaseID, e.GroupID, e.Type.String(), e.At,
		actorNotifier, "oto", truncate(e.Summary, 500), payload, key,
	)
	return mapErr(err, "event_not_found", "append a timeline event")
}

// AppendNotificationCreated records that an intent was minted and fanned out.
func (r *EventRepository) AppendNotificationCreated(
	ctx context.Context, s db.TenantScope, n domain.Notification, destinations int, at time.Time,
) error {
	groupID := n.GroupID
	return r.Append(ctx, s, Event{
		Type:    kernel.EventNotificationCreated,
		AlertID: n.AlertID,
		CaseID:  n.CaseID,
		GroupID: &groupID,
		Summary: "oto notified " + strconv.Itoa(destinations) + " destination(s): " +
			string(n.Reason),
		Payload: map[string]any{
			"notification_id": n.ID,
			"reason":          string(n.Reason),
			"state_version":   n.StateVersion,
			"destinations":    destinations,
		},
		DedupeKey: "notif:" + n.ID.String() + ":created",
		At:        at,
	})
}

// AppendNotificationSuppressed records that oto deliberately said nothing, and
// why.
//
// The `also` list carries every suppressor that applied, not just the winner.
// When a signal is snoozed AND throttled AND the channel is quiet, an operator
// who unsnoozes and still hears nothing needs to already know why.
func (r *EventRepository) AppendNotificationSuppressed(
	ctx context.Context, s db.TenantScope, n domain.Notification,
	also []domain.SuppressedReason, at time.Time,
) error {
	others := make([]string, 0, len(also))
	for _, a := range also {
		others = append(others, string(a))
	}
	groupID := n.GroupID
	return r.Append(ctx, s, Event{
		Type:    kernel.EventNotificationSuppressed,
		AlertID: n.AlertID,
		CaseID:  n.CaseID,
		GroupID: &groupID,
		Summary: "oto did not notify (" + string(n.SuppressedReason) + "): " +
			string(n.Reason),
		Payload: map[string]any{
			"notification_id":   n.ID,
			"reason":            string(n.Reason),
			"suppressed_reason": string(n.SuppressedReason),
			"also_applied":      others,
			"state_version":     n.StateVersion,
		},
		DedupeKey: "notif:" + n.ID.String() + ":suppressed",
		At:        at,
	})
}

// AppendDeliveryOutcome records what happened to one materialisation.
//
// `delivery.sent` and `delivery.updated` are separate types because they answer
// different questions: "a new message appeared in the channel" and "the card
// that was already there now says something else". Collapsing them would make a
// thread of forty updates read like forty messages.
func (r *EventRepository) AppendDeliveryOutcome(
	ctx context.Context, s db.TenantScope,
	d domain.Delivery, groupID uuid.UUID, alertID *uuid.UUID, detail string, at time.Time,
) error {
	var (
		kind    kernel.EventType
		summary string
		dedupe  string
	)
	switch d.Status {
	case domain.DeliverySent:
		if d.Mode == domain.ModeUpdateRoot {
			kind, summary = kernel.EventDeliveryUpdated, "oto updated the card in place"
		} else {
			kind, summary = kernel.EventDeliverySent, "oto delivered a message"
		}
		dedupe = "del:" + d.ID.String() + ":sent"
	case domain.DeliveryDead:
		kind, summary = kernel.EventDeliveryDead, "oto gave up delivering: "+detail
		dedupe = "del:" + d.ID.String() + ":dead"
	case domain.DeliverySkipped:
		kind, summary = kernel.EventDeliverySkipped, "oto skipped a delivery: "+detail
		dedupe = "del:" + d.ID.String() + ":skipped"
	case domain.DeliveryFailed:
		// Deliberately NOT deduped: every failed attempt is its own fact, and
		// collapsing them would hide a channel that has been failing for an hour.
		kind, summary = kernel.EventDeliveryFailed, "a delivery attempt failed: "+detail
	default:
		return nil
	}

	return r.Append(ctx, s, Event{
		Type:    kind,
		AlertID: alertID,
		GroupID: &groupID,
		Summary: summary,
		Payload: map[string]any{
			"delivery_id": d.ID,
			"channel_id":  d.ChannelID,
			"mode":        string(d.Mode),
			"attempts":    d.Attempts,
			"error_class": string(d.ErrorClass),
			"ambiguous":   d.Ambiguous,
			"detail":      detail,
		},
		DedupeKey: dedupe,
		At:        at,
	})
}

const threadSubjectSQL = `
SELECT subject_id FROM channel_threads WHERE org_id = $1 AND id = $2`

// AppendThreadSkip records a gap-recovery advance (§G.7.3).
//
// It resolves the group from the thread rather than taking it as an argument
// because the ordering gate, correctly, knows nothing about alert groups.
func (r *EventRepository) AppendThreadSkip(
	ctx context.Context, s db.TenantScope,
	threadID uuid.UUID, seq int, deliveryID uuid.UUID, reason string,
) error {
	var groupID uuid.UUID
	if err := r.db(ctx).QueryRow(ctx, threadSubjectSQL, s.OrgID(), threadID).Scan(&groupID); err != nil {
		return mapErr(err, "thread_not_found", "resolve the thread's subject")
	}

	payload := map[string]any{
		"thread_id":  threadID,
		"thread_seq": seq,
		"reason":     reason,
	}
	if deliveryID != uuid.Nil {
		payload["delivery_id"] = deliveryID
	}

	return r.Append(ctx, s, Event{
		Type:      kernel.EventDeliverySkipped,
		GroupID:   &groupID,
		Summary:   "oto advanced past an unsent message in this thread (" + reason + ")",
		Payload:   payload,
		DedupeKey: "thread:" + threadID.String() + ":skip:" + strconv.Itoa(seq),
		At:        r.clk.Now().UTC(),
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var errNoEventTime = errTimeRequired{}

type errTimeRequired struct{}

func (errTimeRequired) Error() string { return "a timeline event needs a timestamp" }
