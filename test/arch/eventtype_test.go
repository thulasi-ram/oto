package arch

import (
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

	"github.com/thulasiram/oto/internal/alerts/domain"
)

// ⭐⭐ WHY THIS FILE EXISTS.
//
// `alerts/domain.EventType` is a CLOSED value object: one unexported field, and
// `NewEventType` as its only parser. For most of this repo's life four other
// packages re-declared eighteen of its thirty-six values as bare `string`
// constants and used the copies —
//
//	internal/alerts/service/seam.go          6 × `group.*`
//	internal/notification/repository/events.go   7 × `notification.*`, `delivery.*`
//	internal/rules/service/service.go        3 × `rule.*`
//	internal/enrichment/service/pipeline.go  2 × `enrichment.*`
//
// plus ten more as inline map keys in `internal/notification/service/view.go`.
// The values are gone and every `Type` field on that path is now `EventType`, so
// the compiler refuses the copies outright.
//
// ⛔ THE TYPE SYSTEM CLOSES THE ASSIGNMENT, NOT THE SPELLING, AND THIS IS THE
// DIFFERENCE. `Type: "group.opened"` no longer compiles. What still compiles is
//
//	const groupOpened = "group.opened"
//	typ, _ := domain.NewEventType(groupOpened)
//
// — a second spelling of the same value, reached through the sanctioned parser,
// which is where the eighteen would grow back. Nothing in the type system can see
// that, because by the time the value has a type it is already correct. Only a
// scan of the SOURCE can, so that is what this is.
//
// ⚠️ WHY HERE AND NOT `tools/lintvocab`. lintvocab is a word-ban over identifiers
// and literals, which is the right SHAPE, and it is the wrong tool anyway: its own
// doc comment says "⛔ THIS IS AC-49 ONLY. IT IS NOT AC-50, AC-51 OR AC-52", it is
// scoped to SCOPE-BOUNDARY's fixed list of banned product words, and its baseline
// file is a register of naming debt. An event-type check is neither a banned word
// nor debt: it is an assertion about this repo's import and declaration structure,
// which is what every other test in this directory is, run by the same walk over
// the same tree.

// eventTypeTrees are the trees this gate walks: every directory in the module
// that holds production Go.
//
// ⛔ `internal/` ALONE WAS A HOLE, AND IT WAS THE HOLE SHAPED LIKE THE NEXT
// VIOLATION. Nothing about "the enum has one Go spelling" is a statement about
// `internal/`; a `const eventAlertCreated = "alert.created"` in a `cmd/` tool or a
// `tools/` linter is the same second spelling, in code that ships, and it was
// unscanned. `TestEventTypeTreesCoverTheModule` is what stops a new top-level tree
// from being unscanned the same way.
//
// `test/` is deliberately absent, for the reason scanEventTypeLiterals skips
// `_test.go`: a fake or a fixture that spells a value out is a readability
// question, not a second source of truth. `pkg/` holds no Go at all today, and the
// coverage test above is what notices if it ever does.
var eventTypeTrees = []string{"internal", "cmd", "tools", "api", "db"}

// eventTypeExempt names the packages allowed to hold an `alert_events.type`
// literal, and returns "" for everything else.
//
// ⚠️ `streaming/domain` IS NOT A LOOPHOLE, IT IS A DIFFERENT ENUM THAT SHARES ONE
// SPELLING. `streaming/domain.Kind` is `ui_events.kind` — the SSE `event:` field,
// bounded by `ui_events_kind_ck`, whose members are `alert.upserted`,
// `case.upserted`, `group.upserted`, `event.appended`, `source.health` and
// `delivery.updated`. Only the last collides with an `alert_events.type`, and the
// collision is coincidence: one says "a delivery row changed, re-read it", the
// other says "oto amended a card in a channel". They are separate columns with
// separate CHECKs and neither may be built from the other.
func eventTypeExempt(pkg string) string {
	switch {
	case underPackage(pkg, "internal/alerts/domain"):
		return "the enum's home: this IS the declaration"
	case underPackage(pkg, "internal/streaming/domain"):
		return "ui_events.kind is a different closed enum that shares one spelling"
	}
	return ""
}

