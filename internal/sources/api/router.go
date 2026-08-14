package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// Timeouts that bound the two endpoints which talk to somebody else's server.
//
// ⛔ NEITHER MAY BLOCK ON A SLOW UPSTREAM FOR AS LONG AS THE UPSTREAM LIKES. An
// Alertmanager behind a black-holed firewall rule does not return an error, it
// returns nothing, and an HTTP worker parked on it is a worker that is not
// serving the alert list. Both handlers therefore run under their own deadline,
// derived from the request context so a client disconnect still cancels
// immediately, and a blown deadline becomes a `504` rather than a hang.
const (
	// ProbeTimeout bounds `testSource`. One status call plus one buildinfo call.
	ProbeTimeout = 10 * time.Second
	// ReconcileTimeout bounds `reconcileSource`. It is longer because a pass reads
	// the full alert set, and still far below any sane proxy timeout.
	ReconcileTimeout = 30 * time.Second
)

// Options are the Router's dependencies. Everything is a port, so the whole
// surface is exercisable with fakes and an httptest.Server.
type Options struct {
	Sources SourceReader
	// Registry is the module's write facade. It owns the transaction, the
	// credential and ingest-token orchestration and the `Idempotency-Key` claim;
	// this layer supplies a bound request and renders what comes back.
	Registry SourceRegistry
	Clusters ClusterRegistry
	// ClusterWrites registers a cluster. It is a DIFFERENT collaborator from
	// Clusters — the service rather than the repository — because the
	// `Idempotency-Key` claim has to join the insert's transaction, and the
	// transaction boundary is not an HTTP concern. Nil is a declared `503` on
	// `createCluster` and nothing at all to the cluster reads.
	ClusterWrites ClusterCreator
	Reconcile     Reconciler
	// Feeds is the ingestion module's read half: the per-source rejection feed
	// and the failed-batch list. Nil means this deployment cannot answer "why did
	// my alert never appear", which is a declared 503 rather than a panic.
	Feeds IngestFeeds
	// Guard refuses a base or Prometheus URL that resolves somewhere oto must not
	// dial. Nil means no configuration-time feedback — the dialer still refuses.
	Guard AddressGuard
	// AllowInsecureTLS is a DEPLOYMENT-LEVEL switch, never a tenant's.
	// `tls_skip_verify` disables certificate verification on an outbound
	// connection, which is a decision about the operator's own network trust, not
	// about one org's source. With this false, a request that sets the flag is
	// refused (§M2).
	AllowInsecureTLS bool
	Clock            clock.Clock
	// BaseURL is oto's public root, used to render the absolute `webhook_url` an
	// operator pastes into `webhook_config`. Empty means the response carries the
	// path only, which is still actionable.
	BaseURL string
}

// Router serves the Sources tag: sources, their health, their probes and the
// clusters they belong to.
type Router struct {
	sources     SourceReader
	registry    SourceRegistry
	clusters    ClusterRegistry
	clusterW    ClusterCreator
	reconcile   Reconciler
	feeds       IngestFeeds
	guard       AddressGuard
	allowNoTLSV bool
	clk         clock.Clock
	baseURL     string
}

// NewRouter builds the sources HTTP surface.
func NewRouter(o Options) *Router {
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	return &Router{
		sources:     o.Sources,
		registry:    o.Registry,
		clusters:    o.Clusters,
		clusterW:    o.ClusterWrites,
		reconcile:   o.Reconcile,
		feeds:       o.Feeds,
		guard:       o.Guard,
		allowNoTLSV: o.AllowInsecureTLS,
		clk:         clk,
		baseURL:     strings.TrimRight(o.BaseURL, "/"),
	}
}

