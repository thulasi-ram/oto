package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// alertRow is the row model of `alerts`. It is UNEXPORTED and never leaves this
// package: the three-model rule (CONTEXT.md §5.5) says a DTO may not embed a row
// and a domain type may not be one. Mapping is explicit, in toDomain.
type alertRow struct {
	id        uuid.UUID
	orgID     uuid.UUID
	clusterID uuid.UUID

	alertKey    string
	fingerprint string
	alertName   string
	severity    *string
	namespace   *string
	service     *string
	clusterKey  string

	labels       []byte
	annotations  []byte
	generatorURL *string

	state               string
	currentOccurrenceID *uuid.UUID
	ackState            string
	snoozedUntil        *time.Time

	firstSeenAt       time.Time
	lastSeenAt        time.Time
	lastStateChangeAt time.Time
	totalOccurrences  int32
	flapScore         float32
	isFlapping        bool
}

// alertColumnList is the projection every alert query selects, in scan order.
// It is a slice because the list query is built with squirrel, which wants the
// columns one by one, and a string because every other query is hand-written
// SQL. Deriving one from the other is what keeps the scan order honest.
var alertColumnList = []string{
	"id", "org_id", "cluster_id", "alert_key", "source_fingerprint", "alertname", "severity",
	"namespace", "service", "cluster_key", "labels", "annotations", "generator_url", "state",
	"current_occurrence_id", "ack_state", "snoozed_until", "first_seen_at", "last_seen_at",
	"last_state_change_at", "total_occurrences", "flap_score", "is_flapping",
}

// alertColumns is alertColumnList rendered for hand-written SQL.
var alertColumns = strings.Join(alertColumnList, ", ")

func (r *alertRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.clusterID, &r.alertKey, &r.fingerprint, &r.alertName, &r.severity,
		&r.namespace, &r.service, &r.clusterKey, &r.labels, &r.annotations, &r.generatorURL,
		&r.state, &r.currentOccurrenceID, &r.ackState, &r.snoozedUntil, &r.firstSeenAt,
		&r.lastSeenAt, &r.lastStateChangeAt, &r.totalOccurrences, &r.flapScore, &r.isFlapping,
	}
}

// toDomain maps one row onto the Alert entity, re-proving every §D.4 invariant
// through the constructor. A row that cannot become an Alert is a mapper bug and
// says so.
func (r *alertRow) toDomain() (domain.Alert, error) {
	labels, err := decodeStringMap(r.labels)
	if err != nil {
		return domain.Alert{}, err
	}
	ls, err := domain.NewLabelSet(labels)
	if err != nil {
		return domain.Alert{}, errs.Internal("alert_labels_invalid", err)
	}
	annotations, err := decodeStringMap(r.annotations)
	if err != nil {
		return domain.Alert{}, err
	}
	ann, err := domain.NewAnnotations(annotations)
	if err != nil {
		return domain.Alert{}, errs.Internal("alert_annotations_invalid", err)
	}
	key, err := domain.NewAlertKey(r.alertKey)
	if err != nil {
		return domain.Alert{}, errs.Internal("alert_key_invalid", err)
	}
	fp, err := domain.NewSourceFingerprint(r.fingerprint)
	if err != nil {
		return domain.Alert{}, errs.Internal("alert_fingerprint_invalid", err)
	}
	ck, err := domain.NewClusterKey(r.clusterKey)
	if err != nil {
		return domain.Alert{}, errs.Internal("alert_cluster_key_invalid", err)
	}
	state, err := domain.NewState(r.state)
	if err != nil {
		return domain.Alert{}, errs.Internal("alert_state_invalid", err)
	}
	ack, err := domain.NewAckState(r.ackState)
	if err != nil {
		return domain.Alert{}, errs.Internal("alert_ack_state_invalid", err)
	}

	a, err := domain.NewAlert(domain.AlertParams{
		ID:                  r.id,
		OrgID:               r.orgID,
		ClusterID:           r.clusterID,
		Key:                 key,
		Fingerprint:         fp,
		ClusterKey:          ck,
		Labels:              ls,
		Annotations:         ann,
		GeneratorURL:        strOrEmpty(r.generatorURL),
		State:               state,
		AckState:            ack,
		CurrentOccurrenceID: idOrNil(r.currentOccurrenceID),
		SnoozedUntil:        timeOrZero(r.snoozedUntil),
		FirstSeenAt:         r.firstSeenAt,
		LastSeenAt:          r.lastSeenAt,
		LastStateChangeAt:   r.lastStateChangeAt,
		TotalOccurrences:    int(r.totalOccurrences),
		FlapScore:           r.flapScore,
		IsFlapping:          r.isFlapping,
	})
	if err != nil {
		return domain.Alert{}, errs.Internal("alert_row_invalid", err)
	}
	return a, nil
}

