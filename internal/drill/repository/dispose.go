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
// `alert_cases`, `alert_groups`, `notifications`, `notification_deliveries`
// and `channel_threads` are never reaped — is a promise about THE RECORD OF AN
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
	// `alerts` and `alert_groups` across four statements, on the maintenance queue,
	// for rows nobody will ever read again.

	// 1. The timeline. `alert_events` is partitioned and carries no FK to its
	//    subjects, so it is deleted explicitly and BEFORE them, by the three ids
	//    it could carry. The `recorded_at` predicate is there to prune partitions.
	if d.AlertID != uuid.Nil || d.CaseID != uuid.Nil || d.GroupID != uuid.Nil {
		if _, err := q.Exec(ctx, `
DELETE FROM alert_events
 WHERE org_id = $1 AND recorded_at >= $2
   AND (alert_id = $3 OR case_id = $4 OR group_id = $5)`,
			s.OrgID(), from, nilID(d.AlertID), nilID(d.CaseID), nilID(d.GroupID)); err != nil {
			return mapErr(err, "delete the drill's timeline")
		}
	}

	// ⛔⛔ THE GROUP IS NOT THIS DRILL'S TO DELETE ON ITS OWN. Since ADR 0038 the
	// §C.4 key is `(org, cluster, alertname, namespace-or-∅)` and neither the
	// drill's receiver nor its `oto_drill` nonce is an axis, so two drills fired
	// inside `group_close_delay` (20 minutes by default) land in ONE synthetic
	// generation and one thread. Each still has its own Alert and its own case —
	// those go in step 4 — but the generation and the thread are SHARED, and an
	// unconditional delete here would take the second drill's group out from under
	// a drill that is still running or still being read.
	//
	// ⭐ THE FAILURE WAS SELF-TRIGGERING, which is why it is a guard and not a
	// comment. A drill exists to give an operator confidence in the delivery path;
	// a second drill whose result screen is missing its group and its thread reads
	// as "the delivery path is broken", and the natural response is to run another
	// drill immediately — well inside the same twenty minutes. The retry IS the
	// collision, so the modal way to meet this bug was to have met it once already.
	//
	// ⭐ THE GUARD IS A PREDICATE, NOT A NEW GROUPING AXIS. Giving each drill its
	// own value on an axis would be a grouping rule invented for exactly one
	// caller, which is what ADR 0038 forbids. The collision is not really about the
	// key: it is about a delete reaching a row another live drill still points at.
	// So the delete asks that question directly.
	//
	// ⭐ IT STAYS IDEMPOTENT, which is what lets this function keep running without
	// a transaction (see above). A skipped delete is not a lost one: the group and
	// its thread are collected by whichever drill sharing them disposes LAST, whose
	// own guard then finds no other undisposed holder. `disposed_at` is still
	// stamped for this drill, so `retention.prune` does not spin on it; and a
	// process that dies before the stamp is re-run by the next sweep with every
	// statement here a no-op.
	//
	// ⚠️ `alert_events` (step 1) is deliberately NOT guarded. Its rows are the
	// narration, not the subject: nothing on the drill path ever reads
	// `alert_events` (`test/arch/sqltables_test.go` declares it delete-only for
	// this module), so losing a shared generation's timeline rows early degrades
	// no result screen. Guarding it would mean splitting one partition-pruned
	// delete into three for no reader's benefit.
	const groupStillHeld = `
   AND NOT EXISTS (
         SELECT 1 FROM delivery_drills dd
          WHERE dd.org_id = $1 AND dd.group_id = $2
            AND dd.disposed_at IS NULL AND dd.id <> $3)`

	// 2. The thread binding. `channel_threads.subject_id` carries no FK to
	//    `alert_groups` — the column is designed to grow other subject kinds — so
	//    nothing cascades and it is deleted by hand. It goes BEFORE the group
	//    because a delivery's `thread_id` is ON DELETE SET NULL, and deleting the
	//    thread afterwards would be a pointless write to rows already gone.
	//
	//    It carries the same guard as the group: the thread belongs to the
	//    GENERATION, not to the drill, and a surviving group whose thread has been
	//    deleted is the same degraded result screen by another route.
	if d.GroupID != uuid.Nil {
		if _, err := q.Exec(ctx, `
DELETE FROM channel_threads
 WHERE org_id = $1 AND subject_kind = 'alert_group' AND subject_id = $2`+groupStillHeld,
			s.OrgID(), d.GroupID, d.ID); err != nil {
			return mapErr(err, "delete the drill's thread")
		}
		// 3. The group. CASCADES to `notifications` and through them to
		//    `notification_deliveries`. It does NOT cascade to the episodes: since
		//    00051 membership is `alert_cases.group_id`, whose FK is
		//    ON DELETE SET NULL, so the drill's episodes are briefly groupless
		//    between this delete and step 4 — which deletes the alert and cascades
		//    them away for real.
		if _, err := q.Exec(ctx,
			`DELETE FROM alert_groups WHERE org_id = $1 AND id = $2 AND synthetic`+groupStillHeld,
			s.OrgID(), d.GroupID, d.ID); err != nil {
			return mapErr(err, "delete the drill's group")
		}
	}

	// 4. The alert. CASCADES to `alert_cases` and `alert_snoozes`.
	//
	// ⭐ `AND synthetic` on both deletes is a BELT-AND-BRACES PREDICATE, not a
	// filter. The ids already came from this drill's own manifest; the extra
	// clause means that even a corrupted manifest cannot delete a real customer
	// alert or a real group. It is the cheapest possible insurance against the one
	// mistake in this file that would be unforgivable.
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
