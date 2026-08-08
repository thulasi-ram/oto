package service

// `stats.rollup` — the write side of alert-hygiene accounting (SPEC §G.3).
//
// ⭐ ADR 0014: the hygiene report is served from `alert_quality_daily` and NEVER
// from a scan of `alert_events`. This is what fills it, and it is the reason
// Postgres-only is a viable architecture rather than a promise that expires at
// scale.

import (
	"context"
	"time"

	"github.com/thulasiram/oto/internal/platform/db"
)

// RollupBackfillDays is how many days one run recomputes, counting back from the
// day it was asked for.
//
// It is TWO, and the second one is not padding. The periodic tick asks for
// "today", so the first tick after midnight UTC would otherwise leave yesterday
// frozen at whatever it looked like at 23:45 — every episode that opened in the
// last quarter hour of the day permanently under-counted. Recomputing the
// previous day as well finalises it, and costs one more upsert of a table that
// holds one row per (day, cluster, alertname).
const RollupBackfillDays = 2

// RollupResult is the audit of one `stats.rollup` run for one tenant.
type RollupResult struct {
	// Days are the UTC days that were recomputed, most recent first.
	Days []time.Time
	// Rows is how many rollup rows were written across those days.
	Rows int
}

// Rollup recomputes the hygiene rollup for `day` and the day before it.
//
// ⭐⭐ IT IS CONVERGENT AND SAFELY RE-RUNNABLE, which is the only property that
// matters for a periodic job on an at-least-once queue: every counter is
// RECOMPUTED FROM THE SOURCE TABLES AND REPLACED, never incremented. Running it
// twice for the same day leaves the same numbers as running it once. Any design
// that added to the stored value would triple a customer's reported alert volume
// the first time a worker was restarted mid-run — and would do it silently, in
// the one report whose whole purpose is to be trusted enough to delete an alert
// rule.
func (s *Service) Rollup(ctx context.Context, scope db.TenantScope, day time.Time) (RollupResult, error) {
	now := s.Now()
	if day.IsZero() {
		day = now
	}
	day = day.UTC().Truncate(24 * time.Hour)

	out := RollupResult{Days: make([]time.Time, 0, RollupBackfillDays)}
	for i := range RollupBackfillDays {
		d := day.AddDate(0, 0, -i)
		n, err := s.repo.RollupDay(ctx, scope, d, now)
		if err != nil {
			return out, err
		}
		out.Days = append(out.Days, d)
		out.Rows += n
	}
	return out, nil
}

// ParseRollupDay parses the `stats.rollup` payload's day.
//
// An empty or unparseable day means TODAY rather than an error. The payload is
// minted by oto's own scheduler, so a bad value is a bug — and skipping the
// rollup because of it would make the hygiene report silently stop updating,
// which is far harder to notice than a rollup for the wrong day.
func ParseRollupDay(s string, fallback time.Time) time.Time {
	if s == "" {
		return fallback.UTC()
	}
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return fallback.UTC()
	}
	return d.UTC()
}
