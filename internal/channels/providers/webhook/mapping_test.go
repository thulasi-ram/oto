package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/template"
	"github.com/thulasiram/oto/internal/platform/clock"
)

// ADR 0055 §2 at the socket (git-bug 2205620): a mapped Connection sends the mapped
// body and headers, with its secrets filled and the whole signed; it never sends an
// unmapped body; and an unmapped Connection sends the envelope byte for byte, as
// before. 506ff21's mapping-path override is here too, because the provider is what
// reads the response.

const testEnvelope = `{"schema":"oto.notification.v1","reason":"drawn","summary":"Incident #4 drawn",` +
	`"incident":{"id":"0199a1b2","number":4}}`

// mapForTest renders an envelope through a mapping the way channels/service.Mapper
// does at claim time, without importing it (it imports this package).
func mapForTest(t *testing.T, mapping json.RawMessage) domain.RenderedMessage {
	t.Helper()
	m, err := template.CompiledMapping(mapping)
	if err != nil {
		t.Fatalf("CompiledMapping: %v", err)
	}
	out, err := m.Render(json.RawMessage(testEnvelope))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return domain.RenderedMessage{
		Payload: json.RawMessage(out.Body), Fallback: "Incident #4 drawn", Hash: "h",
		Mapped: true, Headers: out.Headers,
	}
}

func openMapped(t *testing.T, url string, mapping json.RawMessage, cred domain.Credential) domain.Channel {
	t.Helper()
	p := NewProvider(Options{Clock: clock.New(), AllowPrivateTargets: true})
	ch, err := p.Open(context.Background(), domain.ChannelConfig{
		Raw: rawConfig(t, url), PayloadMapping: mapping,
	}, cred)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	return ch
}

const incidentMapping = `{
  "body": "{\"routing_key\": \"{{ secrets.routing_key }}\", \"dedup_key\": \"{{ incident.id }}\", \"summary\": \"{{ summary }}\"}",
  "headers": {"X-Vendor-Event": "oto {{ reason }}", "Authorization": "Bearer nope", "X-Oto-Delivery-Id": "forged"}
}`

// TestAMappedBodyIsSentWithItsSecretFilledAndSigned is the whole send: the vendor
// gets the mapped body with the sealed key in it and the mapped header, and the
// signature verifies over exactly those bytes. The message the delivery row would
// store holds the reference, not the key.
func TestAMappedBodyIsSentWithItsSecretFilledAndSigned(t *testing.T) {
	t.Parallel()
	srv, last := stubReceiver(t)
	const key = `R0UTING"KEY`
	ch := openMapped(t, srv.URL, json.RawMessage(incidentMapping), domain.Credential{
		Kind:    CredNone,
		Signing: domain.SigningSecret{Current: "sign-me"},
		Secrets: map[string]string{"routing_key": key},
	})

	msg := mapForTest(t, json.RawMessage(incidentMapping))
	if strings.Contains(string(msg.Payload), "R0UTING") {
		t.Fatalf("⛔ the stored body holds the secret: %s", msg.Payload)
	}
	id := uuid.New()
	if _, err := ch.Deliver(context.Background(), domain.DeliverRequest{
		Message: msg, Mode: domain.ModePostRoot, DeliveryID: id,
	}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	got := last()
	var body map[string]string
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("the vendor got a body that is not JSON: %v\n%s", err, got.body)
	}
	if body["routing_key"] != key || body["dedup_key"] != "0199a1b2" || body["summary"] != "Incident #4 drawn" {
		t.Fatalf("mapped body = %v", body)
	}
	if v := got.header.Get("X-Vendor-Event"); v != "oto drawn" {
		t.Fatalf("X-Vendor-Event = %q, want the mapped header", v)
	}
	// The two a mapping may never set, whatever is stored: oto's framing wins and
	// no credential travels in a mapping.
	if v := got.header.Get("Authorization"); v != "" {
		t.Fatalf("⛔ a mapping's Authorization header was sent: %q", v)
	}
	if v := got.header.Get("X-Oto-Delivery-Id"); v != id.String() {
		t.Fatalf("X-Oto-Delivery-Id = %q, want oto's own %s", v, id)
	}
	if !verify("sign-me", got.header, got.body, time.Now()) {
		t.Fatal("the signature does not verify over the mapped bytes on the wire")
	}
}

