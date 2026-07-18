package jobs

import (
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/store"
)

// The proxy of a fresh server must never start as a side effect of a
// background job: the FIRST start is the operator's explicit action, after
// they reviewed the proxy settings (§20.1 step 5). This table is the whole
// contract of that decision.
func TestProxyBootstrapDecision(t *testing.T) {
	cases := []struct {
		name    string
		server  store.Server
		wantRun bool
		reason  string // substring the skip message must carry
	}{
		{
			name: "build server never gets a proxy",
			server: store.Server{
				IsBuildServer:     true,
				ProxyType:         store.ProxyTypeTraefik,
				ProxyDesiredState: store.ProxyDesiredStateRunning,
			},
			wantRun: false,
			reason:  "build server",
		},
		{
			name: "proxy_type none routes nothing",
			server: store.Server{
				ProxyType:         store.ProxyTypeNone,
				ProxyDesiredState: store.ProxyDesiredStateRunning,
			},
			wantRun: false,
			reason:  "proxy_type is none",
		},
		{
			name: "intent stopped: the first start belongs to the operator",
			server: store.Server{
				ProxyType:         store.ProxyTypeTraefik,
				ProxyDesiredState: store.ProxyDesiredStateStopped,
			},
			wantRun: false,
			reason:  "proxy intent is stopped",
		},
		{
			name: "intent running: validation converges the proxy",
			server: store.Server{
				ProxyType:         store.ProxyTypeTraefik,
				ProxyDesiredState: store.ProxyDesiredStateRunning,
			},
			wantRun: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, reason := proxyBootstrapDecision(tc.server)
			if run != tc.wantRun {
				t.Fatalf("run = %v, want %v", run, tc.wantRun)
			}
			if !run && !strings.Contains(reason, tc.reason) {
				t.Fatalf("skip reason %q does not mention %q", reason, tc.reason)
			}
			if run && reason != "" {
				t.Fatalf("a positive decision must not carry a skip reason, got %q", reason)
			}
		})
	}
}
