package slack

import (
	"os"
	"regexp"
	"sort"
	"testing"

	"github.com/thulasiram/oto/internal/channels/render/wording"
)

// reach classifies how a Wording could ever reach one of §L.6's outbound checks.
type reach int

const (
	// structural means Go builds the thing being checked and a Wording cannot
	// influence it at all. A Wording's output type is a STRING; it never becomes a
	// block, an attachment, a colour, an id, an action or a flag.
	structural reach = iota
	// bounded means a Wording's text does flow into the checked value, and the
	// check is a length or emptiness rule the sink already enforces before the
	// payload is built — escape() then truncateSection/truncateField, plus the
	// fallback to Go's own text when a Wording renders empty.
	bounded
)

// wordingReach is ADR 0037's gate artifact: every identifier in validate.go
// classified user-unreachable or budget-bounded.
//
//	"If a check lands in neither bucket, the feature does not ship."
//
// ⚠️ THE ADR ASKS FOR A COMPILE-TIME FAILURE AND THIS IS A TEST-TIME ONE. The
// checks are identified by STRING LITERALS passed to fail(), not by constants, so
// there is no symbol a new check would have to name and nothing for the compiler
// to notice. Changing that would mean rewriting validate.go's error path for the
// benefit of this test. The scan below is the honest substitute: it reads the
// source, finds every identifier actually used, and fails on any that is missing
// here — which catches the same mistake one step later, at `just test` rather than
// at `go build`, and cannot be satisfied by forgetting to update a list.
var wordingReach = map[string]struct {
	how  reach
	note string
}{
	"V0": {structural, "the payload is marshalled by Go; a Wording contributes a string inside it, never its framing"},
	"V1": {structural, "attachment count is Go's; a Wording emits no attachment"},
	"V2": {structural, "colour encodes state exclusively (R10, ADR 0037 refuses it)"},
	"V3": {structural, "block count is Go's; a Wording substitutes text INTO a block Go already decided to emit"},
	"V4": {structural, "the block-type whitelist is Go's; a Wording cannot introduce a type"},

	"V5": {bounded, "section text — the primary surface a Wording reaches. Bounded by truncateSection, and an empty render falls back to Go's text"},
	"V6": {bounded, "field text — reached by the fields stanza. Bounded by truncateField; field COUNT stays Go's"},
	"V7": {bounded, "context element text — reached by the footer/trail stanzas. Inner text bounded by the shared text-object rule; element COUNT stays Go's"},

	"V8":  {structural, "actions block elements; StanzaActions is refused outright"},
	"V9":  {structural, "button text; a button label is bound to its action id, not wordable"},
	"V10": {structural, "images; a Wording emits no image and no URL"},
	"V11": {structural, "button and option values are bare UUIDs minted by Go"},
	"V12": {structural, "block_id and action_id namespaces are assigned by Go"},
	"V13": {structural, "button style; a Wording chooses no style"},

	// ⚠️ V14 IS BOUNDED BY REFUSAL RATHER THAN BY THE SINK, AND THE DIFFERENCE
	// MATTERS. SPEC §L.6 S5: the top-level text is the push notification, the
	// sidebar preview, the search snippet and the only thing a screen reader reads,
	// and it is the one surface only partially escaped by design. ADR 0037 refuses
	// a Wording there. So this check is unreachable TODAY because of a policy
	// decision, not because of a mechanism — which is exactly the kind of guarantee
	// that decays silently. TestTopLevelTextTakesNoWording is what holds it.
	"V14": {structural, "top-level text; ADR 0037 refuses a Wording on this surface (S5)"},

	"V15": {structural, "unfurl flags are Go's"},
	"V16": {structural, "block_id uniqueness is Go's"},
	"V17": {structural, "metadata is Go's; a Wording contributes nothing to event_payload"},
	"V18": {bounded, "total payload size; every Wording's contribution passes the per-surface sink first"},
}

var vIdentifier = regexp.MustCompile(`"(V\d+)"`)

// TestEveryOutboundCheckIsClassified is the gate. A new check in validate.go that
// nobody has thought about with respect to Wordings fails here.
func TestEveryOutboundCheckIsClassified(t *testing.T) {
	src, err := os.ReadFile("validate.go")
	if err != nil {
		t.Fatalf("read validate.go: %v", err)
	}
	found := map[string]bool{}
	for _, m := range vIdentifier.FindAllStringSubmatch(string(src), -1) {
		found[m[1]] = true
	}
	if len(found) == 0 {
		t.Fatal("found no V-identifiers — the scan is broken, not validate.go")
	}

	var missing, stale []string
	for id := range found {
		if _, ok := wordingReach[id]; !ok {
			missing = append(missing, id)
		}
	}
	for id := range wordingReach {
		if !found[id] {
			stale = append(stale, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	if len(missing) > 0 {
		t.Errorf("validate.go has checks %v that are not classified in wordingReach.\n"+
			"ADR 0037: every check must be user-unreachable or budget-bounded, and "+
			"\"if a check lands in neither bucket, the feature does not ship\".\n"+
			"Decide which bucket the new check is in and say why.", missing)
	}
	if len(stale) > 0 {
		t.Errorf("wordingReach classifies %v, which validate.go no longer has. "+
			"Remove them so the list keeps meaning something.", stale)
	}
}

// TestTheReachableSetIsSmallAndDeliberate pins WHICH checks a Wording can reach.
// Widening this set is a real change to the safety property and should be a
// deliberate edit with an argument attached, not a side effect.
func TestTheReachableSetIsSmallAndDeliberate(t *testing.T) {
	var reachable []string
	for id, c := range wordingReach {
		if c.how == bounded {
			reachable = append(reachable, id)
		}
	}
	sort.Strings(reachable)
	want := []string{"V18", "V5", "V6", "V7"} // sorted lexically
	if len(reachable) != len(want) {
		t.Fatalf("a Wording now reaches %v; ADR 0037's argument covers %v", reachable, want)
	}
	for i := range want {
		if reachable[i] != want[i] {
			t.Fatalf("a Wording now reaches %v; ADR 0037's argument covers %v", reachable, want)
		}
	}
}

// TestEveryClassificationCarriesAReason — a bucket with no sentence is a guess.
func TestEveryClassificationCarriesAReason(t *testing.T) {
	for id, c := range wordingReach {
		if len(c.note) < 20 {
			t.Errorf("%s is classified with no real reason: %q", id, c.note)
		}
	}
}

// TestTopLevelTextTakesNoWording holds V14's classification, which rests on a
// refusal rather than on a mechanism.
//
// SPEC §L.6 S5: the top-level text is the push notification, the sidebar preview,
// the search snippet "and the only thing screen readers read", and it is the one
// surface only partially escaped by design. ADR 0037 refuses a Wording on it. The
// refusal is enforced by the top-level text simply not being one of §H.7's eight
// Stanzas — so this test fails the moment somebody adds a ninth that is.
func TestTopLevelTextTakesNoWording(t *testing.T) {
	for _, s := range wording.AllStanzas {
		switch s {
		case wording.StanzaTitle, wording.StanzaBody, wording.StanzaFields,
			wording.StanzaMembers, wording.StanzaTrail, wording.StanzaRule,
			wording.StanzaActions, wording.StanzaFooter:
		default:
			t.Errorf("%q is a new stanza. If it is the top-level text or the "+
				"attachment fallback, ADR 0037 refuses it (S5); if it is something "+
				"else, classify it here deliberately.", s)
		}
	}
	if len(wording.AllStanzas) != 8 {
		t.Errorf("SPEC §H.7 budgets eight blocks; the stanza set has %d", len(wording.AllStanzas))
	}
}
