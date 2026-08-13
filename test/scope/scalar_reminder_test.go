package scope

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	identityapi "github.com/thulasiram/oto/internal/identity/api"
	identitydomain "github.com/thulasiram/oto/internal/identity/domain"
	notificationapi "github.com/thulasiram/oto/internal/notification/api"
	notificationdomain "github.com/thulasiram/oto/internal/notification/domain"
	notificationservice "github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/test/harness"
)

// ---------------------------------------------------------------------------
// AC-52 (SPEC.md:3906-3907, §P-19c)
//
//	`unacked_reminder_after_s` is a scalar `*int` in every layer, and a
//	compile-time test asserts the field is not a slice or array type (§G.9.1).
//
// ⭐ ONE STAGE, FOREVER. A reminder ladder — remind at 5 minutes, then 15, then
// page the next person — is the mechanism of an escalation policy, and an
// escalation policy is a rota with extra steps. §G.9.1 fixes oto at one stage,
// and the type is how that decision is enforced rather than remembered: a
// scalar cannot express a ladder, so the ladder cannot be built without a
// migration, a DTO change and this test going red.
//
// ⚠️ WHY A GREP CANNOT DO THIS AT ALL, and it is the cleanest of the three:
// `grep unacked_reminder_after_s` finds the name and says nothing about the
// type. `*int`, `[]int`, `[]Stage`, `map[int][]string` — every one of them
// matches the same line. The gate has to be the compiler.
//
// Four instruments, because "every layer" is four different kinds of place:
//
//	compile-time  `var _ *int = …` per exported field. Widening any of them to a
//	              slice does not fail this test — it fails the COMPILER, before a
//	              single test runs. ⚠️ NOT `go build ./...`: the assertions live in
//	              a `_test.go`, which `go build` never compiles. What type-checks
//	              them is anything that builds the TEST package — `go vet ./...`,
//	              `go test ./test/scope/...` (even with `-run` matching nothing)
//	              and `golangci-lint run`, all three of which `just ci` runs.
//	reflect       the same fields at run time, so the failure names the field and
//	              the type instead of being a wall of assignment errors, and so
//	              the wrapper types (`NullableInt32`) are reached through.
//	AST           every struct field in `internal/` that carries this concept,
//	              including the UNEXPORTED ones no import can reach
//	              (`identity/repository.settingsJSON`) and any layer added later.
//	live schema   `notification_policies.unacked_reminder_after_s` itself:
//	              `data_type` must not be `ARRAY`. A Go scalar over an `integer[]`
//	              column is a compile-clean product that cannot read its own
//	              database.
// ---------------------------------------------------------------------------

/* -------------------------------------------------------------------------- */
/* 1. The compile-time assertion the AC names.                                */
/* -------------------------------------------------------------------------- */

