package repository

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// sourceRow is the row model of `alert_sources`. It is UNEXPORTED and never
// leaves this package: the three-model rule (CONTEXT.md §5.5) says a DTO may not
// embed a row and a domain type may not be one. Mapping is explicit, in toDomain.
type sourceRow struct {
	id        uuid.UUID
	orgID     uuid.UUID
	clusterID uuid.UUID
	name      string
	kind      string

	baseURL       string
	prometheusURL *string
	credentialID  *uuid.UUID
	tlsSkipVerify bool

	injectLabels      []byte
	ignoreLabels      []string
	redactLabels      []string
	redactAnnotations []string

	pushEnabled bool
	// ⛔ THERE IS NO `reconcileEnabled`. 00038 dropped the column: reconciliation
	// is not a per-source preference (ADR 0006 and its second amendment). The
	// interval below is the whole of the tuning surface.
	reconcileInterval int32

	createdAt time.Time
	updatedAt time.Time
	deletedAt *time.Time
}

// sourceColumns is the projection every source query selects, in scan order.
// `name` is CITEXT and is cast so pgx scans it as a plain string.
const sourceColumns = `
	id, org_id, cluster_id, name::text, kind, base_url, prometheus_url, auth_credential_id,
	tls_skip_verify, inject_labels, ignore_labels, redact_labels, redact_annotations,
	push_enabled, reconcile_interval_s, created_at, updated_at, deleted_at`

func (r *sourceRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.clusterID, &r.name, &r.kind, &r.baseURL, &r.prometheusURL,
		&r.credentialID, &r.tlsSkipVerify, &r.injectLabels, &r.ignoreLabels,
		&r.redactLabels, &r.redactAnnotations, &r.pushEnabled,
		&r.reconcileInterval, &r.createdAt, &r.updatedAt, &r.deletedAt,
	}
}

// toDomain maps one row onto the Source entity, re-proving the closed kind
// vocabulary. A row that cannot become a Source is a mapper bug and says so.
func (r *sourceRow) toDomain() (domain.Source, error) {
	kind := domain.Kind(r.kind)
	if kind != domain.KindAlertmanager && kind != domain.KindGrafana {
		return domain.Source{}, errs.Internal("source_kind_invalid",
			errsMissing("alert_sources.kind is outside the closed set: "+r.kind))
	}
	inject, err := decodeStringMap(r.injectLabels)
	if err != nil {
		return domain.Source{}, err
	}

	return domain.Source{
		ID:                r.id,
		OrgID:             r.orgID,
		ClusterID:         r.clusterID,
		Name:              r.name,
		Kind:              kind,
		BaseURL:           r.baseURL,
		PrometheusURL:     strOrEmpty(r.prometheusURL),
		AuthCredentialID:  r.credentialID,
		TLSSkipVerify:     r.tlsSkipVerify,
		InjectLabels:      inject,
		IgnoreLabels:      r.ignoreLabels,
		RedactLabels:      r.redactLabels,
		RedactAnnotations: r.redactAnnotations,
		PushEnabled:       r.pushEnabled,
		ReconcileInterval: time.Duration(r.reconcileInterval) * time.Second,
		CreatedAt:         r.createdAt,
		UpdatedAt:         r.updatedAt,
		DeletedAt:         r.deletedAt,
	}, nil
}

// SourceRepository is the SQL over `alert_sources` and `source_health`. It
// implements the `sources/service.SourceRepository` port and adds the write half
// the settings API needs.
//
// Every statement carries an `org_id` predicate. A missing one is not a
// performance bug, it is a data leak, so there is no query in this file that can
// be reached without a db.TenantScope.
type SourceRepository struct {
	q     db.Querier
	clock clock.Clock
}

// NewSourceRepository builds the repository over a fallback querier, normally the
// general pool. The clock is injected because ListDue is a predicate about "now"
// and must stay testable.
func NewSourceRepository(q db.Querier, clk clock.Clock) *SourceRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &SourceRepository{q: q, clock: clk}
}

