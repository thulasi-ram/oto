package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEveryFailureInTheRetentionFoldWidensTheWindow pins the one promise
// `effectiveRetention`'s own comment makes in the strongest terms this codebase
// has: an unreadable settings walk — the tenant list, an org row, a lookup that
// times out, all of which now surface as ONE failing `MaxRetention` read — must
// never make `partitions.manage` drop MORE than it would have with the read
// intact.
//
// ⭐⭐ IT IS THE FAILURE PATH THAT IS THE TEST, because the happy path was never
// the bug and because both of the fold's historical defects were INVISIBLE at
// the call site: each returned a plausible pair of numbers, and the tick that
// used them dropped partitions without complaint. What separates the two
// answers is not a status but a DIRECTION, and it is only observable against
// the widest window the fold should have seen.
//
//   - THE TENANT LIST FAILING returned the CONFIG FLOOR, which ships at 30 days
//     (`tuning.DefaultRawRetention`), because the per-org loop never ran at all.
//     An org configured to the 365-day bound had 335 days of raw payloads dropped
//     by one timeout.
//   - ONE ORG'S ROW FAILING kept whatever maximum had accumulated so far, which is
//     the correct answer for every org except the single one that changes a
//     maximum: the one asking for the longest window. The old log line even said
//     "keeping the wider window" while doing exactly that.
//
// Both were the same unrecoverable loss reached two ways, and both are now the
// same observable event — `identity/service.MaxRetention` is exact or it is an
// error — so the FAILURE row asserts an INEQUALITY rather than an equality: the
// fold may return anything from the widest configured tenant upward, and the
// settings ceiling it falls back to is deliberately wider than the fixture.
//
// ⛔ THE READABLE ROWS ASSERT EQUALITIES, WHICH IS THE OTHER HALF. An
// inequality alone is satisfied by a fold that widens to the ceiling on EVERY
// tick — 365 days and 120 months, ten years of `alert_events` partitions kept
// for an install nobody asked it of. "Widening is free" is about a broken read
// costing an hour of disk, not about a working one, so the rows where the read
// succeeds pin the answer exactly: the identity maximum when it is wider than
// the config floor, and the FLOOR when every tenant is narrower — a tenant
// population that asks for less than the deployment must not be able to NARROW
// the deployment's own window either.
func TestEveryFailureInTheRetentionFoldWidensTheWindow(t *testing.T) {
	t.Parallel()

	// The deployment's own configured floor, as shipped: 30 days of payloads and
	// 13 months of events.
	const floorRawDays, floorEventMonths = 30, 13

	// The widest tenant's effective window, as MaxRetention reports it.
	//
	// ⚠️ BOTH NUMBERS SIT STRICTLY BETWEEN THE FLOOR AND THE CEILING, and that is
	// what makes the assertions below say anything at all: at the floor the
	// inequality holds vacuously under the OLD code, and at the ceiling a fold
	// that widened on every tick — including the ticks where the read succeeded —
	// would satisfy the exact assertion too. 200 days and 60 months are reachable
	// only by actually using the answer.
	const widestRawDays, widestEventMonths = 200, 60

	widest := stubCeiling{
		raw:   widestRawDays * 24 * time.Hour,
		event: widestEventMonths * 30 * 24 * time.Hour,
	}

	for _, tc := range []struct {
		name    string
		ceiling stubCeiling
		// wantRaw/wantEvent pin the answer exactly when exact is true; otherwise
		// they are the LOWER BOUND the fold may never come back under.
		wantRaw, wantEvent int
		exact              bool
		why                string
	}{
		{"the widest tenant is read", widest, widestRawDays, widestEventMonths, true,
			"the fold is the widest window any tenant asked for — no more, or a working read " +
				"costs the install ten years of event partitions it never configured"},
		{"every tenant is narrower than the deployment", stubCeiling{raw: 24 * time.Hour, event: 30 * 24 * time.Hour},
			floorRawDays, floorEventMonths, true,
			"the deployment's own retention is a floor: a tenant population asking for one day " +
				"must not pull the window below what the operator configured"},
		{"the settings walk cannot be read", stubCeiling{err: errors.New("orgs.list: connection reset")},
			widestRawDays, widestEventMonths, false,
			"a failing MaxRetention used to be a failing Scopes() returning the config floor of " +
				"30 days, so a tenant keeping 200 lost 170 days of raw payloads to one transient " +
				"error on one hourly tick; the only honest fallback is the settings ceiling, " +
				"which no org can be configured beyond"},
	} {
		rawDays, eventMonths := foldRetention(
			context.Background(), quietLogger(), tc.ceiling, floorRawDays, floorEventMonths)

		require.GreaterOrEqual(t, rawDays, tc.wantRaw,
			"%s: raw_retention_days came back %d, narrower than %d. partitions.manage DROPS "+
				"those partitions and nothing restores them — %s", tc.name, rawDays, tc.wantRaw, tc.why)
		require.GreaterOrEqual(t, eventMonths, tc.wantEvent,
			"%s: event_retention_months came back %d, narrower than %d — %s",
			tc.name, eventMonths, tc.wantEvent, tc.why)

		if !tc.exact {
			continue
		}
		require.Equal(t, tc.wantRaw, rawDays,
			"%s: raw_retention_days came back %d for a fold whose read succeeded. The answer "+
				"is %d exactly — %s", tc.name, rawDays, tc.wantRaw, tc.why)
		require.Equal(t, tc.wantEvent, eventMonths,
			"%s: event_retention_months came back %d for a fold whose read succeeded. The "+
				"answer is %d exactly — %s", tc.name, eventMonths, tc.wantEvent, tc.why)
	}
}

// TestTheSettingsCeilingOnlyEverWidens pins the fallback itself, in the one
// direction it is allowed to move.
//
// ⛔ A DEPLOYMENT ALREADY CONFIGURED PAST THE BOUND MUST NOT BE PULLED BACK TO IT.
// `OTO_RETENTION_RAW_PAYLOADS` has no upper bound of its own, so an install
// keeping 1000 days of payloads is legal — and a fallback that ASSIGNED the
// ceiling instead of raising to it would answer a broken read by deleting 635
// days of that install's data, which is the very failure this whole path exists
// to prevent.
func TestTheSettingsCeilingOnlyEverWidens(t *testing.T) {
	t.Parallel()

	rawDays, eventMonths := widenToSettingsCeiling(1000, 240)
	require.Equal(t, 1000, rawDays,
		"a deployment configured wider than the per-org bound keeps its own window")
	require.Equal(t, 240, eventMonths,
		"a deployment configured wider than the per-org bound keeps its own window")

	rawDays, eventMonths = widenToSettingsCeiling(1, 1)
	require.Equal(t, 365, rawDays, "the ceiling is the raw_retention_days bound (identity/domain)")
	require.Equal(t, 120, eventMonths, "the ceiling is the event_retention_months bound")
}

// stubCeiling is the `retentionCeiling` port with its failure under the test's
// control. The production implementation behind it,
// `identity/service.MaxRetention`, is a keyset walk over Postgres with the
// declarative overlay applied per row, and it cannot be asked to fail — the
// per-row mechanics, including the overlay beating an org's own value, are
// pinned in `identity/service`'s own tests.
type stubCeiling struct {
	raw, event time.Duration
	err        error
}

func (s stubCeiling) MaxRetention(context.Context) (time.Duration, time.Duration, error) {
	if s.err != nil {
		return 0, 0, s.err
	}
	return s.raw, s.event, nil
}
