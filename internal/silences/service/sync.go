package service

// `silences.sync` — SPEC §G.3. The refresh of the read-only mirror.
//
// ⛔ READ-ONLY IS ABOUT THE CLUSTER, NOT ABOUT THIS TABLE. oto has no write path
// into Alertmanager (R3): it cannot create, edit or expire a silence, and the
// only silence affordance in v1 is a deep link into the Alertmanager UI. What
// this file does is COPY what Alertmanager already decided into oto's database so
// the UI can answer "why is this alert quiet, who silenced it, and when does it
// come back?" — a question a webhook can never answer, because a silenced alert
// never reaches a webhook at all.
//
// The mirror and the reconciler are two halves of the same answer: the reconciler
// learns THAT an alert is suppressed (§G.8, ADR 0006), and the mirror carries the
// silence's comment, creator and expiry so oto can say WHY and UNTIL WHEN.

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/silences/domain"
)

// MaxMirroredSilences bounds one sync. An Alertmanager with fifty thousand
// silences is a broken Alertmanager, and a bounded mirror that says so beats an
// unbounded transaction that stalls.
const MaxMirroredSilences = 5_000

// UpstreamSilence is one Alertmanager silence as this module needs to receive it.
//
// ⚠️ IT IS DECLARED HERE, IN PLAIN TYPES, rather than reusing
// `sources/domain.GettableSilence` — `silences` may not import another domain's
// internals (depguard, silences-must-not-reach-into-other-domains), and the
// composition root supplies the adapter. That is the same shape
// `sources/service.CredentialSealer` uses for the same reason, and the signature IS
// the whole contract.
//
// It carries no `json` tags and is not a DTO: it never crosses the wire.
type UpstreamSilence struct {
	// ID is Alertmanager's own silence id — the natural key of the mirror.
	ID          string
	Matchers    []UpstreamMatcher
	StartsAt    time.Time
	EndsAt      time.Time
	UpdatedAt   time.Time
	CreatedBy   string
	Comment     string
	Annotations map[string]string
	// State is `active | pending | expired`, AS UPSTREAM REPORTS IT. oto never
	// computes it from its own clock.
	State string
}

// UpstreamMatcher encodes all four operators via `(IsRegex, IsEqual)`:
// `=` is (false,true), `!=` is (false,false), `=~` is (true,true), `!~` is
// (true,false).
type UpstreamMatcher struct {
	Name    string
	Value   string
	IsRegex bool
	IsEqual bool
}

// SilenceSource reads one source's silences upstream. The composition root
// satisfies it with an adapter over `*sources/service.Service`.
//
// ⛔ IT IS A READ PORT AND HAS NO WRITE METHOD. The ruling is expressed as an
// ABSENT METHOD rather than as a runtime check, because a method that does not
// exist cannot be called by accident.
type SilenceSource interface {
	// UpstreamSilences reads active, pending AND expired silences. All three,
	// always: without the expired ones a silence that lapsed between two syncs
	// would keep `state = 'active'` in the mirror forever.
	UpstreamSilences(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) ([]UpstreamSilence, error)
}

// SilenceMirror is the write half of `silences`, satisfied by
// `*silences/repository.SilenceRepository`. It is reachable from the sync job and
// from nothing else.
type SilenceMirror interface {
	UpsertBatch(ctx context.Context, s db.TenantScope, in []domain.Silence) (int, error)
	ExistingIDs(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) (map[string]uuid.UUID, error)
}

// SyncResult is the audit of one `silences.sync` run.
type SyncResult struct {
	SourceID uuid.UUID
	// Fetched is how many silences the upstream returned.
	Fetched int
	// Mirrored is how many rows the upsert touched.
	Mirrored int
	// Skipped is how many upstream silences oto could not represent — a silence
	// with no matchers, an end before its start, a blank creator. They are counted
	// rather than fatal: one malformed silence must not cost the mirror.
	Skipped int
}

