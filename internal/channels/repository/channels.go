package repository

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// channelRow is the row model of `channels`. It is UNEXPORTED and never leaves
// this package: the three-model rule (CONTEXT.md §5.5) says a DTO may not embed a
// row and a domain type may not be one. Mapping is explicit, in toDomain.
type channelRow struct {
	id        uuid.UUID
	orgID     uuid.UUID
	kind      string
	name      string
	config    []byte
	credID    *uuid.UUID
	caps      int64
	renderer  string
	verbosity string

	threadUpdates  bool
	showFieldEmoji bool
	enabled        bool

	healthStatus    string
	healthError     *string
	healthCheckedAt *time.Time

	createdAt time.Time
	updatedAt time.Time
	deletedAt *time.Time

	// Joined from channel_credentials. Never the sealed blob.
	credKind      *string
	credRotatedAt *time.Time
}

// channelColumns is the projection every channel query selects, in scan order.
// `c.` and `cc.` are spelled out because every read joins the credential meta.
const channelColumns = `
	c.id, c.org_id, c.type, c.name::text, c.config, c.credential_id, c.capabilities,
	c.renderer, c.verbosity, c.thread_updates, c.show_field_emoji, c.enabled,
	c.health_status, c.health_error, c.health_checked_at,
	c.created_at, c.updated_at, c.deleted_at,
	cc.kind, cc.rotated_at`

const channelFrom = `
  FROM channels c
  LEFT JOIN channel_credentials cc ON cc.id = c.credential_id AND cc.org_id = c.org_id`

func (r *channelRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.kind, &r.name, &r.config, &r.credID, &r.caps,
		&r.renderer, &r.verbosity, &r.threadUpdates, &r.showFieldEmoji, &r.enabled,
		&r.healthStatus, &r.healthError, &r.healthCheckedAt,
		&r.createdAt, &r.updatedAt, &r.deletedAt,
		&r.credKind, &r.credRotatedAt,
	}
}

// toDomain maps one row onto the entity, re-proving the closed vocabularies. A
// row that cannot become an Instance is a mapper bug and says so (§L.9(1)).
func (r *channelRow) toDomain() (domain.Instance, error) {
	t := domain.Type(r.kind)
	if !t.Valid() {
		return domain.Instance{}, errs.Internal("channel_type_invalid",
			errsMissing("channels.type is outside the closed set: "+r.kind))
	}
	h := domain.InstanceHealth(r.healthStatus)
	if !h.Valid() {
		return domain.Instance{}, errs.Internal("channel_health_invalid",
			errsMissing("channels.health_status is outside the closed set: "+r.healthStatus))
	}
	cfg := json.RawMessage(r.config)
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	if r.caps < 0 {
		return domain.Instance{}, errs.Internal("channel_capabilities_invalid",
			errsMissing("channels.capabilities is negative"))
	}

	return domain.Instance{
		ID:                  r.id,
		OrgID:               r.orgID,
		Type:                t,
		Name:                r.name,
		Config:              cfg,
		CredentialID:        r.credID,
		CredentialKind:      strOrEmpty(r.credKind),
		CredentialRotatedAt: r.credRotatedAt,
		Capabilities:        domain.Capability(uint32(r.caps)), //nolint:gosec // channels_caps_ck bounds it
		Renderer:            domain.RendererID(r.renderer),
		Verbosity:           domain.Verbosity(r.verbosity),
		ThreadUpdates:       r.threadUpdates,
		ShowFieldEmoji:      r.showFieldEmoji,
		Enabled:             r.enabled,
		Health:              h,
		HealthError:         strOrEmpty(r.healthError),
		HealthCheckedAt:     r.healthCheckedAt,
		CreatedAt:           r.createdAt,
		UpdatedAt:           r.updatedAt,
		DeletedAt:           r.deletedAt,
	}, nil
}

func errsMissing(what string) error { return &missingError{what: what} }

type missingError struct{ what string }

func (e *missingError) Error() string { return "repository: " + e.what }

// ChannelRepository is the SQL over `channels`.
//
// Every statement carries an `org_id` predicate. A missing one is not a
// performance bug, it is a data leak, so there is no query in this file that can
// be reached without a db.TenantScope.
type ChannelRepository struct {
	q     db.Querier
	clock clock.Clock
}

// NewChannelRepository builds the repository over a fallback querier, normally
// the general pool.
func NewChannelRepository(q db.Querier, clk clock.Clock) *ChannelRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &ChannelRepository{q: q, clock: clk}
}