// AlertRepository is the SQL over `alerts`. It implements
// service.AlertRepository.
//
// Every statement carries an `org_id` predicate. A missing one is not a
// performance bug, it is a data leak, so there is no query in this file that can
// be reached without a db.TenantScope.
type AlertRepository struct {
	q     db.Querier
	clock clock.Clock
	sb    sq.StatementBuilderType
}

// NewAlertRepository builds the repository over a fallback querier, normally the
// general pool. The clock is injected because the snoozed facet of the list is a
// predicate about "now" and must stay testable.
func NewAlertRepository(q db.Querier, clk clock.Clock) *AlertRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &AlertRepository{
		q:     q,
		clock: clk,
		sb:    sq.StatementBuilder.PlaceholderFormat(sq.Dollar),
	}
}

func (r *AlertRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// ------------------------------------------------------------------- upsert

var upsertAlertsSQL = `
INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname, severity,
                    namespace, service, cluster_key, labels, annotations, generator_url,
                    state, first_seen_at, last_seen_at, last_state_change_at)
SELECT u.id, $1, u.cluster_id, u.alert_key, u.fingerprint, u.alertname, u.severity,
       u.namespace, u.service, u.cluster_key, u.labels, u.annotations, u.generator_url,
       u.state, u.seen_at, u.seen_at, u.seen_at
  FROM unnest($2::uuid[], $3::uuid[], $4::text[], $5::text[], $6::text[], $7::text[],
              $8::text[], $9::text[], $10::text[], $11::jsonb[], $12::jsonb[], $13::text[],
              $14::text[], $15::timestamptz[])
    AS u(id, cluster_id, alert_key, fingerprint, alertname, severity, namespace, service,
         cluster_key, labels, annotations, generator_url, state, seen_at)
ON CONFLICT (org_id, alert_key) DO UPDATE SET
    last_seen_at       = GREATEST(alerts.last_seen_at, EXCLUDED.last_seen_at),
    labels             = EXCLUDED.labels,
    annotations        = EXCLUDED.annotations,
    severity           = EXCLUDED.severity,
    namespace          = EXCLUDED.namespace,
    service            = EXCLUDED.service,
    generator_url      = EXCLUDED.generator_url,
    source_fingerprint = EXCLUDED.source_fingerprint,
    updated_at         = now()
RETURNING ` + alertColumns + `, (xmax = 0) AS was_inserted`

// UpsertBatch writes one webhook's worth of alerts in ONE round trip (§D.12c).
//
// Dedup is enforced by alerts_key_uniq, never by a read-then-write check (C.2),
// and `was_inserted` comes from `(xmax = 0)` so the caller learns whether to emit
// `alert.created` without a prior SELECT.
//
// Two observations in the same batch may carry the same alert_key — Alertmanager
// sends `firing` and `resolved` for one label set in one payload. `ON CONFLICT DO
// UPDATE` cannot touch the same row twice in one statement, so the input is
// collapsed by key first, keeping the LAST occurrence (the most recently seen).
// The returned slice is index-aligned with `in`, and WasInserted is true only on
// the first index of each key.
func (r *AlertRepository) UpsertBatch(
	ctx context.Context, s db.TenantScope, in []domain.AlertUpsert,
) ([]domain.AlertUpsertResult, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	if len(in) == 0 {
		return nil, nil
	}

	// Collapse duplicates, remembering the first index each key appeared at.
	order := make([]string, 0, len(in))
	winner := make(map[string]domain.AlertUpsert, len(in))
	firstIdx := make(map[string]int, len(in))
	for i, u := range in {
		k := u.AlertKey.String()
		if _, seen := winner[k]; !seen {
			order = append(order, k)
			firstIdx[k] = i
		}
		winner[k] = u
	}

	n := len(order)
	ids := make([]uuid.UUID, n)
	clusterIDs := make([]uuid.UUID, n)
	keys := make([]string, n)
	fps := make([]string, n)
	names := make([]string, n)
	sevs := make([]*string, n)
	namespaces := make([]*string, n)
	services := make([]*string, n)
	clusterKeys := make([]string, n)
	labels := make([][]byte, n)
	annotations := make([][]byte, n)
	urls := make([]*string, n)
	states := make([]string, n)
	seenAt := make([]time.Time, n)

	for i, k := range order {
		u := winner[k]
		if err := requireID("alert id", u.ID); err != nil {
			return nil, err
		}
		if err := requireID("cluster_id", u.ClusterID); err != nil {
			return nil, err
		}
		if u.AlertKey.IsZero() || u.Fingerprint.IsZero() || u.ClusterKey.IsZero() {
			return nil, errs.Internal("alert_upsert_incomplete",
				errsMissing("alert_key, source_fingerprint and cluster_key are required"))
		}
		if u.Labels.IsZero() {
			return nil, errs.Internal("alert_upsert_incomplete", errsMissing("labels are required"))
		}
		if !u.State.IsOpen() && !u.State.IsTerminal() {
			return nil, errs.Internal("alert_upsert_incomplete", errsMissing("state is required"))
		}
		if u.SeenAt.IsZero() {
			return nil, errs.Internal("alert_upsert_incomplete", errsMissing("seen_at is required"))
		}

		lb, err := jsonbMap(u.Labels.Map())
		if err != nil {
			return nil, err
		}
		an, err := jsonbMap(u.Annotations)
		if err != nil {
			return nil, err
		}

		ids[i] = u.ID
		clusterIDs[i] = u.ClusterID
		keys[i] = u.AlertKey.String()
		fps[i] = u.Fingerprint.String()
		names[i] = u.AlertName
		sevs[i] = u.Severity
		namespaces[i] = nilIfEmpty(u.Namespace)
		services[i] = nilIfEmpty(u.Service)
		clusterKeys[i] = u.ClusterKey.String()
		labels[i] = lb
		annotations[i] = an
		urls[i] = nilIfEmpty(u.GeneratorURL)
		states[i] = u.State.String()
		seenAt[i] = u.SeenAt.UTC()
	}

	rows, err := r.db(ctx).Query(ctx, upsertAlertsSQL, s.OrgID(), ids, clusterIDs, keys, fps, names,
		sevs, namespaces, services, clusterKeys, labels, annotations, urls, states, seenAt)
	if err != nil {
		return nil, mapErr(err, "upsert alerts")
	}
	defer rows.Close()

	byKey := make(map[string]domain.AlertUpsertResult, n)
	for rows.Next() {
		var row alertRow
		var inserted bool
		dest := append(row.scanDest(), &inserted)
		if err := rows.Scan(dest...); err != nil {
			return nil, mapErr(err, "scan upserted alert")
		}
		a, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		byKey[row.alertKey] = domain.AlertUpsertResult{Alert: a, WasInserted: inserted}
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read upserted alerts")
	}

	out := make([]domain.AlertUpsertResult, len(in))
	for i, u := range in {
		k := u.AlertKey.String()
		res, ok := byKey[k]
		if !ok {
			return nil, errs.Internal("alert_upsert_missing_row",
				errsMissing("the upsert returned no row for "+k))
		}
		// Only the first index of a key may claim the insert; the later ones
		// describe the same row and must not re-emit `alert.created`.
		if firstIdx[k] != i {
			res.WasInserted = false
		}
		out[i] = res
	}
	return out, nil
}

// ------------------------------------------------------------------ reads

// GetByID reads one Alert within the caller's org.
func (r *AlertRepository) GetByID(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Alert, error) {
	if err := requireScope(s); err != nil {
		return domain.Alert{}, err
	}
	return r.getOne(ctx, `SELECT `+alertColumns+` FROM alerts WHERE org_id = $1 AND id = $2`,
		s.OrgID(), id)
}

// GetByAlertKey reads one Alert by its §C.2 identity key.
func (r *AlertRepository) GetByAlertKey(ctx context.Context, s db.TenantScope, alertKey string) (domain.Alert, error) {
	if err := requireScope(s); err != nil {
		return domain.Alert{}, err
	}
	return r.getOne(ctx, `SELECT `+alertColumns+` FROM alerts WHERE org_id = $1 AND alert_key = $2`,
		s.OrgID(), alertKey)
}

// GetByAlertKeys reads many Alerts by identity key in one round trip.
//
// It exists for the T2 material-change probe, which must see an Alert as it was
// BEFORE the §D.12(c) upsert overwrote its annotations and severity. One extra
// round trip per batch, not one per alert.
func (r *AlertRepository) GetByAlertKeys(
	ctx context.Context, s db.TenantScope, alertKeys []string,
) (map[string]domain.Alert, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	if len(alertKeys) == 0 {
		return map[string]domain.Alert{}, nil
	}

	rows, err := r.db(ctx).Query(ctx,
		`SELECT `+alertColumns+` FROM alerts WHERE org_id = $1 AND alert_key = ANY($2)`,
		s.OrgID(), alertKeys)
	if err != nil {
		return nil, mapErr(err, "read alerts by key")
	}
	defer rows.Close()

	out := make(map[string]domain.Alert, len(alertKeys))
	for rows.Next() {
		var row alertRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "scan alert")
		}
		a, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out[row.alertKey] = a
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read alerts by key")
	}
	return out, nil
}

