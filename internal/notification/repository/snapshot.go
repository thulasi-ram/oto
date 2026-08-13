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

// groupFactsSQL reads the card's group-level facts, and decides ONE thing about
// the joined source beyond its address: whether that address is somewhere oto
// may send an operator.
//
// ⛔ THE `kind` PREDICATE IS THE WHOLE POINT OF THE `CASE`, and it is why this
// query reads a second column it never returns. `base_url` is documented
// everywhere as the Alertmanager API root; for `alertmanager` the API root and
// the UI root are the same origin, so `<base>/#/alerts` and
// `<base>/#/silences/new` resolve. For `grafana` neither half holds: the source
// factory appends stock `/api/v2/...` paths without ever reading kind, so a
// grafana `base_url` must already carry Grafana's AM-compat prefix — and Grafana
// serves its silences at `/alerting/silences`, not at `/#/silences`. Whichever
// way an operator configured it, the link would be wrong.
//
// So a source oto cannot vouch for yields the EMPTY STRING, which the renderer
// already draws as no link and no Silence button. That is the same verdict
// `silenceBaseURLs` reaches on the silences feed by leaving such a source out of
// its map (`internal/app/silencesource.go`): kind is the filter, absence is the
// answer, and the layer above renders nothing rather than something wrong.
//
// ⚠️ IT IS DECIDED HERE, NOT UPSTAIRS, and that is deliberate. This file is
// already the one place in `notification` that names `alert_sources` and its
// columns; carrying the raw kind instead would put the source-kind taxonomy into
// `notification/domain` (a field) and `notification/service` (a comparison) as
// well — three copies of an enum this module has no business knowing. What
// crosses the boundary is a URL oto has vouched for, or nothing, and the layers
// above have no vocabulary for the difference.
const groupFactsSQL = `
SELECT g.id, g.group_key, g.generation, coalesce(g.source_group_key,''), g.receiver,
       g.group_labels, g.title, g.state, coalesce(g.severity,''), g.state_version,
       g.firing_count, g.suppressed_count, g.resolved_count, g.expired_count,
       g.total_count, g.acked_count, g.storm_mode, g.storm_since,
       coalesce(g.last_notification_reason,''),
       CASE WHEN s.kind = 'alertmanager' THEN coalesce(s.base_url,'') ELSE '' END,
       g.first_seen_at, g.last_activity_at, g.closed_at
  FROM alert_groups g
  LEFT JOIN alert_sources s ON s.id = g.source_id
 WHERE g.org_id = $1 AND g.id = $2`

// groupFiringSinceSQL is the UPSTREAM start of the generation (§H.3 "Started").
//
// It is `min(source_starts_at)` over the members that are still live, because
// that — not oto's `first_seen_at` — is when the thing an operator is being
// woken up about actually began. `started_at` is the fallback for an occurrence
// whose upstream sent no usable `startsAt`; taking the two in one `least` keeps
// a member with a missing upstream time from dropping out of the aggregate
// entirely.
//
// It reads every occurrence of the GENERATION rather than only the live ones, so
// a resolved card's Duration still spans the whole episode. `occ_group_idx`
// (org_id, group_id, started_at DESC) is the index it uses.
const groupFiringSinceSQL = `
SELECT min(least(o.source_starts_at, o.started_at))
  FROM alert_occurrences o
 WHERE o.org_id = $1 AND o.group_id = $2`

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

// MaxTrailEntries bounds the state trail rendered on a card (§H.4).
//
// A long-lived flapping alert would otherwise grow the trail without bound and
// blow the section budget; the renderer elides the middle rather than the end,
// because the first transition and the last are the two a reader needs.
const MaxTrailEntries = 12

// groupTrailSQL reads the group's own state history.
//
// The type list is CLOSED and short on purpose: this is the card's receipt, not
// the timeline. `alert.mutated`, the enrichment events and the notification
// events are all real history and all belong in oto's timeline view; putting
// them here would turn a four-line trail into a scrollback and teach people to
// ignore it.
//
// Ordered by `recorded_at` — oto's clock, which is the causal order — and
// DISPLAYED by `occurred_at`, which is upstream's. Conflating the two is how a
// skewed cluster gets a trail that reads backwards.
const groupTrailSQL = `
SELECT type, occurred_at, coalesce(actor_label,'')
  FROM alert_events
 WHERE org_id = $1 AND group_id = $2
   AND type IN ('occurrence.opened','occurrence.reopened','occurrence.suppressed',
                'occurrence.unsuppressed','occurrence.resolved','occurrence.expired',
                'occurrence.acknowledged','occurrence.unacknowledged',
                'group.opened','group.closed','group.storm_started','group.storm_ended')
 ORDER BY recorded_at DESC, id DESC
 LIMIT $3`

