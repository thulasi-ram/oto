package slack_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/channels/domain"
	slackrender "github.com/thulasiram/oto/internal/channels/render/slack"
	"github.com/thulasiram/oto/internal/channels/render/wording"
	"github.com/thulasiram/oto/internal/platform/clock"
)

// continuedMarkerText is §H.9's marker as the card spells it. The renderer holds
// the constant; a test in the external package has to name the string, and if the
// two ever disagree TestAWordedFooterKeepsTheContinuedMarker is what says so.
const continuedMarkerText = "_continued from an earlier card_"

// renderWith renders the first live run's card with the given wordings applied.
func renderWith(t *testing.T, w map[string]string) (domain.RenderedMessage, slackrender.Payload) {
	t.Helper()
	return renderOpts(t, func(o *domain.RenderOptions) { o.Wordings = w })
}

func renderOpts(t *testing.T, tweak func(*domain.RenderOptions)) (domain.RenderedMessage, slackrender.Payload) {
	t.Helper()
	o := domain.RenderOptions{
		Mode:           domain.ModePostRoot,
		Verbosity:      domain.VerbosityAll,
		BaseURL:        "http://localhost:8080",
		MaxInstances:   10,
		ShowFieldEmoji: true,
	}
	tweak(&o)
	msg, err := slackrender.New(clock.New()).Render(context.Background(), smokeView(), o)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var p slackrender.Payload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return msg, p
}

func blockText(p slackrender.Payload, id string) string {
	for _, b := range p.Attachments[0].Blocks {
		if !strings.HasPrefix(b.BlockID, "oto_"+id+"_") {
			continue
		}
		if b.Text != nil {
			return b.Text.Text
		}
		if len(b.Elements) > 0 {
			raw, _ := json.Marshal(b.Elements)
			return string(raw)
		}
	}
	return ""
}

// TestAWordingChangesTheWordsAndNothingElse is the feature working, and the
// safety property holding, in one test.
func TestAWordingChangesTheWordsAndNothingElse(t *testing.T) {
	_, before := renderWith(t, nil)
	_, after := renderWith(t, map[string]string{
		"body": `{{ alert.name | default: "a signal" }} has been firing ` +
			`{{ group.firing_for | default: "a while" }} and is ` +
			`{{ case.ack_state | default: "unacked" }}`,
	})

	if len(before.Attachments) != len(after.Attachments) {
		t.Errorf("a wording changed the attachment count: %d -> %d",
			len(before.Attachments), len(after.Attachments))
	}
	if before.Attachments[0].Color != after.Attachments[0].Color {
		t.Errorf("a wording changed the colour, which encodes state exclusively (R10): %q -> %q",
			before.Attachments[0].Color, after.Attachments[0].Color)
	}
	if before.Text != after.Text {
		t.Errorf("a wording reached the top-level text, which ADR 0037 refuses (S5):\n%q\n%q",
			before.Text, after.Text)
	}
	if before.Attachments[0].Fallback != after.Attachments[0].Fallback {
		t.Errorf("a wording reached the attachment fallback (S5): %q -> %q",
			before.Attachments[0].Fallback, after.Attachments[0].Fallback)
	}
	if len(before.Attachments[0].Blocks) != len(after.Attachments[0].Blocks) {
		t.Fatalf("a wording changed the block count: %d -> %d",
			len(before.Attachments[0].Blocks), len(after.Attachments[0].Blocks))
	}
	for i := range before.Attachments[0].Blocks {
		bb, ab := before.Attachments[0].Blocks[i], after.Attachments[0].Blocks[i]
		if bb.Type != ab.Type {
			t.Errorf("block %d changed type: %q -> %q", i, bb.Type, ab.Type)
		}
		if bb.BlockID != ab.BlockID {
			t.Errorf("block %d changed block_id: %q -> %q", i, bb.BlockID, ab.BlockID)
		}
	}

	body := blockText(after, "body")
	if !strings.Contains(body, "has been firing") {
		t.Errorf("the wording did not reach the body: %q", body)
	}
	if body == blockText(before, "body") {
		t.Error("the body is unchanged — the wording did nothing")
	}
}

