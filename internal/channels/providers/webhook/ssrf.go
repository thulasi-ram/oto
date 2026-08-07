package webhook

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// oto's webhook target is operator-supplied, and oto runs inside the operator's
// network. That combination is the textbook Server-Side Request Forgery setup: a
// URL pointing at 169.254.169.254 turns a notification channel into a cloud
// credential exfiltration tool, and one pointing at 127.0.0.1 turns it into a way
// to reach oto's own admin surfaces from outside.
//
// So a target is checked at CONFIGURATION time, when a human is present to read
// the error, and again before every request, because DNS can be re-pointed after
// the config was saved.

// Guard decides which network targets a webhook channel may reach.
type Guard struct {
	// AllowPrivate opens the guard for a self-hosted install whose receiver
	// genuinely is on a private address. It is off by default and is set from
	// OTO_ALLOW_PRIVATE_WEBHOOK_TARGETS (§L.5).
	AllowPrivate bool
	// Resolver is the DNS resolver. It is injectable so the rule can be tested
	// without a network.
	Resolver *net.Resolver
}

// NewGuard builds the SSRF guard.
func NewGuard(allowPrivate bool) *Guard {
	return &Guard{AllowPrivate: allowPrivate, Resolver: net.DefaultResolver}
}

// CheckURL validates the scheme, then every address the host resolves to.
//
// EVERY address, not the first: a hostname with one public and one loopback A
// record would otherwise pass the check and then be dialled at the loopback one.
func (g *Guard) CheckURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return violation("url", "format", "the endpoint URL could not be parsed")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return violation("url", "pattern", "the endpoint URL must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return violation("url", "format", "the endpoint URL has no host")
	}

	if g.AllowPrivate {
		return nil
	}

	// A literal address needs no resolution and must not get one.
	if addr, err := netip.ParseAddr(host); err == nil {
		return g.checkAddr(addr)
	}

	resolver := g.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return violation("url", "format", "the endpoint host could not be resolved")
	}
	if len(addrs) == 0 {
		return violation("url", "format", "the endpoint host resolved to no addresses")
	}
	for _, a := range addrs {
		if err := g.checkAddr(a); err != nil {
			return err
		}
	}
	return nil
}

func (g *Guard) checkAddr(addr netip.Addr) error {
	if g.AllowPrivate {
		return nil
	}
	a := addr.Unmap()
	switch {
	case a.IsLoopback():
		return violation("url", "pattern", "the endpoint URL resolves to a loopback address")
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		// 169.254.169.254 is the cloud instance metadata service. This is the
		// single most valuable target an SSRF can reach.
		return violation("url", "pattern", "the endpoint URL resolves to a link-local address")
	case a.IsPrivate():
		return violation("url", "pattern", "the endpoint URL resolves to a private address")
	case a.IsUnspecified():
		return violation("url", "pattern", "the endpoint URL resolves to an unspecified address")
	case a.IsMulticast(), a.IsInterfaceLocalMulticast():
		return violation("url", "pattern", "the endpoint URL resolves to a multicast address")
	case isUniqueLocalV6(a):
		return violation("url", "pattern", "the endpoint URL resolves to a unique-local address")
	default:
		return nil
	}
}

// isUniqueLocalV6 covers fc00::/7, which netip does not classify as private.
func isUniqueLocalV6(a netip.Addr) bool {
	return a.Is6() && a.As16()[0]&0xfe == 0xfc
}

// forbiddenHeaders are headers a user-supplied config may not set.
//
// Authorization is the one that matters: a credential belongs in
// channel_credentials, sealed and rotatable, not in a config blob that is served
// to the settings UI and logged in an audit trail (§L.5). The rest would let a
// config override oto's own framing of the request.
var forbiddenHeaders = map[string]string{
	"authorization":       "credentials belong in the channel credential, not in headers",
	"proxy-authorization": "credentials belong in the channel credential, not in headers",
	"content-length":      "oto sets this header",
	"host":                "oto sets this header",
	"transfer-encoding":   "oto sets this header",
	"connection":          "oto sets this header",
}

// CheckHeaders rejects the headers a config may not carry.
func CheckHeaders(headers map[string]string) error {
	var violations []errs.Violation
	for name := range headers {
		if reason, bad := forbiddenHeaders[strings.ToLower(strings.TrimSpace(name))]; bad {
			violations = append(violations, errs.Violation{
				Field:   "headers/" + name,
				Code:    "forbidden",
				Message: reason,
			})
		}
		if strings.ContainsAny(name, "\r\n") {
			violations = append(violations, errs.Violation{
				Field: "headers/" + name, Code: "pattern",
				Message: "a header name may not contain a newline",
			})
		}
	}
	for name, value := range headers {
		if strings.ContainsAny(value, "\r\n") {
			violations = append(violations, errs.Violation{
				Field: "headers/" + name, Code: "pattern",
				Message: "a header value may not contain a newline",
			})
		}
	}
	if len(violations) > 0 {
		return errs.Validation("config_invalid", "the webhook headers are not permitted", violations...)
	}
	return nil
}

func violation(field, code, message string) error {
	return errs.Validation("config_invalid", message,
		errs.Violation{Field: field, Code: code, Message: message})
}
