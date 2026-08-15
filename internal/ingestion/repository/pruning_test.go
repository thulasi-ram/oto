package repository

// This file is INSIDE the package on purpose. It asserts a property of the SQL
// text itself — that the feed's cursored query prunes partitions — and the only
// honest way to assert that is to EXPLAIN the exact constant the repository runs.
// A copy of the query in an external test would pass forever after the real one
// drifted.

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/test/harness"
)

// TestTheCursoredFeedPrunesPartitions is the verification behind the redundant
// `received_at <= $5` in listRejectionsFromSQL and listFailedBatchesFromSQL.
//
// ⭐ THE CLAIM BEING TESTED. `ingest_rejections` is PARTITION BY RANGE
// (received_at) with daily partitions. A keyset on `(received_at, id) < (a, b)`
// looks like it bounds the partition key, and it does not: the planner does not
// decompose a ROW COMPARISON into a bound on its leading column, so a query
// carrying only the row comparison opens every retained partition on every page.
// The redundant simple comparison is what the planner actually prunes on, and
// this test is the difference between believing that and knowing it.
//
// The assertion is deliberately structural rather than numeric: with a cursor at
// today, TOMORROW's partition must not appear in the plan, and today's must. The
// first-page query — which has no bound to prune on — is EXPLAINed alongside as
// the control, so a pass cannot come from tomorrow's partition simply not
// existing.
func TestTheCursoredFeedPrunesPartitions(t *testing.T) {
	t.Parallel()

	h := harness.New(t)

	now := time.Now().UTC()
	cursorAt := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	tomorrow := cursorAt.Add(24 * time.Hour)

	partitionName := func(at time.Time) string {
		var name string
		if err := h.Pool.QueryRow(h.Ctx,
			`SELECT oto_partition_name('ingest_rejections', 'day', $1)`, at).Scan(&name); err != nil {
			t.Fatalf("partition name: %v", err)
		}
		return name
	}
	todayPart, tomorrowPart := partitionName(cursorAt), partitionName(tomorrow)

	// The control's control: both partitions must exist, or "not in the plan"
	// would be a statement about the schema rather than about pruning.
	for _, name := range []string{todayPart, tomorrowPart} {
		var exists bool
		if err := h.Pool.QueryRow(h.Ctx,
			`SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&exists); err != nil {
			t.Fatalf("partition exists: %v", err)
		}
		if !exists {
			t.Fatalf("partition %s does not exist; oto_partitions_manage did not run", name)
		}
	}

	explain := func(sql string, args ...any) string {
		var plan []byte
		if err := h.Pool.QueryRow(h.Ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&plan); err != nil {
			t.Fatalf("explain: %v", err)
		}
		return string(plan)
	}

	org, source := uuid.New(), uuid.New()
	noReasons := []string{}

	firstPage := explain(listRejectionsSQL, org, source, noReasons, 51)
	if !strings.Contains(firstPage, tomorrowPart) {
		t.Fatalf("the FIRST page's plan does not mention %s, so this test cannot tell pruning from "+
			"a missing partition:\n%s", tomorrowPart, firstPage)
	}

	nextPage := explain(listRejectionsFromSQL, org, source, noReasons, 51, cursorAt, uuid.New())
	if strings.Contains(nextPage, tomorrowPart) {
		t.Fatalf("a cursored page still opens %s: the keyset is not pruning partitions, so every page "+
			"of every rejection feed scans all of them\n%s", tomorrowPart, nextPage)
	}
	if !strings.Contains(nextPage, todayPart) {
		t.Fatalf("a cursored page pruned %s, the partition its own cursor points into:\n%s",
			todayPart, nextPage)
	}
}