func (r *ChannelRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// ------------------------------------------------------------------- reads

const getChannelSQL = `SELECT ` + channelColumns + channelFrom + ` WHERE c.org_id = $1 AND c.id = $2`

// Get reads one channel, DELETED OR NOT.
//
// A soft-deleted channel is still readable because delivery history points at it:
// a delivery whose destination 404s from the API is a history an operator cannot
// interpret. The caller decides what a deleted row means for its endpoint.
func (r *ChannelRepository) Get(
	ctx context.Context, s db.TenantScope, channelID uuid.UUID,
) (domain.Instance, error) {
	if err := requireScope(s); err != nil {
		return domain.Instance{}, err
	}
	var row channelRow
	if err := r.db(ctx).QueryRow(ctx, getChannelSQL, s.OrgID(), channelID).Scan(row.scanDest()...); err != nil {
		if isNoRows(err) {
			return domain.Instance{}, errs.NotFound("channel_not_found", "no such channel")
		}
		return domain.Instance{}, mapErr(err, "channel_not_found", "read a channel")
	}
	return row.toDomain()
}

const listChannelsSQL = `SELECT ` + channelColumns + channelFrom + `
 WHERE c.org_id = $1
   AND ($2 OR c.deleted_at IS NULL)
   AND ($3::timestamptz IS NULL OR (c.created_at, c.id) < ($3, $4))
 ORDER BY c.created_at DESC, c.id DESC
 LIMIT $5`

// List returns a keyset page of channels, newest first. There is no OFFSET in
// this codebase (SPEC §E.1).
//
// `includeDeleted` is a plain bool rather than a filter struct so that the port
// `channels/api` declares can name the signature without either package
// importing the other (CONTEXT.md §5.1). The settings screen passes false; an
// audit view passes true.
func (r *ChannelRepository) List(
	ctx context.Context, s db.TenantScope, includeDeleted bool, p db.Keyset,
) ([]domain.Instance, db.Cursor, error) {
	if err := requireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := clampLimit(p.Limit)

	var (
		afterAt *time.Time
		afterID uuid.UUID
	)
	if !p.Cursor.IsZero() {
		at := p.Cursor.SortKey.UTC()
		afterAt, afterID = &at, p.Cursor.ID
	}

	// One extra row decides HasMore without a COUNT.
	rows, err := r.db(ctx).Query(ctx, listChannelsSQL,
		s.OrgID(), includeDeleted, afterAt, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "channel_not_found", "list channels")
	}
	defer rows.Close()

	out := make([]domain.Instance, 0, limit+1)
	for rows.Next() {
		var row channelRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, db.Cursor{}, mapErr(err, "channel_not_found", "scan a channel")
		}
		inst, err := row.toDomain()
		if err != nil {
			return nil, db.Cursor{}, err
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "channel_not_found", "read channels")
	}

	page, hasMore := pageOf(out, limit)
	cur := db.Cursor{Hash: p.Cursor.Hash}
	if len(page) > 0 {
		last := page[len(page)-1]
		cur = nextCursor(last.CreatedAt, last.ID, p.Cursor.Hash, hasMore)
	}
	return page, cur, nil
}

// ------------------------------------------------------------------ writes

// ⭐ `created_at` AND `updated_at` ARE BOTH PASSED, from the same read of the
// injected clock ($13 twice). Neither is left to the column default, and 00032
// removed that default so this cannot regress quietly.
//
// The application owns time here (`platform/clock`), and the reason is
// `channels_time_ck`: `updated_at >= created_at`. If `created_at` came from
// `DEFAULT now()` — the DATABASE's clock — while every `updated_at` writer below
// stamps the GO process's, then an app server a few milliseconds behind its
// database would fail the FIRST health write on a new channel with a 23514: a
// 500 on the first delivery, with nothing actually wrong.
const insertChannelSQL = `
INSERT INTO channels (id, org_id, type, name, config, credential_id, capabilities,
                      renderer, verbosity, thread_updates, show_field_emoji, enabled,
                      created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)
RETURNING id`

