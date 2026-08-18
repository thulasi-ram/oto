package repository

// This file is INSIDE the package on purpose, for the reason
// internal/ingestion/repository/pruning_test.go and
// internal/stats/repository/rollup_plan_test.go are: it asserts a property of
// the SQL TEXT itself — that the only read of a generation's current members
// reaches them through an index in the order it wants them, rather than reading
// the whole membership and sorting it — and the only honest way to assert that
// is to EXPLAIN the exact constant the repository runs. A copy of the query in
// an external test would pass forever after the real one drifted.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// previewLimit is what `Service.Get` asks for: the preview bound plus the one
// extra row `ListCurrentMembers` reads to answer `has_more`.
//
// It is DERIVED rather than written out. The bound lives in `grouping/domain`
// precisely so this file can name it: `service` imports `repository`, so a
// preview bound declared in `service` could not be named from an in-package
// repository test at all — that is an import cycle, and it is how this constant
// came to be a hand-written `21`, a third copy of a number with one owner.
const previewLimit = domain.MemberPreviewLimit + 1

// TestTheCurrentMemberReadRidesTheIndexAndNeverSorts is the verification behind
// 00051's `case_group_live_idx`, and it is the successor to the same assertion
// 00044 made about `gm_current_idx`.
//
// ⭐ THE CLAIM BEING TESTED. `listCurrentMembersSQL` asks for `org_id = $1 AND
// group_id = $2 AND ended_at IS NULL`, ordered `(started_at DESC, id DESC)`, with
// a LIMIT. The two access paths `alert_cases` already carried for a group
// cannot serve it: `case_group_idx (org_id, group_id, started_at DESC)` offers the
// leading sort column and NO TIEBREAK for the keyset's row comparison, and it
// spans the generation's whole history, so the liveness predicate becomes a heap
// filter over every episode the generation ever held; `case_one_open_idx` is
// UNIQUE (alert_id) and supplies no group at all. So the LIMIT would bound the
// ANSWER and nothing would bound the WORK.
//
// ⭐ WHY "NO SORT NODE" IS THE ASSERTION, and not a row count. A Sort must
// consume its ENTIRE input before it can emit the first row, so a plan with a
// Sort in it reads every live member of the generation no matter how small the
// LIMIT is — which is precisely the defect, moved from Go into Postgres. An
// ordered index scan under a Limit stops. The absence of the node IS the bound,
// and it is a structural fact rather than an estimate.
//
// ⛔ AND A PLAN TEST THAT WOULD PASS WITHOUT THE INDEX PROVES NOTHING, so the
// control is not decoration. The same statement is EXPLAINed a second time
// inside a transaction that has DROPPED case_group_live_idx and is then rolled
// back: same rows, same statistics, one difference. There the plan must sort — if
// it does not, the assertion above was never evidence that 00051 changed
// anything.
//
// The table is seeded and ANALYZEd rather than left empty because the question
// is a COST question: on an empty table a sequential scan is correct and the
// planner would be right to choose one, so an empty-table assertion would be
// about nothing.
func TestTheCurrentMemberReadRidesTheIndexAndNeverSorts(t *testing.T) {
	t.Parallel()

	h := harness.New(t)

	// One tenant, deliberately, for the reason the rollup plan test gives: a
	// second org would make `org_id = $1` selective on its own and hand the
	// control an index it could ride for the tenant predicate alone.
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)

	// The generation under test, and a decoy beside it. Without the decoy
	// `group_id = $2` selects the whole table and "it used the index" would be
	// indistinguishable from "there was nothing else to read". The decoy carries
	// LIVE episodes of its own, so `case_group_live_idx` holds more than one
	// generation and the group predicate is doing real work inside it.
	// ⭐ THE DECOY MUST DIFFER ON AN AXIS. Since ADR 0038 the key is derived from
	// `(cluster, alertname, namespace-or-∅)`, so two groups separated only by
	// `severity` would hash to ONE key and this test would silently stop having a
	// second generation to discriminate against.
	group := h.GroupWith(org, source, cluster, map[string]string{"alertname": "HighErrorRate"})
	decoy := h.GroupWith(org, source, cluster, map[string]string{"alertname": "DiskFilling"})

	// ⭐ THE STORM THE CODE ITSELF NAMES, seeded the only way the schema now
	// permits. `member.go` calls "a storm of five thousand" the case the old
	// full-membership fetch was wrong for. Since membership IS the episode,
	// `case_one_open_idx` — UNIQUE (alert_id) WHERE ended_at IS NULL — caps the LIVE
	// membership at one episode per alert, which is itself the defect this change
	// closes: the join table could list one alert twice, and this cannot.
	//
	// So: 200 alerts × 25 episodes = 5 000 members of the generation, of which 200
	// are live. The live ones are every 25th row in `started_at` order, so a plan
	// that reached for `case_group_idx` would have to walk ~525 entries and filter
	// on the heap to find the 21 this read wants.
	const alertsInGroup = 200
	const alertsInDecoy = 40
	const episodesPerAlert = 25

	seedAlerts := func(prefix string, n int) []uuid.UUID {
		ids := make([]uuid.UUID, 0, n)
		for i := range n {
			a := h.AlertWith(org, cluster, map[string]string{
				"alertname": fmt.Sprintf("%s%03d", prefix, i),
				"severity":  "critical",
				"service":   "checkout",
			})
			ids = append(ids, a.ID)
		}
		return ids
	}

	// The LAST episode of each alert is the open one, which is what an episode
	// sequence means: 1..n-1 have ended, n is the firing one. Physical correlation
	// between `started_at` and insertion order is an input to the planner's choice,
	// so the rows are written in that order too.
	seed := func(groupID uuid.UUID, alertIDs []uuid.UUID) {
		h.Exec(`
			INSERT INTO alert_cases
			  (id, org_id, alert_id, group_id, seq, state, resolve_reason,
			   started_at, ended_at, last_observed_at, source_starts_at)
			SELECT gen_random_uuid(),
			       $1,
			       ($2::uuid[])[a + 1],
			       $3,
			       s,
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
			org.ID, alertIDs, groupID, episodesPerAlert, harness.Epoch, len(alertIDs))
	}
	seed(group.ID, seedAlerts("MemberPlanAlert", alertsInGroup))
	seed(decoy.ID, seedAlerts("MemberPlanDecoy", alertsInDecoy))

	// ⛔ WITHOUT THIS THE TEST IS A COIN TOSS. A freshly written table has
	// `reltuples = -1` and no histogram, so the planner assumes a handful of
	// pages and picks a sequential scan with the index and without it — and the
	// control would agree with the assertion for the wrong reason.
	h.Exec(`ANALYZE alert_cases`)

	withIndex := explainCurrentMembers(t, h, org.ID, group.ID)

	scan := scanOf(t, withIndex, "alert_cases")
	if scan.NodeType == "Seq Scan" {
		t.Fatalf("the current-member read scans alert_cases sequentially:\n%s",
			withIndex.pretty())
	}
	if !indexesUnder(scan)["case_group_live_idx"] {
		t.Fatalf("the current-member read reaches alert_cases by %q without naming "+
			"case_group_live_idx, so 00051's index is not what is serving `org_id = $1 AND group_id = "+
			"$2 AND ended_at IS NULL` ordered by (started_at DESC, id DESC):\n%s",
			scan.NodeType, withIndex.pretty())
	}
	if sorts := sortsIn(withIndex.root); len(sorts) > 0 {
		t.Fatalf("the current-member read still sorts (%s), so the LIMIT bounds the ANSWER and "+
			"not the WORK: a Sort consumes its whole input before it emits a row, which is the "+
			"entire live membership of the generation — the same fetch-everything-and-order-it the "+
			"detail page used to do in Go, moved one process away:\n%s",
			strings.Join(sorts, ", "), withIndex.pretty())
	}

	// ⭐ THE CONTROL. Postgres DDL is transactional, so the index can be taken
	// away, the same statement planned against the same rows and the same
	// statistics, and the whole thing rolled back — the database this test
	// borrowed is handed back exactly as it was found.
	without := explainWithoutTheIndex(t, h, org.ID, group.ID)

	if sorts := sortsIn(without.root); len(sorts) == 0 {
		t.Fatalf("the plan sorts nowhere even with case_group_live_idx dropped, so the assertion "+
			"above was not evidence that 00051 changed anything — some other access path is "+
			"already delivering (started_at DESC, id DESC) under the liveness predicate:\n%s",
			without.pretty())
	}
	if node := scanOf(t, without, "alert_cases"); indexesUnder(node)["case_group_live_idx"] {
		t.Fatalf("case_group_live_idx is still named after being dropped inside the control "+
			"transaction:\n%s", without.pretty())
	}
}