// TestAMappedConnectionNeverSendsAnUnmappedBody: the plain envelope reaching a vendor
// that cannot parse it is the missing incident §2 forbids, so a message that skipped
// the mapping is refused as `config_invalid` and nothing is sent.
func TestAMappedConnectionNeverSendsAnUnmappedBody(t *testing.T) {
	t.Parallel()
	srv, last := stubReceiver(t)
	ch := openMapped(t, srv.URL, json.RawMessage(incidentMapping), domain.Credential{
		Secrets: map[string]string{"routing_key": "k"},
	})
	_, err := ch.Deliver(context.Background(), domain.DeliverRequest{
		Message: domain.RenderedMessage{Payload: json.RawMessage(testEnvelope), Fallback: "a", Hash: "h"},
		Mode:    domain.ModePostRoot, DeliveryID: uuid.New(),
	})
	var pe *domain.Error
	if !errors.As(err, &pe) || pe.Class != domain.ClassConfigInvalid {
		t.Fatalf("an unmapped body on a mapped connection: err = %v, want config_invalid", err)
	}
	if got := last(); got.body != nil {
		t.Fatalf("⛔ the plain envelope was sent: %s", got.body)
	}
}

// TestAMissingSecretFailsTheDeliveryAndSendsNothing: a mapping that names a secret
// the connection no longer holds does not send an empty key.
func TestAMissingSecretFailsTheDeliveryAndSendsNothing(t *testing.T) {
	t.Parallel()
	srv, last := stubReceiver(t)
	ch := openMapped(t, srv.URL, json.RawMessage(incidentMapping), domain.Credential{})
	_, err := ch.Deliver(context.Background(), domain.DeliverRequest{
		Message: mapForTest(t, json.RawMessage(incidentMapping)), Mode: domain.ModePostRoot, DeliveryID: uuid.New(),
	})
	var pe *domain.Error
	if !errors.As(err, &pe) || pe.Class != domain.ClassConfigInvalid {
		t.Fatalf("err = %v, want config_invalid", err)
	}
	if strings.Contains(err.Error(), "R0UTING") {
		t.Fatalf("the error names a secret value: %v", err)
	}
	if got := last(); got.body != nil {
		t.Fatalf("a request went out with an unfilled secret: %s", got.body)
	}
}