// Sync refreshes the mirror for one source.
//
// ⭐ IT IS CONVERGENT AND SAFELY RE-RUNNABLE. The upsert is keyed by
// Alertmanager's own silence id, so running it twice writes the same rows twice
// and changes nothing — which is what an at-least-once queue requires.
//
// It asks for active, pending AND expired. Expired matters: without it a silence
// that lapsed between two syncs would keep `state = 'active'` in the mirror
// forever, and oto would tell an operator an alert is silenced when it is not.
// The mirror mirrors — oto never computes `state` from its own clock.
func (s *Service) Sync(ctx context.Context, scope db.TenantScope, sourceID uuid.UUID) (SyncResult, error) {
	out := SyncResult{SourceID: sourceID}
	if s.sources == nil || s.mirror == nil {
		return out, errs.Internal("silences_sync_unwired",
			errs.New(errs.KindInternal, "missing_dependency",
				"silences.sync needs both an upstream reader and the mirror"))
	}

	upstream, err := s.sources.UpstreamSilences(ctx, scope, sourceID)
	if err != nil {
		// An unreachable Alertmanager leaves the previous mirror in place, stale
		// but honest, and `mirrored_at` is what tells the UI how stale. Wiping it
		// would render every alert as un-silenced, which is the more dangerous lie.
		return out, err
	}
	out.Fetched = len(upstream)
	if len(upstream) > MaxMirroredSilences {
		upstream = upstream[:MaxMirroredSilences]
		s.log.WarnContext(ctx, "silences: sync truncated an oversized silence set",
			"source_id", sourceID, "returned", out.Fetched, "kept", MaxMirroredSilences)
	}
	if len(upstream) == 0 {
		return out, nil
	}

	existing, err := s.mirror.ExistingIDs(ctx, scope, sourceID)
	if err != nil {
		return out, err
	}

	now := s.Now()
	rows := make([]domain.Silence, 0, len(upstream))
	for _, up := range upstream {
		row, err := s.mirrorRow(scope, sourceID, up, existing, now)
		if err != nil {
			out.Skipped++
			s.log.DebugContext(ctx, "silences: skipped an unrepresentable upstream silence",
				"source_id", sourceID, "source_silence_id", up.ID, "error", err)
			continue
		}
		rows = append(rows, row)
	}

	written, err := s.mirror.UpsertBatch(ctx, scope, rows)
	if err != nil {
		return out, err
	}
	out.Mirrored = written
	return out, nil
}

// mirrorRow maps one upstream silence onto the mirror entity, re-proving every
// §D.9 invariant on the way in.
//
// The row id is REUSED when oto has already mirrored this upstream id, so a
// silence keeps one identity for its whole life and any link to it stays valid.
func (s *Service) mirrorRow(
	scope db.TenantScope, sourceID uuid.UUID, up UpstreamSilence,
	existing map[string]uuid.UUID, now time.Time,
) (domain.Silence, error) {
	matchers := make([]domain.Matcher, 0, len(up.Matchers))
	for _, m := range up.Matchers {
		mm, err := domain.NewMatcher(m.Name, m.Value, m.IsRegex, m.IsEqual)
		if err != nil {
			return domain.Silence{}, err
		}
		matchers = append(matchers, mm)
	}

	state, err := domain.NewState(up.State)
	if err != nil {
		return domain.Silence{}, err
	}

	rowID, ok := existing[up.ID]
	if !ok {
		rowID = id.New()
	}

	return domain.New(domain.Params{
		ID:              rowID,
		OrgID:           scope.OrgID(),
		SourceID:        sourceID,
		SourceSilenceID: up.ID,
		Matchers:        matchers,
		StartsAt:        up.StartsAt,
		EndsAt:          up.EndsAt,
		CreatedBy:       up.CreatedBy,
		Comment:         up.Comment,
		Annotations:     up.Annotations,
		State:           state,
		SourceUpdatedAt: up.UpdatedAt,
		// MirroredAt is oto's clock at this sync — the staleness indicator the UI
		// reads. It is never the upstream's.
		MirroredAt: now,
	})
}
