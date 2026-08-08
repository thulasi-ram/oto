package service

// THE READ SIDE of the Alertmanager silence mirror.
//
// ⛔ READ-ONLY IS A PRODUCT RULING, not an omission (SPEC R3, CONTEXT.md §4).
// There is no Create, no Update and no Expire in this package and there will not
// be one in v1: a silence write path is safety-critical, because a bug in one
// suppresses a real incident, and the deep link into the Alertmanager UI is v1's
// only silence affordance.

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	alertdomain "github.com/thulasiram/oto/internal/alerts/domain"
	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/silences/domain"
)

// MaxMatchedAlerts bounds the "what does this silence cover" answer. The contract
// caps `matched_alerts` at 200, and an unbounded match over every live alert is a
// scan nobody asked for.
const MaxMatchedAlerts = 200

// SilenceRepository is the storage port. It is declared by this package, and
// satisfied by `silences/repository`.
type SilenceRepository interface {
	List(ctx context.Context, s db.TenantScope, f domain.Filter, p db.Keyset) ([]domain.Silence, db.Cursor, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Silence, error)
}

// AlertLister answers "which alerts does oto believe this silence covers".
//
// ⛔ It is a PORT and not a repository import: CONTEXT.md §5.4 binds every
// cross-domain call to be service → service through an interface the CONSUMER
// declares. When it is not wired, a silence detail still renders — with no
// matched alerts, which is honest, rather than failing the page.
type AlertLister interface {
	List(ctx context.Context, s db.TenantScope, q alerts.ListQuery) (alerts.ListResult, error)
}

// Deps is the explicit dependency set.
type Deps struct {
	Silences SilenceRepository
	Alerts   AlertLister
	// Sources reads the upstream silences for `silences.sync`. Optional: without
	// it the module still SERVES the mirror, it just never refreshes it.
	Sources SilenceSource
	// Mirror is the write half of `silences`, reachable from the sync job only.
	Mirror SilenceMirror
	Clock  clock.Clock
	Logger *slog.Logger
}

// Service is the silences module's read logic.
//
// ⛔ It NEVER calls time.Now(). Every clock reading comes from the injected
// clock.
type Service struct {
	repo    SilenceRepository
	alerts  AlertLister
	sources SilenceSource
	mirror  SilenceMirror
	clock   clock.Clock
	log     *slog.Logger
}

// New builds the silences service, refusing a dependency set that cannot work.
func New(d Deps) (*Service, error) {
	if d.Silences == nil {
		return nil, errs.Internal("silence_repo_required",
			errs.New(errs.KindInternal, "missing_dependency", "silences service requires SilenceRepository"))
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.New()
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		repo:    d.Silences,
		alerts:  d.Alerts,
		sources: d.Sources,
		mirror:  d.Mirror,
		clock:   clk,
		log:     logger,
	}, nil
}

// Now is the service's clock reading, in UTC.
func (s *Service) Now() time.Time { return s.clock.Now().UTC() }

// ListResult is one page of mirrored silences.
type ListResult struct {
	Silences []domain.Silence
	Cursor   db.Cursor
}

// List serves `GET /api/v1/silences`.
func (s *Service) List(
	ctx context.Context, scope db.TenantScope, f domain.Filter, p db.Keyset,
) (ListResult, error) {
	if !p.Cursor.IsZero() && p.Cursor.Hash != f.FilterHash {
		return ListResult{}, errs.Malformed("cursor_filter_mismatch",
			"this cursor was minted against a different set of filters")
	}
	rows, cur, err := s.repo.List(ctx, scope, f, p)
	if err != nil {
		return ListResult{}, err
	}
	return ListResult{Silences: rows, Cursor: cur}, nil
}

// Detail is `GET /api/v1/silences/{id}`: one silence and the alerts oto believes
// it currently covers — the answer to "why is this alert quiet, and when does it
// come back?"
type Detail struct {
	Silence domain.Silence
	// Matched are the live alerts whose labels satisfy every matcher.
	Matched []alertdomain.Alert
	// MatchedCount is len(Matched), capped at MaxMatchedAlerts.
	MatchedCount int
}

// Get serves `GET /api/v1/silences/{id}`.
//
// ⚠️ The match is oto's BELIEF, computed from the mirrored matchers against the
// alerts oto can see. Alertmanager remains the authority on what is actually
// suppressed — a suppressed alert is invisible to webhooks, so this is a
// best-effort explanation and never a claim of equivalence.
func (s *Service) Get(ctx context.Context, scope db.TenantScope, id uuid.UUID) (Detail, error) {
	sil, err := s.repo.Get(ctx, scope, id)
	if err != nil {
		return Detail{}, err
	}
	out := Detail{Silence: sil}
	if s.alerts == nil {
		return out, nil
	}

	res, err := s.alerts.List(ctx, scope, alerts.ListQuery{
		Filter: alertdomain.AlertFilter{
			States: []alertdomain.State{alertdomain.StateFiring, alertdomain.StateSuppressed},
		},
		Sort: alerts.SortLastSeenDesc,
		Page: db.Keyset{Limit: MaxMatchedAlerts},
	})
	if err != nil {
		// A silence detail that cannot list alerts is still worth rendering: the
		// creator, the comment and the expiry are the answer to the question that
		// was asked.
		s.log.WarnContext(ctx, "silences: could not resolve matched alerts",
			"silence_id", id, "error", err)
		return out, nil
	}
	for _, a := range res.Alerts {
		if len(out.Matched) == MaxMatchedAlerts {
			break
		}
		if sil.Matches(a.Labels().Map()) {
			out.Matched = append(out.Matched, a)
		}
	}
	out.MatchedCount = len(out.Matched)
	return out, nil
}
