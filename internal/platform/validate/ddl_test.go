package validate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// migrationsDir is db/migrations relative to this package.
const migrationsDir = "../../../db/migrations"

// patternsFile is the source this test reflects over. Enumerating the constants
// from the AST rather than from a hand-written list is the whole trick: a new
// exported pattern added without a DDL counterpart fails this test on the day it
// is written, which is the only moment anyone still remembers what the CHECK
// says.
const patternsFile = "patterns.go"

// ddlOf maps an exported pattern to the CHECK constraints that must contain it,
// byte for byte. Several names per pattern is normal and desirable: SHA-256 hex
// is the shape of five different columns, and the point of the shared constant is
// that all five agree.
var ddlOf = map[string][]string{
	"PatternClusterKey":        {"clusters_key_ck"},
	"PatternOrgSlug":           {"orgs_slug_ck"},
	"PatternAlertKey":          {"alerts_key_ck"},
	"PatternGroupKey":          {"groups_key_ck"},
	"PatternSourceFingerprint": {"alerts_srcfp_ck"},
	"PatternSHA256Hex": {
		"ingest_batches_dedup_ck",
		"ingest_dedup_key_ck",
		"rule_snapshots_fp_ck",
		"notifications_idem_ck",
		"deliveries_hash_ck",
	},
	"PatternSlackTS":                  {"threads_ts_ck"},
	"PatternSlackTeamID":              {"slack_identities_team_ck"},
	"PatternSlackUserID":              {"slack_identities_user_ck"},
	"PatternEventType":                {"ev_type_ck"},
	"PatternHTTPURL":                  {"alert_sources_base_ck", "alert_sources_prom_ck"},
	"PredicateHTTPURLNoTrailingSlash": {"alert_sources_base_ck", "alert_sources_prom_ck"},
}

// noDDL lists the exported patterns that deliberately have no CHECK to mirror,
// with the reason. A pattern is either checked against DDL or explained here;
// there is no third state, which is what stops this test from being quietly
// outgrown.
var noDDL = map[string]string{
	"PatternLabelName": "label names live in a JSONB document, so Postgres has nothing to CHECK; " +
		"the ingest normaliser and the domain LabelSet are the only enforcement points",
}

// literalPredicates are patterns that are not regexes and so are matched as a
// substring of the CHECK expression rather than against its `~ '...'` operands.
var literalPredicates = map[string]bool{"PredicateHTTPURLNoTrailingSlash": true}

