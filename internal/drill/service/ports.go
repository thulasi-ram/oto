package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/drill/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// Every interface here is a PORT DECLARED BY THE CONSUMER (CONTEXT.md §5.4).
// This service says what it needs; `internal/app/container.go` decides what
// satisfies it. In particular the ingest port is satisfied by
// `ingestion/service.Service` — the SAME object the webhook handler calls, which
// is the whole reason a drill is evidence.

// IngestAcceptor is the front door. A drill pushes its payload through it and
// through nothing else.
//
// ⛔ THERE IS NO SECOND METHOD HERE, AND THERE MUST NOT BE. The moment this port
// grows a "just observe these alerts directly" shortcut, a passing drill stops
// proving that the accept transaction, the outbox enqueue, the decoder, the
// bounds, the redactor and the worker all work — which is most of what it is for.
type IngestAcceptor interface {
	Accept(ctx context.Context, s db.TenantScope, cmd AcceptCommand) (AcceptResult, error)
}

// AcceptCommand mirrors `ingestion/service.AcceptCommand` in this module's own
// vocabulary, because a consumer-declared port may not name another domain's
// types (RULE K grants only `alerts/domain`). The adapter in `internal/app`
// translates.
type AcceptCommand struct {
	SourceID uuid.UUID
	Body     []byte
}

// AcceptResult is the receipt: the durable batch handle.
type AcceptResult struct {
	BatchID uuid.UUID
	// Duplicate is true when `ingest_dedup` collapsed this batch onto an earlier
	// one. For a drill that is impossible by construction — the §C.5 key covers
	// the alert fingerprints, and every drill's payload carries a fresh nonce — so
	// a true here means two drills are about to observe each other's artefacts,
	// and it is logged rather than ignored.
	Duplicate bool
}

// SourceReader answers the two things a drill needs to know about the source it
// is drilling: that it still exists, and which cluster identity its alerts get.
type SourceReader interface {
	DrillTarget(ctx context.Context, s db.TenantScope, id uuid.UUID) (SourceTarget, error)
}

// SourceTarget is the slice of an AlertSource a drill depends on.
type SourceTarget struct {
	// ClusterKey participates in Alert identity (§C.2), so the synthetic payload
	// carries it as its `cluster` label and the drill's Alert lands in the same
	// identity space a real one from this source would.
	ClusterKey string
	// HasPrometheus is why the rule-snapshot stage can distinguish "no rule
	// matched" from "oto could not have looked one up in the first place".
	HasPrometheus bool
	Deleted       bool
}

// DrillStore is the persistence this service owns.
type DrillStore interface {
	Create(ctx context.Context, s db.TenantScope, in domain.NewDrill) (domain.Drill, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Drill, error)
	ListForSource(ctx context.Context, s db.TenantScope, sourceID uuid.UUID, limit int) ([]domain.Drill, error)
	Unfinished(ctx context.Context, s db.TenantScope, limit int) ([]domain.Drill, error)
	Disposable(ctx context.Context, s db.TenantScope, before time.Time, limit int) ([]domain.Drill, error)

	SetBatch(ctx context.Context, s db.TenantScope, id, batchID uuid.UUID) error
	RecordArtefacts(ctx context.Context, s db.TenantScope, id uuid.UUID, a domain.Artifacts) error
	Freeze(ctx context.Context, s db.TenantScope, id uuid.UUID, res domain.Result, at time.Time) error

	Artifacts(ctx context.Context, s db.TenantScope, d domain.Drill) (domain.Artifacts, error)
	Dispose(ctx context.Context, s db.TenantScope, d domain.Drill, at time.Time) error
}
