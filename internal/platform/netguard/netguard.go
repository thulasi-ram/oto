package netguard

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// DefaultCode is the error code a blocked target reports when Options.Code is
// empty. It is a validation code because the caller can fix it by configuring a
// different URL.
const DefaultCode = "url_not_permitted"

// DefaultField is the violation field a blocked target reports when
// Options.Field is empty.
const DefaultField = "url"

// CodeUnresolved is the violation code for a check that COULD NOT DECIDE because
// the host did not resolve.
//
// ⭐ IT IS NOT A REFUSAL, and telling the two apart matters. The configuration-time
// check is feedback, not the control: an operator registering
// `alertmanager.monitoring.svc` from a machine that cannot resolve it, or during
// a DNS blip, must not be blocked from saving a URL that will resolve perfectly
// well from oto's pod. The DIALER decides, every time, on the address it actually
// connects to — so "I could not look this up" is a reason to defer to it, not a
// reason to reject the operator's configuration.
//
// A blocked ADDRESS is a different answer entirely and always uses "pattern".
const CodeUnresolved = "unresolved"

// dialTimeout bounds one guarded connection attempt. It is deliberately shorter
// than any caller's overall budget: a dial that hangs is a worker that is not
// serving anything.
const dialTimeout = 5 * time.Second

// Options build a Guard.
//
// ⛔ AllowPrivate IS A DEPLOYMENT-LEVEL SETTING AND MUST NEVER BE PER-TENANT. A
// self-hosted operator whose Alertmanager genuinely sits on 10.0.0.0/8 turns it
// on for the whole process, knowingly. A tenant that could turn it on for its own
// source would be a tenant that can read the host's metadata service.
type Options struct {
	// AllowPrivate opens the guard for a deployment whose upstreams are on a
	// private network. DEFAULT CLOSED: the zero Options refuse every private,
	// loopback, link-local, CGNAT and special-use address.
	AllowPrivate bool
	// Resolver is the DNS resolver. Nil means net.DefaultResolver.
	Resolver *net.Resolver
	// Lookup replaces the resolution step entirely, which is how a test models an
	// attacker's DNS — including a TTL-0 rebind — without a DNS server. It takes
	// precedence over Resolver. Leave nil in production.
	Lookup func(ctx context.Context, network, host string) ([]netip.Addr, error)
	// Code namespaces the error a blocked target produces, so a caller can tell
	// its own violation apart. Empty means DefaultCode.
	Code string
	// Field is the violation field path reported to the caller. Empty means
	// DefaultField.
	Field string
	// Dialer is the underlying dialer. Nil means a net.Dialer with dialTimeout.
	// It exists so a test can substitute a connection factory.
	Dialer *net.Dialer
}

// lookupFunc is the resolution step, isolated so a test can model an attacker's
// DNS without a DNS server. Production always holds `(*net.Resolver).LookupNetIP`.
type lookupFunc func(ctx context.Context, network, host string) ([]netip.Addr, error)

// Guard decides which network targets this process may reach.
type Guard struct {
	allowPrivate bool
	lookup       lookupFunc
	code         string
	field        string
	dialer       *net.Dialer
}

// New builds a Guard. The zero Options produce a guard that is closed by default.
func New(o Options) *Guard {
	resolver := o.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	lookup := o.Lookup
	if lookup == nil {
		lookup = resolver.LookupNetIP
	}
	g := &Guard{
		allowPrivate: o.AllowPrivate,
		lookup:       lookup,
		code:         o.Code,
		field:        o.Field,
		dialer:       o.Dialer,
	}
	if g.code == "" {
		g.code = DefaultCode
	}
	if g.field == "" {
		g.field = DefaultField
	}
	if g.dialer == nil {
		g.dialer = &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	}
	return g
}

// AllowsPrivate reports whether this deployment has opted into private targets.
func (g *Guard) AllowsPrivate() bool { return g != nil && g.allowPrivate }

