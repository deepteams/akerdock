// Package jobs implements the job handlers executed by the queue workers.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/serverdial"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeServerValidate is the job type of POST /servers/{uuid}/validate.
const TypeServerValidate = "server.validate"

// ServerValidatePayload references the target server (UUIDs only, never
// secrets — INV-003).
type ServerValidatePayload struct {
	ServerID int64 `json:"server_id"`
}

// ServerValidate implements the onboarding workflow of §20.1, reduced to
// its P0 core: SSH connectivity + host key, system facts, Docker >= 24
// (snap refused). Docker auto-install and proxy bootstrap come with the
// deployment engine. Each step is visible with a remediation message.
type ServerValidate struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Docker  dockerruntime.Source
	HostOps hostops.Source
	Logger  *slog.Logger
	// ControlPlanePort is the published port of this instance (AKERDOCK_PORT),
	// used to route the instance FQDN on the server that hosts it (§14.2).
	ControlPlanePort int
	// AgentImage is this release's own image (AKERDOCK_IMAGE): the agent
	// helper is provisioned from it at validation time (ADR-054 tranche B) —
	// every later operation on the server rides its channel.
	AgentImage string
	// InstanceURL is the explicit agent dial-back base URL
	// (AKERDOCK_INSTANCE_URL); empty derives it per server.
	InstanceURL string
}

// minDockerMajor is the minimum supported Docker Engine version (§3.1).
const minDockerMajor = 24

