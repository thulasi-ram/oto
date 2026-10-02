package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/template"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ADR 0055 §2's save gate and claim-time mapper (git-bug 2205620). "A mapping that
// does not render cannot be saved" is tested for EVERY fact, one at a time, because
// a mapping cannot decline a fact and the 422 has to name the one it broke on.

// mappingDoc builds a mapping document from a body and optional parts.
func mappingDoc(t *testing.T, m domain.PayloadMapping) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// incidentIOBody is the shape incident.io's HTTP alert source asks for — `title` and
// `status` at the top level — written over the envelope, a fact at a time.
const incidentIOBody = `{
  "title": "{{ summary }}",
  "status": "{% if reason == 'quiet' %}resolved{% endif %}{% unless reason == 'quiet' %}firing{% endunless %}",
  "deduplication_key": "{% if incident %}{{ incident.id }}{% endif %}{% unless incident %}{{ group.id }}{% endunless %}",
  "routing_key": "{{ secrets.routing_key }}",
  "metadata": {"reason": "{{ reason }}", "first_alert": "{{ alerts[0].alert_name }}"}
}`

func violationsOf(t *testing.T, err error) []errs.Violation {
	t.Helper()
	e, ok := errs.As(err)
	if !ok || e.Kind != errs.KindValidation {
		t.Fatalf("err = %v, want a 422", err)
	}
	return e.Violations
}

// TestAMappingThatRendersEveryFactIsSaved is the ordinary case.
func TestAMappingThatRendersEveryFactIsSaved(t *testing.T) {
	t.Parallel()
	raw := mappingDoc(t, domain.PayloadMapping{
		Body:     incidentIOBody,
		Headers:  map[string]string{"X-Source": "oto {{ reason }}"},
		Response: &domain.MappingResponse{ExternalURL: "data.url"},
	})
	if err := ValidateMapping(raw, []string{"routing_key"}); err != nil {
		t.Fatalf("a mapping that renders every fact was refused: %v", err)
	}
}

// TestAMappingThatFailsAnyOneFactCannotBeSavedAndTheRefusalNamesIt walks all twenty
// facts. For each, a mapping that renders broken JSON for THAT fact alone is refused,
// and the violation names the fact.
func TestAMappingThatFailsAnyOneFactCannotBeSavedAndTheRefusalNamesIt(t *testing.T) {
	t.Parallel()
	facts := domain.MappingFacts()
	if len(facts) != 20 {
		t.Fatalf("there are %d facts, want the 20 an envelope carries", len(facts))
	}
	for _, fact := range facts {
		t.Run(fact, func(t *testing.T) {
			t.Parallel()
			// The custom `if` has no `else` (template/tags.go), so `unless` is the other half.
			is := `reason == '` + fact + `'`
			body := `{% if ` + is + ` %}{"broken": {% endif %}{% unless ` + is + ` %}{"ok": true}{% endunless %}`
			err := ValidateMapping(mappingDoc(t, domain.PayloadMapping{Body: body}), nil)
			if err == nil {
				t.Fatalf("a mapping that cannot render %s was saved", fact)
			}
			named := false
			for _, v := range violationsOf(t, err) {
				if !strings.HasPrefix(v.Message, fact+" ") {
					t.Errorf("a violation names another fact: %+v", v)
					continue
				}
				named = named || v.Field == "payload_mapping/body"
			}
			if !named {
				t.Fatalf("no violation names %s: %+v", fact, violationsOf(t, err))
			}
		})
	}
}

// TestAFactBodyThatFailsIsPlacedOnThatFact: the field points at the source.
func TestAFactBodyThatFailsIsPlacedOnThatFact(t *testing.T) {
	t.Parallel()
	err := ValidateMapping(mappingDoc(t, domain.PayloadMapping{
		Body: `{"ok": true}`, Facts: map[string]string{"drawn": `{"title": {{ summary }}}`},
	}), nil)
	vs := violationsOf(t, err)
	if len(vs) == 0 || vs[0].Field != "payload_mapping/facts/drawn" || !strings.HasPrefix(vs[0].Message, "drawn") {
		t.Fatalf("violations = %+v, want one on payload_mapping/facts/drawn naming drawn", vs)
	}
}

