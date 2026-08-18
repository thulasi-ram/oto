package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ⭐⭐ WHY THIS FILE EXISTS.
//
// `alerts.snoozed_until` was a mirror of the active `alert_snoozes` row, written
// in the same transaction by three separate call sites, and it was what the
// NOTIFICATION path read to decide whether oto should speak. That read crossed a
// module boundary as a string literal in SQL — CONTEXT.md §4's fourth mechanism,
// the one with *"no Go edge whatsoever"* — so nothing in the build system could
// see it. Deleting the column (00048) fixed the instance. This file is what stops
// it coming back, because the thing that made the defect expensive was not the
// column but the fact that reintroducing it costs nobody a compiler error.
//
// It gates two surfaces, and the split matters:
//
//   - THE SCHEMA. Replayed from `db/migrations`, which is the only place a column
//     on `alerts` can be born. A migration that adds `snoozed_until` back fails
//     here, in the change that adds it, naming the reason.
//   - THE SQL. Every string literal in production Go, comments excluded by
//     construction (`forEachSQLLiteral` reads the AST, not the bytes). A query
//     that names the column fails even before a migration creates it, so a
//     half-landed reintroduction cannot pass by arriving in two pieces.
//
// ⚠️ WHAT IT DOES NOT COVER, stated so nobody mistakes it for total: `snooze` as
// a CONCEPT is not banned and must not be. `alert_snoozes.snoozed_until` is the
// authoritative column and is named freely — by the alert list's two tabs, by the
// group card's member counts, by the expiry sweep. What is refused is exactly one
// spelling: that column duplicated onto the signal row.

// snoozeProjectionColumn is the column that must not return, and the table it
// must not return to.
const (
	snoozeProjectionTable  = "alerts"
	snoozeProjectionColumn = "snoozed_until"
	snoozeProjectionIndex  = "alerts_snooze_idx"
)

// TestAlertsHasNoSnoozeProjection replays every migration's Up section and
// asserts the realised `alerts` table carries no snooze column.
//
// ⭐ IT REPLAYS RATHER THAN GREPS, and the difference is 00017. That migration
// still contains `ALTER TABLE alerts ADD COLUMN snoozed_until` and always will —
// a migration is a record of what happened, not a description of the world — so a
// test that searched the directory for the string would fail on history. What is
// asserted is the FOLD: add, then drop, leaves nothing.
func TestAlertsHasNoSnoozeProjection(t *testing.T) {
	cols := replayColumns(t, snoozeProjectionTable)
	if len(cols) == 0 {
		t.Fatalf("the column replay found no columns on `%s` — the scan is broken, "+
			"not the schema", snoozeProjectionTable)
	}
	if cols[snoozeProjectionColumn] {
		t.Errorf(""+
			"`%s.%s` is back.\n"+
			"It is a mirror of the `alert_snoozes` row that is currently in force, and a "+
			"mirror is not what any of its readers actually wanted: a bare timestamp "+
			"cannot name who asked for quiet, what they wrote, or how the quiet period "+
			"ended, and `notification` was deciding whether oto speaks by reading it. "+
			"Join `alert_snoozes` instead — `alert_snoozes_active_idx` is "+
			"UNIQUE (alert_id) WHERE ended_at IS NULL, so the relation is the snoozes "+
			"currently in force and the join costs approximately nothing. See "+
			"db/migrations/00048_snooze_reads_the_authoritative_row.sql.",
			snoozeProjectionTable, snoozeProjectionColumn)
	}

	if idx := replayIndexes(t); idx[snoozeProjectionIndex] {
		t.Errorf("index `%s` is back. It existed to serve the `?snoozed=` base-table "+
			"predicate, which is now an anti-join against `alert_snoozes`; "+
			"`alert_snoozes_active_org_idx` (00022) is the index that answers this "+
			"question.", snoozeProjectionIndex)
	}
}

