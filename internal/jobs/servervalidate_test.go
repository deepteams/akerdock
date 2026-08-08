package jobs

import (
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/sshexec"
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

// The single most common first-start failure is a port already taken by an
// existing web server: the operator must get the remediation, not a raw
// Docker bind error.
func TestPortConflictHint(t *testing.T) {
	conflict := &sshexec.Result{Stderr: "docker: Error response from daemon: driver failed programming external connectivity on endpoint akerdock-proxy: Bind for 0.0.0.0:80 failed: port is already allocated."}
	if hint := portConflictHint(conflict, 80, 443); !strings.Contains(hint, "proxy_http_port") || !strings.Contains(hint, "80") {
		t.Fatalf("the bind conflict must carry its remediation, got %q", hint)
	}
	inUse := &sshexec.Result{Stdout: "Error starting userland proxy: listen tcp4 0.0.0.0:443: bind: address already in use"}
	if hint := portConflictHint(inUse, 8080, 8443); !strings.Contains(hint, "8443") {
		t.Fatalf("address-already-in-use must carry its remediation, got %q", hint)
	}
	if hint := portConflictHint(&sshexec.Result{Stderr: "no space left on device"}, 80, 443); hint != "" {
		t.Fatalf("an unrelated failure must not claim a port conflict, got %q", hint)
	}
	if hint := portConflictHint(nil, 80, 443); hint != "" {
		t.Fatalf("nil result must yield no hint, got %q", hint)
	}
}

func TestProxyPortPublishArgsIncludesHTTPSUDP(t *testing.T) {
	got := proxyPortPublishArgs(8080, 8443, []int{15432})
	want := "-p 8080:8080 -p 8443:8443 -p 8443:8443/udp -p 15432:15432 "
	if got != want {
		t.Fatalf("publish args = %q, want %q", got, want)
	}
}

// Traefik reads its static file once, at startup, and `docker run` publishes
// its ports once, at creation: a proxy created before an AkerDock upgrade
// keeps the old entrypoints, timeouts and ports until something replaces the
// container. Drift is what decides that, so it must not be silent — and must
// not fire on a file that already matches, or every validation would recreate
// the proxy.
func TestProxyStaticDriftDecidesRecreation(t *testing.T) {
	static := proxy.GenerateStatic(80, 443, "ops@example.com", "", nil, 7)
	read := func(fileContent string) *sshexec.Result {
		return &sshexec.Result{Stdout: proxyStaticBeginMarker + fileContent + proxyStaticEndMarker}
	}
	if proxyStaticDrifted(read(static), static) {
		t.Fatal("an identical deployed config must not recreate the proxy")
	}
	// A chatty login shell is not drift: recreating on it would cut the
	// traffic of every server on every validation.
	noisy := &sshexec.Result{Stdout: "Welcome to Ubuntu\n" + proxyStaticBeginMarker + static + proxyStaticEndMarker + "\nbye"}
	if proxyStaticDrifted(noisy, static) {
		t.Fatal("shell noise around the markers must not count as drift")
	}
	stale := read(strings.ReplaceAll(static, "readTimeout: 0s", "readTimeout: 60s"))
	if !proxyStaticDrifted(stale, static) {
		t.Fatal("a deployed config that drifted must recreate the proxy")
	}
	if !proxyStaticDrifted(read(""), static) {
		t.Fatal("a missing config file must recreate the proxy")
	}
	if !proxyStaticDrifted(&sshexec.Result{Stdout: "connection reset"}, static) {
		t.Fatal("an unreadable config must recreate the proxy")
	}
	if !proxyStaticDrifted(nil, static) {
		t.Fatal("a missing result must recreate the proxy")
	}
}

// The instance FQDN rides the proxy of exactly one server — the localhost
// server that hosts the instance (PRD §14.2). Every other combination must
// yield no route, and the content must be stable so bootstrap can compare it
// with the last applied revision and skip a no-op re-apply.
func TestControlPlaneRouteContent(t *testing.T) {
	host := store.Server{ID: 3, IsLocalhost: true}
	remote := store.Server{ID: 4, IsLocalhost: false}

	content := controlPlaneRouteContent(host, "manager.example.com", 8080)
	for _, want := range []string{
		"Host(`manager.example.com`)",
		"00-control-plane-r0",
		"http://host.docker.internal:8080",
		"certResolver: http01",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("control-plane route missing %q\n%s", want, content)
		}
	}
	if content != controlPlaneRouteContent(host, "manager.example.com", 8080) {
		t.Fatal("content must be deterministic across bootstraps")
	}

	for name, got := range map[string]string{
		"remote server": controlPlaneRouteContent(remote, "manager.example.com", 8080),
		"no FQDN":       controlPlaneRouteContent(host, "", 8080),
		"unknown port":  controlPlaneRouteContent(host, "manager.example.com", 0),
	} {
		if got != "" {
			t.Errorf("%s: want no route, got\n%s", name, got)
		}
	}
}
