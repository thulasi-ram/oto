package repository

// THE SQL behind the Stats tag.
//
// ⭐ ADR 0014 is binding: the hygiene report is served from the
// `alert_quality_daily` ROLLUP and never from a scan of `alert_events`. The
// rollup's primary key `(org_id, day, cluster_key, alertname)` is already the
// access path for every read here, which is what keeps Postgres-only viable.
//
// The overview counts read the CURRENT-STATE PROJECTIONS on `alerts`,
// `notification_deliveries`, `source_health` and `channels` — each an indexed
// column on a bounded table — and never the append-only event stream. It also
// touches `alert_cases`, twice and only ever by primary key: once for the ack
// axis, which lives on the firing episode, and once as the middle hop of the
// drill exclusion. Neither is a count over that table.
//
// ⛔ `alert_groups` WAS THE FIFTH PROJECTION IN THAT LIST AND IS DELETED (git-bug
// `7570090`, migration `00069`). It supplied the group half of the roll-up and the
// `synthetic` flag the delivery count filtered on; the first is gone from the
// contract and the second moved to `alerts.synthetic`, which was always the source
// of truth. See `overviewSQL` below for the whole argument.

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
         SUM(cases)::bigint          AS cases,
         SUM(notifications)::bigint        AS notifications,
         SUM(deliveries)::bigint           AS deliveries,
         SUM(acked_cases)::bigint    AS acked_cases,
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
           WHEN 'ack_rate'              THEN CASE WHEN cases > 0
                                                  THEN acked_cases::double precision / cases
                                                  ELSE 0 END
           WHEN '-flap_transitions'     THEN flap_transitions::double precision
           WHEN '-total_firing_seconds' THEN total_firing_seconds::double precision
           ELSE cases::double precision
         END AS sort_value
    FROM rollup
)
SELECT cluster_key, alertname, cases, notifications, deliveries,
       acked_cases, auto_resolved, expired, total_firing_seconds,
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
		return nil, false, mapErr(err, "stats_quality_failed",
			"could not read the alert-hygiene rollup")
	}
	defer rows.Close()

	out := make([]QualityRow, 0, limit)
	hasMore := false
	for rows.Next() {
		var (
			q                      domain.AlertQuality
			ac, notif, del         int64
			acked, auto, exp, flap int64
			firing                 int64
			sortValue              float64
			keysetKey              string
		)
		if err := rows.Scan(&q.ClusterKey, &q.AlertName, &ac, &notif, &del,
			&acked, &auto, &exp, &firing, &flap, &sortValue, &keysetKey); err != nil {
			return nil, false, mapErr(err, "stats_quality_scan_failed",
				"could not read the alert-hygiene rollup")
		}
		if len(out) == limit {
			// The extra row proves another page exists without a COUNT.
			hasMore = true
			break
		}
		q.Cases = int(ac)
		q.Notifications = int(notif)
		q.Deliveries = int(del)
		q.AckedCases = int(acked)
		q.AutoResolved = int(auto)
		q.Expired = int(exp)
		q.TotalFiringSeconds = firing
		q.FlapTransitions = int(flap)
		out = append(out, QualityRow{Quality: q, SortValue: sortValue, KeysetKey: keysetKey})
	}
	if err := rows.Err(); err != nil {
		return nil, false, mapErr(err, "stats_quality_failed",
			"could not read the alert-hygiene rollup")
	}
	return out, hasMore, nil
}