func (r *AlertRepository) getOne(ctx context.Context, sql string, args ...any) (domain.Alert, error) {
	var row alertRow
	if err := r.db(ctx).QueryRow(ctx, sql, args...).Scan(row.scanDest()...); err != nil {
		if isNoRows(err) {
			return domain.Alert{}, errs.NotFound("alert_not_found", "no such alert")
		}
		return domain.Alert{}, mapErr(err, "read alert")
	}
	return row.toDomain()
}

// List is the §D.12(a) hot query: the alert list, keyset-paginated, never
// OFFSET. It sorts by `-last_seen_at`, which is what alerts_list_idx and
// alerts_open_idx are built for.
func (r *AlertRepository) List(
	ctx context.Context, s db.TenantScope, f domain.AlertFilter, p db.Keyset,
) ([]domain.Alert, db.Cursor, error) {
	return r.ListSorted(ctx, s, f, sortLastSeenDesc, p)
}

// ListSorted is List with the §E.3 `sort` parameter made explicit.
// domain.AlertFilter carries no sort field (§F.5.2) while §E.3 exposes two keys,
// so the sort travels as its own argument rather than being invented onto a
// contract type this package does not own.
//
// `-last_seen_at` is served by alerts_list_idx / alerts_open_idx.
// `-first_seen_at` has NO covering index and is a filter-then-sort; it is the
// rarer of the two and adding an index is a migration, which this module does
// not own.
func (r *AlertRepository) ListSorted(
	ctx context.Context, s db.TenantScope, f domain.AlertFilter, sort string, p db.Keyset,
) ([]domain.Alert, db.Cursor, error) {
	if err := requireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}

	sortCol := "last_seen_at"
	switch sort {
	case "", sortLastSeenDesc:
	case sortFirstSeenDesc:
		sortCol = "first_seen_at"
	default:
		return nil, db.Cursor{}, errs.Validation("sort_invalid",
			"sort must be one of: -last_seen_at, -first_seen_at")
	}

	limit := clampLimit(p.Limit)

	q := r.sb.Select(alertColumnList...).
		From("alerts").
		Where(sq.Eq{"org_id": s.OrgID()})

	q, err := applyAlertFilter(q, f, r.clock.Now())
	if err != nil {
		return nil, db.Cursor{}, err
	}

	if !p.Cursor.IsZero() {
		q = q.Where(sq.Expr("("+sortCol+", id) < (?, ?)", p.Cursor.SortKey.UTC(), p.Cursor.ID))
	}

	sql, args, err := q.
		OrderBy(sortCol+" DESC", "id DESC").
		Limit(uint64(limit) + 1). //nolint:gosec // limit is clamped to [1,200]
		ToSql()
	if err != nil {
		return nil, db.Cursor{}, errs.Internal("alert_list_build_failed", err)
	}

	rows, err := r.db(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "list alerts")
	}
	defer rows.Close()

	type entry struct {
		alert domain.Alert
		sort  time.Time
	}
	collected := make([]entry, 0, limit+1)
	for rows.Next() {
		var row alertRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, db.Cursor{}, mapErr(err, "scan alert")
		}
		a, err := row.toDomain()
		if err != nil {
			return nil, db.Cursor{}, err
		}
		key := row.lastSeenAt
		if sortCol == "first_seen_at" {
			key = row.firstSeenAt
		}
		collected = append(collected, entry{alert: a, sort: key})
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "read alerts")
	}

	page, hasMore := pageOf(collected, limit)
	out := make([]domain.Alert, len(page))
	for i, e := range page {
		out[i] = e.alert
	}
	var cur db.Cursor
	if len(page) > 0 {
		last := page[len(page)-1]
		cur = nextCursor(last.sort, last.alert.ID(), f.FilterHash, hasMore)
	} else {
		cur = db.Cursor{Hash: f.FilterHash}
	}
	return out, cur, nil
}

