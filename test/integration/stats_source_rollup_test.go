package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// ⭐⭐ THE ROLL-UP MUST NOT COUNT A SOURCE THAT HAS BEEN DELETED, and until this
// test existed it counted it forever.
//
// `SoftDelete` sets `alert_sources.deleted_at` and leaves the `source_health` row
// exactly as the reconciler last wrote it. Nothing in the system removes that row
// — the `ON DELETE CASCADE` on `source_health.source_id` fires only on a hard
// delete, which this codebase deliberately never performs, because deleting a
// source must never erase the record of what it once reported.
//
// So an org that retired an Alertmanager while it was unreachable used to carry
// `sources.unreachable >= 1` for the rest of its life. That number is not a
// dashboard curiosity: it is the org-wide evidence behind the shell strip that
// announces the §B.4 reaper guard, the one message that must never be ignored. A
// permanent unactionable strip above the alert table is how it gets ignored.
//
// This runs against the real Postgres and the real HTTP surface, because the
// defect lived in one CTE's FROM clause and nothing above it could have seen it.
func TestTheOverviewStopsCountingADeletedSource(t *testing.T) {
	e := newEnv(t)
	boot, cluster, live := bootstrapSource(t, e, "rollup-deleted")

	// A second source in the same cluster, which is the one that gets retired.
	var second struct {
		Data struct {
			Source struct {
				ID uuid.UUID `json:"id"`
			} `json:"source"`
		} `json:"data"`
	}
	e.do(t, http.MethodPost, "/api/v1/sources", boot.Token, map[string]any{
		"name": "am-2", "cluster_id": cluster.String(),
		"kind": "alertmanager", "base_url": "https://alertmanager-2.rollup.example",
	}, http.StatusCreated, &second)
	retired := second.Data.Source.ID

	// Both are unreachable, and the one about to be deleted is the WORSE of the
	// two on every column the CTE aggregates — so a query that still counts it
	// fails on the count, on the skew and on the divergence rather than on one of
	// them. The reconciler is not driven here on purpose: this test is about what
	// the aggregate reads, and driving it would make a failure ambiguous between
	// the two.
	markUnreachable(t, e, live, 500, 2)
	markUnreachable(t, e, retired, 9000, 7)

	e.do(t, http.MethodDelete, "/api/v1/sources/"+retired.String(), boot.Token, nil,
		http.StatusNoContent, nil)

	// The hazard is real and not hypothetical: the health row outlives the delete.
	// If this ever stops being true the test above it is passing for a reason it
	// was not written for.
	var rows int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM source_health WHERE source_id = $1`, retired).Scan(&rows); err != nil {
		t.Fatalf("read source_health: %v", err)
	}
	if rows != 1 {
		t.Fatalf("the deleted source has %d health rows, want 1 — this test no longer "+
			"reproduces the condition the roll-up has to survive", rows)
	}

	var out struct {
		Data struct {
			Sources struct {
				Healthy         int   `json:"healthy"`
				Degraded        int   `json:"degraded"`
				Unreachable     int   `json:"unreachable"`
				Unknown         int   `json:"unknown"`
				MaxClockSkewMS  int64 `json:"max_clock_skew_ms"`
				TotalDivergence int   `json:"total_divergence"`
			} `json:"sources"`
		} `json:"data"`
	}
	e.do(t, http.MethodGet, "/api/v1/stats/overview", boot.Token, nil, http.StatusOK, &out)

	if out.Data.Sources.Unreachable != 1 {
		t.Errorf("sources.unreachable = %d, want 1 — the roll-up is counting a source the "+
			"operator deleted, and the shell strip built on this number would name nothing and "+
			"never clear", out.Data.Sources.Unreachable)
	}
	// The live source is still counted: a filter that excluded everything would
	// pass the assertion above for the wrong reason.
	if out.Data.Sources.Unknown+out.Data.Sources.Healthy+out.Data.Sources.Degraded != 0 {
		t.Errorf("the other statuses total %d, want 0 — one source is live and it is unreachable",
			out.Data.Sources.Unknown+out.Data.Sources.Healthy+out.Data.Sources.Degraded)
	}
	// The same rows inflated these two, and the same join fixes them.
	if out.Data.Sources.MaxClockSkewMS != 500 {
		t.Errorf("max_clock_skew_ms = %d, want 500 — the worst skew oto ever measured on a "+
			"source it no longer polls is not a fact about the estate",
			out.Data.Sources.MaxClockSkewMS)
	}
	if out.Data.Sources.TotalDivergence != 2 {
		t.Errorf("total_divergence = %d, want 2 — a divergence count from a retired "+
			"Alertmanager is a canary nobody can act on", out.Data.Sources.TotalDivergence)
	}
}

// markUnreachable puts one source's health row where three consecutive failures
// would have put it. `last_error` is not decoration: `source_health_error_ck`
// refuses an unreachable row without one.
func markUnreachable(t *testing.T, e *env, sourceID uuid.UUID, skewMS int64, divergence int) {
	t.Helper()
	tag, err := e.pool.Exec(e.ctx, `
UPDATE source_health
   SET status = 'unreachable', last_error = 'dial tcp: i/o timeout',
       consecutive_failures = 3, clock_skew_ms = $2, divergence_count = $3
 WHERE source_id = $1`, sourceID, skewMS, divergence)
	if err != nil {
		t.Fatalf("mark %s unreachable: %v", sourceID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("mark %s unreachable touched %d rows — creating a source is supposed to seed "+
			"its health row", sourceID, tag.RowsAffected())
	}
}
