// Scale-to-zero provisioning (ADR-036): deploy the akerdock-waker helper
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

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/compose"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/waker"
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
func wakerConfigFromRouteGroup(resourceUUID string, rg proxy.RouteGroup, wakeSet []waker.WakeContainer) waker.Config {
	seen := map[string]bool{}
	var containers []string
	var set []waker.WakeContainer
	add := func(c waker.WakeContainer) {
		if c.Container != "" && !seen[c.Container] {
			seen[c.Container] = true
			containers = append(containers, c.Container)
			set = append(set, c)
		}
	}
	for _, c := range wakeSet {
		add(c)
	}
	var routes []waker.Route
	var routed []string
	for _, rt := range rg.Routes {
		container := rt.Endpoint
		if container == "" {
			container = rg.Endpoint
		}
		if container == "" {
			container = rg.AppUUID
		}
		routes = append(routes, waker.Route{
			Host: rt.FQDN, ResourceUUID: resourceUUID, Container: container, Port: rt.TargetPort,
		})
		routed = append(routed, container)
	}
	sort.Strings(routed)
	for _, c := range routed {
		add(waker.WakeContainer{Container: c})
	}
	res := waker.Resource{UUID: resourceUUID, Containers: containers}
	// The dependency graph is only worth shipping when edges exist; a flat set
	// (plain app, routed-only fallback) wakes dependency-free either way.
	for _, c := range set {
		if len(c.Needs) > 0 {
			res.WakeSet = set
			break
		}
	}
	return waker.Config{
		Routes:    routes,
		Resources: []waker.Resource{res},
	}
}

