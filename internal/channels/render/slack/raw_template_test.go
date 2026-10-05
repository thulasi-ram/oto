package slack_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/render/slack"
)

// renderTemplated renders v through a raw template in mode, and fails the test
// if the result is something oto's own outbound gate would refuse.
func renderTemplated(
	t *testing.T, v *domain.NotificationView, mode domain.Mode, ref *domain.TemplateRef,
) slack.Payload {
	t.Helper()
	msg, err := slack.New(nil).Render(context.Background(), v, domain.RenderOptions{
		Mode: mode, BaseURL: "https://oto.example", ShowFieldEmoji: true, Template: ref,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := slack.Validate(msg.Payload); err != nil {
		t.Fatalf("a templated message failed oto's own outbound validation: %v", err)
	}
	var p slack.Payload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return p
}

func rawRef(source, reply string) *domain.TemplateRef {
	return &domain.TemplateRef{
		ID: "11111111-1111-1111-1111-111111111111", Version: 1,
		Format: "raw", Source: source, ReplySource: reply,
	}
}

func blockText(b slack.Block) string {
	if b.Text != nil {
		return b.Text.Text
	}
	var out strings.Builder
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(b.Elements)
	return out.String()
}

// The message an operator asked for: their colour, their notification line, their
// layout, oto's link and oto's buttons — and braces written the way JSON writes them.
const wholeMessage = `{"text":"{{ alert.name }} is {{ alert.state }}","color":"#123abc","blocks":[
{"type":"section","text":{"type":"mrkdwn","text":"*<{{ links.group }}|{{ alert.name }}>*  ·  {{ alert.severity | upper | bold }}"}},
{"type":"context","elements":[{"type":"mrkdwn","text":"Fired {{ group.started_at }}"}]},
{"type":"oto_actions"}
]}`

// ⭐ A RAW TEMPLATE OWNS EVERY ELEMENT OF THE MESSAGE (ADR 0051).
func TestARawTemplateOwnsTheWholeMessage(t *testing.T) {
	t.Parallel()
	p := renderTemplated(t, smokeView(), domain.ModePostRoot, rawRef(wholeMessage, ""))

	if got := p.Attachments[0].Color; got != "#123abc" {
		t.Errorf("the bar is %s, want the template's #123abc", got)
	}
	if p.Text != "OtoSmokeTest is firing" {
		t.Errorf("the top-level text is %q, want the template's own", p.Text)
	}
	blocks := p.Attachments[0].Blocks
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3: %+v", len(blocks), blocks)
	}
	title := blockText(blocks[0])
	if !strings.Contains(title, "<http://localhost:8080/groups/019fe297-d84f-7599-b5b2-1f231749104a|OtoSmokeTest>") {
		t.Errorf("the link handle did not become oto's address: %q", title)
	}
	if !strings.Contains(title, "*CRITICAL*") {
		t.Errorf("`| bold` did not become Slack's bold in a mrkdwn object: %q", title)
	}
	if footer := blockText(blocks[1]); !strings.Contains(footer, "<!date^") {
		t.Errorf("the time mark did not become a <!date> token: %q", footer)
	}
	if blocks[2].Type != slack.BlockActions || !strings.Contains(blockText(blocks[2]), `"oto.ack"`) {
		t.Errorf("`oto_actions` did not become oto's own action row: %+v", blocks[2])
	}
	for _, b := range blocks {
		if strings.ContainsFunc(blockText(b), func(r rune) bool { return r >= '' && r <= '' }) {
			t.Errorf("a private-use mark reached Slack: %q", blockText(b))
		}
	}
}

// ⛔ AN ALERT'S OWN TEXT CAN NEITHER BREAK THE AUTHOR'S JSON NOR PING THE CHANNEL.
// Anyone who can fire a metric can write an annotation.
func TestARawValueCannotBreakTheJSONOrPingTheChannel(t *testing.T) {
	t.Parallel()
	v := smokeView()
	v.Alerts[0].Annotations = map[string]string{"summary": "say \"hi\" \\ then\n<!channel> & go"}
	v.Focus = &v.Alerts[0]

	src := `[{"type":"section","text":{"type":"mrkdwn","text":"{{ annotations.summary }}"}},
{"type":"section","text":{"type":"mrkdwn","text":"{{ annotations.summary | upper }}"}}]`
	p := renderTemplated(t, v, domain.ModePostRoot, rawRef(src, ""))

	blocks := p.Attachments[0].Blocks
	if len(blocks) != 2 || !strings.HasPrefix(blocks[0].BlockID, "oto_raw") {
		t.Fatalf("the template fell back to oto's own card, so the value broke its JSON: %+v", blocks)
	}
	got := blockText(blocks[0])
	if want := "say \"hi\" \\ then\n&lt;!channel&gt; &amp; go"; got != want {
		t.Errorf("summary rendered as %q, want %q", got, want)
	}
	if up := blockText(blocks[1]); up != "SAY \"HI\" \\ THEN\n&lt;!CHANNEL&gt; &amp; GO" {
		t.Errorf("`| upper` changed what the value means: %q", up)
	}
}

// ⛔ A TEMPLATE SLACK WOULD REFUSE COSTS THE TEMPLATE, NEVER THE ALERT (ADR 0050).
// A hand-written button with a value oto cannot dispatch used to fail the whole
// render, and Render's error marks a delivery dead.
func TestARawTemplateSlackWouldRefuseFallsBackInsteadOfKillingTheAlert(t *testing.T) {
	t.Parallel()
	builtin := renderTemplated(t, smokeView(), domain.ModePostRoot, nil)
	src := `[{"type":"actions","elements":[{"type":"button","text":{"type":"plain_text","text":"Ack"},"action_id":"oto.ack","value":"x"}]}]`
	p := renderTemplated(t, smokeView(), domain.ModePostRoot, rawRef(src, ""))

	if len(p.Attachments[0].Blocks) != len(builtin.Attachments[0].Blocks) {
		t.Fatalf("got %d blocks, want oto's own card's %d", len(p.Attachments[0].Blocks), len(builtin.Attachments[0].Blocks))
	}
}

// A colour Slack cannot parse is dropped silently by Slack, taking the state cue with
// it — so oto keeps its own instead.
func TestARawColourSlackCannotParseIsOtos(t *testing.T) {
	t.Parallel()
	src := `{"color":"red","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"x"}}]}`
	p := renderTemplated(t, smokeView(), domain.ModePostRoot, rawRef(src, ""))
	if got, want := p.Attachments[0].Color, slack.CardFiring.Colour(); got != want {
		t.Errorf("the bar is %s, want oto's firing colour %s", got, want)
	}
}

// ⭐ A REPLY BODY RESTYLES THE REPLIES IT NAMES AND LEAVES OTO'S OWN FOR THE REST.
func TestAReplyTemplateRendersOnlyTheReasonsItHandles(t *testing.T) {
	t.Parallel()
	reply := `{% if reason == 'all_resolved' %}{"text":"{{ alert.name }} is over","color":"good","blocks":[
{"type":"context","elements":[{"type":"mrkdwn","text":":white_check_mark: over after {{ group.firing_for }}"}]}]}{% endif %}`
	ref := rawRef(wholeMessage, reply)

	p := renderTemplated(t, resolvedView(), domain.ModeThreadReply, ref)
	if p.Text != "OtoSmokeTest is over" || p.Attachments[0].Color != "good" {
		t.Errorf("the all_resolved reply is not the template's: text %q colour %q", p.Text, p.Attachments[0].Color)
	}
	if b := p.Attachments[0].Blocks; len(b) != 1 || !strings.HasPrefix(b[0].BlockID, "oto_replyraw") {
		t.Errorf("the reply's blocks are not the template's: %+v", b)
	}

	// Any other reason renders nothing from the reply body, so the reply is oto's.
	acked := resolvedView()
	acked.Reason = "acked"
	builtin := renderTemplated(t, acked, domain.ModeThreadReply, nil)
	got := renderTemplated(t, acked, domain.ModeThreadReply, ref)
	if got.Text != builtin.Text {
		t.Errorf("an unhandled reason did not keep oto's own reply: %q, want %q", got.Text, builtin.Text)
	}

	// And a template with no reply body leaves every reply oto's.
	none := renderTemplated(t, resolvedView(), domain.ModeThreadReply, rawRef(wholeMessage, ""))
	own := renderTemplated(t, resolvedView(), domain.ModeThreadReply, nil)
	if none.Text != own.Text {
		t.Errorf("a template with no reply body changed the reply: %q, want %q", none.Text, own.Text)
	}
}