// Register mounts every route this package owns onto r, which `internal/app` has
// already rooted at /api/v1.
func (rt *Router) Register(r chi.Router) {
	r.Route("/sources", func(r chi.Router) {
		r.Get("/", rt.listSources)
		r.Post("/", rt.createSource)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", rt.getSource)
			r.Patch("/", rt.updateSource)
			r.Delete("/", rt.deleteSource)
			r.Post("/test", rt.testSource)
			r.Post("/rotate-token", rt.rotateSourceIngestToken)
			r.Post("/reconcile", rt.reconcileSource)
			r.Get("/health", rt.getSourceHealth)
			// The two ingestion feeds. They hang off `/sources/{id}` rather than
			// off the ingest surface because the ingest router is mounted with NO
			// middleware at all — no session, no body cap, no timeout — so a UI
			// read cannot live there, and because the question they answer is
			// asked while looking at the source.
			r.Get("/rejections", rt.listSourceRejections)
			r.Get("/failed-batches", rt.listSourceFailedBatches)
		})
	})

	r.Route("/clusters", func(r chi.Router) {
		r.Get("/", rt.listClusters)
		r.Post("/", rt.createCluster)
		r.Patch("/{id}", rt.updateCluster)
	})
}

// Mount is Register under the name the other domain routers use.
func (rt *Router) Mount(r chi.Router) { rt.Register(r) }

// now is the ONE clock reading a request makes, so a response never disagrees
// with itself about "now".
//
// ⛔ It is the injected clock and never time.Now(): a handler that reads the wall
// clock directly is a handler no test can pin.
func (rt *Router) now() time.Time { return rt.clk.Now().UTC() }

// scopeOf resolves the caller's tenant, which is the only sanctioned path from a
// request to a db.TenantScope.
func scopeOf(r *http.Request) (db.TenantScope, error) {
	_, s, err := authn.Scope(r.Context())
	return s, err
}

// principalOf resolves the caller itself, for the one handler that must record
// WHO made a request and not only which tenant it belonged to.
//
// An `Idempotency-Key` is a client's private handle on its own retry, so a claim
// is keyed by the principal as well as the org: one org member's key must never
// be able to refuse another's request.
func principalOf(r *http.Request) (authn.Principal, error) {
	p, _, err := authn.Scope(r.Context())
	return p, err
}

// webhookURL renders the absolute URL for a source's ingest path.
func (rt *Router) webhookURL(path string) string {
	if rt.baseURL == "" {
		return ""
	}
	return rt.baseURL + path
}

// requireDependency turns a missing collaborator into an honest 503 rather than a
// nil-pointer panic.
//
// A deployment wired without an ingest-token issuer or a reconciler is a
// misconfiguration, not a caller error, and `503` is the one status that says so
// without inviting a retry of the same broken request as a 500 would.
func requireDependency(present bool, code, what string) error {
	if present {
		return nil
	}
	return errs.Unavailable(code, what, 0)
}

// simplePageParams is the allow-list of the plainly-paginated list endpoints in
// this tag. §E.3 is binding: an unknown query parameter is REJECTED, because a
// typo'd filter that is silently ignored returns the wrong page and looks right.
var simplePageParams = []string{"limit", "cursor", "since_seq"}

// page compiles the keyset request for a list endpoint with no filters of its
// own.
//
// ⚠️ `since_seq` is accepted because the contract declares it on `listSources`,
// and is deliberately NOT applied: it is the SSE polling fallback and selects on
// the owning `ui_events.seq`, which the sources module does not project. Sources
// are configuration and change at human speed; a client polling them re-reads a
// page of twenty rows. Silently accepting it keeps a generic client working, and
// this comment is here so nobody later mistakes the omission for an oversight.
func page(r *http.Request, filterHash string) (db.Keyset, int, error) {
	p := httpx.NewParams(r, simplePageParams...)
	if err := p.Err(); err != nil {
		return db.Keyset{}, 0, err
	}
	limit := p.Limit()
	if n := p.Int("since_seq", 0); n < 0 {
		return db.Keyset{}, 0, errs.Malformed("validation_failed", "since_seq must be >= 0")
	}
	if err := p.Err(); err != nil {
		return db.Keyset{}, 0, err
	}
	cursor, err := httpx.DecodeCursor(p.Cursor(), filterHash)
	if err != nil {
		return db.Keyset{}, 0, err
	}
	return httpx.Keyset(limit, cursor), limit, nil
}
