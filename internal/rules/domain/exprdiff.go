package domain

import (
	"slices"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// THE SEAM: "given two expression strings, what changed?"
// ---------------------------------------------------------------------------

// ExprVerdict is what an ExprComparer was able to establish about a pair of
// expressions. It is a closed set of three answers, and the third one is the
// point of the type: "the expression changed and I cannot say how" is a first
// class result, not an empty list.
type ExprVerdict string

// The verdicts.
const (
	// ExprNotCompared is the zero value: no comparison was performed. It is what
	// a Diff carries when the expression did not change at all. An ExprComparer
	// never returns it.
	ExprNotCompared ExprVerdict = ""

	// ExprNumbersMoved means the two expressions are the same expression with
	// different numbers in it: every difference between them is a numeric
	// operand that the implementation is CONFIDENT it identified correctly, and
	// ExprDiff.Numbers says which ones moved and by how much. This is the only
	// verdict under which a "the threshold moved from X to Y" narrative may be
	// shown.
	ExprNumbersMoved ExprVerdict = "numbers_moved"

	// ExprStructuralChange means the expressions differ in shape: a different
	// metric, a different aggregation, a new label matcher, a term added or
	// removed. There is no positional correspondence between their numbers, so
	// no numeric claim is made and none can be reconstructed by the caller.
	ExprStructuralChange ExprVerdict = "structural"

	// ExprUncharacterised means the expressions differ SOMEWHERE THE
	// IMPLEMENTATION REFUSES TO GUESS. The shape lines up, but the edit lands on
	// a token this implementation cannot interpret with confidence — a range
	// window, a subquery step, an `offset`, an `@` timestamp — either alone or
	// mixed with a genuine threshold edit.
	//
	// The honest rendering is "the expression changed", with both versions
	// shown, and NO claim about which numbers moved. Widening `[5m]` to `[10m]`
	// is not a threshold getting less sensitive, and saying "5 → 10" would tell
	// an operator the opposite of the truth.
	ExprUncharacterised ExprVerdict = "uncharacterised"
)

// ExprDiff is the result of comparing two expressions.
//
// Numbers is populated ONLY for ExprNumbersMoved. Under the other two verdicts
// it is nil, and that nil means "no claim", never "nothing changed" — the
// verdict is what the caller must branch on.
type ExprDiff struct {
	Verdict ExprVerdict
	// Numbers are the numeric literals that moved, in the order they appear.
	// Empty under ExprNumbersMoved means the expressions differ only in
	// whitespace.
	Numbers []NumberChange
}

// ExprComparer answers "given two versions of a rule expression, what changed?".
//
// # CONTRACT
//
// Input: two expression strings, oldest first. Neither is validated, neither is
// assumed to be well-formed PromQL, and an empty string is a legal input (a
// snapshot whose origin is `unavailable` has one).
//
// Output: exactly one ExprVerdict, never ExprNotCompared, plus — under
// ExprNumbersMoved only — the list of literals that moved. Implementations MUST
// be pure and deterministic: the same pair of strings always produces the same
// ExprDiff, because this feeds a rendered timeline and a diff that reorders or
// changes its mind between two reads is a diff nobody can trust.
//
// # THE RULE THAT MATTERS: IT MAY RETURN "I DON'T KNOW"
//
// An implementation MUST return ExprUncharacterised rather than emit a number
// change it is not sure of. A wrong threshold claim is worse than no threshold
// claim: an operator reading "5 → 10" concludes the alert got less sensitive,
// and if the edit was actually `[5m]` → `[10m]` that conclusion is backwards.
// The claim is also ALL OR NOTHING — an edit that moves a real threshold AND
// something uninterpretable is ExprUncharacterised in full, because reporting
// only the half that was understood is a partial truth that reads as the whole
// one.
//
// # IMPLEMENTING A PARSER-BACKED COMPARER
//
// LexicalExprComparer is a deliberately shallow scanner. The correct long-term
// implementation parses both sides with `promql/parser` and walks the two ASTs:
//
//   - Parse both. If EITHER fails to parse, return ExprUncharacterised — a
//     half-understood expression is exactly the case this verdict exists for.
//   - Walk the two trees in lockstep. Any difference in node type, function
//     name, metric name, label matcher, aggregation, modifier or child count is
//     ExprStructuralChange.
//   - If the trees are congruent, collect the differing leaves. Leaves that are
//     `*parser.NumberLiteral` are real numbers: report them as NumberChange in
//     traversal order, which is left-to-right source order and therefore the
//     same Index space this heuristic uses.
//   - Differences in a `MatrixSelector.Range`, a `SubqueryExpr.Range`/`Step`,
//     an `Offset`, a `StartOrEnd`/`@` timestamp or a duration of any kind are
//     NOT numbers. Congruent-but-differing there is ExprUncharacterised. A
//     parser MAY choose to characterise them properly later by extending
//     ExprDiff with a durations list; until it does, silence is the contract.
//
// Substituting one is a CompareWith argument. No caller of Compare changes.
type ExprComparer interface {
	CompareExpr(oldExpr, newExpr string) ExprDiff
}

// ---------------------------------------------------------------------------
// The shipped implementation: a lexical scanner, no parser, no dependency
// ---------------------------------------------------------------------------

// LexicalExprComparer is the ExprComparer oto ships today.
//
// It does not parse PromQL. It scans both expressions into a SKELETON — the
// text with every numeric literal replaced by a placeholder — and compares the
// skeletons. Two kinds of placeholder are used, and the difference between them
// is the whole design:
//
//   - a number that CLEARLY STANDS ALONE becomes a number hole and its value is
//     recorded for positional comparison;
//   - a run of digits the scanner will not vouch for becomes an opaque hole and
//     its raw text is recorded, so that a change confined to opaque holes can be
//     reported as "changed, and I will not characterise it".
//
// "Stands alone" is decided lexically, and conservatively. A literal is bare
// only when ALL of these hold:
//
//   - it is not part of an identifier. Identifiers are consumed whole and first,
//     so the `4` in `http_4xx_total` and the `5m` in `job:x:rate5m` are never
//     even offered to the number scanner;
//   - it is not inside a string literal. `{code="500"}` is a label matcher, and
//     a change there is structural;
//   - it carries no unit suffix. `5m`, `1h30m`, `100ms` and `0x1f` all end in
//     identifier characters and are therefore opaque, not thresholds;
//   - it is not inside a range or subquery bracket. Everything between `[` and
//     `]` is opaque on sight, including the step after the `:`;
//   - it does not abut a colon on either side;
//   - it is not the operand of `offset`, `@`, `for` or `keep_firing_for`. Those
//     take durations and timestamps, never thresholds.
//
// A leading `-` or `+` is folded into the literal when it is in prefix position
// (the previous token is not a value), so `< -10` → `< -20` reports -10 → -20
// and not 10 → 20, while `x - 5` keeps the minus as the operator it is.
//
// What this buys, and what it costs: `rate(x[5m]) > 100` → `rate(x[10m]) > 100`
// is ExprUncharacterised instead of a fabricated threshold drift, and
// `a > 100` → `a > 200` is still reported exactly. What it cannot do is tell
// you that the WINDOW widened, because a scanner that claimed to know `[10m]`
// is a duration would be back in the business of guessing.
type LexicalExprComparer struct{}

// CompareExpr implements ExprComparer.
func (LexicalExprComparer) CompareExpr(oldExpr, newExpr string) ExprDiff {
	from, to := scanExpr(oldExpr), scanExpr(newExpr)

	// Different skeletons means the expressions are not the same expression with
	// different numbers in it. The number holes are part of the skeleton, so a
	// literal appearing or disappearing lands here too.
	if from.shape != to.shape || len(from.numbers) != len(to.numbers) {
		return ExprDiff{Verdict: ExprStructuralChange}
	}

	// Same shape, but something moved inside a hole the scanner will not read.
	// This is checked BEFORE the numbers so that an edit touching both is
	// reported as uncharacterised in full rather than as a bare threshold drift.
	if !slices.Equal(from.opaque, to.opaque) {
		return ExprDiff{Verdict: ExprUncharacterised}
	}

	var moved []NumberChange
	for i := range from.numbers {
		if from.numbers[i] != to.numbers[i] {
			moved = append(moved, NumberChange{Index: i, Old: from.numbers[i], New: to.numbers[i]})
		}
	}
	return ExprDiff{Verdict: ExprNumbersMoved, Numbers: moved}
}

// The two placeholders. NUL cannot appear in a rule expression that Prometheus
// accepted, which is what makes them safe to use as holes.
const (
	numberHole = "\x00N\x00"
	opaqueHole = "\x00O\x00"
)

// skeleton is one expression reduced to its shape plus the values punched out of
// it. Two skeletons with the same shape describe the same expression.
type skeleton struct {
	// shape is the expression with every literal replaced by a hole.
	shape string
	// numbers are the values of the bare literals, in source order.
	numbers []float64
	// opaque are the raw texts of the digit-carrying runs the scanner refuses to
	// interpret, in source order.
	opaque []string
}

// tokenKind is the little state the scanner needs about what it just emitted:
// enough to tell a unary sign from a binary operator, and to know when a
// duration is expected next.
type tokenKind uint8

const (
	// tokStart is the beginning of the expression.
	tokStart tokenKind = iota
	// tokValue is something a binary operator may follow: an identifier, a
	// literal, a string, or a closing bracket.
	tokValue
	// tokOperator is anything else — an operator, a comma, an opening bracket.
	tokOperator
	// tokDurationKeyword is a token whose operand is a duration or a timestamp
	// and therefore never a threshold.
	tokDurationKeyword
)

// durationKeywords take a duration or a timestamp as their operand. `for` and
// `keep_firing_for` are rule fields rather than expression syntax, but a rule
// pasted whole into an expression field is not worth a wrong answer.
var durationKeywords = map[string]bool{
	"offset": true, "for": true, "keep_firing_for": true,
}

// scanExpr reduces an expression to its skeleton.
//
// Whitespace is collapsed first so that a reformatted rule does not read as an
// edit. The scan is a single left-to-right pass with no backtracking: whichever
// branch claims a byte owns it, and identifiers claim theirs before the number
// scanner is ever offered a digit.
func scanExpr(expr string) skeleton {
	src := strings.Join(strings.Fields(expr), " ")

	var (
		out  skeleton
		b    strings.Builder
		prev = tokStart
		// depth counts range/subquery brackets. Anything inside one is opaque.
		depth int
	)

	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ':
			b.WriteByte(c)
			i++

		case isIdentStart(c):
			j := i + 1
			for j < len(src) && isIdentChar(src[j]) {
				j++
			}
			word := src[i:j]
			b.WriteString(word)
			if durationKeywords[strings.ToLower(word)] {
				prev = tokDurationKeyword
			} else {
				prev = tokValue
			}
			i = j

		case c == '"' || c == '\'' || c == '`':
			j := endOfString(src, i)
			b.WriteString(src[i:j])
			prev = tokValue
			i = j

		case startsNumber(src, i, prev):
			end, bare := scanNumber(src, i)
			raw := src[i:end]
			bare = bare && depth == 0 && prev != tokDurationKeyword && !abutsColon(src, i, end)
			if bare {
				if f, err := strconv.ParseFloat(raw, 64); err == nil {
					out.numbers = append(out.numbers, f)
					b.WriteString(numberHole)
				} else {
					// Unparseable but bare: record it as opaque rather than as a
					// number nobody can act on.
					out.opaque = append(out.opaque, raw)
					b.WriteString(opaqueHole)
				}
			} else {
				out.opaque = append(out.opaque, raw)
				b.WriteString(opaqueHole)
			}
			prev = tokValue
			i = end

		default:
			switch c {
			case '[':
				depth++
			case ']':
				if depth > 0 {
					depth--
				}
			}
			b.WriteByte(c)
			prev = punctuationKind(c)
			i++
		}
	}

	out.shape = b.String()
	return out
}