// ⛔ THIS FUNCTION FAILS AT COMPILE TIME, NOT AT TEST TIME. Widening any of these
// fields to `[]int` makes the TEST PACKAGE refuse to build, so `go vet ./...`,
// `go test ./test/scope/...` and `golangci-lint run` each stop on it before a
// test runs. That is deliberate: §G.9.1 is not a property to discover in CI, it
// is a property the compiler holds.
//
// ⚠️ IT IS NOT `go build ./...`, WHICH NEVER COMPILES A `_test.go` FILE. This
// file is one, so a claim that `go build` is the gate would be a claim nobody had
// checked — and it would still be green with every assertion below deleted.
//
// It is named `_` so that it is type-checked and never called — the assertions
// are the assignments themselves, and a `var _ *int = (*T)(nil).Field` at package
// scope would be an initialiser that dereferences a nil pointer at start-up.
//
// ⛔ THE EXPLICIT TYPE ON EVERY LINE IS THE ASSERTION, WHICH IS WHY staticcheck's
// QF1011 ("could omit type from declaration; it will be inferred") is suppressed
// for this function and only for it. Inference is exactly what must not happen
// here: `var x = policyDTO.UnackedReminderAfterSeconds` compiles whatever the
// field is, including `[]int32`, and the gate becomes a variable declaration that
// proves nothing. The type is not noise — it is the whole of the check.
//
//nolint:staticcheck // QF1011: the explicit type IS the assertion; inferring it deletes the gate.
func _() {
	var (
		patch      identitydomain.SettingsPatch
		settings   identitydomain.Settings
		orgDTO     identityapi.OrgSettingsDTO
		orgPatch   identityapi.OrgSettingsPatchDTO
		orgUpdate  identityapi.UpdateOrgSettingsRequest
		policy     notificationdomain.Policy
		policyDTO  notificationapi.PolicyDTO
		createReq  notificationapi.CreatePolicyRequest
		updateReq  notificationapi.UpdatePolicyRequest
		orgDefault notificationservice.OrgDefaults
	)

	var _ *int = patch.UnackedReminderAfterS
	var _ time.Duration = settings.UnackedReminderAfter
	var _ int = orgDTO.UnackedReminderAfterS
	var _ *int = orgPatch.UnackedReminderAfterS
	var _ *int = orgUpdate.UnackedReminderAfterS
	var _ time.Duration = policy.UnackedReminderAfter
	var _ *int32 = policyDTO.UnackedReminderAfterSeconds
	var _ *int32 = createReq.UnackedReminderAfterSeconds
	// The nullable wrapper is a struct, so the assertion has to reach THROUGH it:
	// a ladder introduced on `NullableInt32.Value` would touch no field named for
	// the reminder at all.
	var _ *int32 = updateReq.UnackedReminderAfterSeconds.Value
	var _ time.Duration = orgDefault.UnackedReminderAfter
}

/* -------------------------------------------------------------------------- */
/* 2. The same property through reflect, so a failure is readable.            */
/* -------------------------------------------------------------------------- */

// layer is one place the reminder delay is spelled.
type layer struct {
	pkg   string
	typ   reflect.Type
	field string
}

var reminderLayers = []layer{
	{"identity/domain", reflect.TypeOf(identitydomain.SettingsPatch{}), "UnackedReminderAfterS"},
	{"identity/domain", reflect.TypeOf(identitydomain.Settings{}), "UnackedReminderAfter"},
	{"identity/api", reflect.TypeOf(identityapi.OrgSettingsDTO{}), "UnackedReminderAfterS"},
	{"identity/api", reflect.TypeOf(identityapi.OrgSettingsPatchDTO{}), "UnackedReminderAfterS"},
	{"identity/api", reflect.TypeOf(identityapi.UpdateOrgSettingsRequest{}), "UnackedReminderAfterS"},
	{"notification/domain", reflect.TypeOf(notificationdomain.Policy{}), "UnackedReminderAfter"},
	{"notification/api", reflect.TypeOf(notificationapi.PolicyDTO{}), "UnackedReminderAfterSeconds"},
	{"notification/api", reflect.TypeOf(notificationapi.CreatePolicyRequest{}), "UnackedReminderAfterSeconds"},
	{"notification/api", reflect.TypeOf(notificationapi.UpdatePolicyRequest{}), "UnackedReminderAfterSeconds"},
	// The nullable wrapper's payload. `UpdatePolicyRequest` reaches the value
	// through it, so a ladder could be introduced here without touching any
	// field named for the reminder at all.
	{"notification/api", reflect.TypeOf(notificationapi.NullableInt32{}), "Value"},
	{"notification/service", reflect.TypeOf(notificationservice.OrgDefaults{}), "UnackedReminderAfter"},
}

// scalarViolation returns "" when the named field is a scalar, and the reason it
// is not otherwise.
//
// It dereferences pointers — `*int` is a scalar that may be absent, which is the
// whole vocabulary of "this org sets no default" — and then refuses every kind
// that can hold more than one value.
func scalarViolation(typ reflect.Type, field string) string {
	f, ok := typ.FieldByName(field)
	if !ok {
		return fmt.Sprintf("has no field %s at all; the layer was renamed and this gate "+
			"was not updated with it", field)
	}
	ft := f.Type
	for ft.Kind() == reflect.Pointer {
		ft = ft.Elem()
	}
	switch ft.Kind() {
	case reflect.Slice, reflect.Array:
		return fmt.Sprintf("%s is %s — an ORDERED SEQUENCE of delays, which is a reminder "+
			"ladder and therefore an escalation policy", field, f.Type)
	case reflect.Map:
		return fmt.Sprintf("%s is %s — a delay per key, which is a routing table over "+
			"whoever the keys name", field, f.Type)
	case reflect.Chan, reflect.Func, reflect.Interface:
		return fmt.Sprintf("%s is %s, which is not a value the schema can hold", field, f.Type)
	default:
		return ""
	}
}

