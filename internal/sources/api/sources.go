package api

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/idempotency"
	"github.com/thulasiram/oto/internal/platform/netguard"
	"github.com/thulasiram/oto/internal/platform/validate"
	"github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/service"
)

// idempotencyIntent reads the caller's `Idempotency-Key` into the intent the
// write facade acts on (see idempotency.IntentFromRequest for the seam's rules).
// The hash is passed in because what "the same request" means differs by
// operation: a create is identified by the RAW bytes it sent, and a rotation —
// which has no body — by the source whose credential it replaces, which the
// service folds in itself.
func (rt *Router) idempotencyIntent(
	r *http.Request, hash idempotency.RequestHash,
) (service.Idempotency, error) {
	return idempotency.IntentFromRequest(r, hash)
}

// listSources serves GET /api/v1/sources.
//
// Each row carries its current health, resolved for the whole page in one query.
// Health beside the row is not decoration: it is what tells an operator that the
// reason they have seen no alerts from prod-eu for two hours is that oto cannot
// reach it (§B.4).
func (rt *Router) listSources(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	keyset, limit, err := page(r, httpx.FilterHash("sources"))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	sources, cursor, err := rt.sources.List(r.Context(), scope, domain.SourceFilter{}, keyset)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out, err := rt.decorate(r.Context(), scope, sources)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.List(w, r, out, httpx.PageOf(cursor, limit), started)
}

// decorate joins cluster keys and health onto a page of sources.
//
// Two queries for the whole page, never two per row: a settings screen with
// twenty upstreams must not be forty-one round trips.
func (rt *Router) decorate(
	ctx context.Context, scope db.TenantScope, sources []domain.Source,
) ([]SourceDTO, error) {
	out := make([]SourceDTO, 0, len(sources))
	if len(sources) == 0 {
		return out, nil
	}

	ids := make([]uuid.UUID, 0, len(sources))
	clusterIDs := make([]uuid.UUID, 0, len(sources))
	for _, s := range sources {
		ids = append(ids, s.ID)
		clusterIDs = append(clusterIDs, s.ClusterID)
	}

	health := map[uuid.UUID]domain.SourceHealth{}
	if rt.registry != nil {
		h, err := rt.registry.HealthFor(ctx, scope, ids)
		if err != nil {
			return nil, err
		}
		health = h
	}
	keys := map[uuid.UUID]string{}
	if rt.clusters != nil {
		k, err := rt.clusters.ClusterKeysFor(ctx, scope, clusterIDs)
		if err != nil {
			return nil, err
		}
		keys = k
	}

	for _, s := range sources {
		h, ok := health[s.ID]
		if !ok {
			// A source that has never been probed is `unknown`, and `unknown`
			// blocks the reaper. Rendering it as absent would hide exactly the
			// state an operator most needs to see on a freshly added source.
			h = domain.SourceHealth{SourceID: s.ID, OrgID: s.OrgID, Status: domain.HealthUnknown}
		}
		out = append(out, sourceDTO(s, keys[s.ClusterID], &h))
	}
	return out, nil
}

