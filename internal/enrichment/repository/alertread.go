package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/enrichment/enrichers/alerthistory"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/relatedalerts"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// AlertReadModel is a READ-ONLY projection over the `alerts` module's tables,
// serving the two enrichers whose whole job is to summarise alert history.
//
// It reads `alerts` and `alert_cases`, and it
// writes exactly one column that exists for it: `alert_cases
// .rule_snapshot_id` (SPEC §D.6). Nothing here touches the state machine, the
// timeline or a lifecycle column.
//
// This is a deliberate, bounded exception to "a module reads its own tables",
// and it is documented rather than hidden. The alternative is a chatty
// service-to-service call inside a 200 ms budget for what is one indexed
// aggregate. When `alerts/service` grows a query port of its own shape, these
// three statements move behind it and the enricher ports do not change — which
// is exactly why the enrichers declare their own Store interfaces rather than
// depending on this type.
type AlertReadModel struct {
	q db.Querier
}

// Compile-time proof that the read model satisfies both enricher ports.
var (
	_ alerthistory.Store  = (*AlertReadModel)(nil)
	_ relatedalerts.Store = (*AlertReadModel)(nil)
)

// NewAlertReadModel builds the projection over a fallback querier.
func NewAlertReadModel(q db.Querier) *AlertReadModel { return &AlertReadModel{q: q} }

func (r *AlertReadModel) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const alertProjectionSQL = `
SELECT total_cases, flap_score, is_flapping, first_seen_at, last_seen_at
  FROM alerts
 WHERE org_id = $1 AND id = $2`

// caseCountsSQL counts episodes in three rolling windows with ONE scan.
//
// Three FILTERed aggregates over one range predicate, not three queries: the
// 30-day bound rides alerts_list_idx's sibling case_alert_idx (org_id, alert_id,
// seq DESC) and the two tighter windows are then free.
const caseCountsSQL = `
SELECT count(*) FILTER (WHERE started_at >= $3) AS c24h,
       count(*) FILTER (WHERE started_at >= $4) AS c7d,
       count(*)                                 AS c30d
  FROM alert_cases
 WHERE org_id = $1 AND alert_id = $2 AND started_at >= $5`

// firingDurationsSQL samples the CLOSED episodes, newest first.
//
// "Firing duration" is the vocabulary, and it is the only vocabulary: this is
// how long the SIGNAL was firing. It is not MTTR and must never be described as
// one — MTTR measures humans and is permanently out of scope (SPEC §A.1, R8).
//
// Only closed episodes are sampled: an episode still firing has no duration
// yet, and counting "so far" into a distribution would drag every percentile
// down whenever something is currently broken.
const firingDurationsSQL = `
SELECT EXTRACT(EPOCH FROM (ended_at - started_at))::float8
  FROM alert_cases
 WHERE org_id = $1 AND alert_id = $2 AND ended_at IS NOT NULL AND started_at >= $3
 ORDER BY started_at DESC
 LIMIT $4`

// AlertHistory returns the counts and firing durations for one Alert identity.
func (r *AlertReadModel) AlertHistory(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, now time.Time,
) (alerthistory.Stats, error) {
	now = now.UTC()
	from24h := now.Add(-24 * time.Hour)
	from7d := now.Add(-7 * 24 * time.Hour)
	from30d := now.Add(-30 * 24 * time.Hour)

	batch := &pgx.Batch{}
	batch.Queue(alertProjectionSQL, s.OrgID(), alertID)
	batch.Queue(caseCountsSQL, s.OrgID(), alertID, from24h, from7d, from30d)
	batch.Queue(firingDurationsSQL, s.OrgID(), alertID, from30d, alerthistory.SampleLimit)

	results := r.db(ctx).SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()

	var out alerthistory.Stats

	err := results.QueryRow().Scan(
		&out.TotalCases, &out.FlapScore, &out.IsFlapping, &out.FirstSeenAt, &out.LastSeenAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// An alert that vanished between the fire and the enrichment is not an
		// error: the enricher reports an empty history and the pipeline moves on.
		return alerthistory.Stats{}, nil
	case err != nil:
		return alerthistory.Stats{}, mapErr(err, CodeQueryFailed,
			"could not read the alert's history")
	}

	if err := results.QueryRow().Scan(&out.Count24h, &out.Count7d, &out.Count30d); err != nil {
		return alerthistory.Stats{}, mapErr(err, CodeQueryFailed,
			"could not count the alert's cases")
	}

	rows, err := results.Query()
	if err != nil {
		return alerthistory.Stats{}, mapErr(err, CodeQueryFailed,
			"could not read the alert's firing durations")
	}
	for rows.Next() {
		var seconds float64
		if err := rows.Scan(&seconds); err != nil {
			rows.Close()
			return alerthistory.Stats{}, mapErr(err, CodeQueryFailed,
				"could not read the alert's firing durations")
		}
		out.FiringDurationsSeconds = append(out.FiringDurationsSeconds, seconds)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return alerthistory.Stats{}, mapErr(err, CodeQueryFailed,
			"could not read the alert's firing durations")
	}

	out.FirstSeenAt = out.FirstSeenAt.UTC()
	out.LastSeenAt = out.LastSeenAt.UTC()
	return out, nil
}

