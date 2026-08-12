package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	silencesapi "github.com/thulasiram/oto/internal/silences/api"
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

// silenceBaseURLs is `silences/api.SourceBaseURLs`, the other half of the
// per-silence Alertmanager deep link: the mirror knows which source a silence
// came from, and this says where that source's UI lives.
//
// ⚠️ SAME BOUNDARY AS ABOVE, same reason: the port speaks ids and strings because
// `silences` may not name `sources/domain`, so the flattening happens here.
//
// It is per-source rather than per-deployment because a process-wide Alertmanager
// URL would deep-link every cluster's silences at one cluster's UI — the 404 the
// link exists to avoid.
type silenceBaseURLs struct{ svc *sourcesservice.Service }

// silenceBaseURLs satisfies the port the silences module declared.
var _ silencesapi.SourceBaseURLs = silenceBaseURLs{}

// BaseURLs resolves a whole page of source ids in one lookup, and answers only
// for the sources oto can vouch for as ALERTMANAGER UI ROOTS.
//
// ⛔ KIND IS THE FILTER, AND IT BELONGS HERE. `base_url` is documented
// everywhere as the Alertmanager API root; for `KindAlertmanager` the API root
// and the UI root are the same origin, so `<base>/#/silences/<id>` resolves. For
// `KindGrafana` neither half holds: the source factory appends stock
// `/api/v2/...` paths without ever reading Kind, so a grafana `base_url` must
// already carry Grafana's AM-compat prefix — and Grafana serves its silences at
// `/alerting/silences`, not at `/#/silences`. Whichever way an operator
// configured it, the link would be wrong.
//
// So a source that is not an Alertmanager is simply ABSENT from this map, which
// the silences layer already renders as no link at all. That is the honest
// answer: an operator who clicks a fabricated link mid-incident and lands on a
// 404 has lost the one affordance v1 offers.
func (b silenceBaseURLs) BaseURLs(
	ctx context.Context, scope db.TenantScope, ids []uuid.UUID,
) (map[uuid.UUID]string, error) {
	srcs, err := b.svc.ListByIDs(ctx, scope, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(srcs))
	for _, src := range srcs {
		if src.Kind != sourcesdomain.KindAlertmanager {
			continue
		}
		out[src.ID] = src.BaseURL
	}
	return out, nil
}