// Execute runs one validation attempt. Idempotent by design: every step
// only observes or converges remote state.
func (h *ServerValidate) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload ServerValidatePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	server, err := h.Store.GetServerByID(ctx, payload.ServerID)
	if err != nil {
		return nil, fmt.Errorf("server not found (deleted since enqueue?): %w", err)
	}
	if err := h.Store.SetServerStatus(ctx, store.SetServerStatusParams{ID: server.ID, Status: store.ServerStatusValidating}); err != nil {
		return nil, err
	}

	// Step 1 — SSH connectivity with the team (or instance) key.
	rec.Start(ctx, "ssh_connect")
	key, err := h.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		rec.Fail(ctx, "the referenced private key no longer exists")
		return nil, h.markUnreachable(ctx, server.ID, err)
	}
	pem, err := h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		rec.Fail(ctx, "the private key could not be decrypted — check the master key file (runbook key-rotation.md)")
		return nil, err
	}
	client, err := serverdial.DialWithKey(ctx, server, string(pem))
	if errors.Is(err, sshexec.ErrHostKeyChanged) {
		// Not a connectivity problem: the machine answering is not the one we
		// onboarded. Either it was rebuilt — in which case an operator clears
		// the pin deliberately — or someone is in the middle. Never auto-repin.
		rec.Fail(ctx, "the server presented a different SSH host key than the one pinned at onboarding ("+
			firstLine(err.Error())+") — if the server was legitimately rebuilt, clear the pin before re-validating (§20.1)")
		return nil, h.markUnreachable(ctx, server.ID, err)
	}
	if err != nil {
		rec.Fail(ctx, "SSH connection failed — check host, port, user, firewall, and that the public key is in authorized_keys")
		return nil, h.markUnreachable(ctx, server.ID, err)
	}
	defer func() { _ = client.Close() }()
	// Trust-on-first-use: pin what the server presented, if nothing is pinned
	// yet. From here on, every job refuses a server that answers with another
	// key (§20.1) — the connection above already enforced that, since it was
	// given the pinned fingerprint.
	if err := h.Store.PinServerHostKey(ctx, store.PinServerHostKeyParams{
		ID: server.ID, HostKeyFingerprint: &client.HostKeyFingerprint,
	}); err != nil {
		return nil, err
	}
	rec.Succeed(ctx, "connected; host key "+client.HostKeyFingerprint)

	// Step 1.5 — the non-root contract (§3.1, ADR-076): a use_sudo server runs
	// every later command under `sudo -n`, so prove the escalation works
	// before any step fails for the wrong-looking reason. Its own step because
	// §20.1 demands a DISTINCT error for interactive sudo — and because the
	// remediation (a sudoers line) has nothing in common with the SSH one.
	if server.UseSudo {
		rec.Start(ctx, "check_sudo")
		res, err := client.Run(ctx, "true")
		if err == nil && res.ExitCode != 0 {
			// The classified refusals (password prompt, sudo absent) arrive as
			// errors already; anything else non-zero is still a broken
			// escalation — a restricted sudoers, typically.
			err = fmt.Errorf("sudo escalation failed (exit %d, %s) — the sudoers entry must allow "+
				"the user to run any command without a password (NOPASSWD: ALL, §3.1)", res.ExitCode, stderrOf(res))
		}
		if err != nil {
			rec.Fail(ctx, firstLine(err.Error()))
			if statusErr := h.Store.SetServerStatus(ctx, store.SetServerStatusParams{ID: server.ID, Status: store.ServerStatusPending}); statusErr != nil {
				return nil, statusErr
			}
			return nil, err
		}
		rec.Succeed(ctx, "passwordless sudo confirmed for "+server.SshUser)
	}

	// Step 2 — system facts: OS and architecture (amd64/arm64 only, §22.4).
	rec.Start(ctx, "detect_system")
	facts, err := detectSystem(ctx, client)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, h.recordFacts(ctx, server.ID, facts, store.ServerStatusPending, err)
	}
	rec.Succeed(ctx, fmt.Sprintf("%s (%s)", facts.OSName, facts.Architecture))

	// Step 3 — Docker Engine >= 24, snap installs refused (§3.1).
	rec.Start(ctx, "check_docker")
	if err := checkDocker(ctx, client, facts); err != nil {
		rec.Fail(ctx, err.Error())
		return nil, h.recordFacts(ctx, server.ID, facts, store.ServerStatusPending, err)
	}
	rec.Succeed(ctx, "Docker Engine "+facts.DockerVersion)

	if err := h.recordFacts(ctx, server.ID, facts, store.ServerStatusReady, nil); err != nil {
		return nil, err
	}
	if err := h.ensureDefaultDestination(ctx, server.ID); err != nil {
		return nil, err
	}

	// Step 4 — the agent (ADR-051/054): since Docker operations flow through
	// its channel exclusively, a server without an agent is a server AkerDock
	// cannot operate — provisioning it is part of first contact, and the
	// validation only succeeds once the channel answers. The scheduler's
	// reconciliation keeps it converged afterwards.
	rec.Start(ctx, "provision_agent")
	rt, ops, err := h.provisionAgent(ctx, client, server)
	if err != nil {
		rec.Fail(ctx, firstLine(err.Error()))
		return nil, err
	}
	rec.Succeed(ctx, "agent connected — the command channel answers")

	// Endpoint declarations can be created while a server is validating, and
	// an earlier routing job may have failed after Traefik was written. Rebuild
	// the agent's authoritative ingress table before the proxy is exposed.
	if server.ProxyType == store.ProxyTypeTraefik {
		rec.Start(ctx, "sync_ingress_routes")
		if err := ReconcileIngressRoutes(ctx, h.Store, ops, server); err != nil {
			rec.Fail(ctx, firstLine(err.Error()))
			return nil, err
		}
		rec.Succeed(ctx, "ingress host table synchronized")
	}

	// Step 5 — proxy bootstrap (proxy-contract §1.3): static config +
	// managed Traefik container, when the operator's intent asks for it.
	if run, reason := proxyBootstrapDecision(server); !run {
		rec.Skip(ctx, "bootstrap_proxy", reason)
	} else {
		rec.Start(ctx, "bootstrap_proxy")
		if err := bootstrapProxy(ctx, h.Store, h.Keyring, client, rt, ops,
			&EdgeSyncer{Store: h.Store, Docker: h.Docker, Host: h.HostOps, Logger: h.Logger},
			server, false, h.ControlPlanePort); err != nil {
			rec.Fail(ctx, "proxy bootstrap failed — retry the validation once the cause is fixed: "+firstLine(err.Error()))
			return nil, err
		}
		rec.Succeed(ctx, "proxy running on ports "+fmt.Sprintf("%d/%d", server.ProxyHttpPort, server.ProxyHttpsPort))
	}

	// Step 6 — build packs that need a binary on the server (§5.5). Failing to
	// install it does NOT fail the validation: a server that can run image and
	// dockerfile deployments is a usable server. The deployment engine
	// reinstalls on demand and fails there, where the user asked for nixpacks.
	if err := installNixpacks(ctx, client); err != nil {
		// Recorded as skipped, not failed: the job succeeded, and a step marked
		// failed inside a succeeded job reads as a bug.
		rec.Skip(ctx, "provision_build_packs",
			"nixpacks unavailable ("+firstLine(err.Error())+") — image, dockerfile and static deployments are unaffected")
		h.Logger.Warn("nixpacks provisioning failed", "server_id", server.ID, "error", err)
	} else {
		rec.Start(ctx, "provision_build_packs")
		rec.Succeed(ctx, "nixpacks "+NixpacksVersion+" available")
	}

	h.Logger.Info("server validated", "server_id", server.ID, "docker", facts.DockerVersion)
	return map[string]any{
		"os_name":              facts.OSName,
		"architecture":         facts.Architecture,
		"docker_version":       facts.DockerVersion,
		"host_key_fingerprint": client.HostKeyFingerprint,
	}, nil
}

