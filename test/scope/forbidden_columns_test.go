package scope

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/test/harness"
)

// ---------------------------------------------------------------------------
// AC-50 (SPEC.md:3901-3903, §P-19a)
//
//	`alerts`, `alert_occurrences` and `alert_groups` contain no column matching
//	`assigned|owner|watcher|subscriber|incident|ticket|sla_`, asserted by a
//	schema introspection test against the live database, not by reading the
//	migration files (§D.4.0).
//
// ⭐ THE PATTERN IS THE SPEC'S, THE SUBJECT IS NOT. `personSubject` below is
// AC-50's alternation copied verbatim, and matching a pattern is unavoidable
// because the AC is written as one. What makes this a structural test rather
// than a second grep is WHAT IT MATCHES AGAINST: rows returned by
// `information_schema.columns` on a database that has had every migration
// applied — the realised schema, not the text that was supposed to produce it.
//
// The three failures that pass `tools/lintvocab` and fail here:
//
//   - DDL that never went through `db/migrations/` — a hotfix `ALTER TABLE`, a
//     column added by an operator, an extension's trigger. There is no source
//     text to grep.
//   - An expand/contract migration whose contract half was written, reviewed and
//     never deployed. The migration files say the column is gone; the database
//     disagrees, and the database is what has rows in it.
//   - A `-- +goose Down` that restores a forbidden column. `lintvocab` exempts
//     Down sections deliberately (a Down MUST restore the world as it was), so a
//     rolled-back deployment reintroduces the column with the lint still green.
//
// ⚠️ WHAT THIS GATE DOES NOT COVER, stated so nobody mistakes it for total: the
// AC's alternation matches `assigned`, not `assign`. `assignee_id`,
// `oncall_for` and `escalation_owner` are person-subject columns that this
// pattern lets through — the first two entirely, the third only via `owner`.
// Widening the alternation is a SPEC amendment (§N), not a test edit, because
// AC-50's text is the contract this asserts. `tools/lintvocab` catches those
// spellings in source today; nothing catches them in a live schema.
// ---------------------------------------------------------------------------

// personSubject is AC-50's alternation, verbatim. It is applied to column names
// the database reports, never to a file.
var personSubject = regexp.MustCompile(`assigned|owner|watcher|subscriber|incident|ticket|sla_`)

// scopedTables are the three tables AC-50 names. They are the tables whose rows
// are a fact about a SIGNAL; a person-subject column on any of them turns the
// row into a fact about a human, which is the whole scope boundary.
var scopedTables = []string{"alerts", "alert_occurrences", "alert_groups"}

// querier is the read surface both a pool and a transaction offer, so the gate
// and the planted-violation test run the SAME query against a live schema and
// against an uncommitted one.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// column is one introspected column.
type column struct {
	table string
	name  string
}

func (c column) String() string { return c.table + "." + c.name }