func TestTheUnackedReminderIsAScalarInEveryLayer(t *testing.T) {
	for _, l := range reminderLayers {
		if why := scalarViolation(l.typ, l.field); why != "" {
			t.Error(reminderFailure(l.pkg+"."+l.typ.Name(), why))
		}
	}
}

// reminderFailure is the message a violation earns.
func reminderFailure(where, why string) string {
	return fmt.Sprintf(
		"%s: %s\n\n"+
			"AC-52 (SPEC §G.9.1): `unacked_reminder_after_s` is a scalar in every layer.\n\n"+
			"oto sends ONE unacked reminder, to the policy's own channels, once. A second "+
			"stage is not a feature on top of the first — it is the mechanism of an "+
			"escalation policy, and an escalation policy needs somebody to escalate TO. "+
			"That is a rota, and a rota is the product oto is not (SCOPE-BOUNDARY §4.8).\n\n"+
			"The field is a scalar so that the ladder cannot be expressed. If oto is "+
			"genuinely becoming an on-call product, that is an ADR and a SPEC amendment "+
			"(§N) — not a type change.",
		where, why)
}

func TestScalarViolationGateFires(t *testing.T) {
	// THE VIOLATIONS: the three shapes a ladder actually arrives as.
	type ladder struct {
		Stages      []int          // remind at 5m, then 15m, then 30m
		PerSeverity map[string]int // a different delay per severity
		Fixed       [3]int         // "only three, so it is not really a ladder"
		Scalar      *int           // the control: this one is legal
	}
	typ := reflect.TypeOf(ladder{})

	for _, tc := range []struct{ field, want string }{
		{"Stages", "ORDERED SEQUENCE"},
		{"PerSeverity", "routing table"},
		{"Fixed", "ORDERED SEQUENCE"},
	} {
		why := scalarViolation(typ, tc.field)
		if why == "" {
			t.Errorf("%s was accepted as a scalar; a ladder declared as %s is exactly "+
				"what AC-52 exists to refuse", tc.field, typ.Field(mustFieldIndex(t, typ, tc.field)).Type)
			continue
		}
		if !strings.Contains(why, tc.want) {
			t.Errorf("%s was refused for the wrong reason: %q does not mention %q",
				tc.field, why, tc.want)
		}
		if !strings.Contains(reminderFailure("planted", why), "AC-52") {
			t.Errorf("the failure message for %s does not cite the AC", tc.field)
		}
	}

	if why := scalarViolation(typ, "Scalar"); why != "" {
		t.Errorf("*int was refused: %s. The gate is not allowed to object to the shape "+
			"the product actually has", why)
	}
	if why := scalarViolation(typ, "Absent"); why == "" {
		t.Error("a field that does not exist was accepted; a renamed layer must be " +
			"reported rather than silently skipped")
	}
}

func mustFieldIndex(t *testing.T, typ reflect.Type, name string) int {
	t.Helper()
	f, ok := typ.FieldByName(name)
	if !ok {
		t.Fatalf("no field %s on %s", name, typ)
	}
	return f.Index[0]
}

/* -------------------------------------------------------------------------- */
/* 3. The AST sweep, which reaches the layers no import can.                  */
/* -------------------------------------------------------------------------- */

// reminderField is one struct field in `internal/` that carries the reminder
// delay, wherever it is declared and whether or not it is exported.
type reminderField struct {
	pkg    string // path relative to internal/, e.g. "identity/repository"
	strukt string
	name   string
	typ    string // the type expression, rendered
}

func (f reminderField) String() string {
	return fmt.Sprintf("%s.%s.%s %s", f.pkg, f.strukt, f.name, f.typ)
}

