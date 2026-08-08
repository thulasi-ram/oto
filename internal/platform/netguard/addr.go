package netguard

import (
	"net/netip"
	"strconv"
	"strings"
)

// blockedPrefix is one refused range and the phrase used to explain it.
type blockedPrefix struct {
	prefix netip.Prefix
	reason string
}

// blockedV4 is the IPv4 deny list.
//
// The first four are the ones every SSRF guard has. The rest are the ones the
// audit found missing, and each is a real reachable target:
//
//   - 100.64.0.0/10 is CGNAT, and Alibaba Cloud's instance metadata service lives
//     at 100.100.100.200. A guard that stops at RFC1918 hands an Alibaba tenant's
//     credentials to anyone who can type a URL.
//   - 192.0.0.0/24 is IETF protocol assignments, which includes the DS-Lite
//     address 192.0.0.8 and NAT64's well-known prefix.
//   - 198.18.0.0/15 is benchmarking, routinely used for internal test networks.
//   - 240.0.0.0/4 is reserved and is accepted by many stacks as a local address.
//   - 192.88.99.0/24 is the deprecated 6to4 anycast relay.
var blockedV4 = []blockedPrefix{
	{netip.MustParsePrefix("0.0.0.0/8"), "an unspecified or this-network address"},
	{netip.MustParsePrefix("10.0.0.0/8"), "a private address"},
	{netip.MustParsePrefix("127.0.0.0/8"), "a loopback address"},
	{netip.MustParsePrefix("169.254.0.0/16"), "a link-local address (the cloud metadata service)"},
	{netip.MustParsePrefix("172.16.0.0/12"), "a private address"},
	{netip.MustParsePrefix("192.168.0.0/16"), "a private address"},
	{netip.MustParsePrefix("100.64.0.0/10"), "a carrier-grade NAT address (the Alibaba metadata service)"},
	{netip.MustParsePrefix("192.0.0.0/24"), "an IETF protocol assignment address"},
	{netip.MustParsePrefix("192.0.2.0/24"), "a documentation address"},
	{netip.MustParsePrefix("192.88.99.0/24"), "a 6to4 relay anycast address"},
	{netip.MustParsePrefix("198.18.0.0/15"), "a benchmarking address"},
	{netip.MustParsePrefix("198.51.100.0/24"), "a documentation address"},
	{netip.MustParsePrefix("203.0.113.0/24"), "a documentation address"},
	{netip.MustParsePrefix("224.0.0.0/4"), "a multicast address"},
	{netip.MustParsePrefix("240.0.0.0/4"), "a reserved address"},
}

// blockedV6 is the IPv6 deny list. IPv4-mapped and IPv4-translated forms are not
// listed here because unwrap has already reduced them to their IPv4 address,
// which is then checked against blockedV4.
var blockedV6 = []blockedPrefix{
	{netip.MustParsePrefix("::/128"), "an unspecified address"},
	{netip.MustParsePrefix("::1/128"), "a loopback address"},
	{netip.MustParsePrefix("fc00::/7"), "a unique-local address"},
	{netip.MustParsePrefix("fe80::/10"), "a link-local address"},
	{netip.MustParsePrefix("ff00::/8"), "a multicast address"},
	{netip.MustParsePrefix("2001:db8::/32"), "a documentation address"},
	{netip.MustParsePrefix("100::/64"), "a discard-only address"},
}

// v4MappedPrefixes are the IPv6 ranges that CARRY an IPv4 address in their low
// 32 bits. Each has been used to smuggle 127.0.0.1 past a guard that only looked
// at the 16-byte form.
var v4MappedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("::ffff:0:0/96"),  // IPv4-mapped
	netip.MustParsePrefix("::/96"),          // IPv4-compatible (deprecated)
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 well-known
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use
	netip.MustParsePrefix("2002::/16"),      // 6to4: the v4 address is bits 16..47
}

