package repository_test

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
)

// ⛔ THIS FILE EXISTS BECAUSE A COMMENT WAS TRUSTED AND WAS WRONG.
//
// 7f8e710 added the `orgs` join to `resolveByEmailSQL` and wrote, in `users.go`,
// that the login, cookie and bearer resolvers "are the whole set of ways a
// request acquires an org". They were three of five. The two it did not know
// about — `resolveSlackIdentitySQL` here and
// `channels/repository.resolveSlackConversationSQL` — had the identical defect,
// including the lockout half of it, and one of them was the tenant resolver for
// every Slack button press in production.
//
// The reason that survived review is not that anyone was careless. It is that a
// confident invariant stated in prose STOPS THE NEXT READER LOOKING, and there
// was nothing anywhere that disagreed with it: the tests covered exactly the
// three resolvers the author already believed in, so the suite agreed with the
// comment and the comment was the bug.
//
// So the roll-call is no longer only prose. This test REFLECTS OVER EVERY SQL
// CONSTANT IN `internal/` and sorts the org-producing ones into three buckets:
//
//	1. it excludes soft-deleted tenants (`JOIN orgs … deleted_at IS NULL`) — fine;
//	2. it is named in `notARequestResolver`, with the reason it cannot be a way a
//	   request acquires a tenancy — fine;
//	3. it is named in `knownGaps`, which is a defect with a name and an owner.
//
// Anything else FAILS, with the statement printed. A sixth resolver therefore
// cannot be added quietly: the day it is written, this test asks its author which
// of the three it is. That is the only moment anybody still has the context to
// answer.
//
// ⚠️ IT IS A TEXT SCAN, NOT A TYPE-CHECKED ANALYSIS, and it is deliberately
// crude: it costs nothing, it needs no database, and being over-eager is the
// desired failure mode — a statement it flags wrongly costs one line and one
// sentence in `notARequestResolver`, while a statement it misses costs a tenant.
// The one thing it cannot see is SQL that is not a package-level constant, which
// is a reason to keep writing them that way.
//
// It lives in `identity/repository` because that is where four of the five
// resolvers are and where `auth_resolvers_test.go` pins their behaviour. It reads
// source, so it needs no fixture and belongs to no package in particular.

// scanRoot is `internal/`, relative to this package.
const scanRoot = "../.."

var (
	// producesOrg matches a statement that can hand an org id to its caller:
	// either it projects an `org_id` column or it reads `orgs` itself.
	producesOrg = regexp.MustCompile(`(?i)\borg_id\b|\bfrom\s+orgs\b|\bjoin\s+orgs\b`)
	// consumesOrg matches the ordinary org-scoped statement — `org_id = $n`, a
	// scope going IN — which is the overwhelming majority and never interesting
	// here.
	consumesOrg = regexp.MustCompile(`(?i)\borg_id\s*=\s*\$\d`)
	// excludesDeadOrg is the invariant: the INNER join that makes a soft-deleted
	// tenant resolve to no row at all.
	excludesDeadOrg = regexp.MustCompile(`(?i)\bjoin\s+orgs\s+\w*\s*on\b[^\n]*\bdeleted_at\s+is\s+null`)
	looksLikeSelect = regexp.MustCompile(`(?i)^\s*(with|select)\b`)
)

