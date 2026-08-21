package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// listChannelTypes serves GET /api/v1/channel-types.
//
// ⛔ THIS IS WHAT MAKES THE SETTINGS UI DYNAMIC. Each descriptor carries the
// provider's JSON Schema verbatim — the SAME BYTES `createChannel` validates
// against — so the form is generated from the schema and the UI has no
// per-provider code. Adding a provider is a schema file and a registration, and
// nothing in `web/` changes.
//
// It is unpaginated on purpose: the provider set is fixed at boot and v1 ships
// exactly two.
func (rt *Router) listChannelTypes(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	if _, err := scopeOf(r); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.registry != nil, "channels_registry_unavailable",
		"no channel providers are registered in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	descriptors := rt.registry.Descriptors()
	out := make([]ChannelTypeDTO, 0, len(descriptors))
	for _, d := range descriptors {
		out = append(out, descriptorDTO(d))
	}
	// ⚠️ The SINGLE-RESOURCE envelope carrying an array, not the paged list
	// envelope. `ChannelTypeListResponse` is `additionalProperties: false` and
	// requires exactly `data` and `meta`: emitting a `page` object here would be
	// a contract violation, and there is nothing to page anyway — the provider set
	// is fixed at boot.
	httpx.Data(w, r, http.StatusOK, out, started)
}