func (r *SourceRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// ------------------------------------------------------------------- reads

const getSourceSQL = `SELECT ` + sourceColumns + ` FROM alert_sources WHERE org_id = $1 AND id = $2`

// Get returns one source, or an errs.KindNotFound.
//
// It returns SOFT-DELETED rows too. `sources/service` is the layer that turns a
// deleted source into a 404 (CodeSourceDeleted), and it can only do that if the
// repository hands the row over rather than hiding it — "deleted" and "never
// existed" are different answers and the service needs to be able to tell them
// apart.
func (r *SourceRepository) Get(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) (domain.Source, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Source{}, err
	}
	var row sourceRow
	if err := r.db(ctx).QueryRow(ctx, getSourceSQL, s.OrgID(), sourceID).Scan(row.scanDest()...); err != nil {
		if isNoRows(err) {
			return domain.Source{}, errs.NotFound("sources_not_found", "no such source")
		}
		return domain.Source{}, mapErr(err, "sources_not_found", "read a source")
	}
	return row.toDomain()
}

const listSourcesSQL = `SELECT ` + sourceColumns + ` FROM alert_sources
 WHERE org_id = $1
   AND ($2 OR deleted_at IS NULL)
   AND ($3::uuid IS NULL OR cluster_id = $3)
   AND ($4 = '' OR kind = $4)
   AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6))
 ORDER BY created_at DESC, id DESC
 LIMIT $7`

// List returns a keyset page of sources. There is no OFFSET in this codebase
// (SPEC §F.5.3).
func (r *SourceRepository) List(
	ctx context.Context, s db.TenantScope, f domain.SourceFilter, p db.Keyset,
) ([]domain.Source, db.Cursor, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := db.ClampLimit(p.Limit)

	var (
		afterAt *time.Time
		afterID uuid.UUID
	)
	if !p.Cursor.IsZero() {
		at := p.Cursor.SortKey.UTC()
		afterAt, afterID = &at, p.Cursor.ID
	}
	var clusterID *uuid.UUID
	if f.ClusterID != nil && *f.ClusterID != uuid.Nil {
		clusterID = f.ClusterID
	}

	rows, err := r.db(ctx).Query(ctx, listSourcesSQL,
		s.OrgID(), f.IncludeDeleted, clusterID, string(f.Kind),
		afterAt, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "sources_not_found", "list sources")
	}
	defer rows.Close()

	out := make([]domain.Source, 0, limit+1)
	for rows.Next() {
		var row sourceRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, db.Cursor{}, mapErr(err, "sources_not_found", "scan a source")
		}
		src, err := row.toDomain()
		if err != nil {
			return nil, db.Cursor{}, err
		}
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "sources_not_found", "read sources")
	}

	page, hasMore := db.PageOf(out, limit)
	cur := db.Cursor{Hash: p.Cursor.Hash}
	if len(page) > 0 {
		last := page[len(page)-1]
		cur = db.NextCursor(last.CreatedAt, last.ID, p.Cursor.Hash, hasMore)
	}
	return page, cur, nil
}

const listSourcesByIDsSQL = `SELECT ` + sourceColumns + ` FROM alert_sources
 WHERE org_id = $1 AND deleted_at IS NULL AND id = ANY($2)`

// ListByIDs returns the live sources named by ids, in ONE query.
//
// Unlike Get it hides soft-deleted rows: a caller batching a page of rows that
// each name a source is asking which of those sources still resolve, and there is
// no id-shaped 404 to distinguish "deleted" from "never existed" against.
func (r *SourceRepository) ListByIDs(
	ctx context.Context, s db.TenantScope, ids []uuid.UUID,
) ([]domain.Source, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	rows, err := r.db(ctx).Query(ctx, listSourcesByIDsSQL, s.OrgID(), ids)
	if err != nil {
		return nil, mapErr(err, "sources_not_found", "list sources by id")
	}
	defer rows.Close()

	out := make([]domain.Source, 0, len(ids))
	for rows.Next() {
		var row sourceRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "sources_not_found", "scan a source")
		}
		src, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "sources_not_found", "read sources by id")
	}
	return out, nil
}

