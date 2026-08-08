package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// warningJSON is the stored shape of one element of `source_health.warnings`.
// It is UNEXPORTED: the wire shape of a jsonb column is a repository concern, and
// the API renders `domain.HealthWarning` through its own DTO.
type warningJSON struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	Subject string    `json:"subject,omitempty"`
	At      time.Time `json:"at"`
}

// healthRow is the row model of `source_health`.
type healthRow struct {
	sourceID uuid.UUID
	orgID    uuid.UUID
	status   string

	lastPushAt          *time.Time
	lastReconcileAt     *time.Time
	lastReconcileStatus *string
	lastError           *string
	consecutiveFailures int32

	amVersion       *string
	sendResolved    *bool
	clockSkewMS     int64
	divergenceCount int32

	warnings  []byte
	updatedAt time.Time
}

const healthColumns = `
	source_id, org_id, status, last_push_at, last_reconcile_at, last_reconcile_status,
	last_error, consecutive_failures, am_version, send_resolved, clock_skew_ms,
	divergence_count, warnings, updated_at`

func (r *healthRow) scanDest() []any {
	return []any{
		&r.sourceID, &r.orgID, &r.status, &r.lastPushAt, &r.lastReconcileAt,
		&r.lastReconcileStatus, &r.lastError, &r.consecutiveFailures, &r.amVersion,
		&r.sendResolved, &r.clockSkewMS, &r.divergenceCount, &r.warnings, &r.updatedAt,
	}
}

func (r *healthRow) toDomain() (domain.SourceHealth, error) {
	status := domain.HealthStatus(r.status)
	switch status {
	case domain.HealthUnknown, domain.HealthHealthy, domain.HealthDegraded, domain.HealthUnreachable:
	default:
		return domain.SourceHealth{}, errs.Internal("source_health_status_invalid",
			errsMissing("source_health.status is outside the closed set: "+r.status))
	}

	var stored []warningJSON
	if len(r.warnings) > 0 {
		if err := json.Unmarshal(r.warnings, &stored); err != nil {
			return domain.SourceHealth{}, errs.Internal("jsonb_decode_failed", err)
		}
	}
	warnings := make([]domain.HealthWarning, 0, len(stored))
	for _, w := range stored {
		warnings = append(warnings, domain.HealthWarning{
			Code: w.Code, Message: w.Message, Subject: w.Subject, At: w.At,
		})
	}

	return domain.SourceHealth{
		SourceID:            r.sourceID,
		OrgID:               r.orgID,
		Status:              status,
		LastPushAt:          r.lastPushAt,
		LastReconcileAt:     r.lastReconcileAt,
		LastReconcileStatus: strOrEmpty(r.lastReconcileStatus),
		LastError:           strOrEmpty(r.lastError),
		ConsecutiveFailures: int(r.consecutiveFailures),
		AMVersion:           strOrEmpty(r.amVersion),
		SendResolved:        r.sendResolved,
		ClockSkew:           time.Duration(r.clockSkewMS) * time.Millisecond,
		DivergenceCount:     int(r.divergenceCount),
		Warnings:            warnings,
		UpdatedAt:           r.updatedAt,
	}, nil
}

const getHealthSQL = `SELECT ` + healthColumns + ` FROM source_health WHERE org_id = $1 AND source_id = $2`

// GetHealth returns the liveness projection.
//
// A source that has never been probed returns a zero-value SourceHealth with
// HealthUnknown and NOT an error: "not yet observed" is a state, not a failure —
// and because anything other than `healthy` blocks the reaper (§B.4), returning
// an error here would turn an unobserved source into a 500 on a page whose whole
// job is to say "we have not looked yet".
func (r *SourceRepository) GetHealth(
	ctx context.Context, s db.TenantScope, sourceID uuid.UUID,
) (domain.SourceHealth, error) {
	if err := requireScope(s); err != nil {
		return domain.SourceHealth{}, err
	}
	var row healthRow
	err := r.db(ctx).QueryRow(ctx, getHealthSQL, s.OrgID(), sourceID).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			return domain.SourceHealth{
				SourceID: sourceID,
				OrgID:    s.OrgID(),
				Status:   domain.HealthUnknown,
			}, nil
		}
		return domain.SourceHealth{}, mapErr(err, "sources_not_found", "read source health")
	}
	return row.toDomain()
}

const listHealthSQL = `SELECT ` + healthColumns + `
  FROM source_health WHERE org_id = $1 AND source_id = ANY($2)`

// HealthFor reads the projection for many sources in ONE round trip.
//
// The sources list renders health beside every row, and doing that with one query
// per source is how a settings page with twenty upstreams becomes twenty-one
// queries. Sources with no row are simply absent from the map; the caller
// substitutes the `unknown` zero value.
func (r *SourceRepository) HealthFor(
	ctx context.Context, s db.TenantScope, sourceIDs []uuid.UUID,
) (map[uuid.UUID]domain.SourceHealth, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	if len(sourceIDs) == 0 {
		return map[uuid.UUID]domain.SourceHealth{}, nil
	}

	rows, err := r.db(ctx).Query(ctx, listHealthSQL, s.OrgID(), sourceIDs)
	if err != nil {
		return nil, mapErr(err, "sources_not_found", "read source health")
	}
	defer rows.Close()

	out := make(map[uuid.UUID]domain.SourceHealth, len(sourceIDs))
	for rows.Next() {
		var row healthRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "sources_not_found", "scan source health")
		}
		h, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out[h.SourceID] = h
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "sources_not_found", "read source health")
	}
	return out, nil
}

