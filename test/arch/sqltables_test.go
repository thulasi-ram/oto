package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ⭐⭐ WHY THIS FILE EXISTS.
//
// CONTEXT.md §4 lists FOUR mechanisms that cross a module boundary and says of the
// fourth — "Table names in SQL — no Go edge whatsoever" — that it is *"invisible to
// the compiler, to depguard and to the arch test alike"*. `arch_test.go` next door
// says the same from the other side: *"COMPILE-TIME EDGES ONLY."* This file is the
// gate on mechanism 4, and `internal/drill` is why it exists.
//
// `drill` imports no other module. Its production code names exactly two non-
// `platform` internal packages and both are its own. Every guard oto has —
// depguard's per-module blocks, `arch_test.go`'s direction check, §4's arrows —
// reads the Go import graph, so all of them are blind to `drill` by construction,
// while its repository reads twelve tables belonging to six other modules and
// deletes rows from four of them.
//
// ⛔ THE PORTS ARE THE WRONG FIX HERE, AND THAT IS A DECISION, NOT AN OMISSION.
// §5 rule 4 makes cross-domain calls service → service through a consumer-declared
// port, and `drill` already obeys it for everything it WRITES: `service/ports.go`
// declares `IngestAcceptor`, `internal/app` satisfies it from `ingestion/service`,
// and a drill's synthetic alert enters oto through the same front door a real
// Alertmanager batch does. What is left is the READS, and for those a port would
// destroy the thing the module is for. `repository/artifacts.go` puts it in one
// line — *"EVERY QUERY HERE IS A LOOK, NEVER A REPORT. No stage tells the drill it
// succeeded; the drill goes and finds the row."* A port satisfied by
// `notification/service` would have the drill ask the code under test whether the
// code under test worked. A stage that silently stopped writing its row would keep
// answering yes, which is the exact failure a drill is bought to catch.
//
// The deletes fail the port test even harder. To route `Dispose` through
// `alerts/service` that service would need a method that deletes an Alert — a
// capability oto deliberately does not have, reachable then by every consumer of
// that service, in order to remove a coupling that today is four id-scoped
// statements in one file. That trade is strictly worse.
//
// ⭐ SO THE COUPLING STAYS AND BECOMES VISIBLE INSTEAD. Every table `internal/drill`
// names in SQL is DECLARED below with its owning module and how far the drill may
// go against it. A new table name fails this gate the way a new import fails
// `arch_test.go`, a declared table nothing names any more fails as stale, a read
// that grows into a DELETE fails even though the table was already declared, and a
// table renamed out from under `drill` by its owner's migration fails here rather
// than at runtime on the drill path.
//
// ⚠️ IT IS `drill` ONLY, AND THE REST OF §4 MECHANISM 4 IS STILL UNGATED.
// `notification/repository/snapshot.go` joins `alert_sources`, which §4 names, and
// `stats` reads ten tables it does not own. Both are the same defect and neither is
// declared here: gating them means writing their claims, which is their own change.
// `gatedModules` is a map so that adding one is data rather than code — but until a
// module appears in it, nothing checks its SQL, and this comment is the only place
// that says so.

// sqlTableAccess is how far a module's SQL may go against one table. The ladder is
// ordered: a claim permits its own rung and every rung below it.
type sqlTableAccess int

const (
	// tableRead is SELECT and JOIN. It is the default and the one that needs no
	// argument: reading a row somebody else wrote destroys nothing.
	tableRead sqlTableAccess = iota
	// tableDelete adds DELETE. ⛔ It is claimed by exactly one module against four
	// tables — see `drillTableClaims` and ADR 0024 — and a fifth is an architectural
	// change, not a line of SQL.
	tableDelete
	// tableOwn adds INSERT and UPDATE, and is only ever claimed by the module the
	// table belongs to.
	tableOwn
)

func (a sqlTableAccess) String() string {
	switch a {
	case tableRead:
		return "read"
	case tableDelete:
		return "read+delete"
	case tableOwn:
		return "own (insert/update/delete)"
	}
	return fmt.Sprintf("sqlTableAccess(%d)", int(a))
}