// applyAlertFilter compiles §E.3 onto the query. Every dimension is optional and
// a nil or empty value means "no constraint", which is what makes the default
// list the whole open world rather than an accidental subset.
func applyAlertFilter(q sq.SelectBuilder, f domain.AlertFilter, now time.Time) (sq.SelectBuilder, error) {
	if len(f.States) > 0 {
		states := make([]string, len(f.States))
		for i, st := range f.States {
			states[i] = st.String()
		}
		q = q.Where(sq.Eq{"state": states})
	}
	if len(f.Severities) > 0 {
		q = q.Where(sq.Eq{"severity": f.Severities})
	}
	if len(f.Namespaces) > 0 {
		q = q.Where(sq.Eq{"namespace": f.Namespaces})
	}
	if len(f.ClusterKeys) > 0 {
		q = q.Where(sq.Eq{"cluster_key": f.ClusterKeys})
	}
	if len(f.Services) > 0 {
		q = q.Where(sq.Eq{"service": f.Services})
	}
	if len(f.AlertNames) > 0 {
		q = q.Where(sq.Eq{"alertname": f.AlertNames})
	}
	if f.AckState != nil {
		q = q.Where(sq.Eq{"ack_state": f.AckState.String()})
	}
	if f.Flapping != nil {
		q = q.Where(sq.Eq{"is_flapping": *f.Flapping})
	}
	// §B.8.6: nil means INCLUDE BOTH, and nil is the default. Hiding snoozed
	// alerts from the default list is how an incident is lost.
	if f.Snoozed != nil {
		if *f.Snoozed {
			q = q.Where(sq.Expr("snoozed_until IS NOT NULL AND snoozed_until > ?", now.UTC()))
		} else {
			q = q.Where(sq.Expr("(snoozed_until IS NULL OR snoozed_until <= ?)", now.UTC()))
		}
	}
	if len(f.LabelsAll) > 0 {
		b, err := jsonbMap(f.LabelsAll)
		if err != nil {
			return q, err
		}
		q = q.Where(sq.Expr("labels @> ?::jsonb", b))
	}
	for _, name := range sortedKeys(f.LabelsAny) {
		values := f.LabelsAny[name]
		if len(values) == 0 {
			continue
		}
		or := make(sq.Or, 0, len(values))
		for _, v := range values {
			b, err := jsonbMap(map[string]string{name: v})
			if err != nil {
				return q, err
			}
			or = append(or, sq.Expr("labels @> ?::jsonb", b))
		}
		q = q.Where(or)
	}
	// `NOT (a OR b)` is `NOT a AND NOT b`, so a multi-valued negation becomes one
	// negated containment per value. NOT-containment has no index (measured:
	// Parallel Seq Scan), so these are Filter predicates riding whichever
	// positive containment above drives the plan.
	for _, name := range sortedKeys(f.LabelsNone) {
		for _, v := range f.LabelsNone[name] {
			b, err := jsonbMap(map[string]string{name: v})
			if err != nil {
				return q, err
			}
			q = q.Where(sq.Expr("NOT (labels @> ?::jsonb)", b))
		}
	}
	if f.Since != nil {
		q = q.Where(sq.GtOrEq{"last_seen_at": f.Since.UTC()})
	}
	if strings.TrimSpace(f.Query) != "" {
		// Mirrors alerts_text_idx exactly; any deviation makes the index unusable.
		q = q.Where(sq.Expr(
			`to_tsvector('simple', alertname || ' ' || coalesce(annotations->>'summary','')
			                                 || ' ' || coalesce(annotations->>'description',''))
			 @@ plainto_tsquery('simple', ?)`, f.Query))
	}
	return q, nil
}

