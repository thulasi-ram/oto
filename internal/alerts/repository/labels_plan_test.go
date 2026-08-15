package repository

// This file is INSIDE the package for the reason
// internal/stats/repository/rollup_plan_test.go and
// internal/ingestion/repository/pruning_test.go are: it asserts a property of
// the SQL TEXT itself — that the two label typeaheads reach a bounded projection
// instead of scanning the tenant's `alerts` — and the only honest way to assert
// that is to EXPLAIN the exact constants the repository runs. A copy of the
// queries in an external test would pass forever after the real ones drifted.
//
// TestMain lives in prune_test.go; this file adds no second one.

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// labelPlanAlerts is the alert estate the typeaheads are planned against: 12 000
// alerts carrying ten labels each, which is 120 000 projection rows.
//
// ⛔ WITHOUT VOLUME THIS TEST IS ABOUT NOTHING, for the reason the rollup plan
// test spells out: on a small table a sequential scan is CORRECT and the planner
// would be right to choose one, so the control and the assertion would agree for
// the wrong reason. The numbers are also the shape the ticket argued about —
// `alerts` is on ADR 0024's never-reaped list, so this is one install-month, not
// a worst case.
const labelPlanAlerts = 12000

// labelPlanNames is how many DISTINCT label names the org has. It is seeded
// separately and deliberately large.
//
// ⭐ WHY IT IS SEPARATE FROM THE ALERT COUNT, which is the whole argument for
// `alert_label_names` existing. The NAME typeahead's work is bounded by the
// org's LABEL cardinality; the alert count no longer enters it at all. Ten
// alerts with ten labels and ten million alerts with ten labels are the same
// query here. So the number that has to be big for the index question to be a
// real cost question is THIS one — at the dozen names a fresh install has, a Seq
// Scan of a single page is the right plan and asserting otherwise would be
// asserting a fiction. Two thousand is what a real estate looks like once
// kube-state-metrics, a few exporters and a relabelling config have all had an
// opinion.
const labelPlanNames = 2000