// startsNumber reports whether a numeric literal begins at i, folding in a
// leading sign only when that sign is in prefix position. `x -5` is a
// subtraction of 5; `> -5` is a threshold of minus five.
func startsNumber(src string, i int, prev tokenKind) bool {
	c := src[i]
	if (c == '-' || c == '+') && prev != tokValue {
		return i+1 < len(src) && startsDigits(src, i+1)
	}
	return startsDigits(src, i)
}

// startsDigits reports whether a digit run (possibly `.5`) begins at i.
func startsDigits(src string, i int) bool {
	if i >= len(src) {
		return false
	}
	if isDigit(src[i]) {
		return true
	}
	return src[i] == '.' && i+1 < len(src) && isDigit(src[i+1])
}

// scanNumber consumes the literal starting at i and reports where it ends and
// whether it is BARE — a self-contained number with no unit glued to it.
//
// A suffix of identifier characters is what makes a literal a duration (`5m`,
// `1h30m`) or something stranger (`0x1f`), and either way it is not a threshold.
func scanNumber(src string, i int) (end int, bare bool) {
	j := i
	if j < len(src) && (src[j] == '-' || src[j] == '+') {
		j++
	}
	for j < len(src) && isDigit(src[j]) {
		j++
	}
	if j < len(src) && src[j] == '.' {
		j++
		for j < len(src) && isDigit(src[j]) {
			j++
		}
	}
	// An exponent is part of the literal, so `1e9` is a number and not a `1`
	// with a `e9` unit. It only counts when digits actually follow.
	if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
		k := j + 1
		if k < len(src) && (src[k] == '-' || src[k] == '+') {
			k++
		}
		if k < len(src) && isDigit(src[k]) {
			for k < len(src) && isDigit(src[k]) {
				k++
			}
			j = k
		}
	}
	// Whatever is glued on the end comes with the literal, so that the opaque
	// text records `10m` and not a `10` that lost its unit.
	if j < len(src) && isUnitChar(src[j]) {
		for j < len(src) && isUnitChar(src[j]) {
			j++
		}
		return j, false
	}
	return j, true
}

