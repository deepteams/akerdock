package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// RealIP rewrites r.RemoteAddr to the address of the actual client when — and
// only when — the immediate peer is a proxy the operator declared trusted.
//
// Everything downstream reads the caller's address from RemoteAddr: the audit
// trail, the auth rate limiter, a token's CIDR allowlist, the row a tunnel or
// terminal session leaves behind. Behind a reverse proxy that address is the
// proxy's, so all of it degrades at once — every event attributed to one
// address, one shared rate-limit bucket for the whole internet, and an IP
// allowlist that admits everyone the proxy admits. Resolving it here, once, is
// what keeps every one of those sites correct without any of them knowing that
// a proxy exists.
//
// It is deliberately NOT "read X-Forwarded-For". That header is written by
// whoever speaks last, so trusting it unconditionally would let any client
// claim any address — rotate through rate-limit buckets, forge the address in
// an audit record, walk through a CIDR allowlist. The rule is the classic one:
//
//   - the peer itself (RemoteAddr) must be a trusted proxy, otherwise the
//     headers are ignored entirely;
//   - the forwarded chain is walked RIGHT to LEFT, skipping the addresses of
//     trusted proxies, and the first one that is not is the client. Everything
//     to its left was appended by that untrusted client and is not evidence.
//
// With no trusted proxy configured this middleware does nothing at all, which
// is the correct posture for a process exposed directly.
func RealIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(trusted) > 0 {
				if client, ok := clientFromForwarded(r, trusted); ok {
					// Still host:port, because every reader downstream splits
					// it and a bare IP would make them all fall back. The port
					// is 0: the client's real one never crossed the proxy, and
					// inventing the proxy's would pair two unrelated things.
					r = r.Clone(r.Context())
					r.RemoteAddr = net.JoinHostPort(client.String(), "0")
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientFromForwarded resolves the client behind the trusted hops, or reports
// false to leave RemoteAddr alone.
func clientFromForwarded(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	peer, ok := addrOf(r.RemoteAddr)
	if !ok || !within(peer, trusted) {
		// A direct client, or an unparseable peer: nothing to unwrap, and its
		// headers are its own claims.
		return netip.Addr{}, false
	}

	// Right to left: the last entry was appended by the nearest proxy and is
	// the most trustworthy, the first by whoever opened the connection.
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		addr, ok := addrOf(strings.TrimSpace(hops[i]))
		if !ok {
			continue
		}
		if within(addr, trusted) {
			continue // another hop of our own infrastructure
		}
		return addr, true
	}

	// No usable chain. X-Real-IP is what an nginx sets when it does not
	// maintain the chain; same trust condition, so it is read the same way.
	if addr, ok := addrOf(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ok {
		return addr, true
	}
	// Every hop was ours, or no header at all: the peer is the best answer,
	// and leaving RemoteAddr untouched says exactly that.
	return netip.Addr{}, false
}

// addrOf parses an address that may or may not carry a port, and unwraps the
// IPv4-in-IPv6 form so a CIDR written in v4 matches what a dual-stack proxy
// hands over.
func addrOf(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func within(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
