package repository

// THE WRITE SIDE of alert-hygiene accounting — `stats.rollup` (SPEC §G.3).
//
// ⭐ ADR 0014 IS WHY THIS FILE EXISTS. oto is Postgres-only: there is no
// ClickHouse, no Druid, no second store. That is only viable because the hygiene
// report is served from a small, pre-aggregated table instead of from a scan of
// an append-only event stream that grows to hundreds of millions of rows. This is
// the job that fills it. Without it, `GET /api/v1/stats/alert-quality` answers
// truthfully with zeroes forever.
//
// ⛔ TEAM- AND ALERT-SCOPED ONLY (R8). The target table carries no user column and
// nothing here selects one. Per-person response metrics are not omitted, they are
// unrepresentable — and a feature that does not exist cannot be misused.

import (
	"context"
	"time"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// rollupDaySQL recomputes one UTC day for one org, in ONE statement.
//
// ⭐⭐ IT IS AN UPSERT, NOT AN APPEND, AND THAT IS THE WHOLE CONTRACT. Every
// counter is REPLACED by the recomputed value rather than added to it, so running
// the job five times for the same day leaves exactly the same numbers as running
// it once. An at-least-once queue guarantees it WILL run more than once; a `+=`
// here would silently triple a customer's alert volume after one worker restart.
//
// The four sources, and the joins that attribute each to a `(cluster, alertname)`:
//
//   - occ    — `alert_occurrences` that STARTED on this day. Everything about an
//     episode is attributed to the day it opened, which is what makes
//     `acked_occurrences <= occurrences` true by construction and therefore what
//     satisfies `alert_quality_acked_ck` without an application check.
//   - notif  — `notifications` minted on this day, attributed through the group's
//     membership. A notification is about a GROUP, so it is counted once per
//     distinct alertname in that group (count DISTINCT, never a row count).
//   - flaps  — state-changing `alert_events` recorded on this day. `recorded_at`
//     is the partition key, so the day predicate prunes partitions rather than
//     scanning thirteen months.
//
// `resolved` and `expired` are counted into SEPARATE columns and are never
// summed. Conflating them is precisely the lie oto exists to prevent.
//
// ⛔⛔ ALL THREE CTEs EXCLUDE `a.synthetic`, AND ALL THREE MUST. A synthetic
// Alert is one oto manufactured for a DELIVERY DRILL — an operator pressing a
// button to prove the notification path works. Nothing fired in any cluster.
// Counting one here would inflate an org's occurrence count, its notification
// count and its flap count with oto's own plumbing, in the exact table the
// hygiene report is sold from, permanently — this is an upsert of a day, so the
// wrong number would survive every recompute. Every CTE already joins `alerts`,
// so the predicate costs one boolean test per row and there is no excuse.
//
// A fourth CTE added later joins `alerts` and excludes them too, or the report
// is wrong in a way nobody will notice until a customer disputes an invoice.
//
// ⛔ THE SIX EVENT TYPES IN `flaps` ARE A COPY OF THE KERNEL'S ENUM, IN SQL, AND
// THEY CANNOT BE ANYTHING ELSE. `alerts/domain.EventType` closes the value
// everywhere it is a Go value; it cannot reach a predicate that is planned with
// the statement rather than bound with the parameters, and this one has to be a
// predicate — it is what lets the day's partition pruning do the work. The failure
// that copy invites is the quiet kind and it is permanent here: a value renamed in
// SPEC §D.4.1 and not renamed in this list makes `flap_transitions` count fewer
// transitions than happened, the upsert overwrites the day with that number, and
// the hygiene report reads calm. `test/arch.TestEventTypeSQLNamesLiveValues` reads
// inside this literal — this package is registered in `eventTypeSQLSites` — and
// fails when a value in it is no longer a member.
const rollupDaySQL = `
WITH bounds AS (
  SELECT $2::date                                             AS day,
         $2::date::timestamptz                                AS day_start,
         ($2::date + 1)::timestamptz                           AS day_end,
         LEAST($3::timestamptz, ($2::date + 1)::timestamptz)   AS as_of
), occ AS (
  SELECT a.cluster_key,
         a.alertname,
         count(*)                                              AS occurrences,
         count(*) FILTER (WHERE o.ack_state = 'acked')         AS acked_occurrences,
         count(*) FILTER (WHERE o.state = 'resolved')          AS auto_resolved,
         count(*) FILTER (WHERE o.state = 'expired')           AS expired,
         COALESCE(SUM(GREATEST(0, EXTRACT(EPOCH FROM
             (COALESCE(o.ended_at, b.as_of) - o.started_at))))::bigint, 0)
                                                               AS total_firing_seconds
    FROM alert_occurrences o
    JOIN alerts a  ON a.id = o.alert_id AND a.org_id = o.org_id
   CROSS JOIN bounds b
   WHERE o.org_id = $1
     AND NOT a.synthetic
     AND o.started_at >= b.day_start
     AND o.started_at <  b.day_end
   GROUP BY a.cluster_key, a.alertname
), notif AS (
  SELECT a.cluster_key,
         a.alertname,
         count(DISTINCT n.id) AS notifications,
         count(DISTINCT d.id) AS deliveries
    FROM notifications n
   CROSS JOIN bounds b
    JOIN alert_group_members m ON m.group_id = n.group_id AND m.org_id = n.org_id
    JOIN alerts a              ON a.id = m.alert_id       AND a.org_id = n.org_id
    LEFT JOIN notification_deliveries d
                               ON d.notification_id = n.id AND d.org_id = n.org_id
   WHERE n.org_id = $1
     AND NOT a.synthetic
     AND n.created_at >= b.day_start
     AND n.created_at <  b.day_end
   GROUP BY a.cluster_key, a.alertname
), flaps AS (
  SELECT a.cluster_key,
         a.alertname,
         count(*) AS flap_transitions
    FROM alert_events e
   CROSS JOIN bounds b
    JOIN alerts a ON a.id = e.alert_id AND a.org_id = e.org_id
   WHERE e.org_id = $1
     AND NOT a.synthetic
     AND e.recorded_at >= b.day_start
     AND e.recorded_at <  b.day_end
     AND e.type IN ('occurrence.opened','occurrence.reopened','occurrence.suppressed',
                    'occurrence.unsuppressed','occurrence.resolved','occurrence.expired')
   GROUP BY a.cluster_key, a.alertname
), keys AS (
  SELECT cluster_key, alertname FROM occ
  UNION
  SELECT cluster_key, alertname FROM notif
  UNION
  SELECT cluster_key, alertname FROM flaps
)
INSERT INTO alert_quality_daily (
    org_id, day, cluster_key, alertname, occurrences, notifications, deliveries,
    acked_occurrences, auto_resolved, expired, total_firing_seconds, flap_transitions)
SELECT $1, (SELECT day FROM bounds), k.cluster_key, k.alertname,
       COALESCE(o.occurrences, 0),
       COALESCE(n.notifications, 0),
       COALESCE(n.deliveries, 0),
       COALESCE(o.acked_occurrences, 0),
       COALESCE(o.auto_resolved, 0),
       COALESCE(o.expired, 0),
       COALESCE(o.total_firing_seconds, 0),
       COALESCE(f.flap_transitions, 0)
  FROM keys k
  LEFT JOIN occ   o USING (cluster_key, alertname)
  LEFT JOIN notif n USING (cluster_key, alertname)
  LEFT JOIN flaps f USING (cluster_key, alertname)
ON CONFLICT (org_id, day, cluster_key, alertname) DO UPDATE SET
    occurrences          = EXCLUDED.occurrences,
    notifications        = EXCLUDED.notifications,
    deliveries           = EXCLUDED.deliveries,
    acked_occurrences    = EXCLUDED.acked_occurrences,
    auto_resolved        = EXCLUDED.auto_resolved,
    expired              = EXCLUDED.expired,
    total_firing_seconds = EXCLUDED.total_firing_seconds,
    flap_transitions     = EXCLUDED.flap_transitions`

// RollupDay recomputes one org's `alert_quality_daily` rows for one UTC day and
// returns how many rows it wrote.
//
// `asOf` is oto's clock, injected rather than `now()` in SQL so the rollup of an
// open episode is reproducible in a test. It bounds the firing duration of an
// episode that has not ended, and is itself clamped to the end of the day being
// rolled up: a day in the past therefore produces the SAME numbers however long
// afterwards it is recomputed.
func (r *StatsRepository) RollupDay(
	ctx context.Context, s db.TenantScope, day time.Time, asOf time.Time,
) (int, error) {
	if !s.Valid() {
		return 0, errs.Forbidden("forbidden", "a tenant scope is required")
	}
	if day.IsZero() {
		return 0, errs.Internal("stats_rollup_day_required",
			errs.New(errs.KindInternal, "missing_field", "a rollup needs the day it is rolling up"))
	}
	if asOf.IsZero() {
		return 0, errs.Internal("stats_rollup_clock_required",
			errs.New(errs.KindInternal, "missing_field", "a rollup needs oto's clock"))
	}

	tag, err := r.db(ctx).Exec(ctx, rollupDaySQL, s.OrgID(), day.UTC(), asOf.UTC())
	if err != nil {
		return 0, mapErr(err, "stats_rollup_failed",
			"the alert-hygiene rollup could not be computed")
	}
	return int(tag.RowsAffected()), nil
}