// agentReadyTimeout bounds the wait for a freshly provisioned agent to dial
// back and answer on its channel; agentReadyPoll is the probe cadence (vars
// so tests can shrink them).
var (
	agentReadyTimeout = 90 * time.Second
	agentReadyPoll    = 2 * time.Second
)

// provisionAgent converges the agent helper over SSH (the bootstrap family —
// nothing else can carry it, ADR-054) and waits until its command channel
// answers, returning the server's runtime and file primitives. The failure
// messages name the remediation: this is where the operator is looking.
func (h *ServerValidate) provisionAgent(ctx context.Context, client *sshexec.Client, server store.Server) (dockerruntime.Runtime, hostops.Ops, error) {
	if h.AgentImage == "" {
		return nil, nil, fmt.Errorf("AKERDOCK_IMAGE is not set — the agent runs the AkerDock image and every Docker operation rides its channel (ADR-051); set it and re-validate")
	}
	env := AgentEnvForServer(ctx, h.Store, h.Keyring, h.Logger, server, h.ControlPlanePort, h.InstanceURL)
	if env.InstanceURL == "" || env.Token == "" {
		return nil, nil, fmt.Errorf("agent enrollment unavailable — the agent dials the instance back: set AKERDOCK_INSTANCE_URL or the instance FQDN, then re-validate")
	}
	// The helper joins the server's default destination network when one
	// exists — created here if needed, exactly as the proxy bootstrap does
	// (it may not have run yet).
	network := "bridge"
	if dest, err := h.Store.GetDefaultDestination(ctx, server.ID); err == nil && dest.Network != "" {
		network = dest.Network
		if res, err := client.Run(ctx, fmt.Sprintf(
			"docker network inspect %s >/dev/null 2>&1 || docker network create --label akerdock.managed=true %s",
			network, network)); err != nil || res.ExitCode != 0 {
			return nil, nil, fmt.Errorf("destination network: %v (exit %d, %s)", err, exitCode(res), stderrOf(res))
		}
	}
	res, err := client.Run(ctx, AgentEnsureCommand(network, h.AgentImage, env))
	if err != nil {
		return nil, nil, err
	}
	if res.ExitCode != 0 {
		// The known failure mode of a source-only install (ADR-078): the
		// image exists in no registry, so obtaining it either built from the
		// public repository at this instance's commit (the prelude) or failed
		// a hopeless pull. Name the remediation for the exact situation —
		// the operator is looking at precisely this message.
		hint := ""
		combined := res.Stdout + "\n" + res.Stderr
		for _, marker := range []string{
			"pull access denied", "repository does not exist", "manifest unknown", "Unable to find image",
			"could not find remote ref", "repository not found", "failed to fetch",
		} {
			if !strings.Contains(combined, marker) {
				continue
			}
			if agentSource.Commit != "" {
				hint = fmt.Sprintf(" — the server builds %q from %s#%s (ADR-078): is that commit pushed? "+
					"push it and re-validate, or ship unpushed work with ./install.sh seed <user>@%s",
					h.AgentImage, agentSource.Repo, agentSource.Commit, server.Host)
			} else {
				hint = fmt.Sprintf(" — the image %q is not pullable from this server, and this instance carries "+
					"no source commit to rebuild it from (a dirty tree at install time, ADR-078): commit, push and "+
					"re-run ./install.sh, or ship the working tree with ./install.sh seed <user>@%s",
					h.AgentImage, server.Host)
			}
			break
		}
		return nil, nil, fmt.Errorf("agent deploy failed (exit %d): %s%s", res.ExitCode, stderrOf(res), hint)
	}
	// The agent dials OUTBOUND: wait for its channel, not for the container.
	deadline := time.Now().Add(agentReadyTimeout)
	for {
		if rt, err := h.Docker.Runtime(ctx, server.ID); err == nil {
			if _, err := rt.Ping(ctx); err == nil {
				ops, err := h.HostOps.HostOps(ctx, server.ID)
				if err == nil {
					return rt, ops, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("the agent did not connect within %s — the container is running but its channel never reached this instance: check that %s is reachable FROM the server (firewall, DNS)", agentReadyTimeout, env.InstanceURL)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(agentReadyPoll):
		}
	}
}

// NixpacksVersion is pinned per AkerDock release (§5.5): a build pack that
// silently follows upstream would make the same commit produce a different
// image from one day to the next.
const NixpacksVersion = "1.38.0"

// nixpacksBin is where the binary lives on a target server. /usr/local/bin is
// not writable by a non-root deploy user, so it goes under the AkerDock root —
// the one directory the engine already owns.
const nixpacksBin = "/var/lib/akerdock/bin/nixpacks"

// installNixpacks converges the pinned binary on the server. Idempotent: an
// already-correct version is left alone, so a re-validation costs nothing.
func installNixpacks(ctx context.Context, client *sshexec.Client) error {
	check, err := client.Run(ctx, nixpacksBin+" --version 2>/dev/null | awk '{print $2}'")
	if err != nil {
		return err
	}
	if strings.TrimSpace(check.Stdout) == NixpacksVersion {
		return nil
	}

	// The release tarball is named by target triple; the binary must match the
	// server's architecture, not the control plane's.
	script := fmt.Sprintf(`set -e
mkdir -p /var/lib/akerdock/bin
case "$(uname -m)" in
  x86_64) target=x86_64-unknown-linux-musl ;;
  aarch64|arm64) target=aarch64-unknown-linux-musl ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
url="https://github.com/railwayapp/nixpacks/releases/download/v%[1]s/nixpacks-v%[1]s-${target}.tar.gz"
tmp=$(mktemp -d)
curl -fsSL "$url" -o "$tmp/nixpacks.tar.gz"
tar -xzf "$tmp/nixpacks.tar.gz" -C "$tmp"
install -m 0755 "$tmp/nixpacks" %[2]s
rm -rf "$tmp"
%[2]s --version`, NixpacksVersion, nixpacksBin)

	res, err := client.Run(ctx, script)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("nixpacks install failed: %s", firstLine(res.Stderr))
	}
	return nil
}

type systemFacts struct {
	OSName        string
	Architecture  string
	DockerVersion string
}

func detectSystem(ctx context.Context, client *sshexec.Client) (*systemFacts, error) {
	facts := &systemFacts{}
	res, err := client.Run(ctx, "uname -m && (. /etc/os-release 2>/dev/null && echo \"$PRETTY_NAME\") || uname -s")
	if err != nil || res.ExitCode != 0 {
		return facts, fmt.Errorf("could not detect the system (uname failed) — is this a Linux server?")
	}
	lines := strings.SplitN(res.Stdout, "\n", 2)
	switch strings.TrimSpace(lines[0]) {
	case "x86_64", "amd64":
		facts.Architecture = "amd64"
	case "aarch64", "arm64":
		facts.Architecture = "arm64"
	default:
		return facts, fmt.Errorf("unsupported architecture %q — AkerDock supports amd64 and arm64 (§3.1)", lines[0])
	}
	if len(lines) > 1 {
		facts.OSName = strings.TrimSpace(lines[1])
	}
	return facts, nil
}

func checkDocker(ctx context.Context, client *sshexec.Client, facts *systemFacts) error {
	which, err := client.Run(ctx, "command -v docker")
	if err != nil {
		return err
	}
	if which.ExitCode != 0 {
		return fmt.Errorf("docker engine not found — install Docker >= %d (https://docs.docker.com/engine/install/); automated install lands with the deployment engine", minDockerMajor)
	}
	if strings.Contains(which.Stdout, "/snap/") {
		return fmt.Errorf("docker installed via snap is not supported (§3.1) — reinstall from the official packages")
	}
	res, err := client.Run(ctx, "docker version --format '{{.Server.Version}}'")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker daemon unreachable (%s) — is the docker service running and the user allowed to use it?", firstLine(res.Stderr))
	}
	version := strings.TrimSpace(res.Stdout)
	major, _, _ := strings.Cut(version, ".")
	if m, err := strconv.Atoi(major); err != nil || m < minDockerMajor {
		return fmt.Errorf("docker engine %s is too old — version >= %d required (§3.1)", version, minDockerMajor)
	}
	facts.DockerVersion = version
	return nil
}

