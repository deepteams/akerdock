package jobs

import (
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/agent"
	"github.com/deepteams/akerdock/internal/compose"
	"github.com/deepteams/akerdock/internal/proxy"
)

func TestWakerConfigFromRouteGroup(t *testing.T) {
	rg := proxy.RouteGroup{
		AppUUID: "pv-1",
		Routes: []proxy.Route{
			{FQDN: "api-pv.example.com", Path: "/", TargetPort: 8080, Endpoint: "pv-1-api"},
			{FQDN: "web-pv.example.com", Path: "/", TargetPort: 3000, Endpoint: "pv-1-web"},
			// A second route to the same container must not duplicate the wake set.
			{FQDN: "web2-pv.example.com", Path: "/", TargetPort: 3000, Endpoint: "pv-1-web"},
		},
	}
	cfg := wakerConfigFromRouteGroup("pv-1", rg, nil)

	if len(cfg.Routes) != 3 {
		t.Fatalf("routes = %d, want 3", len(cfg.Routes))
	}
	if len(cfg.Resources) != 1 || cfg.Resources[0].UUID != "pv-1" {
		t.Fatalf("resources = %#v", cfg.Resources)
	}
	// Distinct containers only, sorted.
	got := cfg.Resources[0].Containers
	if len(got) != 2 || got[0] != "pv-1-api" || got[1] != "pv-1-web" {
		t.Fatalf("wake set = %v, want [pv-1-api pv-1-web]", got)
	}
	// Each route maps its host to the real container:port.
	byHost := map[string]agent.Route{}
	for _, r := range cfg.Routes {
		byHost[r.Host] = r
	}
	if r := byHost["api-pv.example.com"]; r.Container != "pv-1-api" || r.Port != 8080 || r.ResourceUUID != "pv-1" {
		t.Fatalf("api route = %#v", r)
	}
}

func TestWakerConfigSingleContainerFallback(t *testing.T) {
	// No per-route endpoint: the container is the group endpoint (single app).
	rg := proxy.RouteGroup{
		AppUUID:  "pv-2",
		Endpoint: "pv-2",
		Routes:   []proxy.Route{{FQDN: "pv2.example.com", Path: "/", TargetPort: 80}},
	}
	cfg := wakerConfigFromRouteGroup("pv-2", rg, nil)
	if cfg.Routes[0].Container != "pv-2" || cfg.Resources[0].Containers[0] != "pv-2" {
		t.Fatalf("single-container fallback wrong: %#v / %#v", cfg.Routes[0], cfg.Resources[0])
	}
}

func TestWakerConfigWakeSetIncludesDependencies(t *testing.T) {
	// A compose stack: only `web` is routed, but the wake set must carry the
	// whole stack in start order — a stopped dependency loses its DNS alias,
	// so waking `web` alone boots it against a name that no longer resolves.
	rg := proxy.RouteGroup{
		AppUUID: "app-1",
		Routes:  []proxy.Route{{FQDN: "app.example.com", Path: "/", TargetPort: 8080, Endpoint: "app-1-web"}},
	}
	cfg := wakerConfigFromRouteGroup("app-1", rg, []agent.WakeContainer{
		{Container: "app-1-nats"},
		{Container: "app-1-postgres"},
		{Container: "app-1-web", Needs: []string{"app-1-nats", "app-1-postgres"}},
	})

	got := cfg.Resources[0].Containers
	want := []string{"app-1-nats", "app-1-postgres", "app-1-web"}
	if len(got) != len(want) {
		t.Fatalf("wake set = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wake set = %v, want %v (start order preserved)", got, want)
		}
	}
	// The dependency graph ships with the config.
	ws := cfg.Resources[0].WakeSet
	if len(ws) != 3 || ws[2].Container != "app-1-web" || len(ws[2].Needs) != 2 {
		t.Fatalf("wake graph = %#v, want web needing nats+postgres", ws)
	}
}