const listDueSQL = `SELECT ` + `s.id, s.org_id, s.cluster_id, s.name::text, s.kind, s.base_url,
	s.prometheus_url, s.auth_credential_id, s.tls_skip_verify, s.inject_labels, s.ignore_labels,
	s.redact_labels, s.redact_annotations, s.push_enabled,
	s.reconcile_interval_s, s.created_at, s.updated_at, s.deleted_at` + `
  FROM alert_sources s
  LEFT JOIN source_health h ON h.source_id = s.id
 WHERE s.org_id = $1
   AND s.deleted_at IS NULL
   AND (h.last_reconcile_at IS NULL
        OR h.last_reconcile_at + make_interval(secs => s.reconcile_interval_s) <= $2)
 ORDER BY h.last_reconcile_at NULLS FIRST, s.id
 LIMIT $3`

// ListDue returns the sources whose reconcile interval has elapsed.
//
// It is the reconciler's fan-out query and is BOUNDED, never unbounded: a
// deployment with a thousand sources must not turn one tick into a thousand
// simultaneous outbound calls. A source that has never reconciled sorts first, so
// a newly registered Alertmanager is observed before an already-healthy one is
// re-observed.
//
// ⛔⛔ THE ONLY PREDICATES ARE `deleted_at IS NULL` AND THE INTERVAL, AND NOTHING
// ELSE MAY BE ADDED. This query used to carry `AND s.reconcile_enabled`, and that
// single conjunct was how the component ADR 0006 calls mandatory got switched off
// with one PATCH. A source excluded here is a source oto stops polling while
// `source_health` keeps its last verdict — so the reaper goes on trusting a
// `healthy` that nothing refreshes, and ends episodes for alerts that are merely
// silenced upstream. A source oto should poll less often gets a bigger
// `reconcile_interval_s`; there is no value of any column that means "never".
func (r *SourceRepository) ListDue(ctx context.Context, s db.TenantScope, limit int) ([]domain.Source, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, listDueSQL, s.OrgID(), r.clock.Now().UTC(), db.ClampLimit(limit))
	if err != nil {
		return nil, mapErr(err, "sources_not_found", "list due sources")
	}
	defer rows.Close()

	out := make([]domain.Source, 0, 16)
	for rows.Next() {
		var row sourceRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "sources_not_found", "scan a due source")
		}
		src, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "sources_not_found", "read due sources")
	}
	return out, nil
}

const resolveSourceOrgSQL = `SELECT org_id FROM alert_sources WHERE id = $1`

// ResolveOrg returns the org that owns a source.
//
// ⚠️ THIS IS THE ONE METHOD IN THIS FILE WITHOUT A TenantScope, and it is the
// same exception `ingestion/repository.BatchRepository.ResolveOrg` documents: the
// `source.reconcile` and `silences.sync` payloads name a source id and no org
// (§G.3), because a source id is globally unique and an org id in a payload is
// one more thing that can go stale. The org therefore has to be DISCOVERED before
// a scope can exist.
//
// It reads ONE column of ONE row addressed by its primary key and returns nothing
// else. Every call the worker makes afterwards is scoped by what it returns.
func (r *SourceRepository) ResolveOrg(ctx context.Context, sourceID uuid.UUID) (uuid.UUID, error) {
	var orgID uuid.UUID
	if err := r.db(ctx).QueryRow(ctx, resolveSourceOrgSQL, sourceID).Scan(&orgID); err != nil {
		if isNoRows(err) {
			return uuid.Nil, errs.NotFound("sources_not_found", "no such source")
		}
		return uuid.Nil, mapErr(err, "sources_not_found", "resolve the source's org")
	}
	return orgID, nil
}

