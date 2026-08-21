package webhookjson_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/render/webhookjson"
	"github.com/thulasiram/oto/internal/platform/clock"
)

func renderEnvelope(t *testing.T, w map[string]string) (webhookjson.Envelope, []byte) {
	t.Helper()
	msg, err := webhookjson.New(clock.New()).Render(context.Background(), baseView(), domain.RenderOptions{
		Mode:         domain.ModePostRoot,
		BaseURL:      "http://localhost:8080",
		MaxInstances: 10,
		Wordings:     w,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var env webhookjson.Envelope
	if err := json.Unmarshal(msg.Payload, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return env, msg.Payload
}

// TestNoWordingsLeavesTheEnvelopeAlone is the freeze holding: a v1 consumer sees
// nothing new until its destination is configured with a Wording.
func TestNoWordingsLeavesTheEnvelopeAlone(t *testing.T) {
	_, raw := renderEnvelope(t, nil)
	if strings.Contains(string(raw), `"rendered"`) {
		t.Errorf("the rendered key appeared on an envelope with no wordings: %s", raw)
	}
}

// TestTheWebhookIsNotADegradedSlack is the cross-channel claim, checked: ONE
// template, two providers, two spellings, one meaning.
func TestTheWebhookIsNotADegradedSlack(t *testing.T) {
	const tmpl = `{{ group.severity | bold }} on {{ alert.service | code }} ` +
		`since {{ group.started_at | datetime }}`
	env, raw := renderEnvelope(t, map[string]string{"body": tmpl})

	body, ok := env.Rendered["body"]
	if !ok {
		t.Fatalf("no rendered body in %s", raw)
	}
	if strings.ContainsAny(body, "*`~_") {
		t.Errorf("a consumer was handed Slack's punctuation: %q", body)
	}
	if strings.Contains(body, "<!date^") {
		t.Errorf("a consumer was handed Slack's date token: %q", body)
	}
	if !strings.Contains(body, "on") || !strings.Contains(body, "since") {
		t.Errorf("the words did not survive marker stripping: %q", body)
	}
	// The structured fields are still there. A Wording ADDS prose; it never
	// replaces the facts a consumer parses.
	if env.Summary == "" {
		t.Error("a wording displaced the frozen summary key")
	}
	if env.Group == nil {
		t.Error("a wording displaced the frozen group key")
	}
}

// TestOnlyWordableStanzasAppear — the refusals hold on this provider too.
func TestOnlyWordableStanzasAppear(t *testing.T) {
	env, _ := renderEnvelope(t, map[string]string{
		"body":    `ok`,
		"fields":  `REPLACED`,
		"members": `REPLACED`,
		"trail":   `REPLACED`,
		"actions": `REPLACED`,
	})
	if _, ok := env.Rendered["body"]; !ok {
		t.Error("the body wording did not render")
	}
	for _, refused := range []string{"fields", "members", "trail", "actions"} {
		if got, ok := env.Rendered[refused]; ok {
			t.Errorf("%s is refused but rendered %q", refused, got)
		}
	}
}

// TestAFailingWordingOmitsItsKey — an absent key is the truthful rendering, and a
// smaller claim than an empty string a consumer would print.
func TestAFailingWordingOmitsItsKey(t *testing.T) {
	env, _ := renderEnvelope(t, map[string]string{
		"body":   `{{ alert.name | no_such_filter }}`,
		"footer": `{{ nothing.here | default: "" }}`,
		"title":  `fine {{ org.name | default: "x" }}`,
	})
	if _, ok := env.Rendered["body"]; ok {
		t.Error("a wording with an unknown filter emitted a key")
	}
	if _, ok := env.Rendered["footer"]; ok {
		t.Error("a wording that rendered empty emitted a key")
	}
	if env.Rendered["title"] == "" {
		t.Error("the working wording should still have rendered")
	}
}

// TestAWordingCannotPingAWebhookConsumersOnwardChannel. A consumer commonly
// forwards oto's text into a chat product; a Wording must not be usable as a
// laundering step for a ping it was not allowed to send.
func TestAWordingCannotPingAWebhookConsumersOnwardChannel(t *testing.T) {
	env, _ := renderEnvelope(t, map[string]string{
		"body": `@everyone @channel @here @room deploy now`,
	})
	body := env.Rendered["body"]
	for _, tok := range []string{"@everyone", "@channel", "@here", "@room"} {
		if strings.Contains(strings.ToLower(body), tok) {
			t.Errorf("%q survived into a webhook payload: %q", tok, body)
		}
	}
}
