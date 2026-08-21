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

// connectionRow is the row model of `channel_connections`. Unexported, per the
// three-model rule (CONTEXT.md §5.5) — same shape and same reasoning as
// channelRow in channels.go, one level up: an org-wide setup rather than one
// destination.
type connectionRow struct {
	id     uuid.UUID
	orgID  uuid.UUID
	kind   string
	name   string
	config []byte
	credID *uuid.UUID

	createdAt time.Time
	updatedAt time.Time
	deletedAt *time.Time

	// Joined from channel_credentials. Never the sealed blob.
	credKind      *string
	credRotatedAt *time.Time
}

const connectionColumns = `
	cx.id, cx.org_id, cx.type, cx.name::text, cx.config, cx.credential_id,
	cx.created_at, cx.updated_at, cx.deleted_at,
	cc.kind, cc.rotated_at`

const connectionFrom = `
  FROM channel_connections cx
  LEFT JOIN channel_credentials cc ON cc.id = cx.credential_id AND cc.org_id = cx.org_id`

func (r *connectionRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.kind, &r.name, &r.config, &r.credID,
		&r.createdAt, &r.updatedAt, &r.deletedAt,
		&r.credKind, &r.credRotatedAt,
	}
}

func (r *connectionRow) toDomain() (domain.Connection, error) {
	t := domain.Type(r.kind)
	if !t.Valid() {
		return domain.Connection{}, errs.Internal("connection_type_invalid",
			errsMissing("channel_connections.type is outside the closed set: "+r.kind))
	}
	cfg := json.RawMessage(r.config)
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	return domain.Connection{
		ID:                  r.id,
		OrgID:               r.orgID,
		Type:                t,
		Name:                r.name,
		Config:              cfg,
		CredentialID:        r.credID,
		CredentialKind:      strOrEmpty(r.credKind),
		CredentialRotatedAt: r.credRotatedAt,
		CreatedAt:           r.createdAt,
		UpdatedAt:           r.updatedAt,
		DeletedAt:           r.deletedAt,
	}, nil
}

// ConnectionRepository is the SQL over `channel_connections`.
//
// Every statement carries an `org_id` predicate, for the same reason
// ChannelRepository's do: a missing one is a data leak, not a performance bug.
type ConnectionRepository struct {
	q     db.Querier
	clock clock.Clock
}

// NewConnectionRepository builds the repository over a fallback querier.
func NewConnectionRepository(q db.Querier, clk clock.Clock) *ConnectionRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &ConnectionRepository{q: q, clock: clk}
}

func (r *ConnectionRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const getConnectionSQL = `SELECT ` + connectionColumns + connectionFrom + ` WHERE cx.org_id = $1 AND cx.id = $2`

// Get reads one connection, DELETED OR NOT — a channel still pointing at a
// soft-deleted connection has to be explainable, same reasoning as
// ChannelRepository.Get.
func (r *ConnectionRepository) Get(
	ctx context.Context, s db.TenantScope, connectionID uuid.UUID,
) (domain.Connection, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Connection{}, err
	}
	var row connectionRow
	if err := r.db(ctx).QueryRow(ctx, getConnectionSQL, s.OrgID(), connectionID).Scan(row.scanDest()...); err != nil {
		if isNoRows(err) {
			return domain.Connection{}, errs.NotFound("connection_not_found", "no such connection")
		}
		return domain.Connection{}, mapErr(err, "connection_not_found", "read a connection")
	}
	return row.toDomain()
}

const listConnectionsSQL = `SELECT ` + connectionColumns + connectionFrom + `
 WHERE cx.org_id = $1
   AND ($2 OR cx.deleted_at IS NULL)
   AND ($3::timestamptz IS NULL OR (cx.created_at, cx.id) < ($3, $4))
 ORDER BY cx.created_at DESC, cx.id DESC
 LIMIT $5`

// List returns a keyset page of connections, newest first.
func (r *ConnectionRepository) List(
	ctx context.Context, s db.TenantScope, includeDeleted bool, p db.Keyset,
) ([]domain.Connection, db.Cursor, error) {
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

	rows, err := r.db(ctx).Query(ctx, listConnectionsSQL,
		s.OrgID(), includeDeleted, afterAt, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "connection_not_found", "list connections")
	}
	defer rows.Close()

	out := make([]domain.Connection, 0, limit+1)
	for rows.Next() {
		var row connectionRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, db.Cursor{}, mapErr(err, "connection_not_found", "scan a connection")
		}
		conn, err := row.toDomain()
		if err != nil {
			return nil, db.Cursor{}, err
		}
		out = append(out, conn)
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "connection_not_found", "read connections")
	}

	page, hasMore := db.PageOf(out, limit)
	cur := db.Cursor{Hash: p.Cursor.Hash}
	if len(page) > 0 {
		last := page[len(page)-1]
		cur = db.NextCursor(last.CreatedAt, last.ID, p.Cursor.Hash, hasMore)
	}
	return page, cur, nil
}

