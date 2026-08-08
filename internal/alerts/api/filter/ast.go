package filter

import (
	"sort"
	"strings"
)

// Op is one of Alertmanager's four matcher operators. The set is closed: there is
// no `contains`, no `startswith` and no `in`, because each of those is either
// expressible with the four or not index-backed.
type Op string

// The four operators (openapi.yaml `MatcherOp`).
const (
	// OpEqual is `=`.
	OpEqual Op = "="
	// OpNotEqual is `!=`.
	OpNotEqual Op = "!="
	// OpRegex is `=~`.
	OpRegex Op = "=~"
	// OpNotRegex is `!~`.
	OpNotRegex Op = "!~"
)

// Valid reports whether o is one of the four.
func (o Op) Valid() bool {
	switch o {
	case OpEqual, OpNotEqual, OpRegex, OpNotRegex:
		return true
	default:
		return false
	}
}

// IsRegex reports whether the operator matches by regular expression.
func (o Op) IsRegex() bool { return o == OpRegex || o == OpNotRegex }

// IsNegative reports whether the operator excludes rather than includes.
func (o Op) IsNegative() bool { return o == OpNotEqual || o == OpNotRegex }

// Matcher is one label predicate: `namespace="payments"`, `severity=~"crit.*"`.
type Matcher struct {
	Name  string
	Op    Op
	Value string
}

// String renders the matcher back into its wire syntax.
func (m Matcher) String() string { return m.Name + string(m.Op) + `"` + m.Value + `"` }

// ---------------------------------------------------------------------- AST

// Node is one predicate in the filter AST.
//
// The interface is deliberately closed — the only implementations are in this
// file — so that Compile can exhaustively decide, for every shape, whether an
// index can answer it.
type Node interface {
	// Selectivity is a coarse ordering hint, not a cost model: lower sorts first
	// so that the cheapest containment test is written into the query first.
	Selectivity() int
	node()
}

// And is the conjunction of its children. Distinct filter dimensions AND
// together (openapi.yaml `listAlerts`).
type And struct{ Children []Node }

// Or is the disjunction of its children. Comma-separated values within one
// parameter OR together.
type Or struct{ Children []Node }

// Not negates its child. It is produced by `!=`, `!~` and the `label[!k]=v`
// negation marker.
type Not struct{ Child Node }

// Label is a leaf: one Matcher against the alert's label set.
type Label struct{ Matcher Matcher }

func (And) node()   {}
func (Or) node()    {}
func (Not) node()   {}
func (Label) node() {}

// Selectivity of a conjunction is its cheapest child.
func (n And) Selectivity() int { return minSelectivity(n.Children, 0) }

// Selectivity of a disjunction is its most expensive child.
func (n Or) Selectivity() int { return maxSelectivity(n.Children) }

// Selectivity of a negation is its child's, plus a penalty: a NOT can never be
// answered by a containment index on its own.
func (n Not) Selectivity() int {
	if n.Child == nil {
		return 100
	}
	return n.Child.Selectivity() + 50
}

// Selectivity of a leaf ranks equality above regex, because only equality is
// answerable from the GIN containment index.
func (n Label) Selectivity() int {
	if n.Matcher.Op.IsRegex() {
		return 40
	}
	return 10
}

func minSelectivity(ns []Node, def int) int {
	best := -1
	for _, c := range ns {
		if c == nil {
			continue
		}
		if s := c.Selectivity(); best < 0 || s < best {
			best = s
		}
	}
	if best < 0 {
		return def
	}
	return best
}

func maxSelectivity(ns []Node) int {
	best := 0
	for _, c := range ns {
		if c == nil {
			continue
		}
		if s := c.Selectivity(); s > best {
			best = s
		}
	}
	return best
}

// Selector is a parsed label selector: a flat conjunction of matchers, which is
// the only shape either front end can produce today.
type Selector struct{ Matchers []Matcher }

// IsZero reports whether the selector constrains nothing.
func (s Selector) IsZero() bool { return len(s.Matchers) == 0 }

// AST lifts the selector into the filter AST.
//
// Matchers that share a name are OR-ed with each other and AND-ed with the rest,
// which is exactly the semantics openapi.yaml gives `label[team]=core,platform`.
// Negative matchers never join an OR group: "not core or not platform" is a
// predicate nobody means, and treating `label[!team]=core,platform` as
// "neither core nor platform" is the reading that matches the documented example.
func (s Selector) AST() Node {
	if s.IsZero() {
		return nil
	}

	positives := map[string][]Matcher{}
	var order []string
	var negatives []Matcher

	for _, m := range s.Matchers {
		if m.Op.IsNegative() {
			negatives = append(negatives, m)
			continue
		}
		if _, seen := positives[m.Name]; !seen {
			order = append(order, m.Name)
		}
		positives[m.Name] = append(positives[m.Name], m)
	}

	var children []Node
	for _, name := range order {
		group := positives[name]
		if len(group) == 1 {
			children = append(children, Label{Matcher: group[0]})
			continue
		}
		alts := make([]Node, 0, len(group))
		for _, m := range group {
			alts = append(alts, Label{Matcher: m})
		}
		children = append(children, Or{Children: alts})
	}
	for _, m := range negatives {
		children = append(children, Not{Child: Label{Matcher: Matcher{
			Name: m.Name, Op: positiveOf(m.Op), Value: m.Value,
		}}})
	}

	// Cheapest predicate first, stably: two runs over the same query must build
	// the same tree, or the filter hash a cursor is bound to would move.
	sort.SliceStable(children, func(i, j int) bool {
		return children[i].Selectivity() < children[j].Selectivity()
	})

	if len(children) == 1 {
		return children[0]
	}
	return And{Children: children}
}

func positiveOf(o Op) Op {
	switch o {
	case OpNotEqual:
		return OpEqual
	case OpNotRegex:
		return OpRegex
	default:
		return o
	}
}

// Canonical renders the selector as a stable string, for the cursor filter hash.
// Two spellings of the same selector must hash identically or a caller would be
// told its own cursor is invalid for reordering its own query string.
func (s Selector) Canonical() string {
	parts := make([]string, 0, len(s.Matchers))
	for _, m := range s.Matchers {
		parts = append(parts, m.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
