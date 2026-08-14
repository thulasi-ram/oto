package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	identitydomain "github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
)

// TestEveryFailureInTheRetentionFoldWidensTheWindow pins the one promise
// `effectiveRetention`'s own comment makes in the strongest terms this codebase
// has: an unreadable tenant list, an unreadable org row, a settings lookup that
// times out — none of them may make `partitions.manage` drop MORE than it would
// have with the read intact.
//
// ⭐⭐ IT IS THE FAILURE PATHS THAT ARE THE TEST, because the happy path was never
// the bug and because both defects were INVISIBLE at the call site: each returned
// a plausible pair of numbers, and the tick that used them dropped partitions
// without complaint. What separates the two answers is not a status but a
// DIRECTION, and it is only observable against the widest org the fold should
// have seen.
//
//   - THE TENANT LIST FAILING returned the CONFIG FLOOR, which ships at 30 days
//     (`tuning.DefaultRawRetention`), because the per-org loop never ran at all.
//     An org configured to the 365-day bound had 335 days of raw payloads dropped
//     by one `Scopes()` timeout.
//   - ONE ORG'S ROW FAILING kept whatever maximum had accumulated so far, which is
//     the correct answer for every org except the single one that changes a
//     maximum: the one asking for the longest window. The old log line even said
//     "keeping the wider window" while doing exactly that.
//
// Both are the same unrecoverable loss reached two ways: `oto_partitions_manage`
// DROPS, there is no soft delete, and ADR 0024 records cold-storage export as
// scoped rather than built. So the FAILURE rows assert an INEQUALITY rather than
// an equality — the fold may return anything from the widest configured org
// upward, and the settings ceiling it falls back to is deliberately wider than
// any org in this fixture.
//
// ⛔ AND THE READABLE ROW ASSERTS AN EQUALITY, WHICH IS THE OTHER HALF. An
// inequality alone is satisfied by a fold that widens to the ceiling on EVERY
// tick — 365 days and 120 months, ten years of `alert_events` partitions kept for
// an install nobody asked it of. "Widening is free" is about a broken read
// costing an hour of disk, not about a working one, so the row where every read
// succeeds pins the answer exactly.
func TestEveryFailureInTheRetentionFoldWidensTheWindow(t *testing.T) {
	t.Parallel()

	// The deployment's own configured floor, as shipped: 30 days of payloads and
	// 13 months of events.
	const floorRawDays, floorEventMonths = 30, 13

	// Three tenants, and only the third one matters: it is the only org whose
	// absence from the fold changes the answer, which is precisely the org a
	// failing read is not allowed to lose.
	//
	// ⚠️ BOTH OF ITS NUMBERS SIT STRICTLY BETWEEN THE FLOOR AND THE CEILING, and
	// that is what makes the assertions below say anything at all.
	//
	//   - AT THE FLOOR they say nothing. An event window left at the shipped 13
	//     months IS the seed the fold starts from, so "at least 13" holds under the
	//     OLD code on every row and the event half of both failure paths goes
	//     unexercised, with the raw half carrying the whole test.
	//   - AT THE CEILING they say nothing about OVERSHOOT. A raw window of 365 days
	//     is the bound the fallback widens to, so a fold that widened on every tick
	//     — including the ticks where every read succeeded — would satisfy the exact
	//     assertion too.
	//
	// 200 days and 60 months are reachable only by folding this org in.
	narrow := configuredOrg(t, 1, 1)
	shipped := configuredOrg(t, floorRawDays, floorEventMonths)
	widest := configuredOrg(t, 200, 60)

	tenants := stubTenants{scopes: []db.TenantScope{narrow.scope, shipped.scope, widest.scope}}
	orgs := stubOrgs{settings: map[uuid.UUID]identitydomain.Settings{
		narrow.scope.OrgID():  narrow.settings,
		shipped.scope.OrgID(): shipped.settings,
		widest.scope.OrgID():  widest.settings,
	}}

	for _, tc := range []struct {
		name    string
		tenants stubTenants
		orgs    stubOrgs
		// exact marks the row where every read succeeds, which is the only row that
		// may pin the answer rather than bound it: the fold saw every org, so the
		// widest one IS the answer and anything above it is disk kept for nobody.
		exact bool
		why   string
	}{
		{"every tenant readable", tenants, orgs, true,
			"the fold is the widest window any tenant asked for — no more, or a working read " +
				"costs the install ten years of event partitions it never configured"},
		{"the tenant list cannot be read", stubTenants{err: errors.New("scopes: connection reset")}, orgs, false,
			"a Scopes() failure used to return the config floor of 30 days, so a tenant keeping " +
				"200 lost 170 days of raw payloads to one transient error on one hourly tick"},
		{"the widest tenant's row cannot be read", tenants, orgs.unreadable(widest.scope.OrgID()), false,
			"a GetOrg() failure used to continue with the maximum it had, and the only org whose " +
				"loss changes a maximum is the one asking for the longest window"},
	} {
		rawDays, eventMonths := foldRetention(
			context.Background(), quietLogger(), tc.tenants, tc.orgs, floorRawDays, floorEventMonths)

		require.GreaterOrEqual(t, rawDays, widest.rawDays,
			"%s: raw_retention_days came back %d, narrower than the %d days the widest tenant "+
				"is configured to keep. partitions.manage DROPS those partitions and nothing "+
				"restores them — %s", tc.name, rawDays, widest.rawDays, tc.why)
		require.GreaterOrEqual(t, eventMonths, widest.eventMonths,
			"%s: event_retention_months came back %d, narrower than the %d months the widest "+
				"tenant is configured to keep — %s",
			tc.name, eventMonths, widest.eventMonths, tc.why)

		if !tc.exact {
			continue
		}
		require.Equal(t, widest.rawDays, rawDays,
			"%s: raw_retention_days came back %d for a fold that read every org. The answer is "+
				"the widest tenant's %d days — %s", tc.name, rawDays, widest.rawDays, tc.why)
		require.Equal(t, widest.eventMonths, eventMonths,
			"%s: event_retention_months came back %d for a fold that read every org. The answer "+
				"is the widest tenant's %d months — %s",
			tc.name, eventMonths, widest.eventMonths, tc.why)
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

// orgFixture is one tenant as the fold sees it: a scope to be listed, the
// effective settings `GetOrg` would return for it, and the same two numbers in
// the units the fold REPORTS — so an assertion quotes the configured window
// rather than a literal that has to be kept in step with the fixture by hand.
type orgFixture struct {
	scope       db.TenantScope
	settings    identitydomain.Settings
	rawDays     int
	eventMonths int
}

// configuredOrg mints a tenant whose retention is stated in the units the fold
// reports — days of payloads, months of events — so a fixture cannot disagree
// with the assertion about what it was configured to keep.
func configuredOrg(t *testing.T, rawDays, eventMonths int) orgFixture {
	t.Helper()

	scope, err := db.NewTenantScope(id.New())
	require.NoError(t, err)
	return orgFixture{
		scope: scope,
		settings: identitydomain.Settings{
			RawRetention:   time.Duration(rawDays) * 24 * time.Hour,
			EventRetention: time.Duration(eventMonths) * 30 * 24 * time.Hour,
		},
		rawDays:     rawDays,
		eventMonths: eventMonths,
	}
}

// stubTenants is the `retentionTenants` port with its failure under the test's
// control. The production implementation behind it, `orgLister`, is a keyset walk
// over Postgres, which cannot be asked to fail.
type stubTenants struct {
	scopes []db.TenantScope
	err    error
}

func (s stubTenants) Scopes(context.Context) ([]db.TenantScope, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.scopes, nil
}

// stubOrgs is the `retentionSettings` port — `identity.Service.GetOrg` in
// production — with one named org made unreadable.
//
// It returns the settings as EFFECTIVE values, which is what the real service
// returns: `Org.Settings` is already the overrides folded onto the defaults,
// clamped, and overlaid with the deployment's Declarative.
type stubOrgs struct {
	settings map[uuid.UUID]identitydomain.Settings
	broken   uuid.UUID
}

// unreadable returns a copy in which one org's read fails — the fixture is shared
// across the table's rows, so it is never mutated in place.
func (s stubOrgs) unreadable(orgID uuid.UUID) stubOrgs {
	s.broken = orgID
	return s
}

func (s stubOrgs) GetOrg(_ context.Context, scope db.TenantScope) (identitydomain.Org, error) {
	if scope.OrgID() == s.broken {
		return identitydomain.Org{}, errors.New("orgs.get: context deadline exceeded")
	}
	return identitydomain.Org{ID: scope.OrgID(), Settings: s.settings[scope.OrgID()]}, nil
}