// TestNoSQLNamesTheSnoozeProjection refuses the query before the column exists.
//
// A reintroduction arriving in two changes — the migration in one, the read in
// another — would otherwise slip past the schema test in whichever half landed
// first. Either half fails on its own here.
func TestNoSQLNamesTheSnoozeProjection(t *testing.T) {
	root := repoRoot(t)
	var found []string

	for _, dir := range []string{"internal", "cmd"} {
		forEachSQLLiteral(t, filepath.Join(root, dir), func(_, file, lit string) {
			for _, m := range snoozeProjectionSites(lit) {
				found = append(found, file+": "+m)
			}
		})
	}

	sort.Strings(found)
	for _, f := range found {
		t.Errorf(""+
			"%s names `%s.%s`.\n"+
			"That column is gone (00048) and the SQL would fail at runtime, which is the "+
			"cheap version of this failure. The expensive version is the one that made it "+
			"worth a gate: a cross-module SQL read has no import to break, so a query "+
			"reaching for the projection compiles perfectly and answers wrongly. The "+
			"active snooze row is at `alert_snoozes` — join it.",
			f, snoozeProjectionTable, snoozeProjectionColumn)
	}
}

// TestSnoozeProjectionGateFires plants each violation the gate is written for and
// asserts the matcher actually catches it. A gate nobody has seen fail is a gate
// nobody knows is connected.
func TestSnoozeProjectionGateFires(t *testing.T) {
	planted := []struct {
		name string
		sql  string
	}{
		{
			name: "qualified by table name",
			sql:  "SELECT id, alerts.snoozed_until FROM alerts WHERE org_id = $1",
		},
		{
			name: "qualified by the file's usual alias",
			sql:  "SELECT a.state, a.snoozed_until FROM alerts a WHERE a.org_id = $1",
		},
		{
			name: "written back by an UPDATE",
			sql:  "UPDATE alerts SET snoozed_until = $3 WHERE org_id = $1 AND id = $2",
		},
		{
			name: "a filter predicate on the base table",
			sql:  "SELECT id FROM alerts WHERE snoozed_until IS NOT NULL AND snoozed_until > $2",
		},
	}
	for _, p := range planted {
		t.Run(p.name, func(t *testing.T) {
			if len(snoozeProjectionSites(p.sql)) == 0 {
				t.Errorf("the gate did not catch a planted violation:\n%s", p.sql)
			}
		})
	}

	// ⛔ AND THE OTHER DIRECTION, WHICH IS THE HALF THAT MATTERS MOST. The
	// authoritative column has the same name; a gate that could not tell the two
	// apart would forbid reading the very row this change exists to make everyone
	// read, and would be deleted within a week.
	allowed := []struct {
		name string
		sql  string
	}{
		{
			name: "the authoritative table, aliased",
			sql: "SELECT count(*) FILTER (WHERE z.snoozed_until > $3) FROM alert_cases o " +
				"LEFT JOIN alert_snoozes z ON z.alert_id = o.alert_id AND z.ended_at IS NULL",
		},
		{
			name: "the authoritative table, unaliased",
			sql:  "SELECT id FROM alert_snoozes WHERE org_id = $1 AND ended_at IS NULL AND snoozed_until > $2",
		},
		{
			name: "the anti-join the main tab rides",
			sql: "NOT EXISTS (SELECT 1 FROM alert_snoozes s WHERE s.alert_id = alerts.id " +
				"AND s.ended_at IS NULL AND s.snoozed_until > $1)",
		},
		{
			name: "an insert into the authoritative table",
			sql:  "INSERT INTO alert_snoozes (id, org_id, alert_id, snoozed_at, snoozed_until) VALUES ($1,$2,$3,$4,$5)",
		},
	}
	for _, a := range allowed {
		t.Run(a.name, func(t *testing.T) {
			if sites := snoozeProjectionSites(a.sql); len(sites) > 0 {
				t.Errorf("the gate refused a legitimate read of `alert_snoozes`: %v\n%s",
					sites, a.sql)
			}
		})
	}
}

// ---------------------------------------------------------------- the matcher

