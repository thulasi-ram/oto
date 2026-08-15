package repository

import (
	"regexp"
	"strings"
	"testing"
)

// listDuePredicates returns just the WHERE clause of `listDueSQL` — the region
// that decides which sources are polled.
//
// ⛔ THE SLICE IS THE POINT. The ban below is on PREDICATES, not on column names:
// `ListDue` returns whole `domain.Source` values, so its SELECT list legitimately
// projects every column including `push_enabled`, and a check against the raw
// statement text cannot tell a projected column from a conjunct. Scanning the
// whole string caught `s.push_enabled` in the SELECT list — a column that gates
// the INBOUND WEBHOOK and has nothing to do with reconciliation — and would have
// pushed the next reader toward deleting the guard rather than fixing it.
//
// Failing when the markers are missing is deliberate: a rewrite that moves the
// filtering into a CTE, a JOIN condition or a subquery has moved it somewhere
// this test no longer reads, and that must be a failure rather than a silent pass.
func listDuePredicates(t *testing.T) string {
	t.Helper()

	_, after, ok := strings.Cut(listDueSQL, "WHERE")
	if !ok {
		t.Fatal("listDueSQL has no WHERE clause: the fan-out's filtering has moved somewhere " +
			"this guard no longer reads, so re-point it before trusting it again")
	}
	before, _, ok := strings.Cut(after, "ORDER BY")
	if !ok {
		t.Fatal("listDueSQL has no ORDER BY: the fan-out's ordering guarantee (a never-reconciled " +
			"source sorts first) is gone, and this guard can no longer bound the predicate region")
	}
	return before
}

// optOutShapes are the ways a "stop polling this source" switch is spelled in
// SQL. This is a shape ban, not a column ban: `reconcile_enabled` was the one
// that existed, and the next one will have a different name.
var optOutShapes = regexp.MustCompile(`(?i)enabled|disabled|paused|suspended|\bactive\b|\bis\s+false\b|=\s*false`)

// ⛔⛔ TestListDueHasNoOptOutPredicate guards the one line that made ADR 0006's
// "mandatory" a lie.
//
// `listDueSQL` is the reconcile fan-out: every source it returns gets polled, and
// every source it omits does not. It used to carry `AND s.reconcile_enabled`, so
// one boolean column decided whether the only producer of `suppressed` — and the
// only writer that keeps `source_health` fresh — ever ran for a source. A source
// dropped from this query keeps its LAST health verdict forever, because nothing
// else moves that column, so the §B.4 reaper guard goes on trusting a `healthy`
// that is arbitrarily stale and ends episodes for alerts that are merely silenced
// upstream.
//
// The predicate list is therefore closed on purpose: a live source, and its own
// interval having elapsed. A deployment that must poll gently raises
// `reconcile_interval_s`; there is no column value here that means "never", and
// adding one back would recreate the defect in a single conjunct.
//
// This is a string assertion because the property is about the SQL TEXT and not
// about any row: the failure it prevents is somebody re-adding a filter, and that
// is visible without a database.
func TestListDueHasNoOptOutPredicate(t *testing.T) {
	t.Parallel()

	// The exact column is banned from the WHOLE statement, projection included:
	// migration 00038 dropped it, so naming it anywhere is a query that cannot run.
	if strings.Contains(listDueSQL, "reconcile_enabled") {
		t.Fatal("the reconcile fan-out names reconcile_enabled again; see ADR 0006's second " +
			"amendment, which dropped the column precisely so this could not come back")
	}

	// Everything else is banned only where it would DECIDE something. The rule is
	// exactly one sentence: the only reasons to skip a source are that it is
	// deleted or that its own interval has not elapsed. Any conjunct that reads a
	// boolean flag off the row — whatever it is called — is a way to switch off the
	// component ADR 0006 calls mandatory, and re-creates the defect that amendment
	// removed: a source dropped here keeps its LAST `source_health` verdict
	// forever, because nothing else moves that column, so the §B.4 reaper goes on
	// trusting a `healthy` that is arbitrarily stale and ends episodes for alerts
	// that are merely silenced upstream. A deployment that must poll gently raises
	// `reconcile_interval_s`; no column value may mean "never".
	//
	// ⚠️ `push_enabled` in the SELECT list is NOT that, and this test used to say
	// it was. It gates the inbound webhook endpoint (`SourceConfig.AcceptsPush`),
	// i.e. whether Alertmanager may push TO oto. A pull-only source is the one that
	// needs reconciling MOST — reconciliation is the only way oto sees it at all —
	// so `push_enabled` appearing as a predicate here would be a genuine second
	// opt-out, and it is correctly absent from the WHERE clause while being
	// correctly present in the projection.
	if m := optOutShapes.FindString(listDuePredicates(t)); m != "" {
		t.Fatalf("listDueSQL's WHERE clause grew an opt-out predicate (matched %q); the only "+
			"reasons to skip a source are that it is deleted or not yet due, and a boolean flag "+
			"that means 'never poll this' is how ADR 0006's 'mandatory' became a lie the first time", m)
	}
	// The two predicates that ARE allowed must both still be there, AND in the
	// WHERE clause specifically — a fan-out that lost `deleted_at IS NULL` would
	// poll retired upstreams forever, and one that lost the interval would poll
	// every source on every tick. `h.last_reconcile_at` also appears in the
	// ORDER BY, so only the sliced region can tell the two uses apart.
	where := listDuePredicates(t)
	for _, required := range []string{"s.deleted_at IS NULL", "h.last_reconcile_at", "s.reconcile_interval_s"} {
		if !strings.Contains(where, required) {
			t.Fatalf("listDueSQL's WHERE clause no longer contains %q", required)
		}
	}
}

// The write paths must not name the column either: an INSERT or UPDATE that still
// did would fail against the schema 00038 leaves behind, and the failure would
// land on an operator editing a source name.
func TestSourceWritesDoNotNameTheDroppedColumn(t *testing.T) {
	t.Parallel()

	for name, sql := range map[string]string{
		"insert":      insertSourceSQL,
		"update":      updateSourceSQL,
		"soft delete": softDeleteSourceSQL,
		"columns":     sourceColumns,
	} {
		if strings.Contains(sql, "reconcile_enabled") {
			t.Fatalf("the %s statement still names reconcile_enabled, which 00038 dropped", name)
		}
	}
}
