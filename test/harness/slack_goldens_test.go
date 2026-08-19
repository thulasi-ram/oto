package harness_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/thulasiram/oto/test/harness"
)

// ⭐⭐ THE ARTEFACT A HUMAN WITH A WORKSPACE NEEDS, FROZEN SO IT CANNOT DRIFT.
//
// git-bug edb670f: the Block Kit card has never been rendered by Slack. Nothing
// in this file changes that. What it does is make closing the gap a ten-minute
// job instead of a rebuild: every card variant oto can emit is checked in as the
// exact bytes it would send, plus a second file in the shape Slack's own Block
// Kit Builder accepts. A reviewer opens
//
//	https://app.slack.com/block-kit-builder
//
// pastes seven files, and sees Slack's renderer draw them. That is the only
// offline route to Slack's actual renderer that exists.
//
// ⛔ THE TWO FILES ARE NOT INTERCHANGEABLE AND THE DIFFERENCE IS THE FINDING.
// `*.message.json` is the wire truth. `*.blockkit.json` is that card's blocks
// lifted out of its attachment, because BLOCK KIT BUILDER CANNOT RENDER
// ATTACHMENTS — and every oto block lives inside one, which is the only way to
// get the colour bar (§H.1 S3). So the builder can prove the blocks are legal
// and legible and it can prove NOTHING about the colour bar, the top-level text,
// the fallback or the metadata. Those four are the residual, and
// docs/setup/slack.md is the checklist for them.

var updateGoldens = flag.Bool("update-slack-goldens", false,
	"rewrite test/fixtures/slack from the current renderer")

// slackGoldenDir is harness.SlackGoldenDir seen from this package's directory.
func slackGoldenDir() string { return filepath.Join("..", "fixtures", "slack") }

// TestTheSlackCardCorpusMatchesItsCheckedInCaptures is the regression fence.
//
// A diff here is a rendering change somebody has to look at ON PURPOSE — and,
// more to the point, it means the files a human was about to paste into Block
// Kit Builder no longer describe the card oto sends. A stale capture is worse
// than none: it gets verified, signed off, and vouches for something else.
func TestTheSlackCardCorpusMatchesItsCheckedInCaptures(t *testing.T) {
	t.Parallel()

	files, err := harness.SlackGoldenFiles()
	if err != nil {
		t.Fatalf("render the card corpus: %v", err)
	}
	dir := slackGoldenDir()

	if *updateGoldens {
		if err := harness.WriteSlackGoldens(dir); err != nil {
			t.Fatalf("write goldens: %v", err)
		}
		t.Log("rewrote", dir)
		return
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		want := files[name]
		got, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a fixture path built from a constant
		if err != nil {
			t.Errorf("%s is missing (run `go test ./test/harness -run Corpus "+
				"-update-slack-goldens`): %v", name, err)
			continue
		}
		if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
			t.Errorf("%s no longer matches the renderer.\n--- checked in ---\n%s\n"+
				"--- rendered now ---\n%s", name, got, want)
		}
	}

	// A file in the directory that the corpus no longer produces is a capture for
	// a card that no longer exists, and it will be pasted into Block Kit Builder
	// by somebody who has no way to know that.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if _, ok := files[e.Name()]; !ok {
			t.Errorf("%s is an orphaned capture: the renderer no longer produces it", e.Name())
		}
	}
}