// tableClaim is one table a gated module names in SQL, the module that owns it, and
// the strongest statement the gate will allow against it.
//
// ⭐ ADDING A LINE HERE IS THE DECISION, NOT THE PAPERWORK — the same rule
// `allowedEdges` carries in arch_test.go, for the same reason. A new table is a new
// cross-module dependency that no compiler will ever see; state in `why` what the
// drill needs from it and update the `drill` row in CONTEXT.md §4 in the same commit.
type tableClaim struct {
	table  string
	owner  string
	access sqlTableAccess
	why    string
}

// drillTableClaims IS the table list in CONTEXT.md §4's `drill` row.
//
// Twelve tables across six modules, plus the one `drill` owns. The reads are
// ordered as `Artifacts` walks them, which is the order of the pipeline itself:
// accept, identify, group, notify, deliver.
var drillTableClaims = []tableClaim{
	{
		table: "ingest_batches", owner: "ingestion", access: tableRead,
		why: "stage 1 asks whether the front door durably accepted the drill's payload. " +
			"It is read by the batch id `IngestAcceptor` returned, inside a ±5min " +
			"`received_at` window so the partitioned scan prunes to one day.",
	},
	{
		table: "ingest_rejections", owner: "ingestion", access: tableRead,
		why: "a rejected drill has to say WHY, and the reason lives here rather than on " +
			"the batch. Capped at 20 rows: this is a report, not an export.",
	},
	{
		table: "alerts", owner: "alerts", access: tableDelete,
		why: "stage 2 is `did an Alert exist`, found by `labels @> {oto_drill: <id>}` " +
			"rather than by `alert_key` because `ignore_labels` means the drill cannot " +
			"compute the key its own payload will produce. Deleted on disposal by the id " +
			"the drill recorded, `AND synthetic` (ADR 0024 carve-out).",
	},
	{
		table: "alert_cases", owner: "alerts", access: tableRead,
		why: "stage 3 is the case the Alert opened. Not deleted directly — the " +
			"`alerts` delete CASCADEs to it.",
	},
	{
		table: "alert_events", owner: "alerts", access: tableDelete,
		why: "the timeline carries no FK to its subjects and is partitioned, so disposal " +
			"deletes it explicitly and FIRST, by the three ids a drill can have. Never read: " +
			"a drill reports on rows, not on the narration of them.",
	},
	{
		table: "rule_snapshots", owner: "rules", access: tableRead,
		why: "LEFT JOINed onto the case for the rule name, so a drill can tell " +
			"`no rule matched` from `the source has no Prometheus to look one up in`.",
	},
	{
		table: "alert_groups", owner: "grouping", access: tableDelete,
		why: "stage 4 is the group the Alert landed in. Deleted on disposal by id, " +
			"`AND synthetic`; the CASCADE from here is what removes `notifications` and " +
			"`notification_deliveries`. Membership is NOT removed by it: since 00051 " +
			"membership is `alert_cases.group_id`, whose FK is ON DELETE SET NULL, " +
			"and the episodes go with the alert one step later.",
	},
	{
		table: "notifications", owner: "notification", access: tableRead,
		why: "stage 5 is the OLDEST intent for the generation — the one the drill " +
			"caused — with its status and suppression reason. CASCADEd away with the group.",
	},
	{
		table: "notification_policies", owner: "notification", access: tableRead,
		why: "LEFT JOINed for the policy name, so a suppressed drill names the policy " +
			"that suppressed it instead of printing a uuid at an operator.",
	},
	{
		table: "notification_deliveries", owner: "notification", access: tableRead,
		why: "stage 6, the per-channel outcome: status, attempts, error class, whether " +
			"the provider result was ambiguous. CASCADEd away with the notification.",
	},
	{
		table: "channel_threads", owner: "channels", access: tableDelete,
		why: "the thread the group opened, which is most of what `POST /channels/{id}/test` " +
			"cannot prove. `subject_id` carries no FK to `alert_groups`, so nothing cascades " +
			"and disposal deletes it by subject, BEFORE the group.",
	},
	{
		table: "channels", owner: "channels", access: tableRead,
		why: "joined onto threads and deliveries for the channel name only. A drill " +
			"reports which channel, not what the channel is configured to be.",
	},
	{
		table: "delivery_drills", owner: "drill", access: tableOwn,
		why: "the drill's OWN table and the only one it writes. The receipt outlives " +
			"disposal by design — `disposed_at` is stamped here, last, and the frozen " +
			"outcome stays readable a year later (ADR 0024, `retention_exports`).",
	},
}