var (
	// snoozeProjQualified is the two spellings that name the column on `alerts`
	// explicitly: by table and by the alias every query in `alerts/repository`
	// uses for it.
	snoozeProjQualified = regexp.MustCompile(`(?i)\b(alerts|a)\.snoozed_until\b`)
	// snoozeProjUpdate is a write. `UPDATE alerts SET … snoozed_until`.
	snoozeProjUpdate = regexp.MustCompile(`(?is)\bUPDATE\s+alerts\b.*?\bSET\b.*?\bsnoozed_until\b`)
	// snoozeProjBare is an UNQUALIFIED `snoozed_until` in a statement whose only
	// snooze-bearing relation is `alerts` — the base-table predicate and the
	// projected column list. A statement that also names `alert_snoozes` is
	// resolving the name against that table and is left alone; the qualified and
	// UPDATE matchers above still cover the mixed case.
	snoozeProjBare      = regexp.MustCompile(`(?i)\bsnoozed_until\b`)
	snoozeProjFromAlert = regexp.MustCompile(`(?is)\b(FROM|UPDATE|INTO|JOIN)\s+alerts\b`)
	snoozeProjAuthTable = regexp.MustCompile(`(?i)\balert_snoozes\b`)
)

// snoozeProjectionSites returns the ways one SQL literal names the deleted
// column, or nothing. It is a function rather than four regexes at the call site
// so that the gate and the planted-violation test cannot drift.
func snoozeProjectionSites(sql string) []string {
	if !snoozeProjBare.MatchString(sql) {
		return nil
	}
	var out []string
	if m := snoozeProjQualified.FindString(sql); m != "" {
		out = append(out, strings.TrimSpace(m))
	}
	if snoozeProjUpdate.MatchString(sql) {
		out = append(out, "UPDATE alerts SET snoozed_until")
	}
	if len(out) == 0 &&
		snoozeProjFromAlert.MatchString(sql) && !snoozeProjAuthTable.MatchString(sql) {
		out = append(out, "unqualified snoozed_until on alerts")
	}
	return out
}

// ---------------------------------------------------------------- the replay

var (
	alterAddColumn = regexp.MustCompile(
		`(?is)ALTER\s+TABLE\s+(?:ONLY\s+)?(\w+)\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)`)
	alterDropColumn = regexp.MustCompile(
		`(?is)ALTER\s+TABLE\s+(?:ONLY\s+)?(\w+)\s+DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?(\w+)`)
	alterRenameColumn = regexp.MustCompile(
		`(?is)ALTER\s+TABLE\s+(?:ONLY\s+)?(\w+)\s+RENAME\s+COLUMN\s+(\w+)\s+TO\s+(\w+)`)
	createTableHead = regexp.MustCompile(
		`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s*\(`)
	createIndexHead = regexp.MustCompile(
		`(?is)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s+ON\s`)
	dropIndexHead = regexp.MustCompile(
		`(?is)DROP\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+EXISTS\s+)?(\w+)`)
	// alterIndexRename is `ALTER INDEX … RENAME TO`. 00052 does ten of them,
	// including the auto-named PRIMARY KEY index that `ALTER TABLE … RENAME TO`
	// does not carry with it. It is anchored at the start of a statement for the
	// same reason `createdTable` is (sqltables_test.go): 00052's own prose
	// explains what RENAME TO does and does not follow, and an unanchored pattern
	// reads the explanation as the statement.
	alterIndexRename = regexp.MustCompile(
		`(?mi)^\s*ALTER\s+INDEX\s+(?:IF\s+EXISTS\s+)?(?:public\.)?([a-z_][a-z0-9_]*)\s+RENAME\s+TO\s+(?:public\.)?([a-z_][a-z0-9_]*)`)
	// notAColumn is the leading word of a table-level constraint or clause, which
	// shares the comma-separated body with the columns and is not one.
	notAColumn = map[string]bool{
		"constraint": true, "primary": true, "unique": true, "check": true,
		"foreign": true, "exclude": true, "like": true, "partition": true,
	}
)

