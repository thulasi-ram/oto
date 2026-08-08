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
//	LabelsNone → NOT (labels @> {k:v1}) AND …    (no entry may hold)
//
// Nothing else exists, and that is the whole point of ADR 0017: the set of
// predicates that survive translation is a short, explicit list rather than an
// emergent property of a query planner.
//
// Measured on 60 000 alerts (Postgres 17, `alerts_labels_gin`, jsonb_path_ops):
// LabelsAll is a Bitmap Index Scan; LabelsAny is a BitmapOr over one Bitmap
// Index Scan per value; LabelsNone is a Filter applied on top of whichever of
// those drives the plan. A negation alone drives nothing — see the note on
// compileLabel.
type Compiled struct {
	LabelsAll  map[string]string
	LabelsAny  map[string][]string
	LabelsNone map[string][]string
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
// index-backed. Any other regex — a character class, a quantifier, a
// wildcard — is refused, because answering it means reading every row. The two
// plans, measured:
//
//	severity=~"critical|warning"  BitmapOr → 2 × Bitmap Index Scan, 3 758 buffers
//	severity=~"crit.*"            Seq Scan, 60 000 rows examined, 48 000 discarded
//
// The second is what "silently degraded to a scan" looks like, and it is why the
// answer is a 422 naming the matcher instead.
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
		// `NOT (a OR b)` over a GIN containment is `NOT a AND NOT b`, and both
		// halves are representable, so a multi-valued negation is admitted whole
		// rather than refused. Two negated matchers on the same label simply
		// accumulate — they conjoin, which is what an operator means by writing
		// both.
		//
		// ⚠️ A negation is a FILTER and never a DRIVER. `NOT (labels @> …)` has
		// no index (measured: Parallel Seq Scan over all 60 000 rows), so it is
		// applied on top of whichever positive predicate drives the plan. It is
		// admitted regardless because `label[!tier]=canary` is a documented
		// filter of this API and always has been; what ADR 0017 forbids is a
		// predicate whose SUPPORTED SPELLING implies an index it does not have.
		if out.LabelsNone == nil {
			out.LabelsNone = map[string][]string{}
		}
		out.LabelsNone[m.Name] = dedupe(append(out.LabelsNone[m.Name], values...))
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

// regexMetacharacters is every character whose presence in an alternation branch
// means the branch is not a literal. It is listed exhaustively rather than
// probed with regexp, because "does this regex happen to match only literals" is
// undecidable in the direction that matters and a wrong answer here is a scan.
const regexMetacharacters = `.*+?()[]{}\^$|`

// unsupportedRegex is the message a refused regex matcher gets.
//
// ⭐ ADR 0017 binds the MANNER of the refusal, not only the fact of it: a
// predicate that cannot use an index is rejected at parse time "with a precise
// message". Precise means the caller learns exactly which spellings work without
// having to guess, so the message enumerates both halves of the boundary.
const unsupportedRegex = `a regular-expression matcher is supported only when it is an ` +
	`alternation of literal values, because that is exactly an IN list over the label ` +
	`index — for example severity=~"critical|warning" or tier!~"canary|staging", with ` +
	`optional ^ and $ anchors. Metacharacters (. * + ? ( ) [ ] { } \) are refused rather ` +
	`than answered by reading every alert: a filter that works in staging and times out ` +
	`during an incident is worse than one that says no. Use = or != for a single value, ` +
	`comma-separated values for a set, or the q= parameter for free-text search.`

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
		return nil, unsupported(field, unsupportedRegex)
	}
	parts := strings.Split(body, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, regexMetacharacters) {
			return nil, unsupported(field, unsupportedRegex)
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
