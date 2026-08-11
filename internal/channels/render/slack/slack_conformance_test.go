package slack_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/channels/domain"
	slackrender "github.com/thulasiram/oto/internal/channels/render/slack"
)

// ⭐⭐ THE DIVERGENCES BETWEEN OTO'S BELIEF ABOUT BLOCK KIT AND SLACK'S PUBLISHED
// REFERENCE, PINNED ONE BY ONE.
//
// git-bug edb670f: the card has never been rendered by Slack, so V0–V18 are
// oto's own belief checked against itself. Reading Slack's reference line by line
// against those eighteen rules turned up the cases below. Each one is a place
// where the loop was closed on a wrong number or an absent rule, and each would
// have surfaced as `invalid_blocks` — a dead delivery — the first time anything
// produced the shape.
//
// ⛔ THESE ARE NOT STYLE TESTS. An oto card that Slack refuses is not a card that
// looks worse; it is an alert nobody receives, and oto's silence must never be
// indistinguishable from "there was no alert".

func mustValidate(t *testing.T, payload string) error {
	t.Helper()
	return slackrender.Validate(json.RawMessage(payload))
}

// checkName is the §L.6 check that refused the payload. It is the label on
// `oto_render_invalid_total{check}`, so naming it in an assertion is naming the
// counter an operator would see.
func checkName(err error) string {
	var e *slackrender.Error
	if err == nil {
		return ""
	}
	if !errors.As(err, &e) {
		return "?"
	}
	return e.Check
}

// envelope wraps a block list in the smallest payload that reaches
// validateBlock, so each case is about one rule and nothing else.
func envelope(blocks string) string {
	return `{"text":"a complete sentence.","unfurl_links":false,"unfurl_media":false,` +
		`"attachments":[{"color":"#a30200","fallback":"f","blocks":[` + blocks + `]}]}`
}

// ⛔ DIVERGENCE 1 — AN OPTION'S `value` IS 150 CHARACTERS, NOT A BUTTON'S 2000.
//
// Slack's option-object reference: "maximum length for this field is 150
// characters". oto checked overflow option values against `maxButtonValue`, which
// is 2000 — thirteen times too permissive. Nothing oto renders today is near
// either number (the one option that carries a value is `labels|<uuid>`, 43
// characters), which is exactly why it survived: the rule was wrong in a place
// the renderer never visited, so it would have failed for the first caller who
// did.
func TestAnOverflowOptionValueIsBoundedByTheOptionLimitAndNotTheButtonLimit(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("v", 151)
	err := mustValidate(t, envelope(`{"type":"actions","block_id":"b1","elements":[
		{"type":"overflow","action_id":"oto.more","options":[
			{"text":{"type":"plain_text","text":"Show all labels"},"value":"`+long+`"}]}]}`))
	if err == nil {
		t.Fatal("a 151-character overflow option value was accepted; Slack's limit is 150 " +
			"and the whole card would come back invalid_blocks")
	}
	if got := checkName(err); got != "V11" {
		t.Fatalf("check = %q, want V11", got)
	}

	// 150 exactly is legal, so the bound is the documented one and not an
	// off-by-one in the other direction.
	ok := strings.Repeat("v", 150)
	if err := mustValidate(t, envelope(`{"type":"actions","block_id":"b1","elements":[
		{"type":"overflow","action_id":"oto.more","options":[
			{"text":{"type":"plain_text","text":"Show all labels"},"value":"`+ok+`"}]}]}`)); err != nil {
		t.Fatalf("a 150-character option value is legal and was refused: %v", err)
	}
}