const saveHealthSQL = `
INSERT INTO source_health (source_id, org_id, status, last_push_at, last_reconcile_at,
                           last_reconcile_status, last_error, consecutive_failures,
                           am_version, send_resolved, clock_skew_ms, divergence_count,
                           warnings, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (source_id) DO UPDATE SET
    status                = EXCLUDED.status,
    last_push_at          = EXCLUDED.last_push_at,
    last_reconcile_at     = EXCLUDED.last_reconcile_at,
    last_reconcile_status = EXCLUDED.last_reconcile_status,
    last_error            = EXCLUDED.last_error,
    consecutive_failures  = EXCLUDED.consecutive_failures,
    am_version            = EXCLUDED.am_version,
    send_resolved         = EXCLUDED.send_resolved,
    clock_skew_ms         = EXCLUDED.clock_skew_ms,
    divergence_count      = EXCLUDED.divergence_count,
    warnings              = EXCLUDED.warnings,
    updated_at            = EXCLUDED.updated_at`

// SaveHealth writes the projection IN PLACE.
//
// `source_health` is a PROJECTION, not an event: it has no history and is the one
// table in this module that is UPDATEd rather than appended to. The upsert exists
// because Create seeds a row and a probe may still race it on a fresh source.
func (r *SourceRepository) SaveHealth(ctx context.Context, s db.TenantScope, h domain.SourceHealth) error {
	if err := requireScope(s); err != nil {
		return err
	}
	if err := requireID("source_id", h.SourceID); err != nil {
		return err
	}
	if h.Status == "" {
		h.Status = domain.HealthUnknown
	}
	if h.ConsecutiveFailures < 0 || h.DivergenceCount < 0 {
		// source_health_fail_ck / _div_ck would make this a 23514 and therefore a
		// 500. Refuse it here, where the caller can be named.
		return errs.Internal("source_health_negative_counter",
			errsMissing("consecutive_failures and divergence_count must be >= 0"))
	}
	lastError := h.LastError
	if h.Status == domain.HealthUnreachable && lastError == "" {
		// source_health_error_ck: an unreachable row MUST carry an error.
		lastError = "unknown_error"
	}

	stored := make([]warningJSON, 0, len(h.Warnings))
	for _, w := range h.Warnings {
		stored = append(stored, warningJSON{
			Code: w.Code, Message: w.Message, Subject: w.Subject, At: w.At.UTC(),
		})
	}
	warnings, err := json.Marshal(stored)
	if err != nil {
		return errs.Internal("jsonb_encode_failed", err)
	}

	updatedAt := h.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = r.clock.Now()
	}

	_, err = r.db(ctx).Exec(ctx, saveHealthSQL,
		h.SourceID, s.OrgID(), string(h.Status), timePtr(derefTime(h.LastPushAt)),
		timePtr(derefTime(h.LastReconcileAt)), nilIfEmpty(h.LastReconcileStatus),
		nilIfEmpty(lastError), int32(h.ConsecutiveFailures), //nolint:gosec // guarded above
		nilIfEmpty(h.AMVersion), h.SendResolved, h.ClockSkew.Milliseconds(),
		int32(h.DivergenceCount), warnings, updatedAt.UTC()) //nolint:gosec // guarded above
	if err != nil {
		return mapErr(err, "sources_not_found", "write source health")
	}
	return nil
}

const touchPushSQL = `
INSERT INTO source_health (source_id, org_id, status, last_push_at, updated_at)
VALUES ($1, $2, 'unknown', $3, $3)
ON CONFLICT (source_id) DO UPDATE SET
    last_push_at = GREATEST(source_health.last_push_at, EXCLUDED.last_push_at),
    updated_at   = EXCLUDED.updated_at`

// TouchPush records that a webhook batch was accepted from this source.
//
// It deliberately does NOT move `status`. A push proves the source can reach oto;
// it proves nothing about whether oto can reach the source, which is what the
// reaper guard is asking about. Conflating the two would let a chatty
// Alertmanager mask an unreachable one.
func (r *SourceRepository) TouchPush(
	ctx context.Context, s db.TenantScope, sourceID uuid.UUID, at time.Time,
) error {
	if err := requireScope(s); err != nil {
		return err
	}
	if at.IsZero() {
		at = r.clock.Now()
	}
	if _, err := r.db(ctx).Exec(ctx, touchPushSQL, sourceID, s.OrgID(), at.UTC()); err != nil {
		return mapErr(err, "sources_not_found", "record a source push")
	}
	return nil
}

func derefTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}