// abutsColon reports whether a colon touches the literal on either side. A colon
// is a recording-rule name separator or a subquery step separator, and a number
// next to one is part of a construct rather than a threshold in its own right.
func abutsColon(src string, start, end int) bool {
	if start > 0 && src[start-1] == ':' {
		return true
	}
	return end < len(src) && src[end] == ':'
}

// endOfString returns the offset just past the string literal opening at i,
// honouring backslash escapes. An unterminated literal runs to end of input,
// which keeps its bytes out of the number scanner.
func endOfString(src string, i int) int {
	quote := src[i]
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			if quote != '`' {
				j++
			}
		case quote:
			return j + 1
		}
	}
	return len(src)
}

// punctuationKind classifies a single punctuation byte for the unary-sign test.
// A closing bracket is a value: `foo[5m] - 1` subtracts.
func punctuationKind(c byte) tokenKind {
	switch c {
	case ')', ']', '}':
		return tokValue
	case '@':
		return tokDurationKeyword
	default:
		return tokOperator
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isIdentStart excludes ':' deliberately. A leading colon is legal in a
// recording-rule name but is far more often a subquery step separator, and
// mistaking `[30m:1m]` for an identifier would hide the step from the scan.
func isIdentStart(c byte) bool { return isLetter(c) || c == '_' }

func isIdentChar(c byte) bool { return isLetter(c) || isDigit(c) || c == '_' || c == ':' }

// isUnitChar is what may be glued to a literal to stop it being bare.
func isUnitChar(c byte) bool { return isLetter(c) || isDigit(c) || c == '_' }