// TestAnUnmappedConnectionSendsTheEnvelopeByteForByte: no mapping, no change.
func TestAnUnmappedConnectionSendsTheEnvelopeByteForByte(t *testing.T) {
	t.Parallel()
	srv, last := stubReceiver(t)
	ch := openMapped(t, srv.URL, nil, domain.Credential{})
	msg := domain.RenderedMessage{Payload: json.RawMessage(testEnvelope), Fallback: "a", Hash: "h"}
	if _, err := ch.Deliver(context.Background(), domain.DeliverRequest{
		Message: msg, Mode: domain.ModePostRoot, DeliveryID: uuid.New(),
	}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if got := last(); string(got.body) != testEnvelope {
		t.Fatalf("the envelope changed on the wire:\n got %s\nwant %s", got.body, testEnvelope)
	}
}

// TestAMappingResponsePathIsReadAndOnlyIt is 506ff21's override: a mapping that
// names where the tool's answer carries its incident is read THERE, the default
// top-level keys are not read at all, and what is read passes the same validator —
// a bad value is absent, never a failure, and provider_response keeps no byte of it.
func TestAMappingResponsePathIsReadAndOnlyIt(t *testing.T) {
	t.Parallel()
	const mapping = `{"body": "{\"a\": 1}",
	  "response": {"external_url": "data.incident.html_url", "external_id": "data.incident.id"}}`
	cases := []struct {
		name string
		body string
		want domain.ExternalIncident
	}{
		{"both at the path, and a decoy at the top level",
			`{"external_url":"https://decoy.example/x","external_id":"DECOY",` +
				`"data":{"incident":{"html_url":"https://tool.example/i/7","id":"P7"}}}`,
			domain.ExternalIncident{URL: "https://tool.example/i/7", ID: "P7"}},
		{"only the default keys: not read",
			`{"external_url":"https://tool.example/i/7","external_id":"P7"}`, domain.ExternalIncident{}},
		{"a non-https url at the path",
			`{"data":{"incident":{"html_url":"http://tool.example/i/7","id":"P7"}}}`,
			domain.ExternalIncident{ID: "P7"}},
		{"an oversized id at the path",
			`{"data":{"incident":{"id":"` + strings.Repeat("x", domain.MaxExternalIDLength+1) + `"}}}`,
			domain.ExternalIncident{}},
		{"a number at the path", `{"data":{"incident":{"id":7}}}`, domain.ExternalIncident{}},
		{"not JSON", `<html>ok</html>`, domain.ExternalIncident{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := openMapped(t, echoReceiver(t, http.StatusAccepted, tc.body), json.RawMessage(mapping), domain.Credential{})
			res, err := ch.Deliver(context.Background(), domain.DeliverRequest{
				Message: mapForTest(t, json.RawMessage(mapping)), Mode: domain.ModePostRoot, DeliveryID: uuid.New(),
			})
			if err != nil {
				t.Fatalf("a 202 failed because of what its body said: %v", err)
			}
			if res.External != tc.want {
				t.Fatalf("External = %+v, want %+v", res.External, tc.want)
			}
			for _, leaked := range []string{"tool.example", "P7", "DECOY"} {
				if strings.Contains(string(res.Raw), leaked) {
					t.Fatalf("⛔ provider_response carries receiver bytes (%q): %s", leaked, res.Raw)
				}
			}
		})
	}
}

// TestAMappingWithNoResponsePathKeepsTheDefaultEcho: the override is only an
// override when the mapping names a path.
func TestAMappingWithNoResponsePathKeepsTheDefaultEcho(t *testing.T) {
	t.Parallel()
	const mapping = `{"body": "{\"a\": 1}"}`
	ch := openMapped(t, echoReceiver(t, http.StatusCreated,
		`{"external_url":"https://tool.example/i/1","external_id":"INC-1"}`), json.RawMessage(mapping), domain.Credential{})
	res, err := ch.Deliver(context.Background(), domain.DeliverRequest{
		Message: mapForTest(t, json.RawMessage(mapping)), Mode: domain.ModePostRoot, DeliveryID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.External != (domain.ExternalIncident{URL: "https://tool.example/i/1", ID: "INC-1"}) {
		t.Fatalf("External = %+v, want the default top-level echo", res.External)
	}
}

// TestAStoredMappingThatNoLongerParsesDoesNotOpen: it does not open as an unmapped
// channel, which would send the envelope.
func TestAStoredMappingThatNoLongerParsesDoesNotOpen(t *testing.T) {
	t.Parallel()
	p := NewProvider(Options{Clock: clock.New(), AllowPrivateTargets: true})
	_, err := p.Open(context.Background(), domain.ChannelConfig{
		Raw: rawConfig(t, "http://127.0.0.1:1/hook"), PayloadMapping: json.RawMessage(`{"bodyy": "{}"}`),
	}, domain.Credential{})
	var pe *domain.Error
	if !errors.As(err, &pe) || pe.Class != domain.ClassConfigInvalid {
		t.Fatalf("Open = %v, want config_invalid", err)
	}
}
