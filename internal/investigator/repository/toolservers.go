package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// ToolServerRepository is every statement against `tool_servers` and
// `tool_server_tools` (migration 00093, git-bug 2e9a086).
//
// ⛔ IT NEVER SEES THE TOKEN. The row holds `credential_id`; sealing goes through the
// CredentialWriter port inside the same transaction as the INSERT, and the one reader of
// the sealed value is KeyStore.ResolveToolServerToken.
//
// ⛔ EVERY METHOD IS TENANT-SCOPED, AND `org_id` IS IN EVERY PREDICATE, so another org's
// ToolServer is the same answer as one that never existed: 404.
type ToolServerRepository struct {
	q db.Querier
}

// NewToolServerRepository builds the repository over a fallback querier; a transaction
// travelling in the context wins over it.
func NewToolServerRepository(q db.Querier) *ToolServerRepository { return &ToolServerRepository{q: q} }

func (r *ToolServerRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// mapToolServerErr is the §L.9 translation under this module's codes. A name already
// taken in the org is a 23505 on `tool_servers_org_name_uniq`, which MapError turns into
// a 409 whose code is that index's name.
func mapToolServerErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "tool_server_not_found",
		NotFoundMessage:    "no such ToolServer",
		QueryFailed:        "investigator_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
	})
}

const toolServerColumns = `id, org_id, name, url, transport, access, credential_id, call_timeout_s,
  max_result_bytes, discovered_at, discovery_failed_at, discovery_error, created_at, updated_at`

// ⭐ BOTH TIMESTAMPS ARE PASSED, never defaulted, and are the SAME instant on insert, so
// `tool_servers_time_ck` holds by construction.
const insertToolServerSQL = `
INSERT INTO tool_servers (id, org_id, name, url, transport, access, credential_id,
                          call_timeout_s, max_result_bytes, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
RETURNING ` + toolServerColumns

// Insert stores a validated draft's public half. `draft.Token` is NOT read here: the
// caller has already sealed it and passes the row id (uuid.Nil for none).
func (r *ToolServerRepository) Insert(
	ctx context.Context, s db.TenantScope, draft domain.ToolServerDraft, credentialID uuid.UUID, at time.Time,
) (domain.ToolServerConfig, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.ToolServerConfig{}, err
	}
	var cred *uuid.UUID
	if credentialID != uuid.Nil {
		cred = &credentialID
	}
	row := r.db(ctx).QueryRow(ctx, insertToolServerSQL,
		id.New(), s.OrgID(), draft.Name, draft.URL, string(draft.Transport), string(draft.Access), cred,
		draft.Limits.TimeoutSeconds(), draft.Limits.MaxResultBytes, at.UTC())
	out, err := scanToolServer(row)
	if err != nil {
		return domain.ToolServerConfig{}, mapToolServerErr(err, "store a ToolServer")
	}
	return out, nil
}

// Get reads one ToolServer.
func (r *ToolServerRepository) Get(ctx context.Context, s db.TenantScope, toolServerID uuid.UUID) (domain.ToolServerConfig, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.ToolServerConfig{}, err
	}
	if err := db.RequireID("tool_server_id", toolServerID); err != nil {
		return domain.ToolServerConfig{}, err
	}
	row := r.db(ctx).QueryRow(ctx,
		`SELECT `+toolServerColumns+` FROM tool_servers WHERE org_id = $1 AND id = $2`, s.OrgID(), toolServerID)
	out, err := scanToolServer(row)
	if err != nil {
		return domain.ToolServerConfig{}, mapToolServerErr(err, "read a ToolServer")
	}
	return out, nil
}

// MaxListedToolServers bounds List and ByNames. An org runs a handful of ToolServers.
const MaxListedToolServers = 200

// List reads an org's ToolServers by name, served by `tool_servers_org_name_uniq`.
func (r *ToolServerRepository) List(ctx context.Context, s db.TenantScope) ([]domain.ToolServerConfig, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	return r.query(ctx, "list ToolServers",
		`SELECT `+toolServerColumns+` FROM tool_servers WHERE org_id = $1 ORDER BY name LIMIT $2`,
		s.OrgID(), MaxListedToolServers)
}

// ByNames reads the org's ToolServers with these names; a name it does not have is
// absent from the result, never an error.
func (r *ToolServerRepository) ByNames(ctx context.Context, s db.TenantScope, names []string) ([]domain.ToolServerConfig, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return []domain.ToolServerConfig{}, nil
	}
	return r.query(ctx, "read ToolServers by name",
		`SELECT `+toolServerColumns+` FROM tool_servers WHERE org_id = $1 AND name = ANY($2::text[])
		  ORDER BY name LIMIT $3`,
		s.OrgID(), names, MaxListedToolServers)
}