// stackWakeSet is a compose stack's scale-to-zero wake set: every service
// container in topological start order (compose-spec §2.6), each carrying its
// depends_on edges container-resolved — so the waker reproduces `docker
// compose up`'s start behavior. One-shot jobs (restart:no, §7.3) are excluded
// from the set and from the edges — `docker start` on a completed job would
// re-run it, and its exited state would never count as ready.
func stackWakeSet(plan *compose.Plan) []waker.WakeContainer {
	oneShot := map[string]bool{}
	containerOf := map[string]string{}
	for _, sp := range plan.Services {
		containerOf[sp.Name] = sp.ContainerName
		if sp.OneShot {
			oneShot[sp.Name] = true
		}
	}
	var set []waker.WakeContainer
	for _, sp := range plan.Services {
		if sp.OneShot {
			continue
		}
		wc := waker.WakeContainer{Container: sp.ContainerName}
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
	out.Endpoint = proxy.WakerContainerName
	out.Routes = make([]proxy.Route, len(rg.Routes))
	for i, rt := range rg.Routes {
		rt.Endpoint = proxy.WakerContainerName
		rt.TargetPort = proxy.WakerPort
		out.Routes[i] = rt
	}
	return out
}

// mergeWakerConfig replaces the entries of resourceUUID in base with add's,
// leaving every other resource intact — one waker serves every STZ resource on
// the server.
func mergeWakerConfig(base waker.Config, resourceUUID string, add waker.Config) waker.Config {
	out := removeWakerResource(base, resourceUUID)
	out.Routes = append(out.Routes, add.Routes...)
	out.Resources = append(out.Resources, add.Resources...)
	return out
}

// removeWakerResource drops every route and resource entry of resourceUUID.
func removeWakerResource(base waker.Config, resourceUUID string) waker.Config {
	var out waker.Config
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

// ensureWaker deploys the waker helper container (idempotent) and merges this
// resource's routes into the shared table. image is the AkerDock release image;
// empty is a configuration error, never a guessed registry. agentEnv carries
// the ADR-040 enrollment (instance URL + per-server token); zero-valued, the
// helper runs waker-only. The container converge stays SSH (bootstrap family,
// ADR-054 — it may be creating the agent itself); the routing table is a file
// under the mounted tree and rides the channel.
func ensureWaker(ctx context.Context, client *sshexec.Client, ops hostops.Ops, network, image, resourceUUID string, cfg waker.Config, agentEnv AgentEnv) error {
	if image == "" {
		return fmt.Errorf("scale_to_zero requires AKERDOCK_IMAGE to be set — the waker runs the AkerDock image")
	}
	if err := depositWakerRoutes(ctx, ops, mergeWakerConfig(readWakerConfig(ctx, ops), resourceUUID, cfg)); err != nil {
		return err
	}
	res, err := client.Run(ctx, WakerEnsureCommand(network, image, agentEnv))
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
	logger *slog.Logger, server store.Server, controlPlanePort int,
) AgentEnv {
	url := AgentInstanceURL(ctx, q, server, controlPlanePort)
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

// wakerSpec is the run-spec generation of the waker container. Bump it whenever
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
const wakerSpec = "7"

// WakerEnsureCommand is the idempotent deploy of the waker helper. It recreates
// the container when the running image OR the run spec differs (or when it is
// absent), otherwise a no-op — so it provisions the waker, upgrades it when the
// release image changes, and re-applies run-flag fixes when wakerSpec is bumped.
// The routing table and activity files live in a bind mount, so a recreate
// preserves them.
//
// Deployed on the same internal network as the proxy (reachable as
// akerdock-waker:8080, never published). It runs as root (--user 0) because it
// needs the local Docker socket — whose access is root-equivalent anyway, so the
// distroless nonroot default simply cannot read it, and every wake would fail.
// Shared by the deploy path (ensureWaker) and the scheduler's cross-server
// upgrade reconciliation. agentEnv (ADR-040) enrolls the agent loop; empty
// fields inject nothing and the helper runs waker-only.
func WakerEnsureCommand(network, image string, agentEnv AgentEnv) string {
	env := ""
	if agentEnv.InstanceURL != "" && agentEnv.Token != "" {
		env = fmt.Sprintf("-e AKERDOCK_INSTANCE_URL=%s -e AKERDOCK_AGENT_TOKEN=%s ",
			shellQuote(agentEnv.InstanceURL), shellQuote(agentEnv.Token))
	}
	return fmt.Sprintf(
		"mkdir -p %s && "+
			"img=$(docker inspect -f '{{.Config.Image}}' %s 2>/dev/null || true); "+
			"spec=$(docker inspect -f '{{index .Config.Labels \"akerdock.waker_spec\"}}' %s 2>/dev/null || true); "+
			"if [ \"$img\" != \"%s\" ] || [ \"$spec\" != \"%s\" ]; then old_img=$img; docker rm -f %s >/dev/null 2>&1 || true; "+
			"docker run -d --name %s --restart unless-stopped --network %s --user 0 "+
			"--add-host=host.docker.internal:host-gateway "+
			"-v /var/run/docker.sock:/var/run/docker.sock -v %s:%s "+
			"--label akerdock.managed=true --label akerdock.type=helper --label akerdock.waker_spec=%s "+
			"%s%s waker || exit $?; "+
			"if [ -n \"$old_img\" ] && [ \"$old_img\" != \"%s\" ]; then docker image rm \"$old_img\" >/dev/null 2>&1 || true; fi; fi",
		wakerDir, proxy.WakerContainerName, proxy.WakerContainerName,
		image, wakerSpec, proxy.WakerContainerName,
		// The full akerdock tree, not just the waker's corner: the agent
		// executes the ADR-054 file primitives on it.
		proxy.WakerContainerName, network, hostops.Root, hostops.Root, wakerSpec, env, image, image)
}

// removeWakerRoutes drops a resource from the shared table (preview destroy).
// The container stays: it still serves the server's other STZ resources.
func removeWakerRoutes(ctx context.Context, ops hostops.Ops, resourceUUID string) error {
	return depositWakerRoutes(ctx, ops, removeWakerResource(readWakerConfig(ctx, ops), resourceUUID))
}

// readWakerConfig reads the current routing table; absent or invalid → empty.
func readWakerConfig(ctx context.Context, ops hostops.Ops) waker.Config {
	var cfg waker.Config
	res, err := ops.ReadFile(ctx, agentwire.FileReadParams{Path: wakerDir + "/" + waker.RoutesFile})
	if err != nil || !res.Found {
		return cfg
	}
	_ = json.Unmarshal(res.Content, &cfg)
	return cfg
}

// depositWakerRoutes writes the routing table atomically, so the waker —
// which reloads on mtime change — never reads a half-written file.
func depositWakerRoutes(ctx context.Context, ops hostops.Ops, cfg waker.Config) error {
	raw, err := waker.MarshalConfig(cfg)
	if err != nil {
		return err
	}
	if err := ops.WriteFile(ctx, agentwire.FileWriteParams{
		Path: wakerDir + "/" + waker.RoutesFile, Content: raw,
		Mode: 0o600, MakeDirs: true, DirMode: 0o755, Atomic: true,
	}); err != nil {
		return fmt.Errorf("waker routes deposit failed: %s", firstLine(err.Error()))
	}
	return nil
}