// relatedAlertsSQL finds the alerts that were firing nearby.
//
// The relations are evaluated in ONE pass with a CASE, and the CASE is ORDERED:
// same_alertname beats same_namespace. That ordering is a claim about signal
// strength, not an implementation convenience — the same rule firing twice says
// more than two unrelated rules sharing a namespace. Each alert therefore appears
// under its STRONGEST relation and never twice.
//
// ⛔ THE `subject` CTE AND THE `same_group` ARM ARE DELETED (git-bug `7570090`).
// The CTE was `SELECT group_id FROM alert_cases WHERE org_id = $1 AND id = $2` and
// the arm read `o.group_id IS NOT NULL AND o.group_id = (SELECT group_id FROM
// subject) THEN 'same_group'`, ranked ABOVE both survivors because group membership
// was Alertmanager's own routing decision while the label relations are oto's
// inference. 00069 drops `alert_cases.group_id`, so this is a forced deletion and
// not a preference: left in place the statement raises a 42703 on every execution
// and the enricher fails wholesale, taking the two working relations with it.
//
// ⚠️ THE PLACEHOLDERS ARE RENUMBERED AND HAD TO BE — LEAVING THE GAP DOES NOT
// COMPILE, WHICH IS WORTH KNOWING BEFORE SOMEBODY "TIDIES" IT BACK. The case id
// was `$2` and the CTE was its only reader, so dropping the CTE left a bound
// parameter that no expression mentions. Postgres cannot infer a type for such a
// parameter and refuses the statement outright with
// `42P18: could not determine data type of parameter $2` — at PREPARE time, on
// every call, before a single row is considered. Every placeholder after it
// therefore shifts down by one, and `relatedalerts.Query` loses its `CaseID`
// field: nothing else in this statement ever read it.
//
// DISTINCT ON collapses an alert that has several episodes in the window down
// to its newest, so a flapping neighbour contributes one line rather than forty.
const relatedAlertsSQL = `
WITH candidates AS (
  SELECT o.id           AS case_id,
         o.alert_id     AS alert_id,
         -- ADR 0040: alert_cases.state is open | closed, so the four-word reading
         -- an enrichment renders is recomposed here. The join to alerts is already
         -- on this statement for alertname and severity, and an OPEN episode is
         -- always its alert's current one (case_one_open_idx), so a.state IS this
         -- episode's state; a closed one says which kind of ending it had in
         -- resolve_reason, the column that always carried that.
         -- ADR 0041: 'suppressed' is an axis, not a state, so the display
         -- reading recomposes it from a.suppression_reason. a.state now says
         -- 'firing' throughout a silence, which is what every COUNT needed.
         CASE WHEN o.state = 'open' AND a.suppression_reason IS NOT NULL THEN 'suppressed'
              WHEN o.state = 'open' THEN a.state
              WHEN o.resolve_reason = 'timeout' THEN 'expired'
              ELSE 'resolved' END              AS state,
         o.started_at   AS started_at,
         a.alert_key    AS alert_key,
         a.alertname    AS alertname,
         a.severity     AS severity,
         a.namespace    AS namespace,
         a.service      AS service,
         CASE
           WHEN $4 <> ''  AND a.alertname = $4 THEN 'same_alertname'
           WHEN $5 <> ''  AND a.namespace = $5 THEN 'same_namespace'
         END AS relation
    FROM alert_cases o
    JOIN alerts a ON a.id = o.alert_id AND a.org_id = o.org_id
   WHERE o.org_id = $1
     -- A DELIVERY DRILL is never "what else was firing". Nothing fired: oto
     -- manufactured it to prove the notification path works, and offering it as
     -- context during a real incident would be actively misleading.
     AND NOT a.synthetic
     AND o.alert_id <> $2
     AND o.started_at >= $3
     AND o.started_at <  $6
),
deduped AS (
  SELECT DISTINCT ON (alert_id) *
    FROM candidates
   WHERE relation IS NOT NULL
   ORDER BY alert_id, started_at DESC
),
ranked AS (
  SELECT d.*,
         row_number() OVER (PARTITION BY relation ORDER BY started_at DESC, alert_id DESC) AS rn,
         count(*)     OVER (PARTITION BY relation)                                         AS total
    FROM deduped d
)
SELECT relation, alert_id, alert_key, alertname, severity, namespace, service,
       state, case_id, started_at, total
  FROM ranked
 WHERE rn <= $7
 ORDER BY relation, started_at DESC`