// bootstrapProxy converges the managed Traefik container (§1.3):
// filesystem layout, static configuration, and an idempotent docker run.
// The docker socket is never mounted — all configuration is file-based.
// bootstrapProxy converges the managed proxy on a server: static config, DNS-01
// env-file, container.
//
// `recreate` forces the container to be replaced. It is what a TCP port change
// needs: Traefik cannot add a listener at runtime, so the entrypoint is static —
// and that is precisely why the port lives on the PROXY and not on the database
// container. Recreating the proxy costs seconds and touches no data; restarting
// the database drops every open connection.
//
// A free function, not a method: the database job needs it too, and a proxy that
// two jobs converge differently is a proxy that drifts.
// proxyBootstrapDecision says whether a background job may create and start
// the managed proxy of this server, and if not, why (the reason is shown as
// the step's skip message).
//
// A build server hosts no application (§3.4), so it routes nothing: giving it
// a proxy would bind ports 80/443 on a machine that has no reason to listen
// on them. And a proxy whose desired state is `stopped` — the state every new
// server starts in — belongs to the operator: they review the proxy settings
// first, then the FIRST start is their explicit action (POST /proxy/start),
// never a side effect of validation (§20.1 step 5, « selon les options »).
func proxyBootstrapDecision(server store.Server) (run bool, skipReason string) {
	switch {
	case server.IsBuildServer:
		return false, "build server: it hosts no application, so it routes nothing"
	case server.ProxyType != store.ProxyTypeTraefik:
		return false, "proxy_type is none — this server routes nothing until an operator changes it"
	case server.ProxyDesiredState != store.ProxyDesiredStateRunning:
		return false, "proxy intent is stopped — review the proxy settings (ports, wildcard domain, ACME email), then start it from the server page"
	default:
		return true, ""
	}
}

