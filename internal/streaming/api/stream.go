package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/streaming/domain"
	"github.com/thulasiram/oto/internal/streaming/service"
)

// StreamService is the port this handler declares for itself. It is satisfied by
// *service.Service.
type StreamService interface {
	Replay(ctx context.Context, scope db.TenantScope, sinceSeq int64) (service.ReplayResult, error)
}

// Hub is the port this handler declares for the live feed. It is satisfied by
// *service.Hub.
type Hub interface {
	Subscribe(orgID uuid.UUID, interest domain.Interest) *service.Subscription
	Unsubscribe(s *service.Subscription)
}

// ScopeResolver produces the caller's tenant scope from the request context.
//
// Declared here, by the consumer, rather than imported from authn: the handler
// needs exactly one fact about the principal — which org it is — and depending on
// the whole identity module for that would invert the layering.
type ScopeResolver interface {
	Scope(ctx context.Context) (db.TenantScope, error)
}

// ScopeResolverFunc adapts a function to ScopeResolver.
type ScopeResolverFunc func(ctx context.Context) (db.TenantScope, error)

// Scope implements ScopeResolver.
func (f ScopeResolverFunc) Scope(ctx context.Context) (db.TenantScope, error) { return f(ctx) }

// Compile-time proof that the service satisfies the ports this layer declares.
var (
	_ StreamService = (*service.Service)(nil)
	_ Hub           = (*service.Hub)(nil)
)

// Router mounts the streaming HTTP surface.
type Router struct {
	svc     StreamService
	hub     Hub
	scopes  ScopeResolver
	clock   clock.Clock
	log     *slog.Logger
	metrics *service.Metrics
}

// NewRouter builds the streaming router.
func NewRouter(
	svc StreamService, hub Hub, scopes ScopeResolver,
	clk clock.Clock, logger *slog.Logger, metrics *service.Metrics,
) *Router {
	if clk == nil {
		clk = clock.New()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Router{svc: svc, hub: hub, scopes: scopes, clock: clk, log: logger, metrics: metrics}
}

// Mount registers GET /stream on r.
//
// ⚠️ THE STREAM ROUTE MUST NOT SIT UNDER A REQUEST-TIMEOUT MIDDLEWARE. A timeout
// cancels the request context, and this handler is meant to live for hours. Mount
// it on a chi.Group that excludes httpx/middleware.Timeout. If it is mounted
// under one anyway the failure is survivable rather than silent — the connection
// ends, EventSource reconnects, and Last-Event-ID resumes with no data loss — but
// it produces a reconnect storm at exactly the timeout interval.
func (rt *Router) Mount(r chi.Router) {
	r.Get("/stream", rt.stream)
}

// stream serves GET /api/v1/stream (SPEC §E.4).
func (rt *Router) stream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	scope, err := rt.scopes.Scope(ctx)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	interest, sinceSeq, err := parseStreamRequest(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// SUBSCRIBE BEFORE REPLAYING. Everything that commits while the replay query
	// runs is then buffered rather than lost, and the live feed is de-duplicated
	// against the replay watermark below. Replay-then-subscribe leaves a hole
	// exactly the width of the query, and it is invisible in testing because the
	// query is fast.
	sub := rt.hub.Subscribe(scope.OrgID(), interest)
	defer rt.hub.Unsubscribe(sub)

	replay, err := rt.svc.Replay(ctx, scope, sinceSeq)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// From here on the status line is committed and errors can only be logged.
	// NewSSEWriter sends the header block and then flushes; if that flush fails
	// the 200 is already on the wire, so a problem+json body would be appended to
	// a text/event-stream response rather than replacing it. Log and hang up.
	sse, err := httpx.NewSSEWriter(w)
	if err != nil {
		rt.log.ErrorContext(ctx, "streaming: response writer cannot stream", "error", err)
		return
	}

	log := rt.log.With(
		"org_id", scope.OrgID(),
		"last_event_id", sinceSeq,
		"replayed", len(replay.Events),
	)

	watermark := replay.LastSeq

	if replay.Resync != "" {
		rt.metrics.ResyncCounter(string(replay.Resync))
		if err := rt.writeResync(sse, scope.OrgID(), watermark, replay.Resync); err != nil {
			return
		}
	}
	for _, e := range replay.Events {
		if err := rt.writeEvent(sse, e); err != nil {
			return
		}
		watermark = e.Seq
	}

	log.InfoContext(ctx, "streaming: client attached")
	rt.pump(ctx, sse, sub, scope.OrgID(), watermark, log)
	log.InfoContext(ctx, "streaming: client detached")
}

// pump is the live phase: batches, heartbeats and shutdown.
func (rt *Router) pump(
	ctx context.Context, sse *httpx.SSEWriter, sub *service.Subscription,
	orgID uuid.UUID, watermark int64, log *slog.Logger,
) {
	heartbeat := time.NewTicker(service.HeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-heartbeat.C:
			// A bare comment line. Not a frame, no data, does not move the
			// client's cursor — but it keeps proxies from reaping an idle
			// connection AND makes a vanished peer produce a write error within
			// one interval instead of never.
			if err := sse.Comment("ping"); err != nil {
				return
			}

		case batch, ok := <-sub.Batches():
			if !ok {
				// The hub detached us: a clean shutdown. Ending the response is
				// the polite version — the client reconnects and resumes.
				return
			}

			if batch.Resync != "" {
				rt.metrics.ResyncCounter(string(batch.Resync))
				log.WarnContext(ctx, "streaming: sending resync", "reason", string(batch.Resync))
				if err := rt.writeResync(sse, orgID, watermark, batch.Resync); err != nil {
					return
				}
				continue
			}

			for _, e := range batch.Events {
				// De-duplicate against the replay watermark. This is the other
				// half of subscribe-before-replay: without it, every event that
				// arrived during the replay is delivered twice.
				if e.Seq <= watermark {
					continue
				}
				if err := rt.writeEvent(sse, e); err != nil {
					return
				}
				watermark = e.Seq
			}
		}
	}
}