// RelatedAlerts returns what else was firing around this case.
func (r *AlertReadModel) RelatedAlerts(
	ctx context.Context, s db.TenantScope, q relatedalerts.Query,
) ([]relatedalerts.Related, map[string]int, error) {
	limit := q.Limit
	if limit <= 0 || limit > 50 {
		limit = relatedalerts.MaxPerRelation
	}

	rows, err := r.db(ctx).Query(ctx, relatedAlertsSQL,
		s.OrgID(), q.AlertID,
		q.From.UTC(), q.AlertName, q.Namespace, q.To.UTC(), limit)
	if err != nil {
		return nil, nil, mapErr(err, CodeQueryFailed, "could not read the related alerts")
	}
	defer rows.Close()

	var (
		out    []relatedalerts.Related
		counts = map[string]int{}
	)
	for rows.Next() {
		var (
			rel                          relatedalerts.Related
			alertID, caseID              uuid.UUID
			severity, namespace, service *string
			total                        int64
		)
		if err := rows.Scan(
			&rel.Relation, &alertID, &rel.AlertKey, &rel.AlertName,
			&severity, &namespace, &service,
			&rel.State, &caseID, &rel.StartedAt, &total,
		); err != nil {
			return nil, nil, mapErr(err, CodeQueryFailed, "could not read the related alerts")
		}
		rel.AlertID = alertID.String()
		rel.CaseID = caseID.String()
		rel.Severity = deref(severity)
		rel.Namespace = deref(namespace)
		rel.Service = deref(service)
		rel.StartedAt = rel.StartedAt.UTC()

		out = append(out, rel)
		counts[rel.Relation] = int(total)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, mapErr(err, CodeQueryFailed, "could not read the related alerts")
	}
	return out, counts, nil
}

// bindRuleSnapshotSQL pins the captured rule to the episode it explains.
//
// It is guarded by `rule_snapshot_id IS NULL` so the binding is WRITE-ONCE: the
// snapshot bound to a case is the rule as it was WHEN THAT CASE
// FIRED, and a re-run of the enricher hours later must never quietly replace it
// with a newer capture. Overwriting it would destroy the one fact the whole
// feature exists to preserve.
const bindRuleSnapshotSQL = `
UPDATE alert_cases
   SET rule_snapshot_id = $3, updated_at = now()
 WHERE org_id = $1 AND id = $2 AND rule_snapshot_id IS NULL`

// BindRuleSnapshot pins a snapshot to a case, once.
func (r *AlertReadModel) BindRuleSnapshot(
	ctx context.Context, s db.TenantScope, caseID, snapshotID uuid.UUID,
) error {
	if caseID == uuid.Nil || snapshotID == uuid.Nil {
		return errs.New(errs.KindValidation, "enrichment_bad_binding",
			"binding a rule snapshot needs both a case and a snapshot")
	}
	if _, err := r.db(ctx).Exec(ctx, bindRuleSnapshotSQL, s.OrgID(), caseID, snapshotID); err != nil {
		return mapErr(err, CodeWriteFailed, "could not bind the rule snapshot to the case")
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