// TestTheLabelTypeaheadsNeverScanAlerts is the verification behind 00045.
//
// ⭐ THE CLAIM BEING TESTED, in two halves that fail differently.
//
// The STRUCTURAL half is that neither statement reads `alerts` at all. That is
// the ticket's actual complaint and it does not depend on a cost model: `alerts`
// is never reaped, so a per-keystroke read of it is unbounded for the life of
// the install no matter which plan the planner picks today. It is asserted at
// any table size.
//
// The COST half is that each statement is DRIVEN by the index 00045 created —
// `alert_label_names_rank_idx` for the names, `alert_labels_value_idx` for the
// values — rather than merely happening to touch the right table. A projection
// that is sequentially scanned per keystroke is a smaller version of the same
// bug.
//
// ⛔ AND A PLAN TEST THAT WOULD PASS WITHOUT THE INDEX PROVES NOTHING, so each
// cost assertion has a control: the same statement is EXPLAINed again inside a
// transaction that has DROPPED that index and is then rolled back. Sequential
// scans there, index scans here, same rows, same statistics, one difference.
func TestTheLabelTypeaheadsNeverScanAlerts(t *testing.T) {
	t.Parallel()

	h := harness.New(t)

	// One tenant, deliberately — a second org would make `org_id = $1` selective
	// on its own and hand the control an index it could ride for the tenant
	// predicate alone.
	org := h.Org()
	cluster := h.Cluster(org)

	// ⚠️ SEEDED AS SQL RATHER THAN THROUGH `UpsertBatch`, and the projection is
	// seeded with the SAME EXPRESSION 00045's backfill uses. Driving twelve
	// thousand alerts through the ingest path would be testing the writer, and
	// this test is about the reader; what it needs is a database in the state the
	// writer leaves, which is exactly what the backfill statement defines.
	//
	// `alert_key` is `ak_` plus 26 characters of [0-9a-v] (alerts_key_ck) and
	// `source_fingerprint` is 16 hex characters (alerts_srcfp_ck); hex satisfies
	// both alphabets, so `to_hex` covers them without the test having to care.
	h.Exec(`
		INSERT INTO alerts
		  (id, org_id, cluster_id, alert_key, source_fingerprint, alertname, severity,
		   cluster_key, labels, state, first_seen_at, last_seen_at, last_state_change_at,
		   total_occurrences, synthetic)
		SELECT gen_random_uuid(), $1, $2,
		       'ak_' || lpad(to_hex(i), 26, '0'),
		       lpad(to_hex(i), 16, '0'),
		       'PlanAlert' || (i % 500),
		       (ARRAY['critical','warning','info'])[(i % 3) + 1],
		       $3,
		       jsonb_build_object(
		         'alertname', 'PlanAlert' || (i % 500),
		         'severity',  (ARRAY['critical','warning','info'])[(i % 3) + 1],
		         'team',      'team' || (i % 12),
		         'namespace', 'ns' || (i % 20),
		         'service',   'svc' || (i % 40),
		         'instance',  'host-' || i || '.internal:9100',
		         'pod',       'pod-' || i,
		         'container', 'c' || (i % 60),
		         'endpoint',  'ep' || (i % 30),
		         'job',       'job' || (i % 25)),
		       'firing', $4, $4, $4, 1, false
		  FROM generate_series(1, $5) AS i`,
		org.ID, cluster.ID, cluster.Key.String(), h.Now(), labelPlanAlerts)

	h.Exec(`
		INSERT INTO alert_labels (org_id, alert_id, label_name, label_value)
		SELECT a.org_id, a.id, e.key, coalesce(e.value, '')
		  FROM alerts a, LATERAL jsonb_each_text(a.labels) AS e(key, value)
		 WHERE a.org_id = $1 AND NOT a.synthetic`, org.ID)

	h.Exec(`
		INSERT INTO alert_label_names (org_id, label_name, alert_count)
		SELECT org_id, label_name, count(*)
		  FROM alert_labels WHERE org_id = $1 GROUP BY org_id, label_name`, org.ID)

	// The long tail of label names. These carry no `alert_labels` rows, which is
	// correct for what they are here to do: the name typeahead reads
	// `alert_label_names` and nothing else, so the cardinality of THAT table is
	// the only input its plan has.
	h.Exec(`
		INSERT INTO alert_label_names (org_id, label_name, alert_count)
		SELECT $1, 'plan_label_' || lpad(i::text, 6, '0'), (i % 40) + 1
		  FROM generate_series(1, $2) AS i`, org.ID, labelPlanNames)

	// ⛔ WITHOUT THIS THE TEST IS A COIN TOSS. A freshly written table has
	// `reltuples = -1` and no histogram, so the planner assumes a handful of
	// pages and picks a sequential scan with the index and without it, and the
	// control would agree with the assertion for the wrong reason.
	h.Exec(`ANALYZE alerts, alert_labels, alert_label_names`)

	// ---------------------------------------------------------------- the names
	//
	// The EMPTY prefix, on purpose: it is the state the typeahead OPENS in, it is
	// the one keystroke that has no range to narrow, and it is therefore the case
	// the old statement handled worst — a full aggregate of the tenant to return
	// at most two hundred rows.
	names := explainLabelQuery(t, h, distinctLabelNamesSQL, org.ID, "", 200)
	assertNothingReadsAlerts(t, names, "the label NAME typeahead")

	if node := scanOfRelation(t, names, "alert_label_names"); node.NodeType == "Seq Scan" {
		t.Fatalf("the label name typeahead sequentially scans alert_label_names — 00045's "+
			"projection is bounded by label cardinality, but a per-keystroke full pass over it "+
			"is the same bug at a smaller size:\n%s", names.pretty())
	} else if !indexesNamedUnder(node)["alert_label_names_rank_idx"] {
		t.Fatalf("the label name typeahead reaches alert_label_names by %q without naming "+
			"alert_label_names_rank_idx, so 00045's index is not what serves `org_id = $1 AND "+
			"alert_count > 0 ORDER BY alert_count DESC, label_name`:\n%s",
			node.NodeType, names.pretty())
	}

	// ⭐ AND NO SORT, which is a separate claim from "an index was used". The
	// index carries `alert_count DESC, label_name`, so the LIMIT terminates the
	// scan. A plan that used the index and then sorted would have read every name
	// in the org before returning twenty-five of them, which is precisely the
	// mistake the old note made about LIMIT.
	if sortNodes(names.root) != 0 {
		t.Fatalf("the label name typeahead sorts before its LIMIT, so it reads every name in "+
			"the org to return a page of them — alert_label_names_rank_idx exists to supply "+
			"`alert_count DESC, label_name` as the index order:\n%s", names.pretty())
	}

	// --------------------------------------------------------------- the values
	//
	// `instance` with a `host-1` prefix: the highest-cardinality label in the
	// seed, narrowed by a prefix. It is the case the ticket names — "a longer
	// prefix narrows the result and not the work" — so a plan whose Index Cond
	// does NOT carry the prefix would be the defect surviving the migration.
	values := explainLabelQuery(t, h, distinctLabelValuesSQL, org.ID, "instance", "host-1", 200)
	assertNothingReadsAlerts(t, values, "the label VALUE typeahead")

	valueScan := scanOfRelation(t, values, "alert_labels")
	if valueScan.NodeType == "Seq Scan" {
		t.Fatalf("the label value typeahead sequentially scans alert_labels:\n%s", values.pretty())
	}
	if !indexesNamedUnder(valueScan)["alert_labels_value_idx"] {
		t.Fatalf("the label value typeahead reaches alert_labels by %q without naming "+
			"alert_labels_value_idx:\n%s", valueScan.NodeType, values.pretty())
	}
	// ⭐ THE PREFIX HAS TO BE IN THE INDEX CONDITION, not in a Filter above it.
	// An index that is used for `(org_id, label_name)` and then filters the
	// prefix has narrowed the RESULT and not the WORK, which is the exact
	// sentence the ticket used about the old query.
	if !condMentions(valueScan, "label_value") {
		t.Fatalf("alert_labels_value_idx is used but its Index Cond does not constrain "+
			"label_value, so the prefix is being applied after the scan and a longer prefix "+
			"still costs the same — text_pattern_ops is what turns `LIKE $3 || '%%'` into a "+
			"range:\n%s", values.pretty())
	}

	// ⭐ THE CONTROLS. Postgres DDL is transactional, so each index can be taken
	// away, the same statement planned against the same rows and the same
	// statistics, and the whole thing rolled back — the database this test
	// borrowed is handed back exactly as it was found.
	withoutNames := explainWithoutIndex(t, h, "alert_label_names_rank_idx",
		distinctLabelNamesSQL, org.ID, "", 200)
	if node := scanOfRelation(t, withoutNames, "alert_label_names"); node.NodeType != "Seq Scan" {
		t.Fatalf("alert_label_names is reached by %q even with alert_label_names_rank_idx "+
			"dropped, so the assertion above was not evidence that 00045 changed anything:\n%s",
			node.NodeType, withoutNames.pretty())
	}

	withoutValues := explainWithoutIndex(t, h, "alert_labels_value_idx",
		distinctLabelValuesSQL, org.ID, "instance", "host-1", 200)
	if node := scanOfRelation(t, withoutValues, "alert_labels"); node.NodeType != "Seq Scan" {
		t.Fatalf("alert_labels is reached by %q even with alert_labels_value_idx dropped, so "+
			"the assertion above was not evidence that 00045 changed anything:\n%s",
			node.NodeType, withoutValues.pretty())
	}
}

