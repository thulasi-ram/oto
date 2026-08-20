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

// The TWO subjects a roll-up can be asked for.
//
// ⛔ `RollupGroup RollupSubject = "alert_group"` WAS THE THIRD AND IS DELETED
// (git-bug `7570090`, migration `00069`). It named an `alert_groups` generation,
// which is the entity the migration drops; there is no id a caller could pass and
// no row it could match, so the value was not narrowed, it was emptied. It had no
// caller outside this package — `internal/app/adapters.go` only ever asked for
// `RollupAlert` and `RollupCase` — so nothing outside had to change with it.
//
// ⚠️ THE COMMENT THAT SAID THESE "CORRESPOND EXACTLY TO THE FOUR DETAIL RESPONSES
// THAT CARRY `delivery_summary`" IS DELIBERATELY NOT RESTATED, because I could not
// verify the count after the group detail response went. What is still true, and is
// all this file depends on, is that `/notifications/{id}` summarises its own fan-out
// directly and needs no subject.
const (
	// RollupAlert is one Alert: everything anybody was told about this alert,
	// whether the intent named the alert directly or one of its Cases.
	//
	// ⚠️ THAT SECOND CLAUSE USED TO READ "or the group it was part of", and the
	// difference is not wording — see `rollupSQL` below.
	RollupAlert RollupSubject = "alert"
	// RollupCase is one firing episode, which is now also one CONVERSATION.
	RollupCase RollupSubject = "case"
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
// The `$3` guard is what lets one statement serve both subjects without two
// near-identical strings drifting apart: exactly one of the disjuncts is live for
// any call, and the other is constant-folded away by the planner because
// `$3 = 'alert'` is false for the whole scan.
//
// ⛔⛔ THE FAN-OUT RULE IS DELETED, AND IT CHANGES WHAT AN ALERT'S ROLL-UP COUNTS
// (git-bug `7570090`, migration `00069`). The alert arm used to read:
//
//	n.alert_id = $2
//	 OR n.group_id IN (SELECT o.group_id FROM alert_cases o
//	                    WHERE o.org_id = $1 AND o.alert_id = $2
//	                      AND o.group_id IS NOT NULL)
//
// That second disjunct was NOT a mis-typed subject lookup, and re-pointing it at
// `conversation_id` would have been the wrong repair. It was a RULE: *a notification
// about the GROUP counts as a notification about each of its member alerts*. It was
// one-to-many — a generation held forty alerts, and "somebody was told about the
// group" is not the same claim as "somebody was told about THIS alert" — and it was
// tolerable only because oto had no finer thing to notify about. The container is
// dropped, so the rule has no object left and is gone, not moved. Ticket comment #6
// and `00069`'s own header both settle it this way.
//
// ⚠️ THE ARGUMENT THE OLD COMMENT MADE STILL APPLIES, so it is re-pointed rather than
// deleted. It said: `notifications.alert_id` is set only when the intent names a
// FOCUS, so counting only those would report `total: 0` for alerts that really were
// announced — the exact false silence this field exists to prevent. That is still
// true. What changed is only the mechanism that answers it. `subjectOf`
// (notification/service/notify.go) gives every intent a Case, and `alert_id` is
// populated only when the caller supplied a focus: `enrichment/service/notify.go`
// sets it "only when the enrichment happened to be about one", and the integration
// seed for `fired` leaves it NULL outright. So the first disjunct alone is not
// enough, exactly as it was not enough before.
//
// ⭐⭐ SO THE ALERT ARM REACHES THROUGH ITS CASES INSTEAD, AND THAT IS CONTAINMENT
// AND NOT A FAN-OUT. The difference is the cardinality and it is the whole argument:
// `alert_cases.alert_id` is NOT NULL, so a Case belongs to EXACTLY ONE alert. "A
// notification about a case of this alert is a notification about this alert" is
// therefore TOTAL — it charges nothing to an alert the fact was not about — where the
// group version charged one message to forty alerts and was true of none of them in
// particular. Replacing a one-to-many rule with a many-to-one containment is not
// re-introducing the rule the ticket deleted; it is answering the question the rule
// was standing in for.
//
// ⛔ AND IT IS LOAD-BEARING, NOT BELT-AND-BRACES.
// `test/integration/delivery_summary_test.go` seeds the `fired` intent with
// `alert_id NULL` — which is what a `fired` notification looks like once a
// conversation is a Case — and asserts that `GET /alerts/{id}` still reports the
// five-way fan-out. With only `n.alert_id = $2` that endpoint answers an all-zero
// roll-up: not "nothing was delivered" but "this page cannot see its own deliveries",
// which is the exact false silence the file's header calls the product's central
// claim. That test is the ruling; this leg is the code that satisfies it.
//
// The subquery is keyed by `(org_id, alert_id)`, which `case_alert_idx (org_id,
// alert_id, seq DESC)` leads with, so it is an index range and not a scan.
//
// ⭐ THE CASE ARM DID GET RE-POINTED, and that is the difference between the two.
// Its second disjunct was `n.group_id IN (SELECT o.group_id FROM alert_cases o WHERE
// o.id = $2 …)` — "everything that landed on the thread this episode belonged to" —
// which is a DELIVERY-TARGET question, and delivery targets did not disappear, they
// changed spelling. The pair `(conversation_kind, conversation_id)` is that spelling
// (migration 00064), and since a conversation now holds exactly ONE Case the answer
// is exact where the group version was approximate. `n.case_id = $2` is kept beside
// it because it is the SUBJECT half and the two are different questions that happen
// to agree today; a row satisfying both is still counted once, because this is a
// disjunction in a WHERE and not a join.
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
         OR n.case_id IN (SELECT o.id FROM alert_cases o
                           WHERE o.org_id = $1 AND o.alert_id = $2)))
     OR ($3 = 'case' AND (
            n.case_id = $2
         OR (n.conversation_kind = 'case' AND n.conversation_id = $2)))
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
	case RollupAlert, RollupCase:
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
