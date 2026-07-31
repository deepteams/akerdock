// Scale-to-zero provisioning (ADR-036): deploy the agent helper (born akerdock-waker, renamed by ADR-056)
// container in front of a preview and maintain its shared routing table. The
// waker routes by Host, so an STZ preview's dynamic file just points its service
// at the waker — the protection middlewares (basic auth, noindex, SSO) are
// untouched, exactly as for a normal preview.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	"github.com/deepteams/akerdock/internal/agent"
	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/compose"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// wakerDir is the server-side directory holding the routing table and the
// per-resource activity files (ADR-036 §8.1).
const wakerDir = "/var/lib/akerdock/waker"

// wakerConfigFromRouteGroup derives the waker routing table from a RouteGroup
// whose endpoints still point at the REAL containers: each route's host maps to
// its container and port. wakeOrder is the resource's full wake set in stack
// start order (ADR-037 §5): every container of the stack, not just the routed
// ones — a stopped dependency (database, broker) loses its Docker DNS alias,
// so waking only the routed service boots it against a name that no longer
// resolves. Routed containers absent from wakeOrder are appended last: their
// dependencies wake first. An empty wakeOrder — a plain single-container app —
// falls back to the routed containers alone.
func wakerConfigFromRouteGroup(resourceUUID string, rg proxy.RouteGroup, wakeSet []agent.WakeContainer) agent.Config {
	seen := map[string]bool{}
	var containers []string
	var set []agent.WakeContainer
	add := func(c agent.WakeContainer) {
		if c.Container != "" && !seen[c.Container] {
			seen[c.Container] = true
			containers = append(containers, c.Container)
			set = append(set, c)
		}
	}
	for _, c := range wakeSet {
		add(c)
	}
	var routes []agent.Route
	var routed []string
	for _, rt := range rg.Routes {
		container := rt.Endpoint
		if container == "" {
			container = rg.Endpoint
		}
		if container == "" {
			container = rg.AppUUID
		}
		routes = append(routes, agent.Route{
			Host: rt.FQDN, ResourceUUID: resourceUUID, Container: container, Port: rt.TargetPort,
		})
		routed = append(routed, container)
	}
	sort.Strings(routed)
	for _, c := range routed {
		add(agent.WakeContainer{Container: c})
	}
	res := agent.Resource{UUID: resourceUUID, Containers: containers}
	// The dependency graph is only worth shipping when edges exist; a flat set
	// (plain app, routed-only fallback) wakes dependency-free either way.
	for _, c := range set {
		if len(c.Needs) > 0 {
			res.WakeSet = set
			break
		}
	}
	return agent.Config{
		Routes:    routes,
		Resources: []agent.Resource{res},
	}
}

// stackWakeSet is a compose stack's scale-to-zero wake set: every service
// container in topological start order (compose-spec §2.6), each carrying its
// depends_on edges container-resolved — so the waker reproduces `docker
// compose up`'s start behavior. One-shot jobs (restart:no, §7.3) are excluded
// from the set and from the edges — `docker start` on a completed job would
// re-run it, and its exited state would never count as ready.
func stackWakeSet(plan *compose.Plan) []agent.WakeContainer {
	oneShot := map[string]bool{}
	containerOf := map[string]string{}
	for _, sp := range plan.Services {
		containerOf[sp.Name] = sp.ContainerName
		if sp.OneShot {
			oneShot[sp.Name] = true
		}
	}
	var set []agent.WakeContainer
	for _, sp := range plan.Services {
		if sp.OneShot {
			continue
		}
		wc := agent.WakeContainer{Container: sp.ContainerName}
		for _, dep := range sp.DependsOn {
			if oneShot[dep.Service] {
				continue
			}
			if cn, ok := containerOf[dep.Service]; ok {
				wc.Needs = append(wc.Needs, cn)
			}
		}
		set = append(set, wc)
	}
	return set
}

// pointRouteGroupAtWaker rewrites every route to target the waker container, so
// GenerateDynamic emits a file that sends the preview's traffic to the waker
// (which forwards to the real container and wakes it on demand). TLS, priorities
// and the injected middlewares are untouched — the waker routes by Host.
func pointRouteGroupAtWaker(rg proxy.RouteGroup) proxy.RouteGroup {
	out := rg
	out.Endpoint = proxy.AgentContainerName
	out.Routes = make([]proxy.Route, len(rg.Routes))
	for i, rt := range rg.Routes {
		rt.Endpoint = proxy.AgentContainerName
		rt.TargetPort = proxy.AgentPort
		out.Routes[i] = rt
	}
	return out
}