// introspect asks the database — not the migration files — which columns the
// named tables actually have.
//
// It joins `information_schema.tables` so that only BASE TABLEs answer: a view
// with one of these names would report columns it does not own, and a table
// that has been renamed away must produce nothing rather than something.
func introspect(ctx context.Context, q querier, tables []string) ([]column, error) {
	rows, err := q.Query(ctx, `
		SELECT c.table_name::text, c.column_name::text
		  FROM information_schema.columns c
		  JOIN information_schema.tables t
		    ON t.table_schema = c.table_schema
		   AND t.table_name   = c.table_name
		 WHERE c.table_schema = 'public'
		   AND t.table_type   = 'BASE TABLE'
		   AND c.table_name::text = ANY($1::text[])
		 ORDER BY c.table_name, c.ordinal_position`, tables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.table, &c.name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// personSubjectColumns is the predicate. It is separate from the query so the
// companion test can feed it a schema with a violation planted in it.
func personSubjectColumns(cols []column) []column {
	var bad []column
	for _, c := range cols {
		if personSubject.MatchString(c.name) {
			bad = append(bad, c)
		}
	}
	return bad
}

// assertEveryScopedTableAnswered is the anti-vacuity guard, and it is the
// difference between this gate and a gate that cannot fail.
//
// ⛔ AN EMPTY RESULT IS NOT A PASS. Rename `alerts`, misspell one entry in
// `scopedTables`, point the harness at a database where the migrations did not
// run, and `personSubjectColumns` returns nothing — green, forever, while the
// deployed schema is entirely unexamined. Every table must have answered with a
// plausible number of columns before "no forbidden column" means anything.
func assertEveryScopedTableAnswered(t *testing.T, cols []column) {
	t.Helper()

	seen := map[string]int{}
	for _, c := range cols {
		seen[c.table]++
	}
	for _, tbl := range scopedTables {
		switch n := seen[tbl]; {
		case n == 0:
			t.Fatalf("introspection found no columns on %q.\n\n"+
				"AC-50 is asserted by ASKING THE DATABASE, so a table that answers with "+
				"nothing means the gate examined nothing — not that the table is clean. "+
				"Either the migrations did not run against this database, or the table has "+
				"been renamed and `scopedTables` was not updated with it.", tbl)
		case n < 5:
			t.Fatalf("introspection found only %d column(s) on %q, which is not a table "+
				"this schema has. The gate is looking at the wrong database.", n, tbl)
		}
	}
}

// forbiddenColumnFailure is the message a violation earns. A gate that fires
// without saying why gets the column renamed until it stops firing.
func forbiddenColumnFailure(bad []column) string {
	names := make([]string, 0, len(bad))
	for _, c := range bad {
		names = append(names, c.String())
	}
	sort.Strings(names)

	return fmt.Sprintf(
		"the live schema carries %d person-subject column(s): %s\n\n"+
			"AC-50 (SPEC §I.1.1, ADR 0013): `alerts`, `alert_occurrences` and `alert_groups` "+
			"contain no column matching `%s`.\n\n"+
			"CONTEXT.md: `occurrence.acked_by = alice` is a fact about the OCCURRENCE — it was "+
			"acknowledged, by whom. `occurrence.assigned_to = alice` is a fact about ALICE — she "+
			"owes work. Identical columns; opposite products. A column is worse than a word: a "+
			"word can be renamed, a column has rows, and the rows are what a migration cannot "+
			"take back.\n\n"+
			"If oto is genuinely becoming an on-call product, that is an ADR and a SPEC "+
			"amendment (§N) — not a column.",
		len(bad), strings.Join(names, ", "), personSubject.String())
}

// TestNoPersonSubjectColumnOnTheAlertTables is AC-50.
func TestNoPersonSubjectColumnOnTheAlertTables(t *testing.T) {
	h := harness.New(t)

	cols, err := introspect(h.Ctx, h.Pool, scopedTables)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	assertEveryScopedTableAnswered(t, cols)

	if bad := personSubjectColumns(cols); len(bad) > 0 {
		t.Error(forbiddenColumnFailure(bad))
	}
}

// TestPersonSubjectColumnGateFires plants the columns that must never exist and
// checks the real introspection reports them.
//
// ⭐ IT IS THE ONLY PROOF THE GATE HAS TEETH. No migration adds a forbidden
// column, so TestNoPersonSubjectColumnOnTheAlertTables passes whether the query
// is correct, matches the wrong catalogue, or returns nothing at all.
//
// The plant is an `ALTER TABLE` inside a transaction that is rolled back. DDL is
// transactional in Postgres and `information_schema` is an ordinary view over
// `pg_catalog`, so the uncommitted column is visible to the SAME query the gate
// runs — and invisible to every other session, and gone at rollback. Two columns
// on two different tables, because a walk hard-coded to `alerts` would pass a
// one-column plant.
func TestPersonSubjectColumnGateFires(t *testing.T) {
	h := harness.New(t)

	tx, err := h.Pool.Begin(h.Ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Registered immediately: a t.Fatalf below must not leave the transaction
	// open on a pooled connection.
	defer func() { _ = tx.Rollback(h.Ctx) }()

	// THE VIOLATIONS. `assigned_to` is the column CONTEXT.md names as the one
	// that turns a fact about a signal into a fact about a human; `sla_due_at` is
	// a deadline on a person, on a different table, spelled with the alternation's
	// only underscored member.
	for _, ddl := range []string{
		`ALTER TABLE alerts ADD COLUMN assigned_to UUID`,
		`ALTER TABLE alert_groups ADD COLUMN sla_due_at TIMESTAMPTZ`,
	} {
		if _, err := tx.Exec(h.Ctx, ddl); err != nil {
			t.Fatalf("planting %q: %v", ddl, err)
		}
	}
	// NOT a violation, and it is here to prove the gate is not simply flagging
	// every recently added column: acknowledgement identity is stored on purpose
	// (§B.4) and `acked_by` is the exact column the scope boundary permits.
	if _, err := tx.Exec(h.Ctx, `ALTER TABLE alert_occurrences ADD COLUMN acked_by_deputy TEXT`); err != nil {
		t.Fatalf("planting the control column: %v", err)
	}

	cols, err := introspect(h.Ctx, tx, scopedTables)
	if err != nil {
		t.Fatalf("introspect in transaction: %v", err)
	}
	assertEveryScopedTableAnswered(t, cols)

	bad := personSubjectColumns(cols)
	got := make([]string, 0, len(bad))
	for _, c := range bad {
		got = append(got, c.String())
	}
	sort.Strings(got)

	want := []string{"alert_groups.sla_due_at", "alerts.assigned_to"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("planted exactly %v, gate reported %v", want, got)
	}

	// The message has to survive too.
	msg := forbiddenColumnFailure(bad)
	for _, want := range []string{"alerts.assigned_to", "AC-50", "a column has rows"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure message does not mention %q:\n%s", want, msg)
		}
	}

	// And the rollback has to be real: the checker must be reading the database
	// rather than remembering what it was told.
	if err := tx.Rollback(h.Ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	after, err := introspect(h.Ctx, h.Pool, scopedTables)
	if err != nil {
		t.Fatalf("introspect after rollback: %v", err)
	}
	if bad := personSubjectColumns(after); len(bad) > 0 {
		t.Fatalf("the planted columns outlived their transaction: %v", bad)
	}
}
