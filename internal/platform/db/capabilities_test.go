package db_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// TestTrigramAvailableReportsFalseWhenTheExtensionIsNotEnabled proves the
// detector's default answer on a database that never opted in — which is every
// database oto's own migrations produce, since oto never runs `CREATE
// EXTENSION pg_trgm` itself.
func TestTrigramAvailableReportsFalseWhenTheExtensionIsNotEnabled(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	ok, err := db.TrigramAvailable(t.Context(), h.Pool)
	require.NoError(t, err)
	require.False(t, ok, "a freshly migrated database must not have pg_trgm enabled")
}

// TestTrigramAvailableReportsTrueAfterTheOperatorOptsIn proves the OTHER side:
// once an operator runs the one-time SQL snippet this feature documents
// (CREATE EXTENSION IF NOT EXISTS pg_trgm), the detector notices on its next
// read — no oto code enabled it, and no privilege beyond a normal connected
// role was needed to observe it.
func TestTrigramAvailableReportsTrueAfterTheOperatorOptsIn(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	h.Exec(`CREATE EXTENSION IF NOT EXISTS pg_trgm`)

	ok, err := db.TrigramAvailable(t.Context(), h.Pool)
	require.NoError(t, err)
	require.True(t, ok, "pg_trgm was enabled by the test but the detector did not see it")
}
