package filter

import (
	"sort"
	"strings"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Compiled is the index-backed form of a filter AST: three containment shapes
// that `alerts_labels_idx` (GIN, jsonb_path_ops) can answer directly.
//
//	LabelsAll  → labels @> {…}                   (every entry must hold)
//	LabelsAny  → labels @> {k:v1} OR {k:v2}      (one entry per key must hold)
//	LabelsNone → NOT (labels @> {…})             (no entry may hold)
//
// Nothing else exists, and that is the whole point of ADR 0017: the set of
// predicates that survive translation is a short, explicit list rather than an
// emergent property of a query planner.
type Compiled struct {
	LabelsAll  map[string]string
	LabelsAny  map[string][]string
	LabelsNone map[string]string
}

// IsZero reports whether nothing was compiled.
func (c Compiled) IsZero() bool {
	return len(c.LabelsAll) == 0 && len(c.LabelsAny) == 0 && len(c.LabelsNone) == 0
}

// Compile lowers an AST onto Compiled, or refuses.
//
// ⭐ ADR 0017, binding: a predicate that cannot be answered from an index is
// rejected here with a precise message naming the matcher. It is NEVER degraded
// into a sequential scan — a query that works in staging and times out during an
// incident is worse than one that says no.
//
// A regular-expression matcher survives ONLY when it is a pure alternation of
// literals (`critical|warning`), which is exactly an IN list and therefore
// index-backed. Any other regex — a character class, a quantifier, an anchor — is
// refused, because answering it means reading every row.
func Compile(field string, n Node) (Compiled, error) {
	out := Compiled{}
	if n == nil {
		return out, nil
	}
	if err := compileNode(field, n, &out, false); err != nil {
		return Compiled{}, err
	}
	return out, nil
}

func compileNode(field string, n Node, out *Compiled, negated bool) error {
	switch v := n.(type) {
	case And:
		for _, c := range v.Children {
			if c == nil {
				continue
			}
			if err := compileNode(field, c, out, negated); err != nil {
				return err
			}
		}
		return nil

	case Or:
		return compileOr(field, v, out, negated)

	case Not:
		if negated {
			return unsupported(field, "a doubly-negated matcher is not supported")
		}
		if v.Child == nil {
			return nil
		}
		return compileNode(field, v.Child, out, true)

	case Label:
		return compileLabel(field, v.Matcher, out, negated)

	default:
		return unsupported(field, "this predicate cannot be answered from an index")
	}
}

func compileLabel(field string, m Matcher, out *Compiled, negated bool) error {
	values, err := literalValues(field, m)
	if err != nil {
		return err
	}

	negative := negated != m.Op.IsNegative()
	if negative {
		if len(values) != 1 {
			// `NOT (a OR b)` over a GIN containment is `NOT a AND NOT b`, which
			// is representable — but only one negative entry per key exists in
			// LabelsNone, so a multi-valued negation would silently lose a term.
			// Refuse rather than under-filter.
			return unsupported(field,
				"a negated matcher must name exactly one value; split it into separate matchers")
		}
		if out.LabelsNone == nil {
			out.LabelsNone = map[string]string{}
		}
		if prior, dup := out.LabelsNone[m.Name]; dup && prior != values[0] {
			return unsupported(field,
				"two negated matchers on the same label are not supported; use one")
		}
		out.LabelsNone[m.Name] = values[0]
		return nil
	}

	if len(values) == 1 {
		if prior, dup := out.LabelsAll[m.Name]; dup && prior != values[0] {
			// `k="a" AND k="b"` is unsatisfiable rather than unsupported, but a
			// filter bar that produces it has a bug and a silent empty page hides
			// it.
			return unsupported(field,
				"two conflicting matchers on the same label can never match; use one")
		}
		if out.LabelsAll == nil {
			out.LabelsAll = map[string]string{}
		}
		out.LabelsAll[m.Name] = values[0]
		return nil
	}

	addAny(out, m.Name, values)
	return nil
}

func compileOr(field string, v Or, out *Compiled, negated bool) error {
	if negated {
		return unsupported(field, "a negated disjunction is not supported")
	}

	name := ""
	var values []string
	for _, c := range v.Children {
		lbl, ok := c.(Label)
		if !ok {
			// The AST admits nested shapes; the index does not. Only a
			// same-key disjunction is an IN list.
			return unsupported(field, "only a disjunction over one label is supported")
		}
		if lbl.Matcher.Op.IsNegative() {
			return unsupported(field, "a disjunction of negated matchers is not supported")
		}
		if name == "" {
			name = lbl.Matcher.Name
		} else if name != lbl.Matcher.Name {
			return unsupported(field, "only a disjunction over one label is supported")
		}
		vals, err := literalValues(field, lbl.Matcher)
		if err != nil {
			return err
		}
		values = append(values, vals...)
	}
	if name == "" || len(values) == 0 {
		return nil
	}
	addAny(out, name, values)
	return nil
}

func addAny(out *Compiled, name string, values []string) {
	if out.LabelsAny == nil {
		out.LabelsAny = map[string][]string{}
	}
	out.LabelsAny[name] = dedupe(append(out.LabelsAny[name], values...))
}

// literalValues turns a matcher into the literal values an index can test.
//
// An equality matcher is one value. A regex matcher is expanded ONLY when it is a
// pure alternation of literals; anything else is refused here rather than pushed
// down as a scan.
func literalValues(field string, m Matcher) ([]string, error) {
	if !m.Op.IsRegex() {
		return []string{m.Value}, nil
	}

	body := strings.TrimSuffix(strings.TrimPrefix(m.Value, "^"), "$")
	if body == "" {
		return nil, unsupported(field, "an empty regular expression cannot be answered from an index")
	}
	parts := strings.Split(body, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, `.*+?()[]{}\^$|`) {
			return nil, unsupported(field,
				"only a regular expression that is an alternation of literal values "+
					`(for example severity=~"critical|warning") can be answered from an index`)
		}
		out = append(out, p)
	}
	return dedupe(out), nil
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, v := range in {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func unsupported(field, message string) error {
	return errs.Validation("validation_failed", "1 field failed validation.", errs.Violation{
		Field: field, Code: "invalid", Message: message,
	})
}
