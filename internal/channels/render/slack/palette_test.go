package slack_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/thulasiram/oto/internal/channels/render/slack"
)

// specPath is docs/design/SPEC.md seen from this package's directory.
func specPath() string {
	return filepath.Join("..", "..", "..", "..", "docs", "design", "SPEC.md")
}

// h2Row reads one row of §H.2's state table: a state word, its hex and its emoji.
// The `acknowledged` row carries a parenthesised gloss after the state word,
// which is why the first cell is not anchored to its closing backtick.
var h2Row = regexp.MustCompile("(?m)^\\|\\s*`([a-z]+)`[^|]*\\|\\s*`(#[0-9a-f]{6})`\\s*\\|\\s*`(:[a-z_]+:)`\\s*\\|")

// anyHex finds every six-digit hex literal in a Go source file.
var anyHex = regexp.MustCompile(`#[0-9a-fA-F]{6}`)

// section returns the SPEC from a heading up to the next heading of the same or
// higher level, so a table in a later section cannot be mistaken for this one's.
func section(t *testing.T, heading string) string {
	t.Helper()

	raw, err := os.ReadFile(specPath())
	if err != nil {
		t.Fatalf("read the SPEC: %v", err)
	}
	doc := string(raw)

	start := regexp.MustCompile("(?m)^" + regexp.QuoteMeta(heading)).FindStringIndex(doc)
	if start == nil {
		t.Fatalf("%s has no %q heading — this gate reads that section, so a renamed or "+
			"renumbered heading must be followed here rather than silently skipped",
			specPath(), heading)
	}
	rest := doc[start[1]:]
	if next := regexp.MustCompile("(?m)^#{1,3} ").FindStringIndex(rest); next != nil {
		return rest[:next[0]]
	}
	return rest
}

// TestSlackPaletteUnchanged pins the five card colours to §H.2 — SPEC §M.7, AC-48.
//
// ⭐⭐ IT ASSERTS AGAINST THE SPEC, NOT AGAINST THE CODE. A test that read the
// palette out of `palette.go` into a Go literal would pass forever: an edit to a
// hue would be copied into the fixture by whoever made it, in the same diff, and
// the gate would agree with itself. §H.2 is the source of truth precisely because
// it is a different document with a different reason to exist — the OnCall
// palette's provenance (§M.6) — so an edit that changes one and not the other is
// a decision somebody has to make twice, in the open.
//
// ⛔ THIS IS NOT WHAT THE GOLDEN CORPUS COVERS. `test/harness` byte-compares the
// rendered cards against `test/fixtures/slack/*.message.json`, which carry four of
// these five literals (`expired` has no capture), and
// `TestEachCardStateCarriesItsOwnColourForAHumanToVerify` asserts only that they
// are DISTINCT — never what they are. A coordinated edit to `palette.go` and the
// captures passes both. It does not pass this.
//
// ⛔ IT WAS SIX UNTIL ADR 0042. `storm` and its `#7b1fa2` are deleted from
// `palette.go` because no group can enter storm mode, and the row-count guard
// below is what makes §H.2 drop its `storm` row too: the deletion is made twice,
// in the open, which is the whole design of this gate.
func TestSlackPaletteUnchanged(t *testing.T) {
	t.Parallel()

	// The mapping from §H.2's word to the state it names. Everything else —
	// which states exist, what colour each is, which emoji it carries — comes
	// off the page.
	states := map[string]slack.CardState{
		"firing":       slack.CardFiring,
		"acknowledged": slack.CardAcknowledged,
		"suppressed":   slack.CardSuppressed,
		"resolved":     slack.CardResolved,
		"expired":      slack.CardExpired,
	}

	rows := h2Row.FindAllStringSubmatch(section(t, "### H.2 Palettes (binding)"), -1)

	// Guards the guard. Every assertion below runs inside this loop, so a regex
	// that stops matching turns this test into a green tick over nothing.
	if len(rows) != len(states) {
		t.Fatalf("read %d state rows out of §H.2 and expected %d — an unparsed row is an "+
			"unpinned colour", len(rows), len(states))
	}

	specHexes := map[string]string{}
	for _, row := range rows {
		word, hex, emoji := row[1], row[2], row[3]

		state, ok := states[word]
		if !ok {
			t.Errorf("§H.2 names the card state %q, which this gate cannot map to a CardState. "+
				"A new state is a new colour to pin, not a row to skip.", word)
			continue
		}
		specHexes[hex] = word

		if got := state.Colour(); got != hex {
			t.Errorf("§H.2 gives %s the colour %s and palette.go returns %s. §M.6: the Slack "+
				"palette is a SEPARATE, UNCHANGED system — if this hue is meant to move, it moves "+
				"in the SPEC first and the provenance argument has to be re-made.",
				word, hex, got)
		}
		if got := state.Emoji(); got != emoji {
			t.Errorf("§H.2 gives %s the emoji %s and palette.go returns %s. Colour is not the "+
				"only channel the card answers on (§H.2: Slack's accessibility guidance requires "+
				"more than colour), so this one is load-bearing too.", word, emoji, got)
		}
	}

	if len(specHexes) != len(states) {
		t.Errorf("§H.2 spends %d colours on %d states; two states sharing a hue makes the "+
			"peripheral-vision answer to \"do I need to act?\" ambiguous", len(specHexes), len(states))
	}

	// The other direction: a hue in the renderer that §H.2 never sanctioned. The
	// `default:` arms return an existing state's colour on purpose; a NEW literal
	// here is a sixth colour nobody specified.
	source, err := os.ReadFile("palette.go")
	if err != nil {
		t.Fatalf("read palette.go: %v", err)
	}
	for _, hex := range anyHex.FindAllString(string(source), -1) {
		if _, ok := specHexes[hex]; !ok {
			t.Errorf("palette.go contains the hex %s, which appears nowhere in §H.2's table. "+
				"Every colour this file can return is specified there.", hex)
		}
	}
}
