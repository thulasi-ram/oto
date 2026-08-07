package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/ingestion/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// IngestService is the port this handler declares for itself. It is satisfied by
// *service.Service.
type IngestService interface {
	Accept(ctx context.Context, s db.TenantScope, cmd service.AcceptCommand) (service.AcceptResult, error)
	RecordBodyTooLarge(ctx context.Context, s db.TenantScope, sourceID uuid.UUID, bytes int64)
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ IngestService = (*service.Service)(nil)

// SourceIDParam is the chi path parameter naming the AlertSource.
const SourceIDParam = "source_id"

// Router mounts the ingest surface: one route, and deliberately only one.
type Router struct {
	svc     IngestService
	auth    *Authenticator
	shed    *Shedder
	clk     clock.Clock
	log     *slog.Logger
	metrics *service.Metrics
}

// NewRouter builds the ingest router.
func NewRouter(
	svc IngestService, auth *Authenticator, shed *Shedder,
	clk clock.Clock, logger *slog.Logger, metrics *service.Metrics,
) *Router {
	if clk == nil {
		clk = clock.New()
	}
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = service.NewMetrics(nil)
	}
	return &Router{svc: svc, auth: auth, shed: shed, clk: clk, log: logger, metrics: metrics}
}

// Mount registers POST /ingest/alertmanager/{source_id} on r.
//
// ⚠️ THIS ROUTE MUST NOT SIT BEHIND THE GLOBAL `MaxBody` MIDDLEWARE, and it must
// not sit behind a request timeout shorter than the accept budget. The body cap
// here is B1 (8 MiB), which is larger than the API-wide default, and the handler
// applies it itself so that an oversized body produces a RECORDED 413 rather than
// a bare one from a middleware that cannot write an `ingest_rejections` row.
//
// It should also be mounted on a chi.Group carrying no session/PAT
// authentication: the ingest token is the only credential this route accepts, and
// a session cookie must never reach it.
func (rt *Router) Mount(r chi.Router) {
	r.Post("/ingest/alertmanager/{"+SourceIDParam+"}", rt.webhook)
}

// webhook is POST /api/v1/ingest/alertmanager/{source_id} (SPEC §G.1).
//
// ⭐ THE RESPONSE CONTRACT (§G.2, C4, ADR 0007), which is the single most
// consequential rule in oto:
//
//	durably persisted, or a duplicate    202
//	overload, pool exhaustion, timeout,
//	  slow Postgres, failed enqueue      503 + Retry-After
//	missing / invalid / wrongly-scoped
//	  ingest token                       401
//	body over 8 MiB (B1)                 413, recorded
//	body that cannot decode (B16)        400, recorded
//	ANYTHING TRANSIENT                   NEVER 4xx. NEVER 429.
//
// Alertmanager retries 5xx and ONLY 5xx. A 4xx — 429 included — makes it discard
// the notification permanently and silently, during exactly the window when the
// customer's cluster is on fire. The three 4xx above are permitted because all
// three are genuinely permanent: the same bytes with the same token could not
// succeed on a retry, so no retry is being denied. Every one of them is still
// written to `ingest_rejections`, so nothing disappears without a trace.
//
// Per-alert problems are not in that table at all. A bad label, a missing
// alertname, a timestamp from 2087: recorded, the rest of the batch processed,
// still 202.
func (rt *Router) webhook(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()
	ctx := r.Context()

	sourceID, err := uuid.Parse(chi.URLParam(r, SourceIDParam))
	if err != nil {
		// 401 rather than 404: no token can be scoped to a source id that is not a
		// uuid, and a 404 would tell an unauthenticated caller which ids exist.
		httpx.Error(w, r, unauthorized())
		return
	}

	scope, _, err := rt.auth.Authenticate(r, sourceID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Backpressure BEFORE the body is read. Shedding after reading eight megabytes
	// has already spent the resource the shed was meant to protect.
	release, err := rt.shed.Enter(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return // the client hung up; there is nobody to answer
		}
		httpx.Error(w, r, err)
		return
	}
	defer release()

	body, err := rt.readBody(r, scope, sourceID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	res, err := rt.svc.Accept(ctx, scope, service.AcceptCommand{
		SourceID: sourceID,
		Body:     body,
		Mode:     domain.ModePush,
	})
	if err != nil {
		rt.logFailure(ctx, sourceID, err)
		httpx.Error(w, r, err)
		return
	}

	httpx.Data(w, r, http.StatusAccepted, acceptedDTO(res), started)
}

// readBody reads the request body under bound B1.
//
// The cap is applied with an io.LimitReader of MaxBodyBytes+1 rather than
// http.MaxBytesReader: MaxBytesReader writes its own 413 with no body and no
// recorded rejection, and this path owes the operator a row in
// `ingest_rejections` explaining what arrived.
//
// A read that fails mid-stream is NOT a client error. A dropped connection during
// upload is transient by definition, so it is 503 with a Retry-After — the
// upstream still holds the notification and can send it again.
func (rt *Router) readBody(r *http.Request, scope db.TenantScope, sourceID uuid.UUID) ([]byte, error) {
	limited := io.LimitReader(r.Body, domain.MaxBodyBytes+1)

	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindUnavailable, "ingest_body_unreadable",
			"the request body could not be read; retry").WithRetryAfter(domain.RetryAfter)
	}

	if int64(len(body)) > domain.MaxBodyBytes {
		// Recorded before the 413 is written, because the row is the whole reason a
		// permanent refusal is defensible here.
		rt.svc.RecordBodyTooLarge(r.Context(), scope, sourceID, int64(len(body)))
		rt.log.WarnContext(r.Context(), "ingest: body over the B1 limit",
			"source_id", sourceID, "limit", domain.MaxBodyBytes)
		return nil, errs.TooLarge(service.CodeBodyTooLarge,
			"the request body exceeds the 8 MiB ingest limit")
	}

	// An empty body is deliberately NOT short-circuited here. It is undecodable,
	// and undecodable bodies are recorded in `ingest_rejections` by the service —
	// answering 400 from the transport would skip the row that makes the 400
	// defensible.
	return body, nil
}

// logFailure records an accept failure at the right level: a 503 is operational
// and belongs at warn, a bad payload is the upstream's and belongs at info. The
// PAYLOAD IS NEVER LOGGED — not at info, not at any level (§L.3.3).
func (rt *Router) logFailure(ctx context.Context, sourceID uuid.UUID, err error) {
	attrs := []any{"source_id", sourceID, "code", errs.CodeOf(err), "error", err.Error()}
	if errs.IsKind(err, errs.KindUnavailable) {
		rt.log.WarnContext(ctx, "ingest: shedding, batch not accepted", attrs...)
		return
	}
	rt.log.InfoContext(ctx, "ingest: batch refused", attrs...)
}