// eventTypeSQLSites are the packages allowed to name event types inside a SQL
// statement, each with the filter that needs it.
//
// ⛔ A SQL `IN (…)` LIST CANNOT REFERENCE THE GO ENUM, AND THAT IS THE ONLY REASON
// THESE EXIST. The statements are `const` strings that Postgres plans; the values
// are part of the predicate, not of the parameter list, and every one of these
// three filters is chosen so the planner can walk `ev_type_idx` — a
// `type = ANY($n)` over a Go-built array would change the arity of a hot query for
// a property this gate can enforce on the text instead. So the copies stay, and
// the deal is that they are REGISTERED and CHECKED: `TestEventTypeSQLNamesLiveValues`
// proves every value spelled in them is still a member of the enum, which is the
// failure a rename actually causes — a filter that matches zero rows, forever,
// while every test stays green because a read that returns nothing looks exactly
// like a fact that never happened.
//
// A fourth entry is a decision, not paperwork: it is one more place a SPEC
// amendment has to reach.
//
// ⛔⛔ THIS GATE IS BLIND TO A BOUND PARAMETER, AND THAT BLIND SPOT HAS ALREADY COST
// ONE DEFECT. It reads SQL string LITERALS (`sqlQuoted`), so a predicate whose value
// arrives as `$n` is invisible to it however wrong the value is —
// `notification/repository.readCause` shipped `type = $3` bound from
// `EventType.String()`, which matched none of the thirteen months of pre-ADR-0036
// rows 00052 deliberately left on disk, and every gate in the tree stayed green.
// Binding a value gates the TYPO (a constant that left the enum is a compile error);
// it does not gate the RENAME. The rule that does is on
// `alerts/domain.EventType.PersistedSpellings`: every predicate on
// `alert_events.type`, literal or parameter, spells BOTH forms. Nothing here can
// prove it — a reviewer grepping `type = $` and `type = ANY(` can.
var eventTypeSQLSites = map[string]string{
	// ⛔ IT WAS `groupTrailSQL: the 12 lifecycle types`, AND THE 12 WAS ALREADY
	// WRONG BEFORE THE RENAME. The literal held EIGHT facts in BOTH spellings
	// (`case.*` and the pre-ADR-0036 `occurrence.*`) plus `group.opened` and
	// `group.closed`, which is 18 strings, not 12. git-bug `7570090` renamed the
	// const to `caseTrailSQL`, re-keyed it on `case_id` and dropped the two group
	// values — an event about a group names `group_id`, so on this key they were
	// two values that could not match a row — leaving 16.
	"internal/notification/repository": "caseTrailSQL: the 8 state-trail facts a card shows, in both spellings — 16 literals",
	"internal/alerts/repository":       "stateChangeCountsSQL: the 6 transitions `flap.score` counts",
	"internal/stats/repository":        "rollupDaySQL: the same 6, aggregated per day for §G.3",
}

// eventTypeShape is `ev_type_ck`'s `<subject>.<fact>`, which is a SHAPE and not a
// vocabulary — see AllEventTypes. It is what tells a SQL literal that MEANT to be
// an event type from an actor kind or a delivery status sitting in the same query.
var eventTypeShape = regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`)

// sqlQuoted matches one SQL string literal inside a Go string.
var sqlQuoted = regexp.MustCompile(`'([^']*)'`)

// notTokenChar splits a literal into the words a value could be spelled as.
//
// ⚠️ TOKENISED AND NOT SUBSTRING, AND THE DIFFERENCE IS WHAT THE GATE CAN CONCLUDE.
// A substring search sees `case.opened` inside `case.opened_at` and
// inside `oto.notification.v1`, so it fires on column names and message ids; worse,
// it can only ever answer "this text appears", which says nothing about the
// literal `'case.renamed'` left behind by a rename. Splitting on everything
// that cannot be part of a value — the dot and the underscore stay in — yields
// whole tokens, so the gate can ask both questions: is this token a member (a
// second spelling), and is this event-type-shaped token NOT a member (a filter
// that has stopped matching anything).
var notTokenChar = regexp.MustCompile(`[^a-z0-9_.]+`)