// ddlKind is the statement shapes the replay follows.
//
// ⭐ THE LIST IS THE MODEL'S, NOT SQL'S. The two things replayed here are
// `table -> column set` and `index name set`, so a statement earns a kind
// exactly when it can move one of those. 00052 also does twenty-seven
// `ALTER TABLE … RENAME CONSTRAINT`s: a constraint is neither a column nor an
// index name, nothing here can answer a question about one, and giving them a
// kind would be modelling for its own sake. If a gate ever asks whether
// `case_ackorder_ck` exists, that is the change that adds the kind.
type ddlKind int

const (
	ddlCreateTable ddlKind = iota
	ddlDropTable
	ddlRenameTable
	ddlAddColumn
	ddlDropColumn
	ddlRenameColumn
	ddlCreateIndex
	ddlDropIndex
	ddlRenameIndex
)

// ddlOp is one statement the replay understands, carrying the byte offset it was
// written at.
//
// ⛔ THE OFFSET IS NOT BOOKKEEPING. Statements inside one migration fold in the
// order they were WRITTEN, and a fold that instead applied every rename after
// every add would misread the ordinary shape "rename the table, then add a
// column to its new name" — silently, as a missing column, which every gate
// here reads as clean.
type ddlOp struct {
	at      int
	kind    ddlKind
	table   string   // the table a column statement acts on; the index name for index kinds
	from    string   // rename: the old name
	to      string   // rename: the new name; add/drop column: the column
	columns []string // ddlCreateTable only
}

// ddlOps reads one migration's Up section into the statements this replay
// follows, in the order they appear.
func ddlOps(up string) []ddlOp {
	var ops []ddlOp
	add := func(op ddlOp) { ops = append(ops, op) }
	at := func(m []int, n int) string { return strings.ToLower(up[m[2*n]:m[2*n+1]]) }

	for _, m := range createTableHead.FindAllStringSubmatchIndex(up, -1) {
		add(ddlOp{at: m[0], kind: ddlCreateTable, table: at(m, 1),
			// m[1] is one past the opening parenthesis the head matched.
			columns: createTableColumns(up[m[1]-1:])})
	}
	for _, m := range droppedTable.FindAllStringSubmatchIndex(up, -1) {
		add(ddlOp{at: m[0], kind: ddlDropTable, table: at(m, 1)})
	}
	for _, m := range renamedTable.FindAllStringSubmatchIndex(up, -1) {
		add(ddlOp{at: m[0], kind: ddlRenameTable, from: at(m, 1), to: at(m, 2)})
	}
	for _, m := range alterAddColumn.FindAllStringSubmatchIndex(up, -1) {
		add(ddlOp{at: m[0], kind: ddlAddColumn, table: at(m, 1), to: at(m, 2)})
	}
	for _, m := range alterDropColumn.FindAllStringSubmatchIndex(up, -1) {
		add(ddlOp{at: m[0], kind: ddlDropColumn, table: at(m, 1), to: at(m, 2)})
	}
	for _, m := range alterRenameColumn.FindAllStringSubmatchIndex(up, -1) {
		add(ddlOp{at: m[0], kind: ddlRenameColumn, table: at(m, 1), from: at(m, 2), to: at(m, 3)})
	}
	for _, m := range createIndexHead.FindAllStringSubmatchIndex(up, -1) {
		add(ddlOp{at: m[0], kind: ddlCreateIndex, to: at(m, 1)})
	}
	for _, m := range dropIndexHead.FindAllStringSubmatchIndex(up, -1) {
		add(ddlOp{at: m[0], kind: ddlDropIndex, to: at(m, 1)})
	}
	for _, m := range alterIndexRename.FindAllStringSubmatchIndex(up, -1) {
		add(ddlOp{at: m[0], kind: ddlRenameIndex, from: at(m, 1), to: at(m, 2)})
	}

	sort.SliceStable(ops, func(i, j int) bool { return ops[i].at < ops[j].at })
	return ops
}

// replayColumns folds every migration's Up section, in migration order, into the
// set of columns the named table actually has.
func replayColumns(t *testing.T, table string) map[string]bool {
	t.Helper()
	return replaySchema(t)[strings.ToLower(table)]
}