// notARequestResolver names the unscoped statements that touch an org id and are
// NOT a way an inbound request acquires a tenancy, with the reason each one is
// not. A statement is either joined to a live org, or explained here, or a bug.
//
// ⚠️ THE JOB-PAYLOAD RESOLVERS ARE A DIFFERENT CATEGORY, not a laxer one. They
// derive the org from the SUBJECT ROW a job names — an occurrence, a batch, a
// group, a delivery — precisely so that a job payload cannot decide its own
// tenancy. Nothing external chooses their input, so a deleted tenant cannot use
// one to get a scope; the worst they can do is let a worker finish work for a
// tenant that was deleted while the job sat in the queue, which is a retention
// question and not an authentication one.
var notARequestResolver = map[string]string{
	"identity/repository.selectOrgSQL": "reads the caller's OWN org row by `o.id = $1`, where $1 is " +
		"`s.OrgID()` from a TenantScope that already exists. It CONSUMES a tenancy; it also selects " +
		"`deleted_at` so the domain can answer whether the tenant is alive",
	"notification/repository.orgFactsSQL": "the org's display facts for a card, by `id = $1` from a " +
		"scope the caller already holds. Same shape as selectOrgSQL: a consumer, not a producer",

	"app.orgOfOccurrenceSQL":             "job-payload scope for `enrich.run`: the org comes from the occurrence the job names (§G.3)",
	"ingestion/repository.resolveOrgSQL": "job-payload scope for `ingest.process_batch`: the org comes from the batch row",
	"ingestion/repository.locateBatchSQL": "operator scope for `oto replay`: the org comes from the batch row, the " +
		"same category as `resolveOrgSQL` above with a different caller. An operator running the CLI has " +
		"a batch id and nothing else — the batch's partition key is not knowable without reading it — so " +
		"the row is what supplies both the org and the `received_at` every subsequent read is scoped by. " +
		"It is not a request resolver: there is no inbound credential to turn into a tenancy, and the " +
		"caller already holds the database",
	"notification/repository.orgOfGroupSQL":    "job-payload scope for the notification workers: the org comes from the group row",
	"notification/repository.orgOfDeliverySQL": "job-payload scope for the delivery workers: the org comes from the delivery row",
	"sources/repository.resolveSourceOrgSQL":   "job-payload scope for the source workers: the org comes from the source row",

	"app.liveOrgSQL": "job-payload scope for the per-tenant periodics (`occurrence.reap`, `group.close`, " +
		"`flap.score`, `retention.prune`'s drill sweep, `stats.rollup`). It is the same category as the " +
		"five above — the org comes from a ROW, not from a credential — with the twist that the row IS " +
		"the org, because a periodic sweep has no subject to derive one from. That is exactly why it " +
		"exists rather than the handler casting the payload's org id into a scope: a job payload is " +
		"data, so the table decides. It carries `deleted_at IS NULL` inline rather than as a join " +
		"because there is nothing to join it TO, and a departed tenant therefore resolves to no row and " +
		"gets no sweep — the same answer `listOrgIDsSQL` gives on the enqueue side",

	"app.listOrgIDsSQL": "the tenant enumerator the per-tenant periodics fan out from. Not a " +
		"resolver — nothing outside chooses its input — and it carries `WHERE deleted_at IS NULL`, " +
		"so a departed tenant is not swept. It reads one keyset PAGE (`id > $1 … LIMIT $2`) rather " +
		"than the whole table, which is a cost decision and not a tenancy one",

	"identity/repository.listLiveOrgsSQL": "the tenant enumerator for the retention fold " +
		"(`identity/service.MaxRetention`). Not a resolver — " +
		"nothing outside chooses its input, and what comes out feeds a maximum over `orgs.settings`, " +
		"never a scope. It carries `WHERE deleted_at IS NULL` inline, the same keyset page shape as " +
		"app.listOrgIDsSQL, so a departed tenant cannot widen the deployment's partition window",
}

// knownGaps names statements that DO turn an inbound request's credential into a
// tenancy, do NOT exclude a soft-deleted org, and are outside the blast radius of
// the change that added this test.
//
// ⛔ AN ENTRY HERE IS A DEFECT WITH A NAME, NOT A LICENCE. The test asserts each
// one is STILL missing the join, so fixing the statement fails this test until
// the entry is deleted: an entry cannot outlive the bug it describes, and the map
// cannot quietly become the place unscoped resolvers go to be forgotten.
// ⭐ IT IS EMPTY, AND KEEPING IT EMPTY IS THE POINT. It had exactly one entry
// when this test was written — `ingestion/repository.lookupTokenSQL`, the sixth
// resolver and the worst of them, because an ingest token lives in an
// `alertmanager.yml` on every cluster the customer runs and nothing on their side
// stops sending it when the tenant is deleted. That entry was deleted in the same
// change that added the join, which is exactly the lifecycle described above.
var knownGaps = map[string]string{}

