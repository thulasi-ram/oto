package wording

import (
	"strings"
	"time"

	"github.com/osteele/liquid"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// freeFormRoots are the branches of StanzaInput whose keys come from the
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

// referencedPaths pulls every dotted identifier out of liquid's quoted expression.
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
	var out []string
	for _, tok := range strings.FieldsFunc(expr, func(r rune) bool {
		return r != '.' && r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9')
	}) {
		tok = strings.Trim(tok, ".")
		// A bare number, a filter name with no dot, or a quoted literal's contents
		// are not field references. A reference oto could satisfy always has a root
		// in StanzaInput or is a dotted path.
		if tok == "" || !strings.Contains(tok, ".") {
			continue
		}
		if r := tok[0]; r >= '0' && r <= '9' {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// lookup reports whether a dotted path already resolves in the probe.
func lookup(in StanzaInput, path string) bool {
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
func plant(in StanzaInput, path string) {
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
func maximalInput() StanzaInput {
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
	return BuildInput(v, at)
}