// eventTypeValues is the closed set as a lookup, taken from the enum rather than
// restated here: a gate with its own copy of the vocabulary is the bug it is
// gating.
//
// ⚠️ IT IS `AllPersistedEventTypes` AND NOT `AllEventTypes`, AND THE DIFFERENCE IS
// THE ONE THIS GATE IS ABOUT. `AllEventTypes` is what oto puts ON THE WIRE — the
// thirty-six values `components.schemas.AlertEventType` publishes. This gate reads
// SQL predicates, and a predicate is judged against what the COLUMN may hold, which
// since ADR 0036 is eight strings larger: the pre-rename `occurrence.*` spellings
// that migration 00052 deliberately left in thirteen months of `alert_events`.
// Judging a filter against the wire set would report every one of those as a value
// that "matches zero rows forever" — the exact opposite of the truth, since they
// are the only thing that still matches the old rows.
func eventTypeValues() map[string]bool {
	persisted := domain.AllPersistedEventTypes()
	out := make(map[string]bool, len(persisted))
	for _, v := range persisted {
		out[v] = true
	}
	return out
}

// eventTypeFault is what is wrong with one literal.
type eventTypeFault int

const (
	// faultSecondSpelling is a Go literal that spells a member of the enum.
	faultSecondSpelling eventTypeFault = iota
	// faultUnregisteredSQL is a SQL filter naming event types from a package that
	// is not in eventTypeSQLSites.
	faultUnregisteredSQL
	// faultStaleSQLValue is an event-type-shaped value inside a timeline query
	// that is not a member of the enum — a filter that matches nothing.
	faultStaleSQLValue
)

// eventTypeFinding is one fault against one literal.
type eventTypeFinding struct {
	lit   eventTypeLiteral
	fault eventTypeFault
	value string // the offending token
}

// eventTypeFindings is THE predicate: every fault in a scanned tree.
//
// ⭐ IT IS ONE FUNCTION BECAUSE THE SELF-TEST BELOW HAS TO CALL THE REAL ONE. The
// previous version of `TestEventTypeGateFires` rebuilt this rule inline — three
// lines that happened to agree with the rule the day they were written — so
// inverting the real filter left the self-test green, which is the one thing a
// self-test exists to prevent.
func eventTypeFindings(lits []eventTypeLiteral) []eventTypeFinding {
	values := eventTypeValues()

	var out []eventTypeFinding
	for _, lit := range lits {
		if eventTypeExempt(lit.pkg) != "" {
			continue
		}

		spelled := eventTypeMembersIn(lit.value, values)
		// A timeline query is the one place a copy can be argued for, so it is the
		// one place the gate treats differently. Anything else naming a member is a
		// second spelling whatever else is in the string.
		isTimelineSQL := strings.Contains(strings.ToLower(lit.value), "alert_events")

		switch {
		case len(spelled) == 0:
		case !isTimelineSQL:
			for _, v := range spelled {
				out = append(out, eventTypeFinding{lit: lit, fault: faultSecondSpelling, value: v})
			}
		case eventTypeSQLSites[lit.pkg] == "":
			out = append(out, eventTypeFinding{lit: lit, fault: faultUnregisteredSQL, value: spelled[0]})
		}

		if !isTimelineSQL {
			continue
		}
		for _, tok := range sqlQuoted.FindAllStringSubmatch(lit.value, -1) {
			if eventTypeShape.MatchString(tok[1]) && !values[tok[1]] {
				out = append(out, eventTypeFinding{lit: lit, fault: faultStaleSQLValue, value: tok[1]})
			}
		}
	}
	return out
}

// eventTypeMembersIn returns every member of the enum spelled as a whole token in
// s, in first-seen order and without repeats.
func eventTypeMembersIn(s string, values map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, tok := range notTokenChar.Split(s, -1) {
		tok = strings.Trim(tok, ".")
		if values[tok] && !seen[tok] {
			seen[tok] = true
			out = append(out, tok)
		}
	}
	return out
}

