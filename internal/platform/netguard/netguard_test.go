package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestCheckAddrRefusesEveryDangerousRange is the deny list, stated as a test.
//
// The four the original guard had are here for regression; the rest are the ones
// the audit found missing, and each is a real reachable target.
func TestCheckAddrRefusesEveryDangerousRange(t *testing.T) {
	t.Parallel()
	g := New(Options{})

	blockedCases := []struct{ addr, why string }{
		{"127.0.0.1", "loopback"},
		{"127.1.2.3", "the rest of 127/8, which resolvers accept"},
		{"169.254.169.254", "AWS/GCP/Azure instance metadata (IMDSv1)"},
		{"169.254.0.1", "the rest of link-local"},
		{"10.0.0.1", "RFC1918"},
		{"172.16.0.1", "RFC1918"},
		{"172.31.255.254", "the top of 172.16/12"},
		{"192.168.1.1", "RFC1918"},
		{"0.0.0.0", "unspecified: connects to localhost on Linux"},
		{"0.0.0.1", "the rest of 0/8"},
		{"100.64.0.1", "CGNAT"},
		{"100.100.100.200", "Alibaba Cloud instance metadata"},
		{"192.0.0.8", "IETF protocol assignments (DS-Lite)"},
		{"198.18.0.1", "benchmarking, routinely used for internal test networks"},
		{"198.19.255.254", "the top of 198.18/15"},
		{"240.0.0.1", "reserved, accepted as local by many stacks"},
		{"224.0.0.1", "multicast"},
		{"192.88.99.1", "6to4 relay anycast"},

		{"::1", "IPv6 loopback"},
		{"::", "IPv6 unspecified"},
		{"fe80::1", "IPv6 link-local"},
		{"fd00::1", "IPv6 unique-local"},
		{"ff02::1", "IPv6 multicast"},

		// ⭐ The IPv4-in-IPv6 smuggling forms. Each is a 16-byte address that no
		// IPv4 rule matches and that still delivers a packet to the target.
		{"::ffff:127.0.0.1", "IPv4-mapped loopback"},
		{"::ffff:169.254.169.254", "IPv4-mapped metadata service"},
		{"::ffff:10.0.0.1", "IPv4-mapped RFC1918"},
		{"64:ff9b::169.254.169.254", "NAT64-translated metadata service"},
		{"2002:a9fe:a9fe::", "6to4-encapsulated 169.254.169.254"},
		{"2002:7f00:1::", "6to4-encapsulated 127.0.0.1"},
	}
	for _, tc := range blockedCases {
		addr := netip.MustParseAddr(tc.addr)
		if err := g.CheckAddr(addr); err == nil {
			t.Errorf("CheckAddr(%s) allowed, but it is %s", tc.addr, tc.why)
		}
	}

	for _, ok := range []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"} {
		if err := g.CheckAddr(netip.MustParseAddr(ok)); err != nil {
			t.Errorf("CheckAddr(%s) refused a public address: %v", ok, err)
		}
	}
}

// TestParseHostAddrUnderstandsHistoricalSpellings covers the encodings that
// reach 127.0.0.1 without ever looking like a dotted quad.
func TestParseHostAddrUnderstandsHistoricalSpellings(t *testing.T) {
	t.Parallel()

	loopbacks := []string{
		"2130706433",   // decimal
		"0x7f000001",   // hexadecimal
		"017700000001", // octal, one part
		"0177.0.0.1",   // octal, dotted
		"0x7f.0.0.1",   // hexadecimal, dotted
		"127.1",        // two-part shorthand
		"127.0.1",      // three-part shorthand
	}
	g := New(Options{})
	for _, h := range loopbacks {
		addr, ok := ParseHostAddr(h)
		if !ok {
			t.Fatalf("ParseHostAddr(%q) did not parse; it reaches 127.0.0.1", h)
		}
		if err := g.CheckAddr(addr); err == nil {
			t.Errorf("CheckAddr(%q → %s) allowed a loopback address", h, addr)
		}
	}

	// The metadata service, spelled as one integer.
	if addr, ok := ParseHostAddr("2852039166"); !ok || addr.String() != "169.254.169.254" {
		t.Fatalf("ParseHostAddr(2852039166) = %v, %v; want 169.254.169.254", addr, ok)
	}

	// A hostname must not be mistaken for an address.
	for _, h := range []string{"example.com", "alertmanager.internal", "am-1.prod", ""} {
		if _, ok := ParseHostAddr(h); ok {
			t.Errorf("ParseHostAddr(%q) parsed a hostname as an address", h)
		}
	}
}