// Create inserts a channel and returns it as stored.
//
// It does NOT validate the config against the provider schema: that is layer 4
// and belongs to the caller holding the registry (§L.5). The repository's job
// here is the org predicate, the row shape and the SQLSTATE translation.
func (r *ChannelRepository) Create(
	ctx context.Context, s db.TenantScope, in domain.NewInstance,
) (domain.Instance, error) {
	if err := requireScope(s); err != nil {
		return domain.Instance{}, err
	}
	if !in.Type.Valid() {
		return domain.Instance{}, errs.Internal("channel_type_invalid",
			errsMissing("a channel type is required"))
	}
	if strings.TrimSpace(in.Name) == "" {
		return domain.Instance{}, errs.Internal("channel_name_missing",
			errsMissing("a channel name is required"))
	}
	cfg := in.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	renderer := in.Renderer
	if renderer == "" {
		renderer = "default"
	}
	verbosity := in.Verbosity.Normalise()

	now := r.clock.Now().UTC()
	newID := id.New()

	var stored uuid.UUID
	err := r.db(ctx).QueryRow(ctx, insertChannelSQL,
		newID, s.OrgID(), string(in.Type), in.Name, []byte(cfg), in.CredentialID,
		int64(in.Capabilities), string(renderer), string(verbosity),
		in.ThreadUpdates, in.ShowFieldEmoji, in.Enabled, now,
	).Scan(&stored)
	if err != nil {
		return domain.Instance{}, mapErr(err, "channel_not_found", "create a channel")
	}
	return r.Get(ctx, s, stored)
}

// Update applies a partial change.
//
// Every column is written as `COALESCE($n, column)` so that one statement serves
// every combination of supplied fields. `updated_at` moves unconditionally,
// because channels_time_ck requires it to be >= created_at and the settings
// screen orders on it.
//
// ⭐ GREATEST KEEPS `updated_at` MONOTONIC, and that is a correctness guard, not
// a nicety. Every timestamp on this row comes from the application (00032), but
// "the application" is N pods and N clocks: a pod a few milliseconds behind the
// one that created the channel would otherwise write an `updated_at` BELOW
// `created_at` and fail channels_time_ck with a 23514 — a 500 on an ordinary
// settings PATCH. GREATEST makes the check unfalsifiable while leaving the value
// app-owned; it is the same idiom, for the same reason, as OrderingStore.Advance.
const updateChannelSQL = `
UPDATE channels SET
    name             = COALESCE($3, name),
    config           = COALESCE($4, config),
    credential_id    = CASE WHEN $5 THEN $6 ELSE credential_id END,
    capabilities     = COALESCE($7, capabilities),
    renderer         = COALESCE($8, renderer),
    verbosity        = COALESCE($9, verbosity),
    thread_updates   = COALESCE($10, thread_updates),
    show_field_emoji = COALESCE($11, show_field_emoji),
    enabled          = COALESCE($12, enabled),
    updated_at       = GREATEST(updated_at, $13)
 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
RETURNING id`

// Update applies a ChannelPatch. An empty patch is refused rather than silently
// bumping `updated_at`: a no-op PATCH that reports success is a PATCH whose
// author will believe something changed.
func (r *ChannelRepository) Update(
	ctx context.Context, s db.TenantScope, channelID uuid.UUID, p domain.InstancePatch,
) (domain.Instance, error) {
	if err := requireScope(s); err != nil {
		return domain.Instance{}, err
	}
	if err := requireID("channel_id", channelID); err != nil {
		return domain.Instance{}, err
	}
	if p.IsEmpty() {
		return domain.Instance{}, errs.Validation("empty_patch", "supply at least one field to change")
	}

	var (
		cfg     *[]byte
		setCred bool
		credVal *uuid.UUID
		caps    *int64
		rend    *string
		verb    *string
	)
	if p.Config != nil {
		b := []byte(*p.Config)
		cfg = &b
	}
	if p.CredentialID != nil {
		setCred, credVal = true, *p.CredentialID
	}
	if p.Capabilities != nil {
		v := int64(*p.Capabilities)
		caps = &v
	}
	if p.Renderer != nil {
		v := string(*p.Renderer)
		rend = &v
	}
	if p.Verbosity != nil {
		v := string(p.Verbosity.Normalise())
		verb = &v
	}

	var stored uuid.UUID
	err := r.db(ctx).QueryRow(ctx, updateChannelSQL,
		s.OrgID(), channelID, p.Name, cfg, setCred, credVal, caps, rend, verb,
		p.ThreadUpdates, p.ShowFieldEmoji, p.Enabled, r.clock.Now().UTC(),
	).Scan(&stored)
	if err != nil {
		if isNoRows(err) {
			return domain.Instance{}, errs.NotFound("channel_not_found", "no such channel")
		}
		return domain.Instance{}, mapErr(err, "channel_not_found", "update a channel")
	}
	return r.Get(ctx, s, stored)
}