// ------------------------------------------------------------------ roll-ups

// rollupKeyExpr maps a §E.3a axis onto the SQL that buckets by it.
//
// Every expression is COALESCEd to a non-NULL text, because the bucket key is
// also the keyset position: a NULL key would make `key > $cursor` unsatisfiable
// and silently truncate the page at the first alert with no namespace.
//
// ⛔ The map is exhaustive over domain.RollupKey and there is no default arm.
// An axis that is not here is one no index can serve, and the domain constructor
// refuses it before this function is ever reached.
var rollupKeyExpr = map[string]string{
	"alertname":   "alertname",
	"namespace":   "COALESCE(namespace, '')",
	"fingerprint": "source_fingerprint",
}

// Rollup is §E.3a: the alert list aggregated onto one axis.
//
// ⭐ It is a SERVER-SIDE aggregate over the WHOLE filtered set, which is the
// entire point. A client rolling up the rows it happens to have loaded produces
// counts that are quietly wrong the moment the result exceeds one page, and a
// quietly wrong count during an incident is worse than no count at all.
//
// ⛔ A roll-up bucket is a VIEW and is NEVER an AlertGroup: no row, no
// generation, no chat thread (§A.1).
//
// ⭐ IT IS ONE PASS OVER THE BASE TABLE, and the shape is load-bearing. The
// obvious spelling — a CTE holding the filtered rows, plus a correlated subquery
// per bucket for the severity breakdown — makes Postgres MATERIALISE the CTE and
// rescan it once per bucket: measured, 280 ms and a Seq Scan where this shape is
// 23 ms. Aggregating by `(bucket, severity)` and re-aggregating to `bucket` gets
// the same answer from a single grouped read.
//
// NOTE (planner), measured on 60 000 alerts:
//
//   - The keyset predicate is pushed onto the BASE TABLE, not applied after
//     grouping, so `alertname > $after` rides alerts_name_idx
//     (org_id, alertname, …) as a Bitmap Index Scan — paging deeper gets
//     cheaper rather than costing the same every time.
//   - An UNFILTERED first page is a Parallel Seq Scan, and that is correct
//     rather than a missing index: a complete count over every alert in an org
//     has to read every alert in that org. No index removes that, and reporting
//     a count that had not read them all is the failure this endpoint exists to
//     fix.
//   - `namespace` and `fingerprint` have no (org_id, <key>) index, so their
//     keyset predicate is a filter rather than a range. They are still one
//     grouped pass; the DDL that would make them index ranges is reported
//     rather than written, because migrations are not owned here.
func (r *AlertRepository) Rollup(
	ctx context.Context, s db.TenantScope, f domain.AlertFilter, key domain.RollupKey,
	after string, limit int,
) ([]domain.AlertRollup, bool, error) {
	if err := requireScope(s); err != nil {
		return nil, false, err
	}
	expr, ok := rollupKeyExpr[key.String()]
	if !ok {
		return nil, false, errs.Validation("group_by_invalid",
			"group_by must be one of: alertname, namespace, fingerprint")
	}
	n := clampLimit(limit)
	now := r.clock.Now().UTC()

	// The inner SELECT carries the ORDINARY alert-list filter, unchanged and
	// shared: a roll-up that honoured a different set of filters from the list it
	// summarises would be two answers to one question.
	//
	// `severity` is the RAW label and is grouped on, never ranked — operators
	// choose their own vocabulary (§L.4.2), so precedence is the client's.
	inner := r.sb.
		Select(expr+" AS bucket").
		Column("COALESCE(severity, '') AS sev").
		Column("count(*) AS n").
		Column("count(*) FILTER (WHERE state = 'firing') AS firing").
		Column("count(*) FILTER (WHERE state = 'suppressed') AS suppressed").
		Column("count(*) FILTER (WHERE state = 'resolved') AS resolved").
		Column("count(*) FILTER (WHERE state = 'expired') AS expired").
		Column("count(*) FILTER (WHERE ack_state = 'acked') AS acked").
		Column("count(*) FILTER (WHERE is_flapping) AS flapping").
		Column("count(*) FILTER (WHERE snoozed_until > ?) AS snoozed", now).
		Column("min(first_seen_at) AS first_seen").
		Column("max(last_seen_at) AS last_seen").
		From("alerts").
		Where(sq.Eq{"org_id": s.OrgID()})

	inner, err := applyAlertFilter(inner, f, now)
	if err != nil {
		return nil, false, err
	}
	if after != "" {
		inner = inner.Where(sq.Expr(expr+" > ?", after))
	}

	innerSQL, args, err := inner.GroupBy("1", "2").ToSql()
	if err != nil {
		return nil, false, errs.Internal("alert_rollup_build_failed", err)
	}
	args = append(args, n+1)

	sql := fmt.Sprintf(`
SELECT bucket,
       sum(n)::bigint          AS total,
       sum(firing)::bigint     AS firing,
       sum(suppressed)::bigint AS suppressed,
       sum(resolved)::bigint   AS resolved,
       sum(expired)::bigint    AS expired,
       sum(acked)::bigint      AS acked,
       sum(flapping)::bigint   AS flapping,
       sum(snoozed)::bigint    AS snoozed,
       min(first_seen)         AS first_seen_at,
       max(last_seen)          AS last_seen_at,
       COALESCE(jsonb_object_agg(sev, n) FILTER (WHERE sev <> ''), '{}'::jsonb) AS severity_counts
  FROM (%s) agg
 GROUP BY bucket
 ORDER BY bucket ASC
 LIMIT $%d`, innerSQL, len(args))

	rows, err := r.db(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, false, mapErr(err, "roll up alerts")
	}
	defer rows.Close()

	out := make([]domain.AlertRollup, 0, n+1)
	for rows.Next() {
		var (
			b   domain.AlertRollup
			sev []byte
		)
		if err := rows.Scan(&b.Key, &b.Total, &b.Firing, &b.Suppressed, &b.Resolved,
			&b.Expired, &b.Acked, &b.Flapping, &b.Snoozed,
			&b.FirstSeenAt, &b.LastSeenAt, &sev); err != nil {
			return nil, false, mapErr(err, "scan alert rollup")
		}
		counts, err := decodeIntMap(sev)
		if err != nil {
			return nil, false, err
		}
		b.SeverityCounts = counts
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, false, mapErr(err, "read alert rollups")
	}

	page, hasMore := pageOf(out, n)
	return page, hasMore, nil
}