func TestWakerConfigRoutedContainerAppendedWhenMissingFromSet(t *testing.T) {
	// A routed container absent from the declared set (defensive: a stale
	// plan) is appended LAST, dependency-free.
	rg := proxy.RouteGroup{
		AppUUID: "app-1",
		Routes:  []proxy.Route{{FQDN: "app.example.com", Path: "/", TargetPort: 8080, Endpoint: "app-1-web"}},
	}
	cfg := wakerConfigFromRouteGroup("app-1", rg, []agent.WakeContainer{{Container: "app-1-nats"}})

	got := cfg.Resources[0].Containers
	if len(got) != 2 || got[0] != "app-1-nats" || got[1] != "app-1-web" {
		t.Fatalf("wake set = %v, want [app-1-nats app-1-web]", got)
	}
}

func TestStackWakeSetSkipsOneShotAndResolvesDeps(t *testing.T) {
	plan := &compose.Plan{Services: []compose.ServicePlan{
		{Name: "nats", ContainerName: "app-1-nats"},
		{
			Name: "migrate", ContainerName: "app-1-migrate", OneShot: true,
			DependsOn: []compose.Dependency{{Service: "nats", Condition: "service_started"}},
		},
		{Name: "web", ContainerName: "app-1-web", DependsOn: []compose.Dependency{
			{Service: "nats", Condition: "service_started"},
			{Service: "migrate", Condition: "service_completed_successfully"},
		}},
		{Name: "worker", ContainerName: "app-1-worker", DependsOn: []compose.Dependency{
			{Service: "nats", Condition: "service_started"},
		}},
	}}
	got := stackWakeSet(plan)
	if len(got) != 3 || got[0].Container != "app-1-nats" || got[1].Container != "app-1-web" || got[2].Container != "app-1-worker" {
		t.Fatalf("wake set = %#v, want [nats web worker] (one-shot excluded, order kept)", got)
	}
	// web's edge to the one-shot migrate job is dropped (it ran at deploy);
	// its edge to nats is container-resolved. worker does NOT depend on web.
	if len(got[1].Needs) != 1 || got[1].Needs[0] != "app-1-nats" {
		t.Fatalf("web needs = %v, want [app-1-nats] only", got[1].Needs)
	}
	if len(got[2].Needs) != 1 || got[2].Needs[0] != "app-1-nats" {
		t.Fatalf("worker needs = %v, want [app-1-nats] — independent of web", got[2].Needs)
	}
}

func TestAgentEnsureCommandAgentEnv(t *testing.T) {
	// With an enrollment, the run command injects the ADR-040 env, quoted;
	// without one, no -e flag at all (waker-only helper).
	with := AgentEnsureCommand("net", "img:1", AgentEnv{InstanceURL: "https://akerdock.example.com", Token: "akda_abc"})
	if !strings.Contains(with, "-e AKERDOCK_INSTANCE_URL='https://akerdock.example.com'") ||
		!strings.Contains(with, "-e AKERDOCK_AGENT_TOKEN='akda_abc'") {
		t.Fatalf("enrollment env missing from the ensure command:\n%s", with)
	}
	// Linux hosts do not resolve host.docker.internal without the gateway
	// alias — without it the localhost server's agent can reach nothing.
	if !strings.Contains(with, "--add-host=host.docker.internal:host-gateway") {
		t.Fatalf("host-gateway alias missing from the ensure command:\n%s", with)
	}
	if !strings.Contains(with, `docker image rm "$old_img"`) ||
		!strings.Contains(with, "agent || exit $?") {
		t.Fatalf("agent replacement does not safely reclaim the old helper image:\n%s", with)
	}
	// The ADR-056 rename mechanics: the container runs under the NEW name
	// with its own spec label, removes any container still bearing the
	// pre-rename name (never two helpers), and carries the legacy name as a
	// network alias so pre-rename scale-to-zero route files keep resolving.
	if !strings.Contains(with, "--name "+proxy.AgentContainerName+" ") ||
		!strings.Contains(with, "akerdock.agent_spec=") {
		t.Fatalf("the helper must run under the renamed identity:\n%s", with)
	}
	if !strings.Contains(with, "docker rm -f "+proxy.LegacyAgentContainerName+" >/dev/null") {
		t.Fatalf("the recreate must retire the pre-rename container:\n%s", with)
	}
	if !strings.Contains(with, "--network-alias "+proxy.LegacyAgentContainerName+" ") {
		t.Fatalf("the legacy DNS alias is missing — pre-rename route files would 502:\n%s", with)
	}
	// The default bridge refuses network-scoped aliases; an observation-only
	// server has no route files to keep resolving, so the alias is omitted.
	if bridge := AgentEnsureCommand("bridge", "img:1", AgentEnv{}); strings.Contains(bridge, "--network-alias") {
		t.Fatalf("the bridge network must not carry an alias:\n%s", bridge)
	}
	// A helper stopped by hand (`docker stop` marks it so `unless-stopped`
	// never relaunches it) matches image and spec: without a terminal
	// `docker start`, the ensure would be a no-op and the agent silent
	// forever. The start is UNCONDITIONAL and strict — a helper that cannot
	// start must fail the ensure, never pass silently.
	if !strings.Contains(with, "fi; docker start "+proxy.AgentContainerName+" >/dev/null") {
		t.Fatalf("the ensure must converge a stopped helper back to running:\n%s", with)
	}
	without := AgentEnsureCommand("net", "img:1", AgentEnv{})
	if strings.Contains(without, "AKERDOCK_INSTANCE_URL") || strings.Contains(without, "-e ") {
		t.Fatalf("empty enrollment must inject nothing:\n%s", without)
	}
}

