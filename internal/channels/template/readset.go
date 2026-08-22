package template

import (
	"regexp"
	"strings"
	"time"

	"github.com/osteele/liquid"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// freeFormRoots are the branches of Input whose keys come from the
// customer's own data rather than from oto's vocabulary.
//
// A missing key under one of these is NOT a typo — `labels.team` is absent from a
// signal that carries no team label and present on the next one — so it must
// degrade at delivery rather than being refused at save time. Everything OUTSIDE
// these roots is oto's closed vocabulary, where a missing key IS a typo and the
// author wants to hear about it now.
var freeFormRoots = map[string]bool{
	"labels":      true,
	"annotations": true,
	"enrichment":  true,
}

// maxReferenceProbes bounds the read-set walk. A Stanza is one line of prose; a
// template with more than this many distinct references is not one.
const maxReferenceProbes = 48

// unknownFields returns the references a Wording makes that oto has no field for.
//
// ⛔ THIS REPLACES StrictVariables AS THE TYPO CHECK, AND IT HAD TO. ADR 0037 says
// authoring is strict so "a typo is refused while a human is present to be told",
// and that every field reference carries a `default` for totality. Those two
// sentences are incompatible in this engine, verified: under StrictVariables,
// `{{ alert.nmae | default: "-" }}` fails with "undefined variable" — the strict
// check fires BEFORE the filter, so `default` never runs and cannot rescue
// anything. Strict alone therefore cannot tell a misspelling from a field that is
// legitimately absent on a digest, a resolved card, or a signal with no rule
// snapshot, and it would refuse every honest wording that mentions one.
//
// So the typo check is done against a MAXIMAL view — one where every field oto can
// ever produce is present — and absence there means the name does not exist rather
// than that this particular card lacks it. Delivery stays lax and `default` does
// the totality job the ADR gave it.
func unknownFields(src string) []string {
	t, err := strictly().ParseString(src)
	if err != nil {
		return nil // the parse pass already reported it
	}
	probe := maximalInput()
	var unknown []string
	seen := map[string]bool{}
	for i := 0; i < maxReferenceProbes; i++ {
		_, err := t.Render(liquid.Bindings(probe))
		if err == nil {
			break
		}
		// ⛔ EVERY PATH IN THE EXPRESSION IS PLANTED, NOT JUST THE FIRST. Liquid's
		// message quotes the whole offending expression and does not say WHICH of
		// its references was undefined. Taking the text before the first `|` named
		// the wrong one whenever the typo was in a FILTER ARGUMENT —
		// `{{ alert.name | default: alert.nmae }}` reported `alert.name`, which is
		// a real field, so planting it changed nothing, the loop burned all its
		// probes, and the save was refused naming a field the author had spelled
		// correctly. Planting every reference guarantees progress, and only the ones
		// that were genuinely absent are reported.
		paths := referencedPaths(err.Error())
		if len(paths) == 0 {
			break // a different failure; Validate's render pass reports it
		}
		progressed := false
		for _, path := range paths {
			if lookup(probe, path) {
				continue // already resolvable, so it was not the undefined one
			}
			progressed = true
			plant(probe, path)
			if root := strings.SplitN(path, ".", 2)[0]; !freeFormRoots[root] && !seen[path] {
				seen[path] = true
				unknown = append(unknown, path)
			}
		}
		// ⚠️ NO PROGRESS MEANS STOP, rather than spin to the probe ceiling. It also
		// means this loop can no longer mask a second typo behind a first: the old
		// version `break`ed the entire pass on any message it could not parse, so
		// `{{ labls["team"] }} {{ alert.nmae }}` reported nothing at all and the
		// typo shipped.
		if !progressed {
			break
		}
	}
	return unknown
}

// referencedPaths pulls every FIELD REFERENCE out of liquid's quoted expression.
//
// ⛔ QUOTED LITERALS ARE REMOVED FIRST, AND THAT IS NOT TIDINESS. Without it the
// author's own strings become "fields": `{{ annotations.runbook | default:
// "runbook.example.com" }}` was refused with "runbook.example.com is not a field
// of an oto notification", which is both wrong and baffling — the thing it names
// is the fallback text they typed.
//
// ⛔ AND A SINGLE-SEGMENT TOKEN IS A REFERENCE TOO. Requiring a dot meant a
// misspelt ROOT — `{{ labls | default: "-" }}` — was silently accepted and rendered
// "-" forever, which is the exact failure mode the typo pass exists to prevent and
// is worse than the noisy version. Filter names are single-segment as well, so
// they are excluded by name from the curated set rather than by shape.
func referencedPaths(msg string) []string {
	const marker = "undefined variable in {{"
	i := strings.Index(msg, marker)
	if i < 0 {
		return nil
	}
	expr := msg[i+len(marker):]
	if j := strings.Index(expr, "}}"); j >= 0 {
		expr = expr[:j]
	}
	expr = dotBrackets(expr)
	expr = stripQuoted(expr)

	filters := map[string]bool{}
	for _, f := range FilterNames {
		filters[f] = true
	}
	keywords := map[string]bool{"true": true, "false": true, "nil": true, "null": true,
		"and": true, "or": true, "contains": true, "empty": true, "blank": true}

	var out []string
	for _, tok := range strings.FieldsFunc(expr, func(r rune) bool {
		return r != '.' && r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9')
	}) {
		tok = strings.Trim(tok, ".")
		if tok == "" || filters[tok] || keywords[strings.ToLower(tok)] {
			continue
		}
		if c := tok[0]; c >= '0' && c <= '9' {
			continue // a numeric literal, or a filter argument like `40`
		}
		out = append(out, tok)
	}
	return out
}

// dotBrackets rewrites `labels["team"]` as `labels.team`.
//
// ⛔ WITHOUT IT A BRACKET INDEX HIDES EVERY LATER TYPO. The two forms mean the same
// thing to Liquid, but the tokeniser only understands dots — so `labls["team"]`
// yielded the bare root, planting it did not satisfy the indexed lookup, the next
// probe failed on the same expression, the loop saw no progress and stopped. A
// second, ordinary typo later in the same template then shipped. Normalising the
// two spellings into one is what lets the planting loop make progress.
var bracketIndex = regexp.MustCompile(`\[\s*["']([A-Za-z0-9_.\-]+)["']\s*\]`)

func dotBrackets(s string) string {
	return bracketIndex.ReplaceAllString(s, ".$1")
}

// stripQuoted blanks the contents of every quoted literal, so an author's own
// fallback text is never mistaken for a field name.
func stripQuoted(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
			b.WriteByte(' ')
		case quote != 0:
			b.WriteByte(' ')
		case r == '\'' || r == '"':
			quote = r
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// lookup reports whether a dotted path already resolves in the probe.
func lookup(in Input, path string) bool {
	var cur any = map[string]any(in)
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[p]
		if !ok {
			return false
		}
	}
	return true
}

// plant inserts a placeholder at a dotted path so the probe can continue past it.
func plant(in Input, path string) {
	parts := strings.Split(path, ".")
	cur := map[string]any(in)
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = ""
			return
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
}

// maximalInput is a view in which every optional field is present, used only to
// decide whether a name EXISTS. It is never rendered onto a card.
func maximalInput() Input {
	at := fixtureClock.Add(90 * time.Minute)
	ended := at
	v := firingView()
	v.Actor = &domain.ActorView{Kind: "user", Label: "someone"}
	v.Comment = "a note"
	v.SnoozedUntil = &ended
	v.Previous = &domain.PreviousState{State: "firing", AckState: "unacked"}
	v.Digest = &domain.DigestView{Count: 1, CoveredFrom: fixtureClock, CoveredTo: at}
	v.Case.EndedAt = &ended
	v.Case.AckedAt = &ended
	v.Case.AckedByLabel = "someone"
	v.Case.AckNote = "looking"
	v.Case.ResolveReason = "upstream"
	v.Case.SuppressionReason = "silenced"
	v.RuleChange = &domain.RuleChangeView{
		PreviousSnapshotID: "s0", PreviousCapturedAt: fixtureClock,
		ExprChanged: true, PreviousExpr: "a", NewExpr: "b",
		ForChanged: true, PreviousFor: time.Minute, NewFor: 2 * time.Minute,
		LabelDiff:      map[string][2]string{"team": {"a", "b"}},
		AnnotationDiff: map[string][2]string{"summary": {"a", "b"}},
	}
	f := v.Alerts[0].Value
	if f == nil {
		x := 1.5
		v.Alerts[0].Value = &x
	}
	// The probe asks which NAMES exist, which is the same set in every format;
	// card is chosen because it is the default and the one with the most surface.
	in, _ := BuildInput(v, at, FormatCard)
	return in
}
