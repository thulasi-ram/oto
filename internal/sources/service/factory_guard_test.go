package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/netguard"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// ⭐ TestOutboundClientsRefuseAtDialTime is the half of C1 that the create-time
// check cannot cover.
//
// A source row can predate the guard, or name a host that resolves somewhere
// harmless at configuration time and somewhere dangerous later. Neither matters,
// because the client this factory builds refuses at the SOCKET: the httptest
// server below really is listening, the URL really is well formed, and the
// request never reaches it.
func TestOutboundClientsRefuseAtDialTime(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"versionInfo":{"version":"LEAKED"}}`))
	}))
	defer srv.Close()

	f := NewClientFactory(nil)
	f.Dial = netguard.New(netguard.Options{}).DialContext

	// httptest listens on loopback, which is exactly what an attacker points a
	// source at to reach oto's own /metrics and /readyz.
	src := domain.Source{
		ID: uuid.New(), Name: "evil", Kind: domain.KindAlertmanager,
		BaseURL: srv.URL, PrometheusURL: srv.URL,
	}

	am, err := f.Alertmanager(src, domain.Credential{Kind: domain.AuthNone})
	if err != nil {
		t.Fatalf("build alertmanager client: %v", err)
	}
	if _, err := am.StatusDetail(context.Background()); err == nil {
		t.Fatal("the Alertmanager client reached a loopback address")
	}

	prom, err := f.Prometheus(src, domain.Credential{Kind: domain.AuthNone}, "")
	if err != nil {
		t.Fatalf("build prometheus client: %v", err)
	}
	if _, err := prom.Rules(context.Background(), nil); err == nil {
		t.Fatal("the Prometheus client reached a loopback address")
	}
}

// ⭐ TestDialTimeGuardBeatsARebindThroughTheRealClient walks the whole outbound
// stack with a resolver that answers public once and then re-points at the
// metadata service — the TTL-0 rebind the audit called structurally certain
// against a pre-flight check.
func TestDialTimeGuardBeatsARebindThroughTheRealClient(t *testing.T) {
	t.Parallel()

	first := true
	guard := netguard.New(netguard.Options{
		Lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			if first {
				first = false
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		},
	})

	// The pre-flight check passes on the first answer, exactly as it would for a
	// real attacker's record.
	if err := guard.CheckURL(context.Background(), "https://rebind.example"); err != nil {
		t.Fatalf("the pre-flight check should have passed: %v", err)
	}

	f := NewClientFactory(nil)
	f.Dial = guard.DialContext
	src := domain.Source{ID: uuid.New(), Kind: domain.KindAlertmanager, BaseURL: "https://rebind.example"}

	am, err := f.Alertmanager(src, domain.Credential{Kind: domain.AuthNone})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	_, err = am.StatusDetail(context.Background())
	if err == nil {
		t.Fatal("the client followed a rebound name to the metadata service")
	}
	// The transport error is wrapped as `alertmanager_unreachable` by the time it
	// leaves the client, so the reason is read out of the cause chain. It matters
	// that it is the GUARD's reason: "the host was unreachable" would also be
	// produced by a sandbox with no network, and that would prove nothing.
	if !strings.Contains(chain(err), "link-local") {
		t.Fatalf("refused for the wrong reason: %v", chain(err))
	}
}

// chain renders an error and every cause under it, because the guard's reason is
// several wraps below the surface by the time a client returns it.
func chain(err error) string {
	var parts []string
	for err != nil {
		parts = append(parts, err.Error())
		err = errors.Unwrap(err)
	}
	return strings.Join(parts, " | ")
}

// ⛔ TestTLSSkipVerifyNeedsTheDeploymentSwitch is §M2 at the layer that actually
// builds the TLS config. `alert_sources.tls_skip_verify` is a tenant-writable
// column, and honouring it unconditionally meant any org member could turn off
// certificate verification for a connection oto's own process makes.
func TestTLSSkipVerifyNeedsTheDeploymentSwitch(t *testing.T) {
	t.Parallel()

	src := domain.Source{TLSSkipVerify: true}
	closed := NewClientFactory(nil)
	if closed.skipVerify(src) {
		t.Fatal("a tenant's tls_skip_verify took effect without the deployment switch")
	}

	open := NewClientFactory(nil)
	open.AllowInsecureTLS = true
	if !open.skipVerify(src) {
		t.Fatal("the deployment switch did not grant tls_skip_verify")
	}
	// And the switch alone grants nothing: the source still has to ask.
	if open.skipVerify(domain.Source{}) {
		t.Fatal("the deployment switch disabled verification for a source that never asked")
	}
}
