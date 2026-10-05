package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/contract/schema"
)

// fixedCaseCounts answers CaseCounts from a map, and records what it was asked.
type fixedCaseCounts struct {
	counts map[uuid.UUID]CaseCount
	asked  []uuid.UUID
}

func (f *fixedCaseCounts) OpenCasesBySource(
	_ context.Context, _ db.TenantScope, ids []uuid.UUID,
) (map[uuid.UUID]CaseCount, error) {
	f.asked = append(f.asked, ids...)
	out := map[uuid.UUID]CaseCount{}
	for _, id := range ids {
		if c, ok := f.counts[id]; ok {
			out[id] = c
		}
	}
	return out, nil
}

// TestTheSourceListSaysHowManyOpenCasesItHolds — ADR 0056 §1. The held count was
// the reaper's, and it went only to a log line; the list row now carries it,
// beside the open Cases on the source's cluster, so an operator can see what an
// unhealthy source is keeping open.
func TestTheSourceListSaysHowManyOpenCasesItHolds(t *testing.T) {
	t.Parallel()

	stack := newContractStack()
	counts := &fixedCaseCounts{counts: map[uuid.UUID]CaseCount{
		contractSourceID: {Open: 12, Held: 12},
	}}
	stack.cases = counts

	resp := stack.client().GET("/sources").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listSources", http.StatusOK, resp.Body())

	rows, _ := resp.JSON(t)["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("data = %v, want one row", rows)
	}
	row, _ := rows[0].(map[string]any)
	if row["open_case_count"] != float64(12) || row["held_case_count"] != float64(12) {
		t.Fatalf("open/held = %v/%v, want 12/12", row["open_case_count"], row["held_case_count"])
	}
	if len(counts.asked) != 1 || counts.asked[0] != contractSourceID {
		t.Fatalf("the counts were asked for %v, want the page's one source", counts.asked)
	}
}

// TestASourceTheCountDidNotReachCarriesNoCount — absent is "not counted". A zero
// would tell an operator the source holds nothing open, which nobody checked.
func TestASourceTheCountDidNotReachCarriesNoCount(t *testing.T) {
	t.Parallel()

	for name, cases := range map[string]CaseCounts{
		"unwired":          nil,
		"not in the count": &fixedCaseCounts{counts: map[uuid.UUID]CaseCount{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stack := newContractStack()
			stack.cases = cases

			resp := stack.client().GET("/sources").MustStatus(t, http.StatusOK)
			schema.Assert(t, "listSources", http.StatusOK, resp.Body())

			rows, _ := resp.JSON(t)["data"].([]any)
			row, _ := rows[0].(map[string]any)
			for _, key := range []string{"open_case_count", "held_case_count"} {
				if _, present := row[key]; present {
					t.Fatalf("%s = %v, want it absent", key, row[key])
				}
			}
		})
	}
}
