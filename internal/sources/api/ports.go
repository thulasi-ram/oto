package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/service"
)

// Every interface in this file is a PORT DECLARED BY THE CONSUMER (CONTEXT.md
// §5.4). This layer says exactly what it calls; `internal/app/container.go`
// decides what satisfies it. Declaring them here — rather than importing a
// concrete — is what makes the HTTP layer testable without a database and what
// stops a handler quietly acquiring a new capability.

// SourceReader is the READ side of the sources module, satisfied by
// `*sources/service.Service`.
type SourceReader interface {
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Source, error)
	List(ctx context.Context, s db.TenantScope, f domain.SourceFilter, p db.Keyset) ([]domain.Source, db.Cursor, error)
	Health(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.SourceHealth, error)
	// Probe never returns an error for an unreachable upstream: "down" is a
	// RESULT, not an exception, which is why `testSource` answers 200 with
	// `ok: false` rather than 502.
	Probe(ctx context.Context, s db.TenantScope, id uuid.UUID) (service.ProbeResult, error)
}

// Compile-time proof that the read service satisfies the port this layer
// declares. A drift here is a compile error at the seam rather than a nil
// interface discovered at boot.
var _ SourceReader = (*service.Service)(nil)

// SourceRegistry is the WRITE side, satisfied by `*sources/repository.SourceRepository`.
//
// ⚠️ ARCHITECTURAL NOTE. CONTEXT.md §5.1 says `api` calls `service`, and this
// port is the exception that proves it: `sources/service` is deliberately
// read-only (there is no write path into a cluster, R3) and has no Create,
// Update or Delete. Rather than inventing business logic in a handler, this layer
// declares the narrow registry port it needs and the composition root injects the
// repository. `api` still does not IMPORT `repository` — depguard's rule holds —
// and the day a `sources/service` write facade exists, it satisfies this port
// unchanged.
type SourceRegistry interface {
	Create(ctx context.Context, s db.TenantScope, in domain.SourceDraft) (domain.Source, error)
	Update(ctx context.Context, s db.TenantScope, id uuid.UUID, p domain.SourcePatch) (domain.Source, error)
	SoftDelete(ctx context.Context, s db.TenantScope, id uuid.UUID) error
	// HealthFor resolves a page of sources' health in ONE round trip. The list
	// renders health beside every row, and doing that per row is how a settings
	// page with twenty upstreams becomes twenty-one queries.
	HealthFor(ctx context.Context, s db.TenantScope, ids []uuid.UUID) (map[uuid.UUID]domain.SourceHealth, error)
}

