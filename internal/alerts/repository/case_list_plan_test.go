package repository

// This file is INSIDE the package for the reason labels_plan_test.go and
// internal/grouping/repository/member_plan_test.go are: it asserts a property of
// the SQL TEXT — that the ONE liveness axis `GET /api/v1/cases` has left reaches
// a partial index — and the only honest way to assert that is to EXPLAIN the
// exact constants `ListCases` assembles. A copy of the statement in an external
// test would pass forever after the real one drifted.
//
// TestMain lives in prune_test.go; this file adds no second one.

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/test/harness"
)

// caseListPlanLimit is a page of the default screen plus the row `PageOf` reads
// to answer `has_more`.
const caseListPlanLimit = 51

// TestTheStateFacetStillReachesThePartialAckIndex is the verification behind ADR
// 0040's collapse of `?open=` into `?state=`, and it is the assertion the ADR
// promised rather than assumed.
//
// ⭐⭐ THE CLAIM, AND WHY IT IS NOT OBVIOUS. `case_terminal_ended` proves
// `state = 'open'` and `ended_at IS NULL` select exactly the same rows, so to a
// READER the collapse is free. It is not free to the PLANNER: a partial index is
// matched against the query's own restriction clauses and NEVER against the
// table's CHECK constraints, so a statement that spelled the axis as
// `state = 'open'` would fall off `case_ack_idx (org_id, ack_state, started_at
// DESC, id DESC) WHERE ended_at IS NULL` and onto the full `case_started_idx`,
// filtering both equalities on the heap. `ListCases` therefore emits the axis AS
// the liveness literal — and this is what stops a later edit "tidying" it back
// into the state word.
//
// ⛔ THE CONTROL IS THE POINT OF THE FILE, and it is the opposite shape to the
// usual one: instead of dropping the index and proving the plan degrades, it
// plans the SAME query with the state word in place of the liveness literal and
// proves THAT degrades. The index is present throughout. What is under test is
// the spelling, so the spelling is what varies.
//
// The table is seeded and ANALYZEd rather than left empty because this is a COST
// question: on an empty table a sequential scan is correct and the planner would
// be right to choose one.
func TestTheStateFacetStillReachesThePartialAckIndex(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)
	group := h.GroupWith(org, source, cluster, map[string]string{"alertname": "CaseListPlan"})

	// 160 alerts × 25 episodes = 4 000 cases, of which 160 are open — the shape a
	// real tenant has, where the live set is a small minority of the history and a
	// heap filter over the whole table is therefore expensive. One tenant only:
	// a second org would make `org_id = $1` selective on its own and hand the
	// control an index it could ride for the tenant predicate alone.
	const alertsInOrg = 160
	const episodesPerAlert = 25

	alertIDs := make([]uuid.UUID, 0, alertsInOrg)
	for i := range alertsInOrg {
		a := h.AlertWith(org, cluster, map[string]string{
			"alertname": fmt.Sprintf("CaseListPlanAlert%03d", i),
			"severity":  "critical",
			"service":   "checkout",
		})
		alertIDs = append(alertIDs, a.ID)
	}

	// The LAST episode of each alert is the open one, which is what an episode
	// sequence means: 1..n-1 have closed, n is running. Two thirds of the open
	// ones are unacked, so `ack_state` is doing real work inside the index rather
	// than matching everything it can see.
	h.Exec(`
		INSERT INTO alert_cases
		  (id, org_id, alert_id, group_id, seq, state, resolve_reason, ack_state,
		   acked_at, acked_by_label,
		   started_at, ended_at, last_observed_at, source_starts_at)
		SELECT gen_random_uuid(),
		       $1,
		       ($2::uuid[])[a + 1],
		       $3,
		       s,
		       CASE WHEN s = $4 THEN 'open' ELSE 'closed' END,
		       CASE WHEN s = $4 THEN NULL ELSE 'upstream' END,
		       CASE WHEN s = $4 AND a % 3 = 0 THEN 'acked' ELSE 'unacked' END,
		       CASE WHEN s = $4 AND a % 3 = 0
		            THEN $5::timestamptz + ((a * $4 + s) * interval '1 second') END,
		       CASE WHEN s = $4 AND a % 3 = 0 THEN 'Ada Lovelace' END,
		       $5::timestamptz + ((a * $4 + s) * interval '1 second'),
		       CASE WHEN s = $4 THEN NULL
		            ELSE $5::timestamptz + ((a * $4 + s) * interval '1 second')
		                 + interval '30 seconds' END,
		       $5::timestamptz + ((a * $4 + s) * interval '1 second') + interval '30 seconds',
		       $5::timestamptz + ((a * $4 + s) * interval '1 second')
		  FROM generate_series(0, $6 - 1) AS a,
		       generate_series(1, $4) AS s`,
		org.ID, alertIDs, group.ID, episodesPerAlert, harness.Epoch, len(alertIDs))

	// ⛔ WITHOUT THIS THE TEST IS A COIN TOSS. A freshly written table has
	// `reltuples = -1` and no histogram, so the planner assumes a handful of pages
	// and picks a sequential scan with the index and without it.
	h.Exec(`ANALYZE alert_cases`)
	h.Exec(`ANALYZE alerts`)

	// What `ListCases` builds for `?state=open&ack=unacked`: the single-value ack
	// arm, and the state axis spelled as the liveness literal.
	shipped := explainCaseList(t, h, org.ID, listCasesHead+ackEqualTo+stateOpen+listCasesTail)

	scan := scanOfRelation(t, shipped, "alert_cases")
	if scan.NodeType == "Seq Scan" {
		t.Fatalf("the default case page scans alert_cases sequentially:\n%s", shipped.pretty())
	}
	if !indexesNamedUnder(scan)["case_ack_idx"] {
		t.Fatalf("the default case page reaches alert_cases by %q without naming case_ack_idx, "+
			"so the ONE index this endpoint exists to ride is not serving it:\n%s",
			scan.NodeType, shipped.pretty())
	}
	if !condMentions(scan, "ack_state") {
		t.Fatalf("ack_state is not an Index Cond, so the single-value ack arm has stopped being "+
			"an equality and the LIMIT no longer stops the scan:\n%s", shipped.pretty())
	}
	if n := sortNodes(shipped.root); n > 0 {
		t.Fatalf("the default case page sorts (%d node(s)), so the LIMIT bounds the ANSWER and "+
			"not the WORK — 00053 widened case_ack_idx by `id` precisely so it would not:\n%s",
			n, shipped.pretty())
	}

	// ⭐ THE CONTROL. The same statement, the same rows, the same statistics, the
	// same indexes — and the state axis spelled as the state word. If this reached
	// case_ack_idx too, the spliced literal above would be pointless and the
	// endpoint could take a bound parameter like every other dimension.
	asTheWord := explainCaseList(t, h, org.ID,
		listCasesHead+ackEqualTo+"\n   AND state = 'open'"+listCasesTail)

	if indexesNamedUnder(scanOfRelation(t, asTheWord, "alert_cases"))["case_ack_idx"] {
		t.Fatalf("`state = 'open'` reached case_ack_idx on its own, which would mean Postgres now "+
			"proves a partial index predicate from a CHECK constraint. If that is real, the "+
			"spliced liveness literal in ListCases is dead weight and ADR 0040 §5 is out of "+
			"date:\n%s", asTheWord.pretty())
	}
}

