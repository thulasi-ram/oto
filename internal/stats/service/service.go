package service

// THE READ SIDE of alert-hygiene accounting.
//
// ⛔ TEAM- AND ALERT-SCOPED ONLY (SPEC R8). There is no method here that takes a
// user id and none that returns one. Per-person response-time metrics and
// leaderboards are unrepresentable in the underlying schema, which carries no
// user column at all.
//
// The vocabulary is binding: the summed time an alert spent firing is FIRING
// DURATION.

import (
	"context"
	"time"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/stats/domain"
	"github.com/thulasiram/oto/internal/stats/repository"
)

// Default windows, from openapi.yaml.
const (
	// DefaultOverviewWindow is the dashboard's default lookback.
	DefaultOverviewWindow = 24 * time.Hour
	// DefaultQualityWindow is the hygiene report's default lookback.
	DefaultQualityWindow = 30 * 24 * time.Hour
	// MaxQualityWindow bounds a hygiene query. The rollup is one row per
	// (day, cluster, alertname); a multi-year range is a scan nobody meant.
	MaxQualityWindow = 400 * 24 * time.Hour
)

// StatsRepository is the storage port, satisfied by `stats/repository`.
type StatsRepository interface {
	AlertQuality(ctx context.Context, s db.TenantScope, f repository.QualityFilter, limit int) ([]repository.QualityRow, bool, error)
	Overview(ctx context.Context, s db.TenantScope, since, until time.Time, clusters []string) (domain.Overview, error)
	// RollupDay is the ONE write in this module: it recomputes and REPLACES one
	// UTC day of `alert_quality_daily` (ADR 0014). `asOf` is oto's clock, injected
	// so an open episode's firing duration is reproducible.
	RollupDay(ctx context.Context, s db.TenantScope, day time.Time, asOf time.Time) (int, error)
}

// Deps is the explicit dependency set.
type Deps struct {
	Repo  StatsRepository
	Clock clock.Clock
}

// Service is the stats module's read logic.
//
// ⛔ It NEVER calls time.Now(). Every clock reading comes from the injected
// clock, which is what makes a windowed rollup testable without waiting a day.
type Service struct {
	repo  StatsRepository
	clock clock.Clock
}

// New builds the stats service, refusing a dependency set that cannot work.
func New(d Deps) (*Service, error) {
	if d.Repo == nil {
		return nil, errs.Internal("stats_repo_required",
			errs.New(errs.KindInternal, "missing_dependency", "stats service requires StatsRepository"))
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.New()
	}
	return &Service{repo: d.Repo, clock: clk}, nil
}

// Now is the service's clock reading, in UTC.
func (s *Service) Now() time.Time { return s.clock.Now().UTC() }

// Window is a resolved time range, with the defaults already applied.
type Window struct {
	Since time.Time
	Until time.Time
}

// resolveWindow applies the contract's defaults and refuses an inverted or
// unbounded range.
func (s *Service) resolveWindow(since, until time.Time, def, maxWindow time.Duration) (Window, error) {
	now := s.Now()
	if until.IsZero() {
		until = now
	}
	if since.IsZero() {
		since = until.Add(-def)
	}
	since, until = since.UTC(), until.UTC()
	if until.Before(since) {
		return Window{}, errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{Field: "until", Code: "field_order", Message: "until must be >= since"})
	}
	if maxWindow > 0 && until.Sub(since) > maxWindow {
		return Window{}, errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{Field: "since", Code: "min", Message: "the window is too wide"})
	}
	return Window{Since: since, Until: until}, nil
}

// OverviewResult is the dashboard roll-up plus the window it was computed over.
type OverviewResult struct {
	Overview    domain.Overview
	Window      Window
	GeneratedAt time.Time
}

// Overview serves `GET /api/v1/stats/overview`.
func (s *Service) Overview(
	ctx context.Context, scope db.TenantScope, since, until time.Time, clusters []string,
) (OverviewResult, error) {
	w, err := s.resolveWindow(since, until, DefaultOverviewWindow, 0)
	if err != nil {
		return OverviewResult{}, err
	}
	o, err := s.repo.Overview(ctx, scope, w.Since, w.Until, clusters)
	if err != nil {
		return OverviewResult{}, err
	}
	return OverviewResult{Overview: o, Window: w, GeneratedAt: s.Now()}, nil
}

// QualityQuery is the compiled form of `GET /api/v1/stats/alert-quality`.
type QualityQuery struct {
	Since      time.Time
	Until      time.Time
	Clusters   []string
	AlertNames []string
	Sort       domain.Sort
	Limit      int
	// AfterValue and AfterKey are the decoded keyset position.
	AfterValue *float64
	AfterKey   string
}

// QualityResult is one page of the hygiene report.
type QualityResult struct {
	Rows    []repository.QualityRow
	HasMore bool
	Window  Window
}

// AlertQuality serves `GET /api/v1/stats/alert-quality`.
//
// ⭐ It reads the `alert_quality_daily` rollup (ADR 0014), never a scan of the
// event stream. That is what lets the report stay fast as the event table grows
// to hundreds of millions of rows — the load-bearing assumption behind
// Postgres-only.
func (s *Service) AlertQuality(
	ctx context.Context, scope db.TenantScope, q QualityQuery,
) (QualityResult, error) {
	w, err := s.resolveWindow(q.Since, q.Until, DefaultQualityWindow, MaxQualityWindow)
	if err != nil {
		return QualityResult{}, err
	}
	sort := q.Sort
	if sort == "" {
		sort = domain.SortOccurrencesDesc
	}

	rows, hasMore, err := s.repo.AlertQuality(ctx, scope, repository.QualityFilter{
		Since:      w.Since,
		Until:      w.Until,
		Clusters:   q.Clusters,
		AlertNames: q.AlertNames,
		Sort:       sort,
		AfterValue: q.AfterValue,
		AfterKey:   q.AfterKey,
	}, q.Limit)
	if err != nil {
		return QualityResult{}, err
	}
	return QualityResult{Rows: rows, HasMore: hasMore, Window: w}, nil
}
