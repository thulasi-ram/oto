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
	var all []uuid.UUID
	for _, c := range cover {
		all = append(all, c.LiveIDs...)
	}
	healthy := s.healthBySource(ctx, scope, all)
	for id, c := range cover {
		// The reaper's R1 guard, as the screen reads it: at least one live
		// source, and every one of them proven healthy.
		c.AllHealthy = len(c.Sources) > 0
		for i := range c.Sources {
			c.Sources[i].Healthy = healthy[c.Sources[i].ID]
			if !c.Sources[i].Healthy {
				c.AllHealthy = false
			}
		}
		cover[id] = c
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
// cluster and how many of those the reaper's guard holds BECAUSE OF IT (ADR 0056
// §1).
//
// Held is every open Case on the cluster while the source is not proven healthy,
// and none otherwise. ⭐ A HEALTHY REPLICA HOLDS NOTHING (owner ruling R1): the
// reaper asks every live source on a cluster, so an HA pair's Cases are held while
// either replica is unhealthy — and it is the unhealthy one's row that says so,
// exactly as the sweep names only the sources it could not vouch for. It is the
// per-source form of the `held` number the sweep used to report only to a log
// line.
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
	self := make([]uuid.UUID, 0, len(counts))
	for id := range counts {
		self = append(self, id)
	}
	healthy := s.healthBySource(ctx, scope, self)
	for id, c := range counts {
		held := 0
		if !healthy[id] {
			held = c.Open
		}
		out[id] = SourceCaseCount{Open: c.Open, Held: held}
	}
	return out, nil
}