// explainCurrentMembers plans the real statement, first page, at the limit
// `Service.Get`'s preview asks for.
//
// EXPLAIN without ANALYZE never executes, so nothing is read and nothing is
// written. The cursor parameters are NULL and the nil UUID, which is the shape
// `ListCurrentMembers` sends for a first page.
func explainCurrentMembers(t *testing.T, h *harness.H, org, group uuid.UUID) plan {
	t.Helper()

	var afterAt *time.Time
	var raw []byte
	if err := h.Pool.QueryRow(h.Ctx,
		"EXPLAIN (FORMAT JSON) "+listCurrentMembersSQL,
		org, group, afterAt, uuid.Nil, previewLimit).Scan(&raw); err != nil {
		t.Fatalf("explain the current-member read: %v", err)
	}
	return parsePlan(t, raw)
}

// explainWithoutTheIndex plans the same statement in a transaction that has
// dropped 00051's index, then rolls it back.
func explainWithoutTheIndex(t *testing.T, h *harness.H, org, group uuid.UUID) plan {
	t.Helper()

	tx, err := h.Pool.Begin(h.Ctx)
	if err != nil {
		t.Fatalf("begin the control transaction: %v", err)
	}
	// Registered before anything can fail inside the transaction, so a t.Fatalf
	// below still hands the database back unchanged.
	defer func() {
		if err := tx.Rollback(h.Ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll the control transaction back: %v", err)
		}
	}()

	if _, err := tx.Exec(h.Ctx, "DROP INDEX case_group_live_idx"); err != nil {
		t.Fatalf("drop case_group_live_idx inside the control transaction: %v — 00051 is what "+
			"creates it, so a missing index here means the migration did not run", err)
	}

	var afterAt *time.Time
	var raw []byte
	if err := tx.QueryRow(h.Ctx,
		"EXPLAIN (FORMAT JSON) "+listCurrentMembersSQL,
		org, group, afterAt, uuid.Nil, previewLimit).Scan(&raw); err != nil {
		t.Fatalf("explain the current-member read without its index: %v", err)
	}
	return parsePlan(t, raw)
}