// causeEventTypes maps a Reason onto the `alert_events` type whose row IS that
// fact — the one entry on the timeline that knows who caused it and, for a
// comment, what they said.
//
// ⛔ IT IS FOUR ENTRIES AND NOT THE WHOLE REASON ENUM, and the absences are the
// design. A reason with no entry costs NOTHING — no row, no round trip — and
// `fired`, `repeat`, `new_alerts`, `all_resolved` and the rest are caused by the
// world rather than by anybody, so there is no name to fetch and no sentence on
// the card that would carry one. What is here is exactly the set the renderer
// attributes: §E.1.1's human verbs as a card renders them (an acknowledgement,
// its withdrawal, a comment) plus the silence, which is attributed too and whose
// answer is that no human at oto caused it.
//
// ⛔ `snoozed` AND `unsnoozed` ARE DELIBERATELY ABSENT. A snooze is a fact about
// oto's own notifications, the Slack renderer has no `snoozed` card at all, and
// a query whose result nothing renders is a query nobody should pay for.
var causeEventTypes = map[domain.Reason]string{
	domain.ReasonAcked:      "occurrence.acknowledged",
	domain.ReasonUnacked:    "occurrence.unacknowledged",
	domain.ReasonComment:    "comment.added",
	domain.ReasonSuppressed: "occurrence.suppressed",
}

// causeByOccurrenceSQL, causeByAlertSQL and causeByGroupSQL read the ONE event
// that caused the fact being rendered: the newest of its type against the
// narrowest subject the notification names.
//
// Three statements rather than one with an `OR`, because an `OR` across three
// columns cannot use any of their indexes: they ride `ev_occ_idx`
// (org_id, occurrence_id, recorded_at DESC, id DESC), `ev_alert_idx` and
// `ev_group_idx` respectively, and each is a LIMIT 1 walk backwards from the
// newest row.
//
// `payload->>'body'` is the comment's text and is NULL for the other three
// types, which is why one set of statements serves all four: the body column is
// simply empty for a fact that has no text.
const causeByOccurrenceSQL = `
SELECT actor_kind, coalesce(actor_id,''), coalesce(actor_label,''),
       coalesce(payload->>'body','')
  FROM alert_events
 WHERE org_id = $1 AND occurrence_id = $2 AND type = $3
 ORDER BY recorded_at DESC, id DESC
 LIMIT 1`

const causeByAlertSQL = `
SELECT actor_kind, coalesce(actor_id,''), coalesce(actor_label,''),
       coalesce(payload->>'body','')
  FROM alert_events
 WHERE org_id = $1 AND alert_id = $2 AND type = $3
 ORDER BY recorded_at DESC, id DESC
 LIMIT 1`

const causeByGroupSQL = `
SELECT actor_kind, coalesce(actor_id,''), coalesce(actor_label,''),
       coalesce(payload->>'body','')
  FROM alert_events
 WHERE org_id = $1 AND group_id = $2 AND type = $3
 ORDER BY recorded_at DESC, id DESC
 LIMIT 1`

// groupNotificationsSQL counts what oto has SAID about this group.
//
// "How loud was this?" is a question about oto's own behaviour, and oto is the
// only thing that can answer it — it belongs on the receipt beside how long the
// outage lasted. Suppressed intents are excluded: they are notifications oto
// decided NOT to send, and counting them would report noise that never happened.
//
// `notif_subject_idx (org_id, subject_kind, subject_id, …)` serves it.
const groupNotificationsSQL = `
SELECT count(*)
  FROM notifications
 WHERE org_id = $1 AND subject_kind = 'alert_group' AND subject_id = $2
   AND status <> 'suppressed'`

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
	r.readTrail(ctx, s, q.GroupID, &snap)
	r.readCause(ctx, s, q, &snap)
	r.readNotificationCount(ctx, s, q.GroupID, &snap)
	return snap, nil
}