// TestEventTypeIsSpelledOnce refuses a second Go spelling of any of the 40
// strings `alert_events.type` may hold — the 32 on the contract plus the 8
// pre-rename spellings ADR 0036 left on disk.
//
// ⚠️ THE NUMBER FELL FROM 36 TO 32 BY DELETION, NOT BY RETIREMENT. Migration
// 00060 narrows `ev_type_ck` to refuse `group.storm_started`, `group.storm_ended`,
// `alert.flapping_started` and `alert.flapping_ended`, so those four leave both
// counts. The three RETIRED types — `group.member_joined`, `group.member_left`,
// `case.reopened` — are still admitted by the CHECK and still counted here.
func TestEventTypeIsSpelledOnce(t *testing.T) {
	if n := len(domain.AllEventTypes()); n != 32 {
		t.Fatalf("AllEventTypes has %d distinct values, expected 32 — "+
			"if the SPEC amended the enum, this number moves with it", n)
	}
	if n := len(eventTypeValues()); n != 40 {
		t.Fatalf("AllPersistedEventTypes has %d distinct values, expected 40 — "+
			"32 on the wire plus the 8 pre-ADR-0036 spellings. This number falls to 32 "+
			"when the last `alert_events` partition holding them is dropped, and not before", n)
	}

	for _, f := range eventTypeFindings(scanEventTypeLiterals(t, repoRoot(t), eventTypeTrees)) {
		if f.fault == faultStaleSQLValue {
			continue // TestEventTypeSQLNamesLiveValues reports that one.
		}
		switch f.fault {
		case faultSecondSpelling:
			t.Errorf(""+
				"%s re-declares the alert_events.type value %q as a string literal\n"+
				"  %s:%d\n"+
				"`alerts/domain.EventType` is a CLOSED value object and this is a SECOND SPELLING "+
				"of one of its members — which is how eighteen of them came to exist in four "+
				"packages at once, and how a value the enum \"closes\" became something Go could "+
				"say two ways.\n"+
				"Name `alerts/domain.Event…` instead. RULE K (CONTEXT.md §5.2b, `.golangci.yml`, "+
				"and `exemptReason` in arch_test.go) grants every module that import uniformly, "+
				"and it is NOT a module edge: the value object crosses, the dependency does not. "+
				"If you are appending to the timeline, the write is still `alerts/service`'s seam "+
				"or the `EventRecorder` port your module declares.",
				f.lit.pkg, f.value, f.lit.file, f.lit.line)
		case faultUnregisteredSQL:
			t.Errorf(""+
				"%s queries alert_events by type and names %q in the statement\n"+
				"  %s:%d\n"+
				"A SQL filter is the one copy of these values that can be argued for — the list is "+
				"part of a predicate the planner walks — but it is a copy, and a copy nobody "+
				"registered is a copy a SPEC rename will not reach. Add the package to "+
				"`eventTypeSQLSites` with the filter that needs it, and this gate will then prove "+
				"every value it spells is still a member of the enum.",
				f.lit.pkg, f.value, f.lit.file, f.lit.line)
		}
	}
}

// TestEventTypeSQLNamesLiveValues is the half of the gate that a SQL filter needs
// and a Go constant does not.
//
// ⛔ A RENAMED VALUE DOES NOT BREAK A QUERY, IT EMPTIES ONE. `type IN (…)` against
// a value the enum no longer has is valid SQL that matches zero rows forever, and
// zero rows is what "nothing happened" looks like too — so the card renders an
// empty trail, the flap score reads zero transitions, the daily rollup reports a
// quiet week, and every test in the tree stays green. This is the only thing in the
// repository positioned to say otherwise, and it is why the three copies in
// `eventTypeSQLSites` are allowed to exist at all.
//
// The second half is registration rot: a registered package that no longer holds
// such a filter is a line that silently sanctions the NEXT copy someone adds there.
func TestEventTypeSQLNamesLiveValues(t *testing.T) {
	lits := scanEventTypeLiterals(t, repoRoot(t), eventTypeTrees)

	for _, f := range eventTypeFindings(lits) {
		if f.fault != faultStaleSQLValue {
			continue
		}
		t.Errorf(""+
			"%s filters alert_events on %q, which is NOT a member of the enum\n"+
			"  %s:%d\n"+
			"This filter matches zero rows and will go on matching zero rows: a read that "+
			"returns nothing is indistinguishable from a fact that never happened, so nothing "+
			"else in this repo can notice. Either the value was renamed in SPEC §D.4.1 and "+
			"this statement was not updated with it, or the string is a typo.",
			f.lit.pkg, f.value, f.lit.file, f.lit.line)
	}

	values := eventTypeValues()
	live := map[string]bool{}
	for _, lit := range lits {
		if strings.Contains(strings.ToLower(lit.value), "alert_events") &&
			len(eventTypeMembersIn(lit.value, values)) > 0 {
			live[lit.pkg] = true
		}
	}
	for pkg, why := range eventTypeSQLSites {
		if !live[pkg] {
			t.Errorf("eventTypeSQLSites registers %s (%q), which no longer holds a timeline "+
				"filter naming an event type. Either the query moved — register where it went — "+
				"or the debt was paid and this line should go.", pkg, why)
		}
	}
}