// sweepReminderFields parses every non-test Go file under root and returns every
// struct field that names the unacked reminder — by identifier or by struct tag.
//
// ⭐ THIS IS THE PART A GREP CANNOT REPLACE, AND ALSO THE PART THAT CLOSES THE
// "SECOND COPY IN ANOTHER FILE" HOLE. It finds fields the reflect list above
// cannot reach because their structs are unexported — `identity/repository`'s
// `settingsJSON` is the storage shape of the whole settings blob and is package
// private — and it finds a NEW layer added next quarter without anybody
// remembering this file exists. What it asserts is not that a name is absent but
// that a TYPE EXPRESSION is not an array, slice or map, which is a question
// about syntax structure rather than about characters.
func sweepReminderFields(t *testing.T, root string) []reminderField {
	t.Helper()

	fset := token.NewFileSet()
	var out []reminderField

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		rel, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		pkg := filepath.ToSlash(rel)

		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				if !mentionsReminder(fld) {
					continue
				}
				for _, nm := range fld.Names {
					out = append(out, reminderField{
						pkg:    pkg,
						strukt: spec.Name.Name,
						name:   nm.Name,
						typ:    render(fset, fld.Type),
					})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("sweeping %s: %v", root, err)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// mentionsReminder is true for a field that carries the concept, whether it is
// spelled as a Go identifier (`UnackedReminderAfterS`) or only in a tag
// (`json:"unacked_reminder_after_s"`), because a storage struct may name the
// column and nothing else.
func mentionsReminder(f *ast.Field) bool {
	for _, nm := range f.Names {
		if strings.Contains(nm.Name, "UnackedReminderAfter") {
			return true
		}
	}
	if f.Tag == nil {
		return false
	}
	tag, err := strconv.Unquote(f.Tag.Value)
	if err != nil {
		return false
	}
	return strings.Contains(tag, "unacked_reminder_after")
}

// render prints a type expression the way it was written, so a failure shows
// `[]int` rather than an ast node dump.
func render(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return fmt.Sprintf("<unprintable: %v>", err)
	}
	return b.String()
}

// sequenceTyped returns the fields whose declared type is an array, slice or map
// — the three syntactic shapes a ladder can have.
//
// It unwraps pointers, because `*[]int` is a slice that may be absent and is a
// ladder either way.
func sequenceTyped(fields []reminderField) []reminderField {
	var bad []reminderField
	for _, f := range fields {
		typ := strings.TrimLeft(f.typ, "*")
		if strings.HasPrefix(typ, "[") || strings.HasPrefix(typ, "map[") {
			bad = append(bad, f)
		}
	}
	return bad
}

// reminderLayerDirs are the packages the sweep MUST find the field in. They are
// the anti-vacuity guard: a sweep pointed at the wrong tree, or one whose
// `mentionsReminder` stops matching after a rename, returns nothing — and a
// nothing that is read as "no ladder anywhere" is the failure this gate exists
// to prevent.
var reminderLayerDirs = []string{
	"identity/domain",
	"identity/repository", // the unexported storage shape no import can reach
	"identity/api",
	"notification/domain",
	"notification/api",
	"notification/service",
}

func TestNoLayerDeclaresTheReminderAsASequence(t *testing.T) {
	fields := sweepReminderFields(t, filepath.Join(repoRoot(t), "internal"))

	found := map[string]bool{}
	for _, f := range fields {
		found[f.pkg] = true
	}
	for _, dir := range reminderLayerDirs {
		if !found[dir] {
			t.Fatalf("the sweep found no reminder field in internal/%s.\n\n"+
				"AC-52 is about EVERY layer, so a sweep that misses one reports a "+
				"scalar it never looked at. Either the field moved, or "+
				"`mentionsReminder` stopped recognising how that layer spells it.\n\n"+
				"found in: %v", dir, sortedKeys(found))
		}
	}

	for _, f := range sequenceTyped(fields) {
		t.Error(reminderFailure(f.pkg+"."+f.strukt,
			fmt.Sprintf("%s is declared %s", f.name, f.typ)))
	}
}

func TestReminderSweepGateFires(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("planting %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte("package p\n\n"+body), 0o644); err != nil {
			t.Fatalf("planting %s: %v", rel, err)
		}
	}

	// THE VIOLATIONS, in the two places they would actually be written.
	write("notification/domain/policy.go", "type Policy struct {\n"+
		"\tUnackedReminderAfter []time.Duration\n}\n")
	// Named for nothing at all — only the column tag gives it away, which is the
	// case an identifier-only sweep misses.
	write("identity/repository/orgs.go", "type settingsJSON struct {\n"+
		"\tStages *[]int `json:\"unacked_reminder_after_s,omitempty\"`\n}\n")

	// CONTROLS. Every one of these is legal and a careless sweep flags it.
	write("identity/domain/settings.go", "type SettingsPatch struct {\n"+
		"\tUnackedReminderAfterS *int\n}\n")
	write("notification/api/dto.go", "type PolicyDTO struct {\n"+
		"\tUnackedReminderAfterSeconds *int32 `json:\"unacked_reminder_after_seconds\"`\n"+
		// A genuine list that has nothing to do with the reminder delay.
		"\tChannelIDs []string `json:\"channel_ids\"`\n"+
		// The mention audience IS a list, deliberately (ADR 0020): a fixed
		// audience, not a rota. A sweep that matched "UnackedReminder" rather
		// than "UnackedReminderAfter" would condemn it.
		"\tUnackedReminderMentionList *[]string `json:\"unacked_reminder_mention_list\"`\n}\n")
	// A test file, which the sweep must skip: this very file declares a ladder
	// on purpose a few lines above.
	write("notification/domain/policy_test.go", "type ladder struct {\n"+
		"\tUnackedReminderAfter []int\n}\n")

	fields := sweepReminderFields(t, root)
	bad := sequenceTyped(fields)

	got := make([]string, 0, len(bad))
	for _, f := range bad {
		got = append(got, f.String())
	}
	sort.Strings(got)

	want := []string{
		"identity/repository.settingsJSON.Stages *[]int",
		"notification/domain.Policy.UnackedReminderAfter []time.Duration",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("planted exactly\n  %s\ngate reported\n  %s\n\nwhole sweep:\n  %v",
			strings.Join(want, "\n  "), strings.Join(got, "\n  "), fields)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

/* -------------------------------------------------------------------------- */
/* 4. The column itself.                                                      */
/* -------------------------------------------------------------------------- */

// reminderColumn is what the database says the column is. `data_type` is the
// SQL-standard name and reads ARRAY for every array type; `udt_name` is the
// underlying one and reads `_int4` for `integer[]`.
type reminderColumn struct{ dataType, udtName string }

func (c reminderColumn) String() string { return c.dataType + " (" + c.udtName + ")" }

// readReminderColumn asks the database — not the migration files — what type
// `notification_policies.unacked_reminder_after_s` actually has.
//
// It takes the same `querier` the AC-50 gate does, so the companion below can run
// this exact query against an uncommitted schema with the ladder planted in it.
func readReminderColumn(ctx context.Context, q querier) (reminderColumn, error) {
	rows, err := q.Query(ctx, `
		SELECT data_type::text, udt_name::text
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name   = 'notification_policies'
		   AND column_name  = 'unacked_reminder_after_s'`)
	if err != nil {
		return reminderColumn{}, err
	}
	defer rows.Close()

	var out reminderColumn
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return reminderColumn{}, err
		}
		return reminderColumn{}, fmt.Errorf(
			"notification_policies has no unacked_reminder_after_s column at all; " +
				"the column moved, which is not the same as it being clean")
	}
	if err := rows.Scan(&out.dataType, &out.udtName); err != nil {
		return reminderColumn{}, err
	}
	return out, rows.Err()
}