// `deleted_at` records the caller's instant exactly; `updated_at` is monotonic
// for the reason given on updateChannelSQL.
const softDeleteChannelSQL = `
UPDATE channels SET deleted_at = $3, enabled = false, updated_at = GREATEST(updated_at, $3)
 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`

// SoftDelete stops future deliveries and leaves history intact.
//
// It is a soft delete because `notification_deliveries.channel_id` is ON DELETE
// CASCADE: a hard delete would erase the record of who was told, when — which is
// the point of the whole module.
func (r *ChannelRepository) SoftDelete(ctx context.Context, s db.TenantScope, channelID uuid.UUID) error {
	if err := requireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx, softDeleteChannelSQL, s.OrgID(), channelID, r.clock.Now().UTC())
	if err != nil {
		return mapErr(err, "channel_not_found", "delete a channel")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("channel_not_found", "no such channel")
	}
	return nil
}

const referencingPoliciesSQL = `
SELECT name::text
  FROM notification_policies
 WHERE org_id = $1 AND enabled AND deleted_at IS NULL AND $2 = ANY(channel_ids)
 ORDER BY priority, name
 LIMIT 16`

// ReferencingPolicies names the live policies that route to this channel.
//
// Deleting a channel that is still an enabled policy's destination is a `409`,
// not a cascade: silently orphaning a policy's only destination would make it
// stop notifying without saying so, which is exactly the invisible silence §B.6
// forbids. The policy NAMES come back so the error can tell the operator which
// ones to fix rather than merely refusing.
func (r *ChannelRepository) ReferencingPolicies(
	ctx context.Context, s db.TenantScope, channelID uuid.UUID,
) ([]string, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, referencingPoliciesSQL, s.OrgID(), channelID)
	if err != nil {
		return nil, mapErr(err, "channel_not_found", "find policies referencing a channel")
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, mapErr(err, "channel_not_found", "scan a policy name")
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "channel_not_found", "read policies referencing a channel")
	}
	return out, nil
}

// `health_checked_at` is the probe's OWN instant and is written verbatim — it
// answers "when was this last checked" and a monotonic version of it would lie.
// `updated_at` is the row's version and is monotonic (see updateChannelSQL):
// this is the write that produced `internal_error/channels_time_ck` in
// production, because it is the FIRST write after an INSERT and therefore the one
// with the least clock distance to absorb.
const setChannelHealthSQL = `
UPDATE channels
   SET health_status = $3, health_error = $4, health_checked_at = $5,
       updated_at = GREATEST(updated_at, $5)
 WHERE org_id = $1 AND id = $2
   AND (health_status IS DISTINCT FROM $3 OR health_error IS DISTINCT FROM $4)`

// SetHealth records what a probe or a delivery attempt learned about a
// destination.
//
// The `IS DISTINCT FROM` guard makes a repeated identical failure a no-op write
// rather than row-version churn: a busy dead channel would otherwise produce one
// UPDATE per attempt per delivery.
func (r *ChannelRepository) SetHealth(
	ctx context.Context, s db.TenantScope, channelID uuid.UUID,
	status domain.InstanceHealth, detail string, at time.Time,
) error {
	if err := requireScope(s); err != nil {
		return err
	}
	if !status.Valid() {
		return errs.Internal("channel_health_invalid", errsMissing("unknown channel health status"))
	}
	if status.NeedsDetail() && detail == "" {
		// channels_health_ck would reject this as a 23514, which is a 500. Refuse
		// it here, where the caller can be named.
		return errs.Internal("channel_health_detail_missing",
			errsMissing("a non-healthy channel status requires a health_error"))
	}
	if at.IsZero() {
		at = r.clock.Now()
	}
	if _, err := r.db(ctx).Exec(ctx, setChannelHealthSQL,
		s.OrgID(), channelID, string(status), nilIfEmpty(detail), at.UTC()); err != nil {
		return mapErr(err, "channel_not_found", "write channel health")
	}
	return nil
}

// Unused helper kept honest: timePtr and idPtr are shared with the credential
// mapper, so reference them here rather than exporting them for no reason.
var _ = timePtr
var _ = idPtr