// ------------------------------------------------------------------ writes

const insertSourceSQL = `
INSERT INTO alert_sources (id, org_id, cluster_id, name, kind, base_url, prometheus_url,
                           auth_credential_id, tls_skip_verify, inject_labels, ignore_labels,
                           redact_labels, redact_annotations, push_enabled,
                           reconcile_interval_s, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16)
RETURNING id`

// Create registers one upstream.
//
// It also seeds a `source_health` row with status `unknown`. That is not
// housekeeping: `source_health.status != 'healthy'` BLOCKS THE REAPER (§B.4), and
// a source with no health row at all would leave the reaper guard answering a
// question about a row that does not exist. "Not yet observed" is a state, and it
// has to be a stored one.
func (r *SourceRepository) Create(ctx context.Context, s db.TenantScope, in domain.SourceDraft) (domain.Source, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Source{}, err
	}
	if err := db.RequireID("cluster_id", in.ClusterID); err != nil {
		return domain.Source{}, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return domain.Source{}, errs.Internal("source_name_missing", errsMissing("a source name is required"))
	}
	if in.Kind == "" {
		in.Kind = domain.KindAlertmanager
	}

	inject, err := jsonbMap(in.InjectLabels)
	if err != nil {
		return domain.Source{}, err
	}
	interval := in.ReconcileInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	now := r.clock.Now().UTC()
	newID := id.New()

	var stored uuid.UUID
	err = r.db(ctx).QueryRow(ctx, insertSourceSQL,
		newID, s.OrgID(), in.ClusterID, in.Name, string(in.Kind), in.BaseURL,
		nilIfEmpty(in.PrometheusURL), in.AuthCredentialID, in.TLSSkipVerify, inject,
		nonNilStrings(in.IgnoreLabels), nonNilStrings(in.RedactLabels),
		nonNilStrings(in.RedactAnnotations), in.PushEnabled,
		int32(interval/time.Second), now, //nolint:gosec // bounded by alert_sources_ivl_ck
	).Scan(&stored)
	if err != nil {
		return domain.Source{}, mapErr(err, "sources_not_found", "create a source")
	}

	if err := r.SaveHealth(ctx, s, domain.SourceHealth{
		SourceID: stored, OrgID: s.OrgID(), Status: domain.HealthUnknown, UpdatedAt: now,
	}); err != nil {
		return domain.Source{}, err
	}
	return r.Get(ctx, s, stored)
}

// ⭐ GREATEST KEEPS `updated_at` MONOTONIC, and that is a correctness guard, not
// a nicety. Both timestamps on this row come from the application — Create above
// names them from the injected clock — but "the application" is N pods with N
// clocks, and the pod serving a settings PATCH is rarely the pod that registered
// the source. A few milliseconds of lag between them would otherwise write an
// `updated_at` BELOW `created_at` and fail `alert_sources_time_ck` with a 23514
// — a 500 on an ordinary edit, with nothing wrong. GREATEST makes the check
// unfalsifiable while leaving the value app-owned; it is the same idiom, for the
// same reason, as `channels`, `orgs` and OrderingStore.Advance.
const updateSourceSQL = `
UPDATE alert_sources SET
    cluster_id         = COALESCE($3, cluster_id),
    name               = COALESCE($4, name),
    base_url           = COALESCE($5, base_url),
    prometheus_url     = CASE WHEN $6  THEN $7  ELSE prometheus_url END,
    auth_credential_id = CASE WHEN $8  THEN $9  ELSE auth_credential_id END,
    tls_skip_verify    = COALESCE($10, tls_skip_verify),
    inject_labels      = COALESCE($11, inject_labels),
    ignore_labels      = COALESCE($12, ignore_labels),
    redact_labels      = COALESCE($13, redact_labels),
    redact_annotations = COALESCE($14, redact_annotations),
    push_enabled       = COALESCE($15, push_enabled),
    reconcile_interval_s = COALESCE($16, reconcile_interval_s),
    updated_at         = GREATEST(updated_at, $17)
 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
RETURNING id`

