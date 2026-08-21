package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// listConnections serves GET /api/v1/channel-connections.
func (rt *Router) listConnections(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.connections != nil, "channels_connections_store_unavailable",
		"the connection store is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	p := httpx.NewParams(r, "limit", "cursor")
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	limit := p.Limit()
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	cursor, err := httpx.DecodeCursor(p.Cursor(), httpx.FilterHash("channel-connections"))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// Soft-deleted connections are excluded for the same reason listChannels
	// excludes soft-deleted channels: Settings is about what is configured now.
	conns, next, err := rt.connections.List(r.Context(), scope, false, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]ChannelConnectionDTO, 0, len(conns))
	for _, c := range conns {
		out = append(out, connectionDTO(c))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// createConnection serves POST /api/v1/channel-connections.
//
// ⛔ NO `Idempotency-Key` HANDLING HERE, unlike createChannel. A connection is
// admin setup, created rarely, and `channel_connections_name_uniq` is the same
// duplicate guard channels had before a6cc834 — sufficient here because
// nothing about a retried create is the kind of unrepeatable act that ticket
// was about (a message a human reads in a real room).
func (rt *Router) createConnection(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.requireConnectionWriteDeps(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[CreateChannelConnectionRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	kind := domain.Type(dto.Type)
	if err := rt.validateConnectionConfig(r.Context(), kind, dto.Config); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	credentialID, err := rt.sealCredential(r.Context(), scope, dto.Credential)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	// channel_connections_cred_ck: a slack connection MUST carry a credential.
	// Saying so here turns a 23514 (a 500 that tells the operator nothing) into
	// a field violation that names the control they left empty.
	if kind == domain.TypeSlack && credentialID == nil {
		httpx.WriteProblem(w, r, errs.Validation("validation_failed",
			"1 field failed validation.",
			errs.Violation{
				Field: "credential", Code: "required",
				Message: "a slack connection requires a bot token",
			}))
		return
	}

	conn, err := rt.connections.Create(r.Context(), scope, dto.toNewConnection(credentialID))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, connectionDTO(conn), started)
}

// getConnection serves GET /api/v1/channel-connections/{id}.
func (rt *Router) getConnection(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.connections != nil, "channels_connections_store_unavailable",
		"the connection store is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	conn, err := rt.connections.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if conn.Deleted() {
		httpx.WriteProblem(w, r, errs.NotFound("connection_deleted", "this connection has been deleted"))
		return
	}
	httpx.Data(w, r, http.StatusOK, connectionDTO(conn), started)
}

// updateConnection serves PATCH /api/v1/channel-connections/{id}.
func (rt *Router) updateConnection(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.requireConnectionWriteDeps(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[UpdateChannelConnectionRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if dto.IsEmpty() {
		httpx.WriteProblem(w, r, errs.Validation("validation_failed",
			"supply at least one field to change",
			errs.Violation{Field: "", Code: "min_properties", Message: "at least one property is required"}))
		return
	}

	existing, err := rt.connections.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if existing.Deleted() {
		httpx.WriteProblem(w, r, errs.NotFound("connection_deleted", "this connection has been deleted"))
		return
	}

	if dto.Config != nil {
		if err := rt.validateConnectionConfig(r.Context(), existing.Type, *dto.Config); err != nil {
			httpx.WriteProblem(w, r, err)
			return
		}
	}

	// A supplied credential ROTATES the existing secret in place, so the
	// connection — and every channel referencing it — never spends a moment
	// pointing at nothing.
	var credential **uuid.UUID
	if dto.Credential != nil {
		newID, cerr := rt.rotateCredential(r.Context(), scope, existing.CredentialID, dto.Credential)
		if cerr != nil {
			httpx.WriteProblem(w, r, cerr)
			return
		}
		if existing.Type == domain.TypeSlack && newID == nil {
			httpx.WriteProblem(w, r, errs.Validation("validation_failed",
				"1 field failed validation.",
				errs.Violation{
					Field: "credential", Code: "required",
					Message: "a slack connection requires a bot token",
				}))
			return
		}
		credential = &newID
	}

	conn, err := rt.connections.Update(r.Context(), scope, id, dto.toPatch(credential))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, connectionDTO(conn), started)
}

// deleteConnection serves DELETE /api/v1/channel-connections/{id}.
//
// ⛔ A connection still open through a live channel is a `409`, never a
// cascade — the same shape as deleteChannel's policy check, one hop further
// out: deleting it would leave those channels unable to open a provider at
// all, which is a worse silence than the `channel_disabled` suppression a
// deleted channel records.
func (rt *Router) deleteConnection(w http.ResponseWriter, r *http.Request) {
	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.connections != nil, "channels_connections_store_unavailable",
		"the connection store is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	channels, err := rt.connections.ReferencingChannels(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if len(channels) > 0 {
		httpx.WriteProblem(w, r, errs.Conflict("connection_in_use",
			"this connection is still open by a channel: "+strings.Join(channels, ", ")))
		return
	}

	if err := rt.connections.SoftDelete(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusNoContent, nil)
}

// resolveSlackConversation serves POST /api/v1/channel-connections/{id}/slack/resolve.
//
// ⭐ THIS IS THE SETTINGS-TIME INFERENCE the ADR restoring channels:read and
// groups:read exists for: given a channel name, answer its id, or the
// reverse — so the operator only ever types one half of "which Slack channel
// is this" and the other is filled in, read-only, from Slack itself.
func (rt *Router) resolveSlackConversation(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.resolver != nil, "channels_resolver_unavailable",
		"conversation resolution is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[ResolveConversationRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	query := domain.ConversationQuery{}
	switch {
	case dto.ConversationID != nil && *dto.ConversationID != "":
		query.ID = *dto.ConversationID
	case dto.Name != nil && *dto.Name != "":
		query.Name = *dto.Name
	default:
		httpx.WriteProblem(w, r, errs.Validation("validation_failed",
			"1 field failed validation.",
			errs.Violation{
				Field: "name", Code: "required",
				Message: "supply a channel name or a conversation_id to resolve",
			}))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), TestTimeout)
	defer cancel()

	res, err := rt.resolver.ResolveConversation(ctx, scope, id, query)
	if err != nil {
		httpx.WriteProblem(w, r, timeoutAware(ctx, r, err, "conversation_resolution_timeout",
			"Slack did not answer within the resolution budget"))
		return
	}
	httpx.Data(w, r, http.StatusOK, resolveConversationDTO(res), started)
}