// createSource serves POST /api/v1/sources.
//
// The ingest token is minted here and RETURNED EXACTLY ONCE. Only its sha256 is
// stored, so it can be replaced but never recovered.
//
// ⭐ THE SOURCE AND ITS CREDENTIAL COMMIT TOGETHER OR NOT AT ALL. They used to be
// three independent commits — seal the credential, insert the row, mint the token
// — and when the mint failed the row stayed. The result was a source that the
// settings screen shows as configured, whose webhook URL an operator has already
// pasted into `webhook_config`, and which answers 401 to every alert forever.
// Alertmanager does not retry a 4xx, so those alerts are simply gone.
//
// ⭐⭐ A RETRY CARRYING THE SAME `Idempotency-Key` IS REFUSED, NOT REPEATED, for
// the same reason `createApiToken` is: this 201 hands out a plaintext ingest
// token exactly once. This endpoint DECLARED the header and read it nowhere, and
// was safe only by accident — `alert_sources_name_uniq (org_id, name)` happens to
// refuse a second create under the same name, which protects nothing the moment a
// client generates the name or an operator retries with a different one. The
// claim is taken in the same transaction as the mint, so a key somebody already
// holds rolls the whole create back and the caller is told the id of the source
// its first attempt made.
func (rt *Router) createSource(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.registry != nil, "sources_registry_unavailable",
		"the source registry is not configured in this deployment"); err != nil {
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

	dto, err := httpx.Bind[CreateSourceRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	idem, err := rt.idempotencyIntent(r, idempotency.HashRequest(raw))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	if err := rt.checkTLSSkipVerify(dto.TLSSkipVerify); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.checkTargets(r.Context(), map[string]string{
		"base_url":       dto.BaseURL,
		"prometheus_url": dto.PrometheusURL,
	}); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	issued, err := rt.registry.Create(r.Context(), scope, service.CreateCommand{
		// The draft carries no credential id: sealing the secret is part of the
		// same transaction as the insert, so the id does not exist yet and this
		// layer must not pretend to know it.
		Draft:       dto.toDraft(nil),
		Credential:  dto.Credential.toInput(),
		Idempotency: idem,
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	body := SourceCreatedDTO{
		// Rendered AFTER the commit: the health and cluster joins are reads, and a
		// read inside the writing transaction would see a world nobody else can.
		Source:      rt.oneDTO(r.Context(), scope, issued.Source),
		IngestToken: issued.Secret,
		TokenPrefix: issued.Prefix,
		WebhookURL:  rt.webhookURL(ingestPath(issued.Source.ID)),
	}
	httpx.Data(w, r, http.StatusCreated, body, started)
}

// getSource serves GET /api/v1/sources/{id}.
func (rt *Router) getSource(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	src, err := rt.sources.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, rt.oneDTO(r.Context(), scope, src), started)
}

// updateSource serves PATCH /api/v1/sources/{id}.
func (rt *Router) updateSource(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.registry != nil, "sources_registry_unavailable",
		"the source registry is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[UpdateSourceRequest](w, r)
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
	// `prometheus_url` carries its own bound because a custom unmarshaller has no
	// field for a validator tag to hang on. The path reported is the JSON name the
	// caller sent, exactly as §L.2.2 requires.
	if dto.PrometheusURL.Supplied() && !validate.IsAbsoluteHTTPURL(dto.PrometheusURL.Value) {
		httpx.WriteProblem(w, r, errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{
				Field: "prometheus_url", Code: "url",
				Message: "prometheus_url must be an absolute http(s) URL with no trailing slash, query string or fragment",
			}))
		return
	}

	if err := rt.checkTLSSkipVerify(dto.TLSSkipVerify); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	targets := map[string]string{}
	if dto.BaseURL != nil {
		targets["base_url"] = *dto.BaseURL
	}
	if dto.PrometheusURL.Supplied() {
		targets["prometheus_url"] = dto.PrometheusURL.Value
	}
	if err := rt.checkTargets(r.Context(), targets); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// The patch carries no credential id: whether a supplied secret rotates the
	// existing row in place or seals a new one is the service's decision, made
	// inside the same transaction as the update.
	src, err := rt.registry.Update(r.Context(), scope, id, service.UpdateCommand{
		Patch:      dto.toPatch(nil),
		Credential: dto.Credential.toInput(),
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, rt.oneDTO(r.Context(), scope, src), started)
}

// deleteSource serves DELETE /api/v1/sources/{id}.
//
// It stops ingestion and reconciliation and REVOKES the ingest token, so a source
// that has been deleted cannot still be pushed to. ALERT HISTORY IS RETAINED:
// deleting a source must never erase the record of what it once reported. Both
// writes commit together or not at all — see `service.SoftDelete`, which owns
// that and the order it happens in.
func (rt *Router) deleteSource(w http.ResponseWriter, r *http.Request) {
	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.registry != nil, "sources_registry_unavailable",
		"the source registry is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	if err := rt.registry.SoftDelete(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusNoContent, nil)
}

// testSource serves POST /api/v1/sources/{id}/test.
//
// ⛔ BOUNDED. The probe talks to somebody else's server, so it runs under
// ProbeTimeout derived from the request context. A deadline that expires becomes
// a `504`; a probe that completes and finds the upstream down is a `200` with
// `ok: false`, because the probe itself succeeded in reporting that.
//
// The receiver-level `send_resolved` map is the payload that matters most here: a
// receiver with `send_resolved: false` can never tell oto that an alert ended, so
// every alert routed through it EXPIRES rather than resolves. oto raises a
// standing warning rather than letting an operator discover that months later.
func (rt *Router) testSource(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), ProbeTimeout)
	defer cancel()

	res, err := rt.sources.Probe(ctx, scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, timeoutAware(ctx, r, err, "sources_probe_timeout",
			"the source did not answer within the probe budget"))
		return
	}
	httpx.Data(w, r, http.StatusOK, probeDTO(res, sendResolvedFrom(res)), started)
}

