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

// SourceRegistry is the WRITE side of the sources module, satisfied by
// `*sources/service.Service`.
//
// ⭐ EVERY METHOD IS ONE WHOLE OPERATION, and that is the point of it. This port
// used to be the repository — Create/Update/SoftDelete over `alert_sources` and
// nothing else — with the handler supplying the transaction, the ordering of the
// writes against `channel_credentials` and `api_tokens`, and the rule that a
// supplied credential rotates in place. Every one of those is a statement about
// what registering a source MEANS, so a job or a CLI needed a router to say it.
// They live in `sources/service` now (CONTEXT.md §5.3) and a handler here binds,
// calls one of these, and maps the error.
type SourceRegistry interface {
	// Create seals the credential, inserts the source and mints its ingest token
	// as ONE transaction, and returns the secret exactly once.
	Create(ctx context.Context, s db.TenantScope, cmd service.CreateCommand) (service.IssuedIngest, error)
	Update(ctx context.Context, s db.TenantScope, id uuid.UUID, cmd service.UpdateCommand) (domain.Source, error)
	// SoftDelete retires the source and revokes its ingest token together.
	SoftDelete(ctx context.Context, s db.TenantScope, id uuid.UUID) error
	// RotateIngestToken mints a replacement ingest credential and revokes what
	// came before, in one transaction, returning the new secret exactly once.
	RotateIngestToken(
		ctx context.Context, s db.TenantScope, id uuid.UUID, idem service.Idempotency,
	) (service.IssuedIngest, error)
	// HealthFor resolves a page of sources' health in ONE round trip. The list
	// renders health beside every row, and doing that per row is how a settings
	// page with twenty upstreams becomes twenty-one queries.
	HealthFor(ctx context.Context, s db.TenantScope, ids []uuid.UUID) (map[uuid.UUID]domain.SourceHealth, error)
}

// Compile-time proof that the write facade satisfies the port this layer
// declares. A drift here is a compile error at the seam rather than a nil
// interface discovered at boot.
var _ SourceRegistry = (*service.Service)(nil)

// ClusterRegistry serves the READ half of the cluster surface plus the one edit
// that touches no identity, satisfied by
// `*sources/repository.ClusterRepository`.
//
// ⛔ There is no method that changes `cluster_key`. It participates in alert
// identity, so changing it would re-key every alert in the cluster.
//
// ⛔ AND THERE IS NO LONGER A `Create` ON IT. See ClusterCreator.
type ClusterRegistry interface {
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Cluster, error)
	List(ctx context.Context, s db.TenantScope, includeDeleted bool, p db.Keyset) ([]domain.Cluster, db.Cursor, error)
	UpdateDisplayName(ctx context.Context, s db.TenantScope, id uuid.UUID, displayName string) (domain.Cluster, error)
	// ClusterKeysFor resolves a page of sources' cluster keys in one round trip.
	ClusterKeysFor(ctx context.Context, s db.TenantScope, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// ClusterCreator registers an identity/failure domain, satisfied by
// `*sources/service.Service`.
//
// ⭐⭐ IT IS A SEPARATE PORT FROM ClusterRegistry BECAUSE IT IS SATISFIED BY A
// DIFFERENT LAYER, and that is the same correction `createSource` made. An
// `Idempotency-Key` claim has to join the insert's own transaction, so a handler
// wired straight to the repository had nowhere to take one — which is why
// `createCluster` answered a same-body retry with a `clusters_key_uniq` conflict
// naming nothing, rather than with the cluster the caller had already made. The
// intent crosses the seam; the transaction stays on the far side of it.
type ClusterCreator interface {
	CreateCluster(ctx context.Context, s db.TenantScope, key, displayName string, idem service.Idempotency) (domain.Cluster, error)
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ ClusterCreator = (*service.Service)(nil)

// ⛔ THE CREDENTIAL STORE, THE INGEST-TOKEN ISSUER, THE UNIT OF WORK AND THE
// `Idempotency-Key` CLAIM STORE ARE NO LONGER PORTS OF THIS LAYER. They are
// `sources/service`'s, declared in its own `ports.go` and satisfied by the same
// composition root. A handler that could reach them was a handler that owned the
// transaction boundary, and the transaction boundary is not an HTTP concern: a
// job, a CLI or a test with no router could not create a source without
// re-deriving the ordering, and the one path that re-derived it committed twice.
// This layer now reads the `Idempotency-Key` header — a transport fact — and hands
// the intent to the service that takes the claim.

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
