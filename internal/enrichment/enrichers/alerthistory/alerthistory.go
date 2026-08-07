package alerthistory

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
)

// Name is the registry id.
const Name = "alert.history"

// Version is bumped when the payload shape or the statistics change.
const Version = 1

// Timeout is the per-call ceiling from SPEC §F.3: one indexed query.
const Timeout = 200 * time.Millisecond

// CacheTTL is short by design. This is a moving number — the whole point is
// "how often has this been firing lately" — so a long TTL would answer a
// question about the past while claiming to answer one about the present.
const CacheTTL = 60 * time.Second

// SampleLimit bounds how many closed episodes feed the distribution. A rule
// that has fired ten thousand times does not need ten thousand rows to have its
// median computed; it needs a bounded query that returns in 200 ms.
const SampleLimit = 200

// Stats is what the store returns: raw counts and raw durations, with no
// statistics applied.
//
// The split is deliberate. SQL owns the indexed scan; Go owns the arithmetic.
// That keeps the query trivially reviewable, keeps the percentile logic unit
// testable without a database, and stops the two drifting apart.
type Stats struct {
	// Count24h, Count7d and Count30d are occurrence counts in rolling windows.
	Count24h int
	Count7d  int
	Count30d int
	// TotalOccurrences is the lifetime count from the alert projection.
	TotalOccurrences int
	// FlapScore and IsFlapping come from the alert projection (SPEC §B.6).
	FlapScore  float64
	IsFlapping bool
	// FirstSeenAt and LastSeenAt bound the alert's known life.
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	// FiringDurationsSeconds are the durations of CLOSED episodes in the 30-day
	// window, at most SampleLimit of them, newest first.
	//
	// The vocabulary is FIRING DURATION and nothing else. It is how long the
	// signal was firing — a fact about the signal. It is emphatically not MTTR,
	// which is a measure of how fast humans fixed something and is permanently
	// out of scope (SPEC §A.1, R8).
	FiringDurationsSeconds []float64
}

// Store is the narrow read port this enricher needs.
type Store interface {
	// AlertHistory returns the counts and durations for one Alert identity.
	AlertHistory(ctx context.Context, s db.TenantScope, alertID uuid.UUID, now time.Time) (Stats, error)
}

// Distribution is the firing-duration summary.
type Distribution struct {
	Samples int     `json:"samples"`
	MinS    float64 `json:"min_s"`
	P50S    float64 `json:"p50_s"`
	P90S    float64 `json:"p90_s"`
	MaxS    float64 `json:"max_s"`
	MeanS   float64 `json:"mean_s"`
}