func bootstrapProxy(ctx context.Context, q *store.Queries, kr *envelope.Keyring, client *sshexec.Client, rt dockerruntime.Runtime, ops hostops.Ops, edge *EdgeSyncer, server store.Server, recreate bool, cpPort int) error {
	h := &ServerValidate{Store: q, Keyring: kr}
	dest, err := h.Store.GetDefaultDestination(ctx, server.ID)
	if err != nil {
		return err
	}
	team := ""
	if server.TeamID != 0 {
		if t, err := h.Store.GetTeamByID(ctx, server.TeamID); err == nil {
			team = pguuid.String(t.Uuid)
		}
	}
	// The ACME contact (§4.3). A wrong address is a SILENT failure: the proxy
	// starts, everything looks healthy, and the certificates never arrive —
	// Let's Encrypt refuses example.com and invalid TLDs outright. So it is an
	// explicit setting, and a missing one stops the bootstrap here, where the
	// operator is looking, instead of in production three weeks later.
	settings, err := h.Store.GetInstanceSettings(ctx)
	if err != nil {
		return err
	}
	email := ""
	switch {
	case settings.AcmeEmail != nil && *settings.AcmeEmail != "":
		email = *settings.AcmeEmail
	case settings.Fqdn != nil && *settings.Fqdn != "":
		// A last-resort derivation, valid as long as the FQDN is a real domain.
		email = "admin@" + *settings.Fqdn
	default:
		return fmt.Errorf("no ACME contact email: set AKERDOCK_ACME_EMAIL (or the instance FQDN) — " +
			"Let's Encrypt refuses to issue without a valid contact, and the failure would be silent (§4.3)")
	}
	// DNS-01 (proxy-contract §7.2): the credential is materialized as acme.env
	// (0600) and injected with --env-file. It never enters traefik.yaml, which
	// is checksummed, stored as a revision and read back — a secret written
	// there would be a secret in the database, twice (INV-003).
	dnsProvider, acmeEnv, err := h.dnsCredential(ctx, server)
	if err != nil {
		return err
	}
	// The TCP entrypoints of the databases routed through this proxy (§2.6).
	// They are part of the static config because Traefik cannot add a listener
	// at runtime — which is the whole reason the port lives here rather than on
	// the database container.
	tcpPorts, err := h.Store.ListTCPProxyPorts(ctx, server.ID)
	if err != nil {
		return err
	}
	ports := make([]int, 0, len(tcpPorts))
	for _, p := range tcpPorts {
		if p != nil {
			ports = append(ports, int(*p))
		}
	}
	trustedIPs, err := edgeTrustedIPs(ctx, h.Store, server)
	if err != nil {
		return err
	}
	static := proxy.GenerateStatic(int(server.ProxyHttpPort), int(server.ProxyHttpsPort), email, dnsProvider, ports, trustedIPs, server.ID)

	// Legacy layout migration (spec amendment: /data/akerdock → FHS
	// /var/lib/akerdock): the old directory carries acme.json — the Let's
	// Encrypt account and every issued certificate — so it is MOVED, never
	// abandoned. Entry by entry, because another step (nixpacks provisioning,
	// typically) may already have created the new root: a whole-directory move
	// would then silently skip, stranding the certificates. The proxy
	// container is removed first — its bind mounts point into the old path —
	// and recreated right below against the new one. Running application
	// containers keep their old mounts alive until their next deployment.
	legacyMove := "if [ -d /data/akerdock ]; then" +
		" docker rm -f " + proxy.ContainerName + " >/dev/null 2>&1 || true;" +
		" mkdir -p /var/lib/akerdock;" +
		" for e in /data/akerdock/* /data/akerdock/.[!.]*; do" +
		"   [ -e \"$e\" ] || continue;" +
		"   [ -e \"/var/lib/akerdock/$(basename \"$e\")\" ] || mv \"$e\" /var/lib/akerdock/;" +
		" done;" +
		" rmdir /data/akerdock 2>/dev/null || true; fi && "
	// A static config change only takes effect on a NEW container (§1.4), and
	// nothing else in the product ever notices one: the run below is idempotent
	// on the container name, so a proxy created before the change would keep
	// its old entrypoints, timeouts and published ports forever. Read what is
	// deployed BEFORE overwriting it, and recreate when it drifted. A missing
	// file (first bootstrap, pruned host) also counts as drift — the recreate
	// is then a no-op `docker rm -f || true`.
	if !recreate {
		deployed, readErr := client.Run(ctx,
			"printf '%s' '"+proxyStaticBeginMarker+"'; cat /var/lib/akerdock/proxy/traefik.yaml 2>/dev/null;"+
				" printf '%s' '"+proxyStaticEndMarker+"'")
		if readErr != nil {
			return readErr
		}
		recreate = proxyStaticDrifted(deployed, static)
	}

	res, err := client.RunInput(ctx,
		legacyMove+
			"mkdir -p /var/lib/akerdock/proxy/dynamic /var/lib/akerdock/proxy/certs /var/lib/akerdock/proxy/auth"+
			" && umask 077 && touch /var/lib/akerdock/proxy/acme.json"+
			" && cat > /var/lib/akerdock/proxy/traefik.yaml"+
			fmt.Sprintf(" && (docker network inspect %s >/dev/null 2>&1 || docker network create --label akerdock.managed=true %s)", dest.Network, dest.Network), static)
	if err != nil || res.ExitCode != 0 {
		// The known failure mode of this step is a non-root SSH user that
		// cannot create /var/lib/akerdock (§3.1: root is the nominal contract,
		// non-root is experimental) — spell out both exits instead of leaving
		// a locale-dependent mkdir error alone.
		return fmt.Errorf("proxy layout: %v (exit %d, %s) — the SSH user must be able to write "+
			"/var/lib/akerdock: onboard the server as root, enable use_sudo on the server "+
			"(passwordless sudo required, ADR-076), or pre-create it for the user "+
			"(sudo mkdir -p /var/lib/akerdock && sudo chown -R <ssh-user>: /var/lib/akerdock)",
			err, exitCode(res), stderrOf(res))
	}

	envFileFlag := ""
	if acmeEnv != "" {
		// Written through stdin, with umask 077: the values never reach argv,
		// and the file is unreadable by anything but root (INV-003).
		res, err := client.RunInput(ctx, "umask 077 && cat > /var/lib/akerdock/proxy/acme.env", acmeEnv)
		if err != nil || res.ExitCode != 0 {
			return fmt.Errorf("writing the DNS-01 credentials failed")
		}
		envFileFlag = "--env-file /var/lib/akerdock/proxy/acme.env "
	}

	portPublish := proxyPortPublishArgs(int(server.ProxyHttpPort), int(server.ProxyHttpsPort), ports)
	if recreate {
		// Removed, not restarted: a static config change only takes effect on a
		// new container (§1.4) — and a published port (the HTTP/3 UDP listener,
		// a database's TCP entrypoint) only on a new `docker run`.
		if _, err := client.Run(ctx, "docker rm -f "+proxy.ContainerName+" >/dev/null 2>&1 || true"); err != nil {
			return err
		}
	}
	runCmd := fmt.Sprintf(
		"docker container inspect %s >/dev/null 2>&1 || docker run -d --name %s --restart unless-stopped --network %s "+
			"%s%s"+
			"-v /var/lib/akerdock/proxy/traefik.yaml:/etc/traefik/traefik.yaml:ro "+
			"-v /var/lib/akerdock/proxy/dynamic:/dynamic:ro "+
			"-v /var/lib/akerdock/proxy/acme.json:/acme/acme.json "+
			"-v /var/lib/akerdock/proxy/certs:/certs:ro "+
			"-v /var/lib/akerdock/proxy/auth:/auth:ro "+
			// host-gateway lets the 00-control-plane route reach the control
			// plane on this host (PRD §14.2) — harmless on every other server.
			"--add-host=host.docker.internal:host-gateway "+
			"--label akerdock.managed=true --label akerdock.type=proxy --label akerdock.team_uuid=%s "+
			"--health-cmd 'traefik healthcheck --ping' --health-interval 5s --health-retries 3 "+
			"%s",
		proxy.ContainerName, proxy.ContainerName, dest.Network, envFileFlag, portPublish,
		team, proxy.Image)
	res, err = client.Run(ctx, runCmd)
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("proxy container: %v (exit %d, %s)%s",
			err, exitCode(res), stderrOf(res), portConflictHint(res, int(server.ProxyHttpPort), int(server.ProxyHttpsPort)))
	}
	// Converge and verify: an existing but stopped/Created container is
	// started, and the bootstrap only succeeds with a running proxy. The
	// start's own stderr is kept: on a retry after a failed first run it is
	// the only place the real cause (a port bind conflict, typically) shows.
	startRes, err := client.Run(ctx, "docker start "+proxy.ContainerName+" 2>&1 >/dev/null || true")
	if err != nil {
		return err
	}
	res, err = client.Run(ctx, fmt.Sprintf(
		"sleep 1; docker inspect --format '{{.State.Status}}' %s", proxy.ContainerName))
	if err != nil {
		return err
	}
	if status := strings.TrimSpace(res.Stdout); status != "running" {
		logs, _ := client.Run(ctx, "docker logs --tail 20 "+proxy.ContainerName+" 2>&1")
		detail := ""
		switch {
		case startRes != nil && strings.TrimSpace(startRes.Stdout) != "":
			detail = " — " + firstLine(startRes.Stdout)
		case logs != nil && strings.TrimSpace(logs.Stdout) != "":
			detail = " — " + firstLine(logs.Stdout)
		}
		hint := portConflictHint(startRes, int(server.ProxyHttpPort), int(server.ProxyHttpsPort))
		if hint == "" {
			hint = portConflictHint(res, int(server.ProxyHttpPort), int(server.ProxyHttpsPort))
		}
		return fmt.Errorf("proxy container is %q, expected running%s%s", status, detail, hint)
	}

	// Route the instance FQDN through this proxy when the server hosts the
	// instance (PRD §14.2, proxy-contract §1.3) — and withdraw the route when
	// the FQDN is removed. Done last: the applier verifies against the Traefik
	// API of the running container.
	fqdn := ""
	if settings.Fqdn != nil {
		fqdn = *settings.Fqdn
	}
	content := controlPlaneRouteContent(server, fqdn, cpPort)
	last, lastErr := q.GetLastAppliedProxyRevision(ctx, store.GetLastAppliedProxyRevisionParams{
		ServerID: server.ID, Scope: proxy.ControlPlaneScope,
	})
	switch {
	case lastErr == nil && last.Content == content:
		// Already converged. Bootstrap runs on every proxy start: re-applying
		// would pile up one identical revision per start for nothing — file
		// drift is the reconciler's job (§6.2.4), not this one's.
		return nil
	case lastErr != nil && content == "":
		return nil // never routed, nothing to withdraw
	}
	if rt == nil || ops == nil {
		// The break-glass path (ADR-062 §3) runs outside the control plane and
		// therefore has no agent channel. The container is back, which is what
		// it was called for, and the routing files it mounts are already on
		// disk; the control-plane route reconverges on the next validation.
		return nil
	}
	applier := &ProxyApplier{Store: q, Docker: rt, Host: ops, Server: server, Network: dest.Network, Edge: edge}
	if err := applier.Apply(ctx, proxy.ControlPlaneScope, content, ""); err != nil {
		return fmt.Errorf("instance FQDN routing (%s): %w", proxy.ControlPlaneScope, err)
	}
	return nil
}