// listChannels serves GET /api/v1/channels.
func (rt *Router) listChannels(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.channels != nil, "channels_store_unavailable",
		"the channel store is not configured in this deployment"); err != nil {
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
	cursor, err := httpx.DecodeCursor(p.Cursor(), httpx.FilterHash("channels"))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// Soft-deleted destinations are excluded: the settings screen is about what is
	// configured now. Their delivery history remains readable through the
	// Notification tag, which is where "who was told" belongs.
	instances, next, err := rt.channels.List(r.Context(), scope, false, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]ChannelDTO, 0, len(instances))
	for _, i := range instances {
		out = append(out, channelDTO(i))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// createChannel serves POST /api/v1/channels.
//
// ⛔ `config` IS VALIDATED AGAINST THE PROVIDER'S PUBLISHED JSON SCHEMA, on the
// server, every time. The UI validating from the same schema is a courtesy, not a
// guarantee: a client is not a trust boundary, and `curl` is a client. Schema
// failures come back as ordinary field-level violations with the JSON Pointer as
// `field`, so pasting `#sre-alerts` where a channel ID belongs highlights that
// exact control with a message derived from the schema the form was built from.
func (rt *Router) createChannel(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.requireWriteDeps(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// The RAW bytes, before Bind consumes the stream: "the same body" is decided
	// by the sha256 of what the caller actually sent, not by a re-encoding of the
	// DTO it parsed into.
	raw, err := httpx.ReadBody(w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[CreateChannelRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	idem, err := idempotencyIntent(r, idempotency.HashRequest(raw))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	kind := domain.Type(dto.Type)
	if err := rt.validateConfig(r.Context(), kind, dto.Config); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.checkRenderer(kind, dto.Renderer); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.checkConnectionType(r.Context(), scope, kind, dto.ConnectionID); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	inst, err := rt.writes.CreateChannel(r.Context(), scope,
		dto.toNewInstance(rt.capabilitiesOf(kind)), idem)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, channelDTO(inst), started)
}

// getChannel serves GET /api/v1/channels/{id}.
func (rt *Router) getChannel(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.channels != nil, "channels_store_unavailable",
		"the channel store is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	inst, err := rt.channels.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if inst.Deleted() {
		httpx.WriteProblem(w, r, errs.NotFound("channel_deleted", "this channel has been deleted"))
		return
	}
	httpx.Data(w, r, http.StatusOK, channelDTO(inst), started)
}

// updateChannel serves PATCH /api/v1/channels/{id}.
//
// Supplying `config` REPLACES it wholesale and re-validates against the provider
// schema. A merge would be worse: half a config that validated in pieces can be
// invalid as a whole, and an operator editing a form expects to be saving what
// they see.
func (rt *Router) updateChannel(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.requireWriteDeps(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[UpdateChannelRequest](w, r)
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

	existing, err := rt.channels.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if existing.Deleted() {
		httpx.WriteProblem(w, r, errs.NotFound("channel_deleted", "this channel has been deleted"))
		return
	}

	if dto.Config != nil {
		if err := rt.validateConfig(r.Context(), existing.Type, *dto.Config); err != nil {
			httpx.WriteProblem(w, r, err)
			return
		}
	}
	if err := rt.checkRenderer(existing.Type, dto.Renderer); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	// A supplied connection_id re-points this destination — checkConnectionType
	// refuses one whose provider does not match, the cross-table invariant no
	// CHECK constraint can see.
	if dto.ConnectionID != nil {
		if err := rt.checkConnectionType(r.Context(), scope, existing.Type, *dto.ConnectionID); err != nil {
			httpx.WriteProblem(w, r, err)
			return
		}
	}

	inst, err := rt.channels.Update(r.Context(), scope, id, dto.toPatch())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, channelDTO(inst), started)
}

// deleteChannel serves DELETE /api/v1/channels/{id}.
//
// ⛔ A channel still referenced by an ENABLED policy is a `409`, never a cascade.
// Silently orphaning a policy's only destination would make it stop notifying
// without saying so, which is exactly the invisible silence §B.6 forbids — and
// the error names the offending policies so the operator can fix them rather than
// go looking.
//
// Existing threads and delivery history are retained, because the record of who
// was told, when, is the point.
func (rt *Router) deleteChannel(w http.ResponseWriter, r *http.Request) {
	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.channels != nil, "channels_store_unavailable",
		"the channel store is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	policies, err := rt.channels.ReferencingPolicies(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if len(policies) > 0 {
		httpx.WriteProblem(w, r, errs.Conflict("channel_in_use",
			"this channel is still a destination of an enabled notification policy: "+
				strings.Join(policies, ", ")))
		return
	}

	if err := rt.channels.SoftDelete(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusNoContent, nil)
}

// testChannel serves POST /api/v1/channels/{id}/test.
//
// ⛔ BOUNDED by TestTimeout: the test sends, and sending is somebody else's
// server. A blown deadline is a `504`.
//
// A `200` with `ok: false` means the test RAN and the provider rejected it. That
// is the useful answer, and turning it into a 502 would throw away `error_class`
// — the field that distinguishes "Slack is flaky" from "your token was revoked".
//
// ⭐⭐ A RETRY CARRYING THE SAME `Idempotency-Key` DOES NOT SEND A SECOND CARD.
// This was the sharpest unfixed operation in ticket a6cc834: it called the tester
// unconditionally on every request, with no dedup of any kind, so every retry —
// deliberate, or one a client library made on its own after a dropped response —
// put a second real alert card into a customer's own Slack workspace or webhook.
// The UI did not send a key either, so the endpoint was unprotected at both ends.
//
// ⛔ THE SUBJECT IS THE HASH. This request has no body, so `HashRequest(nil)` is a
// CONSTANT and every test would digest identically; a client that mints one key
// per gesture and tested channel A then channel B under it would be told its
// second test was a replay of the first. `HashTargetedRequest` makes the two the
// different requests they are — and an honest retry against the same channel
// still digests identically and still replays.
func (rt *Router) testChannel(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.writes != nil, "channels_tester_unavailable",
		"channel testing is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	idem, err := idempotencyIntent(r, idempotency.HashTargetedRequest(id, nil))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), TestTimeout)
	defer cancel()

	res, err := rt.writes.TestChannel(ctx, scope, id, idem)
	if err != nil {
		httpx.WriteProblem(w, r, timeoutAware(ctx, r, err, "channel_test_timeout",
			"the destination did not answer within the test budget"))
		return
	}
	httpx.Data(w, r, http.StatusOK, testDTO(res), started)
}

// ------------------------------------------------------------------- helpers

func (rt *Router) requireWriteDeps() error {
	if err := requireDependency(rt.channels != nil, "channels_store_unavailable",
		"the channel store is not configured in this deployment"); err != nil {
		return err
	}
	if err := requireDependency(rt.writes != nil, "channels_store_unavailable",
		"the channel store is not configured in this deployment"); err != nil {
		return err
	}
	if err := requireDependency(rt.connections != nil, "channels_connections_store_unavailable",
		"the connection store is not configured in this deployment"); err != nil {
		return err
	}
	return requireDependency(rt.registry != nil, "channels_registry_unavailable",
		"no channel providers are registered in this deployment")
}

// checkConnectionType refuses a connection whose provider does not match a
// channel's own type, and one that has been deleted.
//
// ⛔ THIS IS THE CROSS-TABLE INVARIANT NO CHECK CONSTRAINT CAN SEE. A Slack
// channel pointed at a webhook connection would carry a bot-token credential
// that means nothing to the webhook provider — the same shape of defect
// `checkRenderer` catches for a cross-provider renderer, one hop further out.
func (rt *Router) checkConnectionType(
	ctx context.Context, scope db.TenantScope, kind domain.Type, connectionID uuid.UUID,
) error {
	conn, err := rt.connections.Get(ctx, scope, connectionID)
	if err != nil {
		return err
	}
	if conn.Deleted() {
		return errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{
				Field: "connection_id", Code: "deleted",
				Message: "this connection has been deleted",
			})
	}
	if conn.Type != kind {
		return errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{
				Field: "connection_id", Code: "type_mismatch",
				Message: "this connection is a " + string(conn.Type) +
					" connection and cannot back a " + string(kind) + " channel",
			})
	}
	return nil
}