func TestPointRouteGroupAtWaker(t *testing.T) {
	rg := proxy.RouteGroup{
		AppUUID:  "pv-1",
		Endpoint: "pv-1-web",
		Routes: []proxy.Route{
			{FQDN: "a.example.com", Path: "/", TargetPort: 3000, Endpoint: "pv-1-web"},
		},
	}
	out := pointRouteGroupAtWaker(rg)
	if out.Endpoint != proxy.AgentContainerName {
		t.Fatalf("group endpoint = %q, want waker", out.Endpoint)
	}
	for _, r := range out.Routes {
		if r.Endpoint != proxy.AgentContainerName || r.TargetPort != proxy.AgentPort {
			t.Fatalf("route not pointed at waker: %#v", r)
		}
	}
	// The original must be untouched (value copy, not aliased).
	if rg.Routes[0].Endpoint != "pv-1-web" || rg.Routes[0].TargetPort != 3000 {
		t.Fatalf("original RouteGroup mutated: %#v", rg.Routes[0])
	}
}

func TestMergeAndRemoveWakerConfig(t *testing.T) {
	base := agent.Config{
		Routes: []agent.Route{
			{Host: "other.example.com", ResourceUUID: "other", Container: "other", Port: 80},
			{Host: "old-pv1.example.com", ResourceUUID: "pv-1", Container: "pv-1-old", Port: 80},
		},
		Resources: []agent.Resource{
			{UUID: "other", Containers: []string{"other"}},
			{UUID: "pv-1", Containers: []string{"pv-1-old"}},
		},
	}
	add := agent.Config{
		Routes:    []agent.Route{{Host: "new-pv1.example.com", ResourceUUID: "pv-1", Container: "pv-1-new", Port: 3000}},
		Resources: []agent.Resource{{UUID: "pv-1", Containers: []string{"pv-1-new"}}},
	}

	merged := mergeWakerConfig(base, "pv-1", add)
	// pv-1's old entries replaced, other's untouched.
	hosts := map[string]bool{}
	for _, r := range merged.Routes {
		hosts[r.Host] = true
	}
	if hosts["old-pv1.example.com"] {
		t.Error("stale pv-1 route survived the merge")
	}
	if !hosts["new-pv1.example.com"] || !hosts["other.example.com"] {
		t.Errorf("merge dropped a live route: %v", hosts)
	}
	if len(merged.Resources) != 2 {
		t.Errorf("resources = %d, want 2", len(merged.Resources))
	}

	removed := removeWakerResource(merged, "pv-1")
	for _, r := range removed.Routes {
		if r.ResourceUUID == "pv-1" {
			t.Error("pv-1 route survived removal")
		}
	}
	if len(removed.Resources) != 1 || removed.Resources[0].UUID != "other" {
		t.Errorf("resources after removal = %#v", removed.Resources)
	}
}