// TestAHostileLabelDoesNotBreakASavedMapping: the corpus carries a label holding `"`,
// `\`, a newline and `</script>`; a mapping that interpolates labels is still saved,
// because every value is JSON-escaped.
func TestAHostileLabelDoesNotBreakASavedMapping(t *testing.T) {
	t.Parallel()
	body := `{"labels": "{% for a in alerts %}{{ a.labels.alertname }} {% endfor %}",` +
		`"members": "{% for m in incident.members %}{{ m.alert_name }} {{ m.labels.service }} {% endfor %}",` +
		`"by": "{{ incident.drawn_by.label }}", "summary": "{{ summary }}"}`
	if err := ValidateMapping(mappingDoc(t, domain.PayloadMapping{Body: body}), nil); err != nil {
		t.Fatalf("hostile text broke a mapping that only interpolates: %v", err)
	}
}

// TestAMappingReferencingAMissingSecretCannotBeSaved, and the secret it names is
// enough to save it.
func TestAMappingReferencingAMissingSecretCannotBeSaved(t *testing.T) {
	t.Parallel()
	raw := mappingDoc(t, domain.PayloadMapping{Body: `{"routing_key": "{{ secrets.routing_key }}"}`})
	err := ValidateMapping(raw, []string{"api_key"})
	vs := violationsOf(t, err)
	if len(vs) != 1 || vs[0].Code != "missing_secret" || !strings.Contains(vs[0].Message, "routing_key") {
		t.Fatalf("violations = %+v, want one missing_secret naming routing_key", vs)
	}
	if err := ValidateMapping(raw, []string{"routing_key"}); err != nil {
		t.Fatalf("with the secret held: %v", err)
	}
}

// TestAMappingMayNotCarryACredentialHeaderOrOtosFraming: a vendor's token is the
// Connection's sealed credential, and `X-Oto-*` is oto's.
func TestAMappingMayNotCarryACredentialHeaderOrOtosFraming(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"Authorization", "X-Oto-Signature"} {
		err := ValidateMapping(mappingDoc(t, domain.PayloadMapping{
			Body: `{}`, Headers: map[string]string{name: "x"},
		}), nil)
		vs := violationsOf(t, err)
		if len(vs) == 0 || vs[0].Field != "payload_mapping/headers/"+name {
			t.Errorf("%s: violations = %+v", name, vs)
		}
	}
}

// TestAHeaderNamedLikeACredentialMustReadASecret: `X-Api-Key: abc123` written into a
// mapping is a credential stored in the clear; it must be `{{ secrets.<name> }}`.
func TestAHeaderNamedLikeACredentialMustReadASecret(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"X-Api-Key", "X-Routing-Token", "X-Client-Secret", "X-Password"} {
		err := ValidateMapping(mappingDoc(t, domain.PayloadMapping{
			Body: `{}`, Headers: map[string]string{name: "abc123"},
		}), nil)
		vs := violationsOf(t, err)
		if len(vs) == 0 || vs[0].Field != "payload_mapping/headers/"+name || vs[0].Code != "secret_required" {
			t.Errorf("%s with a literal value: violations = %+v", name, vs)
		}
	}
	if err := ValidateMapping(mappingDoc(t, domain.PayloadMapping{
		Body: `{}`, Headers: map[string]string{"X-Api-Key": "{{ secrets.api_key }}"},
	}), []string{"api_key"}); err != nil {
		t.Fatalf("a credential header reading a secret was refused: %v", err)
	}
}