// The deployed static config is read between markers rather than raw: a login
// shell that prints anything of its own (a MOTD echoed from .bashrc, a version
// manager's banner) would otherwise look like drift forever, and drift means
// recreating the proxy — every single validation would cut the traffic.
const (
	proxyStaticBeginMarker = "<<<akerdock-static-begin>>>"
	proxyStaticEndMarker   = "<<<akerdock-static-end>>>"
)

// RepairProxy converges a server's managed proxy over SSH alone, with no API
// session and no agent channel (ADR-062 §3). It is the way back when the
// dashboard is unreachable *because* the proxy is down — the case where every
// in-product path is, by construction, closed.
//
// It runs the same convergence as a proxy start: render the static
// configuration, replace the container when what is deployed drifted, and
// start what it finds. Its authority is possession of the host and of the
// instance's own configuration, never a credential of its own.
func RepairProxy(ctx context.Context, q *store.Queries, kr *envelope.Keyring, client *sshexec.Client, server store.Server, cpPort int) error {
	return bootstrapProxy(ctx, q, kr, client, nil, nil, nil, server, false, cpPort)
}

// proxyStaticDrifted compares the static configuration deployed on the server
// with the one this release renders. Drift means the running container was
// created against different entrypoints, timeouts or published ports than the
// ones it is supposed to have — and since Traefik reads its static file once,
// at startup, and `docker run` publishes ports once, at creation, the only way
// to converge is to replace the container. An empty or unreadable file (first
// bootstrap, pruned host) counts as drift: the recreate is then a no-op.
func proxyStaticDrifted(deployed *sshexec.Result, static string) bool {
	if deployed == nil {
		return true
	}
	_, rest, found := strings.Cut(deployed.Stdout, proxyStaticBeginMarker)
	if !found {
		return true
	}
	body, _, found := strings.Cut(rest, proxyStaticEndMarker)
	if !found {
		return true
	}
	body = strings.TrimSpace(body)
	return body == "" || body != strings.TrimSpace(static)
}