// rotateSourceIngestToken serves POST /api/v1/sources/{id}/rotate-token.
//
// The new secret is returned EXACTLY ONCE and the old one stops working
// immediately. Between rotation and reconfiguration the old token is rejected
// with `401`, which Alertmanager treats as PERMANENT, so notifications sent in
// that window are lost — which is why the contract tells the operator to update
// the receiver promptly and why nothing delays the revocation to be kind.
//
// The mint-before-revoke order, the transaction around them and the
// `Idempotency-Key` claim inside it all live in `service.RotateIngestToken`; this
// handler resolves the subject, reads the header and renders the one response in
// this API that carries a secret.
func (rt *Router) rotateSourceIngestToken(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	// No request hash: this operation declares no body, so the service digests the
	// source it is rotating instead. See `service.Idempotency.RequestHash`.
	idem, err := rt.idempotencyIntent(r, idempotency.RequestHash{})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	issued, err := rt.registry.RotateIngestToken(r.Context(), scope, id, idem)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	httpx.Data(w, r, http.StatusOK, SourceCreatedDTO{
		Source:      rt.oneDTO(r.Context(), scope, issued.Source),
		IngestToken: issued.Secret,
		TokenPrefix: issued.Prefix,
		WebhookURL:  rt.webhookURL(ingestPath(issued.Source.ID)),
	}, started)
}

// reconcileSource serves POST /api/v1/sources/{id}/reconcile.
//
// ⛔ BOUNDED, for the same reason as testSource: one pass reads the whole
// upstream alert set and must not be able to hold an HTTP worker indefinitely. A
// blown deadline is a `504` and NOT a failure of the reconciler — the pass may
// still be running, and the next scheduled tick will report its divergence.
func (rt *Router) reconcileSource(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.reconcile != nil, "sources_reconciler_unavailable",
		"the reconciler is not running in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// Prove the source exists (and is not deleted) before spending a reconcile
	// budget on it, so a bad id is a fast 404 rather than a slow one.
	if _, err := rt.sources.Get(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), ReconcileTimeout)
	defer cancel()

	res, err := rt.reconcile.Reconcile(ctx, scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, timeoutAware(ctx, r, err, "sources_reconcile_timeout",
			"the reconcile pass did not finish within its budget"))
		return
	}
	httpx.Data(w, r, http.StatusOK, reconcileDTO(res), started)
}

