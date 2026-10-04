package repository_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/repository"
	"github.com/thulasiram/oto/test/harness"
)

// TestSaveHealthDoesNotEraseAPush is the second half of `last_push_at` reaching
// the health endpoint.
//
// The webhook path stamps the column (ingestion/repository.PushRepository); the
// probe and the reconcile pass then read the row, make an outbound call that can
// take seconds, and SaveHealth what they read. With `last_push_at =
// EXCLUDED.last_push_at` that write put back the value from BEFORE the call, so
// every push that landed during a probe was erased — and a caller that had never
// read the column at all, like Create's seed, blanked it.
//
// GetHealth is what `GET /api/v1/sources/{id}/health` and the source list's
// embedded health both map, so asserting through it is asserting the wire value.
func TestSaveHealthDoesNotEraseAPush(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewSourceRepository(h.Pool, h.Clock)

	org := h.Org()
	src := h.Source(org, h.Cluster(org))
	scope := harness.Scope(t, org.ID)

	// What a probe read before its outbound call: a row with an older push.
	stale := h.Now()
	require.NoError(t, repo.SaveHealth(h.Ctx, scope, domain.SourceHealth{
		SourceID: src.ID, Status: domain.HealthUnknown, LastPushAt: &stale, UpdatedAt: stale,
	}))
	read, err := repo.GetHealth(h.Ctx, scope, src.ID)
	require.NoError(t, err)

	// A push lands while the probe is waiting on Alertmanager. Written as the
	// webhook path writes it, so this test does not depend on that module.
	pushed := stale.Add(time.Minute)
	h.Exec(`UPDATE source_health SET last_push_at = $1 WHERE source_id = $2`, pushed, src.ID)

	// The probe comes back and saves what it read, with its own verdict on top.
	read.Status = domain.HealthHealthy
	read.UpdatedAt = pushed.Add(time.Second)
	require.NoError(t, repo.SaveHealth(h.Ctx, scope, read))

	got, err := repo.GetHealth(h.Ctx, scope, src.ID)
	require.NoError(t, err)
	require.Equal(t, domain.HealthHealthy, got.Status, "the probe's own verdict must still land")
	require.NotNil(t, got.LastPushAt)
	require.True(t, got.LastPushAt.Equal(pushed),
		"SaveHealth put back the push it read before its outbound call (%s), erasing the one at %s",
		got.LastPushAt, pushed)

	// And a caller that never read the column cannot blank it.
	require.NoError(t, repo.SaveHealth(h.Ctx, scope, domain.SourceHealth{
		SourceID: src.ID, Status: domain.HealthHealthy, UpdatedAt: pushed.Add(2 * time.Second),
	}))
	got, err = repo.GetHealth(h.Ctx, scope, src.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastPushAt, "a SaveHealth with no LastPushAt blanked the column")
	require.True(t, got.LastPushAt.Equal(pushed))
}