// TestEventTypeTreesCoverTheModule refuses a tree of production Go that this gate
// does not walk. Scanning `internal/` only was exactly this bug: a rule stated
// about the repository, enforced over part of it.
func TestEventTypeTreesCoverTheModule(t *testing.T) {
	root := repoRoot(t)
	scanned := map[string]bool{}
	for _, tree := range eventTypeTrees {
		scanned[tree] = true
	}
	// `test/` is test support by definition — see scanEventTypeLiterals on why a
	// fixture spelling a value out is not a second source of truth.
	scanned["test"] = true

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read the module root: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || scanned[e.Name()] {
			continue
		}
		holdsGo := false
		walkErr := filepath.WalkDir(filepath.Join(root, e.Name()),
			func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
					holdsGo = true
				}
				return nil
			})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", e.Name(), walkErr)
		}
		if holdsGo {
			t.Errorf("%s/ holds production Go and no gate in this file walks it. Add it to "+
				"eventTypeTrees: an event-type literal there is the same second spelling as one "+
				"in internal/, in code that ships.", e.Name())
		}
	}
}

// TestEventTypeGateFires plants the violations and checks THE REAL predicate
// reports them.
//
// ⭐ IT IS THE ONLY PROOF THE GATE HAS TEETH, for the same reason
// TestCompositionRootGateFires is: once the eighteen are deleted the tree is clean,
// so the tests above pass whether the scan works, is inverted, or is deleted.
//
// ⛔ IT CALLS `eventTypeFindings`, AND THE VERSION THAT DID NOT WAS GREEN OVER
// NOTHING. It used to rebuild the rule inline — `if values[lit.value] &&
// eventTypeExempt(lit.pkg) == ""` — so it asserted that a predicate written in this
// function finds planted files, which is true of any correct predicate and says
// nothing about the one the repository is actually gated by. Inverting the real
// filter left it passing.
//
// The planted files are the answers the walk has to get right, and the ones a
// careless implementation gets wrong are the literal in a COMMENT (which this
// file's own tombstones contain, and which must never fire) and the SQL list (which
// a whole-string comparison cannot see at all).
func TestEventTypeGateFires(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("planting %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatalf("planting %s: %v", rel, err)
		}
	}

	// THE VIOLATION: the re-declaration, in the exact form the seam used to hold.
	write("internal/grouping/service/events.go",
		"package service\n\nconst GroupEventOpened = \"group.opened\"\n")
	// The same thing outside internal/, which nothing scanned until this ticket.
	write("cmd/otoctl/replay.go",
		"package main\n\nconst createdType = \"alert.created\"\n")
	// A SQL list naming several: invisible to a whole-string comparison, and this
	// package is not registered to hold one.
	write("internal/drill/repository/trail.go",
		"package repository\n\nconst trailSQL = `SELECT type FROM alert_events\n"+
			" WHERE type IN ('case.opened','case.expired')`\n")
	// A REGISTERED SQL site whose values are all still members: allowed, silent.
	write("internal/stats/repository/rollup.go",
		"package repository\n\nconst rollupSQL = `SELECT count(*) FROM alert_events\n"+
			" WHERE type IN ('case.opened','case.resolved')`\n")
	// The same site after a rename nobody carried into the SQL: the filter now
	// matches zero rows, and this is the only thing in the repo that can say so.
	write("internal/alerts/repository/stale.go",
		"package repository\n\nconst staleSQL = `SELECT count(*) FROM alert_events\n"+
			" WHERE type IN ('case.opened','case.renamed')`\n")
	// The enum's home, which must not report itself.
	write("internal/alerts/domain/event.go",
		"package domain\n\nvar EventGroupClosed = EventType{\"group.closed\"}\n")
	// A COMMENT naming a value. Every tombstone left by this ticket looks like
	// this, and a grep-based gate would fire on all of them.
	write("internal/rules/service/service.go",
		"package service\n\n// It was `EventLookupFailed = \"rule.lookup_failed\"`, and it is gone.\nconst X = 1\n")
	// A string that is not in the enum at all: `<subject>.<fact>`-shaped, which is
	// all `ev_type_ck` checks, so a shape-matching gate would report it. Outside a
	// timeline query, where the shape means nothing.
	write("internal/platform/jobs/kinds.go",
		"package jobs\n\nconst KindCaseReap = \"case.reap\"\n")

	type want struct {
		pkg   string
		fault eventTypeFault
		value string
	}
	wants := []want{
		{"cmd/otoctl", faultSecondSpelling, "alert.created"},
		{"internal/alerts/repository", faultStaleSQLValue, "case.renamed"},
		{"internal/drill/repository", faultUnregisteredSQL, "case.opened"},
		{"internal/grouping/service", faultSecondSpelling, "group.opened"},
	}

	var got []want
	for _, f := range eventTypeFindings(scanEventTypeLiterals(t, root, []string{"internal", "cmd"})) {
		got = append(got, want{f.lit.pkg, f.fault, f.value})
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].pkg != got[j].pkg {
			return got[i].pkg < got[j].pkg
		}
		return got[i].value < got[j].value
	})

	if len(got) != len(wants) {
		t.Fatalf("planted %d faults, gate found %d: %v", len(wants), len(got), got)
	}
	for i, w := range wants {
		if got[i] != w {
			t.Errorf("finding %d: got %+v, want %+v", i, got[i], w)
		}
	}
}