// mergeWakerConfig replaces the entries of resourceUUID in base with add's,
// leaving every other resource intact — one waker serves every STZ resource on
// the server.
func mergeWakerConfig(base agent.Config, resourceUUID string, add agent.Config) agent.Config {
	out := removeWakerResource(base, resourceUUID)
	out.Routes = append(out.Routes, add.Routes...)
	out.Resources = append(out.Resources, add.Resources...)
	return out
}

// removeWakerResource drops every route and resource entry of resourceUUID.
func removeWakerResource(base agent.Config, resourceUUID string) agent.Config {
	var out agent.Config
	for _, r := range base.Routes {
		if r.ResourceUUID != resourceUUID {
			out.Routes = append(out.Routes, r)
		}
	}
	for _, r := range base.Resources {
		if r.UUID != resourceUUID {
			out.Resources = append(out.Resources, r)
		}
	}
	return out
}

// ensureAgent deploys the waker helper container (idempotent) and merges this
// resource's routes into the shared table. image is the AkerDock release image;
// empty is a configuration error, never a guessed registry. agentEnv carries
// the ADR-040 enrollment (instance URL + per-server token); zero-valued, the
// helper runs waker-only. The container converge stays SSH (bootstrap family,
// ADR-054 — it may be creating the agent itself); the routing table is a file
// under the mounted tree and rides the channel.
func ensureAgent(ctx context.Context, client *sshexec.Client, ops hostops.Ops, network, image, resourceUUID string, cfg agent.Config, agentEnv AgentEnv) error {
	if image == "" {
		return fmt.Errorf("scale_to_zero requires AKERDOCK_IMAGE to be set — the waker runs the AkerDock image")
	}
	if err := depositWakerRoutes(ctx, ops, mergeWakerConfig(readWakerConfig(ctx, ops), resourceUUID, cfg)); err != nil {
		return err
	}
	res, err := client.Run(ctx, AgentEnsureCommand(network, image, agentEnv))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("waker deploy failed (exit %d): %s", res.ExitCode, stderrOf(res))
	}
	return nil
}

// AgentEnv is the ADR-040 enrollment injected into the helper container at
// (re)creation: where to push observations, and the per-server credential.
// Either field empty disables the agent loop (degradation to SSH scans).
type AgentEnv struct {
	InstanceURL string
	Token       string
}

// AgentEnvForServer assembles the enrollment for one server — best-effort by
// design: an error yields a waker-only helper and a log line, never a failed
// deploy. The agent is an accelerator, not a dependency (ADR-040 §6).
func AgentEnvForServer(ctx context.Context, q AgentEnrollmentStore, keyring *envelope.Keyring,
	logger *slog.Logger, server store.Server, controlPlanePort int, instanceURL string,
) AgentEnv {
	url := AgentInstanceURL(ctx, q, server, controlPlanePort, instanceURL)
	if url == "" {
		return AgentEnv{}
	}
	token, err := EnsureAgentToken(ctx, q, keyring, server.ID)
	if err != nil {
		if logger != nil {
			logger.Warn("agent enrollment unavailable — helper deployed waker-only",
				"server_id", server.ID, "error", err)
		}
		return AgentEnv{}
	}
	return AgentEnv{InstanceURL: url, Token: token}
}

// agentSpec is the run-spec generation of the waker container. Bump it whenever
// the `docker run` flags change — or when the waker's own behavior changes and
// must reach servers whose image tag is unchanged (local "dirty" builds reuse a
// tag): the deploy recreates the container when EITHER the image OR this spec
// differs. 3: ordered wake set + rollback + waiting page, per-container budget.
// 4: agent enrollment env (ADR-040 phase 1). 5: host-gateway add-host — on a
// Linux host, host.docker.internal does not resolve without it, and the
// localhost server's agent could reach neither the channel nor the POST.
// 6: the ADR-052 command channel — the agent must offer akerdock-agent-v2,
// which only a recreated container running the new binary does.
// 7: the whole /var/lib/akerdock tree is mounted (ADR-054) — the agent
// executes the host file primitives on it; the waker's own subdirectory is
// unchanged inside the wider mount.
// 8: filters cross the channel as RawFilters — an executor without the fix
// decodes every label-scoped list/prune as UNFILTERED, and a sweep that
// arrives unfiltered force-removes the helper itself first (the 2026-07-31
// incident). Only a recreated container running the fixed binary is safe.
// 9: the rename (ADR-056) — the container becomes akerdock-agent (label
// akerdock.agent_spec), carrying the legacy name as a network alias so
// pre-rename scale-to-zero route files keep resolving.
const agentSpec = "9"

