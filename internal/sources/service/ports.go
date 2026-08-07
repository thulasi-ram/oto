package service

import (
	"context"

	"github.com/google/uuid"

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