// TestEveryUnscopedOrgProducingStatementIsAccountedFor is the roll-call.
func TestEveryUnscopedOrgProducingStatementIsAccountedFor(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, s := range scanSQLConstants(t, scanRoot) {
		if !looksLikeSelect.MatchString(s.sql) || !producesOrg.MatchString(s.sql) {
			continue
		}
		if consumesOrg.MatchString(s.sql) {
			continue
		}
		seen[s.name] = true

		switch {
		case excludesDeadOrg.MatchString(s.sql):
			if knownGaps[s.name] != "" {
				t.Errorf("%s now excludes a soft-deleted tenant.\n"+
					"DELETE ITS knownGaps ENTRY — a gap that has been closed must not keep its exemption.", s.name)
			}
		case notARequestResolver[s.name] != "", knownGaps[s.name] != "":
			// Accounted for, in writing, with a reason.
		default:
			t.Errorf("UNACCOUNTED UNSCOPED ORG-PRODUCING STATEMENT: %s\n%s\n\n"+
				"This statement yields an org id and takes no org id in, so it is a candidate for "+
				"PRODUCING a tenancy — and it does not exclude a soft-deleted one.\n"+
				"Do one of three things, and no fourth:\n"+
				"  1. add `JOIN orgs o ON o.id = <t>.org_id AND o.deleted_at IS NULL`, as the five "+
				"request resolvers do, and pin it with a test that a DEAD tenant does not resolve AND "+
				"does not shadow a LIVE one sharing the same key;\n"+
				"  2. if a request can never reach it, name it in `notARequestResolver` and say why;\n"+
				"  3. if it is a defect you are not fixing today, name it in `knownGaps` and say whose "+
				"it is.\n"+
				"Prose in a doc comment is not one of the three. That is how this test came to exist.",
				s.name, s.sql)
		}
	}

	var stale []string
	for name := range notARequestResolver {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	for name := range knownGaps {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("these entries name statements that no longer exist, or that this scan no longer "+
			"classifies as org-producing: %v\nAn exemption whose subject is gone is a comment nobody "+
			"will re-read; delete it.", stale)
	}
}

type sqlConst struct {
	name string
	sql  string
}

// scanSQLConstants reflects over every non-test .go file under root and returns
// the string constants whose value looks like SQL, keyed "dir.ConstName".
//
// Enumerating from the AST rather than from a hand-written list is the whole
// trick, the same one `platform/validate/ddl_test.go` plays on the CHECK
// constraints: a list a human maintains is a list that stops being true, and the
// list this test needs is exactly the list nobody knew was incomplete.
func scanSQLConstants(t *testing.T, root string) []sqlConst {
	t.Helper()

	byDir := map[string]map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return perr
		}
		dir := strings.TrimPrefix(filepath.ToSlash(filepath.Dir(path)), root+"/")
		if byDir[dir] == nil {
			byDir[dir] = map[string]string{}
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						byDir[dir][name.Name] = exprText(vs.Values[i])
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tenancy guard: walking %s: %v", root, err)
	}

	var out []sqlConst
	for dir, consts := range byDir {
		for name, raw := range consts {
			sql := resolveConst(raw, consts, 0)
			if !strings.Contains(sql, unresolved) && strings.Contains(strings.ToUpper(sql), "SELECT") {
				out = append(out, sqlConst{name: dir + "." + name, sql: sql})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// unresolved marks the parts of a constant this scan cannot render as text. A
// NUL cannot appear in a Go source string that matters here, so its presence is
// an unambiguous "skip this constant" — and because the shared column lists
// (`userColumns`, `slackIdentityColumns`) ARE constants, almost nothing is
// skipped in practice.
const unresolved = "\x00"

// exprText renders a const expression: string literals become their value,
// identifiers become a substitutable marker, anything else poisons the constant.
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return unresolved
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return unresolved
		}
		return s
	case *ast.Ident:
		return unresolved + v.Name + unresolved
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return unresolved
		}
		return exprText(v.X) + exprText(v.Y)
	default:
		return unresolved
	}
}

// resolveConst substitutes same-directory constants into the markers exprText
// left, until the text is whole. A reference it cannot satisfy poisons the
// constant rather than producing a half-rendered statement to match against.
func resolveConst(s string, consts map[string]string, depth int) string {
	if depth > 8 || !strings.Contains(s, unresolved) {
		return s
	}
	parts := strings.Split(s, unresolved)
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 0 {
			b.WriteString(p)
			continue
		}
		v, ok := consts[p]
		if !ok {
			return unresolved
		}
		b.WriteString(v)
	}
	return resolveConst(b.String(), consts, depth+1)
}
