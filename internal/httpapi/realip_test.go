package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func prefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("bad fixture %q: %v", c, err)
		}
		out = append(out, p)
	}
	return out
}

// seenBy runs the middleware and reports the address every downstream reader
// would see — the audit trail, the limiters, a token's CIDR allowlist.
func seenBy(t *testing.T, trusted []netip.Prefix, peer string, headers map[string][]string) string {
	t.Helper()
	var got string
	h := RealIP(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = ClientIPKey(r)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = peer
	for k, values := range headers {
		for _, v := range values {
			r.Header.Add(k, v)
		}
	}
	h.ServeHTTP(httptest.NewRecorder(), r)
	return got
}

// The header is evidence only when the peer that wrote it is one of ours. This
// is the whole security property: without it, any client could claim any
// address and walk through a CIDR allowlist, forge an audit record, or rotate
// through rate-limit buckets by rotating a string.
func TestRealIPBelievesOnlyTrustedPeers(t *testing.T) {
	docker := prefixes(t, "172.18.0.0/16")
	xff := map[string][]string{"X-Forwarded-For": {"203.0.113.7"}}

	cases := map[string]struct {
		trusted []netip.Prefix
		peer    string
		headers map[string][]string
		want    string
	}{
		"behind the declared proxy": {docker, "172.18.0.1:5000", xff, "203.0.113.7"},
		// The claim of a client we talk to directly is just a claim.
		"a direct client claiming an address": {docker, "198.51.100.9:5000", xff, "198.51.100.9"},
		// Nothing declared: the feature is off, and the socket is the truth.
		"no trusted proxy configured": {nil, "172.18.0.1:5000", xff, "172.18.0.1"},
		// A proxy that forwards nothing leaves the peer as the best answer —
		// never an empty or invented address.
		"a trusted proxy with no header": {docker, "172.18.0.1:5000", nil, "172.18.0.1"},
		// nginx's other convention, same trust condition.
		"x-real-ip alone": {
			docker, "172.18.0.1:5000",
			map[string][]string{"X-Real-IP": {"203.0.113.7"}},
			"203.0.113.7",
		},
		// The chain is walked right to left: the last entry is the nearest
		// proxy's word, the first is whatever the client typed.
		"a client that prepended a lie": {
			docker, "172.18.0.1:5000",
			map[string][]string{"X-Forwarded-For": {"10.9.9.9, 203.0.113.7"}},
			"203.0.113.7",
		},
		// Several hops of our own infrastructure are skipped, not counted.
		"two of our own hops": {
			prefixes(t, "172.18.0.0/16", "10.0.0.0/8"), "172.18.0.1:5000",
			map[string][]string{"X-Forwarded-For": {"203.0.113.7, 10.0.0.5", "172.18.0.9"}},
			"203.0.113.7",
		},
		// Every hop trusted and no client behind them: attributing the request
		// to the first hop would be inventing a client.
		"only our own addresses in the chain": {
			docker, "172.18.0.1:5000",
			map[string][]string{"X-Forwarded-For": {"172.18.0.9"}},
			"172.18.0.1",
		},
		// Garbage in the header is not an escape hatch.
		"an unparseable claim": {
			docker, "172.18.0.1:5000",
			map[string][]string{"X-Forwarded-For": {"not-an-ip"}},
			"172.18.0.1",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := seenBy(t, tc.trusted, tc.peer, tc.headers); got != tc.want {
				t.Errorf("client address = %q, want %q", got, tc.want)
			}
		})
	}
}

// Downstream readers all do SplitHostPort and fall back to the raw string on
// failure — a bare IP would quietly break the audit trail's parse. The rewrite
// must keep the shape net/http produces.
func TestRealIPKeepsRemoteAddrSplittable(t *testing.T) {
	var raw string
	h := RealIP(prefixes(t, "172.18.0.0/16"))(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		raw = r.RemoteAddr
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.18.0.1:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if _, err := netip.ParseAddrPort(raw); err != nil {
		t.Fatalf("RemoteAddr = %q, want a host:port pair (%v)", raw, err)
	}
}

// A dual-stack proxy hands over ::ffff:10.0.0.5 for a v4 hop; the operator
// wrote 10.0.0.0/8. Both must mean the same address.
func TestRealIPUnmapsIPv4InIPv6(t *testing.T) {
	got := seenBy(t, prefixes(t, "172.18.0.0/16", "10.0.0.0/8"), "[::ffff:172.18.0.1]:5000",
		map[string][]string{"X-Forwarded-For": {"203.0.113.7, ::ffff:10.0.0.5"}})
	if got != "203.0.113.7" {
		t.Errorf("client address = %q, want 203.0.113.7", got)
	}
}