// Payload is the enricher's typed output.
type Payload struct {
	Count24h         int `json:"count_24h"`
	Count7d          int `json:"count_7d"`
	Count30d         int `json:"count_30d"`
	TotalOccurrences int `json:"total_occurrences"`

	FlapScore  float64 `json:"flap_score"`
	IsFlapping bool    `json:"is_flapping"`

	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`

	// FiringDuration summarises how long this alert's closed episodes lasted.
	FiringDuration Distribution `json:"firing_duration"`
	// LastFiringDurationS is the most recent closed episode's duration, which
	// is the number an operator actually asks for: "how long did it last last
	// time?".
	LastFiringDurationS float64 `json:"last_firing_duration_s"`

	// Noisy is a rendering hint, not a judgement: more than one fire a day for
	// a month is a rule worth revisiting.
	Noisy bool `json:"noisy"`
}

// NoisyThreshold30d is the 30-day count above which an alert is flagged noisy.
const NoisyThreshold30d = 30

// Enricher attaches an alert's own prior history.
//
// It answers the question a responder asks before any other: "is this new, or
// has it been doing this all week?" — and it answers it about the SIGNAL, never
// about the people responding to it.
type Enricher struct {
	store Store
	clk   clock.Clock
}

// Enricher satisfies the port.
var _ domain.Enricher = (*Enricher)(nil)

// New builds the enricher.
func New(store Store, clk clock.Clock) *Enricher {
	if clk == nil {
		clk = clock.New()
	}
	return &Enricher{store: store, clk: clk}
}

// Name is the stable registry id.
func (*Enricher) Name() string { return Name }

// Version is the payload/semantics version.
func (*Enricher) Version() int { return Version }

// Phase is inline: one indexed query, and "this has fired 40 times today" is
// the context most likely to change what the reader does next.
func (*Enricher) Phase() domain.Phase { return domain.PhaseInline }

// Timeout is the per-call ceiling.
func (*Enricher) Timeout() time.Duration { return Timeout }

// Applicable requires an Alert identity to count against.
func (e *Enricher) Applicable(s *domain.Subject) bool {
	return e.store != nil && s != nil && s.Alert.ID != ""
}

// CacheSeed keys on the Alert identity. The result is shared across every
// occurrence of the same alert within the TTL, which is exactly the case a
// storm produces.
func (*Enricher) CacheSeed(s *domain.Subject) string {
	if s == nil {
		return ""
	}
	return s.Alert.ID
}

// Enrich reads the history and summarises it.
func (e *Enricher) Enrich(ctx context.Context, s *domain.Subject) (domain.Result, error) {
	scope, err := service.ScopeFrom(ctx)
	if err != nil {
		return domain.Result{}, err
	}
	alertID, err := uuid.Parse(s.Alert.ID)
	if err != nil {
		return domain.Result{Status: domain.StatusSkipped, Warnings: []string{"no_alert_id"}}, nil
	}

	stats, err := e.store.AlertHistory(ctx, scope, alertID, e.clk.Now())
	if err != nil {
		return domain.Result{}, err
	}

	payload := Payload{
		Count24h:         stats.Count24h,
		Count7d:          stats.Count7d,
		Count30d:         stats.Count30d,
		TotalOccurrences: stats.TotalOccurrences,
		FlapScore:        stats.FlapScore,
		IsFlapping:       stats.IsFlapping,
		FirstSeenAt:      stats.FirstSeenAt.UTC(),
		LastSeenAt:       stats.LastSeenAt.UTC(),
		FiringDuration:   summarise(stats.FiringDurationsSeconds),
		Noisy:            stats.Count30d >= NoisyThreshold30d,
	}
	if len(stats.FiringDurationsSeconds) > 0 {
		payload.LastFiringDurationS = round(stats.FiringDurationsSeconds[0])
	}

	status := domain.StatusOK
	var warnings []string
	if payload.FiringDuration.Samples == 0 {
		// A first fire has no distribution, and saying so is better than
		// rendering a row of zeroes that reads like a measurement.
		status = domain.StatusPartial
		warnings = append(warnings, "no_closed_episodes_yet")
	}
	if len(stats.FiringDurationsSeconds) >= SampleLimit {
		warnings = append(warnings, "duration_sample_truncated")
	}

	return domain.Result{
		Status:   status,
		Payload:  payload,
		Warnings: warnings,
		TTL:      CacheTTL,
	}, nil
}

// summarise computes the firing-duration distribution.
//
// The percentiles use the nearest-rank method, which is the one that never
// invents a value that did not occur: p50 of a real set of durations is a
// duration that really happened. Interpolated percentiles read better on a
// chart and are a lie on a sample of four.
func summarise(in []float64) Distribution {
	if len(in) == 0 {
		return Distribution{}
	}
	vals := make([]float64, 0, len(in))
	sum := 0.0
	for _, v := range in {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		vals = append(vals, v)
		sum += v
	}
	if len(vals) == 0 {
		return Distribution{}
	}
	sort.Float64s(vals)

	return Distribution{
		Samples: len(vals),
		MinS:    round(vals[0]),
		P50S:    round(nearestRank(vals, 0.50)),
		P90S:    round(nearestRank(vals, 0.90)),
		MaxS:    round(vals[len(vals)-1]),
		MeanS:   round(sum / float64(len(vals))),
	}
}

// nearestRank returns the value at the given quantile, sorted input assumed.
func nearestRank(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// round trims to one decimal: sub-100ms precision on a firing duration measured
// in minutes is noise pretending to be accuracy.
func round(f float64) float64 { return math.Round(f*10) / 10 }