// TestValidatorMatchesDDL is gate §L.8 / §L.2.4 P-10.
//
// SPEC binds every bound to THREE places — the DTO `validate` tag, the domain
// constructor and the DDL CHECK — and requires them to be identical. Layer 1 and
// layer 6 disagreeing is not a cosmetic problem: a value the validator accepts
// and the CHECK rejects is a 23514 that surfaces as a 500 where a 422 belongs,
// and it is invisible until a user types the one string that separates them.
//
// The test parses db/migrations for the LAST definition of each named constraint
// (a migration that drops and re-adds one is honoured, which is how an expand /
// contract rename stays legible) and compares byte for byte. No normalisation, no
// case folding, no "equivalent regex" cleverness — a difference in a single
// character is a difference.
func TestValidatorMatchesDDL(t *testing.T) {
	constraints := parseConstraints(t)
	patterns := exportedPatterns(t)

	if len(patterns) == 0 {
		t.Fatalf("no exported patterns found in %s — the AST walk is broken, not the code", patternsFile)
	}

	for _, name := range sortedKeys(patterns) {
		value := patterns[name]

		names, wanted := ddlOf[name]
		if !wanted {
			if why, ok := noDDL[name]; ok {
				t.Logf("%s: no DDL counterpart — %s", name, why)
				continue
			}
			t.Errorf("%s is exported but appears in neither ddlOf nor noDDL.\n"+
				"Every canonical pattern either mirrors a CHECK or says in writing why it cannot. "+
				"Add it to one of the two maps in %s.", name, "ddl_test.go")
			continue
		}

		for _, cn := range names {
			expr, ok := constraints[cn]
			if !ok {
				t.Errorf("%s names CHECK constraint %q, which does not exist in %s.\n"+
					"Either the migration was never written or the constraint was renamed; "+
					"a validator mirroring a constraint that is not there enforces nothing.",
					name, cn, migrationsDir)
				continue
			}

			if literalPredicates[name] {
				if !strings.Contains(expr.text, value) {
					t.Errorf("%s = %q is absent from CHECK %s.\n  DDL: %s\n"+
						"⭐ C.8: these CHECKs are two predicates ANDed together and the validator "+
						"must mirror BOTH; mirroring only the regex is how the trailing-slash 500 "+
						"survived.", name, value, cn, expr.text)
				}
				continue
			}

			operands := regexOperands(expr.text)
			if len(operands) == 0 {
				t.Errorf("CHECK %s (%s:%d) contains no `~ '...'` operand for %s to mirror.\n  DDL: %s",
					cn, expr.file, expr.line, name, expr.text)
				continue
			}
			if !contains(operands, value) {
				t.Errorf("DRIFT: %s does not match CHECK %s (%s:%d).\n"+
					"  Go : %q\n  DDL: %v\n"+
					"These must be byte-identical (SPEC §L.2.4, P-10). Change both in one commit "+
					"or neither: a validator looser than its CHECK turns a 422 into a 500, and one "+
					"tighter than its CHECK rejects rows the database would have taken.",
					name, cn, expr.file, expr.line, value, operands)
			}
		}
	}
}

// TestDDLConstraintsAreReachable guards the other direction of the same gate: a
// constraint name listed in ddlOf that no migration defines would make
// TestValidatorMatchesDDL pass vacuously for that pattern if the lookup were ever
// made lenient. Asserting the corpus is non-trivial keeps the parser honest.
func TestDDLConstraintsAreReachable(t *testing.T) {
	constraints := parseConstraints(t)
	if len(constraints) < 20 {
		t.Fatalf("parsed only %d CHECK constraints from %s; the migrations define far more, "+
			"so the parser is silently skipping them and every comparison below is vacuous",
			len(constraints), migrationsDir)
	}
}

type constraint struct {
	text string
	file string
	line int
}

// parseConstraints reads every migration's `+goose Up` section, in file order,
// and records the LAST surviving definition of each named CHECK.
func parseConstraints(t *testing.T) map[string]constraint {
	t.Helper()

	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read %s: %v", migrationsDir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	out := map[string]constraint{}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(migrationsDir, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		up, firstLine := upSection(string(raw))
		applyStatements(out, f, firstLine, up)
	}
	return out
}

// upSection returns the text between `+goose Up` and `+goose Down`, plus the
// 1-based line the section starts on so reported positions match the file. A Down
// restores the old world by definition, so reading it would compare the validator
// against a schema that no longer exists.
func upSection(src string) (string, int) {
	i := strings.Index(src, "+goose Up")
	if i < 0 {
		return "", 0
	}
	first := 1 + strings.Count(src[:i], "\n")
	src = src[i:]
	if j := strings.Index(src, "+goose Down"); j >= 0 {
		src = src[:j]
	}
	return src, first
}

// applyStatements walks one Up section in order, applying ADD/inline definitions
// and DROPs to the accumulated constraint set.
func applyStatements(out map[string]constraint, file string, firstLine int, src string) {
	code := stripSQLComments(src)

	for i := 0; i < len(code); i++ {
		rest := code[i:]

		if name, n, ok := matchDrop(rest); ok {
			delete(out, name)
			i += n - 1
			continue
		}

		name, expr, n, ok := matchCheck(rest)
		if !ok {
			continue
		}
		out[name] = constraint{text: expr, file: file, line: firstLine + strings.Count(code[:i], "\n")}
		i += n - 1
	}
}