// gatedModules are the modules whose SQL table names are declared, keyed by module
// directory under `internal/`. ⚠️ A module absent from this map is NOT checked; see
// the file comment.
var gatedModules = map[string][]tableClaim{
	"drill": drillTableClaims,
}

// sqlTableRef is one table name found in SQL position in a module's production Go.
type sqlTableRef struct {
	module string
	table  string
	access sqlTableAccess
	stmt   string // the keyword phrase as written, for the failure message
	file   string
}

// sqlTableSite matches a table name in SQL POSITION — after the keyword that names
// one — rather than anywhere in a string.
//
// ⚠️ POSITION IS THE WHOLE PRECISION OF THIS GATE. A bare word scan for schema table
// names over every string literal in the tree reports `"alerts"` the route segment,
// `"channels"` the error code and `"orgs"` the log field, which is noise in the
// thousands and would get the gate deleted. Requiring FROM/JOIN/INTO/UPDATE in front
// of the name, and requiring the name to be a table the migrations actually create,
// found `drill`'s thirteen and nothing else.
//
// The two-word forms come first because Go's regexp prefers the earliest alternative
// at the leftmost match, so `DELETE FROM alerts` classifies as a delete rather than
// as a read starting at `FROM`.
var sqlTableSite = regexp.MustCompile(
	`(?i)\b(DELETE\s+FROM|INSERT\s+INTO|UPDATE|FROM|JOIN)\s+(?:ONLY\s+)?(?:public\.)?([a-z_][a-z0-9_]*)`)

func sqlAccessOf(keyword string) sqlTableAccess {
	switch strings.ToUpper(strings.Join(strings.Fields(keyword), " ")) {
	case "DELETE FROM":
		return tableDelete
	case "INSERT INTO", "UPDATE":
		return tableOwn
	default:
		return tableRead
	}
}

// TestGatedModulesNameOnlyDeclaredTables is the undeclared-edge half.
func TestGatedModulesNameOnlyDeclaredTables(t *testing.T) {
	for module, refs := range gatedRefs(t) {
		claims := claimIndex(module)
		for _, ref := range refs {
			if _, ok := claims[ref.table]; ok {
				continue
			}
			t.Errorf(""+
				"undeclared table %s named by module %s\n"+
				"  %s writes `%s %s`\n"+
				"`%s` is not in this module's tableClaim list, so nothing in the repo "+
				"connects it to the module that owns it — not depguard, not "+
				"arch_test.go, not CONTEXT.md §4. Either take what you need through a "+
				"port this module declares and internal/app/container.go satisfies, or "+
				"add the claim to %sTableClaims in this file WITH ITS REASON and redraw "+
				"the §4 row in the same commit.",
				ref.table, module, ref.file, ref.stmt, ref.table, ref.table, module)
		}
	}
}

// TestNoStaleTableClaim is the half that keeps the list honest the other way. A
// claim left behind after the query went away re-authorises itself the day somebody
// writes the table name back, silently — the same failure mode as a stale edge in
// `allowedEdges`, and it matters more here because there is no compiler to notice.
func TestNoStaleTableClaim(t *testing.T) {
	refs := gatedRefs(t)
	for module, claims := range gatedModules {
		seen := make(map[string]bool)
		for _, ref := range refs[module] {
			seen[ref.table] = true
		}
		for _, c := range claims {
			if !seen[c.table] {
				t.Errorf(""+
					"module %s declares table `%s` and no longer names it in SQL\n"+
					"delete the claim and the table from CONTEXT.md §4's %s row: a "+
					"declaration nothing backs is a standing permission nobody argued for.",
					module, c.table, module)
			}
		}
	}
}

// TestTableAccessMatchesTheClaim is the check with no counterpart in arch_test.go,
// and the one that closes the failure this gate was written for.
//
// ⛔ A DELETE IS NOT A READ. `Dispose` is the only function in oto that deletes a
// signal row; every other module's retention is a partition detach or a sweeper.
// Declaring a table gets you SELECT — turning one of those SELECTs into a DELETE has
// to fail even though the table was already on the list, because "drill already
// touches that table" is precisely the argument that would carry a fifth delete
// through review.
func TestTableAccessMatchesTheClaim(t *testing.T) {
	for module, refs := range gatedRefs(t) {
		claims := claimIndex(module)
		for _, ref := range refs {
			c, ok := claims[ref.table]
			if !ok {
				continue // TestGatedModulesNameOnlyDeclaredTables owns this one.
			}
			if ref.access <= c.access {
				continue
			}
			t.Errorf(""+
				"module %s exceeds its claim on `%s`: declared %s, found %s\n"+
				"  %s writes `%s %s`\n"+
				"The claim is the argument, and this statement is outside it. Raising the "+
				"claim is an architectural change: say in `why` what makes it safe, and if "+
				"it is a DELETE, say what ADR 0024 promise it does not break.",
				module, ref.table, c.access, ref.access, ref.file, ref.stmt, ref.table)
		}
	}
}

