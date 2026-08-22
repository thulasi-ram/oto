package slack_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/render/slack"
)

const cardSource = `# {{ alert.name }}

{{ annotations.summary }}

---

:::fields
Severity | {{ alert.severity | upper }}
Firing | {{ group.firing_for }}
:::

{{ actions }}

> Rule: {{ rule.name }}`

func renderWith(t *testing.T, ref *domain.TemplateRef) (slack.Payload, string) {
	t.Helper()
	r := slack.New(nil)
	msg, err := r.Render(context.Background(), smokeView(), domain.RenderOptions{
		Mode: domain.ModePostRoot, BaseURL: "https://oto.example", Template: ref,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// ⭐ THE SAME OUTBOUND GATE EVERY BUILT-IN CARD PASSES. A template's whole risk
	// is that it produces a payload Slack rejects — and a rejected payload is an
	// undelivered alert, not an ugly one.
	if err := slack.Validate(msg.Payload); err != nil {
		t.Fatalf("a template-rendered card failed oto's own outbound validation: %v", err)
	}
	var p slack.Payload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return p, msg.Fallback
}

// ⭐ THE END-TO-END PROOF: an operator's Markdown becomes a real Slack card.
func TestACardTemplateBecomesBlockKit(t *testing.T) {
	p, fallback := renderWith(t, &domain.TemplateRef{
		ID: "11111111-1111-1111-1111-111111111111", Version: 1,
		Format: "card", Source: cardSource,
	})

	blocks := p.Attachments[0].Blocks
	kinds := make([]string, 0, len(blocks))
	for _, b := range blocks {
		kinds = append(kinds, b.Type)
	}
	t.Logf("blocks: %v", kinds)
	t.Logf("fallback: %q", fallback)

	want := []string{slack.BlockSection, slack.BlockSection, slack.BlockDivider, slack.BlockSection, slack.BlockActions, slack.BlockSection}
	if len(kinds) != len(want) {
		t.Fatalf("got %v blocks, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("block %d is %s, want %s", i, kinds[i], want[i])
		}
	}
	if len(blocks[3].Fields) != 2 {
		t.Errorf("the `:::fields` grid produced %d cells, want 2", len(blocks[3].Fields))
	}
	// ⛔ THE FALLBACK IS THE TEMPLATE'S OWN WORDS. It is the push notification and
	// the screen-reader line; oto's built-in sentence there would make the
	// notification and the message disagree.
	if !strings.Contains(fallback, "OtoSmokeTest") {
		t.Errorf("the fallback does not say what the card says: %q", fallback)
	}
}

// ⛔ THE OWNER MAY SHIP A CARD WITH NO BUTTONS, AND THIS PINS THAT IT IS THEIR
// CHOICE ALONE — no compiler quietly appends the row back.
func TestATemplateMayOmitTheActionRow(t *testing.T) {
	p, _ := renderWith(t, &domain.TemplateRef{
		ID: "11111111-1111-1111-1111-111111111111", Version: 1,
		Format: "card", Source: "# {{ alert.name }}\n\nno buttons here",
	})
	for _, b := range p.Attachments[0].Blocks {
		if b.Type == slack.BlockActions {
			t.Fatal("a template with no `{{ actions }}` got an action row appended anyway")
		}
	}
}

// ⛔ THE MOST IMPORTANT PROPERTY IN THE FEATURE. Every one of these is a way a
// template can be wrong, and not one of them may cost the alert.
func TestABrokenTemplateStillDeliversOtosOwnCard(t *testing.T) {
	builtin, _ := renderWith(t, nil)
	builtinKinds := len(builtin.Attachments[0].Blocks)

	for _, tc := range []struct {
		name   string
		format string
		source string
	}{
		{"unknown filter", "card", "# {{ alert.name | no_such_filter }}"},
		{"renders nothing", "card", "{{ nothing.at.all }}"},
		{"a table, which no provider draws", "card", "| a | b |\n|---|---|"},
		{"an author's own URL as a link target", "card", "[click](https://evil.example)"},
		{"raw JSON that interpolation broke", "raw", `[{"type":"section","text":{"text":"{{ alert.name }}}}`},
		{"a raw template pointed at the wrong provider", "raw", `{"content": "discord wants this"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, fb := renderWith(t, &domain.TemplateRef{
				ID: "11111111-1111-1111-1111-111111111111", Version: 1,
				Format: tc.format, Source: tc.source,
			})
			if got := len(p.Attachments[0].Blocks); got != builtinKinds {
				t.Fatalf("got %d blocks, want oto's own card's %d — the alert did not fall back", got, builtinKinds)
			}
			if strings.TrimSpace(fb) == "" {
				t.Fatal("the fallback line is empty, so the push notification would be blank")
			}
		})
	}
}
