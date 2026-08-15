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
	synthetic         bool
}

// alertColumnList is the projection every alert query selects, in scan order.
// It is a slice because the list query is built with squirrel, which wants the
// columns one by one, and a string because every other query is hand-written
// SQL. Deriving one from the other is what keeps the scan order honest.
var alertColumnList = []string{
	"id", "org_id", "cluster_id", "alert_key", "source_fingerprint", "alertname", "severity",
	"namespace", "service", "cluster_key", "labels", "annotations", "generator_url", "state",
	"current_occurrence_id", "ack_state", "snoozed_until", "first_seen_at", "last_seen_at",
	"last_state_change_at", "total_occurrences", "flap_score", "is_flapping", "synthetic",
}

// alertColumns is alertColumnList rendered for hand-written SQL.
var alertColumns = strings.Join(alertColumnList, ", ")

func (r *alertRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.clusterID, &r.alertKey, &r.fingerprint, &r.alertName, &r.severity,
		&r.namespace, &r.service, &r.clusterKey, &r.labels, &r.annotations, &r.generatorURL,
		&r.state, &r.currentOccurrenceID, &r.ackState, &r.snoozedUntil, &r.firstSeenAt,
		&r.lastSeenAt, &r.lastStateChangeAt, &r.totalOccurrences, &r.flapScore, &r.isFlapping,
		&r.synthetic,
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
		Synthetic:           r.synthetic,
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

// ⭐ THE UPSERT IS ALSO THE ONLY WRITER OF THE LABEL PROJECTION, which is why it
// grew a tail. `alerts.labels` is written here and nowhere else in this tree —
// the three other UPDATEs on `alerts` touch flap_score, snoozed_until and the
// occurrence projection — so 00045's `alert_labels` and `alert_label_names` are
// maintained in this statement, inside the same transaction, while
// `Service.observe` holds the alert's row lock. A projection maintained anywhere
// else is a projection that can be observed disagreeing with its source.
//
// It stays ONE round trip (§D.12c): the upsert becomes a data-modifying CTE and
// the maintenance hangs off its RETURNING.
//
//	want   the label set each upserted alert SHOULD have, synthetics excluded
//	gone   the alert_labels rows it no longer has  → −1 to that name's count
//	put    the rows it does, upserted by value     → +1 only where a row is NEW
//	delta  the two folded together, per name, zero-sum entries dropped
//	bump   the delta applied to names already counted
//	mint   the delta applied to names that were not
//
// ⛔ `gone`/`put` REPLACE ONE ALERT'S SET RATHER THAN APPLYING A DIFFERENCE, and
// that is what makes them safe: a replacement does not need to know the old
// labels, so it cannot be wrong about them. The COUNT cannot be replaced that
// way — it is shared between alerts — so it takes a delta, and the delta is read
// out of the RETURNING of those two statements rather than out of a re-read of
// `alerts`. Both RETURNINGs are produced under row locks, and READ COMMITTED
// re-reads a locked row before applying, so the arithmetic is exact under
// concurrency rather than nearly exact.
//
// ⚠️ A NO-OP OBSERVATION WRITES NOTHING, which is the whole reason this is
// affordable on the ingest path: `put`'s `WHERE label_value IS DISTINCT FROM`
// declines the update, `gone` deletes nothing, `delta` is empty, and the cost is
// N × L index probes with no dead tuples. That is the overwhelmingly common
// case, since a re-observation almost never changes a label set.
//
// ⚠️ `mint` IS SPLIT FROM `bump` FOR A REASON THAT LOOKS LIKE A TYPO. Postgres
// evaluates CHECK constraints on the proposed tuple BEFORE ON CONFLICT
// arbitration, so a single `INSERT … ON CONFLICT DO UPDATE SET count = count +
// EXCLUDED.count` carrying a −1 fails `alert_label_names_count_ck` on a row that
// was only ever going to be updated. `bump` therefore takes every name that
// already has a row, and `mint` inserts only the strictly positive remainder —
// which is every name it can be, because a −1 exists only where a row was
// deleted, and a row exists only where it was once counted.
var upsertAlertsSQL = `
WITH up AS (
INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname, severity,
                    namespace, service, cluster_key, labels, annotations, generator_url,
                    state, first_seen_at, last_seen_at, last_state_change_at, synthetic)
SELECT u.id, $1, u.cluster_id, u.alert_key, u.fingerprint, u.alertname, u.severity,
       u.namespace, u.service, u.cluster_key, u.labels, u.annotations, u.generator_url,
       u.state, u.seen_at, u.seen_at, u.seen_at, u.synthetic
  FROM unnest($2::uuid[], $3::uuid[], $4::text[], $5::text[], $6::text[], $7::text[],
              $8::text[], $9::text[], $10::text[], $11::jsonb[], $12::jsonb[], $13::text[],
              $14::text[], $15::timestamptz[], $16::boolean[])
    AS u(id, cluster_id, alert_key, fingerprint, alertname, severity, namespace, service,
         cluster_key, labels, annotations, generator_url, state, seen_at, synthetic)
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
RETURNING ` + alertColumns + `, (xmax = 0) AS was_inserted
),
want AS (
    SELECT u.id AS alert_id, e.key AS label_name, coalesce(e.value, '') AS label_value
      FROM up u, LATERAL jsonb_each_text(u.labels) AS e(key, value)
     WHERE NOT u.synthetic
),
gone AS (
    DELETE FROM alert_labels l
     USING up u
     WHERE l.org_id = $1 AND l.alert_id = u.id
       AND NOT EXISTS (SELECT 1 FROM want w
                        WHERE w.alert_id = l.alert_id AND w.label_name = l.label_name)
    RETURNING l.label_name
),
put AS (
    INSERT INTO alert_labels (org_id, alert_id, label_name, label_value)
    SELECT $1, w.alert_id, w.label_name, w.label_value FROM want w
        ON CONFLICT ON CONSTRAINT alert_labels_pk DO UPDATE
       SET label_value = EXCLUDED.label_value
     WHERE alert_labels.label_value IS DISTINCT FROM EXCLUDED.label_value
    RETURNING label_name, (xmax = 0) AS was_inserted
),
delta AS (
    SELECT label_name, sum(d) AS d
      FROM (SELECT label_name, -1 AS d FROM gone
             UNION ALL
            SELECT label_name,  1 AS d FROM put WHERE was_inserted) x
     GROUP BY label_name
    HAVING sum(d) <> 0
),
bump AS (
    UPDATE alert_label_names n
       SET alert_count = n.alert_count + d.d
      FROM delta d
     WHERE n.org_id = $1 AND n.label_name = d.label_name
    RETURNING n.label_name
),
mint AS (
    INSERT INTO alert_label_names (org_id, label_name, alert_count)
    SELECT $1, d.label_name, d.d FROM delta d
     WHERE d.d > 0 AND NOT EXISTS (SELECT 1 FROM bump b WHERE b.label_name = d.label_name)
        ON CONFLICT ON CONSTRAINT alert_label_names_pk DO UPDATE
       SET alert_count = alert_label_names.alert_count + EXCLUDED.alert_count
)
SELECT ` + alertColumns + `, was_inserted FROM up`

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
	if err := db.RequireScope(s); err != nil {
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
	synthetic := make([]bool, n)

	for i, k := range order {
		u := winner[k]
		if err := db.RequireID("alert id", u.ID); err != nil {
			return nil, err
		}
		if err := db.RequireID("cluster_id", u.ClusterID); err != nil {
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
		synthetic[i] = u.Synthetic
	}

	rows, err := r.db(ctx).Query(ctx, upsertAlertsSQL, s.OrgID(), ids, clusterIDs, keys, fps, names,
		sevs, namespaces, services, clusterKeys, labels, annotations, urls, states, seenAt,
		synthetic)
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
	if err := db.RequireScope(s); err != nil {
		return domain.Alert{}, err
	}
	return r.getOne(ctx, `SELECT `+alertColumns+` FROM alerts WHERE org_id = $1 AND id = $2`,
		s.OrgID(), id)
}

// GetByAlertKey reads one Alert by its §C.2 identity key.
func (r *AlertRepository) GetByAlertKey(ctx context.Context, s db.TenantScope, alertKey string) (domain.Alert, error) {
	if err := db.RequireScope(s); err != nil {
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
	if err := db.RequireScope(s); err != nil {
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
	if err := db.RequireScope(s); err != nil {
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

	limit := db.ClampLimit(p.Limit)

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

	page, hasMore := db.PageOf(collected, limit)
	out := make([]domain.Alert, len(page))
	for i, e := range page {
		out[i] = e.alert
	}
	var cur db.Cursor
	if len(page) > 0 {
		last := page[len(page)-1]
		cur = db.NextCursor(last.sort, last.alert.ID(), f.FilterHash, hasMore)
	} else {
		cur = db.Cursor{Hash: f.FilterHash}
	}
	return out, cur, nil
}

// applyAlertFilter compiles §E.3 onto the query. Every dimension is optional and
// a nil or empty value means "no constraint", which is what makes the default
// list the whole open world rather than an accidental subset.
func applyAlertFilter(q sq.SelectBuilder, f domain.AlertFilter, now time.Time) (sq.SelectBuilder, error) {
	// ⭐⭐ SYNTHETICS ARE EXCLUDED BY DEFAULT, and this is the OPPOSITE default
	// from `Snoozed` two dimensions down. The reason is that they answer opposite
	// questions. A snoozed alert is a real thing happening in a real cluster and
	// hiding it is how an incident is lost (§B.8.6). A synthetic alert is
	// something oto manufactured for a delivery drill; nothing in the cluster
	// fired, and counting it as history would make the product lie about the
	// customer's estate. `?synthetic=true` is the explicit, visible way to see
	// one — normally reached from a drill's own result screen, never a chip an
	// operator is expected to know about.
	//
	// ⛔ This is ONE of the reads that had to change. The complete list is on the
	// `alerts.synthetic` column comment in 00039_delivery_drills.sql.
	if f.Synthetic == nil {
		q = q.Where(sq.Expr("NOT synthetic"))
	} else {
		q = q.Where(sq.Eq{"synthetic": *f.Synthetic})
	}
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
	// The §C.3 fingerprint, which is what `group_by=fingerprint` buckets on.
	// alerts_srcfp_idx is (org_id, cluster_key, source_fingerprint), so this
	// predicate is index-backed as a range only when `cluster=` is also given —
	// which is the shape the roll-up drill-down actually sends, because a bucket
	// is already scoped to the cluster whose alerts produced it. On its own it is
	// an org-scoped filter; the DDL that would make it a range of its own is
	// reported rather than written, because migrations are not owned here.
	if len(f.Fingerprints) > 0 {
		q = q.Where(sq.Eq{"source_fingerprint": f.Fingerprints})
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
	if err := db.RequireScope(s); err != nil {
		return nil, false, err
	}
	expr, ok := rollupKeyExpr[key.String()]
	if !ok {
		return nil, false, errs.Validation("group_by_invalid",
			"group_by must be one of: alertname, namespace, fingerprint")
	}
	n := db.ClampLimit(limit)
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

	page, hasMore := db.PageOf(out, n)
	return page, hasMore, nil
}

// ------------------------------------------------------------------ writes

const setProjectionBatchSQL = `
UPDATE alerts a SET
    state                 = p.state,
    current_occurrence_id = p.current_occurrence_id,
    ack_state             = p.ack_state,
    snoozed_until         = p.snoozed_until,
    last_seen_at          = GREATEST(a.last_seen_at, p.last_seen_at),
    last_state_change_at  = GREATEST(a.first_seen_at, p.last_state_change_at),
    total_occurrences     = p.total_occurrences,
    updated_at            = now()
  FROM unnest($2::uuid[], $3::text[], $4::uuid[], $5::text[], $6::timestamptz[],
              $7::timestamptz[], $8::timestamptz[], $9::int[])
       AS p(alert_id, state, current_occurrence_id, ack_state, snoozed_until,
            last_seen_at, last_state_change_at, total_occurrences)
 WHERE a.org_id = $1 AND a.id = p.alert_id`

// SetProjection writes the denormalised current-state summary onto `alerts`.
//
// It runs in the SAME transaction as the occurrence transition that caused it —
// current state is a projection and `alert_events` is the truth, so the two must
// never be observable apart. The GREATEST guards mirror alerts_seen_ck and
// alerts_change_ck so that a skewed clock produces a clamped row rather than a
// 23514 in the middle of an ingest batch.
//
// ⚠️ IT IS LAST-WRITER-WINS AND THAT IS ACCEPTED, because every caller is already
// serialised by something stronger:
//
//   - `Service.observe` holds this alert's row lock from its `UpsertBatch`
//     onwards, so two ingest or reconcile batches touching one alert cannot
//     interleave here at all;
//   - `Service.expire` reaches this line only after WINNING the occurrence's
//     `state_version` compare-and-set, so a projection it writes describes a
//     transition it actually made;
//   - the acknowledge path reaches it only after winning `SetAck`'s version
//     assertion, which is the guard that stopped it rewinding `state` and
//     `current_occurrence_id` to a pre-resolution episode.
//
// Adding a version predicate here would therefore arbitrate between writers that
// cannot race, at the cost of a second failure mode on the ingest hot path. What
// it must NOT become is a write reachable without one of those three guarantees —
// a new caller that reads an alert, thinks, and then writes this row is a caller
// that needs its own compare-and-set first.
func (r *AlertRepository) SetProjection(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, p domain.AlertProjection,
) error {
	return r.SetProjectionBatch(ctx, s, []domain.AlertProjectionWrite{
		{AlertID: alertID, Projection: p},
	})
}

// SetProjectionBatch is SetProjection for a whole observe batch in ONE
// statement — the same columns, the same GREATEST clamps, through one shared
// UPDATE so the two writes cannot drift apart. `unnest` produces its rows in
// array order, but order is immaterial here: each alert appears once, so there
// is no earlier write for a later one to shadow.
//
// ⛔ A DUPLICATE ALERT ID IS REFUSED. `UPDATE ... FROM` applies AT MOST ONE
// join row per target row and Postgres does not say which, so a batch carrying
// two projections for one alert would leave the surviving write to the planner.
// The caller collapses to the last write per alert BEFORE it gets here.
func (r *AlertRepository) SetProjectionBatch(
	ctx context.Context, s db.TenantScope, in []domain.AlertProjectionWrite,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if len(in) == 0 {
		return nil
	}

	n := len(in)
	ids := make([]uuid.UUID, n)
	states := make([]string, n)
	currentOccs := make([]*uuid.UUID, n)
	acks := make([]string, n)
	snoozed := make([]*time.Time, n)
	lastSeen := make([]time.Time, n)
	lastChange := make([]time.Time, n)
	totals := make([]int32, n)

	seen := make(map[uuid.UUID]struct{}, n)
	for i, w := range in {
		if err := db.RequireID("alert_id", w.AlertID); err != nil {
			return err
		}
		if _, dup := seen[w.AlertID]; dup {
			return errs.Internal("alert_projection_invalid",
				errsMissing("each alert may carry at most one projection per batch"))
		}
		seen[w.AlertID] = struct{}{}
		p := w.Projection
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
		ids[i] = w.AlertID
		states[i] = p.State.String()
		currentOccs[i] = p.CurrentOccurrenceID
		acks[i] = ack.String()
		snoozed[i] = p.SnoozedUntil
		lastSeen[i] = p.LastSeenAt.UTC()
		lastChange[i] = p.LastStateChangeAt.UTC()
		totals[i] = int32(p.TotalOccurrences)
	}

	tag, err := r.db(ctx).Exec(ctx, setProjectionBatchSQL, s.OrgID(), ids, states,
		currentOccs, acks, snoozed, lastSeen, lastChange, totals)
	if err != nil {
		return mapErr(err, "write alert projection")
	}
	if int(tag.RowsAffected()) != n {
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
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if err := db.RequireID("alert_id", alertID); err != nil {
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
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if err := db.RequireID("alert_id", alertID); err != nil {
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

// ⛔ BOTH TYPEAHEADS EXCLUDE SYNTHETICS, AND NEITHER QUERY SAYS SO ANY MORE.
// A drill writes an `oto_drill` label carrying a uuid, and a label typeahead that
// offered it — with a count of one, forever — would be advertising oto's own
// plumbing as if it were the customer's estate. Since 00045 the exclusion is
// applied where the projection is WRITTEN (`want` in `upsertAlertsSQL` skips
// synthetic rows), so a synthetic alert has no rows to read here. These are
// still two of the reads listed on the `alerts.synthetic` column comment in
// 00039_delivery_drills.sql; the predicate moved upstream, it did not go away.
//
// ⭐ NEITHER READ TOUCHES `alerts`. That is the point of 00045 and the property
// the plan test asserts: `alerts` is on ADR 0024's never-reaped list, so any
// per-keystroke read of it grows without bound for the life of the install.
const distinctLabelNamesSQL = `
SELECT n.label_name, n.alert_count
  FROM alert_label_names n
 WHERE n.org_id = $1
   AND n.alert_count > 0
   AND n.label_name LIKE $2 || '%'
 ORDER BY n.alert_count DESC, n.label_name ASC
 LIMIT $3`

// DistinctLabelNames feeds the filter bar's label typeahead, WITH the count of
// alerts carrying each name.
//
// ⭐ The count is what the contract has always called `alert_count`, and it is
// not decoration: a typeahead that offers a label matching nothing spends the
// one minute of an incident that matters most. Ordering by it puts the useful
// labels first, with the name as a deterministic tiebreak.
//
// NOTE (planner): `alert_label_names_rank_idx` (00045) serves this WHOLE
// statement — `(org_id, alert_count DESC, label_name) WHERE alert_count > 0`
// matches the filter and the ORDER BY, so the LIMIT terminates an Index Only
// Scan instead of a Sort having to read every name first. The work is bounded by
// the org's LABEL cardinality, not by its alert count.
//
// ⛔ WHAT THIS NOTE USED TO SAY, because it was wrong in a way worth keeping
// visible. It said no index served the query, that it was "bounded by `limit`",
// that it was "a discovery endpoint, not a hot path", and that the fix "would be
// a migration this module does not own". LIMIT bounds rows RETURNED — the
// GROUP BY above it had to aggregate the entire tenant before it could know
// which twenty-five they were. The endpoint is the filter bar of the incident
// view. And the ownership clause was answering the wrong question: an expression
// index over `jsonb_object_keys` is not unowned, it is IMPOSSIBLE — Postgres
// refuses a set-returning function in an index expression in every opclass. That
// is why 00045 is a projection and not an index on `alerts`.
func (r *AlertRepository) DistinctLabelNames(
	ctx context.Context, s db.TenantScope, prefix string, limit int,
) ([]domain.LabelCount, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, distinctLabelNamesSQL, s.OrgID(), prefix, db.ClampLimit(limit))
	if err != nil {
		return nil, mapErr(err, "list label names")
	}
	defer rows.Close()
	return collectLabelCounts(rows, "label name")
}

// ⛔ THE OCTET_LENGTH PREDICATE IS NOT A FILTER THE CALLER ASKED FOR, IT IS HOW
// THE INDEX IS REACHED. `alert_labels_value_idx` (00045) is PARTIAL on the
// identical expression, because B5 admits a 4096-byte label value, a btree tuple
// is capped at 2704, and an over-long entry ERRORS the INSERT rather than
// skipping the index — which on the ingest path, where 00045 writes this table,
// is an outage. Repeating the predicate VERBATIM is what lets the planner prove
// the partial index applies.
//
// ⛔ BOTH CONJUNCTS ARE LOAD-BEARING AND SO IS THE UNIT, and an earlier version
// of this file had both wrong. It said `length(l.label_value) <= 512`:
//
//	`length()` counts CHARACTERS and the btree cap is BYTES. 512 astral code
//	points are 2048 bytes, so a character-based predicate does not EXCLUDE the
//	row that overflows the index — it ADMITS it, and the INSERT fails with
//	`index row size 3104 exceeds btree version 4 maximum 2704`.
//
//	And `label_name` is part of the index row, so bounding the value alone can
//	never be enough however it is measured: the value alone cannot reach 2704,
//	and what overflows is the SUM of the two columns.
//
// Dropping either conjunct here also silently costs the index, because a query
// carrying only one of them does not IMPLY the index's predicate and the planner
// will not use a partial index it cannot prove applies. This must stay
// character-for-character identical to the migration; the plan test is what
// notices if it does not.
const distinctLabelValuesSQL = `
SELECT l.label_value, count(*) AS n
  FROM alert_labels l
 WHERE l.org_id = $1
   AND l.label_name = $2
   AND l.label_value LIKE $3 || '%'
   AND octet_length(l.label_name) <= 1024 AND octet_length(l.label_value) <= 512
 GROUP BY l.label_value
 ORDER BY n DESC, l.label_value ASC
 LIMIT $4`

// DistinctLabelValues feeds the value typeahead for one label name, with the
// same per-value alert count.
//
// NOTE (planner): `alert_labels_value_idx` (00045) drives this — `(org_id,
// label_name)` are equalities and `label_value` is a PREFIX RANGE under
// `text_pattern_ops`, so a longer prefix narrows the WORK and not merely the
// result. That is the specific thing the old statement could not do: it read
// `a.labels ->> $2` off every alert in the org and applied the prefix afterwards.
//
// ⛔ AND IT COULD NEVER HAVE BEEN FIXED WITH AN INDEX. The label name arrives as
// a runtime parameter, and an index expression is fixed at CREATE INDEX time, so
// there was no `labels ->> $2` to index — one index per label name an operator
// might type is not a schema. `alerts_labels_gin` is jsonb_path_ops and answers
// containment, which needs the value this query exists to discover.
func (r *AlertRepository) DistinctLabelValues(
	ctx context.Context, s db.TenantScope, name, prefix string, limit int,
) ([]domain.LabelCount, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, errs.Validation("label_name_required", "a label name is required")
	}
	rows, err := r.db(ctx).Query(ctx, distinctLabelValuesSQL, s.OrgID(), name, prefix, db.ClampLimit(limit))
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