// readCause loads WHO caused the fact being rendered, and what they said.
//
// ⭐ IT READS THE RECORD RATHER THAN CARRYING A COPY, and that is the whole
// choice. The actor and the comment body are already written, exactly once, by
// the module that owns the human verb — in the same transaction that enqueued
// this notification, so they are on disk before any delivery can claim it — and
// `alert_events.actor_label` is denormalised there precisely so a renamed or
// deleted user never rewrites what a card said. Copying either onto the
// notification row would be a second answer to the same question, and two
// answers can disagree.
//
// ⛔ ONE ROUND TRIP, AND ONLY FOR THE REASONS SOMEBODY CAUSES. A reason with no
// entry in `causeEventTypes` returns before touching the database, so the common
// delivery path — fired, repeat, resolved, enriched — pays nothing at all. It is
// not an N+1 either: a snapshot is built once per delivery (C11), and this is one
// indexed `LIMIT 1` beside the round trips already above it.
//
// A failure DEGRADES to no actor, exactly like the trail: a card that cannot
// name the acker is a small loss, and a card that never renders is an alert
// nobody sees.
func (r *SnapshotRepository) readCause(
	ctx context.Context, s db.TenantScope, q domain.SnapshotQuery, snap *domain.Snapshot,
) {
	eventType, ok := causeEventTypes[q.Reason]
	if !ok {
		return
	}

	// The NARROWEST subject the notification names, and every one of these
	// reasons names an EPISODE in production — `alerts` enqueues them from the
	// occurrence it just moved. That matters: a group is many alerts acknowledged
	// one at a time, so a group-scoped read would return whichever member was
	// acted on last and put one person's name against another person's action.
	// The group is the fallback for a preview that names nothing narrower.
	//
	// ⛔ IT NARROWS EXACTLY AS `readOccurrence` DOES, INCLUDING THE ALERT
	// FALLBACK, and the two must not drift. `readOccurrence` answers "which
	// episode is this card about" from the occurrence id and then from the alert
	// id; this answers "who caused that card". A preview by `alert_id` — the
	// policy-preview endpoint takes exactly one of alert/occurrence/group and any
	// reason — would otherwise pair Ada's episode with the whole GROUP's newest
	// acknowledgement, which is grace's, and render "Acknowledged by grace" over
	// Ada's alert. For `comment` it would drag the sibling's WORDS across too.
	sql, subject := causeByGroupSQL, q.GroupID
	switch {
	case q.OccurrenceID != nil:
		sql, subject = causeByOccurrenceSQL, *q.OccurrenceID
	case q.AlertID != nil:
		sql, subject = causeByAlertSQL, *q.AlertID
	}

	var (
		actor domain.ActorFacts
		body  string
	)
	if err := r.db(ctx).QueryRow(ctx, sql, s.OrgID(), subject, eventType).
		Scan(&actor.Kind, &actor.ID, &actor.Label, &body); err != nil {
		return
	}
	snap.Actor = &actor
	snap.Comment = body
}

// readNotificationCount records how many notifications oto has sent about this
// group. It degrades to zero, which the renderer suppresses (S11).
func (r *SnapshotRepository) readNotificationCount(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID, snap *domain.Snapshot,
) {
	var n int
	if err := r.db(ctx).QueryRow(ctx, groupNotificationsSQL, s.OrgID(), groupID).Scan(&n); err == nil {
		snap.NotificationCount = n
	}
}

// readTrail loads the group's state history for the card's receipt (§H.4).
//
// A failure DEGRADES to no trail rather than failing the snapshot. The trail is
// what makes a resolved card legible; a card with no trail is a regression, and a
// card that never renders is an alert nobody sees.
func (r *SnapshotRepository) readTrail(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID, snap *domain.Snapshot,
) {
	rows, err := r.db(ctx).Query(ctx, groupTrailSQL, s.OrgID(), groupID, MaxTrailEntries)
	if err != nil {
		return
	}
	defer rows.Close()

	var out []domain.TransitionFact
	for rows.Next() {
		var f domain.TransitionFact
		if err := rows.Scan(&f.Type, &f.At, &f.ActorLabel); err != nil {
			return
		}
		out = append(out, f)
	}
	if rows.Err() != nil {
		return
	}
	// The query reads newest-first so the LIMIT keeps the RECENT end; the card
	// reads oldest-first because that is the order the story happened in.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	snap.Trail = out
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
		&g.NotificationReason, &g.AlertmanagerURL,
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

	// The upstream start is read separately and DEGRADES to zero rather than
	// failing the snapshot: a card that renders "Started — unknown" is a small
	// loss, and a card that does not render at all because an aggregate went
	// wrong is an alert nobody sees.
	var since *time.Time
	if err := r.db(ctx).QueryRow(ctx, groupFiringSinceSQL, s.OrgID(), groupID).Scan(&since); err == nil &&
		since != nil {
		g.FiringSince = since.UTC()
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