// validateConfig runs layer 4 (§L.5).
//
// The registry maps every schema failure onto an `errs.Violation` whose `field`
// is the JSON Pointer from the failing instance location, so the response points
// at a control. The violations are re-rooted under `config/` here because the
// schema knows nothing about the envelope it arrived in, and a violation reported
// at `/conversation_id` would not find the form field at `config.conversation_id`.
func (rt *Router) validateConfig(ctx context.Context, t domain.Type, raw json.RawMessage) error {
	if len(raw) == 0 {
		return errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{Field: "config", Code: "required", Message: "config is required"})
	}
	err := rt.registry.ValidateConfig(ctx, t, raw)
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

// checkRenderer refuses a renderer the provider does not declare.
//
// `channels_rend_ck` would catch a wholly unknown string, but not a VALID
// renderer belonging to the OTHER provider: `webhook.json` on a Slack channel
// passes the CHECK and then renders a JSON envelope into Block Kit. That is a
// cross-provider mismatch only the registry can see.
func (rt *Router) checkRenderer(t domain.Type, renderer *string) error {
	if renderer == nil || *renderer == "" || *renderer == "default" {
		return nil
	}
	for _, d := range rt.registry.Descriptors() {
		if d.Type != t {
			continue
		}
		for _, id := range d.Renderers {
			if string(id) == *renderer {
				return nil
			}
		}
	}
	return errs.Validation("validation_failed", "1 field failed validation.",
		errs.Violation{
			Field: "renderer", Code: "enum",
			Message: "this renderer does not belong to the " + string(t) + " provider",
		})
}

// capabilitiesOf reads the provider's declared capability bits.
//
// They are stored on the row so the dispatcher can negotiate without consulting
// the registry on every delivery, and they are taken from the DESCRIPTOR rather
// than from the request: capabilities are a property of the provider, and a
// client that could assert them could claim threading on a webhook.
func (rt *Router) capabilitiesOf(t domain.Type) domain.Capability {
	for _, d := range rt.registry.Descriptors() {
		if d.Type == t {
			return d.Capabilities
		}
	}
	return 0
}

// sealCredential stores a supplied credential and returns its id, or nil.
//
// ⛔ The plaintext values travel from the decoded DTO straight into the sealer and
// are referenced nowhere else. Nothing in this file logs `dto.Credential`.
func (rt *Router) sealCredential(
	ctx context.Context, scope db.TenantScope, in *CredentialInputDTO,
) (*uuid.UUID, error) {
	if in == nil || in.Kind == "" || in.Kind == "none" {
		return nil, nil
	}
	if rt.creds == nil {
		return nil, errs.Unavailable("channels_credential_store_unavailable",
			"credentials cannot be sealed in this deployment", 0)
	}
	if len(in.Values) == 0 {
		return nil, errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{Field: "credential/values", Code: "required", Message: "at least one value is required"})
	}
	id, err := rt.creds.CreateCredential(ctx, scope, in.Kind, in.Values)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// rotateCredential re-seals an existing credential in place, or seals a new one
// when the channel had none.
func (rt *Router) rotateCredential(
	ctx context.Context, scope db.TenantScope, existing *uuid.UUID, in *CredentialInputDTO,
) (*uuid.UUID, error) {
	if in == nil {
		return existing, nil
	}
	if in.Kind == "none" {
		return nil, nil
	}
	if existing == nil {
		return rt.sealCredential(ctx, scope, in)
	}
	if rt.creds == nil {
		return nil, errs.Unavailable("channels_credential_store_unavailable",
			"credentials cannot be sealed in this deployment", 0)
	}
	if len(in.Values) == 0 {
		return nil, errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{Field: "credential/values", Code: "required", Message: "at least one value is required"})
	}
	if err := rt.creds.RotateCredential(ctx, scope, *existing, in.Kind, in.Values); err != nil {
		return nil, err
	}
	return existing, nil
}

// timeoutAware turns a blown LOCAL deadline into a `504`, while leaving a client
// disconnect and every other failure exactly as the port reported them.
//
// The distinction matters: `504` says "the destination was too slow", which is
// actionable, whereas the underlying `context.DeadlineExceeded` would surface as
// a `500` and blame oto for somebody else's outage.
func timeoutAware(ctx context.Context, r *http.Request, err error, code, message string) error {
	if ctx.Err() != nil && r.Context().Err() == nil {
		return errs.UpstreamSlow(code, message, err)
	}
	return err
}