// AgentEnsureCommand is the idempotent deploy of the agent helper. It
// recreates the container when the running image OR the run spec differs (or
// when it is absent), and STARTS it either way: a helper stopped by hand
// (`docker stop` marks it so `unless-stopped` never relaunches it) would
// otherwise match image and spec and stay silent forever — the agent is
// mandatory, so a stopped helper is always wrong. The routing table and
// activity files live in a bind mount, so a recreate preserves them; the
// recreate also removes any container still bearing the pre-rename name
// (ADR-056), so a migrated server never runs two helpers.
//
// Deployed on the same internal network as the proxy (reachable as
// akerdock-agent:8080 — and by its legacy alias on user-defined networks,
// where pre-rename route files still point). It runs as root (--user 0)
// because it needs the local Docker socket — whose access is root-equivalent
// anyway, so the distroless nonroot default simply cannot read it. Shared by
// the deploy path (ensureAgent) and the scheduler's cross-server upgrade
// reconciliation. agentEnv (ADR-040) enrolls the channel loop; empty fields
// inject nothing and the helper runs waker-only.
func AgentEnsureCommand(network, image string, agentEnv AgentEnv) string {
	env := ""
	if agentEnv.InstanceURL != "" && agentEnv.Token != "" {
		env = fmt.Sprintf("-e AKERDOCK_INSTANCE_URL=%s -e AKERDOCK_AGENT_TOKEN=%s ",
			shellQuote(agentEnv.InstanceURL), shellQuote(agentEnv.Token))
	}
	// Network-scoped aliases only exist on user-defined networks; on the
	// default bridge (an observation-only server) there are no scale-to-zero
	// route files to keep resolving, so the alias is simply omitted.
	alias := ""
	if network != "bridge" {
		alias = "--network-alias " + proxy.LegacyAgentContainerName + " "
	}
	return fmt.Sprintf(
		"mkdir -p %s && "+
			"img=$(docker inspect -f '{{.Config.Image}}' %s 2>/dev/null || true); "+
			"spec=$(docker inspect -f '{{index .Config.Labels \"akerdock.agent_spec\"}}' %s 2>/dev/null || true); "+
			"if [ \"$img\" != \"%s\" ] || [ \"$spec\" != \"%s\" ]; then old_img=$img; "+
			"docker rm -f %s >/dev/null 2>&1 || true; docker rm -f %s >/dev/null 2>&1 || true; "+
			"docker run -d --name %s --restart unless-stopped --network %s --user 0 "+
			"%s"+
			"--add-host=host.docker.internal:host-gateway "+
			"-v /var/run/docker.sock:/var/run/docker.sock -v %s:%s "+
			"--label akerdock.managed=true --label akerdock.type=helper --label akerdock.agent_spec=%s "+
			"%s%s agent || exit $?; "+
			"if [ -n \"$old_img\" ] && [ \"$old_img\" != \"%s\" ]; then docker image rm \"$old_img\" >/dev/null 2>&1 || true; fi; fi; "+
			"docker start %s >/dev/null",
		wakerDir, proxy.AgentContainerName, proxy.AgentContainerName,
		image, agentSpec,
		proxy.AgentContainerName, proxy.LegacyAgentContainerName,
		proxy.AgentContainerName, network, alias,
		// The full akerdock tree, not just the waker's corner: the agent
		// executes the ADR-054 file primitives on it.
		hostops.Root, hostops.Root, agentSpec, env, image, image,
		proxy.AgentContainerName)
}

// removeWakerRoutes drops a resource from the shared table (preview destroy).
// The container stays: it still serves the server's other STZ resources.
func removeWakerRoutes(ctx context.Context, ops hostops.Ops, resourceUUID string) error {
	return depositWakerRoutes(ctx, ops, removeWakerResource(readWakerConfig(ctx, ops), resourceUUID))
}

// readWakerConfig reads the current routing table; absent or invalid → empty.
func readWakerConfig(ctx context.Context, ops hostops.Ops) agent.Config {
	var cfg agent.Config
	res, err := ops.ReadFile(ctx, agentwire.FileReadParams{Path: wakerDir + "/" + agent.RoutesFile})
	if err != nil || !res.Found {
		return cfg
	}
	_ = json.Unmarshal(res.Content, &cfg)
	return cfg
}

// depositWakerRoutes writes the routing table atomically, so the waker —
// which reloads on mtime change — never reads a half-written file.
func depositWakerRoutes(ctx context.Context, ops hostops.Ops, cfg agent.Config) error {
	raw, err := agent.MarshalConfig(cfg)
	if err != nil {
		return err
	}
	if err := ops.WriteFile(ctx, agentwire.FileWriteParams{
		Path: wakerDir + "/" + agent.RoutesFile, Content: raw,
		Mode: 0o600, MakeDirs: true, DirMode: 0o755, Atomic: true,
	}); err != nil {
		return fmt.Errorf("waker routes deposit failed: %s", firstLine(err.Error()))
	}
	return nil
}