// TestClaimedTablesStillExistInSchema is the direction the ticket cared about most:
// the schema change nobody on the drill side sees coming.
//
// ⭐ IT IS THE ONLY THING THAT RUNS ON THE OWNER'S SIDE. A `notification` migration
// that renames `notifications` breaks `drill` at runtime, on a path exercised a few
// times a day, and the person writing that migration has no reason to grep for it.
// This fails their build instead, with `drill` named in the message.
func TestClaimedTablesStillExistInSchema(t *testing.T) {
	schema := schemaTables(t)
	modules := moduleDirs(t)

	for module, claims := range gatedModules {
		for _, c := range claims {
			if !schema[c.table] {
				t.Errorf(""+
					"module %s claims table `%s` and db/migrations no longer creates it\n"+
					"a table this module reaches by NAME was renamed or dropped by its "+
					"owner (%s). There is no import to break and no compiler error to "+
					"expect: the breakage would have been a runtime SQL error on the %s "+
					"path. Fix the SQL and the claim together.",
					module, c.table, c.owner, module)
			}
			if c.owner != module && !modules[c.owner] {
				t.Errorf("module %s claims table `%s` as owned by %q, which is not a "+
					"module under internal/", module, c.table, c.owner)
			}
			if strings.TrimSpace(c.why) == "" {
				t.Errorf("module %s's claim on `%s` has no reason; a claim without one is "+
					"a permission nobody argued for", module, c.table)
			}
		}
	}
}

// deleteStatement is one DELETE found in a gated module's production Go.
type deleteStatement struct {
	table string
	sql   string
	file  string
}

// syntheticGuarded are the tables whose DELETE must carry `AND synthetic`.
//
// ⛔ IT IS A BELT-AND-BRACES PREDICATE AND IT IS NOT DECORATION. dispose.go argues
// it at length: the ids already come from the drill's own manifest, so the clause
// only matters when the manifest is wrong — and the blast radius of a wrong manifest
// on these two tables is a customer's entire alert history. It was argued in a
// comment on a file nothing in the build system knew was special. Now it is asserted.
var syntheticGuarded = map[string]bool{"alerts": true, "alert_groups": true}

