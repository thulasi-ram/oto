package service

import (
	"context"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	alertsvc "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/idempotency"
	"github.com/thulasiram/oto/internal/platform/jobs"
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
// ⭐ IT DECLARES THE WRITES AS WELL AS THE READS, and that is the whole of ticket
// 0869f21. The write half used to be declared by `sources/api` and satisfied
// straight from the repository, which put the transaction boundary, the ordering
// of three writes across two modules' tables and the credential-rotation rule
// inside an HTTP handler — where nothing but an HTTP request could reach them.
type SourceRepository interface {
	// Get returns one source, or an errs.KindNotFound.
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Source, error)

	// List returns a keyset page of sources. There is no OFFSET in this
	// codebase (SPEC §F.5.3).
	List(ctx context.Context, s db.TenantScope, f domain.SourceFilter, p db.Keyset) ([]domain.Source, db.Cursor, error)

	// ListByIDs returns the live sources named by ids, in one query. Ids that
	// name nothing this org owns are absent from the result rather than an
	// error: it is the batch companion to Get, for a caller holding a page of
	// rows that each name a source.
	ListByIDs(ctx context.Context, s db.TenantScope, ids []uuid.UUID) ([]domain.Source, error)

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

	// HealthFor resolves a page of sources' health in ONE round trip. The list
	// renders health beside every row, and doing that per row is how a settings
	// page with twenty upstreams becomes twenty-one queries.
	HealthFor(ctx context.Context, s db.TenantScope, ids []uuid.UUID) (map[uuid.UUID]domain.SourceHealth, error)

	// ResolveOrg returns the org that owns a source. It is the ONE method here
	// without a TenantScope, because the `source.reconcile` and `silences.sync`
	// payloads name a source and no org, so the org must be discovered before a
	// scope can exist. See the implementation's comment.
	ResolveOrg(ctx context.Context, sourceID uuid.UUID) (uuid.UUID, error)

	// Create inserts one source. It is the registry's only insert, and it never
	// mints the ingest token beside it — that is another module's table, reached
	// through IngestTokens, and the two are made one fact by the transaction the
	// service opens around them rather than by one repository knowing both.
	Create(ctx context.Context, s db.TenantScope, in domain.SourceDraft) (domain.Source, error)

	// Update applies a partial change and returns the row as it now stands.
	Update(ctx context.Context, s db.TenantScope, id uuid.UUID, p domain.SourcePatch) (domain.Source, error)

	// SoftDelete stamps `deleted_at`. ALERT HISTORY IS RETAINED: deleting a
	// source must never erase the record of what it once reported. It answers
	// not-found for an id that does not exist or is already deleted, which is
	// what makes it the call the delete path runs FIRST.
	SoftDelete(ctx context.Context, s db.TenantScope, id uuid.UUID) error
}

// ------------------------------------------------------------------- writes
//
// The three ports below are the collaborators the WRITE PATH needs, and each one
// reaches a table this module does not own. They are declared here, by the
// consumer, for the same reason as everything above: `internal/app/container.go`
// supplies the concretes and `sources` imports neither `channels` nor `identity`.

// CredentialSealer seals an upstream credential into the shared secret store.
//
// It is expressed in PLAIN TYPES rather than in the channels module's meta struct
// because `sources` may not import `channels` internals (depguard,
// sources-must-not-reach-into-other-domains): the composition root supplies the
// adapter over `channels/repository.CredentialRepository`, and this signature is
// the whole contract.
//
// ⛔ The `values` map is secret material. It arrives on a write-only DTO field,
// is handed straight here, and is never logged, echoed or retained.
type CredentialSealer interface {
	// CreateCredential seals a new secret and returns its id.
	CreateCredential(ctx context.Context, s db.TenantScope, kind string, values map[string]string) (uuid.UUID, error)
	// RotateCredential re-seals an existing secret in place, so the referencing
	// source never spends a moment pointing at nothing.
	RotateCredential(ctx context.Context, s db.TenantScope, id uuid.UUID, kind string, values map[string]string) error
}

