package repository

// THE SQL behind the Stats tag.
//
// ⭐ ADR 0014 is binding: the hygiene report is served from the
// `alert_quality_daily` ROLLUP and never from a scan of `alert_events`. The
// rollup's primary key `(org_id, day, cluster_key, alertname)` is already the
// access path for every read here, which is what keeps Postgres-only viable.
//
// The overview counts read the CURRENT-STATE PROJECTIONS on `alerts`,
// `alert_groups`, `notification_deliveries`, `source_health` and `channels` —
// each an indexed column on a bounded table — and never the append-only event
// stream.

import (
	"context"
	"time"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/stats/domain"
)

// QualityFilter is the compiled form of the `getAlertQualityStats` query.
type QualityFilter struct {
	Since      time.Time
	Until      time.Time
	Clusters   []string
	AlertNames []string
	Sort       domain.Sort
	// AfterValue and AfterKey are the keyset position: the sort value of the last
	// row of the previous page, and its `cluster_key\x1falertname` tiebreak.
	AfterValue *float64
	AfterKey   string
}

// StatsRepository is the SQL over the rollup and the state projections.
type StatsRepository struct{ q db.Querier }

// NewStatsRepository builds the repository over a fallback querier.
func NewStatsRepository(q db.Querier) *StatsRepository { return &StatsRepository{q: q} }

