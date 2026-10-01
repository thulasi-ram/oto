package template_test

import (
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/channels/template"
)

// ⭐ JSON CLOSES OBJECTS WITH `}}`, AND A RAW TEMPLATE MAY WRITE IT THAT WAY. Only
// the stray `}}` is excused: every other delimiter mistake still prints template
// syntax on a card and is still refused, in raw as everywhere else.
func TestARawTemplateMayCloseTwoObjectsAtOnce(t *testing.T) {
	t.Parallel()
	tight := `[{"type":"section","text":{"type":"mrkdwn","text":"{{ alert.name }}"}}]`
	if ps := template.Validate(template.FormatRaw, tight); template.Blocking(ps) {
		t.Fatalf("a raw template written as JSON is written was refused: %v", ps)
	}
	if ps := template.Validate(template.FormatCard, "# {{ alert.name }} }}"); !template.Blocking(ps) {
		t.Error("a stray `}}` in a card template is a mistake and must still be refused")
	}
	for _, src := range []string{
		`[{"text":"{{ alert.name"}]`,
		`[{"text":"{% if alert.name %}x"}] %}`,
	} {
		if ps := template.Validate(template.FormatRaw, src); !template.Blocking(ps) {
			t.Errorf("%q is malformed Liquid and was accepted", src)
		}
	}
}

// ⭐ A REPLY BODY THAT HANDLES ONE REASON IS A VALID REPLY BODY.
func TestAReplyBodyMayRenderNothingForReasonsItLeavesToOto(t *testing.T) {
	t.Parallel()
	src := `{% if reason == 'all_resolved' %}{"text":"over","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"{{ alert.name }}"}}]}{% endif %}`
	if ps := template.ValidateReply(template.FormatRaw, src); len(ps) > 0 {
		t.Fatalf("a reply body for one reason was refused: %v", ps)
	}
	// The same body is NOT a valid root card: the root has no oto fallback per
	// reason, so rendering nothing on an ordinary card is still the author's bug.
	if ps := template.Validate(template.FormatRaw, src); !template.Blocking(ps) {
		t.Error("a root body that renders nothing for the firing card was accepted")
	}
	// A reply body is still rendered, so a real mistake inside the branch it does
	// take is still caught.
	broken := `{% if reason == 'all_resolved' %}{"text": {{ alert.name }} }{% endif %}`
	if ps := template.ValidateReply(template.FormatRaw, broken); !template.Blocking(ps) {
		t.Error("a reply body whose all_resolved branch is not JSON was accepted")
	}
	// Every reply reason is exercised, not only the two ordinary cards, so a
	// mistake hiding in a branch no root fixture takes is still caught at save.
	ackedOnly := `{% if reason == 'acked' %}{"text": {{ alert.name }} }{% endif %}`
	if ps := template.ValidateReply(template.FormatRaw, ackedOnly); !template.Blocking(ps) {
		t.Error("a reply body whose acked branch is not JSON was accepted")
	}
	// And the reply body is never told to add an action row.
	for _, p := range template.ValidateReply(template.FormatCard, "{{ alert.name }} is over") {
		if strings.Contains(p.Message, "actions") {
			t.Errorf("a reply body was warned about buttons: %q", p.Message)
		}
	}
}
