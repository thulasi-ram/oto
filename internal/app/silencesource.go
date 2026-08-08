package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	silencesservice "github.com/thulasiram/oto/internal/silences/service"
	sourcesdomain "github.com/thulasiram/oto/internal/sources/domain"
	sourcesservice "github.com/thulasiram/oto/internal/sources/service"
)

// silenceSource is `silences/service.SilenceSource`, the read half of
// `silences.sync`.
//
// ⚠️ IT EXISTS BECAUSE `silences` MAY NOT NAME `sources/domain` (depguard,
// silences-must-not-reach-into-other-domains). The consumer declares the port in
// plain types and the composition root does the mapping — the same arrangement
// `sources/api.CredentialWriter` uses to reach the channels secret store, and for
// the same reason.
//
// ⛔ IT IS READ-ONLY, AND SO IS EVERYTHING BEHIND IT. `sources/service` has no
// silence write method to adapt: oto has no write path into your cluster (R3),
// because a bug in a silence write suppresses a real incident.
type silenceSource struct{ svc *sourcesservice.Service }

// silenceSource satisfies the port the silences module declared.
var _ silencesservice.SilenceSource = silenceSource{}

// UpstreamSilences reads active, pending AND expired silences for one source.
//
// All three, always. Without the expired ones, a silence that lapsed between two
// syncs would keep `state = 'active'` in the mirror forever and oto would tell an
// operator an alert is silenced when it is not. The mirror mirrors.
func (s silenceSource) UpstreamSilences(
	ctx context.Context, scope db.TenantScope, sourceID uuid.UUID,
) ([]silencesservice.UpstreamSilence, error) {
	up, err := s.svc.Silences(ctx, scope, sourceID, sourcesdomain.SilenceFilter{
		Active: true, Pending: true, Expired: true,
	})
	if err != nil {
		return nil, err
	}

	out := make([]silencesservice.UpstreamSilence, 0, len(up))
	for _, sil := range up {
		matchers := make([]silencesservice.UpstreamMatcher, 0, len(sil.Matchers))
		for _, m := range sil.Matchers {
			matchers = append(matchers, silencesservice.UpstreamMatcher{
				Name: m.Name, Value: m.Value, IsRegex: m.IsRegex, IsEqual: m.IsEqual,
			})
		}
		out = append(out, silencesservice.UpstreamSilence{
			ID:          sil.ID,
			Matchers:    matchers,
			StartsAt:    sil.StartsAt,
			EndsAt:      sil.EndsAt,
			UpdatedAt:   sil.UpdatedAt,
			CreatedBy:   sil.CreatedBy,
			Comment:     sil.Comment,
			Annotations: sil.Annotations,
			State:       sil.State,
		})
	}
	return out, nil
}