// Update applies a SourcePatch.
//
// ⚠️ Changing `ignore_labels` does NOT re-key existing alerts. It feeds the
// alert-identity hash, so new identities are created from that point forward.
// That is documented behaviour (§C.2) rather than a defect, and the API
// description says so out loud.
func (r *SourceRepository) Update(
	ctx context.Context, s db.TenantScope, sourceID uuid.UUID, p domain.SourcePatch,
) (domain.Source, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Source{}, err
	}
	if err := db.RequireID("source_id", sourceID); err != nil {
		return domain.Source{}, err
	}
	if p.IsEmpty() {
		return domain.Source{}, errs.Validation("empty_patch", "supply at least one field to change")
	}

	var (
		setProm  bool
		promVal  *string
		setCred  bool
		credVal  *uuid.UUID
		inject   *[]byte
		interval *int32
	)
	if p.PrometheusURL != nil {
		setProm = true
		if v := *p.PrometheusURL; v != nil {
			promVal = nilIfEmpty(*v)
		}
	}
	if p.AuthCredentialID != nil {
		setCred, credVal = true, *p.AuthCredentialID
	}
	if p.InjectLabels != nil {
		b, err := jsonbMap(*p.InjectLabels)
		if err != nil {
			return domain.Source{}, err
		}
		inject = &b
	}
	if p.ReconcileInterval != nil {
		v := int32(*p.ReconcileInterval / time.Second) //nolint:gosec // bounded by the DTO and the CHECK
		interval = &v
	}

	var stored uuid.UUID
	err := r.db(ctx).QueryRow(ctx, updateSourceSQL,
		s.OrgID(), sourceID, p.ClusterID, p.Name, p.BaseURL, setProm, promVal,
		setCred, credVal, p.TLSSkipVerify, inject, p.IgnoreLabels, p.RedactLabels,
		p.RedactAnnotations, p.PushEnabled, interval,
		r.clock.Now().UTC(),
	).Scan(&stored)
	if err != nil {
		if isNoRows(err) {
			return domain.Source{}, errs.NotFound("sources_not_found", "no such source")
		}
		return domain.Source{}, mapErr(err, "sources_not_found", "update a source")
	}
	return r.Get(ctx, s, stored)
}

// `deleted_at` records the caller's instant exactly — it answers "when was this
// retired" and a monotonic version of it would lie. `updated_at` is the row's
// version and is monotonic for the reason given on updateSourceSQL.
// `deleted_at` is what takes the source out of the reconcile fan-out — ListDue's
// first predicate — so a soft delete needs no second switch to stop polling, and
// there is no longer one to set.
const softDeleteSourceSQL = `
UPDATE alert_sources
   SET deleted_at = $3, push_enabled = false,
       updated_at = GREATEST(updated_at, $3)
 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`

// SoftDelete stops ingestion and reconciliation for one source.
//
// ⛔ IT IS A SOFT DELETE AND MUST STAY ONE. `api_tokens.source_id` is ON DELETE
// CASCADE and the alert history references the source through its cluster; a hard
// delete would erase the record of what this upstream once reported. Deleting a
// source must never erase the record of what it once said.
func (r *SourceRepository) SoftDelete(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx, softDeleteSourceSQL, s.OrgID(), sourceID, r.clock.Now().UTC())
	if err != nil {
		return mapErr(err, "sources_not_found", "delete a source")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("sources_not_found", "no such source")
	}
	return nil
}

// nonNilStrings normalises a nil slice onto an empty one. The three `TEXT[]`
// columns are NOT NULL, and `alert_sources_ignore_ck` additionally forbids a NULL
// element, so a nil slice must not reach the driver as SQL NULL.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