// replaySchema folds the Up sections into `table -> its columns`.
//
// ⭐⭐ IT FOLLOWS THE WHOLE SCHEMA, NOT THE ONE TABLE ASKED ABOUT, and
// `alert_cases` is why. Nothing anywhere says `CREATE TABLE alert_cases`: 00007
// creates `alert_occurrences` with all thirty of its columns and
// `00052:93` renames the table. A fold that only watched statements naming the
// table it was asked about therefore answered `{}` — and an empty answer is
// precisely what a "this column must not exist" gate reads as clean, so the
// blindness was invisible from the passing side.
func replaySchema(t *testing.T) map[string]map[string]bool {
	t.Helper()

	schema := map[string]map[string]bool{}
	columnsOf := func(table string) map[string]bool {
		cols, ok := schema[table]
		if !ok {
			cols = map[string]bool{}
			schema[table] = cols
		}
		return cols
	}

	for _, up := range migrationUps(t) {
		for _, op := range ddlOps(up) {
			switch op.kind {
			case ddlCreateTable:
				cols := map[string]bool{}
				for _, c := range op.columns {
					cols[strings.ToLower(c)] = true
				}
				schema[op.table] = cols
			case ddlDropTable:
				delete(schema, op.table)
			case ddlRenameTable:
				if cols, ok := schema[op.from]; ok {
					delete(schema, op.from)
					schema[op.to] = cols
				}
			case ddlAddColumn:
				columnsOf(op.table)[op.to] = true
			case ddlDropColumn:
				delete(columnsOf(op.table), op.to)
			case ddlRenameColumn:
				if cols := columnsOf(op.table); cols[op.from] {
					delete(cols, op.from)
					cols[op.to] = true
				}
			}
		}
	}
	return schema
}

// replayIndexes folds the Up sections into the set of index names that exist.
//
// ⚠️ IT DOES NOT FOLLOW `DROP TABLE`, which takes the table's indexes with it.
// No gate here asks about an index on a table that was dropped, and a fold that
// guessed at the association without tracking `CREATE INDEX … ON <table>` would
// be a model nobody had checked. Add it with the gate that needs it.
func replayIndexes(t *testing.T) map[string]bool {
	t.Helper()

	idx := map[string]bool{}
	for _, up := range migrationUps(t) {
		for _, op := range ddlOps(up) {
			switch op.kind {
			case ddlCreateIndex:
				idx[op.to] = true
			case ddlDropIndex:
				delete(idx, op.to)
			case ddlRenameIndex:
				if idx[op.from] {
					delete(idx, op.from)
					idx[op.to] = true
				}
			}
		}
	}
	return idx
}