// TestCheckURLRefusesLiteralsAndSchemes is the configuration-time feedback.
func TestCheckURLRefusesLiteralsAndSchemes(t *testing.T) {
	t.Parallel()
	g := New(Options{})
	ctx := context.Background()

	for _, raw := range []string{
		"http://169.254.169.254",
		"http://127.0.0.1:9093",
		"https://10.0.0.5:9093",
		"http://[::1]:9093",
		"http://2130706433:9093",
		"file:///etc/passwd",
		"gopher://127.0.0.1:9093",
	} {
		if err := g.CheckURL(ctx, raw); err == nil {
			t.Errorf("CheckURL(%q) allowed a forbidden target", raw)
		}
	}
	if err := g.CheckURL(ctx, "https://alertmanager.example.com:9093"); err != nil {
		// Resolution of a public name may fail in a sandbox; only a *pattern*
		// refusal is a real failure here.
		if !strings.Contains(err.Error(), "could not be resolved") {
			t.Errorf("CheckURL refused a public URL: %v", err)
		}
	}
}

// TestAllowPrivateIsDeploymentLevelEscapeHatch proves the switch actually opens
// the guard, for the self-hosted install whose Alertmanager really is on 10/8.
func TestAllowPrivateIsDeploymentLevelEscapeHatch(t *testing.T) {
	t.Parallel()
	g := New(Options{AllowPrivate: true})
	if err := g.CheckAddr(netip.MustParseAddr("10.0.0.1")); err != nil {
		t.Fatalf("AllowPrivate did not open the guard: %v", err)
	}
	if err := g.CheckURL(context.Background(), "http://127.0.0.1:9093"); err != nil {
		t.Fatalf("AllowPrivate did not open CheckURL: %v", err)
	}
}

// TestDialContextRefusesBlockedLiteral is the control at its simplest.
func TestDialContextRefusesBlockedLiteral(t *testing.T) {
	t.Parallel()
	g := New(Options{})
	if _, err := g.DialContext(context.Background(), "tcp", "169.254.169.254:80"); err == nil {
		t.Fatal("DialContext connected to the metadata service")
	}
	if _, err := g.DialContext(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("DialContext connected to loopback")
	}
}

// ⭐ TestDialTimeGuardDefeatsDNSRebinding is the reason this package exists.
//
// It models the TOCTOU exactly: a resolver whose FIRST answer is a public
// address and whose EVERY LATER answer is the metadata service — a record served
// with TTL 0 and re-pointed between the check and the connect.
//
// A pre-flight `CheckURL` followed by `client.Do` passes the check (answer 1) and
// then dials answer 2, because the transport resolves independently. The guarded
// dialer cannot: it performs the resolution that the connection is made from, so
// the address it inspects IS the address dialled.
func TestDialTimeGuardDefeatsDNSRebinding(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	rebinding := &stubResolver{
		answers: func() []netip.Addr {
			if calls.Add(1) == 1 {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")} // public
			}
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")} // rebound
		},
	}
	g := newWithLookup(false, rebinding.lookup)

	// 1. The pre-flight check PASSES, exactly as it would for a real attacker.
	if err := g.CheckURL(context.Background(), "http://rebind.example"); err != nil {
		t.Fatalf("the pre-flight check should have passed on the first answer: %v", err)
	}

	// 2. The dial that follows is refused, because it re-resolves and inspects
	//    what it got. This is the step that a LookupNetIP-then-Do guard loses.
	_, err := g.DialContext(context.Background(), "tcp", "rebind.example:80")
	if err == nil {
		t.Fatal("the dial-time guard connected to a rebound host; the TOCTOU is still open")
	}
	if !strings.Contains(err.Error(), "link-local") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if calls.Load() < 2 {
		t.Fatalf("the dialer reused the pre-flight resolution (%d lookups); it must resolve for itself", calls.Load())
	}
}

