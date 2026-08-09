package domain

import "sort"

// ChangeKind says what happened to one map entry between two snapshots.
type ChangeKind string

// The map change kinds.
const (
	// ChangeAdded means the key exists only in the newer snapshot.
	ChangeAdded ChangeKind = "added"
	// ChangeRemoved means the key exists only in the older snapshot.
	ChangeRemoved ChangeKind = "removed"
	// ChangeModified means the key exists in both with different values.
	ChangeModified ChangeKind = "modified"
)

// MapChange is one added, removed or modified label or annotation.
type MapChange struct {
	Kind ChangeKind
	Name string
	// Old is empty for ChangeAdded, New is empty for ChangeRemoved.
	Old string
	New string
}

// NumberChange is one numeric literal whose value moved between two versions of
// an expression.
//
// This is the cheap, honest answer to "how has the threshold drifted". oto does
// NOT parse PromQL — a parser is a maintenance liability and a source of
// confident wrong answers — so numbers are extracted positionally: the nth
// numeric literal in the old expression is compared with the nth in the new
// one, and the comparison is reported ONLY when the two expressions are
// otherwise identical and every literal in them clearly stands alone. Those
// conditions are what make the claim safe: if only bare numbers moved, the nth
// number really is the same number.
//
// The Index space contains ONLY literals the comparer vouched for. Durations,
// subquery steps, `offset` operands and digits inside identifiers are not in it
// at all — see ExprComparer, which is the contract that decides.
type NumberChange struct {
	// Index is the ordinal of the literal within the expression, 0-based.
	Index int
	Old   float64
	New   float64
}

// Delta is New minus Old.
func (n NumberChange) Delta() float64 { return n.New - n.Old }

// Diff is the difference between two versions of the same rule.
//
// A Diff is only meaningful between two snapshots sharing a Key. Comparing
// across rules is a category error, and Compare records it rather than
// pretending: SameRule is false and the caller can refuse to render it.
type Diff struct {
	// From is the older snapshot, To the newer.
	From Snapshot
	To   Snapshot

	// SameRule reports that both sides share a rule Key.
	SameRule bool
	// Changed reports that the content addresses differ. Two snapshots of the
	// same rule text always diff to Changed=false, whenever they were captured.
	Changed bool

	ExprChanged bool
	// ExprVerdict is what the ExprComparer could establish about the expression
	// change, and it is the field to branch on. It is ExprNotCompared when the
	// expression did not change.
	ExprVerdict ExprVerdict
	// ExprNumbers are the numeric literals that moved when the expression is
	// otherwise unchanged. Nil under any verdict but ExprNumbersMoved, and that
	// nil means "no claim", never "nothing changed".
	ExprNumbers []NumberChange
	// ExprStructural reports that the threshold-drift narrative does not apply
	// and must not be shown as if it did — either because the expression changed
	// in shape or because oto refused to characterise the change. It does NOT
	// distinguish those two; ExprVerdict does.
	ExprStructural bool

	ForChanged           bool
	ForDelta             float64
	KeepFiringForChanged bool
	KeepFiringForDelta   float64

	Labels      []MapChange
	Annotations []MapChange

	// OriginChanged reports that the two captures came from different recovery
	// paths, which is a reason for a difference that is NOT a rule edit: a
	// generator_url capture knows no `for:`, so promoting to prometheus_api can
	// look like a threshold change when nothing was edited.
	OriginChanged bool
}

// Empty reports whether nothing at all differs.
func (d Diff) Empty() bool { return !d.Changed }

// Compare diffs two snapshots of the same rule, oldest first.
//
// It is a pure function over two rows: no I/O, no clock, no ordering
// assumptions beyond the caller's. Passing them in the wrong order produces a
// correctly-signed diff of the opposite sense, which is why the service always
// orders by captured_at before calling.
//
// Expressions are compared with the ExprComparer oto ships. Swapping in a
// parser-backed one is CompareWith and nothing else.
func Compare(from, to Snapshot) Diff {
	return CompareWith(from, to, LexicalExprComparer{})
}

// CompareWith is Compare with the expression comparer chosen by the caller.
//
// This is the seam. A parser-backed ExprComparer is substituted here, and no
// other line of this package changes when one arrives.
func CompareWith(from, to Snapshot, exprs ExprComparer) Diff {
	d := Diff{
		From:     from,
		To:       to,
		SameRule: sameKey(from.Key, to.Key),
		Changed:  from.Fingerprint != to.Fingerprint,

		ExprChanged:          from.Expr != to.Expr,
		ForChanged:           from.ForSeconds != to.ForSeconds,
		ForDelta:             to.ForSeconds - from.ForSeconds,
		KeepFiringForChanged: from.KeepFiringForSeconds != to.KeepFiringForSeconds,
		KeepFiringForDelta:   to.KeepFiringForSeconds - from.KeepFiringForSeconds,

		Labels:        diffMaps(from.Labels, to.Labels),
		Annotations:   diffMaps(from.Annotations, to.Annotations),
		OriginChanged: from.Origin != to.Origin,
	}

	if d.ExprChanged {
		e := exprs.CompareExpr(from.Expr, to.Expr)
		d.ExprVerdict = e.Verdict
		d.ExprNumbers = e.Numbers
		// Anything but a vouched-for numeric drift means no number claim is on
		// offer, whether because the shape moved or because oto would not guess.
		d.ExprStructural = e.Verdict != ExprNumbersMoved
	}
	return d
}

func sameKey(a, b Key) bool {
	return a.SourceID == b.SourceID && a.Name == b.Name &&
		a.Group == b.Group && a.File == b.File
}

// diffMaps produces a deterministically ordered list of map changes.
func diffMaps(from, to map[string]string) []MapChange {
	seen := make(map[string]struct{}, len(from)+len(to))
	names := make([]string, 0, len(from)+len(to))
	for k := range from {
		seen[k] = struct{}{}
		names = append(names, k)
	}
	for k := range to {
		if _, ok := seen[k]; !ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)

	var out []MapChange
	for _, n := range names {
		oldV, hadOld := from[n]
		newV, hasNew := to[n]
		switch {
		case hadOld && hasNew && oldV != newV:
			out = append(out, MapChange{Kind: ChangeModified, Name: n, Old: oldV, New: newV})
		case hadOld && !hasNew:
			out = append(out, MapChange{Kind: ChangeRemoved, Name: n, Old: oldV})
		case !hadOld && hasNew:
			out = append(out, MapChange{Kind: ChangeAdded, Name: n, New: newV})
		}
	}
	return out
}