// TestTheReplayFollowsTheSchema is the replay's own teeth check, and it is the
// half that was missing.
//
// ⭐ EVERY GATE BUILT ON `replayColumns` FAILS OPEN. "`alerts` has no ack
// column", "`alerts` has no snooze projection" — each is a claim of ABSENCE, and
// a replay that answers `{}` satisfies all of them at once, forever, over a
// schema it never read. The `len(cols)` guards in the two gates catch a total
// blackout; they cannot catch the replay going blind to ONE statement shape.
// This test is the positive control: it asks the replay for facts the migrations
// definitely contain and that only a working fold can produce.
func TestTheReplayFollowsTheSchema(t *testing.T) {
	t.Run("a table is followed through ALTER TABLE … RENAME TO", func(t *testing.T) {
		// 00007 creates `alert_occurrences`; 00052:93 renames it to `alert_cases`.
		// No migration ever says `CREATE TABLE alert_cases`, so this answer can
		// only come from following the rename.
		cols := replayColumns(t, "alert_cases")
		for _, want := range []string{"id", "org_id", "alert_id", "seq", "started_at", "ended_at"} {
			if !cols[want] {
				t.Errorf("the replay lost `alert_cases.%s`. The table is `alert_occurrences` "+
					"renamed (00052), so this is the fold not following "+
					"`ALTER TABLE … RENAME TO`.", want)
			}
		}
		if old := replayColumns(t, "alert_occurrences"); len(old) != 0 {
			t.Errorf("`alert_occurrences` still has %d column(s) after the replay: %v\n\n"+
				"A rename moves the table, it does not copy it. Both names answering is the "+
				"fold applying the rename as an addition.", len(old), sortedColumns(old))
		}
	})

	t.Run("a table is forgotten at DROP TABLE", func(t *testing.T) {
		// 00008 creates `alert_group_members`; 00051:103 drops it, because the
		// membership became a column on the episode.
		if cols := replayColumns(t, "alert_group_members"); len(cols) != 0 {
			t.Errorf("`alert_group_members` still has %d column(s) after the replay: %v\n\n"+
				"00051 dropped the table. A replay that keeps dropped tables would let a gate "+
				"pass on a schema that no longer exists.", len(cols), sortedColumns(cols))
		}
	})

	t.Run("a column is followed through ALTER TABLE … RENAME COLUMN", func(t *testing.T) {
		cols := replayColumns(t, "alerts")
		if !cols["current_case_id"] || cols["current_occurrence_id"] {
			t.Errorf("`alerts` carries current_case_id=%v current_occurrence_id=%v, want true/false\n\n"+
				"00052:102 renames the column. Both true is the fold treating a rename as an "+
				"addition; both false is it not reading the statement at all.",
				cols["current_case_id"], cols["current_occurrence_id"])
		}
	})

	t.Run("an index is followed through ALTER INDEX … RENAME TO", func(t *testing.T) {
		// 00007:198 creates `occ_ack_idx`; 00052:157 renames it to `case_ack_idx`.
		idx := replayIndexes(t)
		if !idx["case_ack_idx"] || idx["occ_ack_idx"] {
			t.Errorf("the index replay says case_ack_idx=%v occ_ack_idx=%v, want true/false\n\n"+
				"`ALTER INDEX … RENAME TO` is the only statement that produces `case_ack_idx`; "+
				"00052 does ten of them and the index fold has to follow all ten or it is "+
				"answering with names the catalogue stopped using.",
				idx["case_ack_idx"], idx["occ_ack_idx"])
		}
	})

	t.Run("a table nobody ever created answers empty", func(t *testing.T) {
		// ⛔ AND THE OTHER DIRECTION. If a misspelled table came back populated the
		// replay would be matching on something other than the name it was given,
		// and every assertion above would be an accident.
		if cols := replayColumns(t, "alerts_that_do_not_exist"); len(cols) != 0 {
			t.Errorf("the replay invented %d column(s) for a table no migration names: %v",
				len(cols), sortedColumns(cols))
		}
	})
}

// TestSnoozeReplayFindsTheAuthoritativeColumn is `TestAlertsHasNoSnoozeProjection`'s
// teeth check, and the exact analogue of `TestAckStaysOnTheCase` one axis over:
// `db/migrations` is clean of `alerts.snoozed_until`, so the gate passes whether
// its lookup works or is blind. Running the SAME lookup over the table where the
// column legitimately lives must find it.
func TestSnoozeReplayFindsTheAuthoritativeColumn(t *testing.T) {
	cols := replayColumns(t, "alert_snoozes")
	for _, want := range []string{
		snoozeProjectionColumn, "snoozed_at", "snoozed_by_label", "ended_at", "ended_reason",
	} {
		if !cols[want] {
			t.Errorf("the replay cannot see `alert_snoozes.%s`.\n\n"+
				"That is the AUTHORITATIVE row (00017), and if the replay cannot find a snooze "+
				"column where one certainly is, its silence about `%s.%s` says nothing at all.",
				want, snoozeProjectionTable, snoozeProjectionColumn)
		}
	}
}