// ------------------------------------------------------------------ writes

const setProjectionSQL = `
UPDATE alerts SET
    state                 = $3,
    current_occurrence_id = $4,
    ack_state             = $5,
    snoozed_until         = $6,
    last_seen_at          = GREATEST(last_seen_at, $7),
    last_state_change_at  = GREATEST(first_seen_at, $8),
    total_occurrences     = $9,
    updated_at            = now()
WHERE org_id = $1 AND id = $2`

// SetProjection writes the denormalised current-state summary onto `alerts`.
//
// It runs in the SAME transaction as the occurrence transition that caused it —
// current state is a projection and `alert_events` is the truth, so the two must
// never be observable apart. The GREATEST guards mirror alerts_seen_ck and
// alerts_change_ck so that a skewed clock produces a clamped row rather than a
// 23514 in the middle of an ingest batch.
func (r *AlertRepository) SetProjection(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, p domain.AlertProjection,
) error {
	if err := requireScope(s); err != nil {
		return err
	}
	if err := requireID("alert_id", alertID); err != nil {
		return err
	}
	if !p.State.IsOpen() && !p.State.IsTerminal() {
		return errs.Internal("alert_projection_invalid", errsMissing("state is required"))
	}
	ack := p.AckState
	if ack.IsZero() {
		ack = domain.AckStateUnacked
	}
	if p.TotalOccurrences < 0 {
		return errs.Internal("alert_projection_invalid", errsMissing("total_occurrences must be >= 0"))
	}

	tag, err := r.db(ctx).Exec(ctx, setProjectionSQL, s.OrgID(), alertID,
		p.State.String(), p.CurrentOccurrenceID, ack.String(), p.SnoozedUntil,
		p.LastSeenAt.UTC(), p.LastStateChangeAt.UTC(), p.TotalOccurrences)
	if err != nil {
		return mapErr(err, "write alert projection")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("alert_not_found", "no such alert")
	}
	return nil
}

