package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// These tests are about ONE production defect, and it is the MIRROR of the one
// 00032 fixed on `channels`.
//
// `orgs_time_ck` is `updated_at >= created_at`. The row is created by
// `internal/app.Bootstrap`, which stamps `created_at` from the GO clock, while
// `UpdateSettings` used to write `updated_at = now()` — the DATABASE's. Two
// machines, two clocks, one CHECK across them. `channels` needed the app server
// running BEHIND its database to fail; `orgs` needs it running AHEAD, and the
// person standing in that window is a brand-new operator opening the settings
// screen right after first setup. 00033 makes the injected clock stamp both
// columns and advances `updated_at` monotonically.
//
// ⚠️ THE HARNESS FAKE CLOCK IS PINNED MONTHS BEHIND THE CONTAINER'S WALL CLOCK,
// which is the WRONG direction for this defect: with the app clock behind,
// `now()` is always the larger value and the CHECK is never troubled. The mirror
// case therefore reads the database's own `now()` and pushes the fake clock PAST
// it, so the skew is not merely present but pointed the way that breaks.

func TestMain(m *testing.M) { harness.Main(m) }

// TestOrgSettingsWriteSurvivesAnAppClockAheadOfTheDatabase is the live defect,
// reproduced along the path a real operator walks: bootstrap the deployment,
// then change a setting.
func TestOrgSettingsWriteSurvivesAnAppClockAheadOfTheDatabase(t *testing.T) {
	t.Parallel()

	h := harness.New(t)

	// Put the application an hour AHEAD of Postgres. In production this is a few
	// milliseconds of NTP drift; an hour only makes the same failure a certainty
	// instead of a coin flip.
	var dbNow time.Time
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `SELECT now()`).Scan(&dbNow))
	h.Clock.Set(dbNow.UTC().Add(time.Hour))

	// The real first-run command, not a hand-written row: `created_at` has to
	// come from the writer that actually creates orgs for this to be the
	// production sequence.
	boot, err := app.Bootstrap(h.Ctx, h.Pool, app.BootstrapRequest{
		OrgSlug:     "brand-new",
		OrgName:     "Brand New",
		Email:       "operator@example.test",
		DisplayName: "Operator",
		Password:    "correct-horse-battery-staple",
		TokenName:   "bootstrap",
	}, h.Now())
	require.NoError(t, err)

	scope := harness.Scope(t, boot.OrgID)
	repo := repository.NewOrgRepository(h.Pool, h.Clock)

	// The first thing a new operator does. Before 00033 this was a 23514 —
	// `internal_error/orgs_time_ck`, a 500 — because the row's `created_at` came
	// from a clock an hour ahead of the one this statement wrote `updated_at`
	// from.
	org, err := repo.UpdateSettings(h.Ctx, scope, domain.SettingsPatch{RefireGraceS: intPtr(900)})
	require.NoError(t, err,
		"a settings write soon after bootstrap must not depend on Postgres agreeing with the app about the time")

	require.Equal(t, h.Now(), org.CreatedAt.UTC(),
		"created_at is the application's instant, as Bootstrap wrote it")
	require.Equal(t, h.Now(), org.UpdatedAt.UTC(),
		"updated_at must come from the injected clock; the database's now() is an hour behind it")
	require.False(t, org.UpdatedAt.Before(org.CreatedAt),
		"orgs_time_ck, restated in Go so the failure names the invariant")
	require.Equal(t, 900, *org.Overrides.RefireGraceS, "the write itself still happened")
}

// TestOrgSettingsSurvivesAPodBehindTheOneThatBootstrapped is the half the clock
// injection alone does not fix.
//
// "The application owns time" does not mean ONE clock: it means N pods and N
// clocks. The pod serving a settings PATCH is rarely the pod that bootstrapped
// the deployment, and if its clock lags a plain `updated_at = $3` lands BELOW
// `created_at` and 23514s exactly as `now()` did. `updated_at` is therefore
// advanced with GREATEST, which makes the check unfalsifiable while leaving the
// value app-owned.
func TestOrgSettingsSurvivesAPodBehindTheOneThatBootstrapped(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()

	// A second pod, two seconds behind the one that wrote the row.
	lagging := clock.NewFake(h.Now().Add(-2 * time.Second))
	repo := repository.NewOrgRepository(h.Pool, lagging)

	got, err := repo.UpdateSettings(h.Ctx, org.Scope, domain.SettingsPatch{FlapThreshold: intPtr(40)})
	require.NoError(t, err,
		"a pod whose clock lags the row's creator must not 500 on orgs_time_ck")

	require.Equal(t, h.Now(), got.UpdatedAt.UTC(),
		"updated_at is monotonic: the lagging write may not drag the row backwards")
	require.Equal(t, 40, *got.Overrides.FlapThreshold,
		"the lagging clock did not cost the org its setting")
}

// TestOrgsTableHasNoClockOfItsOwn pins the migration itself.
//
// A `DEFAULT now()` here is not inert, it is a trap with a delayed fuse: a writer
// that omits the columns succeeds, and the row it plants fails LATER, at somebody
// else's first settings change, as a 500 blaming a CHECK constraint two files
// away. Without the default the same omission fails here, immediately, as a
// not-null violation.
func TestOrgsTableHasNoClockOfItsOwn(t *testing.T) {
	t.Parallel()

	h := harness.New(t)

	_, err := h.Pool.Exec(h.Ctx,
		`INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)`,
		id.New(), "forgetful-"+uuid.NewString()[:8], "Forgetful")
	require.Error(t, err, "an INSERT that omits created_at must be refused, not defaulted")

	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, "23502", pgErr.Code, "not_null_violation, at the statement that got it wrong")
	require.Equal(t, "created_at", pgErr.ColumnName)
}

func intPtr(n int) *int { return &n }