// TestDialContextRefusesMixedAnswerSet proves EVERY candidate is checked before
// ANY is dialled. Pairing one public A record with one loopback record is how an
// attacker wins a guard that stops at the first good answer.
func TestDialContextRefusesMixedAnswerSet(t *testing.T) {
	t.Parallel()

	mixed := &stubResolver{answers: func() []netip.Addr {
		return []netip.Addr{
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("127.0.0.1"),
		}
	}}
	g := newWithLookup(false, mixed.lookup)

	if _, err := g.DialContext(context.Background(), "tcp", "mixed.example:80"); err == nil {
		t.Fatal("DialContext accepted an answer set containing a loopback address")
	}
}

// TestTransportCarriesTheGuard proves the wiring: an http.Client built over
// Guard.Transport cannot reach a blocked address even though the URL is
// syntactically fine and the server really is listening.
func TestTransportCarriesTheGuard(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("SECRET-FROM-THE-METADATA-SERVICE"))
	}))
	defer srv.Close()

	// httptest listens on loopback, which is precisely a blocked range.
	client := &http.Client{Transport: New(Options{}).Transport(nil), Timeout: 5 * time.Second}
	resp, err := client.Get(srv.URL) //nolint:noctx // a two-line guard assertion
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the guarded transport reached a loopback server")
	}

	// With the deployment-level switch on, the same request succeeds — the escape
	// hatch has to actually work or self-hosted installs cannot use oto at all.
	open := &http.Client{Transport: New(Options{AllowPrivate: true}).Transport(nil), Timeout: 5 * time.Second}
	resp, err = open.Get(srv.URL) //nolint:noctx // a two-line guard assertion
	if err != nil {
		t.Fatalf("AllowPrivate transport could not reach loopback: %v", err)
	}
	_ = resp.Body.Close()
}

// TestCheckAddrPortRefusesUnresolvedHost proves the dial-time check fails closed
// on anything that is not a literal: an unresolved address is one the guard
// cannot vouch for.
func TestCheckAddrPortRefusesUnresolvedHost(t *testing.T) {
	t.Parallel()
	g := New(Options{})
	if err := g.CheckAddrPort("example.com:443"); err == nil {
		t.Fatal("CheckAddrPort accepted a hostname")
	}
	if err := g.CheckAddrPort("not-an-address"); err == nil {
		t.Fatal("CheckAddrPort accepted a malformed address")
	}
	if err := g.CheckAddrPort("1.1.1.1:443"); err != nil {
		t.Fatalf("CheckAddrPort refused a public literal: %v", err)
	}
}

// --------------------------------------------------------------------- helpers

// stubResolver answers lookups from a function, so a rebinding attacker can be
// modelled without a DNS server.
type stubResolver struct{ answers func() []netip.Addr }

func (s *stubResolver) lookup(_ context.Context, _, _ string) ([]netip.Addr, error) {
	addrs := s.answers()
	if len(addrs) == 0 {
		return nil, errors.New("no such host")
	}
	return addrs, nil
}

// newWithLookup builds a Guard whose resolution is the supplied function and
// whose dialer never actually connects, so a test that expects a refusal cannot
// accidentally open a socket.
func newWithLookup(allowPrivate bool, lookup lookupFunc) *Guard {
	g := New(Options{AllowPrivate: allowPrivate})
	g.lookup = lookup
	g.dialer = &net.Dialer{Timeout: 50 * time.Millisecond}
	return g
}
