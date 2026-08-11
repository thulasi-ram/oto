package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/repository"
	"github.com/thulasiram/oto/test/harness"
)

// These tests are about ONE defect, on two tables: `internal_error/
// clusters_time_ck` and `internal_error/alert_sources_time_ck` on an ordinary
// edit, with nothing wrong.
//
// Both CHECKs are `updated_at >= created_at`. Both tables stamp both columns
// from the injected Go clock on INSERT — so unlike `channels` in 00032 there is
// no database clock involved — and then wrote a plain `updated_at = $n` on
// UPDATE. That is enough on its own, because "the application owns time" is not
// "one clock": oto runs N pods with N clocks, and the pod serving a PATCH is
// rarely the pod that created the row. A few milliseconds of lag writes an
// `updated_at` BELOW `created_at` and 23514s. The writers now advance
// `updated_at` monotonically, GREATEST(updated_at, $n), which makes the check
// unfalsifiable while leaving the value app-owned.
//
// The lag is simulated by giving the second repository its OWN fake clock,
// deliberately behind the harness clock the first one used. That is what a
// second pod IS — a different process reading a different clock — and it makes
// the skew deterministic and enormous rather than a flake.

func TestMain(m *testing.M) { harness.Main(m) }

// laggingClock is a second pod's clock, `behind` the one that wrote the row.
func laggingClock(behind time.Duration) clock.Clock {
	return clock.NewFake(harness.Epoch.Add(-behind))
}

func TestClusterTimestampsComeFromTheApplicationClock(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewClusterRepository(h.Pool, h.Clock)

	cl, err := repo.Create(h.Ctx, org.Scope, "prod", "Production")
	require.NoError(t, err)

	// The row did not take the database's clock. The harness FakeClock is pinned
	// at Epoch, months behind the container's wall clock, so a `created_at` from
	// `now()` would not be equal to it.
	require.Equal(t, h.Now(), cl.CreatedAt.UTC(),
		"created_at must come from the injected clock, not from the database")
	require.Equal(t, h.Now(), cl.UpdatedAt.UTC(),
		"one read of one clock orders both columns")
}

// TestClusterRenameSurvivesAPodBehindTheOneThatCreatedIt is the case that 500s.
func TestClusterRenameSurvivesAPodBehindTheOneThatCreatedIt(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()

	creator := repository.NewClusterRepository(h.Pool, h.Clock)
	cl, err := creator.Create(h.Ctx, org.Scope, "prod", "Production")
	require.NoError(t, err)

	// A second pod, two seconds behind the first, serves the rename.
	lagging := repository.NewClusterRepository(h.Pool, laggingClock(2*time.Second))
	renamed, err := lagging.UpdateDisplayName(h.Ctx, org.Scope, cl.ID, "Production EU")
	require.NoError(t, err,
		"a pod whose clock lags the row's creator must not 500 on clusters_time_ck")

	require.Equal(t, "Production EU", renamed.DisplayName,
		"the rename itself still happened; only the timestamp was clamped")
	require.Equal(t, cl.CreatedAt.UTC(), renamed.UpdatedAt.UTC(),
		"updated_at is monotonic: the lagging write may not drag the row backwards")
	require.False(t, renamed.UpdatedAt.Before(renamed.CreatedAt),
		"clusters_time_ck, restated in Go so the failure names the invariant")
}

