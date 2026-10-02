package template_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/template"
)

// ADR 0055 §2's payload mapping, at the engine (git-bug 2205620). The gate and the
// dispatcher are tested where they live; this file holds the two properties
// everything else rests on — an interpolated value can never become structure, and a
// secret is a reference until the provider fills it.

func compileMapping(t *testing.T, doc domain.PayloadMapping) *template.Mapping {
	t.Helper()
	m, probs := template.CompileMapping(doc)
	if len(probs) > 0 {
		t.Fatalf("CompileMapping: %+v", probs)
	}
	return m
}

func envelopeWith(t *testing.T, alertname string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"schema": "oto.notification.v1",
		"reason": "fired",
		"alerts": []any{map[string]any{"labels": map[string]any{"alertname": alertname}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAHostileLabelLandsAsAJSONStringAndNeverAsStructure is the escaping promise:
// a label holding `"`, `\`, a newline and `</script>` arrives at the vendor as the
// exact string the alert carried — not broken JSON, not an injected key, and not
// Slack's `&lt;` either, because the mapping escapes for JSON ALONE.
func TestAHostileLabelLandsAsAJSONStringAndNeverAsStructure(t *testing.T) {
	t.Parallel()
	hostile := "quote\" backslash\\ newline\n </script> <!channel> \",\"injected\":\"yes"
	m := compileMapping(t, domain.PayloadMapping{
		Body: `{"title": "{{ alerts[0].labels.alertname }}", "status": "firing"}`,
	})

	out, err := m.Render(envelopeWith(t, hostile))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(out.Body), &body); err != nil {
		t.Fatalf("a hostile label broke the JSON: %v\n%s", err, out.Body)
	}
	if body["title"] != hostile {
		t.Fatalf("title = %q, want the label verbatim %q", body["title"], hostile)
	}
	if _, injected := body["injected"]; injected || len(body) != 2 {
		t.Fatalf("a label became structure: %v", body)
	}
	if strings.Contains(out.Body, "&lt;") {
		t.Fatalf("the mapping escaped for Slack; a vendor reads it as written: %s", out.Body)
	}
}

// TestALabelCannotForgeASecretReference: the sentinel a `secrets.<name>` renders to
// cannot be produced by a value. A label spelling it out must reach the vendor as
// text, and FillSecrets must have nothing to fill in it.
func TestALabelCannotForgeASecretReference(t *testing.T) {
	t.Parallel()
	forged := "\u2028routing_key\u2029"
	m := compileMapping(t, domain.PayloadMapping{
		Body: `{"title": "{{ alerts[0].labels.alertname }}", "key": "{{ secrets.routing_key }}"}`,
	})
	out, err := m.Render(envelopeWith(t, forged))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if refs := template.SecretRefs(out.Body); len(refs) != 1 || refs[0] != "routing_key" {
		t.Fatalf("SecretRefs = %v, want exactly the mapping's own reference", refs)
	}
	filled, _, err := template.FillSecrets([]byte(out.Body), nil, map[string]string{"routing_key": "SEKRET"})
	if err != nil {
		t.Fatalf("FillSecrets: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(filled, &body); err != nil {
		t.Fatalf("filled body is not JSON: %v\n%s", err, filled)
	}
	if body["key"] != "SEKRET" {
		t.Fatalf("key = %q, want the sealed value", body["key"])
	}
	if strings.Contains(body["title"], "SEKRET") {
		t.Fatalf("⛔ a label forged a secret reference and the secret was sent in it: %q", body["title"])
	}
}

// TestASecretIsAReferenceUntilItIsFilled: the rendered body — what the delivery row
// stores — names the secret and never holds it; FillSecrets swaps it in as JSON
// string content, so a secret with a quote in it cannot break the body either.
func TestASecretIsAReferenceUntilItIsFilled(t *testing.T) {
	t.Parallel()
	const secret = `r0ut"ing\key`
	m := compileMapping(t, domain.PayloadMapping{
		Body:    `{"routing_key": "{{ secrets.routing_key }}"}`,
		Headers: map[string]string{"X-Vendor-Key": "{{ secrets.routing_key }}"},
	})
	if got := m.Secrets(); len(got) != 1 || got[0] != "routing_key" {
		t.Fatalf("Secrets = %v", got)
	}
	out, err := m.Render(envelopeWith(t, "a"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out.Body, "r0ut") || strings.Contains(out.Headers["X-Vendor-Key"], "r0ut") {
		t.Fatalf("⛔ the rendered request holds the secret before sending: %s", out.Body)
	}
	body, headers, err := template.FillSecrets([]byte(out.Body), out.Headers, map[string]string{"routing_key": secret})
	if err != nil {
		t.Fatalf("FillSecrets: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil || got["routing_key"] != secret {
		t.Fatalf("filled body = %s (%v), want routing_key %q", body, err, secret)
	}
	if headers["X-Vendor-Key"] != secret {
		t.Fatalf("filled header = %q, want the secret verbatim", headers["X-Vendor-Key"])
	}

	if _, _, err := template.FillSecrets([]byte(out.Body), out.Headers, nil); err == nil {
		t.Fatal("a reference to a secret the connection does not hold was filled with nothing")
	}
}

// TestASecretMustBeReadPlainly: `secrets` alone, a bracket read, or a filter that
// bends the reference out of shape are all refused — the first two at compile, the
// last at render, which the save gate runs for every fact.
func TestASecretMustBeReadPlainly(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"k": "{{ secrets }}"}`,
		`{"k": "{{ secrets["routing_key"] }}"}`,
		`{"k": "{{ secrets.RoutingKey }}"}`,
		"{\"k\": \"\u2028\"}",
	} {
		if _, probs := template.CompileMapping(domain.PayloadMapping{Body: body}); len(probs) == 0 {
			t.Errorf("%s compiled", body)
		}
	}
	m := compileMapping(t, domain.PayloadMapping{Body: `{"k": "{{ secrets.routing_key | upper }}"}`})
	if _, err := m.Render(envelopeWith(t, "a")); err == nil {
		t.Fatal("a secret reference bent by a filter rendered")
	}
	// A JSON key the operator spells "secrets" in literal text is their JSON.
	compileMapping(t, domain.PayloadMapping{Body: `{"secrets": "none"}`})
}

// TestAFactBodyOverridesTheDefault, and the default answers every other fact.
func TestAFactBodyOverridesTheDefault(t *testing.T) {
	t.Parallel()
	m := compileMapping(t, domain.PayloadMapping{
		Body:  `{"event": "{{ reason }}"}`,
		Facts: map[string]string{"quiet": `{"event": "went quiet"}`},
	})
	for reason, want := range map[string]string{"quiet": `{"event": "went quiet"}`, "drawn": `{"event": "drawn"}`} {
		out, err := m.Render(json.RawMessage(`{"schema":"oto.notification.v1","reason":"` + reason + `"}`))
		if err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		if out.Body != want {
			t.Errorf("%s rendered %s, want %s", reason, out.Body, want)
		}
	}
	if _, probs := template.CompileMapping(domain.PayloadMapping{
		Body: `{}`, Facts: map[string]string{"resolved": `{}`},
	}); len(probs) == 0 {
		t.Fatal("a body for a fact no envelope carries compiled")
	}
}

// TestABodyThatIsNotOneJSONObjectIsRefused, with the attempt kept for the dead row.
func TestABodyThatIsNotOneJSONObjectIsRefused(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{"title": {{ summary }}`, `["a"]`, `"text"`, `{"a":1} {"b":2}`} {
		m := compileMapping(t, domain.PayloadMapping{Body: body})
		out, err := m.Render(json.RawMessage(`{"summary":"x"}`))
		if err == nil {
			t.Errorf("%s rendered as a body", body)
		}
		if out.Body == "" {
			t.Errorf("%s: the refused attempt was not kept", body)
		}
	}
}

// TestAHostileAnnotationKeyCannotBecomeStructureOrASecret: a map renders its KEYS
// into the body too, and an annotation's name is as writable as its value. One key
// tries to close the string and add a command field; the other spells a secret
// reference out of the sentinel separators. Both must land as text.
func TestAHostileAnnotationKeyCannotBecomeStructureOrASecret(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(map[string]any{
		"schema": "oto.notification.v1",
		"reason": "fired",
		"alert": map[string]any{"annotations": map[string]any{
			"\",\"event_action\":\"resolve\",\"x\":\"": "v",
			" routing_key ":                            "v",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := compileMapping(t, domain.PayloadMapping{Body: `{"a":"{{ alert.annotations }}"}`})
	out, err := m.Render(raw)
	if err != nil {
		t.Fatalf("Render: %v\n%s", err, out.Body)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(out.Body), &body); err != nil {
		t.Fatalf("a hostile annotation key broke the JSON: %v\n%s", err, out.Body)
	}
	if len(body) != 1 {
		t.Fatalf("an annotation key became structure: %v", body)
	}
	if strings.ContainsRune(out.Body, ' ') || strings.ContainsRune(out.Body, ' ') {
		t.Fatalf("⛔ an annotation key put a sentinel into the body: %q", out.Body)
	}
	if refs := template.SecretRefs(out.Body); len(refs) != 0 {
		t.Fatalf("⛔ an annotation key forged a secret reference: %v", refs)
	}
}

// TestATextFilterCutsTheTextAndNotItsEscape: a bound value is JSON string content,
// so `truncate_runes` over its escaped form could stop between a backslash and what
// it escapes and leave the body broken. The filter cuts the TEXT and re-escapes it.
func TestATextFilterCutsTheTextAndNotItsEscape(t *testing.T) {
	t.Parallel()
	m := compileMapping(t, domain.PayloadMapping{
		Body: `{"title": "{{ alerts[0].labels.alertname | truncate_runes: 3 }}"}`,
	})
	for _, c := range []struct{ label, want string }{
		{`ab"cd`, `ab"…`},
		{`ab\cd`, `ab\…`},
		{"ab\ncd", "ab\n…"},
		// A control character is dropped at binding, so it is not counted.
		{"a\x07bcd", "abc…"},
	} {
		out, err := m.Render(envelopeWith(t, c.label))
		if err != nil {
			t.Errorf("%q: Render: %v\n%s", c.label, err, out.Body)
			continue
		}
		var body map[string]string
		if err := json.Unmarshal([]byte(out.Body), &body); err != nil {
			t.Errorf("%q: truncating broke the JSON: %v\n%s", c.label, err, out.Body)
			continue
		}
		if body["title"] != c.want {
			t.Errorf("%q truncated to %q, want %q", c.label, body["title"], c.want)
		}
	}
}

// TestUpperCaseMapsTheTextAndNotItsEscape: `\n` must not become `\N`.
func TestUpperCaseMapsTheTextAndNotItsEscape(t *testing.T) {
	t.Parallel()
	m := compileMapping(t, domain.PayloadMapping{
		Body: `{"title": "{{ alerts[0].labels.alertname | upper }}"}`,
	})
	out, err := m.Render(envelopeWith(t, "a\"b\\c\nd"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(out.Body), &body); err != nil || body["title"] != "A\"B\\C\nD" {
		t.Fatalf("upper rendered %s (%v)", out.Body, err)
	}
}