// ⛔ DIVERGENCE 2 — SLACK ALLOWS FIVE OVERFLOW OPTIONS AND V0–V18 COUNTED NONE.
//
// "An array of up to five option objects to display in the menu." The only thing
// stopping a sixth was `overflowMenu` declining to add one — a renderer-side
// convention, not a check. A convention is not a rule: it holds until the
// renderer changes, and the point of a validator is to hold when it does.
func TestAnOverflowMenuMayNotCarryMoreThanFiveOptions(t *testing.T) {
	t.Parallel()

	opts := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		opts = append(opts, `{"text":{"type":"plain_text","text":"opt"},"value":"v"}`)
	}
	err := mustValidate(t, envelope(`{"type":"actions","block_id":"b1","elements":[
		{"type":"overflow","action_id":"oto.more","options":[`+strings.Join(opts, ",")+`]}]}`))
	if err == nil {
		t.Fatal("a six-option overflow was accepted; Slack documents a maximum of five")
	}
	if got := checkName(err); got != "V9" {
		t.Fatalf("check = %q, want V9", got)
	}

	// An overflow with no options at all is not a menu.
	if err := mustValidate(t, envelope(`{"type":"actions","block_id":"b1","elements":[
		{"type":"overflow","action_id":"oto.more","options":[]}]}`)); err == nil {
		t.Fatal("an overflow with no options was accepted")
	}
}

// ⛔ DIVERGENCE 3 — AN OPTION'S LABEL IS `plain_text`, ALWAYS.
//
// V9 enforced that for a button and not for an overflow option. mrkdwn in an
// option renders as its own source text, so `*bold*` appears with the asterisks.
func TestAnOverflowOptionLabelMustBePlainText(t *testing.T) {
	t.Parallel()

	err := mustValidate(t, envelope(`{"type":"actions","block_id":"b1","elements":[
		{"type":"overflow","action_id":"oto.more","options":[
			{"text":{"type":"mrkdwn","text":"*Show timeline*"},"value":"v"}]}]}`))
	if err == nil {
		t.Fatal("an mrkdwn overflow option label was accepted")
	}
	if got := checkName(err); got != "V9" {
		t.Fatalf("check = %q, want V9", got)
	}
}

// ⛔ DIVERGENCE 4 — A TEXT OBJECT'S MINIMUM LENGTH IS ONE, NOT ZERO.
//
// "The minimum length is 1 and maximum length is 3000 characters." V5 caught a
// section with no text object at all and not a section with an EMPTY one, so
// `{"type":"mrkdwn","text":""}` passed all eighteen rules and Slack would have
// refused it.
func TestAnEmptyTextObjectIsRefusedRatherThanShippedAsAnEmptyBlock(t *testing.T) {
	t.Parallel()

	if err := mustValidate(t, envelope(
		`{"type":"section","block_id":"b1","text":{"type":"mrkdwn","text":""}}`)); err == nil {
		t.Error("an empty section text object was accepted")
	}
	if err := mustValidate(t, envelope(
		`{"type":"section","block_id":"b1","fields":[{"type":"mrkdwn","text":"   "}]}`)); err == nil {
		t.Error("an empty section field was accepted")
	}
	if err := mustValidate(t, envelope(
		`{"type":"context","block_id":"b1","elements":[]}`)); err == nil {
		t.Error("a context block with no elements was accepted")
	}
	if err := mustValidate(t, envelope(
		`{"type":"actions","block_id":"b1","elements":[]}`)); err == nil {
		t.Error("an actions block with no elements was accepted")
	}
}

// ⛔ DIVERGENCE 5 — `alt_text` IS REQUIRED ON AN IMAGE BLOCK.
//
// Slack's image-block reference lists it as required. V4 whitelists `image` for
// Grafana panel renders and V10 checked only the URL — and `checkURL` returns nil
// for an empty string, so an image block with NEITHER a source nor a description
// passed. oto emits no image blocks today, which is precisely why this went
// unnoticed: the first person to render a panel would have met a dead delivery.
func TestAnImageBlockNeedsBothASourceAndADescription(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"no alt_text":  `{"type":"image","block_id":"b1","image_url":"https://e.example/p.png"}`,
		"no image_url": `{"type":"image","block_id":"b1","alt_text":"CPU"}`,
		"neither":      `{"type":"image","block_id":"b1"}`,
	}
	for name, block := range cases {
		if err := mustValidate(t, envelope(block)); err == nil {
			t.Errorf("%s: an illegal image block was accepted", name)
		} else if got := checkName(err); got != "V10" {
			t.Errorf("%s: check = %q, want V10", name, got)
		}
	}

	if err := mustValidate(t, envelope(
		`{"type":"image","block_id":"b1","image_url":"https://e.example/p.png","alt_text":"CPU"}`)); err != nil {
		t.Fatalf("a legal image block was refused: %v", err)
	}
}