// TestGatedDeletesStayScopedByID asserts the two invariants dispose.go states about
// itself in prose, because prose is what the last reviewer had.
//
// ⛔ `IT IS SCOPED BY ID, NEVER BY PREDICATE.` Every DELETE must name `org_id = $n`
// AND a second id column bound to a parameter. A `DELETE … WHERE synthetic`, or one
// scoped only by a timestamp window, is the single edit in this repository that
// could destroy real customer history, and it would otherwise look like a small
// simplification of a query that already had `synthetic` in it.
//
// ⚠️ `org_id` DOES NOT COUNT AS THE ID, and the first version of this test let it.
// Every repository method in oto is org-scoped (§5.6), so a check that accepts
// `org_id` accepts `DELETE FROM alerts WHERE org_id = $1 AND synthetic` — a
// statement that empties one tenant's synthetic history instead of one drill's, and
// passes a gate written to prevent exactly that.
func TestGatedDeletesStayScopedByID(t *testing.T) {
	orgScoped := regexp.MustCompile(`(?i)\borg_id\s*=\s*\$\d`)
	boundColumn := regexp.MustCompile(`(?i)\b([a-z_][a-z0-9_]*)\s*=\s*\$\d`)
	guard := regexp.MustCompile(`(?i)\bAND\s+synthetic\b`)

	// subjectScoped is "bound to a specific row of a specific subject": some column
	// whose name ends in `id`, bound to a parameter, that is not the tenant.
	subjectScoped := func(sql string) bool {
		for _, m := range boundColumn.FindAllStringSubmatch(sql, -1) {
			col := strings.ToLower(m[1])
			if col != "org_id" && (col == "id" || strings.HasSuffix(col, "_id")) {
				return true
			}
		}
		return false
	}

	stmts := gatedDeletes(t)
	if len(stmts) == 0 {
		t.Fatal("found no DELETE statements in the gated modules at all — the scan is " +
			"broken, not the code")
	}

	for _, s := range stmts {
		if !orgScoped.MatchString(s.sql) {
			t.Errorf("%s: DELETE FROM %s is not scoped by `org_id = $n`\n  %s",
				s.file, s.table, s.sql)
		}
		if !subjectScoped(s.sql) {
			t.Errorf(""+
				"%s: DELETE FROM %s is scoped by the tenant and nothing else\n  %s\n"+
				"dispose.go's rule is `SCOPED BY ID, NEVER BY PREDICATE`: every statement "+
				"names an id the drill itself recorded. A delete bounded only by "+
				"`org_id` and a predicate is one bad migration away from deleting by the "+
				"wrong predicate, across a whole tenant.",
				s.file, s.table, s.sql)
		}
		if syntheticGuarded[s.table] && !guard.MatchString(s.sql) {
			t.Errorf(""+
				"%s: DELETE FROM %s has lost its `AND synthetic` guard\n  %s\n"+
				"⛔ This is the belt-and-braces predicate on the two riskiest deletes in "+
				"oto. It is not a filter — the ids are already the drill's own — it is what "+
				"makes a CORRUPTED manifest unable to delete a real customer's alert or "+
				"group. Restoring it is cheaper than any argument for removing it.",
				s.file, s.table, s.sql)
		}
	}

	for table := range syntheticGuarded {
		found := false
		for _, s := range stmts {
			if s.table == table {
				found = true
			}
		}
		if !found {
			t.Errorf("no DELETE FROM %s found in the gated modules; either the disposal "+
				"moved and this gate no longer covers it, or `syntheticGuarded` is stale",
				table)
		}
	}
}

// TestSQLTableGateFires plants the violations and checks the real scan reports them.
//
// ⭐ IT IS THE ONLY PROOF THE GATE HAS TEETH, for the reason arch_test.go's
// composition-root gate needed the same treatment: the production tree is clean, so
// every check above passes today whether the scan works, is inverted, or matches
// nothing at all. It points the production scanner at a planted tree rather than
// hand-building []sqlTableRef, so the walk, the regexp and the classification are the
// real ones.
func TestSQLTableGateFires(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("planting %s: %v", rel, err)
		}
		src := "package p\n\nconst q = `" + body + "`\n"
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatalf("planting %s: %v", rel, err)
		}
	}

	// THE NEW TABLE: a module reaching for something nobody declared.
	write("drill/repository/new.go", "SELECT * FROM alert_snoozes WHERE org_id = $1")
	// THE WIDENED DELETE: the table IS declared, for reads. The delete is not.
	write("drill/repository/widen.go", "DELETE FROM alert_cases WHERE org_id = $1 AND id = $2")
	// Declared and within its claim — the negative case that catches an over-broad fix.
	write("drill/repository/ok.go", "SELECT id FROM alerts WHERE org_id = $1")
	// Not SQL position. A schema table name in a route or an error code is not a
	// reach into another module, and reporting it is how this gate would die.
	write("drill/api/routes.go", "/orgs/{id}/alerts/channels")
	// A test file: production code only, the boundary arch_test.go and depguard draw.
	write("drill/repository/x_test.go", "DELETE FROM notification_policies WHERE org_id = $1")

	schema := map[string]bool{
		"alerts": true, "alert_cases": true, "alert_snoozes": true,
		"notification_policies": true, "orgs": true, "channels": true,
	}
	refs := scanSQLTableRefs(t, root, schema)

	got := make(map[string]sqlTableRef, len(refs))
	for _, r := range refs {
		if prev, dup := got[r.table]; dup && prev.access >= r.access {
			continue
		}
		got[r.table] = r
	}

	if len(refs) != 3 {
		t.Fatalf("expected 3 refs (alert_snoozes, alert_cases, alerts), got %d: %v",
			len(refs), refs)
	}
	for table, want := range map[string]sqlTableAccess{
		"alert_snoozes": tableRead,
		"alert_cases":   tableDelete,
		"alerts":        tableRead,
	} {
		r, ok := got[table]
		if !ok {
			t.Fatalf("gate missed %s in %v", table, refs)
		}
		if r.access != want {
			t.Errorf("gate classified %s as %s, want %s", table, r.access, want)
		}
		if r.module != "drill" {
			t.Errorf("gate attributed %s to module %q, want drill", table, r.module)
		}
	}
	if _, ok := got["notification_policies"]; ok {
		t.Error("gate read a _test.go file; production code only")
	}
	if _, ok := got["orgs"]; ok {
		t.Error("gate matched a table name outside SQL position — the route literal")
	}

	// The claim check has to agree, or the walk being right buys nothing.
	claims := claimIndex("drill")
	if _, ok := claims["alert_snoozes"]; ok {
		t.Error("alert_snoozes is claimed; pick a table the real list does not carry")
	}
	if c := claims["alert_cases"]; c.access >= tableDelete {
		t.Error("alert_cases is claimed for delete; the widened-delete case proves nothing")
	}
}

