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
			// The same guarantee stated positively, so the check above cannot pass by
			// being vacuous. `<` is the mrkdwn control character Go's encoder would
			// have rewritten, and every card carries at least one — a link, a
			// `<!date^…>` token or a `<@U…>` mention.
			if !bytes.Contains(c.Builder, []byte("<")) {
				t.Error("the capture contains no mrkdwn control character at all, so the " +
					"escaping check above proves nothing for this card")
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

	// firing, acked, silenced, resolved and storm are five different answers to
	// "do I need to act?". The two firing-coloured cards (the root and the unacked
	// reminder) are the same answer, which is why they share one.
	for colour, names := range byColour {
		if len(names) > 2 {
			t.Errorf("%d cards share the colour %s (%v); the colour has stopped "+
				"distinguishing states", len(names), colour, names)
		}
	}
	if len(byColour) < 5 {
		t.Errorf("the corpus only exercises %d colours; §H.2's state palette has more, "+
			"and a colour nobody captured is a colour nobody can verify", len(byColour))
	}
}
