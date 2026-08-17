package jobs

// Coverage tests for the parts of servervalidate.go that surround the SSH
// session: the DNS-01 credential rendered as an env-file, the bookkeeping a
// validation leaves behind (default destination, unreachable status), the
// agent provisioning ladder of ADR-054, the nixpacks convergence, and the
// break-glass proxy repair of ADR-062 §3 — which runs with no agent channel
// at all. The scripted SSH server plays the host; the typed fakes play the
// agent's two seams.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	dockerfake "github.com/deepteams/akerdock/internal/dockerruntime/fake"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/sshkey"
	"github.com/deepteams/akerdock/internal/store"
)

// servalcovDial scripts an SSH host and connects a client to it. respond maps
// a command to its stdout and exit status, so each test drives the exact rung
// of the ladder it is about.
func servalcovDial(t *testing.T, respond func(string) (string, uint32)) *sshexec.Client {
	t.Helper()
	srv := deployrunNewSSHServer(t, respond)
	host, port := srv.address(t)
	material, err := sshkey.GenerateEd25519("servalcov")
	if err != nil {
		t.Fatal(err)
	}
	client, err := sshexec.Dial(context.Background(), host, port, "unit", material.PrivatePEM, 5*time.Second, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// servalcovOK answers every command with a clean exit and the outputs the
// proxy bootstrap reads back: a running container, no deployed static config.
func servalcovOK(command string) (string, uint32) {
	if strings.Contains(command, "{{.State.Status}}") {
		return "running\n", 0
	}
	return "", 0
}

// servalcovHost is a host that passes every observation of §20.1: a supported
// architecture, a recent Docker, the pinned nixpacks already in place.
func servalcovHost(command string) (string, uint32) {
	switch {
	case strings.Contains(command, "uname -m"):
		return "x86_64\nUnit Linux\n", 0
	case strings.Contains(command, "command -v docker"):
		return "/usr/bin/docker\n", 0
	case strings.Contains(command, "docker version"):
		return "26.1.0\n", 0
	case strings.Contains(command, nixpacksBin+" --version"):
		return NixpacksVersion + "\n", 0
	}
	return servalcovOK(command)
}

// servalcovValidate wires a ServerValidate whose server row points at a
// scripted SSH host, with both secrets it reads stored under the right AAD:
// the SSH private key and the agent's enrollment token.
func servalcovValidate(t *testing.T, respond func(string) (string, uint32)) (*ServerValidate, *store.Queries, *prevjobsDB) {
	t.Helper()
	q, keyring, logger, db := prevjobsDeps(t)
	material, err := sshkey.GenerateEd25519("servalcov-client")
	if err != nil {
		t.Fatal(err)
	}
	host, port := deployrunNewSSHServer(t, respond).address(t)
	// The fake gives every string column of a row the same value; host is the
	// only one this path reads back.
	db.strs["GetServerByID"] = host
	db.ints["GetServerByID"] = int64(port)
	db.blobs["GetPrivateKeyByID"] = prevjobsEncrypt(t, keyring,
		"private_keys", "private_key_enc", []byte(material.PrivatePEM))
	db.blobs["GetAgentTokenByServerID"] = prevjobsEncrypt(t, keyring,
		"agent_tokens", "token_enc", []byte("akd_agent_unit"))
	h := &ServerValidate{
		Store: q, Keyring: keyring, Logger: logger,
		AgentImage: "akerdock:unit", InstanceURL: "http://cp.unit.test",
		Docker: fixedSource{rt: &dockerfake.Runtime{}}, HostOps: fixedHost{ops: &hostfake.Ops{}},
	}
	return h, q, db
}

// The ADR-079 probe, on its three worlds: a schedulable GPU (card + NVIDIA
// runtime), a card the daemon cannot use (recorded GPU-less, the fix named
// by the caller), and the ordinary GPU-less host.
func TestDetectGPU(t *testing.T) {
	ctx := context.Background()

	probe := func(t *testing.T, smi, runtimes string) gpuFacts {
		t.Helper()
		client := servalcovDial(t, func(command string) (string, uint32) {
			switch {
			case strings.Contains(command, "nvidia-smi --query-gpu"):
				return smi, 0
			case strings.Contains(command, "{{json .Runtimes}}"):
				return runtimes, 0
			}
			return "", 0
		})
		facts, err := detectGPU(ctx, client)
		if err != nil {
			t.Fatal(err)
		}
		return facts
	}

	t.Run("a schedulable GPU records name and memory", func(t *testing.T) {
		facts := probe(t, "NVIDIA GB10, 122880\n", `{"nvidia":{"path":"/usr/bin/nvidia-container-runtime"},"runc":{}}`)
		if facts.name == nil || *facts.name != "NVIDIA GB10" ||
			facts.memoryMB == nil || *facts.memoryMB != 122880 || facts.runtimeMissing {
			t.Fatalf("facts = %+v", facts)
		}
	})

	t.Run("a card without the NVIDIA runtime is GPU-less to the platform", func(t *testing.T) {
		facts := probe(t, "NVIDIA GB10, 122880\n", `{"runc":{}}`)
		if facts.name != nil || !facts.runtimeMissing {
			t.Fatalf("facts = %+v", facts)
		}
	})

	t.Run("no nvidia-smi means no GPU, cleanly", func(t *testing.T) {
		facts := probe(t, "", `{"runc":{}}`)
		if facts.name != nil || facts.runtimeMissing {
			t.Fatalf("facts = %+v", facts)
		}
	})
}

// One validation attempt end to end, and the rungs it falls off. The server
// here routes nothing (proxy_type none), so the bootstrap is skipped by
// intent rather than by failure — that decision has its own test.
func TestServalcovExecute(t *testing.T) {
	ctx := context.Background()
	job := store.Job{ID: 1, JobType: TypeServerValidate, Payload: []byte(`{"server_id":1}`)}

	t.Run("a complete validation reports the facts it observed", func(t *testing.T) {
		h, q, _ := servalcovValidate(t, servalcovHost)
		out, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job))
		if err != nil {
			t.Fatal(err)
		}
		result, _ := out.(map[string]any)
		if result["architecture"] != "amd64" || result["docker_version"] != "26.1.0" ||
			result["os_name"] != "Unit Linux" {
			t.Fatalf("result = %#v", result)
		}
		if fingerprint, _ := result["host_key_fingerprint"].(string); fingerprint == "" {
			t.Fatal("the host key must be pinned and reported")
		}
	})

	t.Run("an invalid payload never reaches the server", func(t *testing.T) {
		h, q, _ := servalcovValidate(t, servalcovHost)
		bad := store.Job{ID: 1, JobType: TypeServerValidate, Payload: []byte(`{`)}
		if _, err := h.Execute(ctx, bad, queue.NewStepRecorder(q, bad)); err == nil {
			t.Fatal("want a payload error")
		}
	})

	t.Run("a private key that vanished marks the server unreachable", func(t *testing.T) {
		h, q, db := servalcovValidate(t, servalcovHost)
		db.errs["GetPrivateKeyByID"] = pgx.ErrNoRows
		if _, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job)); err == nil {
			t.Fatal("want the missing key")
		}
	})

	t.Run("a private key that does not decrypt names the runbook", func(t *testing.T) {
		h, q, db := servalcovValidate(t, servalcovHost)
		db.blobs["GetPrivateKeyByID"] = []byte("not ciphertext")
		if _, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job)); err == nil ||
			!strings.Contains(err.Error(), "envelope:") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an unsupported architecture stops at the facts", func(t *testing.T) {
		h, q, _ := servalcovValidate(t, func(command string) (string, uint32) {
			if strings.Contains(command, "uname -m") {
				return "riscv64\nUnit Linux\n", 0
			}
			return servalcovHost(command)
		})
		_, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job))
		if err == nil || !strings.Contains(err.Error(), "unsupported architecture") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a Docker older than the floor stops the validation", func(t *testing.T) {
		h, q, _ := servalcovValidate(t, func(command string) (string, uint32) {
			if strings.Contains(command, "docker version") {
				return "20.10.7\n", 0
			}
			return servalcovHost(command)
		})
		_, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job))
		if err == nil || !strings.Contains(err.Error(), "too old") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("Docker installed from snap is refused", func(t *testing.T) {
		h, q, _ := servalcovValidate(t, func(command string) (string, uint32) {
			if strings.Contains(command, "command -v docker") {
				return "/snap/bin/docker\n", 0
			}
			return servalcovHost(command)
		})
		_, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job))
		if err == nil || !strings.Contains(err.Error(), "snap") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a use_sudo server wraps every command in sudo -n", func(t *testing.T) {
		// The fake row fills every bool column true, so this server IS
		// use_sudo: the whole validation must ride the ADR-076 escalation.
		log := &deployrunCommandLog{}
		h, q, _ := servalcovValidate(t, func(command string) (string, uint32) {
			log.record(command)
			return servalcovHost(command)
		})
		if _, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job)); err != nil {
			t.Fatal(err)
		}
		if len(log.matching("LC_ALL=C sudo -n -- sh -c '")) != len(log.matching()) {
			t.Fatalf("some commands escaped the sudo wrap: %q", log.matching())
		}
		if len(log.matching("sudo -n -- sh -c 'true'")) == 0 {
			t.Fatal("the check_sudo probe never ran")
		}
	})

	t.Run("without use_sudo no command is escalated", func(t *testing.T) {
		log := &deployrunCommandLog{}
		h, q, db := servalcovValidate(t, func(command string) (string, uint32) {
			log.record(command)
			return servalcovHost(command)
		})
		db.bools["GetServerByID"] = false
		// All-false bools also clear is_build_server, which would send the
		// validation into the proxy bootstrap; route nothing instead.
		db.enums["ProxyType"] = "none"
		if _, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job)); err != nil {
			t.Fatal(err)
		}
		if wrapped := log.matching("sudo -n"); len(wrapped) != 0 {
			t.Fatalf("a non-sudo server saw escalated commands: %q", wrapped)
		}
	})

	t.Run("a use_sudo server that cannot escalate fails its own step", func(t *testing.T) {
		h, q, _ := servalcovValidate(t, func(command string) (string, uint32) {
			if strings.Contains(command, "sudo -n -- sh -c 'true'") {
				return "", 1
			}
			return servalcovHost(command)
		})
		_, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job))
		if err == nil || !strings.Contains(err.Error(), "NOPASSWD") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("nixpacks failing does not fail the validation", func(t *testing.T) {
		h, q, _ := servalcovValidate(t, func(command string) (string, uint32) {
			if strings.Contains(command, nixpacksBin) {
				return "", 1
			}
			return servalcovHost(command)
		})
		// A server that can run image and dockerfile deployments is a usable
		// server: the step is skipped, the job succeeds.
		if _, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job)); err != nil {
			t.Fatal(err)
		}
	})
}