// SetFlap records a recomputed flap score. Flapping is a VISIBLE UI state, never
// silent suppression (§B.6), which is why it is written to the row the list
// renders from.
func (r *AlertRepository) SetFlap(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, score float32, flapping bool,
) error {
	if err := requireScope(s); err != nil {
		return err
	}
	if err := requireID("alert_id", alertID); err != nil {
		return err
	}
	if score < 0 {
		return errs.Internal("alert_flap_invalid", errsMissing("flap_score must be >= 0"))
	}
	tag, err := r.db(ctx).Exec(ctx,
		`UPDATE alerts SET flap_score = $3, is_flapping = $4, updated_at = now()
		  WHERE org_id = $1 AND id = $2`,
		s.OrgID(), alertID, score, flapping)
	if err != nil {
		return mapErr(err, "write flap score")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("alert_not_found", "no such alert")
	}
	return nil
}

// SetSnoozedUntil writes ONLY the §B.8 projection, leaving state, ack_state and
// severity untouched — the three axes are independent (§B.1) and a snooze that
// moved any of them would be the lie §B.8.1 forbids.
func (r *AlertRepository) SetSnoozedUntil(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, until *time.Time,
) error {
	if err := requireScope(s); err != nil {
		return err
	}
	if err := requireID("alert_id", alertID); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx,
		`UPDATE alerts SET snoozed_until = $3, updated_at = now() WHERE org_id = $1 AND id = $2`,
		s.OrgID(), alertID, until)
	if err != nil {
		return mapErr(err, "write snooze projection")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("alert_not_found", "no such alert")
	}
	return nil
}

