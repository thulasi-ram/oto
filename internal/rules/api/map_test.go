package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/rules/domain"
)

// The wire half of the expression verdict.
//
// `internal/rules/domain` decides whether a threshold narrative may be told, and
// this mapper is the only thing standing between that decision and a client that
// will render whatever it is handed. So the guarantee under test is on the JSON,
// not on the Go struct: under `structural` and `uncharacterised` there must be
// no `numbers` key at all — not an empty one, not a null one — because a client
// that finds the key learns it may look, and looking is the one thing the
// domain's refusal exists to prevent.
//
// The four states are the three verdicts plus the zero value: an expression that
// did not change is `expr_diff: null`, which says "no comparison was performed"
// out loud rather than shipping an empty verdict string nobody can branch on.

// exprSnap builds a snapshot whose expression is the only interesting thing
// about it. Compare is pure, so a row is all a mapper test needs.
func exprSnap(expr string) domain.Snapshot {
	return domain.Snapshot{
		ID:          "0f8fad5b-d9cb-469f-a165-70867728950e",
		OrgID:       "org-1",
		Key:         domain.Key{SourceID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8", Name: "HighErrorRate"},
		Fingerprint: "sha256:" + expr,
		Expr:        expr,
		ForSeconds:  300,
		Origin:      domain.OriginPrometheusAPI,
		Confidence:  domain.ConfidenceExact,
		CapturedAt:  time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

// wireChange renders the diff between two expressions exactly as a client sees
// it, so an assertion about a missing key is an assertion about the payload.
func wireChange(t *testing.T, oldExpr, newExpr string) map[string]any {
	t.Helper()

	d := domain.Compare(exprSnap(oldExpr), exprSnap(newExpr))
	raw, err := json.Marshal(changeDTO(d))
	if err != nil {
		t.Fatalf("marshal change: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal change: %v", err)
	}
	return out
}

// exprDiff pulls out the verdict object, failing if there is not exactly one.
func exprDiff(t *testing.T, change map[string]any) map[string]any {
	t.Helper()

	raw, ok := change["expr_diff"]
	if !ok {
		t.Fatal("expr_diff is absent; the contract requires the key, with null meaning unchanged")
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("expr_diff is %T, want an object", raw)
	}
	return obj
}

// TestAnUnchangedExpressionIsNullAndNotAnEmptyVerdict pins the zero value.
//
// `ExprNotCompared` is the empty string in Go and must never reach the wire as
// one: a client branching on `verdict === ""` is a client one typo away from
// treating "not compared" as "compared, no findings".
func TestAnUnchangedExpressionIsNullAndNotAnEmptyVerdict(t *testing.T) {
	t.Parallel()

	change := wireChange(t, "up == 0", "up == 0")

	if got := change["expr_changed"]; got != false {
		t.Fatalf("expr_changed = %v, want false", got)
	}
	raw, ok := change["expr_diff"]
	if !ok {
		t.Fatal("expr_diff is absent; the key must be present and null")
	}
	if raw != nil {
		t.Fatalf("expr_diff = %#v, want null when the expression did not change", raw)
	}
}

// TestMovedNumbersAreNamed is the only verdict that may carry a value, and the
// values must be the ones the domain vouched for, in its index space.
func TestMovedNumbersAreNamed(t *testing.T) {
	t.Parallel()

	change := wireChange(t,
		`sum(rate(http_errors[5m])) > 0.05`,
		`sum(rate(http_errors[5m])) > 0.1`)

	if got := change["expr_changed"]; got != true {
		t.Fatalf("expr_changed = %v, want true", got)
	}
	diff := exprDiff(t, change)
	if got := diff["verdict"]; got != "numbers_moved" {
		t.Fatalf("verdict = %v, want numbers_moved", got)
	}

	numbers, ok := diff["numbers"].([]any)
	if !ok {
		t.Fatalf("numbers is %T, want an array", diff["numbers"])
	}
	if len(numbers) != 1 {
		t.Fatalf("numbers has %d entries, want 1", len(numbers))
	}
	n, ok := numbers[0].(map[string]any)
	if !ok {
		t.Fatalf("numbers[0] is %T, want an object", numbers[0])
	}
	// Index 0 and not 1: `[5m]` is not in the index space at all, which is the
	// whole reason the space exists.
	if got := n["index"]; got != float64(0) {
		t.Fatalf("index = %v, want 0 — the range window must not be counted", got)
	}
	if got := n["previous_value"]; got != 0.05 {
		t.Fatalf("previous_value = %v, want 0.05", got)
	}
	if got := n["new_value"]; got != 0.1 {
		t.Fatalf("new_value = %v, want 0.1", got)
	}
}

// TestAReformatIsNumbersMovedWithNoNumbers keeps the fourth rendering case
// honest. The verdict vouches for the numbers; there simply are none, because
// only whitespace moved. That is a statement about the rule, not a missing
// answer, and it must not be reported as an unread edit.
func TestAReformatIsNumbersMovedWithNoNumbers(t *testing.T) {
	t.Parallel()

	change := wireChange(t, `up == 0`, `up  ==  0`)

	diff := exprDiff(t, change)
	if got := diff["verdict"]; got != "numbers_moved" {
		t.Fatalf("verdict = %v, want numbers_moved", got)
	}
	if _, ok := diff["numbers"]; ok {
		t.Fatalf("numbers = %#v, want the key omitted when nothing moved", diff["numbers"])
	}
}

// TestAStructuralChangeCarriesNoNumbers: a different metric has no positional
// correspondence with the old one, so there is nothing to compare and nothing to
// offer a client.
func TestAStructuralChangeCarriesNoNumbers(t *testing.T) {
	t.Parallel()

	change := wireChange(t, `up == 0`, `sum(rate(http_errors[5m])) > 0.05`)

	diff := exprDiff(t, change)
	if got := diff["verdict"]; got != "structural" {
		t.Fatalf("verdict = %v, want structural", got)
	}
	if _, ok := diff["numbers"]; ok {
		t.Fatal("a structural verdict carries a numbers key; a client must not be offered one")
	}
}

// TestAWidenedWindowIsUncharacterisedAndNotAThresholdMove is the failure this
// whole design exists to prevent: `[5m]` → `[10m]` reported as `5 → 10` tells an
// operator the alert got less sensitive, and the truth is the opposite.
func TestAWidenedWindowIsUncharacterisedAndNotAThresholdMove(t *testing.T) {
	t.Parallel()

	change := wireChange(t, `rate(x[5m]) > 100`, `rate(x[10m]) > 100`)

	diff := exprDiff(t, change)
	if got := diff["verdict"]; got != "uncharacterised" {
		t.Fatalf("verdict = %v, want uncharacterised", got)
	}
	if _, ok := diff["numbers"]; ok {
		t.Fatal("an uncharacterised verdict carries a numbers key; the refusal must be total")
	}
}

// TestAThresholdEditMixedWithAWindowEditSaysNothing: the claim is all or
// nothing. Reporting only the half that was understood is a partial truth that
// reads as the whole one.
func TestAThresholdEditMixedWithAWindowEditSaysNothing(t *testing.T) {
	t.Parallel()

	change := wireChange(t, `rate(x[5m]) > 100`, `rate(x[10m]) > 200`)

	diff := exprDiff(t, change)
	if got := diff["verdict"]; got != "uncharacterised" {
		t.Fatalf("verdict = %v, want uncharacterised", got)
	}
	if _, ok := diff["numbers"]; ok {
		t.Fatal("the understood half of a mixed edit leaked onto the wire")
	}
}

// TestEveryVerdictIsOneTheContractDeclares guards the enum. The Go side spells a
// verdict by converting a domain string, so a domain rename would otherwise ship
// a value no client can decode.
func TestEveryVerdictIsOneTheContractDeclares(t *testing.T) {
	t.Parallel()

	declared := map[string]bool{
		"numbers_moved":   true,
		"structural":      true,
		"uncharacterised": true,
	}
	pairs := [][2]string{
		{`a > 100`, `a > 200`},
		{`a > 100`, `b > 100`},
		{`rate(x[5m]) > 100`, `rate(x[10m]) > 100`},
	}
	for _, p := range pairs {
		diff := exprDiff(t, wireChange(t, p[0], p[1]))
		verdict, ok := diff["verdict"].(string)
		if !ok {
			t.Fatalf("%q → %q: verdict is %T, want a string", p[0], p[1], diff["verdict"])
		}
		if !declared[verdict] {
			t.Fatalf("%q → %q: verdict %q is not in the contract's enum", p[0], p[1], verdict)
		}
	}
}