// The DNS-01 credential (proxy-contract §7.2): rendered for Lego as an
// env-file, sorted, and never touched at all by a server without a wildcard.
func TestServalcovDNSCredential(t *testing.T) {
	ctx := context.Background()
	withCredential := store.Server{ID: 1, DnsCredentialID: ptr(int64(1))}

	t.Run("a server without a wildcard has no credential to render", func(t *testing.T) {
		q, keyring, _, _ := prevjobsDeps(t)
		h := &ServerValidate{Store: q, Keyring: keyring}
		provider, env, err := h.dnsCredential(ctx, store.Server{ID: 1})
		if err != nil || provider != "" || env != "" {
			t.Fatalf("provider = %q, env = %q, err = %v", provider, env, err)
		}
	})

	t.Run("the credential vanished", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.errs["GetDNSCredentialByID"] = pgx.ErrNoRows
		h := &ServerValidate{Store: q, Keyring: keyring}
		if _, _, err := h.dnsCredential(ctx, withCredential); err == nil ||
			!strings.Contains(err.Error(), "vanished") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a credential that does not decrypt stops the bootstrap", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.blobs["GetDNSCredentialByID"] = []byte("not ciphertext")
		h := &ServerValidate{Store: q, Keyring: keyring}
		if _, _, err := h.dnsCredential(ctx, withCredential); err == nil ||
			!strings.Contains(err.Error(), "envelope:") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a credential that is not a string map stops the bootstrap", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.blobs["GetDNSCredentialByID"] = prevjobsEncrypt(t, keyring,
			"cloud_credentials", "config_enc", []byte(`["not","a","map"]`))
		h := &ServerValidate{Store: q, Keyring: keyring}
		if _, _, err := h.dnsCredential(ctx, withCredential); err == nil {
			t.Fatal("want a decode error")
		}
	})

	t.Run("the env-file is sorted so it does not recreate the proxy every run", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.strs["GetDNSCredentialByID"] = "cloudflare"
		db.blobs["GetDNSCredentialByID"] = prevjobsEncrypt(t, keyring,
			"cloud_credentials", "config_enc", []byte(`{"CF_TOKEN":"t","CF_EMAIL":"a@b.test"}`))
		h := &ServerValidate{Store: q, Keyring: keyring}
		provider, env, err := h.dnsCredential(ctx, withCredential)
		if err != nil || provider != "cloudflare" {
			t.Fatalf("provider = %q, err = %v", provider, err)
		}
		if env != "CF_EMAIL=a@b.test\nCF_TOKEN=t\n" {
			t.Fatalf("env-file = %q, want the keys in sorted order", env)
		}
	})
}

