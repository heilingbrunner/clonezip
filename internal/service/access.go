package service

import (
	"fmt"
	"net/netip"
	"strings"
)

// defaultAllowActionsFrom is the allowlist used when the config omits
// "allowActionsFrom": the loopback interface only, so a service reachable on
// the LAN is a read-only dashboard to everyone but the machine it runs on.
//
// Both families are listed deliberately:
//
//   - ::1/128 because Linux and Windows both resolve "localhost" to ::1 ahead
//     of 127.0.0.1 (glibc follows RFC 6724, which prefers IPv6), so an
//     IPv4-only list would refuse the operator's own browser.
//   - the whole 127.0.0.0/8, not just 127.0.0.1, because Linux routes the
//     entire range to lo - 127.0.0.2 reaches the service just as well.
var defaultAllowActionsFrom = []string{"127.0.0.0/8", "::1/128"}

// ActionAllowList returns the client networks allowed to change anything, as
// parsed prefixes: the configured allowActionsFrom, or loopback when it is
// omitted. Entries that do not parse are skipped - Validate rejects those
// before a config gets this far, and narrowing the allowlist is the safe
// direction to fail in should one ever slip through.
func (c Config) ActionAllowList() []netip.Prefix {
	entries := c.AllowActionsFrom
	if len(entries) == 0 {
		entries = defaultAllowActionsFrom
	}

	out := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		p, err := parseAllowEntry(entry)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// EffectiveAllowActionsFrom returns the allowlist as configured, or the
// loopback default when it is omitted - what the startup banner reports, so
// the operator can see the policy actually in force.
func (c Config) EffectiveAllowActionsFrom() []string {
	if len(c.AllowActionsFrom) == 0 {
		return append([]string(nil), defaultAllowActionsFrom...)
	}
	return append([]string(nil), c.AllowActionsFrom...)
}

// ActionsAllowedFrom reports whether a client at addr may perform actions.
//
// addr is unmapped first: a dual-stack listener (which is what binding
// ":8091" produces on Linux) reports an IPv4 peer as the IPv4-mapped
// ::ffff:127.0.0.1, and netip.Prefix.Contains deliberately never matches a
// 4-in-6 address against an IPv4 prefix.
func ActionsAllowedFrom(allow []netip.Prefix, addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return false
	}
	for _, p := range allow {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// parseAllowEntry accepts either a CIDR block ("192.168.1.0/24") or a bare
// address ("192.168.1.7"), which is treated as a single host.
func parseAllowEntry(entry string) (netip.Prefix, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return netip.Prefix{}, fmt.Errorf("must not be empty")
	}

	if strings.Contains(entry, "/") {
		p, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not a valid CIDR block", entry)
		}
		// Masked() normalises an entry with host bits set, such as
		// "127.0.0.1/8", to the network it actually means - so what the guard
		// matches on is what a reader of the config would predict.
		return p.Masked(), nil
	}

	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf(
			"%q is not a valid IP address or CIDR block such as \"192.168.1.0/24\"", entry)
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}