// getSourceHealth serves GET /api/v1/sources/{id}/health.
func (rt *Router) getSourceHealth(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	h, err := rt.sources.Health(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if h.SourceID == uuid.Nil {
		h.SourceID = id
	}
	httpx.Data(w, r, http.StatusOK, healthDTO(h), started)
}

// ------------------------------------------------------------------- helpers

// subject resolves the tenant and the path id, and rejects unknown query
// parameters, for every endpoint addressed by `{id}`.
func (rt *Router) subject(r *http.Request) (db.TenantScope, uuid.UUID, error) {
	scope, err := scopeOf(r)
	if err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	return scope, id, nil
}

// oneDTO renders one source with its cluster key and health.
//
// A failure to join either is deliberately NOT fatal: the source itself is real
// and returning it without its health is strictly better than a 500 on a page
// whose job is to show configuration.
func (rt *Router) oneDTO(ctx context.Context, scope db.TenantScope, src domain.Source) SourceDTO {
	var (
		clusterKey string
		health     *domain.SourceHealth
	)
	if rt.clusters != nil {
		if keys, err := rt.clusters.ClusterKeysFor(ctx, scope, []uuid.UUID{src.ClusterID}); err == nil {
			clusterKey = keys[src.ClusterID]
		}
	}
	if h, err := rt.sources.Health(ctx, scope, src.ID); err == nil {
		if h.SourceID == uuid.Nil {
			h.SourceID = src.ID
		}
		health = &h
	}
	return sourceDTO(src, clusterKey, health)
}

// checkTargets refuses a URL that resolves somewhere oto must not dial.
//
// ⚠️ THIS IS FEEDBACK, NOT THE CONTROL. It runs here so an operator who pastes
// `http://169.254.169.254` sees a 422 naming the field while they are still
// looking at the form. The control is the guard installed as the outbound
// transport's dialer, which re-checks the address the socket actually connected
// to — a check performed here and a connection opened later are two independent
// DNS resolutions, and a record served with TTL 0 gets to answer them
// differently.
//
// Every supplied field is checked and every violation reported together: an
// operator who got both URLs wrong should learn that once.
func (rt *Router) checkTargets(ctx context.Context, targets map[string]string) error {
	if rt.guard == nil {
		return nil
	}
	// Sorted so the violation order is stable; a problem+json body whose field
	// order changes between identical requests is a body no test can assert on.
	fields := make([]string, 0, len(targets))
	for field := range targets {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	var violations []errs.Violation
	for _, field := range fields {
		raw := targets[field]
		if raw == "" {
			continue
		}
		err := rt.guard.CheckURL(ctx, raw)
		if err == nil {
			continue
		}
		if netguard.Undecided(err) {
			// The guard could not look the host up. That is NOT permission and it is
			// NOT a refusal: the dialer re-checks the address it actually connects
			// to, every time, so an unresolvable name saved today is refused at the
			// moment it would be dialled. Blocking the save instead would mean a DNS
			// blip — or an operator configuring an in-cluster name from a laptop —
			// makes oto impossible to configure.
			continue
		}
		message := "this URL is not a permitted destination"
		if e, ok := errs.As(err); ok && e.Message != "" {
			message = e.Message
		}
		violations = append(violations, errs.Violation{
			Field: field, Code: "forbidden_target", Message: message,
		})
	}
	if len(violations) == 0 {
		return nil
	}
	return errs.Validation("source_target_not_permitted",
		"this source points at an address oto will not connect to", violations...)
}

// checkTLSSkipVerify refuses a tenant's attempt to turn off certificate
// verification.
//
// ⛔ `tls_skip_verify` IS AN OPERATOR DECISION, NOT A TENANT'S. It disables
// certificate verification on an outbound connection made by oto's process, from
// oto's network — a decision about which certificates this DEPLOYMENT trusts. A
// public create/update body could set it, which meant any org member could
// downgrade oto's own TLS posture and then point the source at something they
// wanted to man-in-the-middle. It is now gated on the deployment-level switch and
// refused otherwise (§M2).
func (rt *Router) checkTLSSkipVerify(requested *bool) error {
	if requested == nil || !*requested || rt.allowNoTLSV {
		return nil
	}
	return errs.Validation("tls_skip_verify_not_permitted",
		"certificate verification is enforced by this deployment",
		errs.Violation{
			Field: "tls_skip_verify", Code: "forbidden",
			Message: "this is a deployment-level setting and cannot be changed per source",
		})
}

// timeoutAware turns a blown local deadline into a `504`, while leaving a client
// disconnect and every other failure exactly as the port reported them.
//
// The distinction matters: a `504` says "the upstream was too slow", which is
// actionable, whereas the underlying `context.DeadlineExceeded` would surface as
// a `500` and blame oto for somebody else's firewall.
func timeoutAware(ctx context.Context, r *http.Request, err error, code, message string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && r.Context().Err() == nil {
		return errs.UpstreamSlow(code, message, err)
	}
	return err
}