// The two bits of bookkeeping a validation leaves behind, both idempotent by
// construction.
func TestServalcovDestinationAndUnreachable(t *testing.T) {
	ctx := context.Background()

	t.Run("an existing default destination is left alone", func(t *testing.T) {
		q, keyring, _, _ := prevjobsDeps(t)
		h := &ServerValidate{Store: q, Keyring: keyring}
		if err := h.ensureDefaultDestination(ctx, 1); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("the first validation creates the network destination", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.errs["GetDefaultDestination"] = pgx.ErrNoRows
		h := &ServerValidate{Store: q, Keyring: keyring}
		if err := h.ensureDefaultDestination(ctx, 1); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a refused destination fails the validation", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.errs["GetDefaultDestination"] = pgx.ErrNoRows
		db.errs["CreateDestination"] = fmt.Errorf("constraint violated")
		h := &ServerValidate{Store: q, Keyring: keyring}
		if err := h.ensureDefaultDestination(ctx, 1); err == nil {
			t.Fatal("want the insert failure")
		}
	})

	t.Run("marking unreachable returns the cause, not its own success", func(t *testing.T) {
		q, keyring, _, _ := prevjobsDeps(t)
		h := &ServerValidate{Store: q, Keyring: keyring}
		cause := fmt.Errorf("connection refused")
		if err := h.markUnreachable(ctx, 1, cause); err != cause { //nolint:errorlint // identity is the assertion.
			t.Fatalf("err = %v, want the dial cause", err)
		}
	})

	t.Run("a status write that fails replaces the cause", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.errs["SetServerStatus"] = fmt.Errorf("db gone")
		h := &ServerValidate{Store: q, Keyring: keyring}
		err := h.markUnreachable(ctx, 1, fmt.Errorf("connection refused"))
		if err == nil || !strings.Contains(err.Error(), "db gone") {
			t.Fatalf("err = %v", err)
		}
	})
}