const insertConnectionSQL = `
INSERT INTO channel_connections (id, org_id, type, name, config, credential_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
RETURNING id`

// Create inserts a connection and returns it as stored.
//
// Same division of labour as ChannelRepository.Create: this does not validate
// `config` against the provider's ConnectionConfigSchema — that is layer 4 and
// belongs to the caller holding the registry.
func (r *ConnectionRepository) Create(
	ctx context.Context, s db.TenantScope, in domain.NewConnection,
) (domain.Connection, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Connection{}, err
	}
	if !in.Type.Valid() {
		return domain.Connection{}, errs.Internal("connection_type_invalid",
			errsMissing("a connection type is required"))
	}
	if strings.TrimSpace(in.Name) == "" {
		return domain.Connection{}, errs.Internal("connection_name_missing",
			errsMissing("a connection name is required"))
	}
	cfg := in.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}

	now := r.clock.Now().UTC()
	newID := in.ID
	if newID == uuid.Nil {
		newID = id.New()
	}

	var stored uuid.UUID
	err := r.db(ctx).QueryRow(ctx, insertConnectionSQL,
		newID, s.OrgID(), string(in.Type), in.Name, []byte(cfg), in.CredentialID, now,
	).Scan(&stored)
	if err != nil {
		return domain.Connection{}, mapErr(err, "connection_not_found", "create a connection")
	}
	return r.Get(ctx, s, stored)
}

const updateConnectionSQL = `
UPDATE channel_connections SET
    name          = COALESCE($3, name),
    config        = COALESCE($4, config),
    credential_id = CASE WHEN $5 THEN $6 ELSE credential_id END,
    updated_at    = GREATEST(updated_at, $7)
 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
RETURNING id`

// Update applies a ConnectionPatch. An empty patch is refused, same reasoning
// as ChannelRepository.Update.
func (r *ConnectionRepository) Update(
	ctx context.Context, s db.TenantScope, connectionID uuid.UUID, p domain.ConnectionPatch,
) (domain.Connection, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Connection{}, err
	}
	if err := db.RequireID("connection_id", connectionID); err != nil {
		return domain.Connection{}, err
	}
	if p.IsEmpty() {
		return domain.Connection{}, errs.Validation("empty_patch", "supply at least one field to change")
	}

	var (
		cfg     *[]byte
		setCred bool
		credVal *uuid.UUID
	)
	if p.Config != nil {
		b := []byte(*p.Config)
		cfg = &b
	}
	if p.CredentialID != nil {
		setCred, credVal = true, *p.CredentialID
	}

	var stored uuid.UUID
	err := r.db(ctx).QueryRow(ctx, updateConnectionSQL,
		s.OrgID(), connectionID, p.Name, cfg, setCred, credVal, r.clock.Now().UTC(),
	).Scan(&stored)
	if err != nil {
		if isNoRows(err) {
			return domain.Connection{}, errs.NotFound("connection_not_found", "no such connection")
		}
		return domain.Connection{}, mapErr(err, "connection_not_found", "update a connection")
	}
	return r.Get(ctx, s, stored)
}

const softDeleteConnectionSQL = `
UPDATE channel_connections SET deleted_at = $3, updated_at = GREATEST(updated_at, $3)
 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`

// SoftDelete removes a connection from use. `channels.connection_id` is
// ON DELETE RESTRICT, so a hard delete is refused by the database while any
// channel still references it; the caller should check ReferencingChannels
// first for a named 409 rather than a raw FK violation, same shape as
// ChannelRepository.ReferencingPolicies.
func (r *ConnectionRepository) SoftDelete(ctx context.Context, s db.TenantScope, connectionID uuid.UUID) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx, softDeleteConnectionSQL, s.OrgID(), connectionID, r.clock.Now().UTC())
	if err != nil {
		return mapErr(err, "connection_not_found", "delete a connection")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("connection_not_found", "no such connection")
	}
	return nil
}

const referencingChannelsSQL = `
SELECT name::text
  FROM channels
 WHERE org_id = $1 AND deleted_at IS NULL AND connection_id = $2
 ORDER BY name
 LIMIT 16`

// ReferencingChannels names the live channels still open through this
// connection. A connection still in use is a `409`, never a cascade: deleting
// it would leave those channels unable to open a provider at all.
func (r *ConnectionRepository) ReferencingChannels(
	ctx context.Context, s db.TenantScope, connectionID uuid.UUID,
) ([]string, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, referencingChannelsSQL, s.OrgID(), connectionID)
	if err != nil {
		return nil, mapErr(err, "connection_not_found", "find channels referencing a connection")
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, mapErr(err, "connection_not_found", "scan a channel name")
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "connection_not_found", "read channels referencing a connection")
	}
	return out, nil
}