// CheckURL validates the scheme and then every address the host resolves to.
//
// ⚠️ IT IS FAST FEEDBACK, NOT THE CONTROL. It runs at configuration time so an
// operator who pastes `http://169.254.169.254` learns why it was refused while
// they are still looking at the form. The resolution it performs is not the
// resolution the dial performs, so a passing CheckURL is never permission —
// DialContext re-checks the address that is actually connected.
//
// EVERY resolved address is checked, not the first: a hostname with one public
// and one loopback A record would otherwise pass and then be dialled at the
// loopback one.
func (g *Guard) CheckURL(ctx context.Context, raw string) error {
	if g == nil {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return g.violation("format", "the URL could not be parsed")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return g.violation("pattern", "the URL must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return g.violation("format", "the URL has no host")
	}
	return g.CheckHost(ctx, host)
}

// CheckHost validates one hostname or address literal.
func (g *Guard) CheckHost(ctx context.Context, host string) error {
	if g == nil || g.allowPrivate {
		return nil
	}

	// A literal address needs no resolution and must not get one. ParseHostAddr
	// also understands the decimal, octal and hexadecimal spellings of an IPv4
	// address — `http://2130706433/` is 127.0.0.1 to every resolver on earth and
	// must not survive a check that only understands dotted quads.
	if addr, ok := ParseHostAddr(host); ok {
		return g.CheckAddr(addr)
	}

	addrs, err := g.lookup(ctx, "ip", host)
	if err != nil {
		return g.violation(CodeUnresolved, "the host could not be resolved right now")
	}
	if len(addrs) == 0 {
		return g.violation(CodeUnresolved, "the host resolved to no addresses right now")
	}
	for _, a := range addrs {
		if err := g.CheckAddr(a); err != nil {
			return err
		}
	}
	return nil
}

// CheckAddr is THE rule. Everything else in this package exists to make sure this
// function sees the address that will actually be dialled.
func (g *Guard) CheckAddr(addr netip.Addr) error {
	if g == nil || g.allowPrivate {
		return nil
	}
	if !addr.IsValid() {
		return g.violation("pattern", "the URL resolves to an invalid address")
	}
	// Every IPv4-in-IPv6 spelling is normalised to plain IPv4 first: ::ffff:127.0.0.1
	// is 127.0.0.1, and a rule that only checked the 16-byte form would let it past.
	for _, a := range unwrap(addr) {
		if reason := blocked(a); reason != "" {
			return g.violation("pattern", "the URL resolves to "+reason)
		}
	}
	return nil
}

// CheckAddrPort validates a `host:port` string whose host is an address literal,
// which is the shape a DialContext hook is handed.
func (g *Guard) CheckAddrPort(addr string) error {
	if g == nil || g.allowPrivate {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return g.violation("format", "the dial address could not be parsed")
	}
	parsed, err := netip.ParseAddr(host)
	if err != nil {
		// A DialContext hook is always handed a literal by the transport's own
		// resolver. Anything else means the address was never resolved, and an
		// unresolved address is one this guard cannot vouch for.
		return g.violation("pattern", "the dial address is not a resolved IP")
	}
	return g.CheckAddr(parsed)
}

// DialContext is ⭐ THE ACTUAL SSRF CONTROL.
//
// It resolves the host ITSELF, checks every candidate address, and then dials a
// checked address DIRECTLY — never the hostname. That last detail is what defeats
// DNS rebinding: there is no window between the check and the connect in which a
// TTL-0 record can be re-pointed, because the connect does not consult DNS at
// all.
//
// Install it on an *http.Transport and every request that transport makes is
// covered, including redirects and connection reuse.
func (g *Guard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if g == nil {
		return (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, network, address)
	}
	if g.allowPrivate {
		return g.dialer.DialContext(ctx, network, address)
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, g.violation("format", "the dial address could not be parsed")
	}

	// A literal (in any spelling) is checked and dialled as itself.
	if addr, ok := ParseHostAddr(host); ok {
		if err := g.CheckAddr(addr); err != nil {
			return nil, err
		}
		return g.dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
	}

	addrs, err := g.lookup(ctx, lookupNetwork(network), host)
	if err != nil {
		return nil, g.violation("format", "the host could not be resolved")
	}
	if len(addrs) == 0 {
		return nil, g.violation("format", "the host resolved to no addresses")
	}

	// EVERY candidate is checked before ANY is dialled. Dialling the first good
	// one out of a mixed set would let an attacker pair a public A record with a
	// loopback one and win whichever the resolver happened to order first.
	for _, a := range addrs {
		if err := g.CheckAddr(a); err != nil {
			return nil, err
		}
	}

	var lastErr error
	for _, a := range addrs {
		conn, derr := g.dialer.DialContext(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, lastErr
}

// Transport returns a clone of base with this guard's dialer installed.
//
// A nil base clones http.DefaultTransport. The clone is deliberate: mutating a
// shared transport would apply the guard to every client in the process, or
// worse, remove it from one.
func (g *Guard) Transport(base *http.Transport) *http.Transport {
	if base == nil {
		if def, ok := http.DefaultTransport.(*http.Transport); ok {
			base = def
		} else {
			base = &http.Transport{}
		}
	}
	tr := base.Clone()
	if g != nil {
		tr.DialContext = g.DialContext
		// A proxy would dial the proxy rather than the target, which silently takes
		// the guard out of the path. oto never proxies its outbound API calls.
		tr.Proxy = nil
	}
	return tr
}

// lookupNetwork maps a dial network onto the address family LookupNetIP wants.
func lookupNetwork(network string) string {
	switch network {
	case "tcp4", "udp4", "ip4":
		return "ip4"
	case "tcp6", "udp6", "ip6":
		return "ip6"
	default:
		return "ip"
	}
}

// Undecided reports whether err means "the guard could not look this host up",
// as opposed to "this host is forbidden".
//
// A caller doing configuration-time validation should ACCEPT an undecided
// answer: the dialer will make the real decision, and refusing to save a URL
// because DNS was slow is a product that cannot be configured during a DNS blip.
// Nothing on the DIAL path may use this — there, an address that cannot be
// resolved is an address that cannot be dialled.
func Undecided(err error) bool {
	e, ok := errs.As(err)
	if !ok {
		return false
	}
	for _, v := range e.Violations {
		if v.Code != CodeUnresolved {
			return false
		}
	}
	return len(e.Violations) > 0
}

// violation renders a blocked target as a validation error.
//
// ⚠️ The message names the CLASS of address, never the address itself. Echoing
// the resolved IP back would turn the guard into the DNS oracle it exists to
// prevent: an attacker learns what a name resolves to from inside the network by
// reading the refusal.
func (g *Guard) violation(code, message string) error {
	return errs.Validation(g.code, message,
		errs.Violation{Field: g.field, Code: code, Message: message})
}