func TestSourceEditSurvivesAPodBehindTheOneThatCreatedIt(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	cl := h.Cluster(org)

	creator := repository.NewSourceRepository(h.Pool, h.Clock)
	src, err := creator.Create(h.Ctx, org.Scope, domain.SourceDraft{
		ClusterID:   cl.ID,
		Name:        "am-primary",
		Kind:        domain.KindAlertmanager,
		BaseURL:     "https://am.invalid.example",
		PushEnabled: true,
	})
	require.NoError(t, err)
	require.Equal(t, h.Now(), src.CreatedAt.UTC(),
		"created_at must come from the injected clock, not from the database")

	name := "am-primary-renamed"
	lagging := repository.NewSourceRepository(h.Pool, laggingClock(2*time.Second))
	edited, err := lagging.Update(h.Ctx, org.Scope, src.ID, domain.SourcePatch{Name: &name})
	require.NoError(t, err,
		"a pod whose clock lags the row's creator must not 500 on alert_sources_time_ck")
	require.Equal(t, name, edited.Name)
	require.Equal(t, src.CreatedAt.UTC(), edited.UpdatedAt.UTC(),
		"updated_at is monotonic: the lagging write may not drag the row backwards")
}

// TestSourceSoftDeleteSurvivesALaggingPod covers the OTHER writer of
// `alert_sources.updated_at`, which is the one an operator reaches last and the
// one a fix applied only to Update would leave behind.
//
// `deleted_at` is asserted separately because it must NOT be monotonic: it is
// the caller's own instant, the answer to "when was this retired", and a
// GREATEST on it would report a retirement that never happened at that time.
func TestSourceSoftDeleteSurvivesALaggingPod(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	cl := h.Cluster(org)

	creator := repository.NewSourceRepository(h.Pool, h.Clock)
	src, err := creator.Create(h.Ctx, org.Scope, domain.SourceDraft{
		ClusterID: cl.ID,
		Name:      "am-doomed",
		Kind:      domain.KindAlertmanager,
		BaseURL:   "https://am.invalid.example",
	})
	require.NoError(t, err)

	lagging := repository.NewSourceRepository(h.Pool, laggingClock(2*time.Second))
	require.NoError(t, lagging.SoftDelete(h.Ctx, org.Scope, src.ID),
		"a pod whose clock lags the row's creator must not 500 on alert_sources_time_ck")

	var createdAt, updatedAt, deletedAt time.Time
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT created_at, updated_at, deleted_at FROM alert_sources WHERE id = $1`, src.ID).
		Scan(&createdAt, &updatedAt, &deletedAt))

	require.Equal(t, createdAt.UTC(), updatedAt.UTC(),
		"updated_at is monotonic: the lagging write may not drag the row backwards")
	require.Equal(t, harness.Epoch.Add(-2*time.Second), deletedAt.UTC(),
		"deleted_at is the deleting pod's OWN instant and is recorded verbatim")
}

// TestClustersAndSourcesHaveNoClockOfTheirOwn pins the migration itself.
//
// A `DEFAULT now()` here is not inert, it is a trap with a delayed fuse: a
// writer that omits the columns succeeds, and the row it plants fails LATER, on
// somebody else's UPDATE, as a 500 blaming a CHECK constraint. Without the
// default the same omission fails here, immediately, as a not-null violation.
func TestClustersAndSourcesHaveNoClockOfTheirOwn(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()

	_, err := h.Pool.Exec(h.Ctx,
		`INSERT INTO clusters (id, org_id, cluster_key, display_name)
		 VALUES ($1, $2, 'forgetful', 'Forgetful')`, id.New(), org.ID)
	require.Error(t, err, "an INSERT that omits created_at must be refused, not defaulted")

	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, "23502", pgErr.Code, "not_null_violation, at the statement that got it wrong")
	require.Equal(t, "created_at", pgErr.ColumnName)

	cl := h.Cluster(org)
	_, err = h.Pool.Exec(h.Ctx,
		`INSERT INTO alert_sources (id, org_id, cluster_id, name, kind, base_url)
		 VALUES ($1, $2, $3, 'forgetful', 'alertmanager', 'https://am.invalid.example')`,
		id.New(), org.ID, cl.ID)
	require.Error(t, err, "an INSERT that omits created_at must be refused, not defaulted")

	pgErr = nil
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, "23502", pgErr.Code)
	require.Equal(t, "created_at", pgErr.ColumnName)
}
