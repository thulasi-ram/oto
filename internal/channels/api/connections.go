package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/idempotency"
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
	if err := checkCredentialSlots(kind, dto.Credential, dto.SigningCredential); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	// ⭐ THE MAPPING IS JUDGED BEFORE ANYTHING IS SEALED, so a refused mapping leaves
	// no orphaned credential row behind it.
	if err := checkMapping(kind, dto.PayloadMapping, dto.MappingSecrets, secretNames(dto.MappingSecrets)); err != nil {
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

	signingID, err := rt.sealCredential(r.Context(), scope, dto.SigningCredential)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	mappingID, err := rt.replaceMappingSecrets(r.Context(), scope, nil, dto.MappingSecrets)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	in := dto.toNewConnection(credentialID, signingID)
	if !domain.IsNullMapping(dto.PayloadMapping) {
		in.PayloadMapping = dto.PayloadMapping
	}
	in.MappingCredentialID, in.MappingSecretNames = mappingID, secretNames(dto.MappingSecrets)
	conn, err := rt.connections.Create(r.Context(), scope, in)
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
	if err := checkCredentialSlots(existing.Type, dto.Credential, dto.SigningCredential); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// ⭐ THE MAPPING THE CONNECTION WILL HAVE IS CHECKED AGAINST THE SECRETS IT WILL
	// HAVE, whichever of the two this request changes. Replacing the secrets re-checks
	// the stored mapping, so dropping a secret the mapping still reads is refused here
	// rather than failing every delivery after.
	mappingChanged := len(dto.PayloadMapping) > 0 || dto.MappingSecrets != nil
	names := existing.MappingSecretNames
	if dto.MappingSecrets != nil {
		names = secretNames(dto.MappingSecrets)
	}
	if mappingChanged {
		mapping := existing.PayloadMapping
		if len(dto.PayloadMapping) > 0 {
			mapping = dto.PayloadMapping
		}
		if err := checkMapping(existing.Type, mapping, dto.MappingSecrets, names); err != nil {
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

	// ⭐ A SUPPLIED SIGNING SECRET ROTATES WITH AN OVERLAP. The re-seal is the same
	// in-place UPDATE the credential above gets, and that UPDATE is what keeps the
	// outgoing secret signing beside the new one for domain.SigningSecretOverlap
	// (CredentialRepository.Rotate) — so a receiver that has not been told the new
	// secret yet keeps verifying, rather than rejecting the next delivery.
	var signing **uuid.UUID
	if dto.SigningCredential != nil {
		newID, cerr := rt.rotateCredential(r.Context(), scope, existing.SigningCredentialID, dto.SigningCredential)
		if cerr != nil {
			httpx.WriteProblem(w, r, cerr)
			return
		}
		signing = &newID
	}

	patch := dto.toPatch(credential, signing)
	if len(dto.PayloadMapping) > 0 {
		mapping := dto.PayloadMapping
		if domain.IsNullMapping(mapping) {
			mapping = nil
		}
		patch.PayloadMapping = &mapping
	}
	if dto.MappingSecrets != nil {
		newID, cerr := rt.replaceMappingSecrets(r.Context(), scope, existing.MappingCredentialID, dto.MappingSecrets)
		if cerr != nil {
			httpx.WriteProblem(w, r, cerr)
			return
		}
		patch.MappingSecrets = &domain.MappingSecretsSlot{CredentialID: newID, Names: names}
	}

	conn, err := rt.connections.Update(r.Context(), scope, id, patch)
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

// testConnectionMapping serves POST /api/v1/channel-connections/{id}/mapping/test.
//
// ⚠️ IT MAY OPEN A REAL INCIDENT IN THE TOOL THE MAPPING POINTS AT. It sends one fact
// the operator chose through the connection's payload mapping and one of its
// channels — the real renderer, mapping, secrets, signature and transport — so it
// takes testChannel's `Idempotency-Key` claim for testChannel's reason, and the
// settings page says so beside the button.
func (rt *Router) testConnectionMapping(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.writes != nil, "channels_tester_unavailable",
		"payload mapping testing is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	raw, err := httpx.ReadBody(w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[TestConnectionMappingRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	// The same key on another channel or another fact is a different request.
	idem, err := idempotencyIntent(r, idempotency.HashTargetedRequest(id, raw))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), TestTimeout)
	defer cancel()

	res, err := rt.writes.TestMapping(ctx, scope, id, dto.ChannelID, dto.Fact, idem)
	if err != nil {
		httpx.WriteProblem(w, r, timeoutAware(ctx, r, err, "channel_test_timeout",
			"the destination did not answer within the test budget"))
		return
	}
	httpx.Data(w, r, http.StatusOK, testDTO(res), started)
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

// signingCredentialKind is the only kind the signing slot holds (migration 00088).
const signingCredentialKind = "webhook_signing_secret"

// checkCredentialSlots refuses a secret in the wrong one of a connection's two
// slots, BEFORE anything is sealed.
//
// ⛔ NO CHECK CONSTRAINT CAN SEE THIS, so it is here or nowhere. Both slots
// reference the same `channel_credentials` table, and its kind lives on the row,
// not on the reference. A bearer token in the signing slot would become an HMAC
// key the receiver is also sent as `Authorization`; a signing secret in the
// credential slot would sign nothing and authenticate nothing — the silent
// unsigned connection this split exists to end. Each is a 422 naming the field.
func checkCredentialSlots(t domain.Type, credential, signing *CredentialInputDTO) error {
	var violations []errs.Violation
	if credential != nil && credential.Kind == signingCredentialKind {
		violations = append(violations, errs.Violation{
			Field: "credential/kind", Code: "enum",
			Message: "a signing secret goes in signing_credential, beside this credential, not in it",
		})
	}
	if signing != nil && signing.Kind != "none" {
		switch {
		case signing.Kind != signingCredentialKind:
			violations = append(violations, errs.Violation{
				Field: "signing_credential/kind", Code: "enum",
				Message: "signing_credential holds a webhook_signing_secret, or none to detach it",
			})
		case t != domain.TypeWebhook:
			violations = append(violations, errs.Violation{
				Field: "signing_credential", Code: "forbidden",
				Message: "only a webhook connection signs its outbound requests",
			})
		}
	}
	if len(violations) == 0 {
		return nil
	}
	return errs.Validation("validation_failed",
		strconv.Itoa(len(violations))+" field(s) failed validation.", violations...)
}

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

// checkMapping judges the payload mapping a connection will carry against the secret
// names it will hold, and the secrets a request supplies (ADR 0055 §2). Each refusal
// is a 422 naming the field — for a mapping that does not render, the fact.
//
// ⛔ ONLY A WEBHOOK CARRIES ONE. A Slack connection has no envelope to map.
func checkMapping(t domain.Type, mapping json.RawMessage, secrets map[string]string, names []string) error {
	var violations []errs.Violation
	hasMapping := !domain.IsNullMapping(mapping)
	if t != domain.TypeWebhook {
		if hasMapping {
			violations = append(violations, errs.Violation{Field: "payload_mapping", Code: "forbidden",
				Message: "only a webhook connection carries a payload mapping"})
		}
		if len(secrets) > 0 {
			violations = append(violations, errs.Violation{Field: "mapping_secrets", Code: "forbidden",
				Message: "only a webhook connection seals mapping secrets"})
		}
	}
	if len(secrets) > domain.MaxMappingSecrets {
		violations = append(violations, errs.Violation{Field: "mapping_secrets", Code: "max_properties",
			Message: "a connection seals at most " + strconv.Itoa(domain.MaxMappingSecrets) + " mapping secrets"})
	}
	for _, name := range secretNames(secrets) {
		switch v := secrets[name]; {
		case !domain.ValidSecretName(name):
			violations = append(violations, errs.Violation{Field: "mapping_secrets/" + name, Code: "pattern",
				Message: "a secret name is lower-case letters, digits and underscores, starting with a letter"})
		case strings.TrimSpace(v) == "":
			violations = append(violations, errs.Violation{Field: "mapping_secrets/" + name, Code: "required",
				Message: "a secret needs a value"})
		case len(v) > domain.MaxMappingSecretBytes:
			violations = append(violations, errs.Violation{Field: "mapping_secrets/" + name, Code: "max_length",
				Message: "a secret is at most " + strconv.Itoa(domain.MaxMappingSecretBytes) + " bytes"})
		}
	}
	if len(violations) > 0 {
		return errs.Validation("validation_failed",
			strconv.Itoa(len(violations))+" field(s) failed validation.", violations...)
	}
	if !hasMapping {
		return nil
	}
	return service.ValidateMapping(mapping, names)
}

// secretNames is a supplied set of mapping secrets' names, sorted — the half of it
// that is stored in the clear.
func secretNames(secrets map[string]string) []string {
	names := make([]string, 0, len(secrets))
	for n := range secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// replaceMappingSecrets seals a connection's mapping secrets as ONE credential —
// re-sealing the existing row in place, so the slot never points at nothing — and
// returns the slot's new value. nil secrets leave the slot as it is; an empty set
// detaches it.
//
// ⛔ THE VALUES GO FROM THE DECODED DTO STRAIGHT INTO THE SEALER, as every credential
// in this file does. Only their names are kept in the clear.
func (rt *Router) replaceMappingSecrets(
	ctx context.Context, scope db.TenantScope, existing *uuid.UUID, secrets map[string]string,
) (*uuid.UUID, error) {
	if secrets == nil {
		return existing, nil
	}
	if len(secrets) == 0 {
		return nil, nil
	}
	if rt.creds == nil {
		return nil, errs.Unavailable("channels_credential_store_unavailable",
			"credentials cannot be sealed in this deployment", 0)
	}
	if existing != nil {
		if err := rt.creds.RotateCredential(ctx, scope, *existing, domain.MappingSecretsKind, secrets); err != nil {
			return nil, err
		}
		return existing, nil
	}
	id, err := rt.creds.CreateCredential(ctx, scope, domain.MappingSecretsKind, secrets)
	if err != nil {
		return nil, err
	}
	return &id, nil
}