// ------------------------------------------------------------------ discovery

const distinctLabelNamesSQL = `
SELECT k, count(*) AS n
  FROM alerts a, LATERAL jsonb_object_keys(a.labels) AS k
 WHERE a.org_id = $1 AND ($2 = '' OR k LIKE $2 || '%')
 GROUP BY k
 ORDER BY n DESC, k ASC
 LIMIT $3`

// DistinctLabelNames feeds the filter bar's label typeahead, WITH the count of
// alerts carrying each name.
//
// ⭐ The count is what the contract has always called `alert_count`, and it is
// not decoration: a typeahead that offers a label matching nothing spends the
// one minute of an incident that matters most. Ordering by it puts the useful
// labels first, with the name as a deterministic tiebreak.
//
// NOTE (planner): no index serves this. alerts_labels_gin is jsonb_path_ops,
// which supports containment only; key ENUMERATION is a scan of the org's
// alerts. Aggregating adds no scan — the rows were already being read to be
// DISTINCTed. It is a discovery endpoint, not a hot path, and it is bounded by
// `limit`; an expression index over jsonb_object_keys would be a migration this
// module does not own.
func (r *AlertRepository) DistinctLabelNames(
	ctx context.Context, s db.TenantScope, prefix string, limit int,
) ([]domain.LabelCount, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, distinctLabelNamesSQL, s.OrgID(), prefix, clampLimit(limit))
	if err != nil {
		return nil, mapErr(err, "list label names")
	}
	defer rows.Close()
	return collectLabelCounts(rows, "label name")
}

const distinctLabelValuesSQL = `
SELECT a.labels ->> $2 AS v, count(*) AS n
  FROM alerts a
 WHERE a.org_id = $1
   AND a.labels ->> $2 IS NOT NULL
   AND ($3 = '' OR a.labels ->> $2 LIKE $3 || '%')
 GROUP BY 1
 ORDER BY n DESC, v ASC
 LIMIT $4`

// DistinctLabelValues feeds the value typeahead for one label name, with the
// same per-value alert count.
//
// NOTE (planner): same as DistinctLabelNames — a bounded scan, not an index
// lookup.
func (r *AlertRepository) DistinctLabelValues(
	ctx context.Context, s db.TenantScope, name, prefix string, limit int,
) ([]domain.LabelCount, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, errs.Validation("label_name_required", "a label name is required")
	}
	rows, err := r.db(ctx).Query(ctx, distinctLabelValuesSQL, s.OrgID(), name, prefix, clampLimit(limit))
	if err != nil {
		return nil, mapErr(err, "list label values")
	}
	defer rows.Close()
	return collectLabelCounts(rows, "label value")
}

func collectLabelCounts(rows pgx.Rows, what string) ([]domain.LabelCount, error) {
	out := make([]domain.LabelCount, 0, 32)
	for rows.Next() {
		var v domain.LabelCount
		if err := rows.Scan(&v.Value, &v.Count); err != nil {
			return nil, mapErr(err, "scan "+what)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read "+what+"s")
	}
	return out, nil
}
