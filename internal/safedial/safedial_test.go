package safedial

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestBlocked(t *testing.T) {
	cases := map[string]bool{
		// Blocked: loopback, link-local (metadata), private, CGNAT, unspecified.
		"127.0.0.1":        true,
		"::1":              true,
		"169.254.169.254":  true, // cloud metadata
		"169.254.0.1":      true,
		"10.0.0.5":         true,
		"172.16.9.9":       true,
		"192.168.1.1":      true,
		"100.64.0.1":       true, // CGNAT
		"0.1.2.3":          true, // this network
		"192.0.2.1":        true, // documentation
		"198.51.100.1":     true, // documentation
		"203.0.113.1":      true, // documentation
		"240.0.0.1":        true, // reserved
		"0.0.0.0":          true,
		"fc00::1":          true, // IPv6 ULA
		"fe80::1":          true, // IPv6 link-local
		"64:ff9b::a00:1":   true, // NAT64 embedding 10.0.0.1
		"2002:0a00:0001::": true, // 6to4 embedding 10.0.0.1
		"2001:db8::1":      true, // documentation
		"224.0.0.1":        true, // multicast
		// Allowed: public unicast.
		"8.8.8.8":              false,
		"1.1.1.1":              false,
		"64:ff9b::808:808":     false, // NAT64 embedding public 8.8.8.8
		"2002:0808:0808::":     false, // 6to4 embedding public 8.8.8.8
		"2606:4700:4700::1111": false,
	}
	for ipStr, want := range cases {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			t.Fatalf("bad test IP %q", ipStr)
		}
		if got := Blocked(ip); got != want {
			t.Errorf("Blocked(%s) = %v, want %v", ipStr, got, want)
		}
	}
	// A nil IP is blocked (fail closed).
	if !Blocked(nil) {
		t.Error("Blocked(nil) = false, want true")
	}
}

func TestControl(t *testing.T) {
	if err := control("tcp", "169.254.169.254:80", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("control(metadata) = %v, want ErrBlockedAddress", err)
	}
	if err := control("tcp", "10.1.2.3:5432", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("control(private) = %v, want ErrBlockedAddress", err)
	}
	if err := control("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("control(public) = %v, want nil", err)
	}
	if err := control("tcp", "[fe80::1%en0]:80", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("control(scoped link-local) = %v, want ErrBlockedAddress", err)
	}
	if err := control("tcp", "not-an-ip:80", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("control(unclassified) = %v, want fail closed", err)
	}
}

func TestTransportCannotBeBypassedByProxyOrCustomTLSDialer(t *testing.T) {
	proxyURL, _ := url.Parse("http://proxy.example:8080")
	base := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{ServerName: "private-ca.example", MinVersion: tls.VersionTLS12},
		DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("must not run")
		},
	}
	hardened := Transport(base)
	if hardened == base {
		t.Fatal("Transport mutated and reused a possibly warm transport")
	}
	if hardened.Proxy != nil {
		t.Fatal("an HTTP proxy can relay a private destination around the dial guard")
	}
	if hardened.DialTLSContext != nil {
		t.Fatal("a custom TLS dialer can bypass the guarded DialContext")
	}
	if hardened.DialContext == nil {
		t.Fatal("guarded transport has no DialContext")
	}
	if base.Proxy == nil || base.DialTLSContext == nil {
		t.Fatal("Transport mutated its input")
	}
	if hardened.TLSClientConfig == base.TLSClientConfig ||
		hardened.TLSClientConfig.ServerName != base.TLSClientConfig.ServerName {
		t.Fatal("safe TLS configuration was not independently preserved")
	}
}

func TestHTTPClientRejectsLoopback(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	for _, target := range []string{srv.URL, "http://localhost:1"} {
		resp, err := HTTPClient(time.Second).Get(target)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("%s error = %v, want ErrBlockedAddress", target, err)
		}
	}
}

func TestHTTPClientRejectsPrivateRedirects(t *testing.T) {
	client := HTTPClient(time.Second)
	private, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if err := client.CheckRedirect(private, nil); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("metadata redirect error = %v, want ErrBlockedAddress", err)
	}
	public, _ := http.NewRequest(http.MethodGet, "https://example.com/health", nil)
	if err := client.CheckRedirect(public, nil); err != nil {
		t.Fatalf("public redirect rejected: %v", err)
	}
	via := make([]*http.Request, 10)
	if err := client.CheckRedirect(public, via); err == nil {
		t.Fatal("redirect ceiling was disabled by the SSRF policy")
	}
}