// TestSchemaScanReadsTheLiveSchema is the same teeth argument one layer down: every
// claim check leans on `schemaTables`, and a parser that returned everything ever
// written would pass all of them while catching nothing.
//
// ⛔ THE `-- +goose Down` HALF IS WHY. Almost every migration's Down section drops
// the tables its Up creates, so a parser that read the whole file would see
// `alerts` created and dropped and have to guess. It reads Up only — and
// `rate_limit_buckets`, dropped for real by 00030's Up, is the case that tells a
// working parser from a lucky one.
func TestSchemaScanReadsTheLiveSchema(t *testing.T) {
	schema := schemaTables(t)

	for _, want := range []string{
		"alerts", "alert_events", "alert_groups", "notifications", "channel_threads",
		"ingest_batches", "rule_snapshots", "delivery_drills",
	} {
		if !schema[want] {
			t.Errorf("schema scan missed `%s`, which db/migrations creates", want)
		}
	}
	if schema["rate_limit_buckets"] {
		t.Error("schema scan kept `rate_limit_buckets`, which 00030's Up drops: the " +
			"scan is reporting tables that no longer exist, so no claim check means anything")
	}
	if schema["means"] || schema["public"] {
		t.Error("schema scan picked a word out of a COMMENT ON body; `CREATE TABLE` " +
			"only counts at the start of a statement")
	}
}

// ---------------------------------------------------------------- the scanning

func claimIndex(module string) map[string]tableClaim {
	out := make(map[string]tableClaim)
	for _, c := range gatedModules[module] {
		out[c.table] = c
	}
	return out
}

// gatedRefs walks internal/ once and returns the SQL table references of the gated
// modules, keyed by module.
func gatedRefs(t *testing.T) map[string][]sqlTableRef {
	t.Helper()

	all := scanSQLTableRefs(t, filepath.Join(repoRoot(t), "internal"), schemaTables(t))
	out := make(map[string][]sqlTableRef, len(gatedModules))
	for _, ref := range all {
		if _, gated := gatedModules[ref.module]; gated {
			out[ref.module] = append(out[ref.module], ref)
		}
	}
	for module := range gatedModules {
		if len(out[module]) == 0 {
			t.Fatalf("found no SQL table references in module %s at all — the scan is "+
				"broken, not the module", module)
		}
	}
	return out
}

// scanSQLTableRefs reads every non-test Go file under root and returns the schema
// tables its string literals name in SQL position.
//
// ⚠️ NON-TEST FILES ONLY, the boundary arch_test.go and `.golangci.yml` both draw:
// the arch rules describe production code, and a fixture that seeds `alerts` to set
// a drill test up is not the module reaching across a boundary.
//
// It reads STRING LITERALS rather than the file text so a table name in a comment —
// including this file's own prose, and dispose.go's long argument about which tables
// it does not touch — is not a reference.
func scanSQLTableRefs(t *testing.T, root string, schema map[string]bool) []sqlTableRef {
	t.Helper()

	var out []sqlTableRef
	forEachSQLLiteral(t, root, func(module, file, lit string) {
		for _, m := range sqlTableSite.FindAllStringSubmatch(lit, -1) {
			keyword, table := m[1], m[2]
			if !schema[table] {
				continue
			}
			out = append(out, sqlTableRef{
				module: module,
				table:  table,
				access: sqlAccessOf(keyword),
				stmt:   strings.ToUpper(strings.Join(strings.Fields(keyword), " ")),
				file:   file,
			})
		}
	})

	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		if out[i].table != out[j].table {
			return out[i].table < out[j].table
		}
		return out[i].access < out[j].access
	})
	return out
}