// ------------------------------------------------------------------ plan trees

// node is the part of an EXPLAIN (FORMAT JSON) node this test reads. Subplans,
// InitPlans and bitmap children all hang off `Plans`, so one recursive walk
// reaches every node in the statement.
type node struct {
	NodeType  string `json:"Node Type"`
	Relation  string `json:"Relation Name"`
	IndexName string `json:"Index Name"`
	Plans     []node `json:"Plans"`
}

// plan is one EXPLAIN result: the root node, plus the JSON it came from so a
// failure can print the plan that caused it.
type plan struct {
	root node
	raw  []byte
}

func (p plan) pretty() string { return string(p.raw) }

func parsePlan(t *testing.T, raw []byte) plan {
	t.Helper()

	var out []struct {
		Plan node `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode the plan: %v\n%s", err, raw)
	}
	if len(out) != 1 {
		t.Fatalf("EXPLAIN returned %d plans, want 1:\n%s", len(out), raw)
	}
	return plan{root: out[0].Plan, raw: raw}
}

// sortsIn names every ordering node in the tree. The match is on the SUFFIX
// "Sort" rather than on equality because Postgres spells the node three ways —
// "Sort", "Incremental Sort" and (inside a parallel plan) "Gather Merge" over
// one — and only the first is the one a naive assertion would look for.
func sortsIn(n node) []string {
	var found []string
	var walk func(node)
	walk = func(cur node) {
		if strings.HasSuffix(cur.NodeType, "Sort") {
			found = append(found, cur.NodeType)
		}
		for _, child := range cur.Plans {
			walk(child)
		}
	}
	walk(n)
	return found
}

// scanOf finds the node that reads `table`. A Seq Scan, an Index Scan and the
// Bitmap Heap Scan of a bitmap pair all carry `Relation Name`; the Bitmap Index
// Scan underneath does not, which is why the index name is looked for
// separately.
func scanOf(t *testing.T, p plan, table string) node {
	t.Helper()

	var found []node
	var walk func(n node)
	walk = func(n node) {
		if n.Relation == table {
			found = append(found, n)
		}
		for _, child := range n.Plans {
			walk(child)
		}
	}
	walk(p.root)

	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatalf("no node in the plan reads %s, so this test is asserting about a query that no "+
			"longer touches it:\n%s", table, p.pretty())
	default:
		t.Fatalf("%d nodes read %s; listCurrentMembersSQL scans it once, so the assertion below "+
			"would be ambiguous:\n%s", len(found), table, p.pretty())
	}
	return node{}
}

// indexesUnder is every index named at or below n — the node itself for an Index
// Scan or Index Only Scan, a child for a Bitmap Heap Scan.
func indexesUnder(n node) map[string]bool {
	names := map[string]bool{}
	var walk func(node)
	walk = func(cur node) {
		if cur.IndexName != "" {
			names[cur.IndexName] = true
		}
		for _, child := range cur.Plans {
			walk(child)
		}
	}
	walk(n)
	return names
}
