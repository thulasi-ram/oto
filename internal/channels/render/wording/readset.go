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
	for i := 0; i < maxReferenceProbes; i++ {
		_, err := t.Render(liquid.Bindings(probe))
		if err == nil {
			break
		}
		path, ok := undefinedPath(err.Error())
		if !ok {
			break // a different failure; Validate's render pass reports it
		}
		if root := strings.SplitN(path, ".", 2)[0]; !freeFormRoots[root] {
			unknown = append(unknown, path)
		}
		// Plant the path either way, so the next probe finds the NEXT reference
		// instead of stopping at this one.
		plant(probe, path)
	}
	return unknown
}

// undefinedPath pulls the referenced path out of liquid's message, which reads
// `undefined variable in {{ alert.nmae | default: "-" }}` and carries no line or
// column. For a one-line Stanza the expression is enough to point at.
func undefinedPath(msg string) (string, bool) {
	const marker = "undefined variable in {{"
	i := strings.Index(msg, marker)
	if i < 0 {
		return "", false
	}
	rest := msg[i+len(marker):]
	if j := strings.Index(rest, "}}"); j >= 0 {
		rest = rest[:j]
	}
	if j := strings.IndexAny(rest, "|"); j >= 0 {
		rest = rest[:j]
	}
	path := strings.TrimSpace(rest)
	if path == "" || strings.ContainsAny(path, " \"'()[]") {
		return "", false
	}
	return path, true
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
