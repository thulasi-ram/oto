package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/drill/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// eventWindow is how far before a drill started its timeline events can be.
// `alert_events` is partitioned by `recorded_at`, so the predicate exists to
// prune partitions rather than to filter rows.
const eventWindow = 5 * time.Minute

// Dispose deletes the synthetic signal rows one drill created, then stamps the
// receipt. `at` is oto's clock, injected rather than read, so a test can pin it.
//
// ⛔⛔ THIS IS THE ONLY FUNCTION IN OTO THAT DELETES A SIGNAL ROW, and the
// argument for it is narrow and must stay narrow. ADR 0024's promise — `alerts`,
// `alert_cases`, `notifications`, `notification_deliveries` and `channel_threads`
// are never reaped — is a promise about THE RECORD OF AN
// UPSTREAM EVENT. A drill records none. Nothing fired, no cluster was involved,
// and oto manufactured every byte to answer a question an operator asked by
// pressing a button. Deleting it destroys no history, because none was made.
//
// ⛔ IT IS SCOPED BY ID, NEVER BY PREDICATE. Every statement names an id the
// drill itself recorded. There is deliberately no `DELETE … WHERE synthetic`
// anywhere in this codebase: a query that deletes by a boolean is one bad
// migration away from deleting by the wrong boolean, and the blast radius of
// getting this wrong is a customer's entire alert history.
//
// ⭐ BOTH RULES ABOVE ARE NOW ASSERTED, not merely argued. `test/arch/sqltables_test.go`
// declares every table this module names in SQL with its owning module and its
// permitted access, and it fails the build if a DELETE here loses its id scope, if
// `AND synthetic` disappears from the two riskiest statements, or if disposal reaches
// a table nobody declared. This comment used to be the only thing standing between a
// tidy-looking edit and a customer's alert history.
//
// What is NOT deleted, and why:
//
//   - `ingest_batches` / `ingest_rejections` — the raw tables already have a
//     reaper (ADR 0024, `raw_retention_days`), and the batch is the audit trail
//     of what oto accepted at its own front door. It ages out on schedule.
//   - `alert_event_keys` — the §C.8 idempotency siblings, pruned at 30 days by
//     their own sweeper. An orphaned key suppresses nothing that will ever be
//     written again.
//   - `delivery_drills` itself — the RECEIPT survives, with its frozen outcome,
//     exactly as ADR 0024 designed `retention_exports`. An operator can still
//     answer "did the delivery path work last Tuesday" a year later.
//
// The Slack message is not deleted either, and cannot be: oto never reads Slack
// back, and a `chat.delete` would be oto writing into somebody's channel history
// on a timer. The card says it is a drill; that is the mitigation.
func (r *DrillRepository) Dispose(ctx context.Context, s db.TenantScope, d domain.Drill, at time.Time) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	from := d.StartedAt.Add(-eventWindow)
	q := r.db(ctx)

	// ⭐ NO EXPLICIT TRANSACTION, AND THAT IS THE STRONGER CHOICE. Every statement
	// below is idempotent — each names an id and deletes at most the rows already
	// gone — and `disposed_at` is stamped LAST. A process that dies halfway leaves
	// a drill that is still `disposed_at IS NULL`, so the next `retention.prune`
	// sweep picks it up and re-runs the deletes as no-ops. Wrapping this in one
	// transaction would buy atomicity nobody needs and would hold locks on
	// `alerts` and `notifications` across four statements, on the maintenance queue,
	// for rows nobody will ever read again.

	// 1. The timeline. `alert_events` is partitioned and carries no FK to its
	//    subjects, so it is deleted explicitly and BEFORE them, by the two ids
	//    it could carry. The `recorded_at` predicate is there to prune partitions.
	//
	// ⛔ IT WAS THREE IDS AND THE THIRD WAS `group_id` (git-bug `7570090`). The
	// column survives on `alert_events` — that table is retained thirteen months and
	// its history is readable-but-unwritable by the 00051/00054 bargain — but a drill
	// has no group id to bind to it, and the only rows that ever carried one without
	// also carrying an alert or a case were `group.opened` and `group.closed`, which
	// are retired at the writer. So dropping the clause loses no row that a drill
	// could still create.
	if d.AlertID != uuid.Nil || d.CaseID != uuid.Nil {
		if _, err := q.Exec(ctx, `
DELETE FROM alert_events
 WHERE org_id = $1 AND recorded_at >= $2
   AND (alert_id = $3 OR case_id = $4)`,
			s.OrgID(), from, nilID(d.AlertID), nilID(d.CaseID)); err != nil {
			return mapErr(err, "delete the drill's timeline")
		}
	}

	// ⛔⛔ `groupStillHeld` WAS HERE — A `NOT EXISTS` GUARD ON EVERY DELETE — AND IT IS
	// DELETED (git-bug `7570090`). It read: another undisposed drill still points at
	// this `delivery_drills.group_id`, so leave the row alone. Its whole premise was
	// SHARING, and the premise is what went: since ADR 0038 the §C.4 key was
	// `(org, cluster, alertname, namespace-or-∅)` and neither the drill's receiver nor
	// its `oto_drill` nonce was an axis, so two drills fired inside `group_close_delay`
	// landed in ONE generation and one thread, and disposing the first took the
	// second's evidence out from under it.
	//
	// ⭐ THE COLLISION IS NOW IMPOSSIBLE BY CONSTRUCTION, WHICH IS THE ONLY REASON A
	// GUARD MAY BE DELETED RATHER THAN PORTED. A conversation holds exactly one Case,
	// a Case belongs to exactly one Alert, and an Alert is label identity — so the
	// `oto_drill` nonce that always separated the drills' ALERTS now separates their
	// conversations and their threads too. There is no shared row left for one drill
	// to delete out from under another.
	//
	// ⚠️ THE REASONING BEHIND IT STILL APPLIES TO THE NEXT PERSON, so it is written
	// down rather than dropped. The failure was SELF-TRIGGERING: a drill exists to
	// give an operator confidence in the delivery path, a drill whose result screen is
	// missing its thread reads as "the delivery path is broken", and the natural
	// response is to run another one immediately — inside the same window. The retry
	// IS the collision. Anything that ever makes two drills share a conversation
	// re-opens it, and the fix is a predicate on the delete, never a new axis
	// invented for one caller (ADR 0038).

	// 2. The thread binding. `channel_threads.subject_id` carries no FK — one column
	//    cannot reference the two tables a conversation may be keyed in, a Case or a
	//    policy — so nothing cascades and it is deleted by hand. It goes BEFORE the
	//    notifications because a delivery's
	//    `thread_id` is ON DELETE SET NULL, and deleting the thread afterwards would
	//    be a pointless write to rows already gone.
	//
	// 3. The notifications, and through them `notification_deliveries` by CASCADE.
	//
	// ⛔⛔ STEP 3 IS NEW WORK THIS DRILL DID NOT USED TO DO, AND NOT DOING IT WOULD BE
	// A LEAK (git-bug `7570090`). The intents used to be CASCADEd away by the group:
	// `notifications.group_id REFERENCES alert_groups(id) ON DELETE CASCADE` (00011),
	// so deleting the generation took every intent and every delivery with it. That
	// column is dropped and `case_id`, `subject_id` and `conversation_id` all
	// deliberately carry NO FK, so after 7570090 nothing deletes a drill's intents at
	// all — and ADR 0024 gives `notifications` no reaper, by design. A drill that left
	// them behind would be a feature whose cleanup silently stopped working.
	//
	// ⭐ IT IS DELETED BY THE CONVERSATION, NOT BY THE MANIFEST'S `notification_id`.
	// The manifest holds the OLDEST intent — the one the drill caused — and the group
	// CASCADE took ALL of them, repeats and resolutions included. Matching
	// `(conversation_kind, conversation_id)` keeps that: the id is still one the drill
	// itself recorded, so the rule below is intact.
	if d.CaseID != uuid.Nil {
		if _, err := q.Exec(ctx, `
DELETE FROM channel_threads
 WHERE org_id = $1 AND subject_kind = 'case' AND subject_id = $2`,
			s.OrgID(), d.CaseID); err != nil {
			return mapErr(err, "delete the drill's thread")
		}
		if _, err := q.Exec(ctx, `
DELETE FROM notifications
 WHERE org_id = $1 AND conversation_kind = 'case' AND conversation_id = $2`,
			s.OrgID(), d.CaseID); err != nil {
			return mapErr(err, "delete the drill's notifications")
		}
	}

	// 4. The alert. CASCADES to `alert_cases` and `alert_snoozes`.
	//
	// ⭐ `AND synthetic` is a BELT-AND-BRACES PREDICATE, not a filter. The id already
	// came from this drill's own manifest; the extra clause means that even a
	// corrupted manifest cannot delete a real customer alert. It is the cheapest
	// possible insurance against the one mistake in this file that would be
	// unforgivable.
	//
	// ⚠️ IT USED TO GUARD TWO DELETES AND NOW GUARDS ONE, because the second was
	// `alert_groups` and `alert_groups.synthetic` is dropped with the table (git-bug
	// `7570090`). Neither `channel_threads` nor `notifications` has a `synthetic`
	// column to say the same thing with — their protection is that both are addressed
	// by a Case id the drill recorded, and that a Case reachable from this manifest
	// belongs to an alert that `alerts.synthetic` will refuse to delete if it is real.
	// ⛔ `test/arch/sqltables_test.go`'s `syntheticGuarded` set still lists
	// `alert_groups`; it is another agent's file and has to lose that member.
	if d.AlertID != uuid.Nil {
		if _, err := q.Exec(ctx,
			`DELETE FROM alerts WHERE org_id = $1 AND id = $2 AND synthetic`,
			s.OrgID(), d.AlertID); err != nil {
			return mapErr(err, "delete the drill's alert")
		}
	}

	if _, err := q.Exec(ctx, `
UPDATE delivery_drills SET disposed_at = $3, updated_at = now()
 WHERE org_id = $1 AND id = $2 AND disposed_at IS NULL`,
		s.OrgID(), d.ID, at.UTC()); err != nil {
		return mapErr(err, "stamp the drill as disposed")
	}
	return nil
}
