package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
)

// ADR 0052 §5's echo against a stub receiver (git-bug 506ff21). The governing
// comment is explicit that the vendors' own alert endpoints return no incident URL,
// so the only honest test is a stub that does — and a stub that answers every other
// way a real receiver does, to prove each of them changes nothing.

// echoReceiver answers every request with status and body.
func echoReceiver(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func deliverTo(t *testing.T, url string) (domain.DeliverResult, error) {
	t.Helper()
	ch := openSigned(t, clock.New(), rawConfig(t, url), domain.Credential{Kind: CredNone})
	return ch.Deliver(context.Background(), domain.DeliverRequest{
		Message: testMessage(), Mode: domain.ModePostRoot, DeliveryID: uuid.New(),
	})
}

// TestAReceiverThatEchoesItsIncidentIsHeard is the case the echo exists for — and
// the one byte rule it must not break: `provider_response` still carries the
// status, the body's size and the time, and none of the receiver's bytes.
func TestAReceiverThatEchoesItsIncidentIsHeard(t *testing.T) {
	t.Parallel()
	const (
		extURL = "https://tool.example/incidents/42"
		extID  = "INC-42"
	)
	res, err := deliverTo(t, echoReceiver(t, http.StatusCreated,
		`{"external_url":"`+extURL+`","external_id":"`+extID+`","status":"triggered"}`))
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.External != (domain.ExternalIncident{URL: extURL, ID: extID}) {
		t.Fatalf("External = %+v, want the echoed url and id", res.External)
	}
	raw := string(res.Raw)
	for _, leaked := range []string{extURL, extID, "triggered"} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("⛔ provider_response carries receiver bytes (%q): %s", leaked, raw)
		}
	}
	if !strings.Contains(raw, `"status":201`) || !strings.Contains(raw, `"body_bytes":`) {
		t.Fatalf("provider_response lost its status or size: %s", raw)
	}
}

// TestAReceiverThatEchoesNothingBehavesExactlyAsBefore: every way of saying nothing
// usable is a `sent` delivery with no echo — never a failure.
func TestAReceiverThatEchoesNothingBehavesExactlyAsBefore(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want domain.ExternalIncident
	}{
		{"no keys (PagerDuty's 202 shape)", `{"status":"success","message":"Event processed","dedup_key":"k"}`, domain.ExternalIncident{}},
		{"an empty body", ``, domain.ExternalIncident{}},
		{"not JSON", `<html>ok</html>`, domain.ExternalIncident{}},
		{"a JSON array", `[{"external_url":"https://tool.example/i/1"}]`, domain.ExternalIncident{}},
		{"a non-https url, with a good id", `{"external_url":"http://tool.example/i/1","external_id":"INC-1"}`,
			domain.ExternalIncident{ID: "INC-1"}},
		{"a numeric id", `{"external_id":42}`, domain.ExternalIncident{}},
		{"an oversized id", `{"external_id":"` + strings.Repeat("x", domain.MaxExternalIDLength+1) + `"}`,
			domain.ExternalIncident{}},
		{"a body past the read bound", `{"pad":"` + strings.Repeat("x", maxResponseBytes) +
			`","external_url":"https://tool.example/i/1"}`, domain.ExternalIncident{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := deliverTo(t, echoReceiver(t, http.StatusAccepted, tc.body))
			if err != nil {
				t.Fatalf("a 202 became a failure because of what its body said: %v", err)
			}
			if res.External != tc.want {
				t.Fatalf("External = %+v, want %+v", res.External, tc.want)
			}
		})
	}
}

// TestARefusalIsNotReadForAnEcho: an incident the receiver refused to open has no
// handle worth keeping, whatever its error body names.
func TestARefusalIsNotReadForAnEcho(t *testing.T) {
	t.Parallel()
	res, err := deliverTo(t, echoReceiver(t, http.StatusBadRequest,
		`{"external_url":"https://tool.example/i/1","external_id":"INC-1"}`))
	if err == nil {
		t.Fatal("a 400 was recorded as a success")
	}
	if !res.External.IsZero() {
		t.Fatalf("a refused delivery carried an echo: %+v", res.External)
	}
}