func (r *ToolServerRepository) query(ctx context.Context, what, sql string, args ...any) ([]domain.ToolServerConfig, error) {
	rows, err := r.db(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, mapToolServerErr(err, what)
	}
	defer rows.Close()
	out := []domain.ToolServerConfig{}
	for rows.Next() {
		c, err := scanToolServer(rows)
		if err != nil {
			return nil, mapToolServerErr(err, what)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, mapToolServerErr(err, what)
	}
	return out, nil
}

// ReplaceTools records a successful discovery: the listed Tools replace the last list,
// and the ToolServer row says when and forgets any earlier failure. The caller's
// transaction makes the two one fact.
func (r *ToolServerRepository) ReplaceTools(
	ctx context.Context, s db.TenantScope, toolServerID uuid.UUID, tools []domain.DiscoveredTool, at time.Time,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx,
		`UPDATE tool_servers SET discovered_at = $3, discovery_failed_at = NULL, discovery_error = NULL
		  WHERE org_id = $1 AND id = $2`, s.OrgID(), toolServerID, at.UTC())
	if err != nil {
		return mapToolServerErr(err, "record a discovery")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("tool_server_not_found", "no such ToolServer")
	}
	if _, err := r.db(ctx).Exec(ctx,
		`DELETE FROM tool_server_tools WHERE org_id = $1 AND tool_server_id = $2`, s.OrgID(), toolServerID); err != nil {
		return mapToolServerErr(err, "replace a ToolServer's Tools")
	}
	if len(tools) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, t := range tools {
		var schema any
		if t.InputSchema != nil {
			schema = json.RawMessage(t.InputSchema)
		}
		batch.Queue(`INSERT INTO tool_server_tools (org_id, tool_server_id, name, description, input_schema, read_only_hint)
		             VALUES ($1, $2, $3, $4, $5, $6)`,
			s.OrgID(), toolServerID, t.Name, t.Description, schema, t.ReadOnlyHint)
	}
	if err := r.db(ctx).SendBatch(ctx, batch).Close(); err != nil {
		return mapToolServerErr(err, "store a ToolServer's Tools")
	}
	return nil
}

// RecordDiscoveryFailure records that a discovery failed, keeping the last good list.
func (r *ToolServerRepository) RecordDiscoveryFailure(
	ctx context.Context, s db.TenantScope, toolServerID uuid.UUID, reason string, at time.Time,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx,
		`UPDATE tool_servers SET discovery_failed_at = $3, discovery_error = $4 WHERE org_id = $1 AND id = $2`,
		s.OrgID(), toolServerID, at.UTC(), reason)
	if err != nil {
		return mapToolServerErr(err, "record a failed discovery")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("tool_server_not_found", "no such ToolServer")
	}
	return nil
}

// MaxListedTools bounds Tools: domain.MaxDiscoveredTools is the most a discovery stores.
const MaxListedTools = domain.MaxDiscoveredTools

// Tools reads the Tools one ToolServer listed at its last successful discovery, by name.
func (r *ToolServerRepository) Tools(ctx context.Context, s db.TenantScope, toolServerID uuid.UUID) ([]domain.DiscoveredTool, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx,
		`SELECT name, description, input_schema, read_only_hint FROM tool_server_tools
		  WHERE org_id = $1 AND tool_server_id = $2 ORDER BY name LIMIT $3`,
		s.OrgID(), toolServerID, MaxListedTools)
	if err != nil {
		return nil, mapToolServerErr(err, "list a ToolServer's Tools")
	}
	defer rows.Close()
	out := []domain.DiscoveredTool{}
	for rows.Next() {
		var (
			t      domain.DiscoveredTool
			schema []byte
		)
		if err := rows.Scan(&t.Name, &t.Description, &schema, &t.ReadOnlyHint); err != nil {
			return nil, mapToolServerErr(err, "list a ToolServer's Tools")
		}
		if schema != nil {
			t.InputSchema = json.RawMessage(schema)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, mapToolServerErr(err, "list a ToolServer's Tools")
	}
	return out, nil
}

func scanToolServer(row pgx.Row) (domain.ToolServerConfig, error) {
	var (
		out                 domain.ToolServerConfig
		transport, access   string
		cred                *uuid.UUID
		timeoutS, maxResult int
		discovered, failed  *time.Time
		discoveryErr        *string
	)
	if err := row.Scan(&out.ID, &out.OrgID, &out.Name, &out.URL, &transport, &access, &cred,
		&timeoutS, &maxResult, &discovered, &failed, &discoveryErr, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return domain.ToolServerConfig{}, err
	}
	out.Transport, out.Access = domain.ToolServerTransport(transport), domain.ToolServerAccess(access)
	if cred != nil {
		out.CredentialID = *cred
	}
	out.Limits = domain.CallLimits{Timeout: time.Duration(timeoutS) * time.Second, MaxResultBytes: maxResult}
	if discovered != nil {
		out.DiscoveredAt = discovered.UTC()
	}
	if failed != nil {
		out.DiscoveryFailedAt = failed.UTC()
	}
	if discoveryErr != nil {
		out.DiscoveryError = *discoveryErr
	}
	out.CreatedAt, out.UpdatedAt = out.CreatedAt.UTC(), out.UpdatedAt.UTC()
	return out, nil
}