// reminderColumnViolation returns "" when the column is a scalar integer, and the
// reason it is not otherwise. It is separate from the query so the companion test
// can feed it a schema with the ladder planted in it.
//
// Both spellings are checked because either alone has a form that slips past it:
// `data_type` says ARRAY for every array type, and `udt_name` says `_int4`.
func reminderColumnViolation(c reminderColumn) string {
	if c.dataType == "ARRAY" || strings.HasPrefix(c.udtName, "_") {
		return fmt.Sprintf("unacked_reminder_after_s is %s — an array column, "+
			"which is a reminder ladder in the one place it would have rows", c)
	}
	if c.dataType != "integer" {
		return fmt.Sprintf("unacked_reminder_after_s is %s, want integer — §G.9.1 fixes "+
			"this at ONE stage held as a scalar count of seconds", c)
	}
	return ""
}

// TestTheReminderColumnIsAScalarInteger closes the last layer: the database.
//
// A Go `*int` over an `integer[]` column is a product that compiles, passes
// every test above, and cannot read its own table — and an `ALTER TABLE …
// TYPE integer[]` is precisely how a ladder would be introduced by somebody who
// read §G.9.1 as being about Go.
func TestTheReminderColumnIsAScalarInteger(t *testing.T) {
	h := harness.New(t)

	c, err := readReminderColumn(h.Ctx, h.Pool)
	if err != nil {
		t.Fatalf("introspecting notification_policies.unacked_reminder_after_s: %v", err)
	}
	if why := reminderColumnViolation(c); why != "" {
		t.Error(reminderFailure("db.notification_policies", why))
	}
}

