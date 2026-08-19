package repository

// This file is INSIDE the package on purpose, for the reason
// internal/ingestion/repository/pruning_test.go is: it asserts a property of the
// SQL TEXT itself — that `stats.rollup` reaches its two never-reaped source
// tables by an index range rather than by a sequential scan — and the only
// honest way to assert that is to EXPLAIN the exact constant the repository
// runs. A copy of the query in an external test would pass forever after the
// real one drifted.

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// TestTheRollupReachesBothTablesByIndex is the verification behind 00042.
//
// ⭐ THE CLAIM BEING TESTED. `rollupDaySQL`'s `ac` CTE filters
// `alert_cases` on `(org_id, started_at BETWEEN …)` and its `notif` CTE
// filters `notifications` on `(org_id, created_at BETWEEN …)`. Until 00042 there
// was no index on either table that could lead with that pair — every candidate
// demanded a second equality (alert_id, group_id, ack_state, subject_id,
// case_id) the rollup does not supply — so both CTEs were full scans, run
// `2 × orgs` times every fifteen minutes over two tables ADR 0024 never reaps.
//
// ⛔ AND A PLAN TEST THAT WOULD PASS WITHOUT THE INDEX PROVES NOTHING, so the
// control is not decoration. The same statement is EXPLAINed a second time
// inside a transaction that has DROPPED both indexes and is then rolled back:
// index scans there, sequential scans here, same rows, same statistics, one
// difference. Without that half, a test asserting "not a Seq Scan" could be
// satisfied by any index on the table, or by a planner that had simply been
// handed too few rows to care.
//
// The tables are seeded and ANALYZEd rather than left empty because the question
// is a COST question: on an empty table a sequential scan is correct and the
// planner would be right to choose one, so an empty-table assertion would be
// about nothing. Rows are written in `started_at` order, which is the order the
// ingest path writes them in, because physical correlation is an input to the
// planner's choice and inventing a shuffled table would be inventing a database
// oto does not produce.
func TestTheRollupReachesBothTablesByIndex(t *testing.T) {
	t.Parallel()

	h := harness.New(t)

	// One tenant, deliberately. A second org would make `org_id = $1` selective
	// on its own and hand the control an index it could ride for the tenant
	// predicate alone — which would prove the timestamp range unnecessary rather
	// than proving it served.
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)

	// Eight alerts and eight groups. The rollup GROUPs BY `(cluster_key,
	// alertname)`, and the `notif` CTE reaches `alerts` through
	// `alert_cases.group_id`, so both need a fan-out that is real but small: the
	// volume under test is on the two tables being scanned, not on their join
	// partners.
	const fanout = 8
	alertIDs := make([]uuid.UUID, 0, fanout)
	groupIDs := make([]uuid.UUID, 0, fanout)
	for i := range fanout {
		a := h.AlertWith(org, cluster, map[string]string{
			"alertname": fmt.Sprintf("RollupPlanAlert%02d", i),
			"severity":  "critical",
			"service":   "checkout",
		})
		// `shard` is not an axis (ADR 0038), so it cannot separate generations any
		// more; the alertname is what makes these N distinct groups instead of one.
		g := h.GroupWith(org, source, cluster, map[string]string{
			"alertname": fmt.Sprintf("RollupShard%02d", i),
			"severity":  "critical",
		})
		alertIDs = append(alertIDs, a.ID)
		groupIDs = append(groupIDs, g.ID)
	}

	// Two months of history at one episode every four minutes. Every episode is
	// CLOSED — `case_one_open_idx` permits one open episode per alert, and closed
	// is what the rollup counts anyway (`resolve_reason = 'upstream'`,
	// `resolve_reason = 'timeout'`), so an open-episode seed would be seeding rows
	// the CTE's own FILTERs discard.
	//
	// ⭐ `state` IS `closed` AND THE VERDICT IS `resolve_reason` (ADR 0040). The
	// column used to hold `resolved` and the FILTERs used to read it; since the
	// narrowing, `closed` says only THAT the episode ended and `resolve_reason` says
	// why. A seed still writing `resolved` here would not be counted differently —
	// it would be refused outright by `case_state_ck`.
	const rows = 20000
	base := harness.Epoch
	h.Exec(`
		INSERT INTO alert_cases
		  (id, org_id, alert_id, group_id, seq, state, resolve_reason,
		   started_at, ended_at, last_observed_at, source_starts_at)
		SELECT gen_random_uuid(),
		       $1,
		       ($2::uuid[])[(i % $4) + 1],
		       ($3::uuid[])[(i % $4) + 1],
		       i,
		       'closed',
		       'upstream',
		       $5::timestamptz + (i * interval '4 minutes'),
		       $5::timestamptz + (i * interval '4 minutes') + interval '9 minutes',
		       $5::timestamptz + (i * interval '4 minutes') + interval '9 minutes',
		       $5::timestamptz + (i * interval '4 minutes')
		  FROM generate_series(1, $6) AS i`,
		org.ID, alertIDs, groupIDs, fanout, base, rows)

	// ⭐ NO MEMBERSHIP SEED. Since 00051 the episodes above ARE the membership —
	// each carries the `group_id` the `notif` CTE joins on — so the row that used
	// to be inserted here would have been a copy of one already written. It also
	// means the CTE's join now fans out over every episode of a generation rather
	// than one per generation, which is why it counts DISTINCT notification and
	// delivery ids.

	// The same two months of notifications. `idempotency_key` is 64 hex
	// characters by `notifications_idem_ck` and unique per org by
	// `notifications_idem_uniq`; two md5s of the series index satisfy both
	// without the test having to care.
	h.Exec(`
		INSERT INTO notifications
		  (id, org_id, subject_kind, subject_id, group_id, conversation_kind, conversation_id, reason, state_version,
		   idempotency_key, created_at, updated_at)
		SELECT gen_random_uuid(),
		       $1,
		       'alert_group',
		       ($2::uuid[])[(i % $3) + 1],
		       ($2::uuid[])[(i % $3) + 1],
		       'alert_group',
		       ($2::uuid[])[(i % $3) + 1],
		       'fired',
		       1,
		       md5(i::text) || md5((i + 1)::text),
		       $4::timestamptz + (i * interval '4 minutes'),
		       $4::timestamptz + (i * interval '4 minutes')
		  FROM generate_series(1, $5) AS i`,
		org.ID, groupIDs, fanout, base, rows)

	// ⛔ WITHOUT THIS THE TEST IS A COIN TOSS. A freshly written table has
	// `reltuples = -1` and no histogram, so the planner assumes a handful of
	// pages and picks a sequential scan for both plans — with the index and
	// without it — and the control would agree with the assertion for the wrong
	// reason.
	h.Exec(`ANALYZE alert_cases, notifications, alerts`)

	day := base.Add(30 * 24 * time.Hour).UTC().Truncate(24 * time.Hour)
	asOf := day.Add(24 * time.Hour)

	withIndexes := explainRollup(t, h, org.ID, day, asOf)

	ac := scanOfCTE(t, withIndexes, "ac", "alert_cases")
	if ac.NodeType == "Seq Scan" {
		t.Fatalf("the ac CTE still scans alert_cases sequentially:\n%s", withIndexes.pretty())
	}
	if !indexesUnder(ac)["case_started_idx"] {
		t.Fatalf("the ac CTE reaches alert_cases by %q without naming case_started_idx, so "+
			"00042's index is not what is serving `org_id = $1 AND started_at >= … AND started_at "+
			"< …`:\n%s", ac.NodeType, withIndexes.pretty())
	}

	notif := scanOf(t, withIndexes, "notifications")
	if notif.NodeType == "Seq Scan" {
		t.Fatalf("the notif CTE still scans notifications sequentially:\n%s", withIndexes.pretty())
	}
	if !indexesUnder(notif)["notif_created_idx"] {
		t.Fatalf("the notif CTE reaches notifications by %q without naming notif_created_idx:\n%s",
			notif.NodeType, withIndexes.pretty())
	}

	// ⭐ THE CONTROL. Postgres DDL is transactional, so both indexes can be taken
	// away, the same statement planned against the same rows and the same
	// statistics, and the whole thing rolled back — the database this test
	// borrowed is handed back exactly as it was found.
	without := explainWithoutTheIndexes(t, h, org.ID, day, asOf)

	if node := scanOfCTE(t, without, "ac", "alert_cases"); node.NodeType != "Seq Scan" {
		t.Fatalf("alert_cases is reached by %q even with case_started_idx dropped, so the "+
			"assertion above was not evidence that 00042 changed anything:\n%s",
			node.NodeType, without.pretty())
	}
	if node := scanOf(t, without, "notifications"); node.NodeType != "Seq Scan" {
		t.Fatalf("notifications is reached by %q even with notif_created_idx dropped, so the "+
			"assertion above was not evidence that 00042 changed anything:\n%s",
			node.NodeType, without.pretty())
	}
}