// The pinned build pack binary (§5.5). Its failures never fail a validation,
// but they must be reported honestly to the caller that decides that.
func TestServalcovInstallNixpacks(t *testing.T) {
	ctx := context.Background()
	const versionProbe = "--version 2>/dev/null"

	t.Run("the pinned version already there costs nothing", func(t *testing.T) {
		installed := false
		client := servalcovDial(t, func(command string) (string, uint32) {
			if strings.Contains(command, versionProbe) {
				return NixpacksVersion + "\n", 0 // what the probe's awk prints
			}
			installed = true
			return "", 0
		})
		if err := installNixpacks(ctx, client); err != nil {
			t.Fatal(err)
		}
		if installed {
			t.Fatal("an already-correct binary must not be reinstalled")
		}
	})

	t.Run("another version is converged", func(t *testing.T) {
		client := servalcovDial(t, func(command string) (string, uint32) {
			if strings.Contains(command, versionProbe) {
				return "1.0.0\n", 0
			}
			return "", 0
		})
		if err := installNixpacks(ctx, client); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("an install that fails says why", func(t *testing.T) {
		client := servalcovDial(t, func(command string) (string, uint32) {
			if strings.Contains(command, versionProbe) {
				return "", 0
			}
			return "", 1
		})
		err := installNixpacks(ctx, client)
		if err == nil || !strings.Contains(err.Error(), "nixpacks install failed") {
			t.Fatalf("err = %v", err)
		}
	})
}

// The agent ladder of ADR-054: without it there is no Docker at all, so every
// rung's message has to name what the operator must fix.
func TestServalcovProvisionAgent(t *testing.T) {
	ctx := context.Background()
	server := store.Server{ID: 1, TeamID: 1}

	// enrolled makes the server's agent token readable, which is what turns
	// AgentEnvForServer from "unavailable" into a deployable env.
	enrolled := func(t *testing.T) (*ServerValidate, *prevjobsDB) {
		t.Helper()
		q, keyring, logger, db := prevjobsDeps(t)
		db.blobs["GetAgentTokenByServerID"] = prevjobsEncrypt(t, keyring,
			"agent_tokens", "token_enc", []byte("akd_agent_unit"))
		return &ServerValidate{
			Store: q, Keyring: keyring, Logger: logger,
			AgentImage: "akerdock:unit", InstanceURL: "http://cp.unit.test",
		}, db
	}

	// The wait loop is a var pair precisely so a unit test does not sleep.
	shrinkWait := func(t *testing.T) {
		t.Helper()
		timeout, poll := agentReadyTimeout, agentReadyPoll
		agentReadyTimeout, agentReadyPoll = 0, time.Millisecond
		t.Cleanup(func() { agentReadyTimeout, agentReadyPoll = timeout, poll })
	}

	t.Run("no image means no agent, and no agent means no Docker", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		h := &ServerValidate{Store: q, Keyring: keyring, Logger: logger}
		_, _, err := h.provisionAgent(ctx, servalcovDial(t, servalcovOK), server)
		if err == nil || !strings.Contains(err.Error(), "AKERDOCK_IMAGE") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an agent that cannot dial back is refused before it is deployed", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		// No instance URL and no FQDN: the agent would have nothing to dial.
		h := &ServerValidate{Store: q, Keyring: keyring, Logger: logger, AgentImage: "akerdock:unit"}
		_, _, err := h.provisionAgent(ctx, servalcovDial(t, servalcovOK), server)
		if err == nil || !strings.Contains(err.Error(), "agent enrollment unavailable") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a destination network that cannot be created stops the provisioning", func(t *testing.T) {
		h, _ := enrolled(t)
		client := servalcovDial(t, func(command string) (string, uint32) {
			if strings.Contains(command, "docker network") {
				return "", 1
			}
			return servalcovOK(command)
		})
		_, _, err := h.provisionAgent(ctx, client, server)
		if err == nil || !strings.Contains(err.Error(), "destination network") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an unpullable image names the seed remediation", func(t *testing.T) {
		// The source-only install case (ADR-078): the tag lives only in the
		// instance host's daemon, the server's docker run tries a registry
		// pull that can only fail — the error must say what to run, where.
		h, _ := enrolled(t)
		client := servalcovDial(t, func(command string) (string, uint32) {
			if strings.Contains(command, "docker run -d --name "+proxy.AgentContainerName) {
				return "docker: Error response from daemon: pull access denied for akerdock", 125
			}
			return servalcovOK(command)
		})
		_, _, err := h.provisionAgent(ctx, client, server)
		if err == nil || !strings.Contains(err.Error(), "install.sh seed") || !strings.Contains(err.Error(), "akerdock:unit") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an unpullable image with a known commit asks whether it is pushed", func(t *testing.T) {
		// The nominal ADR-078 lane: the server builds the image from the
		// public repository at the instance's commit; the one way that fails
		// on a healthy server is a commit the repository does not hold yet.
		restoreRepo, restoreCommit := agentSource.Repo, agentSource.Commit
		t.Cleanup(func() { SetAgentSource(restoreRepo, restoreCommit) })
		SetAgentSource("https://github.com/deepteams/akerdock.git", "0123abc")
		h, _ := enrolled(t)
		client := servalcovDial(t, func(command string) (string, uint32) {
			if strings.Contains(command, "docker build") {
				return "fatal: could not find remote ref 0123abc", 128
			}
			return servalcovOK(command)
		})
		_, _, err := h.provisionAgent(ctx, client, server)
		if err == nil || !strings.Contains(err.Error(), "is that commit pushed") ||
			!strings.Contains(err.Error(), "0123abc") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a deploy that exits non-zero names the exit code", func(t *testing.T) {
		h, _ := enrolled(t)
		client := servalcovDial(t, func(command string) (string, uint32) {
			if strings.Contains(command, "docker run") || strings.Contains(command, "akerdock-agent") {
				return "", 3
			}
			return servalcovOK(command)
		})
		_, _, err := h.provisionAgent(ctx, client, server)
		if err == nil || !strings.Contains(err.Error(), "agent deploy failed") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a channel that never answers is a timeout, not a hang", func(t *testing.T) {
		shrinkWait(t)
		h, _ := enrolled(t)
		h.Docker, h.HostOps = unavailableDocker{}, unavailableHost{}
		_, _, err := h.provisionAgent(ctx, servalcovDial(t, servalcovOK), server)
		if err == nil || !strings.Contains(err.Error(), "did not connect within") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a channel that answers hands back both seams", func(t *testing.T) {
		shrinkWait(t)
		h, _ := enrolled(t)
		h.Docker = fixedSource{rt: &dockerfake.Runtime{}}
		h.HostOps = fixedHost{ops: &hostfake.Ops{}}
		rt, gotOps, err := h.provisionAgent(ctx, servalcovDial(t, servalcovOK), server)
		if err != nil || rt == nil || gotOps == nil {
			t.Fatalf("rt = %v, ops = %v, err = %v", rt, gotOps, err)
		}
	})
}

// RepairProxy is bootstrapProxy with no agent channel (ADR-062 §3): its whole
// point is to work when every in-product path is closed, so it must converge
// the container over SSH alone and stop short of the routing that needs the
// channel.
func TestServalcovRepairProxy(t *testing.T) {
	ctx := context.Background()
	server := store.Server{
		ID: 1, TeamID: 1, ProxyType: store.ProxyTypeTraefik,
		ProxyHttpPort: 80, ProxyHttpsPort: 443,
	}

	t.Run("no ACME contact stops before anything is written", func(t *testing.T) {
		q, keyring, _, _ := prevjobsDeps(t)
		// Neither acme_email nor the instance FQDN: the failure would otherwise
		// be silent three weeks later, when no certificate ever arrives.
		err := RepairProxy(ctx, q, keyring, servalcovDial(t, servalcovOK), server, 8000)
		if err == nil || !strings.Contains(err.Error(), "no ACME contact email") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("the instance FQDN is the last-resort contact", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.fillPtr["GetInstanceSettings"] = true // acme_email and fqdn both set
		db.strs["GetInstanceSettings"] = "akerdock.unit.test"
		if err := RepairProxy(ctx, q, keyring, servalcovDial(t, servalcovOK), server, 8000); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a proxy that does not come up reports what it is instead", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.fillPtr["GetInstanceSettings"] = true
		db.strs["GetInstanceSettings"] = "ops@unit.test"
		client := servalcovDial(t, func(command string) (string, uint32) {
			if strings.Contains(command, "{{.State.Status}}") {
				return "exited\n", 0
			}
			return "", 0
		})
		err := RepairProxy(ctx, q, keyring, client, server, 8000)
		if err == nil || !strings.Contains(err.Error(), `is "exited", expected running`) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a port already taken is named, not left as a bind error", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.fillPtr["GetInstanceSettings"] = true
		db.strs["GetInstanceSettings"] = "ops@unit.test"
		client := servalcovDial(t, func(command string) (string, uint32) {
			switch {
			case strings.Contains(command, "{{.State.Status}}"):
				return "created\n", 0
			case strings.Contains(command, "docker start"):
				return "Error: port is already allocated\n", 0
			}
			return "", 0
		})
		err := RepairProxy(ctx, q, keyring, client, server, 8000)
		if err == nil || !strings.Contains(err.Error(), "already listens on port 80 or 443") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("the layout cannot be written by the SSH user", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.fillPtr["GetInstanceSettings"] = true
		db.strs["GetInstanceSettings"] = "ops@unit.test"
		client := servalcovDial(t, func(command string) (string, uint32) {
			if strings.Contains(command, "mkdir -p /var/lib/akerdock/proxy") {
				return "", 1
			}
			return servalcovOK(command)
		})
		err := RepairProxy(ctx, q, keyring, client, server, 8000)
		if err == nil || !strings.Contains(err.Error(), "proxy layout") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a server with no default destination cannot be repaired", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.errs["GetDefaultDestination"] = pgx.ErrNoRows
		if err := RepairProxy(ctx, q, keyring, servalcovDial(t, servalcovOK), server, 8000); err == nil {
			t.Fatal("want the destination failure")
		}
	})
}
