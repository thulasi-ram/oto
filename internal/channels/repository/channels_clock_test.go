package repository_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/repository"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// These tests are about ONE production defect: `internal_error/channels_time_ck`
// on the first delivery to a brand-new destination, with nothing wrong.
//
// `channels_time_ck` is `updated_at >= created_at`. 00011 stamped `created_at`
// from `DEFAULT now()` — the DATABASE's clock — while every writer of
// `updated_at` stamps the GO process's, so an app server milliseconds behind its
// database failed the first health write with a 23514. The decision (00032) is
// that the APPLICATION owns time on this table: the default is gone and the
// injected clock stamps both columns.
//
// The harness FakeClock is pinned at `harness.Epoch`, which is MONTHS behind the
// container's wall clock. That is the point: it is the same skew the production
// hazard needs, made deterministic and enormous, so a row that took the
// database's clock is not a flake but a certainty.

func TestMain(m *testing.M) { harness.Main(m) }

func TestChannelTimestampsComeFromTheApplicationClock(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewChannelRepository(h.Pool, h.Clock)

	inst, err := repo.Create(h.Ctx, org.Scope, newWebhook("alerts"))
	require.NoError(t, err)

	// The row did not take the database's clock. If it had, `created_at` would be
	// the container's wall clock — months ahead of Epoch — and every assertion
	// below would be about a different hazard.
	require.Equal(t, h.Now(), inst.CreatedAt.UTC(),
		"created_at must come from the injected clock, not from the database")
	require.Equal(t, h.Now(), inst.UpdatedAt.UTC(),
		"one read of one clock orders both columns")

	// The production sequence, with the app clock as far behind the database as a
	// fake clock can put it: create a channel, then immediately record health.
	require.NoError(t, repo.SetHealth(h.Ctx, org.Scope, inst.ID,
		domain.InstanceHealthy, "", h.Now()),
		"the first health write must not depend on the database agreeing with the app about the time")

	stored, err := repo.Get(h.Ctx, org.Scope, inst.ID)
	require.NoError(t, err)
	require.Equal(t, domain.InstanceHealthy, stored.Health)
	require.Equal(t, h.Now(), stored.HealthCheckedAt.UTC())
	require.False(t, stored.UpdatedAt.Before(stored.CreatedAt),
		"channels_time_ck, restated in Go so the failure names the invariant")
}

// TestChannelHealthSurvivesAPodBehindTheOneThatCreatedIt is the half that the
// migration alone does not fix.
//
// "The application owns time" does not mean ONE clock: it means N pods and N
// clocks. The dispatcher that records health after the first delivery is rarely
// the pod that created the destination, and if its clock lags by a few
// milliseconds a plain `updated_at = $5` lands BELOW `created_at` and 23514s.
// The writers therefore advance `updated_at` monotonically — GREATEST(updated_at,
// $n) — which makes the check unfalsifiable while leaving the value app-owned.
func TestChannelHealthSurvivesAPodBehindTheOneThatCreatedIt(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewChannelRepository(h.Pool, h.Clock)

	inst, err := repo.Create(h.Ctx, org.Scope, newWebhook("lagging"))
	require.NoError(t, err)

	// A second pod, two seconds behind the first, learns the destination is fine.
	lagging := h.Now().Add(-2 * time.Second)
	require.NoError(t, repo.SetHealth(h.Ctx, org.Scope, inst.ID,
		domain.InstanceHealthy, "", lagging),
		"a pod whose clock lags the row's creator must not 500 on channels_time_ck")

	stored, err := repo.Get(h.Ctx, org.Scope, inst.ID)
	require.NoError(t, err)
	require.Equal(t, inst.CreatedAt.UTC(), stored.UpdatedAt.UTC(),
		"updated_at is monotonic: the lagging write may not drag the row backwards")
	require.Equal(t, lagging, stored.HealthCheckedAt.UTC(),
		"health_checked_at is the probe's OWN instant and is recorded verbatim")

	// The lagging clock did not cost the row its health either.
	require.Equal(t, domain.InstanceHealthy, stored.Health)
}

// TestChannelsTableHasNoClockOfItsOwn pins the migration itself.
//
// A `DEFAULT now()` here is not inert, it is a trap with a delayed fuse: a writer
// that omits the columns succeeds, and the row it plants fails LATER, at the
// first delivery, as a 500 blaming a CHECK constraint. Without the default the
// same omission fails here, immediately, as a not-null violation.
func TestChannelsTableHasNoClockOfItsOwn(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()

	_, err := h.Pool.Exec(h.Ctx,
		`INSERT INTO channels (id, org_id, type, name, config)
		 VALUES ($1, $2, 'webhook', 'forgetful', '{}'::jsonb)`, id.New(), org.ID)
	require.Error(t, err, "an INSERT that omits created_at must be refused, not defaulted")

	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, "23502", pgErr.Code, "not_null_violation, at the statement that got it wrong")
	require.Equal(t, "created_at", pgErr.ColumnName)
}

func newWebhook(name string) domain.NewInstance {
	return domain.NewInstance{
		Type:           domain.TypeWebhook,
		Name:           name,
		Config:         json.RawMessage(`{}`),
		Renderer:       "webhook.json",
		Verbosity:      domain.VerbosityAll,
		ThreadUpdates:  true,
		ShowFieldEmoji: true,
		Enabled:        true,
	}
}