func (r *StatsRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// alertQualitySQL aggregates the daily rollup over a date range.
//
// The ordering expression is chosen by `$5` rather than interpolated, so no part
// of a caller's query string ever reaches the SQL text. The keyset predicate
// compares `(sort_value, key)` as a tuple, which is a total order because the key
// is the rollup's own `(cluster_key, alertname)` and is therefore unique per row.
const alertQualitySQL = `
WITH rollup AS (
  SELECT cluster_key,
         alertname,
         SUM(occurrences)::bigint          AS occurrences,
         SUM(notifications)::bigint        AS notifications,
         SUM(deliveries)::bigint           AS deliveries,
         SUM(acked_occurrences)::bigint    AS acked_occurrences,
         SUM(auto_resolved)::bigint        AS auto_resolved,
         SUM(expired)::bigint              AS expired,
         SUM(total_firing_seconds)::bigint AS total_firing_seconds,
         SUM(flap_transitions)::bigint     AS flap_transitions
    FROM alert_quality_daily
   WHERE org_id = $1
     AND day >= $2::date
     AND day <= $3::date
     AND ($4::text[] IS NULL OR cluster_key = ANY($4))
     AND ($5::text[] IS NULL OR alertname = ANY($5))
   GROUP BY cluster_key, alertname
), scored AS (
  SELECT rollup.*,
         cluster_key || E'\x1f' || alertname AS keyset_key,
         CASE $6::text
           WHEN '-notifications'        THEN notifications::double precision
           WHEN 'ack_rate'              THEN CASE WHEN occurrences > 0
                                                  THEN acked_occurrences::double precision / occurrences
                                                  ELSE 0 END
           WHEN '-flap_transitions'     THEN flap_transitions::double precision
           WHEN '-total_firing_seconds' THEN total_firing_seconds::double precision
           ELSE occurrences::double precision
         END AS sort_value
    FROM rollup
)
SELECT cluster_key, alertname, occurrences, notifications, deliveries,
       acked_occurrences, auto_resolved, expired, total_firing_seconds,
       flap_transitions, sort_value, keyset_key
  FROM scored
 WHERE $7::double precision IS NULL
    OR (CASE WHEN $6::text = 'ack_rate'
             THEN (sort_value, keyset_key) > ($7, $8)
             ELSE (sort_value, keyset_key) < ($7, $8)
        END)
 ORDER BY CASE WHEN $6::text = 'ack_rate' THEN sort_value END ASC,
          CASE WHEN $6::text <> 'ack_rate' THEN sort_value END DESC,
          keyset_key ASC
 LIMIT $9`

// QualityRow is one hygiene row plus its keyset position.
type QualityRow struct {
	Quality   domain.AlertQuality
	SortValue float64
	KeysetKey string
}

// AlertQuality returns one page of the hygiene report, newest problem first.
func (r *StatsRepository) AlertQuality(
	ctx context.Context, s db.TenantScope, f QualityFilter, limit int,
) ([]QualityRow, bool, error) {
	if !s.Valid() {
		return nil, false, errs.Forbidden("forbidden", "a tenant scope is required")
	}
	if limit <= 0 {
		limit = 50
	}

	var clusters, names any
	if len(f.Clusters) > 0 {
		clusters = f.Clusters
	}
	if len(f.AlertNames) > 0 {
		names = f.AlertNames
	}
	var afterValue any
	if f.AfterValue != nil {
		afterValue = *f.AfterValue
	}

	rows, err := r.db(ctx).Query(ctx, alertQualitySQL,
		s.OrgID(), f.Since, f.Until, clusters, names, f.Sort.String(),
		afterValue, f.AfterKey, limit+1)
	if err != nil {
		return nil, false, errs.Wrap(err, errs.KindInternal, "stats_quality_failed",
			"could not read the alert-hygiene rollup")
	}
	defer rows.Close()

	out := make([]QualityRow, 0, limit)
	hasMore := false
	for rows.Next() {
		var (
			q                      domain.AlertQuality
			occ, notif, del        int64
			acked, auto, exp, flap int64
			firing                 int64
			sortValue              float64
			keysetKey              string
		)
		if err := rows.Scan(&q.ClusterKey, &q.AlertName, &occ, &notif, &del,
			&acked, &auto, &exp, &firing, &flap, &sortValue, &keysetKey); err != nil {
			return nil, false, errs.Wrap(err, errs.KindInternal, "stats_quality_scan_failed",
				"could not read the alert-hygiene rollup")
		}
		if len(out) == limit {
			// The extra row proves another page exists without a COUNT.
			hasMore = true
			break
		}
		q.Occurrences = int(occ)
		q.Notifications = int(notif)
		q.Deliveries = int(del)
		q.AckedOccurrences = int(acked)
		q.AutoResolved = int(auto)
		q.Expired = int(exp)
		q.TotalFiringSeconds = firing
		q.FlapTransitions = int(flap)
		out = append(out, QualityRow{Quality: q, SortValue: sortValue, KeysetKey: keysetKey})
	}
	if err := rows.Err(); err != nil {
		return nil, false, errs.Wrap(err, errs.KindInternal, "stats_quality_failed",
			"could not read the alert-hygiene rollup")
	}
	return out, hasMore, nil
}

// overviewSQL reads every dashboard count in ONE round trip.
//
// `resolved` and `expired` are counted into separate columns and are never
// summed, because conflating them is precisely the lie oto exists to prevent.
//
// ⛔⛔ THE FIRST THREE CTEs EXCLUDE DELIVERY DRILLS, and each one had to be
// changed separately because each counts a different table:
//
//   - `a` reads `alerts.synthetic` directly.
//   - `g` reads `alert_groups.synthetic`, which is denormalised at
//     generation-open time for exactly this predicate.
//   - `d` has no column of its own, so it walks the two FKs it already has —
//     delivery -> notification -> group — and reads the group's flag. Both hops
//     are primary-key lookups on a set already bounded by the time window, which
//     is a far better bargain than a fourth denormalised boolean that a future
//     writer could forget to set.
//
// A drill that showed up here would tell an operator their estate had one more
// firing alert, one more open group and one more sent delivery than it does, on
// the dashboard they check first. The other two CTEs need no predicate:
// `source_health` and `channels` count configuration, and a drill creates
// neither.
const overviewSQL = `
WITH a AS (
  SELECT
    COUNT(*) FILTER (WHERE state = 'firing')       AS firing,
    COUNT(*) FILTER (WHERE state = 'suppressed')   AS suppressed,
    COUNT(*) FILTER (WHERE state = 'resolved')     AS resolved,
    COUNT(*) FILTER (WHERE state = 'expired')      AS expired,
    COUNT(*) FILTER (WHERE ack_state = 'acked')    AS acked,
    COUNT(*) FILTER (WHERE ack_state = 'unacked')  AS unacked,
    COUNT(*) FILTER (WHERE is_flapping)            AS flapping
  FROM alerts
 WHERE org_id = $1
   AND NOT synthetic
   AND ($2::text[] IS NULL OR cluster_key = ANY($2))
   AND last_seen_at >= $3 AND last_seen_at <= $4
), g AS (
  SELECT
    COUNT(*) FILTER (WHERE state = 'open')   AS open,
    COUNT(*) FILTER (WHERE state = 'closed') AS closed,
    COUNT(*) FILTER (WHERE storm_mode)       AS storm
  FROM alert_groups
 WHERE org_id = $1 AND NOT synthetic
   AND last_activity_at >= $3 AND last_activity_at <= $4
), d AS (
  SELECT
    COUNT(*) FILTER (WHERE dv.status = 'sent')    AS sent,
    COUNT(*) FILTER (WHERE dv.status = 'failed')  AS failed,
    COUNT(*) FILTER (WHERE dv.status = 'dead')    AS dead,
    COUNT(*) FILTER (WHERE dv.status = 'skipped') AS skipped,
    COUNT(*) FILTER (WHERE dv.status IN ('pending','sending')) AS pending,
    COUNT(*) FILTER (WHERE dv.ambiguous)          AS ambiguous
  FROM notification_deliveries dv
  JOIN notifications n ON n.id = dv.notification_id AND n.org_id = dv.org_id
  JOIN alert_groups ag ON ag.id = n.group_id        AND ag.org_id = n.org_id
 WHERE dv.org_id = $1 AND NOT ag.synthetic
   AND dv.created_at >= $3 AND dv.created_at <= $4
), s AS (
  SELECT
    COUNT(*) FILTER (WHERE status = 'healthy')     AS healthy,
    COUNT(*) FILTER (WHERE status = 'degraded')    AS degraded,
    COUNT(*) FILTER (WHERE status = 'unreachable') AS unreachable,
    COUNT(*) FILTER (WHERE status = 'unknown')     AS unknown,
    COALESCE(MAX(ABS(clock_skew_ms)), 0)           AS max_skew,
    COALESCE(SUM(divergence_count), 0)             AS divergence
  FROM source_health
 WHERE org_id = $1
), c AS (
  SELECT
    COUNT(*) FILTER (WHERE health_status = 'healthy')        AS healthy,
    COUNT(*) FILTER (WHERE health_status = 'degraded')       AS degraded,
    COUNT(*) FILTER (WHERE health_status = 'auth_failed')    AS auth_failed,
    COUNT(*) FILTER (WHERE health_status = 'config_invalid') AS config_invalid
  FROM channels
 WHERE org_id = $1 AND deleted_at IS NULL
)
SELECT a.firing, a.suppressed, a.resolved, a.expired, a.acked, a.unacked, a.flapping,
       g.open, g.closed, g.storm,
       d.sent, d.failed, d.dead, d.skipped, d.pending, d.ambiguous,
       s.healthy, s.degraded, s.unreachable, s.unknown, s.max_skew, s.divergence,
       c.healthy, c.degraded, c.auth_failed, c.config_invalid
  FROM a, g, d, s, c`

// Overview returns the dashboard roll-up for one window.
func (r *StatsRepository) Overview(
	ctx context.Context, s db.TenantScope, since, until time.Time, clusters []string,
) (domain.Overview, error) {
	if !s.Valid() {
		return domain.Overview{}, errs.Forbidden("forbidden", "a tenant scope is required")
	}
	var clusterArg any
	if len(clusters) > 0 {
		clusterArg = clusters
	}

	var o domain.Overview
	err := r.db(ctx).QueryRow(ctx, overviewSQL, s.OrgID(), clusterArg, since, until).Scan(
		&o.Alerts.Firing, &o.Alerts.Suppressed, &o.Alerts.Resolved, &o.Alerts.Expired,
		&o.Alerts.Acked, &o.Alerts.Unacked, &o.Alerts.Flapping,
		&o.Groups.Open, &o.Groups.Closed, &o.Groups.Storm,
		&o.Deliveries.Sent, &o.Deliveries.Failed, &o.Deliveries.Dead,
		&o.Deliveries.Skipped, &o.Deliveries.Pending, &o.Deliveries.Ambiguous,
		&o.Sources.Healthy, &o.Sources.Degraded, &o.Sources.Unreachable, &o.Sources.Unknown,
		&o.Sources.MaxClockSkewMS, &o.Sources.TotalDivergence,
		&o.Channels.Healthy, &o.Channels.Degraded, &o.Channels.AuthFailed, &o.Channels.ConfigInvalid,
	)
	if err != nil {
		return domain.Overview{}, errs.Wrap(err, errs.KindInternal, "stats_overview_failed",
			"could not read the dashboard roll-up")
	}
	return o, nil
}