// ------------------------------------------------------------------- helpers

func (rt *Router) requireConnectionWriteDeps() error {
	if err := requireDependency(rt.connections != nil, "channels_connections_store_unavailable",
		"the connection store is not configured in this deployment"); err != nil {
		return err
	}
	return requireDependency(rt.registry != nil, "channels_registry_unavailable",
		"no channel providers are registered in this deployment")
}

// validateConnectionConfig runs a Connection's config through its provider's
// ConnectionConfigSchema, re-rooting violations under `config/` for the same
// reason validateConfig does for a channel's config.
func (rt *Router) validateConnectionConfig(ctx context.Context, t domain.Type, raw json.RawMessage) error {
	if len(raw) == 0 {
		return errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{Field: "config", Code: "required", Message: "config is required"})
	}
	err := rt.registry.ValidateConnectionConfig(ctx, t, raw)
	if err == nil {
		return nil
	}
	e, ok := errs.As(err)
	if !ok || len(e.Violations) == 0 {
		return err
	}
	rerooted := make([]errs.Violation, 0, len(e.Violations))
	for _, v := range e.Violations {
		rerooted = append(rerooted, errs.Violation{
			Field:   "config/" + strings.TrimPrefix(strings.TrimPrefix(v.Field, "#"), "/"),
			Code:    v.Code,
			Message: v.Message,
		})
	}
	return errs.Validation(e.Code, e.Message, rerooted...)
}