// ------------------------------------------------ the bound that counts OCTETS

// TestIngestSurvivesALabelThatCannotBeIndexed is the regression for the defect
// that would have taken ingest down, and it is written as the exact shape that
// took it down rather than as a large-ish label.
//
// ⛔ THE DEFECT. `alert_labels_value_idx` was PARTIAL on `length(label_value) <=
// 512`. `length()` counts CHARACTERS; the btree tuple cap is 2704 BYTES. So a
// value of 512 ASTRAL code points — 2048 bytes — SATISFIES that predicate, which
// means the partial index does not exclude the row, it ADMITS it. And a row a
// partial index admits but cannot fit does not skip the index, it ERRORS:
//
//	ERROR:  index row size 3104 exceeds btree version 4 maximum 2704
//	        for index "alert_labels_value_idx"
//
// Since 00045 made `upsertAlertsSQL` a writer of this index, that error fails the
// WHOLE ingest transaction — every alert in the batch, not just this one — on a
// label set the domain accepts without complaint. B4 admits a 1024-byte name and
// B5 a 4096-byte value, and `NewLabels` passes the pair below on the first try.
//
// ⛔ AND WHY THE NAME IS PART OF THE SHAPE. The index row is `(org_id,
// label_name, label_value)`, so `label_name` is IN it: a value alone cannot reach
// 2704 bytes, and what overflows is the SUM. A fix bounding only the value would
// have been unfalsifiable by any test that did not also carry a large name, which
// is why this one carries the largest name B4 allows.
//
// ⚠️ THE DATA MUST BE INCOMPRESSIBLE, and this is the trap that makes the bug
// survive a plausible test. Index tuples may hold pglz-compressed varlena values,
// so 512 REPEATED astral characters compress to a few dozen bytes and insert
// perfectly happily — a test written with `repeat()` passes against the broken
// index and proves nothing. Both generators below are seeded PRNGs: deterministic
// so a failure reproduces, and high-entropy so nothing compresses away.
func TestIngestSurvivesALabelThatCannotBeIndexed(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	repo := NewAlertRepository(h.Pool, h.Clock)

	name := incompressibleName(domain.MaxLabelNameBytes)
	value := incompressibleAstral(512)

	// ⭐ THE SHAPE IS ASSERTED BEFORE IT IS USED, because every claim above depends
	// on these three numbers and a generator that quietly drifted would turn this
	// test into one that passes for the wrong reason. 512 characters and 2048 bytes
	// is the whole point: the old predicate read the first number, the btree
	// enforces the second.
	require.Len(t, []byte(name), 1024, "the name must be exactly B4, the largest the domain admits")
	require.Equal(t, 512, utf8.RuneCountInString(value),
		"the value must be exactly 512 CHARACTERS — the bound the old predicate measured, and "+
			"therefore the bound it believed this row satisfied")
	require.Len(t, []byte(value), 2048,
		"and exactly 2048 BYTES — the bound the btree actually enforces, and comfortably inside "+
			"B5's 4096, so nothing in the domain has any reason to object")

	severity := "critical"
	ls, err := domain.NewLabelSet(map[string]string{
		"alertname": "TheUnindexableOne",
		"severity":  severity,
		name:        value,
	})
	require.NoError(t, err,
		"the domain must ACCEPT this label set — if it did not, the ingest path could never "+
			"reach the index with it and there would be no defect to regress against")

	res, err := repo.UpsertBatch(h.Ctx, org.Scope, []domain.AlertUpsert{{
		ID:          id.New(),
		ClusterID:   cluster.ID,
		AlertKey:    domain.ComputeAlertKey(org.ID, cluster.Key, ls, nil),
		Fingerprint: domain.ComputeSourceFingerprint(ls),
		AlertName:   ls.AlertName(),
		Severity:    &severity,
		ClusterKey:  cluster.Key,
		Labels:      ls,
		State:       domain.StateFiring,
		SeenAt:      h.Now(),
	}})
	// ⭐ THIS IS THE ASSERTION. Everything below describes what survival LOOKS
	// like; this line is survival itself.
	require.NoError(t, err,
		"INGEST OUTAGE: one label set inside the bounds the domain already accepts failed the "+
			"whole upsert transaction. alert_labels_value_idx must be partial on OCTET_LENGTH of "+
			"BOTH columns, and distinctLabelValuesSQL must carry the same predicate verbatim")
	require.Len(t, res, 1)
	require.True(t, res[0].WasInserted)

	// ⭐ STORED IN FULL. The bound lives on the INDEX, not on the column, so
	// nothing is truncated and nothing is rejected — 00045 gives up typeahead
	// visibility for this one value and gives up nothing else. A "fix" that
	// clamped the value at write time would have corrupted alert identity.
	var stored string
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT label_value FROM alert_labels WHERE org_id = $1 AND label_name = $2`,
		org.ID, name).Scan(&stored))
	require.Equal(t, value, stored,
		"the projection must store the value verbatim: a label value is part of alert IDENTITY, "+
			"so it is stored as upstream said it or not at all")

	// ⭐ AND SIMPLY NOT TYPEAHEAD-VISIBLE, which is the cost being paid. The read
	// carries the index's predicate, so it cannot return a row the index does not
	// hold — the reader and the index agree by construction rather than by luck.
	vals, err := repo.DistinctLabelValues(h.Ctx, org.Scope, name, "", 200)
	require.NoError(t, err)
	require.Empty(t, vals,
		"a value too large to index must be invisible to the value typeahead, not an error and "+
			"not a partial result")

	// ⛔ AND THE REST OF THE ALERT IS PROJECTED NORMALLY, which is what separates
	// this from the projection silently dropping the alert. Only the one oversized
	// VALUE is out of bounds; the alert's other labels are counted, and the
	// oversized label's NAME is still offered by the name typeahead — it is a real
	// label on a real alert, it merely has no value the picker can show.
	sevs, err := repo.DistinctLabelValues(h.Ctx, org.Scope, "severity", "", 200)
	require.NoError(t, err)
	require.Equal(t, []domain.LabelCount{{Value: "critical", Count: 1}}, sevs,
		"the alert must still be projected: the unindexable value costs that one value its place "+
			"in the typeahead, not the alert its place in the projection")

	names, err := repo.DistinctLabelNames(h.Ctx, org.Scope, "", 200)
	require.NoError(t, err)
	byName := map[string]int{}
	for _, n := range names {
		byName[n.Value] = n.Count
	}
	require.Equal(t, 1, byName[name],
		"the oversized label's NAME is counted like any other — alert_label_names carries no "+
			"value, so nothing about it is out of bounds")
	require.Equal(t, 1, byName["alertname"])
}

// TestBoundingTheValueAloneWouldNotHaveBeenEnough is the test that fails against
// the OTHER plausible fix — the one that corrects `length` to `octet_length` and
// stops there.
//
// ⛔ THE INDEX ROW IS (org_id, label_name, label_value), SO THE NAME IS IN IT.
// A value alone cannot reach 2704 bytes once it is bounded at 512, so a
// value-only predicate looks sufficient and is not: what overflows is the SUM,
// and the trigger is `octet_length(name) + octet_length(value)` above roughly
// 2672. The shape below is measured to sit either side of that line — with only
// the value bounded it is 2744 bytes and ERRORS; with both bounded it is excluded
// and inserts. A test carrying a short name could not tell the two fixes apart.
//
// ⚠️ WHY IT WRITES DIRECTLY RATHER THAN THROUGH INGEST, which is not laziness.
// `NewLabels` admits only `[a-zA-Z_][a-zA-Z0-9_]*`, so a name that arrives
// through ingest is ASCII and its octet count equals its character count — the
// name conjunct can never fire on that path. It fires on the OTHER writer:
// `alert_labels_name_ck` bounds the name in CHARACTERS (1024 of them can be 4096
// bytes), and 00045's backfill/reconcile inserts straight out of `alerts.labels`
// on an install that may hold rows written before that charset was enforced. So
// the bound is asserted where that writer writes.
func TestBoundingTheValueAloneWouldNotHaveBeenEnough(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	alert := h.AlertWith(org, cluster, harness.DefaultLabels())

	// 550 astral code points: 550 CHARACTERS, so `alert_labels_name_ck` admits it,
	// and 2200 BYTES. Paired with a 512-byte value — which is exactly what a
	// value-only predicate would consider in bounds — the index row is 2744 bytes.
	name := incompressibleAstral(550)
	value := incompressibleName(512)
	require.Len(t, []byte(name), 2200)
	require.Len(t, []byte(value), 512)

	// The write must SUCCEED. A 54000 here means the index predicate is admitting a
	// row it cannot hold, which on the reconcile path is a migration that will not
	// apply on precisely the installs carrying the most data.
	h.Exec(`INSERT INTO alert_labels (org_id, alert_id, label_name, label_value)
	        VALUES ($1, $2, $3, $4)`, org.ID, alert.ID, name, value)

	vals, err := NewAlertRepository(h.Pool, h.Clock).
		DistinctLabelValues(h.Ctx, org.Scope, name, "", 200)
	require.NoError(t, err)
	require.Empty(t, vals, "a name too large to index is invisible to the typeahead, not an error")
}

// TestTheBackfillCannotBeBlockedByAnUnstorableName is the migration-ordering
// regression, and it guards the one hazard reordering CANNOT fix.
//
// ⛔ `alert_labels_pk` IS A PRIMARY KEY OVER (org_id, alert_id, label_name), AND
// A PRIMARY KEY CANNOT BE PARTIAL. So the name in it faces the same 2704-byte
// btree cap with no predicate available to exclude it, and no reordering helps
// either — the key is created with the table. Measured: a 700-code-point name is
// 2800 bytes and fails `alert_labels_pk` outright, while `alert_labels_name_ck`
// waves it through because that check counts CHARACTERS and 700 is under 1024.
//
// The domain cannot produce such a name (the label-name charset is ASCII-only),
// but the backfill does not read the domain — it reads `alerts.labels` as it
// finds it, on an install that may predate that enforcement. One such row is not
// a dropped label, it is a goose migration that will not apply. 00045's backfill
// therefore bounds the name in BYTES, which is the unit B4 is written in.
func TestTheBackfillCannotBeBlockedByAnUnstorableName(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)

	// A legacy alert, written straight to `alerts.labels` the way a release that
	// predates the charset could have. 700 code points: under B4 as characters,
	// 2800 bytes, and unstorable in the primary key.
	unstorable := incompressibleAstral(700)
	require.Len(t, []byte(unstorable), 2800)

	alert := h.AlertWith(org, cluster, harness.DefaultLabels())
	h.Exec(`UPDATE alerts SET labels = labels || jsonb_build_object($2::text, 'v')
	         WHERE id = $1`, alert.ID, unstorable)

	// Re-running 00045's backfill is what a migration does on a fresh install and
	// what an operator does after a rollout. It must not raise 54000.
	h.Exec(`
		INSERT INTO alert_labels (org_id, alert_id, label_name, label_value)
		SELECT a.org_id, a.id, e.key, coalesce(e.value, '')
		  FROM alerts a, LATERAL jsonb_each_text(a.labels) AS e(key, value)
		 WHERE NOT a.synthetic
		   AND octet_length(e.key) <= 1024
		    ON CONFLICT ON CONSTRAINT alert_labels_pk DO UPDATE
		   SET label_value = EXCLUDED.label_value
		 WHERE alert_labels.label_value IS DISTINCT FROM EXCLUDED.label_value`)

	// The storable labels are projected; the unstorable name is skipped, and
	// `alerts.labels` still holds it verbatim — it is excluded from a projection,
	// never deleted from the source.
	var projected, source int
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT (SELECT count(*) FROM alert_labels WHERE alert_id = $1),
		        (SELECT count(*) FROM alerts a, LATERAL jsonb_object_keys(a.labels) k
		          WHERE a.id = $1)`, alert.ID).Scan(&projected, &source))
	require.Equal(t, len(harness.DefaultLabels()), projected,
		"every storable label must be projected")
	require.Equal(t, projected+1, source,
		"and the unstorable one must still be in `alerts.labels`: the projection declines to "+
			"index it, the source of truth keeps it")
}

