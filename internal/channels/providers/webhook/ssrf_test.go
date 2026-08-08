package webhook

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/netguard"
)

// This file is the webhook provider's half of oto's SSRF story. The rule itself
// — which addresses are forbidden, and in which spellings — is tested once, in
// `internal/platform/netguard`. What is tested HERE is that the webhook provider
// actually installs that rule on the path a delivery takes: as the DIALER, not as
// a pre-flight check that `client.Do` is free to re-resolve around.

func rawConfig(t *testing.T, url string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"url": url})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testMessage() domain.RenderedMessage {
	return domain.RenderedMessage{Payload: []byte(`{"a":1}`), Fallback: "a", Hash: "h"}
}

// TestValidateConfigRefusesTheMetadataService is the configuration-time feedback:
// an operator who pastes the cloud metadata address learns why while the form is
// still open.
func TestValidateConfigRefusesTheMetadataService(t *testing.T) {
	t.Parallel()
	p := NewProvider(Options{Clock: clock.New()})

	blocked := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/",
		"http://[::1]/",
		"http://10.0.0.1/",
		// The historical IPv4 spellings netguard understands and the webhook
		// provider's old guard did not.
		"http://2130706433/",
		"http://0x7f000001/",
	}
	for _, u := range blocked {
		if err := p.ValidateConfig(context.Background(), rawConfig(t, u)); err == nil {
			t.Errorf("ValidateConfig allowed %s", u)
		}
	}

	if err := p.ValidateConfig(context.Background(), rawConfig(t, "https://receiver.example.com/hook")); err != nil {
		// Resolution may fail in a sandbox; that is UNDECIDED and must be tolerated.
		t.Errorf("ValidateConfig refused a public target: %v", err)
	}

	// Not http(s) at all.
	if err := p.ValidateConfig(context.Background(), rawConfig(t, "file:///etc/passwd")); err == nil {
		t.Error("ValidateConfig allowed a file:// target")
	}
}

// TestAllowPrivateTargetsOpensTheGuard is the self-hosted install whose receiver
// genuinely is on 10.0.0.0/8. It is a DEPLOYMENT switch, never per-tenant.
func TestAllowPrivateTargetsOpensTheGuard(t *testing.T) {
	t.Parallel()
	p := NewProvider(Options{Clock: clock.New(), AllowPrivateTargets: true})
	if err := p.ValidateConfig(context.Background(), rawConfig(t, "http://10.0.0.1/hook")); err != nil {
		t.Errorf("AllowPrivateTargets did not open the guard: %v", err)
	}
}

// TestUnresolvableHostIsNotARefusal covers the reason CheckURL is feedback and
// not the control: an operator registering an in-cluster name from a laptop must
// still be able to save it. The dialer refuses it at the moment it would be
// dialled.
func TestUnresolvableHostIsNotARefusal(t *testing.T) {
	t.Parallel()
	guard := netguard.New(netguard.Options{
		Code: "config_invalid", Field: "url",
		Lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
		},
	})
	p := NewProvider(Options{Clock: clock.New(), Guard: guard})
	if err := p.ValidateConfig(context.Background(), rawConfig(t, "http://receiver.monitoring.svc/hook")); err != nil {
		t.Errorf("an unresolvable host was treated as a refusal: %v", err)
	}
}

// TestDialIsTheControlUnderDNSRebinding is ⭐ the whole point of adopting
// netguard.
//
// The name resolves PUBLIC for EVERY pre-flight check — Open's and send's — and
// to the metadata service only for the lookup the DIAL performs. That is exactly
// the TTL-0 record an attacker serves, and it means no pre-flight can be what
// catches this: if the delivery is refused, the dialer refused it. The old guard
// did a pre-flight LookupNetIP and then let `client.Do` resolve a SECOND time, so
// this sequence dialled 169.254.169.254. Guard.DialContext has no second
// resolution to lose: it inspects the address it hands to the kernel.
func TestDialIsTheControlUnderDNSRebinding(t *testing.T) {
	t.Parallel()

	// Open checks once and send checks once; everything after is the dial.
	const preflights = 2

	var calls atomic.Int32
	guard := netguard.New(netguard.Options{
		Code: "config_invalid", Field: "url",
		Lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			if calls.Add(1) <= preflights {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		},
	})

	p := NewProvider(Options{Clock: clock.New(), Guard: guard})
	ch, err := p.Open(context.Background(),
		domain.ChannelConfig{Raw: rawConfig(t, "http://rebind.example.com/hook")},
		domain.Credential{Kind: CredNone})
	if err != nil {
		t.Fatalf("the FIRST resolution is public, so Open must succeed: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	_, err = ch.Deliver(context.Background(), domain.DeliverRequest{
		Message: testMessage(), Mode: domain.ModePostRoot, DeliveryID: uuid.New(),
	})
	if err == nil {
		t.Fatal("the rebound address was dialled: the guard is not on the dial path")
	}
	if got := calls.Load(); got <= preflights {
		t.Fatalf("the dialer never resolved (%d lookups): the request was refused "+
			"by a pre-flight, which proves nothing about rebinding", got)
	}
	// And it must be refused as a CONFIGURATION error. Classified `retryable`,
	// oto would re-dial a blocked target a dozen times on a backoff instead of
	// showing the operator what is wrong.
	var de *domain.Error
	if !asDomainError(err, &de) {
		t.Fatalf("want a *domain.Error, got %T: %v", err, err)
	}
	if de.Class != domain.ClassConfigInvalid {
		t.Fatalf("Class = %s, want %s: a blocked target is a config error, not a blip",
			de.Class, domain.ClassConfigInvalid)
	}
	if !strings.Contains(de.Cause.Error(), "link-local") {
		t.Fatalf("refused, but not as a blocked address: %v", de.Cause)
	}
}