// eventTypeLiteral is one string literal found in the tree.
type eventTypeLiteral struct {
	pkg   string // module-relative package: "internal/rules/service"
	value string // the unquoted literal
	file  string
	line  int
}

// scanEventTypeLiterals returns every string literal in every non-test Go file
// under each of root's `trees`.
//
// ⚠️ IT PARSES RATHER THAN GREPS, and that is the whole reason it is 30 lines
// instead of one `grep -r`. `go/ast` yields BasicLit nodes and never comment text,
// so the tombstones this ticket left behind — "they were `EventSnapshotCaptured =
// \"rule.snapshot_captured\"`, and they are gone" — do not fire. A gate that fired
// on the comment explaining why the constant was deleted would be deleted itself
// within a week, and lintvocab's doc comment makes exactly this argument about
// exactly this hazard.
//
// ⚠️ NON-TEST FILES ONLY, the boundary `scanImports` and `.golangci.yml` both draw:
// these gates describe production code. A fake in a `_test.go` that spells a type
// out is a readability question, not a second source of truth.
func scanEventTypeLiterals(t *testing.T, root string, trees []string) []eventTypeLiteral {
	t.Helper()

	fset := token.NewFileSet()
	var out []eventTypeLiteral

	for _, tree := range trees {
		treeRoot := filepath.Join(root, tree)
		if _, err := os.Stat(treeRoot); err != nil {
			t.Fatalf("eventTypeTrees names %s, which is not there: %v", tree, err)
		}

		err := filepath.WalkDir(treeRoot, func(path string, d os.DirEntry, err error) error {
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

			f, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return parseErr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value, unquoteErr := strconv.Unquote(lit.Value)
				if unquoteErr != nil {
					return true
				}
				out = append(out, eventTypeLiteral{
					pkg:   filepath.ToSlash(filepath.Dir(rel)),
					value: value,
					file:  rel,
					line:  fset.Position(lit.Pos()).Line,
				})
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", treeRoot, err)
		}
	}

	if len(out) == 0 {
		t.Fatal("found no string literals at all — the scan is broken, not the tree")
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out
}