// ---------------------------------------------------------------- injection

// ⛔⛔ THE ONE THAT IS A SECURITY BUG AND NOT A LIMIT.
//
// `Links.Runbook` is the `runbook_url` annotation copied verbatim out of the
// alert. It reached the top-level `text` UNESCAPED — every other upstream string
// on the card goes through `escape` or `code`, and a URL cannot, because `<`, `>`
// and `&` are all legal in one.
//
// The top-level text is what a locked phone shows and what a screen reader
// announces. `runbook_url: "<!channel>"` put a channel-wide ping there, on every
// alert, for anybody who can write a Prometheus rule annotation.
func TestAnUpstreamRunbookAnnotationCannotInjectIntoTheTopLevelText(t *testing.T) {
	t.Parallel()

	for _, hostile := range []string{
		"<!channel>",
		"<!here>",
		"<https://evil.example.com|Click here to fix it>",
		"javascript:alert(1)",
	} {
		v := conformanceView()
		v.Links.Runbook = hostile
		msg := renderView(t, v, domain.ModePostRoot)
		text := topLevelText(t, msg.Payload)

		if strings.Contains(text, hostile) {
			t.Errorf("a hostile runbook_url reached the push notification verbatim: %q", text)
		}
		if strings.Contains(text, "<!channel>") || strings.Contains(text, "<!here>") {
			t.Errorf("an upstream annotation produced a broadcast mention: %q", text)
		}
	}
}

// ⛔⛔ AND THE ONE THAT IS AN AVAILABILITY BUG.
//
// The same unvalidated annotation becomes a button `url`, where V10 refuses
// anything that is not absolute http(s) — which fails the render, which kills the
// WHOLE delivery as a config_invalid dead letter. So `runbook_url: "see the
// wiki"`, a plausible thing for a human to write, silently stopped oto
// delivering that alert at all.
//
// Losing one button is a degraded card. Losing the delivery is a lost alert.
func TestAnUnusableUpstreamURLCostsOneButtonAndNotTheWholeCard(t *testing.T) {
	t.Parallel()

	v := conformanceView()
	v.Links.Runbook = "see the wiki"
	v.Links.Prometheus = "not-a-url"
	v.Actions = []domain.Action{
		{ID: "oto.ack", Label: "Acknowledge", Style: "primary",
			Value: "019fe297-d84f-7599-b5b2-1f231749104a"},
		{ID: "oto.noop.runbook", Label: "Runbook", URL: "see the wiki"},
	}

	msg, err := slackrender.New(nil).Render(t.Context(), v, domain.RenderOptions{
		Mode: domain.ModePostRoot, Verbosity: domain.VerbosityAll,
		BaseURL: "https://oto.example.com", MaxInstances: 10, ShowFieldEmoji: true,
	})
	if err != nil {
		t.Fatalf("an unusable upstream URL killed the whole delivery: %v\n%s", err, msg.Payload)
	}

	body := string(msg.Payload)
	if strings.Contains(body, "see the wiki") || strings.Contains(body, "not-a-url") {
		t.Errorf("an unusable URL was emitted anyway: %s", body)
	}
	// The card still says everything it was going to say.
	if !strings.Contains(body, "oto.ack") {
		t.Errorf("the Acknowledge button was lost along with the bad link: %s", body)
	}
}

// conformanceView is a minimal, complete view. It is deliberately NOT
// `smokeView` — that fixture pins the observed defects of a specific run and
// changing it to suit a new test would erase the thing it exists to remember.
func conformanceView() *domain.NotificationView {
	v := smokeView()
	return v
}