// TestTheClosedPageFallsToTheFullIndexOnPurpose records the other half of the
// axis, so that "it does not ride a partial index" is a documented answer rather
// than an undiscovered gap.
//
// `ended_at IS NOT NULL` is the exact complement of both partial predicates on
// this table, so NO partial index on live rows could serve a page of ended
// episodes. `case_started_idx (org_id, started_at, id)` carries the whole keyset
// sort key and is the right access path; what matters is that it is an ordered
// index scan and not a sort.
func TestTheClosedPageFallsToTheFullIndexOnPurpose(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)
	group := h.GroupWith(org, source, cluster, map[string]string{"alertname": "ClosedPagePlan"})

	const alertsInOrg = 160
	const episodesPerAlert = 25

	alertIDs := make([]uuid.UUID, 0, alertsInOrg)
	for i := range alertsInOrg {
		a := h.AlertWith(org, cluster, map[string]string{
			"alertname": fmt.Sprintf("ClosedPagePlanAlert%03d", i),
			"severity":  "critical",
		})
		alertIDs = append(alertIDs, a.ID)
	}
	h.Exec(`
		INSERT INTO alert_cases
		  (id, org_id, alert_id, group_id, seq, state, resolve_reason,
		   started_at, ended_at, last_observed_at, source_starts_at)
		SELECT gen_random_uuid(), $1, ($2::uuid[])[a + 1], $3, s,
		       CASE WHEN s = $4 THEN 'open' ELSE 'closed' END,
		       CASE WHEN s = $4 THEN NULL ELSE 'upstream' END,
		       $5::timestamptz + ((a * $4 + s) * interval '1 second'),
		       CASE WHEN s = $4 THEN NULL
		            ELSE $5::timestamptz + ((a * $4 + s) * interval '1 second')
		                 + interval '30 seconds' END,
		       $5::timestamptz + ((a * $4 + s) * interval '1 second') + interval '30 seconds',
		       $5::timestamptz + ((a * $4 + s) * interval '1 second')
		  FROM generate_series(0, $6 - 1) AS a,
		       generate_series(1, $4) AS s`,
		org.ID, alertIDs, group.ID, episodesPerAlert, harness.Epoch, len(alertIDs))
	h.Exec(`ANALYZE alert_cases`)
	h.Exec(`ANALYZE alerts`)

	p := explainCaseList(t, h, org.ID, listCasesHead+ackAnyOf+stateClosed+listCasesTail)

	scan := scanOfRelation(t, p, "alert_cases")
	if scan.NodeType == "Seq Scan" {
		t.Fatalf("the closed page scans alert_cases sequentially, so the org predicate is "+
			"reaching no index at all:\n%s", p.pretty())
	}
	if !indexesNamedUnder(scan)["case_started_idx"] {
		t.Fatalf("the closed page reaches alert_cases by %q without naming case_started_idx, "+
			"which is the only index that carries (org_id, started_at, id) for every row:\n%s",
			scan.NodeType, p.pretty())
	}
	if n := sortNodes(p.root); n > 0 {
		t.Fatalf("the closed page sorts (%d node(s)); case_started_idx carries the whole keyset "+
			"sort key and is read backwards, so a Sort here means the ORDER BY has drifted from "+
			"the index:\n%s", n, p.pretty())
	}
}

// explainCaseList plans a first page: no cursor, no alert-side facets, synthetic
// excluded — the shape the default screen sends. EXPLAIN without ANALYZE never
// executes, so nothing is read and nothing is written.
func explainCaseList(t *testing.T, h *harness.H, org uuid.UUID, sql string) labelPlan {
	t.Helper()

	var (
		acks      = []string{"unacked"}
		groupIDs  []uuid.UUID
		since     *time.Time
		sev       []string
		ns        []string
		clusters  []string
		names     []string
		cursorAt  *time.Time
		cursorID  = uuid.Nil
		synthetic = false
	)
	var raw []byte
	if err := h.Pool.QueryRow(h.Ctx, "EXPLAIN (FORMAT JSON) "+sql,
		org, acks, groupIDs, since, synthetic, sev, ns, clusters, names,
		cursorAt, cursorID, caseListPlanLimit).Scan(&raw); err != nil {
		t.Fatalf("explain the case list: %v", err)
	}
	return parseLabelPlan(t, raw)
}