// incompressibleName builds a label name of exactly n BYTES that pglz cannot
// shrink. It is drawn from the label-name charset (`[a-zA-Z_][a-zA-Z0-9_]*`), so
// `NewLabels` accepts it and one byte is one character.
func incompressibleName(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_"
	r := rand.New(rand.NewPCG(0x0045, 0xB4B5))
	b := make([]byte, n)
	b[0] = 'l' // the charset forbids a leading digit
	for i := 1; i < n; i++ {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

// incompressibleAstral builds a string of exactly `runes` code points from the
// ASTRAL planes, which is exactly 4×runes bytes.
//
// ⭐ ASTRAL IS THE POINT, not decoration. U+10000 and above is where one
// character costs FOUR bytes, which is the widest possible gap between what
// `length()` measures and what a btree charges for — and therefore the shape that
// makes a character-based bound wrong by the largest margin. Every code point in
// [U+10000, U+10FFFF] is 4 bytes in UTF-8 and none is a surrogate, so the result
// is always valid UTF-8 and always passes `UnstorableReason`.
func incompressibleAstral(runes int) string {
	r := rand.New(rand.NewPCG(0x0045, 0x5EED))
	out := make([]rune, runes)
	for i := range out {
		out[i] = rune(0x10000 + r.IntN(0x100000))
	}
	return string(out)
}

// assertNothingReadsAlerts is the structural half, and the one that survives any
// change of planner mood: `alerts` is never reaped (ADR 0024), so a typeahead
// that reads it is unbounded regardless of how it reads it.
func assertNothingReadsAlerts(t *testing.T, p labelPlan, what string) {
	t.Helper()

	var walk func(n planNode) bool
	walk = func(n planNode) bool {
		if n.Relation == "alerts" {
			return true
		}
		for _, child := range n.Plans {
			if walk(child) {
				return true
			}
		}
		return false
	}
	if walk(p.root) {
		t.Fatalf("%s still reads `alerts` — 00045 exists so that it does not, because `alerts` "+
			"is on ADR 0024's never-reaped list and this query runs per keystroke:\n%s",
			what, p.pretty())
	}
}

// explainLabelQuery plans the real statement. EXPLAIN without ANALYZE never
// executes it.
func explainLabelQuery(t *testing.T, h *harness.H, sql string, args ...any) labelPlan {
	t.Helper()

	var raw []byte
	if err := h.Pool.QueryRow(h.Ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("explain the typeahead: %v", err)
	}
	return parseLabelPlan(t, raw)
}

// explainWithoutIndex plans the same statement in a transaction that has dropped
// one of 00045's indexes, then rolls it back.
func explainWithoutIndex(
	t *testing.T, h *harness.H, index, sql string, args ...any,
) labelPlan {
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

	if _, err := tx.Exec(h.Ctx, "DROP INDEX "+index); err != nil {
		t.Fatalf("drop %s inside the control transaction: %v — 00045 is what creates it, so a "+
			"missing index here means the migration did not run", index, err)
	}

	var raw []byte
	if err := tx.QueryRow(h.Ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("explain the typeahead without %s: %v", index, err)
	}
	return parseLabelPlan(t, raw)
}

// ------------------------------------------------------------------ plan trees

// planNode is the part of an EXPLAIN (FORMAT JSON) node these assertions read.
// CTE subplans, InitPlans and bitmap children all hang off `Plans`, so one
// recursive walk reaches every scan in the statement.
type planNode struct {
	NodeType    string     `json:"Node Type"`
	Relation    string     `json:"Relation Name"`
	IndexName   string     `json:"Index Name"`
	IndexCond   string     `json:"Index Cond"`
	RecheckCond string     `json:"Recheck Cond"`
	Plans       []planNode `json:"Plans"`
}

// labelPlan is one EXPLAIN result: the root node, plus the JSON it came from so
// a failure can print the plan that caused it.
type labelPlan struct {
	root planNode
	raw  []byte
}

func (p labelPlan) pretty() string { return string(p.raw) }

func parseLabelPlan(t *testing.T, raw []byte) labelPlan {
	t.Helper()

	var out []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode the plan: %v\n%s", err, raw)
	}
	if len(out) != 1 {
		t.Fatalf("EXPLAIN returned %d plans, want 1:\n%s", len(out), raw)
	}
	return labelPlan{root: out[0].Plan, raw: raw}
}

// scanOfRelation finds the node that reads `table`. A Seq Scan, an Index Scan
// and the Bitmap Heap Scan of a bitmap pair all carry `Relation Name`; the
// Bitmap Index Scan underneath does not, which is why the index name is looked
// for separately.
func scanOfRelation(t *testing.T, p labelPlan, table string) planNode {
	t.Helper()

	var found []planNode
	var walk func(n planNode)
	walk = func(n planNode) {
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
		t.Fatalf("%d nodes read %s; the typeahead scans it once, so the assertion below would "+
			"be ambiguous:\n%s", len(found), table, p.pretty())
	}
	return planNode{}
}

// indexesNamedUnder is every index named at or below n — the node itself for an
// Index Scan or Index Only Scan, a child for a Bitmap Heap Scan.
func indexesNamedUnder(n planNode) map[string]bool {
	names := map[string]bool{}
	var walk func(planNode)
	walk = func(cur planNode) {
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

// condMentions reports whether `column` appears in an Index Cond (or the
// Recheck Cond of a bitmap pair) at or below n. It is deliberately a substring
// test: the rendered condition is planner output and pinning its exact spelling
// would make this a test of Postgres's formatting.
func condMentions(n planNode, column string) bool {
	var walk func(planNode) bool
	walk = func(cur planNode) bool {
		if strings.Contains(cur.IndexCond, column) || strings.Contains(cur.RecheckCond, column) {
			return true
		}
		for _, child := range cur.Plans {
			if walk(child) {
				return true
			}
		}
		return false
	}
	return walk(n)
}

// sortNodes counts explicit Sort nodes anywhere in the plan.
func sortNodes(n planNode) int {
	total := 0
	if n.NodeType == "Sort" || n.NodeType == "Incremental Sort" {
		total++
	}
	for _, child := range n.Plans {
		total += sortNodes(child)
	}
	return total
}
