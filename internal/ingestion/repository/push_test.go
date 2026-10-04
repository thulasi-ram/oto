package repository_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/ingestion/repository"
	"github.com/thulasiram/oto/test/harness"
)

// `source_health.last_push_at` is the first thing `oto_ingest_accepted_total`'s
// runbook sends an operator to — "is this Alertmanager still telling oto
// anything?" — and until PushRepository it was NULL on every source oto had ever
// had: `sources/repository.TouchPush` was written, documented in the SPEC and the
// ADR, and called by nothing. These tests pin the writer that replaced it against
// a real Postgres, because every property below is a property of the SQL.

// seedPushedSource registers a source and the `source_health` row
// `SourceRepository.Create` would have seeded for it.
func seedPushedSource(t *testing.T, h *harness.H) (harness.Org, uuid.UUID) {
	t.Helper()

	org := h.Org()
	src := h.Source(org, h.Cluster(org))
	h.Exec(`INSERT INTO source_health (source_id, org_id, status, updated_at)
	        VALUES ($1, $2, 'unknown', $3)`, src.ID, org.ID, h.Now())
	return org, src.ID
}

// lastPush reads the column back, with the status beside it so a test can show
// the stamp left the reaper's half alone.
func lastPush(t *testing.T, h *harness.H, sourceID uuid.UUID) (*time.Time, string) {
	t.Helper()

	var (
		at     *time.Time
		status string
	)
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT last_push_at, status FROM source_health WHERE source_id = $1`, sourceID).Scan(&at, &status))
	return at, status
}

func TestRecordPushStampsLastPushAtAndLeavesStatusAlone(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewPushRepository(h.Pool)
	org, sourceID := seedPushedSource(t, h)

	before, _ := lastPush(t, h, sourceID)
	require.Nil(t, before, "the fixture must start with no push, or the stamp below proves nothing")

	at := h.Now()
	require.NoError(t, repo.RecordPush(h.Ctx, harness.Scope(t, org.ID), sourceID, at))

	got, status := lastPush(t, h, sourceID)
	require.NotNil(t, got, "an accepted push left last_push_at NULL")
	require.True(t, got.Equal(at), "last_push_at = %s, want the batch's receipt %s", got, at)
	require.Equal(t, "unknown", status,
		"a push moved `status`: it proves the source reaches oto, not that oto reaches the source, "+
			"and only the latter may unblock the reaper (§B.4)")
}

// TestRecordPushIsThrottledAndMonotonic is the hot-row half. The stamp rides the
// accept transaction, so inside domain.PushStampEvery it must be a no-op — and a
// pod whose clock lags another's must never move the value backwards.
func TestRecordPushIsThrottledAndMonotonic(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewPushRepository(h.Pool)
	org, sourceID := seedPushedSource(t, h)
	scope := harness.Scope(t, org.ID)

	first := h.Now()
	require.NoError(t, repo.RecordPush(h.Ctx, scope, sourceID, first))

	inside := first.Add(domain.PushStampEvery - time.Second)
	require.NoError(t, repo.RecordPush(h.Ctx, scope, sourceID, inside))
	got, _ := lastPush(t, h, sourceID)
	require.True(t, got.Equal(first),
		"a push inside the throttle window wrote the row (%s); every accept from this source would "+
			"queue on its lock", got)

	earlier := first.Add(-time.Hour)
	require.NoError(t, repo.RecordPush(h.Ctx, scope, sourceID, earlier))
	got, _ = lastPush(t, h, sourceID)
	require.True(t, got.Equal(first), "a lagging clock moved last_push_at backwards to %s", got)

	past := first.Add(domain.PushStampEvery + time.Second)
	require.NoError(t, repo.RecordPush(h.Ctx, scope, sourceID, past))
	got, _ = lastPush(t, h, sourceID)
	require.True(t, got.Equal(past), "a push past the window did not move last_push_at (still %s)", got)
}

// TestRecordPushIsOrgScoped: the source id comes from the URL, so a scope for the
// wrong tenant must touch nothing.
func TestRecordPushIsOrgScoped(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewPushRepository(h.Pool)
	_, sourceID := seedPushedSource(t, h)
	other := h.Org()

	require.NoError(t, repo.RecordPush(h.Ctx, harness.Scope(t, other.ID), sourceID, h.Now()))

	got, _ := lastPush(t, h, sourceID)
	require.Nil(t, got, "another tenant's scope stamped this source's last_push_at")
}
