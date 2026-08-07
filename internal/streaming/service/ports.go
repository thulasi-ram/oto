package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/streaming/domain"
)

// EventRepository is the port this service declares for itself (SPEC §F.5).
// It is implemented by internal/streaming/repository.
//
// Every method takes a db.TenantScope second and joins the caller's transaction
// through ctx. There are no WithTx variants anywhere in this codebase.
type EventRepository interface {
	// Append writes one ui_event and issues the NOTIFY, both in the caller's
	// transaction.
	Append(ctx context.Context, s db.TenantScope, e domain.NewEvent) (domain.Event, error)

	// AppendBatch writes many in one round trip with a single NOTIFY.
	AppendBatch(ctx context.Context, s db.TenantScope, in []domain.NewEvent) ([]domain.Event, error)

	// ListSince reads the replay window in seq order. cutoff bounds the partition
	// scan and is the caller's clock, never the database's.
	ListSince(ctx context.Context, s db.TenantScope, sinceSeq int64, cutoff time.Time, limit int) ([]domain.Event, error)

	// SeqBounds is the lowest and highest seq still inside the replay window, and
	// whether the org has any retained events at all.
	//
	// One query rather than two because both callers want both numbers: the
	// resume path compares the floor against the client's cursor, and the bridge
	// seeds its watermark from the ceiling so that a newly-watched org does not
	// re-broadcast twenty-four hours of history to every tab that opens.
	SeqBounds(ctx context.Context, s db.TenantScope, cutoff time.Time) (minSeq, maxSeq int64, has bool, err error)
}

// Publisher is what the bridge pushes freshly-read events into. The Hub is the
// only implementation; the port exists so the bridge is testable without one.
type Publisher interface {
	// Publish fans events out. It MUST NOT block: a publisher waiting on a
	// browser tab is ingest latency measured in browser tabs.
	Publish(events []domain.Event)
	// Orgs lists the orgs that currently have at least one subscriber, so the
	// reconciling poll queries only what somebody is watching.
	Orgs() []uuid.UUID
}