func (rt *Router) writeEvent(sse *httpx.SSEWriter, e domain.Event) error {
	data, err := json.Marshal(toFrameDTO(e))
	if err != nil {
		rt.log.Error("streaming: could not marshal frame", "seq", e.Seq, "error", err)
		return nil // one bad frame must not end an otherwise healthy stream
	}
	return sse.EventSeq(e.Seq, string(e.Kind), data)
}

func (rt *Router) writeResync(sse *httpx.SSEWriter, orgID uuid.UUID, seq int64, reason domain.ResyncReason) error {
	frame, err := resyncFrame(orgID, seq, rt.clock.Now(), reason)
	if err != nil {
		return err
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	// No `id:` line: a resync is about the stream, not about a position in it, and
	// moving the client's cursor here would let a reconnect skip the very events
	// the resync is telling it to refetch.
	return sse.Event("", string(domain.KindResync), data)
}

// parseStreamRequest validates the query string and the Last-Event-ID header.
func parseStreamRequest(r *http.Request) (domain.Interest, int64, error) {
	q := r.URL.Query()

	alertID, err := optionalUUID(q.Get("alert_id"), "alert_id")
	if err != nil {
		return domain.Interest{}, 0, err
	}
	groupID, err := optionalUUID(q.Get("group_id"), "group_id")
	if err != nil {
		return domain.Interest{}, 0, err
	}

	interest, err := domain.ParseInterest(q["resources"], alertID, groupID)
	if err != nil {
		return domain.Interest{}, 0, err
	}

	sinceSeq, err := parseLastEventID(r.Header.Get("Last-Event-ID"))
	if err != nil {
		return domain.Interest{}, 0, err
	}
	return interest, sinceSeq, nil
}

func optionalUUID(raw, field string) (*uuid.UUID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, errs.Validation("stream_filter_invalid", field+" must be a uuid").
			WithViolations(errs.Violation{Field: field, Code: "uuid", Message: field + " must be a uuid"})
	}
	return &id, nil
}

// parseLastEventID reads the resume cursor.
//
// A malformed header is treated as "no cursor" rather than as an error: it is set
// automatically by EventSource, the client did not choose it, and refusing the
// connection would leave the UI permanently dark over a value the user cannot
// see. Attaching live is the safe degradation.
func parseLastEventID(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq < 0 {
		return 0, nil
	}
	return seq, nil
}
