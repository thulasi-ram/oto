package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
)

// The delivery roll-up: "was anybody told about this, and did it land?"
//
// ⭐⭐ WHY THIS FILE EXISTS AT ALL. `delivery_summary` was declared on four
// response schemas and emitted by none of them. It was optional in the contract,
// so every validator passed and the gap was invisible — the field simply never
// appeared, and nothing anywhere said so.
//
// It is not a nice-to-have. oto's whole product claim is that its SILENCE is
// trustworthy, and silence is only trustworthy if it can be told apart from
// failure. A user who sees no Slack message needs to know whether nothing fired
// or whether four deliveries died on an expired token, and those two states are
// otherwise indistinguishable from outside the database.

// RollupSubject names what a delivery roll-up is about.
type RollupSubject string

// The three subjects a roll-up can be asked for. They correspond exactly to the
// four detail responses that carry `delivery_summary` — `/notifications/{id}`
// summarises its own fan-out directly and needs no subject.
const (
	// RollupAlert is one Alert: everything anybody was told about this alert,
	// whether the intent named the alert or the group it was part of.
	RollupAlert RollupSubject = "alert"
	// RollupCase is one firing episode.
	RollupCase RollupSubject = "case"
	// RollupGroup is one AlertGroup generation — the subject oto actually
	// notifies about.
	RollupGroup RollupSubject = "alert_group"
)

// DeliveryRollup is the fan-out health of one subject.
//
// ⛔ EVERY COUNT IS READ, NONE IS DERIVED HERE. `pending` is counted from the
// rows rather than reconstructed as `total - sent - failed - dead`, and `skipped`
// is counted separately as well as inside `sent`. The contract documents the
// derivation as a fallback for producers that cannot report the states; this
// producer can, so it does.
type DeliveryRollup struct {
	Total   int
	Sent    int
	Failed  int
	Dead    int
	Skipped int
	Pending int
	// LastErrorClass is the error class of the most recently updated delivery
	// that carries one, or "". It is what turns "one died" into "one died because
	// the token expired".
	LastErrorClass string
	// LastSentAt is when anything for this subject last actually reached a
	// destination. nil means nothing ever did.
	LastSentAt *time.Time
}

// rollupSQL counts one subject's whole fan-out in a single round trip.
//
// The `$3` guard is what lets one statement serve three subjects without three
// near-identical strings drifting apart: exactly one of the three disjuncts is
// live for any call, and the other two are constant-folded away by the planner
// because `$3 = 'alert'` is false for the whole scan.
//
// ⭐ AN ALERT'S ROLL-UP INCLUDES ITS GROUPS' NOTIFICATIONS. `notifications.alert_id`
// is set only for the alert-SCOPED reasons — acked, unacked, refired,
// rule_changed — because oto notifies about GROUP GENERATIONS (§C.7). Counting
// only those would report `total: 0` for the overwhelming majority of alerts,
// including every one that fired and was announced, which is the exact false
// silence this field exists to prevent. Membership is history, so the roll-up
// covers every generation this alert has ever been part of.
//
// The membership subqueries read `alert_cases.group_id`, which since
// migration 00051 IS the membership — one row per episode instead of a join-table
// row saying the same thing. `group_id IS NOT NULL` is the only new clause: an
// episode recorded groupless (the §C.4 key could not be computed) has no
// generation to contribute, where the join table simply had no row for it.
const rollupSQL = `
SELECT
  count(d.id),
  count(d.id) FILTER (WHERE d.status IN ('sent','skipped')),
  count(d.id) FILTER (WHERE d.status = 'failed'),
  count(d.id) FILTER (WHERE d.status = 'dead'),
  count(d.id) FILTER (WHERE d.status = 'skipped'),
  count(d.id) FILTER (WHERE d.status IN ('pending','sending')),
  max(d.sent_at),
  (array_agg(d.error_class ORDER BY d.updated_at DESC)
     FILTER (WHERE d.error_class IS NOT NULL))[1]
  FROM notifications n
  JOIN notification_deliveries d ON d.notification_id = n.id
 WHERE n.org_id = $1
   AND (
        ($3 = 'alert' AND (
            n.alert_id = $2
         OR n.group_id IN (SELECT o.group_id FROM alert_cases o
                            WHERE o.org_id = $1 AND o.alert_id = $2
                              AND o.group_id IS NOT NULL)))
     OR ($3 = 'case' AND (
            n.case_id = $2
         OR n.group_id IN (SELECT o.group_id FROM alert_cases o
                            WHERE o.id = $2 AND o.group_id IS NOT NULL)))
     OR ($3 = 'alert_group' AND n.group_id = $2)
   )`

// DeliveryRollupFor counts one subject's whole fan-out.
//
// ⛔ A SUBJECT WITH NO DELIVERIES RETURNS ZEROES AND NOT AN ERROR. "Nobody has
// been told anything about this" is the single most important answer this query
// gives — it is what a suppressed notification looks like, and it is what an
// alert nobody routed looks like — and turning it into a 404 or an omitted field
// would restore exactly the ambiguity the roll-up exists to remove.
func (r *NotificationRepository) DeliveryRollupFor(
	ctx context.Context, s db.TenantScope, subject RollupSubject, id uuid.UUID,
) (DeliveryRollup, error) {
	switch subject {
	case RollupAlert, RollupCase, RollupGroup:
	default:
		return DeliveryRollup{}, errors.New("unknown delivery roll-up subject " + string(subject))
	}

	var (
		out        DeliveryRollup
		lastSentAt *time.Time
		errorClass *string
	)
	err := r.db(ctx).QueryRow(ctx, rollupSQL, s.OrgID(), id, string(subject)).Scan(
		&out.Total, &out.Sent, &out.Failed, &out.Dead, &out.Skipped, &out.Pending,
		&lastSentAt, &errorClass,
	)
	if err != nil {
		return DeliveryRollup{}, mapErr(err, "notification_not_found",
			"summarise the deliveries for one "+string(subject))
	}
	out.LastSentAt = lastSentAt
	if errorClass != nil {
		out.LastErrorClass = *errorClass
	}
	return out, nil
}
