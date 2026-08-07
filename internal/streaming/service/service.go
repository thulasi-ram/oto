package service

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/streaming/domain"
)

// Service is the write half of streaming: it appends to the durable UI event log
// and publishes through Postgres LISTEN/NOTIFY.
//
// It is deliberately tiny. Every other domain calls Append inside its own
// transaction and knows nothing else about the stream — which is exactly why
// `alerts` can enqueue a UI event without importing a hub, a channel or an
// HTTP connection.
type Service struct {
	repo  EventRepository
	clock clock.Clock
	log   *slog.Logger
}

// NewService builds the streaming write service.
func NewService(repo EventRepository, clk clock.Clock, logger *slog.Logger) *Service {
	if clk == nil {
		clk = clock.New()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, clock: clk, log: logger}
}

// Append records one user-visible change.
//
// IT MUST BE CALLED INSIDE THE CALLER'S TRANSACTION. The row and its NOTIFY then
// commit with the state change they describe, so the UI can never be told about
// something that rolled back, and can never miss something that committed.
//
// payload is the SMALL envelope of §E.4 — a change notice, not a resource. The
// client re-reads for detail; anything over 4 KiB is rejected here rather than by
// a CHECK constraint, because a 23514 on the ingest path would be a 500.
func (s *Service) Append(
	ctx context.Context, scope db.TenantScope, kind domain.Kind, resourceID uuid.UUID, payload json.RawMessage,
) (domain.Event, error) {
	ev, err := domain.NewAppend(kind, resourceID, payload)
	if err != nil {
		return domain.Event{}, err
	}
	return s.repo.Append(ctx, scope, ev)
}

// AppendBatch records many changes in one round trip with a single NOTIFY. A
// 200-alert webhook produces hundreds of these and must not produce hundreds of
// round trips (SPEC §G.4).
func (s *Service) AppendBatch(ctx context.Context, scope db.TenantScope, in []domain.NewEvent) ([]domain.Event, error) {
	if len(in) == 0 {
		return nil, nil
	}
	return s.repo.AppendBatch(ctx, scope, in)
}

// ReplayResult is the outcome of resolving a Last-Event-ID.
type ReplayResult struct {
	// Events is the ordered replay, empty when Resync is set.
	Events []domain.Event
	// Resync, when non-empty, means a replay was refused and the client MUST
	// refetch. It is sent INSTEAD of a replay, never alongside one.
	Resync domain.ResyncReason
	// LastSeq is the highest seq replayed, or the client's own cursor when
	// nothing was replayed. It is the watermark the live feed is de-duplicated
	// against, and it is what makes "no gaps AND no duplicates" true.
	LastSeq int64
}

// Replay resolves a client's Last-Event-ID against the durable log (SPEC §E.4).
//
// The three outcomes, in the order they are decided:
//
//  1. No cursor (sinceSeq <= 0). A fresh EventSource. Attach live with no replay
//     and no resync: there is nothing the client is missing.
//  2. The cursor is below the retained floor, or the org has retained events the
//     cursor cannot reach. Retention has dropped rows between the two and oto
//     cannot prove the replay would be complete, so it sends `resync` instead.
//     A partial replay that LOOKS complete is worse than an explicit refetch.
//  3. Otherwise replay `seq > cursor` in ascending order, capped at
//     MaxReplayRows. Hitting the cap is also a resync: beyond ten thousand
//     frames the client is going to refetch anyway, and this way it knows.
func (s *Service) Replay(ctx context.Context, scope db.TenantScope, sinceSeq int64) (ReplayResult, error) {
	if sinceSeq <= 0 {
		return ReplayResult{}, nil
	}

	cutoff := s.clock.Now().Add(-domain.ReplayWindow)

	floor, _, has, err := s.repo.SeqBounds(ctx, scope, cutoff)
	if err != nil {
		return ReplayResult{}, err
	}
	if !has {
		// Nothing retained at all: a quiet org, or one whose window has fully
		// expired. Either way there is nothing to replay and nothing to warn
		// about — the client's cursor simply names a world that no longer exists
		// and no longer matters.
		return ReplayResult{LastSeq: sinceSeq}, nil
	}
	if floor > sinceSeq {
		s.log.InfoContext(ctx, "streaming: replay window exceeded, sending resync",
			"org_id", scope.OrgID(), "last_event_id", sinceSeq, "retained_floor", floor)
		return ReplayResult{Resync: domain.ResyncReplayWindowExceeded, LastSeq: sinceSeq}, nil
	}

	// Fetch one more than the cap so that "exactly at the cap" is distinguishable
	// from "over it". Off-by-one here is the difference between a silent
	// truncation and an honest resync.
	events, err := s.repo.ListSince(ctx, scope, sinceSeq, cutoff, domain.MaxReplayRows+1)
	if err != nil {
		return ReplayResult{}, err
	}
	if len(events) > domain.MaxReplayRows {
		s.log.InfoContext(ctx, "streaming: replay gap too large, sending resync",
			"org_id", scope.OrgID(), "last_event_id", sinceSeq, "rows", len(events))
		return ReplayResult{Resync: domain.ResyncReplayWindowExceeded, LastSeq: sinceSeq}, nil
	}

	last := sinceSeq
	if n := len(events); n > 0 {
		last = events[n-1].Seq
	}
	return ReplayResult{Events: events, LastSeq: last}, nil
}
