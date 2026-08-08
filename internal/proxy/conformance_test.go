package proxy_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/deepteams/akerdock/internal/proxy"
)

// Conformance fixtures (proxy-contract §9, ADR-009): the same IR, two providers.
// Traefik is P0; Caddy lands in P2 and will be held to these exact same cases —
// that is what makes the providers interchangeable rather than merely both
// present.
//
// Two properties are checked here:
//
//  1. The generated config matches the golden file byte for byte. Not a
//     stylistic wish: the engine applies a config only when its checksum
//     changed (§6.2), so a generator that reordered a map between runs would
//     rewrite the remote file forever and make drift detection meaningless.
//
//  2. The generator is deterministic across runs — generating twice from the
//     same IR yields the same bytes.
//
// `go test ./internal/proxy -update` refreshes the golden files. Reviewing that
// diff IS the review of a proxy change.

var update = flag.Bool("update", false, "rewrite the conformance golden files")

const fixturesDir = "../../tests/proxy-conformance/cases"

type fixture struct {
	Description string `json:"description"`
	IRVersion   int    `json:"ir_version"`
	Static      *struct {
		HTTPPort    int    `json:"http_port"`
		HTTPSPort   int    `json:"https_port"`
		ACMEEmail   string `json:"acme_email"`
		DNSProvider string `json:"dns_provider"`
		TCPPorts    []int  `json:"tcp_ports"`
		Revision    int64  `json:"revision"`
	} `json:"static"`
	Groups []struct {
		AppUUID        string `json:"app_uuid"`
		Endpoint       string `json:"endpoint"`
		ForceHTTPS     bool   `json:"force_https"`
		WildcardDomain string `json:"wildcard_domain"`
		DNSProvider    string `json:"dns_provider"`
		Revision       int64  `json:"revision"`
		Routes         []struct {
			FQDN       string `json:"fqdn"`
			Path       string `json:"path"`
			TargetPort int    `json:"target_port"`
		} `json:"routes"`
	} `json:"groups"`
	TCPRoutes []struct {
		ResourceUUID string `json:"resource_uuid"`
		ListenPort   int    `json:"listen_port"`
		TargetPort   int    `json:"target_port"`
		Revision     int64  `json:"revision"`
	} `json:"tcp_routes"`
	Ingress []struct {
		UUID           string `json:"uuid"`
		FQDN           string `json:"fqdn"`
		WildcardDomain string `json:"wildcard_domain"`
		DNSProvider    string `json:"dns_provider"`
		Revision       int64  `json:"revision"`
	} `json:"ingress"`
}

func TestTraefikConformance(t *testing.T) {
	cases, err := filepath.Glob(filepath.Join(fixturesDir, "*"))
	if err != nil || len(cases) == 0 {
		t.Fatalf("no conformance cases found in %s: %v", fixturesDir, err)
	}

	for _, dir := range cases {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			fx := loadFixture(t, filepath.Join(dir, "ir.json"))
			if fx.IRVersion != 1 {
				t.Fatalf("ir_version %d is not supported by this generator", fx.IRVersion)
			}

			if fx.Static != nil {
				got := proxy.GenerateStatic(fx.Static.HTTPPort, fx.Static.HTTPSPort, fx.Static.ACMEEmail, fx.Static.DNSProvider, fx.Static.TCPPorts, fx.Static.Revision)
				compare(t, filepath.Join(dir, "expected", "traefik", "traefik.yaml"), got)
			}

			for _, g := range fx.Groups {
				group := proxy.RouteGroup{
					AppUUID:        g.AppUUID,
					Endpoint:       g.Endpoint,
					ForceHTTPS:     g.ForceHTTPS,
					WildcardDomain: g.WildcardDomain,
					DNSProvider:    g.DNSProvider,
				}
				for _, r := range g.Routes {
					group.Routes = append(group.Routes, proxy.Route{
						FQDN: r.FQDN, Path: r.Path, TargetPort: r.TargetPort,
					})
				}

				got := proxy.GenerateDynamic(group, g.Revision)
				// Determinism: the checksum-based apply and the drift
				// reconciliation both rest on this.
				if again := proxy.GenerateDynamic(group, g.Revision); again != got {
					t.Fatal("the generator is not deterministic: the same IR produced different bytes twice — " +
						"the proxy would rewrite this file on every reconciliation pass (§6.2)")
				}
				compare(t, filepath.Join(dir, "expected", "traefik", "dynamic", g.AppUUID+".yaml"), got)
			}

			for _, tr := range fx.TCPRoutes {
				route := proxy.TCPRoute{
					ResourceUUID: tr.ResourceUUID, ListenPort: tr.ListenPort, TargetPort: tr.TargetPort,
				}
				got := proxy.GenerateTCP(route, tr.Revision)
				if again := proxy.GenerateTCP(route, tr.Revision); again != got {
					t.Fatal("the TCP generator is not deterministic")
				}
				compare(t, filepath.Join(dir, "expected", "traefik", "dynamic", tr.ResourceUUID+".yaml"), got)
			}

			// Ingress endpoints (ADR-060): a declared FQDN permanently pointed
			// at the agent, plus the reserved high-priority attach router the
			// laptop dials. The router is part of the routing contract, not an
			// implementation detail of the tunnel — a change to how the attach
			// path is served shows up here as a reviewable diff.
			for _, ing := range fx.Ingress {
				group := proxy.IngressGroup{
					UUID:           ing.UUID,
					FQDN:           ing.FQDN,
					WildcardDomain: ing.WildcardDomain,
					DNSProvider:    ing.DNSProvider,
				}
				got := proxy.GenerateIngress(group, ing.Revision)
				if again := proxy.GenerateIngress(group, ing.Revision); again != got {
					t.Fatal("the ingress generator is not deterministic")
				}
				compare(t, filepath.Join(dir, "expected", "traefik", "dynamic", ing.UUID+".yaml"), got)
			}

			// Caddy (P2): an absent expectation is reported, never silently
			// treated as a pass — a case nobody ported is a case nobody covers.
			if _, err := os.Stat(filepath.Join(dir, "expected", "caddy")); os.IsNotExist(err) {
				t.Logf("no Caddy expectation for this case — it is not ported yet (P2, ADR-009)")
			}
		})
	}
}

func loadFixture(t *testing.T, path string) fixture {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read the fixture: %v", err)
	}
	var fx fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("cannot parse %s: %v", path, err)
	}
	return fx
}

// compare checks the output against its golden file, or rewrites it under
// -update.
func compare(t *testing.T, golden, got string) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("missing golden file %s — run `go test ./internal/proxy -update` and review the diff: %v", golden, err)
	}
	if string(want) != got {
		t.Errorf("the generated config drifted from %s\n--- expected ---\n%s\n--- got ---\n%s", golden, want, got)
	}
}