func proxyPortPublishArgs(httpPort, httpsPort int, tcpPorts []int) string {
	var args strings.Builder
	fmt.Fprintf(&args, "-p %d:%d -p %d:%d -p %d:%d/udp ",
		httpPort, httpPort, httpsPort, httpsPort, httpsPort, httpsPort)
	for _, port := range tcpPorts {
		fmt.Fprintf(&args, "-p %d:%d ", port, port)
	}
	return args.String()
}

// controlPlaneRouteContent is the desired content of the 00-control-plane
// dynamic file for this server — empty when the server must not carry the
// route (it does not host the instance, no FQDN is configured, or the
// control-plane port is unknown). The revision stamped in the header is the
// server ID: stable, so identical desired states compare equal across
// bootstraps.
func controlPlaneRouteContent(server store.Server, fqdn string, cpPort int) string {
	if !server.IsLocalhost || fqdn == "" || cpPort <= 0 {
		return ""
	}
	return proxy.GenerateControlPlane(fqdn, cpPort, server.ID)
}

// portConflictHint recognizes the single most common first-start failure — a
// process already listening on the proxy ports (an existing nginx/apache, a
// previous proxy) — and says what to do about it, instead of leaving the
// operator to decode a Docker bind error.
func portConflictHint(res *sshexec.Result, httpPort, httpsPort int) string {
	if res == nil {
		return ""
	}
	combined := res.Stdout + res.Stderr
	if !strings.Contains(combined, "port is already allocated") &&
		!strings.Contains(combined, "address already in use") {
		return ""
	}
	return fmt.Sprintf(" — another process already listens on port %d or %d on this server "+
		"(an existing nginx or apache?): stop it, or change proxy_http_port/proxy_https_port "+
		"in the server's Proxy settings before starting", httpPort, httpsPort)
}

