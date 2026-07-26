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
	"sort"

	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/waker"
)

// wakerDir is the server-side directory holding the routing table and the
// per-resource activity files (ADR-036 §8.1).
const wakerDir = "/var/lib/akerdock/waker"

// wakerConfigFromRouteGroup derives the waker routing table from a RouteGroup
// whose endpoints still point at the REAL containers: each route's host maps to
// its container and port, and the resource's wake set is the distinct set of
// those containers.
func wakerConfigFromRouteGroup(resourceUUID string, rg proxy.RouteGroup) waker.Config {
	seen := map[string]bool{}
	var containers []string
	var routes []waker.Route
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
		if !seen[container] {
			seen[container] = true
			containers = append(containers, container)
		}
	}
	sort.Strings(containers)
	return waker.Config{
		Routes:    routes,
		Resources: []waker.Resource{{UUID: resourceUUID, Containers: containers}},
	}
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
// empty is a configuration error, never a guessed registry.
func ensureWaker(ctx context.Context, client *sshexec.Client, network, image, resourceUUID string, cfg waker.Config) error {
	if image == "" {
		return fmt.Errorf("scale_to_zero requires AKERDOCK_IMAGE to be set — the waker runs the AkerDock image")
	}
	if err := depositWakerRoutes(ctx, client, mergeWakerConfig(readWakerConfig(ctx, client), resourceUUID, cfg)); err != nil {
		return err
	}
	res, err := client.Run(ctx, WakerEnsureCommand(network, image))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("waker deploy failed (exit %d): %s", res.ExitCode, stderrOf(res))
	}
	return nil
}

// WakerEnsureCommand is the idempotent, image-aware deploy of the waker helper.
// It recreates the container when the running image differs from `image` (or
// when it is absent) and is otherwise a no-op — so it both provisions the waker
// and upgrades it in place when the release image changes. The routing table and
// activity files live in a bind mount, so a recreate preserves them.
//
// Deployed on the same internal network as the proxy (reachable as
// akerdock-waker:8080, never published), with the local Docker socket to start
// managed containers. Shared by the deploy path (ensureWaker) and the
// scheduler's cross-server upgrade reconciliation.
func WakerEnsureCommand(network, image string) string {
	return fmt.Sprintf(
		"mkdir -p %s && cur=$(docker inspect -f '{{.Config.Image}}' %s 2>/dev/null || true); "+
			"if [ \"$cur\" != \"%s\" ]; then docker rm -f %s >/dev/null 2>&1 || true; "+
			"docker run -d --name %s --restart unless-stopped --network %s "+
			"-v /var/run/docker.sock:/var/run/docker.sock -v %s:%s "+
			"--label akerdock.managed=true --label akerdock.type=helper "+
			"%s waker; fi",
		wakerDir, proxy.WakerContainerName, image, proxy.WakerContainerName,
		proxy.WakerContainerName, network, wakerDir, wakerDir, image)
}

// removeWakerRoutes drops a resource from the shared table (preview destroy).
// The container stays: it still serves the server's other STZ resources.
func removeWakerRoutes(ctx context.Context, client *sshexec.Client, resourceUUID string) error {
	return depositWakerRoutes(ctx, client, removeWakerResource(readWakerConfig(ctx, client), resourceUUID))
}

// readWakerConfig reads the current routing table; absent or invalid → empty.
func readWakerConfig(ctx context.Context, client *sshexec.Client) waker.Config {
	var cfg waker.Config
	res, err := client.Run(ctx, "cat "+wakerDir+"/"+waker.RoutesFile+" 2>/dev/null || true")
	if err != nil || res == nil || res.Stdout == "" {
		return cfg
	}
	_ = json.Unmarshal([]byte(res.Stdout), &cfg)
	return cfg
}

// depositWakerRoutes writes the routing table atomically (temp + mv), so the
// waker — which reloads on mtime change — never reads a half-written file.
func depositWakerRoutes(ctx context.Context, client *sshexec.Client, cfg waker.Config) error {
	raw, err := waker.MarshalConfig(cfg)
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s/%s.tmp && mv -f %s/%s.tmp %s/%s",
		wakerDir, wakerDir, waker.RoutesFile, wakerDir, waker.RoutesFile, wakerDir, waker.RoutesFile)
	res, err := client.RunInput(ctx, cmd, string(raw))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("waker routes deposit failed (exit %d): %s", res.ExitCode, stderrOf(res))
	}
	return nil
}