// explainRollup plans the real statement and returns its root node.
//
// EXPLAIN without ANALYZE never executes, so the INSERT at the foot of
// `rollupDaySQL` writes nothing.
func explainRollup(t *testing.T, h *harness.H, org uuid.UUID, day, asOf time.Time) plan {
	t.Helper()

	var raw []byte
	if err := h.Pool.QueryRow(h.Ctx,
		"EXPLAIN (FORMAT JSON) "+rollupDaySQL, org, day, asOf).Scan(&raw); err != nil {
		t.Fatalf("explain the rollup: %v", err)
	}
	return parsePlan(t, raw)
}

// explainWithoutTheIndexes plans the same statement in a transaction that has
// dropped both of 00042's indexes, then rolls it back.
func explainWithoutTheIndexes(t *testing.T, h *harness.H, org uuid.UUID, day, asOf time.Time) plan {
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

	for _, index := range []string{"case_started_idx", "notif_created_idx"} {
		if _, err := tx.Exec(h.Ctx, "DROP INDEX "+index); err != nil {
			t.Fatalf("drop %s inside the control transaction: %v — 00042 is what creates it, so "+
				"a missing index here means the migration did not run", index, err)
		}
	}

	var raw []byte
	if err := tx.QueryRow(h.Ctx,
		"EXPLAIN (FORMAT JSON) "+rollupDaySQL, org, day, asOf).Scan(&raw); err != nil {
		t.Fatalf("explain the rollup without its indexes: %v", err)
	}
	return parsePlan(t, raw)
}