// blocked reports why addr is refused, or "" when it is permitted.
func blocked(addr netip.Addr) string {
	a := addr.Unmap()
	if a.Is4() {
		for _, b := range blockedV4 {
			if b.prefix.Contains(a) {
				return b.reason
			}
		}
		return ""
	}
	for _, b := range blockedV6 {
		if b.prefix.Contains(a) {
			return b.reason
		}
	}
	return ""
}

// unwrap returns every address a caller could reach by dialling addr: the address
// itself, plus any IPv4 address embedded in it.
//
// ⛔ THIS IS WHY `::ffff:169.254.169.254` AND `64:ff9b::169.254.169.254` DO NOT
// WORK. Both are 16-byte addresses that no IPv4 rule would ever match, and both
// deliver a packet to the metadata service.
func unwrap(addr netip.Addr) []netip.Addr {
	out := []netip.Addr{addr.Unmap()}
	if !addr.Is6() || addr.Is4In6() {
		return out
	}
	b := addr.As16()
	for _, p := range v4MappedPrefixes {
		if !p.Contains(addr) {
			continue
		}
		var v4 netip.Addr
		if p.Bits() == 16 { // 6to4 carries the address at bytes 2..5.
			v4 = netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})
		} else {
			v4 = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
		}
		out = append(out, v4)
	}
	return out
}

// ParseHostAddr parses a URL host as an IP address, accepting every spelling a
// resolver would.
//
// ⚠️ `http://2130706433/`, `http://0177.0.0.1/` and `http://0x7f.0.0.1/` all reach
// 127.0.0.1. netip.ParseAddr accepts none of them, so a guard built on ParseAddr
// alone falls through to a DNS lookup, gets "no such host" or — on a cgo
// resolver — the loopback address it was trying to refuse. This function
// normalises them so CheckAddr sees the address the kernel will.
func ParseHostAddr(host string) (netip.Addr, bool) {
	h := strings.TrimSpace(host)
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	if h == "" {
		return netip.Addr{}, false
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return addr, true
	}
	// A zone-bearing literal ("fe80::1%eth0") loses its zone for classification.
	if i := strings.IndexByte(h, '%'); i > 0 {
		if addr, err := netip.ParseAddr(h[:i]); err == nil {
			return addr.WithZone(""), true
		}
	}
	return parseNumericV4(h)
}

// parseNumericV4 decodes the historical IPv4 spellings: one to four parts, each
// decimal, octal (leading 0) or hexadecimal (leading 0x), with the last part
// absorbing every remaining byte.
func parseNumericV4(h string) (netip.Addr, bool) {
	parts := strings.Split(h, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return netip.Addr{}, false
	}

	values := make([]uint64, 0, len(parts))
	for _, p := range parts {
		v, ok := parseNumericPart(p)
		if !ok {
			return netip.Addr{}, false
		}
		values = append(values, v)
	}

	// Every leading part is one byte; the final part absorbs the remainder.
	var total uint64
	last := len(values) - 1
	for i := 0; i < last; i++ {
		if values[i] > 0xff {
			return netip.Addr{}, false
		}
		total |= values[i] << (8 * (3 - uint(i)))
	}
	remaining := uint(8 * (4 - len(values) + 1))
	if remaining < 64 && values[last] >= uint64(1)<<remaining {
		return netip.Addr{}, false
	}
	total |= values[last]
	if total > 0xffffffff {
		return netip.Addr{}, false
	}

	return netip.AddrFrom4([4]byte{
		byte(total >> 24), byte(total >> 16), byte(total >> 8), byte(total),
	}), true
}

// parseNumericPart decodes one component in decimal, octal or hexadecimal.
func parseNumericPart(p string) (uint64, bool) {
	if p == "" {
		return 0, false
	}
	base := 10
	digits := p
	switch {
	case len(p) > 2 && (p[0] == '0') && (p[1] == 'x' || p[1] == 'X'):
		base, digits = 16, p[2:]
	case len(p) > 1 && p[0] == '0':
		base, digits = 8, p[1:]
	}
	v, err := strconv.ParseUint(digits, base, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