// TestTheReminderColumnGateFires plants the ladder in the one place it would
// really arrive and checks the production introspection reports it.
//
// ⭐ IT IS THE ONLY PROOF THIS GATE HAS TEETH, and doc.go promises every gate here
// has one. No migration widens the column, so the test above passes whether the
// query is right, reads the wrong catalogue, or is inverted — the same vacuity the
// other three checks in this file each have a companion for.
//
// The plant is the migration a ladder actually needs, inside a transaction that is
// rolled back: DDL is transactional in Postgres and `information_schema` is an
// ordinary view over `pg_catalog`, so the uncommitted type is visible to the SAME
// query the gate runs, invisible to every other session, and gone at rollback.
// `policies_reminder_ck` has to go first and that is not incidental — a CHECK that
// compares the column to 60 cannot survive it becoming an array, so dropping the
// bound is PART of introducing a ladder, and a reviewer who sees that line in a
// migration is looking at an escalation policy.
func TestTheReminderColumnGateFires(t *testing.T) {
	h := harness.New(t)

	tx, err := h.Pool.Begin(h.Ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Registered immediately: a t.Fatalf below must not leave the transaction open
	// on a pooled connection.
	defer func() { _ = tx.Rollback(h.Ctx) }()

	// THE VIOLATION: remind at 5 minutes, then 15, then 30.
	for _, ddl := range []string{
		`ALTER TABLE notification_policies DROP CONSTRAINT policies_reminder_ck`,
		`ALTER TABLE notification_policies
		   ALTER COLUMN unacked_reminder_after_s TYPE integer[]
		   USING CASE WHEN unacked_reminder_after_s IS NULL THEN NULL::integer[]
		               ELSE ARRAY[unacked_reminder_after_s] END`,
	} {
		if _, err := tx.Exec(h.Ctx, ddl); err != nil {
			t.Fatalf("planting the ladder (%s): %v", ddl, err)
		}
	}

	planted, err := readReminderColumn(h.Ctx, tx)
	if err != nil {
		t.Fatalf("introspect in transaction: %v", err)
	}
	why := reminderColumnViolation(planted)
	if why == "" {
		t.Fatalf("an %s column was accepted as a scalar; AC-52's last layer is the one "+
			"place a ladder would have ROWS in it", planted)
	}
	if !strings.Contains(why, "array column") {
		t.Errorf("the column was refused for the wrong reason: %q", why)
	}
	if msg := reminderFailure("db.notification_policies", why); !strings.Contains(msg, "AC-52") {
		t.Errorf("the failure message does not cite the AC:\n%s", msg)
	}

	// And the rollback has to be real: the gate must be reading the database rather
	// than remembering what it was told.
	if err := tx.Rollback(h.Ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	after, err := readReminderColumn(h.Ctx, h.Pool)
	if err != nil {
		t.Fatalf("introspect after rollback: %v", err)
	}
	if why := reminderColumnViolation(after); why != "" {
		t.Fatalf("the planted ladder outlived its transaction: %s", why)
	}
}