// IngestTokens mints and revokes the per-source ingest token.
//
// The token lives in `api_tokens` with `source_id` set, which is the identity
// module's table — hence a port rather than a query. The SECRET IS RETURNED
// EXACTLY ONCE, from IssueIngestToken, and only its sha256 is stored; there is no
// method that reads one back, because there is nothing to read.
type IngestTokens interface {
	// IssueIngestToken mints a new token for the source and revokes any that came
	// before it, returning the plaintext secret and its display prefix.
	IssueIngestToken(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) (secret, prefix string, err error)
	// RevokeIngestTokens revokes every token scoped to the source. Deleting a
	// source that could still be pushed to would be a soft delete in name only.
	RevokeIngestTokens(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) error
}

// UnitOfWork runs fn inside ONE database transaction, satisfied by
// `*sources/repository.TxRunner`.
//
// ⭐ IT IS WHAT MAKES A SOURCE AND ITS INGEST TOKEN ONE FACT. Create writes to two
// tables owned by two modules — `alert_sources` here and `api_tokens` behind
// IngestTokens — and before this port existed they were two independent commits.
// A failure in the second left a source row that could never receive a webhook,
// which is worse than no source at all: the operator has a URL to paste and it
// answers 401 forever.
type UnitOfWork interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// IdempotencyClaims takes a client-supplied `Idempotency-Key` for one operation,
// satisfied by `*platform/idempotency.Repository`.
//
// ⭐ IT GUARDS THE TWO OPERATIONS HERE THAT HAND OUT A SECRET. A retried rotation
// minted a SECOND ingest credential — and because a rotation also revokes what
// came before, the retry destroyed the credential the caller may still have been
// holding from the first attempt. The claim is taken inside the operation's own
// transaction, so a key somebody already holds rolls the whole thing back rather
// than leaving the source with a token nobody knows.
type IdempotencyClaims interface {
	Claim(ctx context.Context, s db.TenantScope, c idempotency.Claim) (idempotency.Result, error)
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

// ClusterWriter registers an identity/failure domain, satisfied by the same
// `*sources/repository.ClusterRepository`.
//
// ⭐⭐ IT IS A PORT OF THE SERVICE AND NOT OF `sources/api`, FOR THE REASON THE
// WHOLE OF `write.go` EXISTS. `createCluster` used to go handler → repository
// directly, so the only place an `Idempotency-Key` claim could join the insert's
// transaction was inside an HTTP request — and the transaction boundary is not an
// HTTP concern. Routed through here, the insert and the claim are one unit of
// work, and a job or a CLI could register a cluster without re-deriving either.
//
// ⛔ There is still NO METHOD THAT CHANGES `cluster_key`. It participates in alert
// identity (§C.2), so changing it would re-key every alert in the cluster.
//
// It takes the id because the claim has to name the row before the row exists:
// see ClusterRepository.Create.
type ClusterWriter interface {
	Create(ctx context.Context, s db.TenantScope, clusterID uuid.UUID, key, displayName string) (domain.Cluster, error)
	Get(ctx context.Context, s db.TenantScope, clusterID uuid.UUID) (domain.Cluster, error)
}

// TenantLister is the tenant list in the two halves a fan-out needs: a bounded
// page of ids to enqueue for, and the lookup that turns the id a payload names
// into the scope the table authorises.
//
// The fan-out needs it because every repository method takes a db.TenantScope by
// construction, and the periodic tick has no request to derive one from.
//
// ⛔ IT IS NOT `Scopes()` ANY MORE, AND THAT IS THE WHOLE OF THE CONVERSION. The
// unbounded whole-list read is what let one tick's cost grow with the customer
// count; a page plus a cursor is what bounds it. See jobs.TenantFanOut.
type TenantLister interface {
	jobs.Tenants
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
