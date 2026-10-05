package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// CaseCover answers, for a page of Cases, who can still speak for each one and
// whether the §B.4 guard would let the reaper act under it (ADR 0056 §1). It is
// what lets a Case say "this cannot expire, and here is why" instead of sitting
// open with nothing on screen to explain it.
//
// ⭐ THE HEALTH VERDICT IS THE REAPER'S OWN. It is asked through the same
// `SourceHealth` port, once per distinct source, and absence reads as "not
// proven healthy" exactly as it does in `reapGuarded` — so the screen cannot
// promise an expiry the reaper would refuse.
//
// A nil map with no error means the reader is not wired: the caller renders
// nothing about expiry rather than guessing.
func (s *Service) CaseCover(
	ctx context.Context, scope db.TenantScope, caseIDs []uuid.UUID,
) (map[uuid.UUID]domain.CaseCover, error) {
	if s.cover == nil || len(caseIDs) == 0 {
		return nil, nil
	}
	cover, err := s.cover.CoverFor(ctx, scope, caseIDs)
	if err != nil {
		return nil, err
	}
	bySource := make(map[uuid.UUID]uuid.UUID, len(cover))
	for id, c := range cover {
		if c.SourceID != uuid.Nil {
			bySource[id] = c.SourceID
		}
	}
	healthy := s.healthBySource(ctx, scope, bySource)
	for id, c := range cover {
		if c.SourceID != uuid.Nil {
			c.Healthy = healthy[c.SourceID]
			cover[id] = c
		}
	}
	return cover, nil
}

// SourceCaseCount is how many open Cases sit on one source's cluster, and how
// many of them the reaper is HOLDING because of that source (§B.4): none of them
// can expire while it lasts.
type SourceCaseCount struct {
	Open int
	Held int
}

// OpenCasesBySource counts, for each named live source, the open Cases on its
// cluster and how many of those the reaper's guard holds (ADR 0056 §1).
//
// Held is every open Case on the cluster when the source is not proven healthy,
// or when the cluster has more than one live source — the reaper speaks for a
// cluster's Cases only through its ONE live source (`caseSourcesSQL`), so an HA
// pair holds all of them. It is the per-source form of the `held` number the
// sweep used to report only to a log line.
//
// A source absent from the result was not counted (removed, foreign, or the
// reader is unwired); the caller must not render it as zero.
func (s *Service) OpenCasesBySource(
	ctx context.Context, scope db.TenantScope, sourceIDs []uuid.UUID,
) (map[uuid.UUID]SourceCaseCount, error) {
	out := make(map[uuid.UUID]SourceCaseCount, len(sourceIDs))
	if s.cover == nil || len(sourceIDs) == 0 {
		return out, nil
	}
	counts, err := s.cover.OpenCasesBySource(ctx, scope, sourceIDs)
	if err != nil {
		return nil, err
	}
	self := make(map[uuid.UUID]uuid.UUID, len(counts))
	for id := range counts {
		self[id] = id
	}
	healthy := s.healthBySource(ctx, scope, self)
	for id, c := range counts {
		held := 0
		if c.LiveInCluster != 1 || !healthy[id] {
			held = c.Open
		}
		out[id] = SourceCaseCount{Open: c.Open, Held: held}
	}
	return out, nil
}