// overviewSQL reads every dashboard count in ONE round trip.
//
// `resolved` and `expired` are counted into separate columns and are never
// summed, because conflating them is precisely the lie oto exists to prevent.
//
// ⛔⛔ THE FIRST TWO CTEs EXCLUDE DELIVERY DRILLS, and both now read the SAME
// column, because there is only one honest one:
//
//   - `a` reads `alerts.synthetic` directly.
//   - `d` has no column of its own, so it walks the FKs it already has —
//     delivery -> notification -> case -> alert — and reads the ALERT's flag.
//     Every hop is a primary-key lookup on a set already bounded by the time
//     window, which is a far better bargain than a denormalised boolean that a
//     future writer could forget to set.
//
// A drill that showed up here would tell an operator their estate had one more
// firing alert and one more sent delivery than it does, on the dashboard they
// check first. The other two CTEs need no predicate: `source_health` and
// `channels` count configuration, and a drill creates neither.
//
// ⛔ THERE WAS A THIRD, `g`, AND IT IS DELETED WITH `alert_groups` (git-bug
// `7570090`, migration `00069`). It counted open and closed generations off
// `alert_groups.state` and excluded drills with `NOT alert_groups.synthetic`.
// A Case is the conversation now; there is no container to count and no
// denormalised copy of the flag to read.
//
// ⛔ AND THE COPY `d` USED TO READ HAD ALREADY OUTLIVED ITS ONLY JUSTIFICATION.
// 00039 added `alert_groups.synthetic` — in its own words — so the dashboard could
// exclude drills "with an indexed predicate instead of a nested loop through
// `alert_group_members`". `00051_membership_is_derived_not_recorded.sql` DROPPED
// that table and made membership a column on the Case, so the nested loop the
// denormalisation was bought to avoid stopped existing one release before this
// ticket touched it. What remained was a second boolean with a write-path
// obligation and no performance to show for it. `alerts.synthetic` is the source
// of truth (00039:73), and its comment is unambiguous about what happens if an
// aggregate over `notification_deliveries` skips it: "it is wrong".
//
// ⭐⭐ THE NEW PATH GOES THROUGH THE CASE, NOT STRAIGHT TO THE ALERT, AND THAT IS
// FORCED RATHER THAN PREFERRED. `notifications` carries both `alert_id` and
// `case_id`, and the two are NOT equally present:
//
//   - `alert_id` is the NARROWER fact and is OPTIONAL. `notifications_focus_ck`
//     (00011:233) demands it for four Reasons only — acked, unacked, refired,
//     rule_changed — and `notification/service.NotifyInput` says so itself:
//     "`AlertID` stays optional because it is the narrower fact".
//   - `case_id` is written for EVERY non-digest intent. `Notify` refuses a nil
//     `CaseID` at the door, and 00069 makes the Case the conversation, so a
//     notification that is about anything at all is about one firing episode.
//
// Joining `alert_id` would therefore leave every drill delivery whose notification
// named no alert UNEXCLUDED — a `fired` drill notification is exactly such a row —
// and it would do it silently, inflating `sent` with oto's own plumbing. The
// second hop cannot fail in the other direction either: `alert_cases.alert_id` is
// `NOT NULL REFERENCES alerts(id)` (00007:122), so a Case always resolves to its
// Alert and the two-hop path is total wherever the one-hop path is.
//
// ⚠️ BOTH HOPS ARE LEFT JOINS AND THE PREDICATE IS `COALESCE(…, false)`, FOR ONE
// ROW SHAPE: A DIGEST. A digest names no alert and no case — its subject is a
// WINDOW OVER A NAMESPACE, the pair `(policy_id, digest_window_start)` (00058) —
// so the chain reaches no `alerts` row and `al.synthetic` is NULL. A digest's
// deliveries MUST BE COUNTED: it is a real message to a real operator, and it can
// never be a drill artefact because the digest reader excludes synthetics from its
// CONTENTS (00058, `notification/repository/digest.go`). Two ways to get that
// wrong, both silent:
//
//   - an INNER join drops every digest delivery from the dashboard;
//   - a bare `NOT al.synthetic` drops them too, because NULL is not false and a
//     predicate that evaluates to NULL discards the row.
//
// `COALESCE(al.synthetic, false)` is what states "unreachable means not synthetic"
// out loud rather than leaving the digest's fate to three-valued logic.
//
// ⛔ AND THE SOURCE CTE JOINS `alert_sources`, BECAUSE A DELETED SOURCE GOES ON
// REPORTING ITS LAST VERDICT FOREVER. `SoftDelete` sets `alert_sources.deleted_at`
// and leaves the `source_health` row exactly as the reconciler last wrote it;
// nothing in the system ever removes that row, and the `ON DELETE CASCADE` the
// table declares fires only on a hard delete this codebase deliberately never
// performs. Unjoined, an org that deleted a source while it was unreachable read
// as permanently unreachable — on the dashboard, and on the shell strip that is
// the sole surface for the reaper guard, over a sources screen with nothing wrong
// on it and no source left to name. The `c` CTE has always carried the same
// `deleted_at IS NULL` predicate; it needs no join only because a channel carries
// its own health column.
//
// `max_skew` and `divergence` were overstated by exactly the same rows and are
// corrected by the same join. The worst skew oto ever measured on a source it no
// longer polls is not a fact about the estate, and a divergence count from a
// retired Alertmanager is a canary for a correctness bug nobody can act on.
//
// The join needs no new index: `source_health.source_id` is that table's primary
// key and `alert_sources.id` is its own, so this is a key lookup per source over
// a table holding one row per configured Alertmanager. `source_health_status_idx
// (org_id, status)` still serves the org filter. The redundant `org_id` equality
// is deliberate — `source_health.org_id` is denormalised with no FK (§D.2), so
// joining on it is what asserts the two rows agree about the tenant.
//
// ⭐ THE ACK COUNTS COME FROM THE CASE, NOT FROM `alerts`. An ack is a receipt
// for ONE firing episode, so `alerts` carries no ack column: the CTE reaches the
// alert's current case by primary key — `current_case_id` is the
// FK — and asks it. `IS DISTINCT FROM 'acked'` rather than `= 'unacked'` because
// an alert with no case at all has nobody's receipt on it, and the honest
// bucket for "no receipt" is unacked.
const overviewSQL = `
WITH a AS (
  SELECT
    -- ADR 0041: firing counts a silenced firing alert, because it IS firing.
    -- suppressed is a SUBSET of it and answers the other axis -- how many of the
    -- live ones is Alertmanager not delivering. Before the axis existed this
    -- overview under-reported firing by exactly the silenced set, which is the
    -- set an operator has most recently touched.
    COUNT(*) FILTER (WHERE al.state = 'firing')                AS firing,
    COUNT(*) FILTER (WHERE al.suppression_reason IS NOT NULL)  AS suppressed,
    COUNT(*) FILTER (WHERE al.state = 'resolved')     AS resolved,
    COUNT(*) FILTER (WHERE al.state = 'expired')      AS expired,
    COUNT(*) FILTER (WHERE o.ack_state = 'acked')     AS acked,
    COUNT(*) FILTER (WHERE o.ack_state IS DISTINCT FROM 'acked') AS unacked,
    COUNT(*) FILTER (WHERE al.is_flapping)            AS flapping
  FROM alerts al
  LEFT JOIN alert_cases o ON o.id = al.current_case_id
 WHERE al.org_id = $1
   AND NOT al.synthetic
   AND ($2::text[] IS NULL OR al.cluster_key = ANY($2))
   AND al.last_seen_at >= $3 AND al.last_seen_at <= $4
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
  -- delivery -> notification -> case -> alert, reading alerts.synthetic, which is
  -- the source of truth (00039). LEFT, both of them, and the predicate COALESCEs:
  -- a digest names no case, so the chain ends at NULL there, and a digest delivery
  -- is a real message that must be COUNTED. See the header for why this is the
  -- case path and not n.alert_id.
  LEFT JOIN alert_cases ac ON ac.id = n.case_id   AND ac.org_id = n.org_id
  LEFT JOIN alerts      al ON al.id = ac.alert_id AND al.org_id = ac.org_id
 WHERE dv.org_id = $1 AND NOT COALESCE(al.synthetic, false)
   AND dv.created_at >= $3 AND dv.created_at <= $4
), s AS (
  SELECT
    COUNT(*) FILTER (WHERE sh.status = 'healthy')     AS healthy,
    COUNT(*) FILTER (WHERE sh.status = 'degraded')    AS degraded,
    COUNT(*) FILTER (WHERE sh.status = 'unreachable') AS unreachable,
    COUNT(*) FILTER (WHERE sh.status = 'unknown')     AS unknown,
    COALESCE(MAX(ABS(sh.clock_skew_ms)), 0)           AS max_skew,
    COALESCE(SUM(sh.divergence_count), 0)             AS divergence
  FROM source_health sh
  JOIN alert_sources src ON src.id = sh.source_id AND src.org_id = sh.org_id
 WHERE sh.org_id = $1 AND src.deleted_at IS NULL
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
       d.sent, d.failed, d.dead, d.skipped, d.pending, d.ambiguous,
       s.healthy, s.degraded, s.unreachable, s.unknown, s.max_skew, s.divergence,
       c.healthy, c.degraded, c.auth_failed, c.config_invalid
  FROM a, d, s, c`

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
		&o.Deliveries.Sent, &o.Deliveries.Failed, &o.Deliveries.Dead,
		&o.Deliveries.Skipped, &o.Deliveries.Pending, &o.Deliveries.Ambiguous,
		&o.Sources.Healthy, &o.Sources.Degraded, &o.Sources.Unreachable, &o.Sources.Unknown,
		&o.Sources.MaxClockSkewMS, &o.Sources.TotalDivergence,
		&o.Channels.Healthy, &o.Channels.Degraded, &o.Channels.AuthFailed, &o.Channels.ConfigInvalid,
	)
	if err != nil {
		return domain.Overview{}, mapErr(err, "stats_overview_failed",
			"could not read the dashboard roll-up")
	}
	return o, nil
}
