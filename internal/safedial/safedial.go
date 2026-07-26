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
// guard targets attacker-influenceable URLs — notably notification webhooks.
package safedial

import (
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned by the dialer when a connection targets a
// non-public address.
var ErrBlockedAddress = errors.New("blocked outbound address (SSRF guard)")

// extraBlocked are ranges that are routable-but-internal, so IsGlobalUnicast
// and IsPrivate do not catch them.
var extraBlocked = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"100.64.0.0/10", // CGNAT (Tailscale, cloud internal)
		"192.0.0.0/24",  // IETF protocol assignments
		"198.18.0.0/15", // benchmarking
	} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// Blocked reports whether an IP must not be dialed by a user-driven request.
func Blocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
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

// control is the net.Dialer.Control hook: it inspects the resolved IP the socket
// is about to connect to.
func control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	if ip := net.ParseIP(host); ip != nil && Blocked(ip) {
		return ErrBlockedAddress
	}
	return nil
}

// Dialer returns a net.Dialer that refuses non-public destinations.
func Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: control}
}

// Transport hardens an http.Transport's dials. base may be nil (a fresh
// transport) or an existing one to keep (e.g. with custom RootCAs); its
// DialContext is replaced by the guarded dialer.
func Transport(base *http.Transport) *http.Transport {
	t := base
	if t == nil {
		t = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	t.DialContext = Dialer(30 * time.Second).DialContext
	return t
}

// HTTPClient returns an http.Client whose connections are SSRF-guarded.
func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport(nil)}
}