// The Block Kit Builder file has to be something the builder will actually
// accept, or the whole artefact is decorative.
func TestEveryBuilderCaptureIsSomethingBlockKitBuilderWillAccept(t *testing.T) {
	t.Parallel()

	captures, err := harness.RenderSlackCards()
	if err != nil {
		t.Fatalf("render the card corpus: %v", err)
	}

	for _, c := range captures {
		t.Run(c.Card.Name, func(t *testing.T) {
			t.Parallel()

			var builder map[string]json.RawMessage
			if err := json.Unmarshal(c.Builder, &builder); err != nil {
				t.Fatalf("the builder capture is not a JSON object: %v", err)
			}
			// The builder takes `{"blocks": […]}` and nothing else. An `attachments`
			// key silently renders as an empty preview and a reviewer concludes the
			// card is broken when it is the paste that is wrong.
			if _, ok := builder["attachments"]; ok {
				t.Error("the builder capture still carries `attachments`, which Block Kit " +
					"Builder cannot render")
			}
			raw, ok := builder["blocks"]
			if !ok {
				t.Fatal("the builder capture has no `blocks` key")
			}
			var blocks []json.RawMessage
			if err := json.Unmarshal(raw, &blocks); err != nil {
				t.Fatalf("`blocks` is not an array: %v", err)
			}
			if len(blocks) == 0 {
				t.Fatal("the builder capture has no blocks; there would be nothing to look at")
			}

			// ⛔ GO'S JSON ESCAPING MUST NOT HAVE CREPT BACK IN. `json.Marshal`
			// rewrites `<`, `>` and `&` as <, > and & by default, which
			// turns every mrkdwn link — `<url|label>` — into a \u-escaped string. It
			// round-trips through a JSON parser identically, so nothing breaks in Go;
			// it breaks for the HUMAN, who is pasting the file into a text box and
			// reading the result. The renderer disables the escaping (`marshal` in
			// blockkit.go) and so must the capture, or the file somebody verifies is
			// not legible as the thing oto sends.
			// The escape is written as bytes rather than as a literal so this test
			// cannot itself be defeated by an editor that helpfully un-escapes it.
			if bytes.Contains(c.Builder, []byte{'\\', 'u'}) {
				t.Error("the builder capture carries JSON unicode escapes; Go's encoder " +
					"rewrites <, > and & that way by default, and the mrkdwn in the file " +
					"is no longer readable by the person pasting it")
			}

			// ⭐ THE ESCAPING GUARANTEE STATED POSITIVELY, PER CARD, so the negative
			// check above cannot pass by being vacuous: a capture with no `<` in it
			// proves nothing about whether the escaping is disabled. `<` is the mrkdwn
			// control character Go's encoder would have rewritten — a link, a
			// `<!date^…>` token or a `<@U…>` mention.
			//
			// ⚠️ THIS IS THE PER-CARD FORM RESTORED. `bd0fb1d` weakened it to a
			// corpus-wide assertion for one honest reason: it deleted the unacked
			// reminder, whose body carried both a mention and an "open in oto" link,
			// and its replacement `broadcast_refired` rendered
			// `:repeat: *Re-fired* — case #N` with no control character at all. The
			// choice then was to invent fixture content or drop the guarantee, and
			// dropping it to corpus scope was the lesser harm. `68653ca` removed the
			// premise instead: a broadcasting reply now carries a real link because a
			// channel reader needs one, so every capture has a `<` again on its own
			// merits and the stronger check costs nothing.
			// ⚠️ IF YOU ARE HERE BECAUSE A CARD YOU ADDED TRIPPED THIS, READ THIS FIRST.
			// The check is an implicit constraint on what a capture may contain, and
			// that constraint is deliberate but it is not obvious. Every ROOT card
			// satisfies it by construction — `root.go` puts a `<!date^…>` token in
			// every one — but several REPLY reasons (`new_alerts`, `some_resolved`,
			// `unsuppressed`, a note-less `enriched`) have no guaranteed link, date or
			// token in their body at all.
			//
			// There are exactly two honest ways out and inventing fixture content is
			// NEITHER of them: either the card is missing an affordance it should have
			// (which is what `68653ca` turned out to be — a broadcast with nothing to
			// click), or it genuinely has no control character and the guarantee has to
			// move back to corpus scope with the reason recorded, which is what
			// `bd0fb1d` did and why it was defensible. Do not add a link to a card that
			// should not have one just to make this line green.
			if !bytes.Contains(c.Builder, []byte("<")) {
				t.Error("this builder capture contains no mrkdwn control character, so " +
					"the unicode-escape check above proves nothing for this card — see the " +
					"comment above for the two honest ways out, neither of which is " +
					"inventing content for the fixture")
			}

			// The wire capture is the one thing that must equal what the provider
			// sends, so it keeps its attachment.
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(c.Wire, &wire); err != nil {
				t.Fatalf("the wire capture is not a JSON object: %v", err)
			}
			for _, key := range []string{"text", "attachments", "unfurl_links", "unfurl_media"} {
				if _, ok := wire[key]; !ok {
					t.Errorf("the wire capture is missing %q — it is not the payload oto sends", key)
				}
			}
		})
	}

}

// ⛔ THE COLOUR BAR IS THE ONE THING NO OFFLINE CHECK CAN REACH, so the corpus
// must at least prove that every state HAS a distinct one to check. A palette
// that collapsed two states onto one colour would make the peripheral-vision
// answer to "do I need to act?" wrong, and Block Kit Builder would never show it.
func TestEachCardStateCarriesItsOwnColourForAHumanToVerify(t *testing.T) {
	t.Parallel()

	captures, err := harness.RenderSlackCards()
	if err != nil {
		t.Fatalf("render the card corpus: %v", err)
	}

	byColour := map[string][]string{}
	for _, c := range captures {
		if c.Colour == "" {
			t.Errorf("%s has no attachment colour; the card answers nothing at a glance",
				c.Card.Name)
			continue
		}
		byColour[c.Colour] = append(byColour[c.Colour], c.Card.Name)
	}

	// firing, acked, silenced and resolved are four different answers to "do I need
	// to act?". The two firing-coloured cards (the root and the unacked reminder)
	// are the same answer, which is why they share one.
	//
	// ⛔ IT WAS FIVE UNTIL ADR 0042. `storm_notice` carried §H.2's `#7b1fa2`, and
	// that colour is deleted from the palette along with the state — no group can
	// enter storm mode, so no card can be purple.
	for colour, names := range byColour {
		if len(names) > 2 {
			t.Errorf("%d cards share the colour %s (%v); the colour has stopped "+
				"distinguishing states", len(names), colour, names)
		}
	}
	if len(byColour) < 4 {
		t.Errorf("the corpus only exercises %d colours; §H.2's state palette has more, "+
			"and a colour nobody captured is a colour nobody can verify", len(byColour))
	}
}