// TestACatalogChoiceNobodyMadeCannotBeSaved: a catalog copy stored without its
// choice answered would send `<<choose:default_severity>>` to the tool on every
// delivery; the save refuses it, and the copy with the pick written in is saved.
func TestACatalogChoiceNobodyMadeCannotBeSaved(t *testing.T) {
	t.Parallel()
	err := ValidateMapping(mappingDoc(t, domain.PayloadMapping{
		Body: `{"severity": "<<choose:default_severity>>"}`,
	}), nil)
	vs := violationsOf(t, err)
	if len(vs) != 1 || vs[0].Field != "payload_mapping/body" || vs[0].Code != "choice_unfilled" {
		t.Fatalf("violations = %+v, want one choice_unfilled on payload_mapping/body", vs)
	}
	if err := ValidateMapping(mappingDoc(t, domain.PayloadMapping{
		Body: `{"severity": "warning"}`,
	}), nil); err != nil {
		t.Fatalf("the filled copy was refused: %v", err)
	}
}

// TestAMappingDocumentIsReadStrictly: a misspelt key is refused, not ignored.
func TestAMappingDocumentIsReadStrictly(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"body": "{}", "fatcs": {}}`, `{"facts": {}}`, `[]`} {
		if err := ValidateMapping(json.RawMessage(raw), nil); err == nil {
			t.Errorf("%s was saved", raw)
		}
	}
}

// TestEveryFactHasASampleEnvelope: the corpus the gate renders against covers every
// fact, and each builds a real envelope.
func TestEveryFactHasASampleEnvelope(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, fx := range template.MappingFixtures() {
		env, err := sampleEnvelope(fx)
		if err != nil {
			t.Fatalf("%s: %v", fx.Name, err)
		}
		var probe struct {
			Schema string `json:"schema"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(env, &probe); err != nil || probe.Schema != "oto.notification.v1" {
			t.Fatalf("%s: not an envelope: %s", fx.Name, env)
		}
		seen[probe.Reason] = true
	}
	for _, fact := range domain.MappingFacts() {
		if !seen[fact] {
			t.Errorf("no sample envelope carries %s", fact)
		}
	}
}

// TestTheMapperFailsConfigInvalidAndKeepsTheAttempt is the claim-time half: a body
// that is not JSON is a `config_invalid` failure naming the fact, the attempt rides
// back for the dead row (as a JSON string, since it is not JSON), and a good body
// comes back mapped, hashed afresh and still naming — not holding — its secret.
func TestTheMapperFailsConfigInvalidAndKeepsTheAttempt(t *testing.T) {
	t.Parallel()
	env := domain.RenderedMessage{
		Payload:  json.RawMessage(`{"schema":"oto.notification.v1","reason":"drawn","summary":"s"}`),
		Fallback: "s", Hash: "envelope-hash",
	}

	bad := mappingDoc(t, domain.PayloadMapping{Body: `{"title": {{ summary }}}`})
	attempt, err := NewMapper().Map(context.Background(), bad, env)
	var pe *domain.Error
	if !errors.As(err, &pe) || pe.Class != domain.ClassConfigInvalid {
		t.Fatalf("err = %v, want config_invalid", err)
	}
	if !strings.Contains(err.Error(), "drawn") || !strings.Contains(err.Error(), "never") &&
		!strings.Contains(err.Error(), "not sent") {
		t.Fatalf("the failure does not explain itself: %v", err)
	}
	var asString string
	if json.Unmarshal(attempt.Payload, &asString) != nil || !strings.Contains(asString, `"title": s`) {
		t.Fatalf("attempt = %s, want the refused text kept as a JSON string", attempt.Payload)
	}

	good := mappingDoc(t, domain.PayloadMapping{
		Body: `{"title": "{{ summary }}", "key": "{{ secrets.k }}"}`, Headers: map[string]string{"X-A": "b"},
	})
	msg, err := NewMapper().Map(context.Background(), good, env)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !msg.Mapped || msg.Hash == env.Hash || msg.Headers["X-A"] != "b" || msg.Fallback != "s" {
		t.Fatalf("mapped message = %+v", msg)
	}
	if refs := template.SecretRefs(string(msg.Payload)); len(refs) != 1 || refs[0] != "k" {
		t.Fatalf("the mapped body does not name its secret: %s", msg.Payload)
	}
}