func exitCode(res *sshexec.Result) int {
	if res == nil {
		return -1
	}
	return res.ExitCode
}

func stderrOf(res *sshexec.Result) string {
	if res == nil {
		return ""
	}
	return firstLine(res.Stderr)
}

// ensureDefaultDestination creates the server's default Docker network
// destination on first successful validation (network named by UUID,
// deployment-engine §6.1).
func (h *ServerValidate) ensureDefaultDestination(ctx context.Context, serverID int64) error {
	if _, err := h.Store.GetDefaultDestination(ctx, serverID); err == nil {
		return nil
	}
	u, err := pguuid.New()
	if err != nil {
		return err
	}
	_, err = h.Store.CreateDestination(ctx, store.CreateDestinationParams{
		Uuid: u, ServerID: serverID, Name: "default", Network: pguuid.String(u), IsDefault: true,
	})
	return err
}

func (h *ServerValidate) markUnreachable(ctx context.Context, serverID int64, cause error) error {
	if err := h.Store.SetServerStatus(ctx, store.SetServerStatusParams{ID: serverID, Status: store.ServerStatusUnreachable}); err != nil {
		return err
	}
	return cause
}

func (h *ServerValidate) recordFacts(ctx context.Context, serverID int64, facts *systemFacts, status store.ServerStatus, cause error) error {
	var osName, arch, docker *string
	if facts.OSName != "" {
		osName = &facts.OSName
	}
	if facts.Architecture != "" {
		arch = &facts.Architecture
	}
	if facts.DockerVersion != "" {
		docker = &facts.DockerVersion
	}
	if err := h.Store.RecordServerFacts(ctx, store.RecordServerFactsParams{
		ID: serverID, OsName: osName, Architecture: arch, DockerVersion: docker, Status: status,
	}); err != nil {
		return err
	}
	return cause
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// dnsCredential decrypts the server's DNS-01 credential, if it has one, and
// renders it as an env-file for Lego.
//
// Returns ("", "", nil) when the server issues no wildcard: DNS-01 is not the
// default challenge, it is the one a wildcard forces.
func (h *ServerValidate) dnsCredential(ctx context.Context, server store.Server) (provider, envFile string, err error) {
	if server.DnsCredentialID == nil {
		return "", "", nil
	}
	cred, err := h.Store.GetDNSCredentialByID(ctx, *server.DnsCredentialID)
	if err != nil {
		return "", "", fmt.Errorf("the DNS credential of this server vanished: %w", err)
	}
	raw, err := h.Keyring.Decrypt("cloud_credentials", "config_enc", pguuid.String(cred.Uuid), cred.ConfigEnc)
	if err != nil {
		return "", "", err
	}
	var vars map[string]string
	if err := json.Unmarshal(raw, &vars); err != nil {
		return "", "", err
	}
	// Sorted: an env-file whose lines move around on every generation would
	// recreate the proxy container on every validation, for nothing.
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, vars[k])
	}
	return cred.Provider, b.String(), nil
}
