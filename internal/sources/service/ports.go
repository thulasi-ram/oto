package service

import (
	"context"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	alertsvc "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/sources/client/alertmanager"
	"github.com/thulasiram/oto/internal/sources/client/prometheus"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// The ports below are declared by the CONSUMER (SPEC §F.5): this service says
// what it needs, `internal/sources/repository` implements it, and
// `internal/app/container.go` injects the concrete. Every method takes a
// db.TenantScope second, which can only be built from an authenticated
// principal, so there is no way to write a query that forgets its org_id.

// SourceRepository owns alert_sources and source_health.
//
// NOTE: this task defines the port only. There is deliberately no Postgres
// implementation yet; the service is complete and testable against a fake.
type SourceRepository interface {
	// Get returns one source, or an errs.KindNotFound.
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Source, error)

	// List returns a keyset page of sources. There is no OFFSET in this
	// codebase (SPEC §F.5.3).
	List(ctx context.Context, s db.TenantScope, f domain.SourceFilter, p db.Keyset) ([]domain.Source, db.Cursor, error)

	// ListDue returns the sources whose reconcile interval has elapsed. It is
	// the reconciler's fan-out query and is bounded, never unbounded.
	ListDue(ctx context.Context, s db.TenantScope, limit int) ([]domain.Source, error)

	// GetHealth returns the liveness projection. A source that has never been
	// probed returns a zero-value SourceHealth with HealthUnknown, not an error:
	// "not yet observed" is a state, not a failure.
	GetHealth(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) (domain.SourceHealth, error)

	// SaveHealth writes the projection in place. source_health is a projection,
	// not an event: it has no history and is the one table in the sources module
	// that is UPDATEd rather than appended to.
	SaveHealth(ctx context.Context, s db.TenantScope, h domain.SourceHealth) error

	// ResolveOrg returns the org that owns a source. It is the ONE method here
	// without a TenantScope, because the `source.reconcile` and `silences.sync`
	// payloads name a source and no org, so the org must be discovered before a
	// scope can exist. See the implementation's comment.
	ResolveOrg(ctx context.Context, sourceID uuid.UUID) (uuid.UUID, error)
}

// CredentialStore unseals an outbound credential.
//
// It is a port rather than a direct dependency on platform/secrets so that the
// service can be exercised with a fake credential and never needs a keyring in a
// test. The returned Credential is a secret and must not be logged or persisted.
type CredentialStore interface {
	Resolve(ctx context.Context, s db.TenantScope, credentialID uuid.UUID) (domain.Credential, error)
}

// AlertmanagerClient is the port shape the service consumes.
//
// It is domain.AlertmanagerClient (SPEC §F.4, unchanged) plus the three reads
// the port does not declare but the sources screen, the silence mirror and the
// probe all need. Keeping them here rather than widening the SPEC's interface
// leaves §F.4 byte-identical to the specification.
type AlertmanagerClient interface {
	domain.AlertmanagerClient

	// StatusDetail adds the config-parse outcome that AMStatus has no room for.
	StatusDetail(ctx context.Context) (alertmanager.Status, error)
	// AlertGroups reads GET /api/v2/alerts/groups.
	AlertGroups(ctx context.Context, f alertmanager.AlertGroupFilter) ([]alertmanager.AlertGroup, error)
	// Silence reads GET /api/v2/silence/{id} (SINGULAR).
	Silence(ctx context.Context, id string) (domain.GettableSilence, error)
}

// PrometheusClient is domain.PrometheusClient plus the version probe.
type PrometheusClient interface {
	domain.PrometheusClient

	// BuildInfo reads GET /api/v1/status/buildinfo.
	BuildInfo(ctx context.Context) (prometheus.BuildInfo, error)
}

// ---------------------------------------------------------------- reconciler
//
// The three ports below belong to `source.reconcile` (SPEC §G.8, ADR 0006). They
// are declared here, by the consumer, for the same reason as everything above it:
// the reconciler must be exercisable against an httptest.Server and a pair of
// fakes, with no database and no job queue.

// AlertObserver is THE ONE WRITE PATH INTO `alerts` — the ingest orchestrator
// that `internal/app` builds over the alerts state machine and `grouping`.
//
// ⛔ THE RECONCILER IS NOT A SECOND WRITE PATH (C18, ADR 0006). It deliberately
// takes the SAME port `ingestion/service` takes, so a reconciler-recovered alert
// joins a group generation and earns a notification exactly as a webhook-borne
// one does. Feeding `alerts/service` directly would have been a second path that
// silently produced groupless alerts — recorded, and never told to anybody.
//
// What the reconciler adds is the only WITNESS that can see suppression begin:
// `Observation.Source` is `reconciler`, and §B.3's T3 admits no other actor.
//
// ObserveBatch is idempotent by construction: the alert upsert is ON CONFLICT and
// every event it appends is claimed through `alert_event_keys` first (§C.8). A
// pass that runs twice against an unchanged upstream therefore costs two HTTP
// calls and writes nothing the first pass did not.
type AlertObserver interface {
	ObserveBatch(ctx context.Context, s db.TenantScope, obs []alerts.Observation) (int, error)
}

// AlertReader is the oto side of the §G.8.4 divergence check: which alert
// identities does oto currently believe are live? Satisfied by
// `*alerts/service.Service`.
//
// It is a separate port from AlertObserver because it is a READ — nothing about
// divergence accounting writes anything, and a port that could would invite one.
type AlertReader interface {
	List(ctx context.Context, s db.TenantScope, q alertsvc.ListQuery) (alertsvc.ListResult, error)
}

// ClusterReader resolves an AlertSource's Cluster, satisfied by
// `*sources/repository.ClusterRepository`.
//
// `cluster_key` participates in ALERT IDENTITY (§C.2). A reconcile pass that
// could not read it would compute a different `alert_key` from the webhook path
// for the same label set and fork the history of everything it touched, so a
// failure here fails the pass rather than degrading it.
type ClusterReader interface {
	Get(ctx context.Context, s db.TenantScope, clusterID uuid.UUID) (domain.Cluster, error)
}

// TenantLister enumerates every tenant for the periodic fan-out.
//
// The fan-out needs it because every repository method takes a db.TenantScope by
// construction, and the periodic tick has no request to derive one from.
type TenantLister interface {
	Scopes(ctx context.Context) ([]db.TenantScope, error)
}

// ClientFactory builds the outbound clients for one Source.
//
// The service holds a factory rather than clients because a client is bound to
// one base URL, one credential and one TLS posture, all of which are per-source
// and all of which change when an operator edits the source. A test supplies a
// factory that returns fakes and never opens a socket.
type ClientFactory interface {
	// Alertmanager builds a client for src.BaseURL.
	Alertmanager(src domain.Source, cred domain.Credential) (AlertmanagerClient, error)
	// Prometheus builds a client for src.PrometheusURL, or for overrideURL when
	// that is non-empty — which is how a federated setup follows the
	// externalURL recovered from an alert's generatorURL to the Prometheus that
	// actually evaluated the rule (research A7 pitfall 5).
	Prometheus(src domain.Source, cred domain.Credential, overrideURL string) (PrometheusClient, error)
}
