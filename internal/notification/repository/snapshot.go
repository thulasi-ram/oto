package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
)

// DefaultMaxAlerts is how many member instances a card shows inline before it
// says "and N more" (§H.3).
const DefaultMaxAlerts = 10

// SnapshotRepository reads the notification READ MODEL.
//
// It is a read model and nothing else: no invariant is enforced here and no row
// it touches is written here. That is what makes it safe for this module to
// project from tables another module owns — the state machine, the projections
// and the events all still have exactly one writer.
//
// It is also the DEFAULT IMPLEMENTATION of the narrow port
// `service.SnapshotSource`. Once `alerts/service` publishes an equivalent, the
// wiring swaps one constructor and this file becomes dead code; until then the
// notification module is not blocked on another module's schedule.
type SnapshotRepository struct {
	q   db.Querier
	clk clock.Clock
}

// NewSnapshotRepository builds the read model over a fallback querier.
func NewSnapshotRepository(q db.Querier, clk clock.Clock) *SnapshotRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &SnapshotRepository{q: q, clk: clk}
}

func (r *SnapshotRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const orgFactsSQL = `SELECT id, slug, name FROM orgs WHERE id = $1`

const groupFactsSQL = `
SELECT id, group_key, generation, coalesce(source_group_key,''), receiver,
       group_labels, title, state, coalesce(severity,''), state_version,
       firing_count, suppressed_count, resolved_count, expired_count,
       total_count, acked_count, storm_mode, storm_since,
       first_seen_at, last_activity_at, closed_at
  FROM alert_groups
 WHERE org_id = $1 AND id = $2`

const memberAlertsSQL = `
SELECT a.id, a.alert_key, a.source_fingerprint, a.alertname,
       coalesce(a.severity,''), coalesce(a.namespace,''), coalesce(a.service,''),
       a.cluster_key, a.labels, a.annotations, coalesce(a.generator_url,''),
       a.state, a.ack_state, a.snoozed_until, a.first_seen_at, a.last_seen_at,
       a.total_occurrences, a.is_flapping, a.flap_score
  FROM alert_group_members m
  JOIN alerts a ON a.id = m.alert_id
 WHERE m.org_id = $1 AND m.group_id = $2 AND m.left_at IS NULL
 ORDER BY a.last_seen_at DESC, a.id DESC
 LIMIT $3`

const memberCountsSQL = `
SELECT count(*),
       count(*) FILTER (WHERE a.snoozed_until IS NOT NULL AND a.snoozed_until > $3)
  FROM alert_group_members m
  JOIN alerts a ON a.id = m.alert_id
 WHERE m.org_id = $1 AND m.group_id = $2 AND m.left_at IS NULL`

const focusAlertSQL = `
SELECT a.id, a.alert_key, a.source_fingerprint, a.alertname,
       coalesce(a.severity,''), coalesce(a.namespace,''), coalesce(a.service,''),
       a.cluster_key, a.labels, a.annotations, coalesce(a.generator_url,''),
       a.state, a.ack_state, a.snoozed_until, a.first_seen_at, a.last_seen_at,
       a.total_occurrences, a.is_flapping, a.flap_score
  FROM alerts a
 WHERE a.org_id = $1 AND a.id = $2`

const occurrenceByIDSQL = `
SELECT id, alert_id, seq, state, coalesce(suppression_reason,''),
       coalesce(resolve_reason,''), started_at, ended_at, reopen_count,
       ack_state, coalesce(acked_by_label,''), acked_at, coalesce(ack_note,''),
       rule_snapshot_id
  FROM alert_occurrences
 WHERE org_id = $1 AND id = $2`

const currentOccurrenceSQL = `
SELECT o.id, o.alert_id, o.seq, o.state, coalesce(o.suppression_reason,''),
       coalesce(o.resolve_reason,''), o.started_at, o.ended_at, o.reopen_count,
       o.ack_state, coalesce(o.acked_by_label,''), o.acked_at, coalesce(o.ack_note,''),
       o.rule_snapshot_id
  FROM alerts a
  JOIN alert_occurrences o ON o.id = a.current_occurrence_id
 WHERE a.org_id = $1 AND a.id = $2`

const ruleSnapshotSQL = `
SELECT id, rule_fingerprint, rule_file, rule_group, rule_name, expr,
       for_seconds, keep_firing_for_seconds, rule_labels, rule_annotations,
       origin, match_confidence, captured_at
  FROM rule_snapshots
 WHERE org_id = $1 AND id = $2`

const previousRuleSnapshotSQL = `
SELECT rs.id, rs.rule_fingerprint, rs.rule_file, rs.rule_group, rs.rule_name,
       rs.expr, rs.for_seconds, rs.keep_firing_for_seconds, rs.rule_labels,
       rs.rule_annotations, rs.origin, rs.match_confidence, rs.captured_at
  FROM alert_occurrences o
  JOIN rule_snapshots rs ON rs.id = o.rule_snapshot_id
 WHERE o.org_id = $1 AND o.alert_id = $2 AND o.seq < $3
   AND o.rule_snapshot_id IS NOT NULL
 ORDER BY o.seq DESC
 LIMIT 1`

const enrichmentsSQL = `
SELECT enricher, status, payload, warnings, coalesce(error,''), computed_at
  FROM enrichments
 WHERE org_id = $1 AND subject_kind = 'occurrence' AND subject_id = $2
 ORDER BY enricher ASC`

// Snapshot builds the whole read model for one delivery, AT CLAIM TIME.
//
// It is several round trips on purpose rather than one heroic join. The group,
// its members, the focused occurrence, the rule and the enrichments have
// genuinely different cardinalities, and a single query would either fan the
// group's columns out across every member row or need lateral subqueries that
// nobody can read a year from now. Delivery is not the hot path — ingestion is —
// and this code is read far more often than it runs.
func (r *SnapshotRepository) Snapshot(
	ctx context.Context, s db.TenantScope, q domain.SnapshotQuery,
) (domain.Snapshot, error) {
	if q.MaxAlerts <= 0 {
		q.MaxAlerts = DefaultMaxAlerts
	}
	now := r.clk.Now().UTC()
	snap := domain.Snapshot{TakenAt: now, SnoozedAlerts: map[uuid.UUID]time.Time{}}

	if err := r.readOrg(ctx, s, &snap); err != nil {
		return domain.Snapshot{}, err
	}
	if err := r.readGroup(ctx, s, q.GroupID, &snap); err != nil {
		return domain.Snapshot{}, err
	}
	if err := r.readMembers(ctx, s, q, now, &snap); err != nil {
		return domain.Snapshot{}, err
	}
	if err := r.readFocus(ctx, s, q, &snap); err != nil {
		return domain.Snapshot{}, err
	}
	if err := r.readOccurrence(ctx, s, q, &snap); err != nil {
		return domain.Snapshot{}, err
	}
	return snap, nil
}

func (r *SnapshotRepository) readOrg(
	ctx context.Context, s db.TenantScope, snap *domain.Snapshot,
) error {
	err := r.db(ctx).QueryRow(ctx, orgFactsSQL, s.OrgID()).
		Scan(&snap.Org.ID, &snap.Org.Slug, &snap.Org.Name)
	return mapErr(err, "org_not_found", "org")
}

func (r *SnapshotRepository) readGroup(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID, snap *domain.Snapshot,
) error {
	var labels []byte
	g := &snap.Group
	err := r.db(ctx).QueryRow(ctx, groupFactsSQL, s.OrgID(), groupID).Scan(
		&g.ID, &g.GroupKey, &g.Generation, &g.SourceGroupKey, &g.Receiver,
		&labels, &g.Title, &g.State, &g.Severity, &g.StateVersion,
		&g.FiringCount, &g.SuppressedCount, &g.ResolvedCount, &g.ExpiredCount,
		&g.TotalCount, &g.AckedCount, &g.StormMode, &g.StormSince,
		&g.FirstSeenAt, &g.LastActivityAt, &g.ClosedAt,
	)
	if err != nil {
		return mapErr(err, "group_not_found", "alert group")
	}
	g.GroupLabels = decodeStringMap(labels)
	if g.StormMode {
		// The storm card counts the alerts that joined this generation. Anything
		// else would understate a collapse that is, by definition, about volume.
		g.StormCount = g.TotalCount
	}
	return nil
}

func (r *SnapshotRepository) readMembers(
	ctx context.Context, s db.TenantScope, q domain.SnapshotQuery, now time.Time,
	snap *domain.Snapshot,
) error {
	rows, err := r.db(ctx).Query(ctx, memberAlertsSQL, s.OrgID(), q.GroupID, q.MaxAlerts)
	if err != nil {
		return mapErr(err, "alert_not_found", "list group members")
	}
	defer rows.Close()

	for rows.Next() {
		a, err := scanAlertFacts(rows)
		if err != nil {
			return mapErr(err, "alert_not_found", "scan a group member")
		}
		if a.SnoozedUntil != nil && a.SnoozedUntil.After(now) {
			snap.SnoozedAlerts[a.ID] = *a.SnoozedUntil
		}
		snap.Alerts = append(snap.Alerts, a)
	}
	if err := rows.Err(); err != nil {
		return mapErr(err, "alert_not_found", "read group members")
	}

	err = r.db(ctx).QueryRow(ctx, memberCountsSQL, s.OrgID(), q.GroupID, now).
		Scan(&snap.MemberCount, &snap.SnoozedMemberCount)
	return mapErr(err, "alert_not_found", "count group members")
}

func (r *SnapshotRepository) readFocus(
	ctx context.Context, s db.TenantScope, q domain.SnapshotQuery, snap *domain.Snapshot,
) error {
	if q.AlertID == nil {
		return nil
	}
	// The focus is usually already in the member slice; reading it separately is
	// what makes the card correct for an alert that has left the group since the
	// intent was minted, which is precisely when a stale card is most misleading.
	a, err := scanAlertFacts(r.db(ctx).QueryRow(ctx, focusAlertSQL, s.OrgID(), *q.AlertID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return mapErr(err, "alert_not_found", "focus alert")
	}
	snap.Focus = &a
	if a.SnoozedUntil != nil {
		snap.SnoozedAlerts[a.ID] = *a.SnoozedUntil
	}
	return nil
}

func (r *SnapshotRepository) readOccurrence(
	ctx context.Context, s db.TenantScope, q domain.SnapshotQuery, snap *domain.Snapshot,
) error {
	var (
		row      pgx.Row
		occ      domain.OccurrenceFacts
		alertID  uuid.UUID
		ruleSnap *uuid.UUID
	)
	switch {
	case q.OccurrenceID != nil:
		row = r.db(ctx).QueryRow(ctx, occurrenceByIDSQL, s.OrgID(), *q.OccurrenceID)
	case q.AlertID != nil:
		row = r.db(ctx).QueryRow(ctx, currentOccurrenceSQL, s.OrgID(), *q.AlertID)
	default:
		return nil
	}

	err := row.Scan(
		&occ.ID, &alertID, &occ.Seq, &occ.State, &occ.SuppressionReason,
		&occ.ResolveReason, &occ.StartedAt, &occ.EndedAt, &occ.ReopenCount,
		&occ.AckState, &occ.AckedByLabel, &occ.AckedAt, &occ.AckNote, &ruleSnap,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return mapErr(err, "occurrence_not_found", "occurrence")
	}
	snap.Occurrence = &occ

	if err := r.readEnrichments(ctx, s, occ.ID, snap); err != nil {
		return err
	}
	if ruleSnap == nil {
		return nil
	}
	return r.readRule(ctx, s, *ruleSnap, alertID, occ.Seq, snap)
}

func (r *SnapshotRepository) readRule(
	ctx context.Context, s db.TenantScope,
	snapshotID, alertID uuid.UUID, seq int, snap *domain.Snapshot,
) error {
	current, err := scanRuleFacts(r.db(ctx).QueryRow(ctx, ruleSnapshotSQL, s.OrgID(), snapshotID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return mapErr(err, "rule_snapshot_not_found", "rule snapshot")
	}
	snap.Rule = &current

	previous, err := scanRuleFacts(r.db(ctx).QueryRow(ctx, previousRuleSnapshotSQL,
		s.OrgID(), alertID, seq))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return mapErr(err, "rule_snapshot_not_found", "previous rule snapshot")
	}
	if previous.Fingerprint == current.Fingerprint {
		// Same content address, same rule. `rule_fingerprint` is a content address
		// (§C.6) precisely so this comparison is exact rather than heuristic.
		return nil
	}
	snap.RuleChange = diffRules(previous, current)
	return nil
}

func (r *SnapshotRepository) readEnrichments(
	ctx context.Context, s db.TenantScope, occurrenceID uuid.UUID, snap *domain.Snapshot,
) error {
	rows, err := r.db(ctx).Query(ctx, enrichmentsSQL, s.OrgID(), occurrenceID)
	if err != nil {
		return mapErr(err, "enrichment_not_found", "list enrichments")
	}
	defer rows.Close()

	for rows.Next() {
		var (
			e       domain.EnrichmentFacts
			payload []byte
		)
		if err := rows.Scan(&e.Enricher, &e.Status, &payload, &e.Warnings,
			&e.Error, &e.ComputedAt); err != nil {
			return mapErr(err, "enrichment_not_found", "scan an enrichment")
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &e.Payload)
		}
		if snap.Enrichments == nil {
			snap.Enrichments = map[string]domain.EnrichmentFacts{}
		}
		snap.Enrichments[e.Enricher] = e
	}
	return mapErr(rows.Err(), "enrichment_not_found", "read enrichments")
}

func scanAlertFacts(row pgx.Row) (domain.AlertFacts, error) {
	var (
		a                  domain.AlertFacts
		labels, annotation []byte
	)
	err := row.Scan(
		&a.ID, &a.AlertKey, &a.SourceFingerprint, &a.AlertName, &a.Severity,
		&a.Namespace, &a.Service, &a.ClusterKey, &labels, &annotation,
		&a.GeneratorURL, &a.State, &a.AckState, &a.SnoozedUntil,
		&a.FirstSeenAt, &a.LastSeenAt, &a.TotalOccurrences, &a.IsFlapping, &a.FlapScore,
	)
	if err != nil {
		return domain.AlertFacts{}, err
	}
	a.Labels = decodeStringMap(labels)
	a.Annotations = decodeStringMap(annotation)
	return a, nil
}

func scanRuleFacts(row pgx.Row) (domain.RuleFacts, error) {
	var (
		f                   domain.RuleFacts
		forS, keepS         float64
		labels, annotations []byte
	)
	err := row.Scan(
		&f.SnapshotID, &f.Fingerprint, &f.File, &f.Group, &f.Name, &f.Expr,
		&forS, &keepS, &labels, &annotations, &f.Origin, &f.MatchConfidence, &f.CapturedAt,
	)
	if err != nil {
		return domain.RuleFacts{}, err
	}
	f.For = time.Duration(forS * float64(time.Second))
	f.KeepFiringFor = time.Duration(keepS * float64(time.Second))
	f.Labels = decodeStringMap(labels)
	f.Annotations = decodeStringMap(annotations)
	return f, nil
}

// diffRules is the headline differentiator's payload: what actually changed in
// the rule between two occurrences of the same alert.
//
// It compares the DEFINITION, not the rendered text: an expression that was
// reformatted is not a change an operator needs woken up about, and one whose
// threshold moved from 0.05 to 0.03 is.
func diffRules(previous, current domain.RuleFacts) *domain.RuleChangeFacts {
	d := &domain.RuleChangeFacts{
		PreviousSnapshotID:  previous.SnapshotID,
		PreviousFingerprint: previous.Fingerprint,
		PreviousCapturedAt:  previous.CapturedAt,
		ExprChanged:         previous.Expr != current.Expr,
		PreviousExpr:        previous.Expr,
		NewExpr:             current.Expr,
		ForChanged:          previous.For != current.For,
		PreviousFor:         previous.For,
		NewFor:              current.For,
		LabelDiff:           diffMaps(previous.Labels, current.Labels),
		AnnotationDiff:      diffMaps(previous.Annotations, current.Annotations),
	}
	return d
}

func diffMaps(before, after map[string]string) map[string][2]string {
	out := map[string][2]string{}
	for k, v := range before {
		if av, ok := after[k]; !ok || av != v {
			out[k] = [2]string{v, after[k]}
		}
	}
	for k, v := range after {
		if _, ok := before[k]; !ok {
			out[k] = [2]string{"", v}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodeStringMap turns a JSONB object into a label map. A decode failure yields
// an empty map rather than an error: a card missing one label is far better than
// a card that never renders, and the raw row is still on disk for anyone asking
// why.
func decodeStringMap(b []byte) map[string]string {
	if len(b) == 0 {
		return map[string]string{}
	}
	out := map[string]string{}
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]string{}
	}
	return out
}