// createTableColumns reads the balanced parenthesised body a CREATE TABLE opens
// with and returns the first token of each top-level item that is a column.
//
// `body` must start AT the opening parenthesis.
func createTableColumns(body string) []string {
	depth, end := 0, -1
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil
	}

	var (
		out   []string
		item  strings.Builder
		items []string
	)
	depth = 0
	for _, r := range body[1:end] {
		switch {
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ',' && depth == 0:
			items = append(items, item.String())
			item.Reset()
			continue
		}
		item.WriteRune(r)
	}
	items = append(items, item.String())

	for _, it := range items {
		fields := strings.Fields(it)
		if len(fields) == 0 || notAColumn[strings.ToLower(fields[0])] {
			continue
		}
		out = append(out, fields[0])
	}
	return out
}

// migrationUps returns every migration's `-- +goose Up` section, in migration
// order, with SQL comments removed.
//
// ⛔ THE DOWN SECTIONS ARE EXCLUDED, and 00048's own Down is why the distinction
// is not pedantic: a Down MUST restore the world exactly as it was, so it puts
// `alerts.snoozed_until` back, complete with its index and its backfill. That is
// correct and it is not the schema. Folding Downs in would make this gate fail on
// the very migration that satisfies it.
//
// ⛔ AND THE COMMENTS ARE STRIPPED, which is not tidiness — see
// `stripSQLComments`. The `-- +goose Up` marker is itself a comment, so the
// split above has to happen first.
func migrationUps(t *testing.T) []string {
	t.Helper()

	dir := filepath.Join(repoRoot(t), "db", "migrations")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("globbing %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no migrations found in %s", dir)
	}
	sort.Strings(files) // numeric prefixes, so lexical order IS migration order

	out := make([]string, 0, len(files))
	for _, path := range files {
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("reading %s: %v", path, readErr)
		}
		out = append(out, stripSQLComments(gooseUp(string(b))))
	}
	return out
}

// stripSQLComments removes `--` line comments and `/* */` block comments,
// keeping the newlines so line-anchored patterns still see where statements
// begin.
//
// ⭐⭐ THIS IS WHY THE ACK GATE HAD NEVER SEEN AN ACK COLUMN. Every table in
// `db/migrations` documents itself in trailing `--` comments, and
// `createTableColumns` splits the body on top-level commas — so
// `alert_key TEXT NOT NULL,   -- C.2, the identity` ends one item and the NEXT
// item STARTS with that comment. Its first word is `--`, not the column, so the
// column after every commented one vanished:
//
//	alerts       lost state, ack_state, alertname, labels, first_seen_at, …
//	alert_cases  lost id, seq, state, started_at, ack_state, acked_at, …
//
// `alerts.ack_state` is the column `TestAlertsHasNoAckColumn` exists to refuse,
// and it was one of the invisible ones: the gate was green over a schema whose
// ack column it could not have seen if it were there. In exchange the fold
// invented columns called `--`, `the`, `and` and `16` out of English prose.
//
// ⚠️ IT IS QUOTE-AWARE BECAUSE IT HAS TO BE. `--` appears INSIDE string literals
// here — the `COMMENT ON` bodies use it as a dash, and 00052's `DO $$` block
// wraps whole UPDATE statements in quotes — and a stripper that cut at the first
// `--` would truncate statements rather than comments. A doubled single quote is
// SQL's only escape, and toggling on every quote handles it: out then straight
// back in.
func stripSQLComments(sql string) string {
	var (
		out      strings.Builder
		inString bool
		inBlock  bool
	)
	out.Grow(len(sql))
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		next := byte(0)
		if i+1 < len(sql) {
			next = sql[i+1]
		}
		switch {
		case inBlock:
			if c == '\n' {
				out.WriteByte('\n') // keep the line structure the anchors read
			} else if c == '*' && next == '/' {
				inBlock, i = false, i+1
			}
		case inString:
			out.WriteByte(c)
			if c == '\'' {
				inString = false
			}
		case c == '\'':
			inString = true
			out.WriteByte(c)
		case c == '-' && next == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			if i < len(sql) {
				out.WriteByte('\n')
			}
		case c == '/' && next == '*':
			inBlock, i = true, i+1
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}