func matchDrop(s string) (name string, n int, ok bool) {
	const kw = "DROP CONSTRAINT"
	if !strings.HasPrefix(s, kw) {
		return "", 0, false
	}
	rest := strings.TrimLeft(s[len(kw):], " \t\n")
	consumed := len(s) - len(rest)
	if after, found := strings.CutPrefix(rest, "IF EXISTS"); found {
		rest = strings.TrimLeft(after, " \t\n")
		consumed = len(s) - len(rest)
	}
	name = leadingIdent(rest)
	if name == "" {
		return "", 0, false
	}
	return name, consumed + len(name), true
}

// matchCheck recognises `CONSTRAINT <name> CHECK ( <balanced> )`, which covers
// both the inline table form and `ALTER TABLE ... ADD CONSTRAINT`.
func matchCheck(s string) (name, expr string, n int, ok bool) {
	const kw = "CONSTRAINT"
	if !strings.HasPrefix(s, kw) {
		return "", "", 0, false
	}
	// `DROP CONSTRAINT` and `ADD CONSTRAINT` both reach here via their own offset;
	// only the bare keyword at position 0 is a definition site.
	rest := strings.TrimLeft(s[len(kw):], " \t\n")
	name = leadingIdent(rest)
	if name == "" {
		return "", "", 0, false
	}
	rest = strings.TrimLeft(rest[len(name):], " \t\n")
	if !strings.HasPrefix(rest, "CHECK") {
		return "", "", 0, false
	}
	rest = strings.TrimLeft(rest[len("CHECK"):], " \t\n")
	if !strings.HasPrefix(rest, "(") {
		return "", "", 0, false
	}
	body, used := balanced(rest)
	if used == 0 {
		return "", "", 0, false
	}
	return name, body, len(s) - len(rest) + used, true
}

// balanced returns the contents of the parenthesised group starting at s[0],
// tracking single-quoted strings so a '(' inside a literal cannot unbalance it.
func balanced(s string) (body string, used int) {
	depth := 0
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr:
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
		case c == '\'':
			inStr = true
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return s[1:i], i + 1
			}
		}
	}
	return "", 0
}

// regexOperands returns every literal on the right of a `~` operator.
func regexOperands(expr string) []string {
	var out []string
	for i := 0; i < len(expr); i++ {
		if expr[i] != '~' {
			continue
		}
		rest := strings.TrimLeft(expr[i+1:], " \t\n")
		if len(rest) == 0 || rest[0] != '\'' {
			continue
		}
		lit, used := sqlString(rest)
		if used == 0 {
			continue
		}
		out = append(out, lit)
		i += len(expr[i+1:]) - len(rest) + used
	}
	return out
}

// sqlString decodes a single-quoted SQL literal, undoubling ” as Postgres does.
func sqlString(s string) (val string, used int) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		return b.String(), i + 1
	}
	return "", 0
}

// stripSQLComments blanks `--` comments, preserving offsets and line count so
// reported line numbers stay true, and leaving string literals alone.
func stripSQLComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	inStr, inComment := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inComment:
			if c == '\n' {
				inComment = false
				b.WriteByte(c)
			} else {
				b.WriteByte(' ')
			}
		case inStr:
			b.WriteByte(c)
			if c == '\'' {
				if i+1 < len(src) && src[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				inStr = false
			}
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			inComment = true
			b.WriteString("  ")
			i++
		case c == '\'':
			inStr = true
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func leadingIdent(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return s[:i]
	}
	return s
}

// exportedPatterns reads the string constants out of patterns.go by AST, so the
// test cannot fall behind the file it is guarding.
func exportedPatterns(t *testing.T) map[string]string {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, patternsFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", patternsFile, err)
	}

	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range vs.Names {
				if !ident.IsExported() || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: unquote %s: %v", patternsFile, ident.Name, err)
				}
				out[ident.Name] = v
			}
		}
		return true
	})
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