// TestProviderResponseCarriesNoUpstreamBytes is the SSRF READ primitive, closed.
//
// `provider_response` is served by GET /deliveries/{id}. A body snippet there
// meant an attacker who caused a request also got the ANSWER back. What may be
// recorded is status, body SIZE and timing — enough to debug a delivery, and not
// a channel for content.
func TestProviderResponseCarriesNoUpstreamBytes(t *testing.T) {
	t.Parallel()

	const secret = "AKIAIOSFODNN7EXAMPLE-and-the-session-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(secret))
	}))
	t.Cleanup(srv.Close)

	// AllowPrivateTargets, because httptest binds loopback. The guard is tested
	// above; what is under test here is what is RECORDED.
	p := NewProvider(Options{Clock: clock.New(), AllowPrivateTargets: true})
	ch, err := p.Open(context.Background(),
		domain.ChannelConfig{Raw: rawConfig(t, srv.URL)}, domain.Credential{Kind: CredNone})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	res, err := ch.Deliver(context.Background(), domain.DeliverRequest{
		Message: testMessage(), Mode: domain.ModePostRoot, DeliveryID: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Raw), secret) {
		t.Fatalf("provider_response leaked the upstream body: %s", res.Raw)
	}

	var got struct {
		Status     int   `json:"status"`
		BodyBytes  int64 `json:"body_bytes"`
		DurationMS int64 `json:"duration_ms"`
	}
	if err := json.Unmarshal(res.Raw, &got); err != nil {
		t.Fatalf("provider_response is not the documented shape: %v", err)
	}
	if got.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", got.Status)
	}
	if got.BodyBytes != int64(len(secret)) {
		t.Errorf("body_bytes = %d, want %d", got.BodyBytes, len(secret))
	}
}

// TestErrorMessageCarriesNoUpstreamBytes closes the same hole on the FAILURE
// path: `error` on a delivery row is just as reachable as `provider_response`.
func TestErrorMessageCarriesNoUpstreamBytes(t *testing.T) {
	t.Parallel()

	const secret = "root:x:0:0:root:/root:/bin/bash"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "31536000") // one year
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(secret))
	}))
	t.Cleanup(srv.Close)

	p := NewProvider(Options{Clock: clock.New(), AllowPrivateTargets: true})
	ch, err := p.Open(context.Background(),
		domain.ChannelConfig{Raw: rawConfig(t, srv.URL)}, domain.Credential{Kind: CredNone})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	_, err = ch.Deliver(context.Background(), domain.DeliverRequest{
		Message: testMessage(), Mode: domain.ModePostRoot, DeliveryID: uuid.New(),
	})
	if err == nil {
		t.Fatal("a 429 must be an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the delivery error leaked the upstream body: %v", err)
	}

	var de *domain.Error
	if !asDomainError(err, &de) {
		t.Fatalf("want a *domain.Error, got %T", err)
	}
	if de.RetryAfter > maxRetryAfter {
		t.Errorf("RetryAfter = %v, want it clamped to %v: an upstream must not park a delivery for a year",
			de.RetryAfter, maxRetryAfter)
	}
	if de.RetryAfter != maxRetryAfter {
		t.Errorf("RetryAfter = %v, want the clamp exactly", de.RetryAfter)
	}
}

// TestCheckHeadersRefusesCredentialsInConfig is the second rule a JSON Schema
// cannot express. It survived the move out of the deleted ssrf.go.
func TestCheckHeadersRefusesCredentialsInConfig(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"Authorization", "authorization", "Proxy-Authorization", "Host", "Content-Length"} {
		if err := CheckHeaders(map[string]string{name: "x"}); err == nil {
			t.Errorf("CheckHeaders allowed %q", name)
		}
	}
	if err := CheckHeaders(map[string]string{"X-Evil": "a\r\nX-Injected: b"}); err == nil {
		t.Error("CheckHeaders allowed a header value with CRLF")
	}
	if err := CheckHeaders(map[string]string{"X-Fine": "value"}); err != nil {
		t.Errorf("CheckHeaders refused an ordinary header: %v", err)
	}
}

func asDomainError(err error, out **domain.Error) bool {
	de, ok := err.(*domain.Error) //nolint:errorlint // the provider returns it directly
	if ok {
		*out = de
	}
	return ok
}
