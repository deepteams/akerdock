// Typed create path of the compose pipeline (ADR-051/052): the ServicePlan
// maps onto the SDK's create body, and the config hash v2 fingerprints that
// body instead of a rendered shell string. The v1 command renderer survives
// in composedeploy.go as a frozen, pure hash input (ADR-053 window) until
// the fleet's containers all carry the v2 label.
package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"

	"github.com/deepteams/akerdock/internal/compose"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/sshexec"
)

// composeStablePeriod is the §4 no-healthcheck stabilization window of a
// compose service — a variable so tests can shrink it. composeOneShotTimeout
// bounds a one-shot job (§7.3), like the CLI's `timeout 600 docker wait`.
var (
	composeStablePeriod   = 5 * time.Second
	composeOneShotTimeout = 600 * time.Second
)

// composeCreateSpec is the typed twin of the frozen composeCreateCommand:
// one service's create body. Aliases land in the networking config — the
// stack network's DNS story (§8.3) — and the caller connects the extra and
// destination networks after the create, before the start.
type composeCreateSpec struct {
	Config     *container.Config
	Host       *container.HostConfig
	Networking *network.NetworkingConfig
}

// buildComposeCreateSpec renders one service's typed create. labels is the
// full label set for a real create — or a reduced one when hashing, so the
// per-deployment identity does not make every service look changed.
func buildComposeCreateSpec(plan *compose.Plan, sp compose.ServicePlan, appDir string, labels map[string]string, env []string, runRef string, opts composeCreateOpts) composeCreateSpec {
	svc := sp.Service

	allLabels := map[string]string{}
	for k, v := range labels {
		allLabels[k] = v
	}
	allLabels["akerdock.component"] = sp.Name
	if sp.OneShot {
		// The lifecycle job must not re-run one-shot jobs on start/restart.
		allLabels["akerdock.oneshot"] = "true"
	}
	for key, value := range svc.Labels {
		allLabels[key] = value
	}

	cfg := &container.Config{
		Image:  runRef,
		Env:    env,
		Labels: allLabels,
	}
	if svc.User != "" {
		cfg.User = svc.User
	}
	if svc.WorkingDir != "" {
		cfg.WorkingDir = svc.WorkingDir
	}
	if len(svc.Entrypoint) > 0 {
		cfg.Entrypoint = append([]string{}, svc.Entrypoint...)
	}
	if len(svc.Command) > 0 {
		cfg.Cmd = append([]string{}, svc.Command...)
	}
	if svc.StopSignal != "" {
		cfg.StopSignal = svc.StopSignal
	}
	if svc.StopGracePeriod != nil {
		grace := int(time.Duration(*svc.StopGracePeriod).Seconds())
		cfg.StopTimeout = &grace
	}
	if health := sp.Health; health != nil {
		switch {
		case health.Disable || (len(health.Test) > 0 && health.Test[0] == "NONE"):
			cfg.Healthcheck = &container.HealthConfig{Test: []string{"NONE"}}
		case len(health.Test) > 1:
			// The CLI path handed the joined test to --health-cmd, which is
			// CMD-SHELL semantics — kept identical here.
			cfg.Healthcheck = &container.HealthConfig{
				Test:        []string{"CMD-SHELL", strings.Join(health.Test[1:], " ")},
				Interval:    health.Interval,
				Timeout:     health.Timeout,
				Retries:     int(health.Retries),
				StartPeriod: health.StartPeriod,
			}
		}
	}

	restart := sp.Restart
	if restart == "" {
		restart = "no" // raw mode without a policy: compose default
	}
	host := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(restart)},
		NetworkMode:   container.NetworkMode(plan.NetworkName),
	}
	for _, mount := range sp.Mounts {
		switch mount.Type {
		case "volume", "bind":
			source := mount.Source
			if mount.Type == "bind" && !strings.HasPrefix(source, "/") {
				// Relative binds resolve inside the clone (§2.4).
				source = appDir + "/mounts/" + strings.TrimPrefix(source, "./")
			}
			bind := source + ":" + mount.Target
			if mount.ReadOnly {
				bind += ":ro"
			}
			host.Binds = append(host.Binds, bind)
		case "tmpfs":
			host.Tmpfs = mapAppend(host.Tmpfs, mount.Target, "")
		}
	}
	for _, port := range svc.Ports {
		target := nat.Port(fmt.Sprintf("%d/%s", port.Target, protocolOr(port.Protocol)))
		if cfg.ExposedPorts == nil {
			cfg.ExposedPorts = nat.PortSet{}
		}
		cfg.ExposedPorts[target] = struct{}{}
		if host.PortBindings == nil {
			host.PortBindings = nat.PortMap{}
		}
		host.PortBindings[target] = append(host.PortBindings[target], nat.PortBinding{
			HostIP: port.HostIP, HostPort: port.Published,
		})
	}
	limits := sp.Limits
	host.Memory = limits.Memory
	host.MemoryReservation = limits.MemoryReservation
	host.MemorySwap = limits.MemorySwap
	if limits.CPUs > 0 {
		host.NanoCPUs = int64(limits.CPUs * 1e9)
	}
	host.CPUShares = limits.CPUShares
	host.CpusetCpus = limits.CPUSet
	if limits.Pids > 0 {
		pids := limits.Pids
		host.PidsLimit = &pids
	}
	if svc.Init != nil && *svc.Init {
		enabled := true
		host.Init = &enabled
	}
	host.ReadonlyRootfs = svc.ReadOnly
	host.ExtraHosts = svc.ExtraHosts.AsList(":")

	networking := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		plan.NetworkName: {Aliases: append([]string{}, opts.Aliases...)},
	}}
	return composeCreateSpec{Config: cfg, Host: host, Networking: networking}
}