// ClusterRegistry serves the cluster half of the Sources tag, satisfied by
// `*sources/repository.ClusterRepository`.
//
// ⛔ There is no method that changes `cluster_key`. It participates in alert
// identity, so changing it would re-key every alert in the cluster.
type ClusterRegistry interface {
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Cluster, error)
	List(ctx context.Context, s db.TenantScope, includeDeleted bool, p db.Keyset) ([]domain.Cluster, db.Cursor, error)
	Create(ctx context.Context, s db.TenantScope, key, displayName string) (domain.Cluster, error)
	UpdateDisplayName(ctx context.Context, s db.TenantScope, id uuid.UUID, displayName string) (domain.Cluster, error)
	// ClusterKeysFor resolves a page of sources' cluster keys in one round trip.
	ClusterKeysFor(ctx context.Context, s db.TenantScope, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// CredentialWriter seals an upstream credential into the shared secret store.
//
// It is expressed in PLAIN TYPES rather than in the channels module's meta struct
// because `sources` may not import `channels` internals (depguard,
// sources-must-not-reach-into-other-domains): the composition root supplies the
// adapter, and this signature is the whole contract.
//
// ⛔ The `values` map is secret material. It arrives on a write-only DTO field,
// is handed straight here, and is never logged, echoed or retained.
type CredentialWriter interface {
	// CreateCredential seals a new secret and returns its id.
	CreateCredential(ctx context.Context, s db.TenantScope, kind string, values map[string]string) (uuid.UUID, error)
	// RotateCredential re-seals an existing secret in place, so the referencing
	// source never spends a moment pointing at nothing.
	RotateCredential(ctx context.Context, s db.TenantScope, id uuid.UUID, kind string, values map[string]string) error
}

// IngestTokenIssuer mints and revokes the per-source ingest token.
//
// The token lives in `api_tokens` with `source_id` set, which is the identity
// module's table — hence a port rather than a query. The SECRET IS RETURNED
// EXACTLY ONCE, here, and only its sha256 is stored; there is no method that
// reads one back, because there is nothing to read.
type IngestTokenIssuer interface {
	// IssueIngestToken revokes any existing token for the source and mints a new
	// one, returning the plaintext secret and its display prefix.
	IssueIngestToken(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) (secret, prefix string, err error)
	// RevokeIngestTokens revokes every token scoped to the source. Deleting a
	// source that could still be pushed to would be a soft delete in name only.
	RevokeIngestTokens(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) error
}

// UnitOfWork runs fn inside ONE database transaction, satisfied by
// `*sources/repository.TxRunner`.
//
// ⭐ IT IS WHAT MAKES A SOURCE AND ITS INGEST TOKEN ONE FACT. `createSource`
// writes to two tables owned by two modules — `alert_sources` here and
// `api_tokens` behind IngestTokenIssuer — and before this port existed they were
// two independent commits. A failure in the second left a source row that could
// never receive a webhook, which is worse than no source at all: the operator has
// a URL to paste and it answers 401 forever.
type UnitOfWork interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// IngestFeeds is the READ half of ingestion, as the source screen needs it: why
// this source's alerts never appeared.
//
// ⭐ IT IS EXPRESSED IN PLAIN TYPES, and that is not laziness. `sources` may not
// import `ingestion/domain` (depguard, sources-must-not-reach-into-other-domains)
// — only `ingestion/service` — so the reasons and statuses travel as strings and
// the two row shapes below are declared here, by the consumer. The composition
// root supplies the adapter over `ingestion/service.Service`, exactly as it does
// for CredentialWriter and Reconciler. An unknown reason is refused by the
// handler before it ever reaches the port, so nothing downstream has to guess
// what a string means.
//
// ⛔ Both feeds are TENANT-SCOPED READS and neither takes a `raw` document. The
// rejection feed carries the label set lifted out of `raw` and the batch feed
// carries no payload at all: the columns behind them hold up to 8 MiB per row,
// and a page of fifty would be four hundred megabytes to render a table of
// reasons.
type IngestFeeds interface {
	// ListRejections is the per-source rejection feed, newest first. An empty
	// `reasons` means every reason, which is what the screen opens with.
	ListRejections(
		ctx context.Context, s db.TenantScope, sourceID uuid.UUID, reasons []string, p db.Keyset,
	) ([]RejectionEntry, db.Cursor, error)
	// ListFailedBatches lists the batches that were accepted and never processed.
	// An empty `statuses` means both `failed` and `partial`.
	ListFailedBatches(
		ctx context.Context, s db.TenantScope, sourceID uuid.UUID, statuses []string, p db.Keyset,
	) ([]BatchFailure, db.Cursor, error)
}

// RejectionEntry is one element oto refused to normalise.
//
// Labels is the rejected alert's label set AS IT WAS STORED — already redacted
// per the source's `redact_labels`, so a matched value reads `[redacted]` here
// because it reads `[redacted]` on disk. There is no plaintext behind it and
// this layer must never grow a way to ask for one.
//
// It is EMPTY, never absent, for the rejections that name no alert: a body oto
// could not decode, a body over the size cap, a batch for an unknown source, and
// the batch-level truncation are all about the payload rather than about any one
// alert in it. For those, Reason and Detail are the whole answer.
type RejectionEntry struct {
	ID       uuid.UUID
	SourceID uuid.UUID
	// BatchID is nil when no batch row exists.
	BatchID    *uuid.UUID
	ReceivedAt time.Time
	Reason     string
	Detail     string
	Labels     map[string]string
}

// BatchFailure is one batch whose alerts are durably on disk and never reached
// the product.
type BatchFailure struct {
	ID         uuid.UUID
	SourceID   uuid.UUID
	Mode       string
	ReceivedAt time.Time
	// Status is `failed` or `partial`; nothing else is listable.
	Status string
	// ProcessedAt is when the batch stopped.
	ProcessedAt *time.Time
	// Error is why it stopped. Always set for `failed`, usually empty for
	// `partial`, which stopped by dying rather than by deciding.
	Error string
	// AlertCount is how many alerts are sitting in that payload unprocessed,
	// which is the number that says what the failure cost.
	AlertCount      int
	TruncatedAlerts int
}

// AddressGuard is the SSRF control, satisfied by `*platform/netguard.Guard`.
//
// ⚠️ The check this layer performs is CONFIGURATION-TIME FEEDBACK, so an operator
// who pastes `http://169.254.169.254` learns why while they are still looking at
// the form. It is NOT the control: the same guard is installed as the outbound
// transport's dialer, and that is what actually decides, on the address the
// socket connected to. A layer that treated a passing CheckURL as permission
// would be a layer defeated by a TTL-0 DNS rebind.
type AddressGuard interface {
	CheckURL(ctx context.Context, raw string) error
}

// Reconciler runs one forced reconcile pass.
//
// ⛔ The reconciler is NOT a second ingestion path (§G.8): it reads the
// Alertmanager v2 API, produces Observations and feeds them to the same state
// machine the webhook feeds. It lives in the ingestion module, so this is a port
// and `internal/app` injects the concrete.
//
// The handler bounds the call with its own deadline — see ReconcileTimeout — so a
// wedged upstream cannot hold an HTTP worker for as long as it likes.
type Reconciler interface {
	Reconcile(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) (domain.ReconcileResult, error)
}