// gatedDeletes returns each DELETE a gated module issues, as the text from the
// keyword to the end of its literal — which is the whole statement, because every
// DELETE in this tree is its own literal.
func gatedDeletes(t *testing.T) []deleteStatement {
	t.Helper()

	schema := schemaTables(t)
	head := regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+(?:ONLY\s+)?(?:public\.)?([a-z_][a-z0-9_]*)`)

	var out []deleteStatement
	forEachSQLLiteral(t, filepath.Join(repoRoot(t), "internal"), func(module, file, lit string) {
		if _, gated := gatedModules[module]; !gated {
			return
		}
		for _, loc := range head.FindAllStringSubmatchIndex(lit, -1) {
			table := lit[loc[2]:loc[3]]
			if !schema[table] {
				continue
			}
			out = append(out, deleteStatement{
				table: table,
				sql:   strings.Join(strings.Fields(lit[loc[0]:]), " "),
				file:  file,
			})
		}
	})

	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].table < out[j].table
	})
	return out
}

// forEachSQLLiteral parses every non-test Go file under root and hands each string
// literal to fn, with the module the file belongs to and a display path.
func forEachSQLLiteral(t *testing.T, root string, fn func(module, file, lit string)) {
	t.Helper()

	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		module := moduleOf(rel)
		display := filepath.ToSlash(filepath.Join(filepath.Base(root), rel))

		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, unqErr := strconv.Unquote(lit.Value)
			if unqErr != nil {
				s = lit.Value
			}
			fn(module, display, s)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

// ---------------------------------------------------------------- the schema

var (
	// createdTable matches a CREATE TABLE at the START of a statement. ⚠️ The anchor
	// is load-bearing: 00005_partitions.sql has `CREATE TABLE IF NOT EXISTS` inside a
	// quoted `format()` string and again inside a COMMENT ON body, and an unanchored
	// pattern reads a table called `means` out of English prose.
	createdTable = regexp.MustCompile(
		`(?mi)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:public\.)?([a-z_][a-z0-9_]*)`)
	droppedTable = regexp.MustCompile(
		`(?mi)^\s*DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:public\.)?([a-z_][a-z0-9_]*)`)
	renamedTable = regexp.MustCompile(
		`(?mi)^\s*ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:public\.)?([a-z_][a-z0-9_]*)\s+RENAME\s+TO\s+(?:public\.)?([a-z_][a-z0-9_]*)`)
)

// schemaTables replays the Up half of every migration in order and returns the
// tables that exist at the end of it.
//
// ⛔ THE `Up` HALF ONLY. goose files carry their own rollback under `-- +goose Down`,
// and nearly every one of them drops what its Up created; reading the whole file
// would leave this function reporting an empty schema and every claim check green.
func schemaTables(t *testing.T) map[string]bool {
	t.Helper()

	dir := filepath.Join(repoRoot(t), "db", "migrations")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("globbing %s: %v", dir, err)
	}
	sort.Strings(files) // numeric prefixes, so lexical order IS migration order

	out := make(map[string]bool)
	for _, path := range files {
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("reading %s: %v", path, readErr)
		}
		up := gooseUp(string(b))
		for _, m := range createdTable.FindAllStringSubmatch(up, -1) {
			out[m[1]] = true
		}
		for _, m := range renamedTable.FindAllStringSubmatch(up, -1) {
			if out[m[1]] {
				delete(out, m[1])
				out[m[2]] = true
			}
		}
		for _, m := range droppedTable.FindAllStringSubmatch(up, -1) {
			delete(out, m[1])
		}
	}
	if len(out) == 0 {
		t.Fatalf("no tables found in %s — the migration scan is broken", dir)
	}
	return out
}

// gooseUp returns the text between `-- +goose Up` and `-- +goose Down`.
func gooseUp(sql string) string {
	start := strings.Index(sql, "-- +goose Up")
	if start < 0 {
		return "" // no Up section: nothing this file creates counts
	}
	rest := sql[start:]
	if end := strings.Index(rest, "-- +goose Down"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// moduleDirs are the directories under internal/, which is the set a claim's `owner`
// has to come from.
func moduleDirs(t *testing.T) map[string]bool {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "internal"))
	if err != nil {
		t.Fatalf("reading internal/: %v", err)
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out[e.Name()] = true
		}
	}
	return out
}