// ------------------------------------------------------------------ plan trees

// node is the part of an EXPLAIN (FORMAT JSON) node this test reads. CTE
// subplans, InitPlans and bitmap children all hang off `Plans`, so one recursive
// walk reaches every scan in the statement.
type node struct {
	NodeType  string `json:"Node Type"`
	Relation  string `json:"Relation Name"`
	IndexName string `json:"Index Name"`
	Subplan   string `json:"Subplan Name"`
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

// scanOf finds the node that reads `table`. A Seq Scan, an Index Scan and the
// Bitmap Heap Scan of a bitmap pair all carry `Relation Name`; the Bitmap Index
// Scan underneath does not, which is why the index name is looked for
// separately.
// scanOfCTE is scanOf narrowed to one CTE's subtree.
//
// ⭐ `alert_cases` IS READ TWICE SINCE 00051. The `ac` CTE reads the episodes that
// started on the day; the membership fan-out reads the same table again, because
// `alert_group_members` is gone and `alert_cases.group_id` IS the membership. A
// plain relation-name walk therefore finds two nodes and cannot say which one the
// assertion is about. EXPLAIN labels a CTE's own subtree with `Subplan Name`, so
// naming the CTE is how the right scan is picked.
func scanOfCTE(t *testing.T, p plan, cte, table string) node {
	t.Helper()

	var sub *node
	var find func(n node)
	find = func(n node) {
		if n.Subplan == "CTE "+cte {
			c := n
			sub = &c
			return
		}
		for _, child := range n.Plans {
			find(child)
		}
	}
	find(p.root)
	if sub == nil {
		t.Fatalf("the plan carries no CTE %q, so rollupDaySQL no longer has the shape this "+
			"test asserts about:\n%s", cte, p.pretty())
	}
	return scanOf(t, plan{root: *sub, raw: p.raw}, table)
}

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
		t.Fatalf("%d nodes read %s in this subtree; the assertion below would be "+
			"ambiguous:\n%s", len(found), table, p.pretty())
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
