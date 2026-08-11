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
	"github.com/thulasiram/oto/internal/platform/netguard"
	"github.com/thulasiram/oto/internal/platform/validate"
	"github.com/thulasiram/oto/internal/sources/domain"
)

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
	if err := requireDependency(rt.tokens != nil, "sources_token_issuer_unavailable",
		"ingest tokens cannot be minted in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[CreateSourceRequest](w, r)
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

	var (
		src    domain.Source
		secret string
		prefix string
	)
	err = rt.inTx(r.Context(), func(ctx context.Context) error {
		credentialID, cerr := rt.sealCredential(ctx, scope, dto.Credential)
		if cerr != nil {
			return cerr
		}
		created, cerr := rt.registry.Create(ctx, scope, dto.toDraft(credentialID))
		if cerr != nil {
			return cerr
		}
		s, p, terr := rt.tokens.IssueIngestToken(ctx, scope, created.ID)
		if terr != nil {
			return terr
		}
		src, secret, prefix = created, s, p
		return nil
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	body := SourceCreatedDTO{
		// Rendered AFTER the commit: the health and cluster joins are reads, and a
		// read inside the writing transaction would see a world nobody else can.
		Source:      rt.oneDTO(r.Context(), scope, src),
		IngestToken: secret,
		TokenPrefix: prefix,
		WebhookURL:  rt.webhookURL(ingestPath(src.ID)),
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

	var src domain.Source
	err = rt.inTx(r.Context(), func(ctx context.Context) error {
		// A supplied credential ROTATES the existing secret in place when there is
		// one, so the source never spends a moment pointing at nothing.
		var credential **uuid.UUID
		if dto.Credential != nil {
			existing, gerr := rt.sources.Get(ctx, scope, id)
			if gerr != nil {
				return gerr
			}
			newID, cerr := rt.rotateCredential(ctx, scope, existing.AuthCredentialID, dto.Credential)
			if cerr != nil {
				return cerr
			}
			credential = &newID
		}
		updated, uerr := rt.registry.Update(ctx, scope, id, dto.toPatch(credential))
		if uerr != nil {
			return uerr
		}
		src = updated
		return nil
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
// deleting a source must never erase the record of what it once reported.
//
// ⭐ THE DELETION AND THE REVOCATION COMMIT TOGETHER OR NOT AT ALL, for the same
// reason `createSource` does: they were two independent commits, and a failure in
// the second left a source that the settings screen shows as gone while its
// ingest token stayed live and usable — a soft delete in name only, and a
// credential nobody can see in order to revoke it.
//
// THE ORDER IS DELIBERATE. The soft delete goes first because it is the call that
// decides the response: it answers not-found for an id that does not exist or is
// already deleted, and running it first keeps that 404 free of any write against
// another source's tokens. It also takes `alert_sources` before `api_tokens`,
// which is the lock order the create path already uses.
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

	err = rt.inTx(r.Context(), func(ctx context.Context) error {
		if derr := rt.registry.SoftDelete(ctx, scope, id); derr != nil {
			return derr
		}
		if rt.tokens == nil {
			return nil
		}
		return rt.tokens.RevokeIngestTokens(ctx, scope, id)
	})
	if err != nil {
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
// the receiver promptly and why nothing here delays the revocation to be kind.
//
// ⛔ THE ONE THING IT MUST NEVER DO IS LEAVE ZERO WORKING TOKENS. The issuer used
// to revoke first and mint second; a mint that failed for any reason therefore
// revoked the source's only credential and left nothing in its place, and because
// Alertmanager never retries a 401 the alerts sent afterwards were destroyed
// rather than delayed — the precise failure ADR 0007 exists to prevent. The whole
// rotation is now one transaction and mints before it revokes, so the failure
// mode is "nothing changed" instead of "nothing works".
func (rt *Router) rotateSourceIngestToken(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.tokens != nil, "sources_token_issuer_unavailable",
		"ingest tokens cannot be minted in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	var (
		src            domain.Source
		secret, prefix string
	)
	err = rt.inTx(r.Context(), func(ctx context.Context) error {
		found, gerr := rt.sources.Get(ctx, scope, id)
		if gerr != nil {
			return gerr
		}
		s, p, terr := rt.tokens.IssueIngestToken(ctx, scope, found.ID)
		if terr != nil {
			return terr
		}
		src, secret, prefix = found, s, p
		return nil
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	httpx.Data(w, r, http.StatusOK, SourceCreatedDTO{
		Source:      rt.oneDTO(r.Context(), scope, src),
		IngestToken: secret,
		TokenPrefix: prefix,
		WebhookURL:  rt.webhookURL(ingestPath(src.ID)),
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

// sealCredential stores a supplied credential and returns its id, or nil.
//
// ⛔ The plaintext values travel from the decoded DTO straight into the sealer
// and are referenced nowhere else. Nothing in this file logs `dto.Credential`.
func (rt *Router) sealCredential(
	ctx context.Context, scope db.TenantScope, in *CredentialInputDTO,
) (*uuid.UUID, error) {
	if in == nil || in.Kind == "" || in.Kind == "none" {
		return nil, nil
	}
	if rt.creds == nil {
		return nil, errs.Unavailable("sources_credential_store_unavailable",
			"credentials cannot be sealed in this deployment", 0)
	}
	if len(in.Values) == 0 {
		return nil, errs.Validation("credential_empty", "a credential must carry at least one value",
			errs.Violation{Field: "credential/values", Code: "required", Message: "at least one value is required"})
	}
	id, err := rt.creds.CreateCredential(ctx, scope, in.Kind, in.Values)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// rotateCredential re-seals an existing credential in place, or seals a new one
// when the source had none.
func (rt *Router) rotateCredential(
	ctx context.Context, scope db.TenantScope, existing *uuid.UUID, in *CredentialInputDTO,
) (*uuid.UUID, error) {
	if in == nil {
		return existing, nil
	}
	if in.Kind == "none" {
		// Detaching is expressed by supplying kind `none`: the row is left in place
		// (other things may reference it) and the source stops pointing at it.
		return nil, nil
	}
	if existing == nil {
		return rt.sealCredential(ctx, scope, in)
	}
	if rt.creds == nil {
		return nil, errs.Unavailable("sources_credential_store_unavailable",
			"credentials cannot be sealed in this deployment", 0)
	}
	if len(in.Values) == 0 {
		return nil, errs.Validation("credential_empty", "a credential must carry at least one value",
			errs.Violation{Field: "credential/values", Code: "required", Message: "at least one value is required"})
	}
	if err := rt.creds.RotateCredential(ctx, scope, *existing, in.Kind, in.Values); err != nil {
		return nil, err
	}
	return existing, nil
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