// composeSkipDecision decides whether a running container already matches
// the desired state (§8.2 step 1): the v2 fingerprint rules; a container
// created before the rollout carries no v2 label — immutable on a running
// container — so the FROZEN v1 fingerprint is its mandatory fallback
// (ADR-053). The window closes per container at its next real change.
func composeSkipDecision(state composeConfigState, hashV1, hashV2 string) bool {
	if !state.running {
		return false
	}
	if state.hashV2 != "" {
		return state.hashV2 == hashV2
	}
	return state.hashV1 == hashV1
}

// composeConfigHashV2 fingerprints the typed desired state of one service
// (ADR-053): the canonical JSON of its create body, hashed like v1 but over
// structs instead of a shell string. The "2:" prefix names the format.
func composeConfigHashV2(spec composeCreateSpec) string {
	payload, _ := json.Marshal(spec)
	sum := sha256.Sum256(payload)
	return "2:" + hex.EncodeToString(sum[:6])
}

// composeServiceEnv renders one service's environment: the typed KEY=VALUE
// entries for the create body, and the v1 shell-file content that remains a
// pure input of the frozen config hash.
func composeServiceEnv(sp compose.ServicePlan) (entries []string, envKeys []string, v1Content string) {
	svc := sp.Service
	envKeys = make([]string, 0, len(svc.Environment))
	for key, value := range svc.Environment {
		if value == nil {
			continue
		}
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	var v1 strings.Builder
	for _, key := range envKeys {
		entries = append(entries, key+"="+*svc.Environment[key])
		fmt.Fprintf(&v1, "export %s=%s\n", key, shellQuote(*svc.Environment[key]))
	}
	return entries, envKeys, v1.String()
}

// connectServiceNetworks attaches a container to its extra stack networks,
// and to the destination network when the proxy must reach it (§2.1).
// tolerant converges an already-connected container instead of failing —
// the unchanged-service path.
func (r *deploymentRun) connectServiceNetworks(ctx context.Context, sp compose.ServicePlan, containerName string, routedComponent, shortAliases, tolerant bool) error {
	connect := func(networkName, alias string) error {
		err := r.rt.NetworkConnect(ctx, networkName, containerName, &network.EndpointSettings{Aliases: []string{alias}})
		if err == nil || tolerant {
			return nil
		}
		if strings.Contains(err.Error(), "already exists in network") {
			return nil // converged by an earlier attempt
		}
		return err
	}
	for _, networkName := range sp.ExtraNetworks {
		alias := containerName
		if shortAliases {
			alias = sp.Name
		}
		if err := connect(networkName, alias); err != nil {
			return err
		}
	}
	if routedComponent {
		if err := connect(r.dest.Network, containerName); err != nil {
			return err
		}
	}
	return nil
}

// seedPreviewVolumes initializes the still-empty preview volumes of one
// service from their production counterparts (ADR-029): `cp -a` inside a
// throwaway container of the service's image, production mounted READ-ONLY.
// A missing production volume skips the pair (nothing to clone — and the
// mount must not create it as a side effect); a failing copy fails the
// deployment deliberately: the operator declared they want data, a silently
// empty database would betray that.
func (r *deploymentRun) seedPreviewVolumes(ctx context.Context, imageRef string, pairs [][2]string) error {
	for _, p := range pairs {
		prod, preview := p[0], p[1]
		if _, err := r.rt.VolumeInspect(ctx, prod); err != nil {
			if dockerruntime.IsNotFound(err) {
				continue
			}
			return err
		}
		if err := runOneShot(ctx, r.rt, &container.Config{
			Image:      imageRef,
			User:       "0",
			Entrypoint: []string{"/bin/sh"},
			Cmd:        []string{"-c", `[ -n "$(ls -A /akerdock-volume)" ] || cp -a /akerdock-seed-from/. /akerdock-volume/`},
		}, &container.HostConfig{Binds: []string{
			prod + ":/akerdock-seed-from:ro", preview + ":/akerdock-volume",
		}}); err != nil {
			return fmt.Errorf("seeding %s from %s: %w", preview, prod, err)
		}
	}
	return nil
}

// composeAwaitHealthy waits for a container's health (compose or image
// healthcheck), or a stable running state when it has none — the §4 verdict,
// decided control-plane-side.
func (r *deploymentRun) composeAwaitHealthy(ctx context.Context, sp compose.ServicePlan, containerName string) error {
	return r.step(ctx, "healthcheck_"+sp.Name, func() (*sshexec.Result, error) {
		verdict, err := r.composeHealthVerdict(ctx, sp, containerName)
		if err != nil {
			return nil, err
		}
		if verdict != "healthy" && verdict != "running" {
			detail := ""
			if out, lerr := containerLogsTail(ctx, r.rt, containerName, 100); lerr == nil && out != "" {
				detail = "\n" + out
			}
			return nil, fmt.Errorf("the service did not turn healthy (%s)%s", verdict, detail)
		}
		return nil, nil
	})
}

func (r *deploymentRun) composeHealthVerdict(ctx context.Context, sp compose.ServicePlan, containerName string) (string, error) {
	deadline := time.Now().Add(time.Duration(r.composeHealthBudget(sp)) * time.Second)
	for time.Now().Before(deadline) {
		resp, err := r.rt.ContainerInspect(ctx, containerName)
		if err == nil && resp.State != nil {
			switch {
			case resp.State.Health != nil && resp.State.Health.Status == "healthy":
				return "healthy", nil
			case resp.State.Health != nil && resp.State.Health.Status == "unhealthy":
				return "unhealthy", nil
			case resp.State.Health == nil && resp.State.Running:
				// No healthcheck: running and still running after a short
				// stabilization window counts as up (§4).
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(composeStablePeriod):
				}
				again, err := r.rt.ContainerInspect(ctx, containerName)
				if err == nil && again.State != nil && again.State.Running {
					return "running", nil
				}
				status := "absent"
				if err == nil && again.State != nil {
					status = again.State.Status
				}
				return status, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(deploymentHealthPoll):
		}
	}
	return "timeout", nil
}

// envEntryKeys lists the KEY halves of KEY=VALUE entries — what the frozen
// v1 hash feeds to envFlags.
func envEntryKeys(entries []string) []string {
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		k, _, _ := strings.Cut(e, "=")
		keys = append(keys, k)
	}
	return keys
}

