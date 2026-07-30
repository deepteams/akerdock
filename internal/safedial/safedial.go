// Package safedial hardens outbound HTTP against SSRF (PRD §23.3, ISO A.8.23):
// a request whose URL comes from a low-privilege user must never reach the
// control plane's own network — loopback, private ranges, link-local (including
// the cloud metadata endpoint 169.254.169.254), CGNAT space, or the unspecified
// address.
//
// The check runs in net.Dialer.Control — on the ACTUAL resolved IP at connect
// time, for every dial including each redirect hop — so it also defeats DNS
// rebinding: a name that resolves public on the first lookup and private on the
// second still connects to the private IP, which is exactly what Control sees.
//
// It is deliberately NOT applied to operator-configured infrastructure (the
// SMTP relay, the OTLP collector, the S3 endpoint, the OIDC issuer): those are
// set by the instance root and legitimately live on internal networks. The
// guard targets attacker-influenceable URLs — notably notification webhooks,
// MCP metadata documents and uptime probes.
package safedial

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned by the dialer when a connection targets a
// non-public address.
var ErrBlockedAddress = errors.New("blocked outbound address (SSRF guard)")

// extraBlocked are special-purpose ranges that IsGlobalUnicast and IsPrivate
// do not fully classify as non-public.
var extraBlocked = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"0.0.0.0/8",       // current network ("this host")
		"100.64.0.0/10",   // CGNAT (Tailscale, cloud internal)
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"192.88.99.0/24",  // deprecated 6to4 relay anycast
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved / limited broadcast
		"::/96",           // deprecated IPv4-compatible addresses
		"64:ff9b:1::/48",  // locally assigned NAT64 translation
		"100::/64",        // discard-only
		"2001::/23",       // IETF protocol/special-purpose assignments
		"2001:db8::/32",   // documentation
		"3fff::/20",       // documentation
		"5f00::/16",       // segment-routing SIDs
		"fec0::/10",       // deprecated site-local
	} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

var (
	nat64WellKnown = mustCIDR("64:ff9b::/96")
	sixToFour      = mustCIDR("2002::/16")
)

func mustCIDR(value string) *net.IPNet {
	_, network, err := net.ParseCIDR(value)
	if err != nil {
		panic(err)
	}
	return network
}

// Blocked reports whether an IP must not be dialed by a user-driven request.
func Blocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else if v6 := ip.To16(); v6 != nil {
		// Translation/tunnel ranges can carry an RFC1918 or link-local IPv4
		// address inside an otherwise global-looking IPv6 address. Validate the
		// embedded destination too, or NAT64/6to4 becomes an SSRF bypass.
		switch {
		case nat64WellKnown.Contains(v6):
			if Blocked(net.IPv4(v6[12], v6[13], v6[14], v6[15])) {
				return true
			}
		case sixToFour.Contains(v6):
			if Blocked(net.IPv4(v6[2], v6[3], v6[4], v6[5])) {
				return true
			}
		}
	}
	// !IsGlobalUnicast catches loopback, link-local (incl. metadata), multicast
	// and the unspecified address; IsPrivate adds RFC 1918 and IPv6 ULA.
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return true
	}
	for _, n := range extraBlocked {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func parsedAddress(address string) net.IP {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	return parsedHost(host)
}

func parsedHost(host string) net.IP {
	// A scoped IPv6 address is presented as fe80::1%zone. net.ParseIP rejects
	// the zone suffix, which must not turn a link-local address into "unknown
	// therefore allowed".
	if i := strings.LastIndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return net.ParseIP(host)
}

// control is the net.Dialer.Control hook: it inspects the resolved IP the socket
// is about to connect to.
func control(_, address string, _ syscall.RawConn) error {
	ip := parsedAddress(address)
	// Control receives a resolved IP for TCP. If that invariant ever changes,
	// fail closed instead of silently allowing an unclassified destination.
	if ip == nil || Blocked(ip) {
		return ErrBlockedAddress
	}
	return nil
}

// Dialer returns a net.Dialer that refuses non-public destinations.
func Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: control}
}

// Transport builds a hardened http.Transport. base may provide a custom TLS
// configuration (e.g. private RootCAs); no routing/dial behavior is inherited.
//
// Proxies and custom TLS dialers are deliberately cleared: either could relay
// the request without passing the actual destination through Control. A fresh
// transport also cannot reuse a connection opened by the unhardened base.
func Transport(base *http.Transport) *http.Transport {
	// Start from known-safe standard values rather than cloning the whole base:
	// a clone would also inherit deprecated/custom dial paths and alternate
	// protocols which need not pass through DialContext.
	t := &http.Transport{
		DialContext:           Dialer(30 * time.Second).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if base != nil && base.TLSClientConfig != nil {
		t.TLSClientConfig = base.TLSClientConfig.Clone()
	}
	return t
}

// HTTPClient returns an http.Client whose connections are SSRF-guarded.
func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     Transport(nil),
		CheckRedirect: safeRedirect,
	}
}

func safeRedirect(req *http.Request, via []*http.Request) error {
	// Preserve net/http's default redirect ceiling when installing this policy.
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return ErrBlockedAddress
	}
	// Reject literal private hops before dialing. Hostnames are deliberately
	// checked later by Control, against the address DNS actually returned.
	if ip := parsedHost(req.URL.Hostname()); ip != nil && Blocked(ip) {
		return ErrBlockedAddress
	}
	return nil
}
