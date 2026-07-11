// host.go implements the W3-05 egress gate's host canonicalization
// (HANDOFF §5 safety #1 flag: every off-box call is subject to a target
// allowlist). CanonicalizeHost normalizes a target host into the SAME
// form Gate compares against policy.yaml's egress.allowlist however
// either side happened to spell it: lowercased, trailing-dot-stripped,
// punycode ("xn--") labels decoded to their Unicode form, and an IP
// literal (v4, v6, or a bracketed "[::1]" form) normalized to net.IP's
// own canonical String() representation. NewGate (gate.go) canonicalizes
// every configured allowlist host through this exact same function once,
// at construction time, so Gate.Check's own per-request canonicalization
// always compares like with like — this is the mechanism behind the
// "case, punycode xn--, trailing dot" normalization acceptance
// criterion.
package egress

import (
	"fmt"
	"net"
	"strings"

	"golang.org/x/net/idna"
)

// CanonicalizeHost normalizes raw. raw == "" (after trimming) is an
// error — Gate.Check must never canonicalize an empty host into some
// default value that could accidentally match a real allowlist entry.
func CanonicalizeHost(raw string) (string, error) {
	h := strings.TrimSpace(raw)
	if h == "" {
		return "", fmt.Errorf("egress: empty host")
	}

	// A bracketed IPv6 literal ("[::1]") — strip the brackets before any
	// further processing (a plain "::1" has no brackets and needs none
	// stripped).
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}

	h = strings.ToLower(h)
	h = strings.TrimSuffix(h, ".")

	if ip := net.ParseIP(h); ip != nil {
		// Normalizes e.g. "192.168.001.001" -> "192.168.1.1", and any
		// IPv6 literal to its canonical shortened form, so a
		// numerically-equivalent-but-differently-written IP literal
		// cannot slip past an exact-string allowlist match either.
		return ip.String(), nil
	}

	// idna.ToUnicode decodes any "xn--" labels into their Unicode form; a
	// host with no such label round-trips unchanged. A malformed label is
	// not itself a canonicalization failure — fail-closed lives in the
	// ALLOWLIST match failing to find this string, not here — so the
	// lowercased/trimmed original is used as-is when idna cannot decode
	// it.
	if decoded, err := idna.ToUnicode(h); err == nil {
		h = decoded
	}
	return h, nil
}

// IsIPLiteral reports whether canonicalHost (already CanonicalizeHost'd)
// is an IP literal (v4 or v6) rather than a hostname.
func IsIPLiteral(canonicalHost string) bool {
	return net.ParseIP(canonicalHost) != nil
}

// privateCIDRs are the RFC1918 + fc00::/7 (ULA) ranges isPrivateOrLinkLocal
// checks in addition to net.IP's own IsLoopback/IsLinkLocalUnicast/
// IsLinkLocalMulticast (which already cover 127/8, 169.254/16, and their
// IPv6 equivalents).
var privateCIDRs = mustParseCIDRs(
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"fc00::/7",
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("egress: bad CIDR literal " + c + ": " + err.Error())
		}
		out = append(out, n)
	}
	return out
}

// isPrivateOrLinkLocal reports whether canonicalHost (already
// CanonicalizeHost'd) is an RFC1918/ULA, loopback (127/8, ::1), or
// link-local (169.254/16 and IPv6 equivalents) address — HANDOFF §5
// safety #1's "proxy-as-pivot into the LAN" gotcha.
//
// This does NOT gate anything on its own: Gate.Check denies an
// unmatched host via the ordinary "not in allowlist" rule regardless of
// whether it is a private/link-local literal — policy.yaml's allowlist
// never lists such an address by default, so the private-range test
// (proxying to 192.168.1.1 or 169.254.169.254 is denied even with no
// allowlist entry naming it) already passes by construction, as long as
// allowlist matching is an EXACT string match (never a prefix/substring/
// resolved-IP match) for every host, IP literal or not. isPrivateOrLinkLocal
// exists purely so Gate.Check can give a more specific Turkish reason
// string when denial is for this reason — an operator who explicitly
// adds such a literal address to policy.yaml's egress.allowlist (the
// "unless explicitly allowlisted" clause) still gets it through, exactly
// like any other explicitly allowlisted host.
//
// kahyad's own loopback listeners (the UDS control socket, each per-task
// anthproxy 127.0.0.1 listener, the local embed service) are never
// reached THROUGH this gate at all — they are direct in-process/loopback
// calls kahyad's own Go code makes without ever calling Gate.Check — so
// the HANDOFF gotcha's "127/8 except kahyad's own listeners" carve-out
// holds by construction, with no separate code path needed here.
func isPrivateOrLinkLocal(canonicalHost string) bool {
	ip := net.ParseIP(canonicalHost)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	for _, cidr := range privateCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}