// TestAWordingStillPassesTheOutboundChecks — the whole point of bounding it.
func TestAWordingStillPassesTheOutboundChecks(t *testing.T) {
	huge := `{{ annotations.summary | default: "x" }}` + strings.Repeat("padding words ", 200)
	msg, _ := renderWith(t, map[string]string{
		"body":   huge,
		"title":  huge,
		"footer": huge,
		"rule":   huge,
	})
	if err := slackrender.Validate(msg.Payload); err != nil {
		t.Fatalf("an oversized wording produced an invalid payload: %v", err)
	}
}

// TestAFailingWordingFallsBackRatherThanKillingTheCard is ADR 0037's central
// promise: a Wording can never mark a delivery dead.
func TestAFailingWordingFallsBackRatherThanKillingTheCard(t *testing.T) {
	_, plain := renderWith(t, nil)
	for name, bad := range map[string]string{
		"unknown filter": `{{ alert.name | no_such_filter }}`,
		"unknown field":  `{{ nothing.at.all }}`,
		"renders empty":  `{{ nothing.at.all | default: "" }}`,
		"unparseable":    `{{ alert.name`,
		"refused stanza": `{{ alert.name }}`,
	} {
		t.Run(name, func(t *testing.T) {
			key := "body"
			if name == "refused stanza" {
				key = "fields"
			}
			msg, p := renderWith(t, map[string]string{key: bad})
			if err := slackrender.Validate(msg.Payload); err != nil {
				t.Fatalf("a broken wording produced an invalid payload: %v", err)
			}
			if got, want := blockText(p, "body"), blockText(plain, "body"); got != want {
				t.Errorf("a broken wording must fall back to oto's own text.\n got %q\nwant %q", got, want)
			}
		})
	}
}

// TestAWordingCannotPingTheChannel through the real renderer, not just the unit.
func TestAWordingCannotPingTheChannel(t *testing.T) {
	msg, p := renderWith(t, map[string]string{
		"body": `<!channel> @everyone <@U024BE7LH> <!subteam^SAZ94GDB8> deploy now`,
	})
	if err := slackrender.Validate(msg.Payload); err != nil {
		t.Fatalf("invalid payload: %v", err)
	}
	body := blockText(p, "body")
	for _, tok := range []string{"<!channel>", "<@U", "<!subteam^", "@everyone"} {
		if strings.Contains(body, tok) {
			t.Errorf("a wording emitted %q: %q", tok, body)
		}
	}
	if strings.Contains(string(msg.Payload), "<!channel>") {
		t.Errorf("a mention reached the payload: %s", msg.Payload)
	}
}

// TestTheRuleLinkSurvivesAWording — Go keeps owning the way out of the card.
func TestTheRuleLinkSurvivesAWording(t *testing.T) {
	_, p := renderWith(t, map[string]string{
		"rule": `the rule said {{ rule.expr | code }}`,
	})
	rule := blockText(p, "rule")
	if !strings.Contains(rule, "the rule said") {
		t.Fatalf("the wording did not reach the rule stanza: %q", rule)
	}
	if strings.Contains(rule, "changed since the last case") != (smokeView().RuleChange != nil) {
		t.Errorf("the rule-change link did not survive the wording: %q", rule)
	}
}

// TestAWordedFooterKeepsTheContinuedMarker — §H.9's marker is not the customer's
// to drop: it is what stops a recovered card reading as a second incident.
func TestAWordedFooterKeepsTheContinuedMarker(t *testing.T) {
	_, p := renderOpts(t, func(o *domain.RenderOptions) {
		o.Continued = true
		o.Wordings = map[string]string{"footer": `sent by oto for {{ org.name | default: "you" }}`}
	})
	footer := blockText(p, "footer")
	if !strings.Contains(footer, "sent by oto") {
		t.Errorf("the wording did not reach the footer: %q", footer)
	}
	if !strings.Contains(footer, continuedMarkerText) {
		t.Errorf("§H.9's continued marker was dropped by a wording: %q", footer)
	}
}

// TestOnlyProseStanzasAcceptAWording, end to end.
func TestOnlyProseStanzasAcceptAWording(t *testing.T) {
	for _, s := range wording.AllStanzas {
		if s.Wordable() {
			continue
		}
		_, p := renderWith(t, map[string]string{string(s): `REPLACED`})
		for _, b := range p.Attachments[0].Blocks {
			if b.Text != nil && strings.Contains(b.Text.Text, "REPLACED") {
				t.Errorf("%s is refused but its text was substituted: %q", s, b.Text.Text)
			}
		}
	}
}
