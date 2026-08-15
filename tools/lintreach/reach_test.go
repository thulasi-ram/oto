package main

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"strings"
	"testing"
)

// The package a fixture is declared in. It must sit under a declRoot or
// declareStructs skips it and every assertion below reads zero.
const fixturePkg = modulePath + "/internal/fixture"

// analyze type-checks one fixture file and runs the full counting walk over it,
// which is the same two phases load() runs against the module.
func analyze(t *testing.T, src string) *analyzer {
	t.Helper()
	a := newAnalyzer()
	f, err := parser.ParseFile(a.fset, "/oto/internal/fixture/fixture.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	a.check(importer.Default(), fixturePkg, []*ast.File{f})
	a.count()
	if len(a.errs) > 0 {
		t.Fatalf("fixture does not type-check: %s", strings.Join(a.errs, "; "))
	}
	return a
}

// counts returns one field's production reads and writes.
func counts(t *testing.T, a *analyzer, field string) (reads, writes int) {
	t.Helper()
	rec := a.fields[fixturePkg+"."+field]
	if rec == nil {
		t.Fatalf("field %s was never declared; known fields: %v", field, keys(a))
	}
	return rec.reads[0], rec.writes[0]
}

func keys(a *analyzer) []string {
	var out []string
	for k := range a.fields {
		out = append(out, strings.TrimPrefix(k, fixturePkg+"."))
	}
	return out
}

func wantCounts(t *testing.T, a *analyzer, field string, reads, writes int) {
	t.Helper()
	gotR, gotW := counts(t, a, field)
	if gotR != reads || gotW != writes {
		t.Errorf("%s: got %d read(s), %d write(s); want %d read(s), %d write(s)",
			field, gotR, gotW, reads, writes)
	}
}

// hasFinding reports whether the analyzer would report key, which is what the
// baseline is written against.
func hasFinding(a *analyzer, key string) bool {
	for _, f := range a.findings() {
		if f.key == key {
			return true
		}
	}
	return false
}

// fixture is a struct tree deep enough to tell a one-hop write from a chained
// one, plus a pointer hop, which is where a chained write must stop.
const fixture = `package fixture

type Leaf struct {
	N int
	M int
}

type Mid struct {
	Leaf Leaf
}

type Outer struct {
	One   int
	Mid   Mid
	Ptr   *Mid
	Plain Leaf
}

func scan(dst ...any) {}

%s
`

func fixtureWith(body string) string { return fmt.Sprintf(fixture, body) }

// TestAddressOfChainWritesEveryHop is the regression this file exists for.
// `&x.A` and `&x.A.B` are the same intent one node apart, and only the first was
// counted, so every struct field a `rows.Scan(&o.A.B)` fills read as
// never-written — six of them were baselined as real debt.
func TestAddressOfChainWritesEveryHop(t *testing.T) {
	a := analyze(t, fixtureWith(`
func Fill(o *Outer) {
	scan(&o.One, &o.Mid.Leaf.N)
}`))

	// The one-hop address-of, which was always right: written and read.
	wantCounts(t, a, "Outer.One", 1, 1)

	// The chained one. The write lands on N, and it lands INSIDE Leaf and
	// inside Mid, both of which are also read on the way to it.
	wantCounts(t, a, "Leaf.N", 1, 1)
	wantCounts(t, a, "Mid.Leaf", 1, 1)
	wantCounts(t, a, "Outer.Mid", 1, 1)

	// Which is the difference between reporting Outer.Mid and not.
	if hasFinding(a, "nowrite:"+fixturePkg+".Outer.Mid") {
		t.Error("Outer.Mid reported as never written, but &o.Mid.Leaf.N writes it")
	}
	if !hasFinding(a, "nowrite:"+fixturePkg+".Outer.Plain") {
		t.Error("Outer.Plain is touched by nothing and must still be reported")
	}
}

// TestChainStopsAtPointer pins the limit of the rule. `o.Ptr.Leaf.N` stores into
// the pointee; the field Ptr itself is only read, and a Ptr nothing ever assigns
// is a real finding.
func TestChainStopsAtPointer(t *testing.T) {
	a := analyze(t, fixtureWith(`
func Fill(o *Outer) {
	scan(&o.Ptr.Leaf.N)
}`))

	wantCounts(t, a, "Leaf.N", 1, 1)
	wantCounts(t, a, "Mid.Leaf", 1, 1) // Leaf lives inside the pointee
	wantCounts(t, a, "Outer.Ptr", 1, 0)

	if !hasFinding(a, "nowrite:"+fixturePkg+".Outer.Ptr") {
		t.Error("Outer.Ptr is never assigned and must still be reported")
	}
}

// TestAssignedChainWritesEveryHop covers the same shape without the address-of:
// `out.Subject.Source.ID = v` is how enrichment/domain.Subject.Source was
// written, and it too was reported as never written.
func TestAssignedChainWritesEveryHop(t *testing.T) {
	a := analyze(t, fixtureWith(`
func Fill(o *Outer) {
	o.Mid.Leaf.N = 1
	o.Mid.Leaf.M++
}`))

	// A plain assignment writes without reading; ++ does both.
	wantCounts(t, a, "Leaf.N", 0, 1)
	wantCounts(t, a, "Leaf.M", 1, 1)
	// Every enclosing hop is written by both statements, and read by both.
	wantCounts(t, a, "Mid.Leaf", 2, 2)
	wantCounts(t, a, "Outer.Mid", 2, 2)
}

// TestReadChainIsNotAWrite is the other half of the distinction: reading through
// a chain must not manufacture a write, or `nowrite` stops finding anything.
func TestReadChainIsNotAWrite(t *testing.T) {
	a := analyze(t, fixtureWith(`
var Sink int

func Read(o *Outer) {
	Sink = o.Mid.Leaf.N
}`))

	wantCounts(t, a, "Leaf.N", 1, 0)
	wantCounts(t, a, "Mid.Leaf", 1, 0)
	wantCounts(t, a, "Outer.Mid", 1, 0)
	if !hasFinding(a, "nowrite:"+fixturePkg+".Outer.Mid") {
		t.Error("nothing writes Outer.Mid here; the finding must survive")
	}
}

// TestPromotedWriteIsOneSelector guards the shape the chain rule must not be
// confused with: `x.N` through an embedded field is ONE selector node with two
// hops, and go/types reports the embedded hop itself. Embedded fields are never
// reported, but the target must still be counted exactly once.
func TestPromotedWriteIsOneSelector(t *testing.T) {
	a := analyze(t, `package fixture

type Leaf struct{ N int }

type Outer struct {
	Leaf
	One int
}

func scan(dst ...any) {}

func Fill(o *Outer) {
	o.N = 1
	scan(&o.One)
}`)

	wantCounts(t, a, "Leaf.N", 0, 1)
	wantCounts(t, a, "Outer.One", 1, 1)
	if hasFinding(a, "nowrite:"+fixturePkg+".Outer.Leaf") {
		t.Error("an embedded field is a composition device and is never reported")
	}
}