// ensureStackNetwork creates a stack network when absent, carrying the run's
// management labels.
func ensureStackNetwork(ctx context.Context, rt dockerruntime.Runtime, name string, labels map[string]string) error {
	if _, err := rt.NetworkInspect(ctx, name, network.InspectOptions{}); err == nil {
		return nil
	} else if !dockerruntime.IsNotFound(err) {
		return err
	}
	_, err := rt.NetworkCreate(ctx, name, network.CreateOptions{Labels: labels})
	if err != nil && dockerruntime.IsConflict(err) {
		return nil // created concurrently
	}
	return err
}

// retireAdoptedProject stops and removes the containers of the original
// compose project of an adopted stack (§20.7) — best-effort, like the CLI
// pipeline it replaces: the volumes survive as external objects.
func (r *deploymentRun) retireAdoptedProject(ctx context.Context, project string) error {
	list, err := r.rt.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+project)),
	})
	if err != nil {
		return err
	}
	grace := 30
	for _, c := range list {
		if err := r.rt.ContainerStop(ctx, c.ID, container.StopOptions{Timeout: &grace}); err != nil && !dockerruntime.IsNotFound(err) {
			r.h.Logger.Warn("adopted container did not stop cleanly", "container", containerName(c), "error", err)
		}
		if err := r.rt.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil && !dockerruntime.IsNotFound(err) {
			r.h.Logger.Warn("adopted container was not removed", "container", containerName(c), "error", err)
		}
	}
	return nil
}

func protocolOr(p string) string {
	if p == "" {
		return "tcp"
	}
	return p
}

func mapAppend(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}
