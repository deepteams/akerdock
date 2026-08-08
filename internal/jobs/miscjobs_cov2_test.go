// Execute-level coverage for the ingress-routing, proxy-lifecycle,
// certificate, scheduled-task and application lifecycle/delete jobs, on the
// shared job-flow scaffolding. All identifiers are prefixed miscjobs.
package jobs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	networktypes "github.com/docker/docker/api/types/network"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/deepteams/akerdock/internal/agent"
	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// miscjobsExecRT scripts the exec cycle with a fixed exit code and output.
func miscjobsExecRT(exit int, output string) *fake.Runtime {
	rt := &fake.Runtime{}
	rt.ContainerExecCreateFn = func(c context.Context, _ string, _ containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
		if c.Err() != nil {
			return containertypes.ExecCreateResponse{}, c.Err()
		}
		return containertypes.ExecCreateResponse{ID: "exec"}, nil
	}
	rt.ContainerExecAttachFn = func(context.Context, string, containertypes.ExecAttachOptions) (types.HijackedResponse, error) {
		var buf bytes.Buffer
		_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stdout).Write([]byte(output))
		client, server := net.Pipe()
		go func() {
			_, _ = server.Write(buf.Bytes())
			_ = server.Close()
		}()
		return types.HijackedResponse{Conn: client, Reader: bufio.NewReader(client)}, nil
	}
	rt.ContainerExecInspectFn = func(context.Context, string) (containertypes.ExecInspect, error) {
		return containertypes.ExecInspect{ExitCode: exit}, nil
	}
	return rt
}

// --- ingressrouting.go -------------------------------------------------------

func miscjobsIngressJob(payload string) store.Job {
	return store.Job{ID: 11, JobType: TypeIngressRouting, Payload: []byte(payload)}
}

func TestMiscjobsIngressRoutingConverges(t *testing.T) {
	q, _, logger, db := miscjobsDeps(t)
	miscjobsEnum(t, "IngressAccess", string(store.IngressAccessNone))
	wildcard := "*.apps.example.test"
	credID := int64(1)
	db.override = func(sql string, index int, dest any) {
		if strings.Contains(sql, "-- name: GetServerByID ") {
			switch index {
			case 19: // wildcard_domain: the endpoint rides the DNS-01 wildcard
				value := wildcard
				*(dest.(**string)) = &value
			case 49: // dns_credential_id
				*(dest.(**int64)) = &credID
			}
		}
	}
	rt := verifyRuntime(jobFixtureUUID + " http://" + proxy.AgentContainerName + ":8080")
	ops := &hostfake.Ops{}
	h := &IngressRouting{Store: q, Docker: fixedSource{rt: rt}, HostOps: fixedHost{ops: ops}, Logger: logger}
	j := miscjobsIngressJob(`{"endpoint_id":1,"endpoint_uuid":"` + jobFixtureUUID + `","server_id":1}`)
	result, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j))
	if err != nil {
		t.Fatalf("ingress converge: %v", err)
	}
	out := result.(map[string]any)
	if out["endpoint_uuid"] != jobFixtureUUID || out["fqdn"] != "unit" {
		t.Fatalf("result = %#v", out)
	}
	// The dynamic file landed and the agent's host table was rewritten.
	var sawRoute, sawTable bool
	for _, c := range ops.CallsTo(agentwire.MethodFileWrite) {
		p := c.(agentwire.FileWriteParams)
		switch {
		case strings.HasPrefix(p.Path, "/var/lib/akerdock/proxy/dynamic/"):
			sawRoute = true
			if !strings.Contains(string(p.Content), "dnschallenge") &&
				!strings.Contains(strings.ToLower(string(p.Content)), "unit") {
				t.Fatalf("routing content = %s", p.Content)
			}
		case p.Path == wakerDir+"/"+agent.RoutesFile:
			sawTable = true
			if !strings.Contains(string(p.Content), jobFixtureUUID) {
				t.Fatalf("host table = %s", p.Content)
			}
		}
	}
	if !sawRoute || !sawTable {
		t.Fatalf("writes = %v", ops.Calls())
	}
}

func TestMiscjobsIngressRoutingRemovesAfterDeletion(t *testing.T) {
	q, _, logger, db := miscjobsDeps(t)
	db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetIngressEndpointByID")
	ops := &hostfake.Ops{ReadFileFn: func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error) {
		raw, _ := agent.MarshalConfig(agent.Config{Ingress: []agent.IngressRoute{
			{Host: "keep.example.test", EndpointUUID: "other"},
			{Host: "gone.example.test", EndpointUUID: jobFixtureUUID},
		}})
		return agentwire.FileReadResult{Found: true, Content: raw}, nil
	}}
	h := &IngressRouting{Store: q, Docker: fixedSource{rt: verifyRuntime("")}, HostOps: fixedHost{ops: ops}, Logger: logger}
	j := miscjobsIngressJob(`{"endpoint_id":1,"endpoint_uuid":"` + jobFixtureUUID + `","server_id":1}`)
	result, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j))
	if err != nil {
		t.Fatalf("ingress removal: %v", err)
	}
	if result.(map[string]any)["status"] != "removed" {
		t.Fatalf("result = %#v", result)
	}
	writes := ops.CallsTo(agentwire.MethodFileWrite)
	if len(writes) != 1 {
		t.Fatalf("writes = %v", writes)
	}
	table := string(writes[0].(agentwire.FileWriteParams).Content)
	if strings.Contains(table, jobFixtureUUID) || !strings.Contains(table, "other") {
		t.Fatalf("host table after removal = %s", table)
	}
}

func TestMiscjobsIngressRoutingFailureVerdicts(t *testing.T) {
	ctx := context.Background()
	j := miscjobsIngressJob(`{"endpoint_id":1,"endpoint_uuid":"` + jobFixtureUUID + `","server_id":1}`)

	t.Run("invalid payload", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		h := &IngressRouting{Store: q, Logger: logger}
		if _, err := h.Execute(ctx, store.Job{Payload: []byte("{")}, nil); err == nil ||
			!strings.Contains(err.Error(), "invalid payload") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unmanaged proxy", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		miscjobsEnum(t, "ProxyType", "none")
		h := &IngressRouting{Store: q, Logger: logger}
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["status"] != "server has no managed proxy" {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("server vanished", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetServerByID")
		h := &IngressRouting{Store: q, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("missing server must fail")
		}
	})
	t.Run("no default destination", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetDefaultDestination")
		h := &IngressRouting{Store: q, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "no default destination") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("agent not connected", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		h := &IngressRouting{Store: q, Docker: unavailableDocker{}, HostOps: unavailableHost{}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("unavailable agent must fail")
		}
	})
	t.Run("host ops not connected", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		h := &IngressRouting{Store: q, Docker: fixedSource{rt: &fake.Runtime{}},
			HostOps: fixedHost{err: errors.New("not connected")}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("unavailable host ops must fail")
		}
	})
	t.Run("unrenderable wall fails closed", func(t *testing.T) {
		// The fixture's access value is not a known wall: never rendered public.
		q, _, logger, _ := miscjobsDeps(t)
		h := &IngressRouting{Store: q, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "unsupported ingress access") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("apply failure", func(t *testing.T) {
		miscjobsShortVerify(t)
		q, _, logger, _ := miscjobsDeps(t)
		miscjobsEnum(t, "IngressAccess", string(store.IngressAccessNone))
		ops := &hostfake.Ops{WriteFileFn: func(context.Context, agentwire.FileWriteParams) error {
			return errors.New("disk full")
		}}
		h := &IngressRouting{Store: q, Docker: fixedSource{rt: verifyRuntime("")}, HostOps: fixedHost{ops: ops}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("apply failure must fail the job")
		}
	})
	t.Run("host table deposit failure", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetIngressEndpointByID")
		ops := &hostfake.Ops{WriteFileFn: func(_ context.Context, p agentwire.FileWriteParams) error {
			if p.Path == wakerDir+"/"+agent.RoutesFile {
				return errors.New("agent tree read-only")
			}
			return nil
		}}
		h := &IngressRouting{Store: q, Docker: fixedSource{rt: verifyRuntime("")}, HostOps: fixedHost{ops: ops}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "waker routes deposit failed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("deposit failure after apply", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		miscjobsEnum(t, "IngressAccess", string(store.IngressAccessNone))
		ops := &hostfake.Ops{WriteFileFn: func(_ context.Context, p agentwire.FileWriteParams) error {
			if p.Path == wakerDir+"/"+agent.RoutesFile {
				return errors.New("agent tree read-only")
			}
			return nil
		}}
		rt := verifyRuntime(jobFixtureUUID + " http://" + proxy.AgentContainerName + ":8080")
		h := &IngressRouting{Store: q, Docker: fixedSource{rt: rt}, HostOps: fixedHost{ops: ops}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "waker routes deposit failed") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMiscjobsIngressAccessPolicy(t *testing.T) {
	ctx := context.Background()
	uuid := mustUUID(t, jobFixtureUUID)
	hash := "user:$2y$05$hash"

	if p, err := ingressAccessPolicy(ctx, nil, store.IngressEndpoint{Access: store.IngressAccessNone}, store.Server{}, 0); p != nil || err != nil {
		t.Fatalf("none = %#v, %v", p, err)
	}
	if _, err := ingressAccessPolicy(ctx, nil, store.IngressEndpoint{Access: store.IngressAccessBasicAuth}, store.Server{}, 0); err == nil ||
		!strings.Contains(err.Error(), "no configured credentials") {
		t.Fatalf("empty basic auth = %v", err)
	}
	p, err := ingressAccessPolicy(ctx, nil, store.IngressEndpoint{Access: store.IngressAccessBasicAuth, BasicAuthHash: &hash}, store.Server{}, 0)
	if err != nil || p.Mode != "basic_auth" || p.BasicAuthHash != hash {
		t.Fatalf("basic auth = %#v, %v", p, err)
	}
	if _, err := ingressAccessPolicy(ctx, nil, store.IngressEndpoint{Access: store.IngressAccess("weird")}, store.Server{}, 0); err == nil ||
		!strings.Contains(err.Error(), "unsupported ingress access") {
		t.Fatalf("unknown wall = %v", err)
	}

	q, _, _, db := miscjobsDeps(t)
	sso := store.IngressEndpoint{Uuid: uuid, Access: store.IngressAccessSso}
	if _, err := ingressAccessPolicy(ctx, q, sso, store.Server{}, 0); err == nil ||
		!strings.Contains(err.Error(), "instance FQDN") {
		t.Fatalf("sso without FQDN = %v", err)
	}
	db.override = func(sql string, index int, dest any) {
		if strings.Contains(sql, "-- name: GetInstanceSettings ") && index == 1 {
			value := "akerdock.example.test"
			*(dest.(**string)) = &value
		}
	}
	p, err = ingressAccessPolicy(ctx, q, sso, store.Server{}, 0)
	if err != nil || p.Mode != "sso" ||
		p.ForwardAuthURL != "https://akerdock.example.test/webhooks/ingress/forward-auth?endpoint="+jobFixtureUUID ||
		p.CallbackPath != "/.akerdock/ingress-callback" {
		t.Fatalf("sso = %#v, %v", p, err)
	}
	p, err = ingressAccessPolicy(ctx, q, sso, store.Server{IsLocalhost: true}, 9080)
	if err != nil || !strings.HasPrefix(p.ForwardAuthURL, "http://host.docker.internal:9080/") {
		t.Fatalf("localhost sso = %#v, %v", p, err)
	}
	db.rowErr = miscjobsFailOn(errors.New("settings down"), "GetInstanceSettings")
	if _, err := ingressAccessPolicy(ctx, q, sso, store.Server{}, 0); err == nil ||
		!strings.Contains(err.Error(), "settings down") {
		t.Fatalf("settings error = %v", err)
	}
}

func TestMiscjobsIngressRouteEntryMergeAndRemove(t *testing.T) {
	base := agent.Config{
		Routes:  []agent.Route{{Host: "app.example.test", ResourceUUID: "res"}},
		Ingress: []agent.IngressRoute{{Host: "old.example.test", EndpointUUID: "e1"}, {Host: "other", EndpointUUID: "e2"}},
	}
	merged := mergeIngressRouteEntry(base, "new.example.test", "e1")
	if len(merged.Ingress) != 2 || merged.Ingress[1].Host != "new.example.test" || len(merged.Routes) != 1 {
		t.Fatalf("merged = %#v", merged)
	}
	removed := removeIngressRouteEntry(merged, "e2")
	if len(removed.Ingress) != 1 || removed.Ingress[0].EndpointUUID != "e1" {
		t.Fatalf("removed = %#v", removed)
	}
}

// --- proxylifecycle.go -------------------------------------------------------

func miscjobsProxyRT(status string) *fake.Runtime {
	rt := verifyRuntime("")
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Status: status},
		}}, nil
	}
	return rt
}

func miscjobsProxyJob(action string) store.Job {
	return store.Job{ID: 12, JobType: "proxy." + action, Payload: []byte(`{"server_id":1,"action":"` + action + `"}`)}
}

func TestMiscjobsProxyLifecycleStopAndRestart(t *testing.T) {
	ctx := context.Background()

	t.Run("stop", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		rt := miscjobsProxyRT("exited")
		h := &ProxyLifecycle{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsProxyJob("stop")
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
		if result.(map[string]any)["proxy_status"] != "exited" {
			t.Fatalf("result = %#v", result)
		}
	})
	t.Run("restart", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		rt := miscjobsProxyRT("running")
		h := &ProxyLifecycle{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsProxyJob("restart")
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
		if result.(map[string]any)["proxy_status"] != "running" {
			t.Fatalf("result = %#v", result)
		}
	})
	t.Run("restart failure marks unhealthy", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		rt := miscjobsProxyRT("running")
		rt.ContainerRestartFn = func(context.Context, string, containertypes.StopOptions) error {
			return errors.New("restart refused\ndetail")
		}
		h := &ProxyLifecycle{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsProxyJob("restart")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "restart refused") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("restart lands not running", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		rt := miscjobsProxyRT("restarting")
		h := &ProxyLifecycle{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsProxyJob("restart")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), `the proxy is "restarting" after restart`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("stop of a proxy never created", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		rt := &fake.Runtime{}
		rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return containertypes.InspectResponse{}, miscjobsNotFound("container")
		}
		h := &ProxyLifecycle{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsProxyJob("stop")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "press Start") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("stop with a broken daemon", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		rt := &fake.Runtime{}
		rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return containertypes.InspectResponse{}, errors.New("daemon down")
		}
		h := &ProxyLifecycle{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsProxyJob("stop")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "daemon down") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("status inspect failure after the action", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		rt := &fake.Runtime{}
		calls := 0
		rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			calls++
			if calls == 1 {
				return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
					State: &containertypes.State{Status: "running"},
				}}, nil
			}
			return containertypes.InspectResponse{}, errors.New("daemon flaked")
		}
		h := &ProxyLifecycle{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsProxyJob("restart")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "daemon flaked") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown action", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		h := &ProxyLifecycle{Store: q, Keyring: keyring, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsProxyJob("reboot")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "unknown proxy action") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unmanaged proxy", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		miscjobsEnum(t, "ProxyType", "none")
		h := &ProxyLifecycle{Store: q, Keyring: keyring, Logger: logger}
		j := miscjobsProxyJob("stop")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "no managed proxy") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("host ops not connected", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		h := &ProxyLifecycle{Store: q, Keyring: keyring, Docker: fixedSource{rt: &fake.Runtime{}},
			HostOps: fixedHost{err: errors.New("not connected")}, Logger: logger}
		j := miscjobsProxyJob("stop")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("unavailable host ops must fail")
		}
	})
}

func TestMiscjobsProxyLifecycleStartBootstrapsOverSSH(t *testing.T) {
	q, keyring, logger, db := miscjobsDeps(t)
	db.host, db.port = newJobSSHServer(t).address(t)
	// The bootstrap requires an ACME contact (§4.3) — inject one.
	db.override = func(sql string, index int, dest any) {
		if strings.Contains(sql, "-- name: GetInstanceSettings ") && index == 14 {
			value := "ops@example.test"
			*(dest.(**string)) = &value
		}
	}
	rt := miscjobsProxyRT("running")
	h := &ProxyLifecycle{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}
	j := miscjobsProxyJob("start")
	result, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if result.(map[string]any)["proxy_status"] != "running" {
		t.Fatalf("result = %#v", result)
	}
}

func TestMiscjobsProxyLifecycleStartFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("ssh connection failed", func(t *testing.T) {
		q, keyring, logger, db := miscjobsDeps(t)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		host, port, _ := net.SplitHostPort(listener.Addr().String())
		_ = listener.Close()
		db.host = host
		fmt.Sscan(port, &db.port)
		h := &ProxyLifecycle{Store: q, Keyring: keyring, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsProxyJob("start")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("a dead SSH endpoint must fail the start")
		}
	})
	t.Run("bootstrap refused without ACME contact", func(t *testing.T) {
		q, keyring, logger, db := miscjobsDeps(t)
		db.host, db.port = newJobSSHServer(t).address(t)
		h := &ProxyLifecycle{Store: q, Keyring: keyring, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsProxyJob("start")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "ACME contact email") {
			t.Fatalf("err = %v", err)
		}
	})
}

// --- certificates.go ---------------------------------------------------------

func miscjobsCertJob(jobType, payload string) store.Job {
	return store.Job{ID: 13, JobType: jobType, Payload: []byte(payload)}
}

func TestMiscjobsCertificateSyncReflectsAcmeStore(t *testing.T) {
	q, _, logger, db := miscjobsDeps(t)
	_ = db
	valid := miscjobsSelfSigned(t, time.Now().Add(30*24*time.Hour))
	expired := miscjobsSelfSigned(t, time.Now().Add(-time.Hour))
	acme := `{"http01":{"Certificates":[
		{"domain":{"main":"Site.Example.Test","sans":["www.example.test"]},"certificate":"` + valid + `"},
		{"domain":{"main":""}},
		{"domain":{"main":"expired.example.test"},"certificate":"` + expired + `"},
		{"domain":{"main":"pending.example.test"},"certificate":"!!not base64!!"}
	]}}`
	ops := &hostfake.Ops{ReadFileFn: func(_ context.Context, p agentwire.FileReadParams) (agentwire.FileReadResult, error) {
		if p.Path != acmePath {
			t.Fatalf("read path = %q", p.Path)
		}
		return agentwire.FileReadResult{Found: true, Content: []byte(acme)}, nil
	}}
	h := &CertificateSync{Store: q, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{ops: ops}, Logger: logger}
	j := miscjobsCertJob(TypeCertificateSync, `{"server_id":1}`)
	result, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.(map[string]any)["certificates"] != 3 {
		t.Fatalf("result = %#v", result)
	}
}

func TestMiscjobsCertificateSyncEdgeCases(t *testing.T) {
	ctx := context.Background()
	j := miscjobsCertJob(TypeCertificateSync, `{"server_id":1}`)

	t.Run("uninitialized acme store", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		h := &CertificateSync{Store: q, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{}, Logger: logger}
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["certificates"] != 0 {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("unreadable acme store is not an error", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		ops := &hostfake.Ops{ReadFileFn: func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error) {
			return agentwire.FileReadResult{Found: true, Content: []byte("{half-written")}, nil
		}}
		h := &CertificateSync{Store: q, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{ops: ops}, Logger: logger}
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["certificates"] != 0 {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("read failure fails the job", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		ops := &hostfake.Ops{ReadFileFn: func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error) {
			return agentwire.FileReadResult{}, errors.New("agent gone")
		}}
		h := &CertificateSync{Store: q, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{ops: ops}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "agent gone") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("upsert failure surfaces", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.execErr = miscjobsFailOn(errors.New("insert down"), "UpsertCertificate")
		valid := miscjobsSelfSigned(t, time.Now().Add(time.Hour))
		ops := &hostfake.Ops{ReadFileFn: func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error) {
			content := `{"http01":{"Certificates":[{"domain":{"main":"a.example.test"},"certificate":"` + valid + `"}]}}`
			return agentwire.FileReadResult{Found: true, Content: []byte(content)}, nil
		}}
		h := &CertificateSync{Store: q, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{ops: ops}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "insert down") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("server vanished", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetServerByID")
		h := &CertificateSync{Store: q, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "server not found") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("host ops not connected", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		h := &CertificateSync{Store: q, HostOps: unavailableHost{}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("unavailable host ops must fail")
		}
	})
}

func TestMiscjobsCertificateRenew(t *testing.T) {
	ctx := context.Background()
	j := miscjobsCertJob(TypeCertificateRenew, `{"server_id":1,"certificate_id":1}`)

	t.Run("renewal triggered then synced", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		rt := &fake.Runtime{}
		restarted := false
		rt.ContainerRestartFn = func(_ context.Context, name string, _ containertypes.StopOptions) error {
			if name != proxy.ContainerName {
				t.Fatalf("restarted %q", name)
			}
			restarted = true
			return nil
		}
		ops := &hostfake.Ops{}
		h := &CertificateSync{Store: q, Docker: fixedSource{rt: rt}, HostOps: fixedHost{ops: ops}, Logger: logger}
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || !restarted {
			t.Fatalf("renew: %v (restarted=%v)", err, restarted)
		}
		if result.(map[string]any)["certificates"] != 0 {
			t.Fatalf("result = %#v", result)
		}
		// The ACME store was backed up before the restart.
		if copies := ops.CallsTo(agentwire.MethodFileCopy); len(copies) != 1 ||
			copies[0].(agentwire.FileCopyParams).Dst != acmePath+".bak" {
			t.Fatalf("backup calls = %v", copies)
		}
	})
	t.Run("restart failure marks the certificate failed", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		rt := &fake.Runtime{}
		rt.ContainerRestartFn = func(context.Context, string, containertypes.StopOptions) error {
			return errors.New("proxy wedged")
		}
		h := &CertificateSync{Store: q, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "could not restart the proxy") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("agent channel unavailable for the restart", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		h := &CertificateSync{Store: q, Docker: unavailableDocker{}, HostOps: fixedHost{}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("unavailable runtime must fail the renewal")
		}
	})
	t.Run("certificate vanished", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetCertificateByID")
		h := &CertificateSync{Store: q, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "certificate not found") {
			t.Fatalf("err = %v", err)
		}
	})
}

// --- scheduledtask.go --------------------------------------------------------

func miscjobsTaskJob() store.Job {
	return store.Job{ID: 14, JobType: TypeScheduledTaskRun, Payload: []byte(`{"task_id":1,"execution_id":1}`)}
}

func miscjobsTaskHandler(t *testing.T, rt *fake.Runtime) (*ScheduledTaskRun, *store.Queries, *miscjobsDB) {
	t.Helper()
	q, _, logger, db := miscjobsDeps(t)
	return &ScheduledTaskRun{
		Store: q, Docker: fixedSource{rt: rt},
		Audit:  &audit.Recorder{Store: q, Logger: logger},
		Logger: logger,
	}, q, db
}

func TestMiscjobsScheduledTaskOutcomes(t *testing.T) {
	ctx := context.Background()

	t.Run("success", func(t *testing.T) {
		h, q, db := miscjobsTaskHandler(t, miscjobsExecRT(0, "all good\n"))
		// A configured container name wins over the resource UUID (INV-011).
		db.override = func(sql string, index int, dest any) {
			if strings.Contains(sql, "-- name: GetScheduledTaskByID ") && index == 4 {
				value := "custom-target"
				*(dest.(**string)) = &value
			}
		}
		j := miscjobsTaskJob()
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["status"] != "succeeded" {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("command failure is a result, not a retry", func(t *testing.T) {
		h, q, _ := miscjobsTaskHandler(t, miscjobsExecRT(3, "boom\n"))
		j := miscjobsTaskJob()
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["status"] != "failed" ||
			result.(map[string]any)["exit_code"] != 3 {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("exec error closes the execution", func(t *testing.T) {
		rt := &fake.Runtime{}
		rt.ContainerExecCreateFn = func(context.Context, string, containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
			return containertypes.ExecCreateResponse{}, errors.New("exec refused\ndetail")
		}
		h, q, _ := miscjobsTaskHandler(t, rt)
		j := miscjobsTaskJob()
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["status"] != "failed" ||
			result.(map[string]any)["reason"] != "exec refused" {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("timeout names the budget", func(t *testing.T) {
		rt := &fake.Runtime{}
		rt.ContainerExecCreateFn = func(c context.Context, _ string, _ containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
			<-c.Done()
			return containertypes.ExecCreateResponse{}, c.Err()
		}
		h, q, db := miscjobsTaskHandler(t, rt)
		db.override = func(sql string, index int, dest any) {
			if strings.Contains(sql, "-- name: GetScheduledTaskByID ") && index == 12 {
				*(dest.(*int32)) = 0 // an already-expired budget: no waiting
			}
		}
		j := miscjobsTaskJob()
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || !strings.Contains(result.(map[string]any)["reason"].(string), "exceeded its timeout of 0s") {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("agent not connected", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		h := &ScheduledTaskRun{Store: q, Docker: unavailableDocker{}, Logger: logger}
		j := miscjobsTaskJob()
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("unavailable agent must fail")
		}
	})
	t.Run("history write failure surfaces", func(t *testing.T) {
		h, q, db := miscjobsTaskHandler(t, miscjobsExecRT(0, ""))
		db.execErr = miscjobsFailOn(errors.New("history down"), "FinishTaskExecution")
		j := miscjobsTaskJob()
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "history down") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("row failures", func(t *testing.T) {
		for _, name := range []string{"GetScheduledTaskByID", "GetApplicationByID", "GetDestinationByID", "GetServerByID"} {
			h, q, db := miscjobsTaskHandler(t, miscjobsExecRT(0, ""))
			db.rowErr = miscjobsFailOn(errors.New("no rows"), name)
			j := miscjobsTaskJob()
			if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
				t.Fatalf("%s failure must fail the job", name)
			}
		}
	})
}

func TestMiscjobsScheduledTaskFailAndPublishHelpers(t *testing.T) {
	ctx := context.Background()
	q, _, logger, db := miscjobsDeps(t)
	// The failure closer logs and swallows a history-write error: the queue
	// must never retry an operator's command because bookkeeping failed.
	db.execErr = miscjobsFailOn(errors.New("history down"), "FinishTaskExecution")
	h := &ScheduledTaskRun{Store: q, Logger: logger}
	h.fail(ctx, 1, nil, "reason")

	// Without an audit recorder, publish is a silent no-op.
	h.publish(ctx, store.ScheduledTask{}, mustUUID(t, jobFixtureUUID), "scheduled_task.failed.v1", map[string]any{})

	// A vanished team never blocks the notification.
	db.execErr = nil
	db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetTeamByID")
	h.Audit = &audit.Recorder{Store: q, Logger: logger}
	h.publish(ctx, store.ScheduledTask{Uuid: mustUUID(t, jobFixtureUUID)},
		mustUUID(t, jobFixtureUUID), "scheduled_task.failed.v1", map[string]any{})
}

// --- applicationlifecycle.go -------------------------------------------------

func miscjobsAppLifecycleJob(action string) store.Job {
	return store.Job{ID: 15, JobType: "application." + action, Payload: []byte(`{"resource_id":1,"action":"` + action + `"}`)}
}

func TestMiscjobsApplicationLifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("start", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		rt := &fake.Runtime{}
		var started string
		rt.ContainerStartFn = func(_ context.Context, name string, _ containertypes.StartOptions) error {
			started = name
			return nil
		}
		h := &ApplicationLifecycle{Store: q, Docker: fixedSource{rt: rt}, Logger: logger}
		j := miscjobsAppLifecycleJob("start")
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["action"] != "start" {
			t.Fatalf("result = %#v, %v", result, err)
		}
		if started != jobFixtureUUID {
			t.Fatalf("started = %q — the container is the resource UUID (INV-011)", started)
		}
	})
	t.Run("stop", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		rt := &fake.Runtime{}
		h := &ApplicationLifecycle{Store: q, Docker: fixedSource{rt: rt}, Logger: logger}
		j := miscjobsAppLifecycleJob("stop")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err != nil {
			t.Fatalf("stop: %v", err)
		}
	})
	t.Run("stack lifecycle by labels", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetApplicationByID")
		miscjobsEnum(t, "ResourceType", string(store.ResourceTypeService))
		rt := &fake.Runtime{}
		rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
			return []containertypes.Summary{{Names: []string{"/svc-web"}}, {Names: nil}}, nil
		}
		var grace *int
		rt.ContainerRestartFn = func(_ context.Context, _ string, opts containertypes.StopOptions) error {
			grace = opts.Timeout
			return nil
		}
		h := &ApplicationLifecycle{Store: q, Docker: fixedSource{rt: rt}, Logger: logger}
		j := miscjobsAppLifecycleJob("restart")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err != nil {
			t.Fatalf("stack restart: %v", err)
		}
		if grace == nil || *grace != 30 {
			t.Fatalf("grace = %v — a stack without a configured grace defaults to 30", grace)
		}
	})
	t.Run("vanished application", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetApplicationByID", "GetResourceByID")
		h := &ApplicationLifecycle{Store: q, Logger: logger}
		j := miscjobsAppLifecycleJob("start")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "application vanished") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("non-service resource", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetApplicationByID")
		h := &ApplicationLifecycle{Store: q, Logger: logger}
		j := miscjobsAppLifecycleJob("start")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "application vanished") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown action", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		h := &ApplicationLifecycle{Store: q, Logger: logger}
		j := miscjobsAppLifecycleJob("reboot")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "unknown lifecycle action") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing container names the fix", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		rt := &fake.Runtime{}
		rt.ContainerStartFn = func(context.Context, string, containertypes.StartOptions) error {
			return miscjobsNotFound("container")
		}
		h := &ApplicationLifecycle{Store: q, Docker: fixedSource{rt: rt}, Logger: logger}
		j := miscjobsAppLifecycleJob("start")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "deploy it first") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("daemon error keeps its first line", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		rt := &fake.Runtime{}
		rt.ContainerStartFn = func(context.Context, string, containertypes.StartOptions) error {
			return errors.New("start refused\nsecond line")
		}
		h := &ApplicationLifecycle{Store: q, Docker: fixedSource{rt: rt}, Logger: logger}
		j := miscjobsAppLifecycleJob("start")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "start refused") || strings.Contains(err.Error(), "second line") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("dependency row failures", func(t *testing.T) {
		for _, name := range []string{"GetDestinationByID", "GetServerByID"} {
			q, _, logger, db := miscjobsDeps(t)
			db.rowErr = miscjobsFailOn(errors.New("no rows"), name)
			h := &ApplicationLifecycle{Store: q, Logger: logger}
			j := miscjobsAppLifecycleJob("start")
			if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
				t.Fatalf("%s failure must fail the job", name)
			}
		}
	})
}

// --- applicationdelete.go ----------------------------------------------------

func miscjobsDeleteJob(deleteVolumes bool) store.Job {
	return store.Job{ID: 16, JobType: TypeApplicationDelete,
		Payload: []byte(fmt.Sprintf(`{"resource_id":1,"delete_volumes":%v}`, deleteVolumes))}
}

// miscjobsDeleteRT answers every read the deletion sweep makes with emptiness.
func miscjobsDeleteRT() *fake.Runtime {
	rt := verifyRuntime("")
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return nil, nil
	}
	rt.NetworkListFn = func(context.Context, networktypes.ListOptions) ([]networktypes.Summary, error) {
		return nil, nil
	}
	rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
		return volumetypes.ListResponse{}, nil
	}
	return rt
}

func TestMiscjobsApplicationDeleteFullPath(t *testing.T) {
	q, _, logger, _ := miscjobsDeps(t)
	// The fixture preview is live: its routing dies with the application.
	miscjobsEnum(t, "PreviewStatus", "running")
	ops := &hostfake.Ops{}
	h := &ApplicationDelete{Store: q, Docker: fixedSource{rt: miscjobsDeleteRT()}, HostOps: fixedHost{ops: ops}, Logger: logger}
	j := miscjobsDeleteJob(true)
	result, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if result.(map[string]any)["deleted"] != jobFixtureUUID || result.(map[string]any)["volumes_deleted"] != true {
		t.Fatalf("result = %#v", result)
	}
	// The application, service and preview directories were all removed.
	removes := ops.CallsTo(agentwire.MethodFileRemove)
	var dirs []string
	for _, c := range removes {
		dirs = append(dirs, c.(agentwire.FileRemoveParams).Path)
	}
	for _, want := range []string{
		"/var/lib/akerdock/applications/" + jobFixtureUUID,
		"/var/lib/akerdock/services/" + jobFixtureUUID,
		"/var/lib/akerdock/previews/" + jobFixtureUUID,
	} {
		found := false
		for _, d := range dirs {
			if d == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("directory %s was not removed (removed: %v)", want, dirs)
		}
	}
}

func TestMiscjobsApplicationDeleteVerdicts(t *testing.T) {
	ctx := context.Background()
	j := miscjobsDeleteJob(false)

	t.Run("already deleted", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetApplicationByID", "GetResourceByID")
		h := &ApplicationDelete{Store: q, Logger: logger}
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["status"] != "already deleted" {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("non-service resource without application row", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetApplicationByID")
		h := &ApplicationDelete{Store: q, Logger: logger}
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["status"] != "already deleted" {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("compose stack shares the flow", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetApplicationByID")
		miscjobsEnum(t, "ResourceType", string(store.ResourceTypeService))
		h := &ApplicationDelete{Store: q, Docker: fixedSource{rt: miscjobsDeleteRT()}, HostOps: fixedHost{}, Logger: logger}
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["deleted"] != jobFixtureUUID {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("routing removal failure leaves the workload", func(t *testing.T) {
		miscjobsShortVerify(t)
		q, _, logger, _ := miscjobsDeps(t)
		ops := &hostfake.Ops{RemoveFn: func(context.Context, agentwire.FileRemoveParams) error {
			return errors.New("agent hiccup")
		}}
		rt := miscjobsDeleteRT()
		h := &ApplicationDelete{Store: q, Docker: fixedSource{rt: rt}, HostOps: fixedHost{ops: ops}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("a failed routing removal must fail the job")
		}
		for _, name := range rt.CallNames() {
			if name == "ContainerList" {
				t.Fatal("the workload must be left untouched when routing removal fails")
			}
		}
	})
	t.Run("preview routing removal failure", func(t *testing.T) {
		miscjobsShortVerify(t)
		q, _, logger, _ := miscjobsDeps(t)
		miscjobsEnum(t, "PreviewStatus", "running")
		calls := 0
		ops := &hostfake.Ops{RemoveFn: func(context.Context, agentwire.FileRemoveParams) error {
			calls++
			if calls > 1 {
				return errors.New("agent hiccup")
			}
			return nil
		}}
		h := &ApplicationDelete{Store: q, Docker: fixedSource{rt: miscjobsDeleteRT()}, HostOps: fixedHost{ops: ops}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("a failed preview routing removal must fail the job")
		}
	})
	t.Run("teardown failure records remnants", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		rt := miscjobsDeleteRT()
		rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
			return nil, errors.New("daemon down")
		}
		h := &ApplicationDelete{Store: q, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "container sweep") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("tombstone failure", func(t *testing.T) {
		q, _, logger, db := miscjobsDeps(t)
		db.execErr = miscjobsFailOn(errors.New("db down"), "SoftDeleteResource")
		h := &ApplicationDelete{Store: q, Docker: fixedSource{rt: miscjobsDeleteRT()}, HostOps: fixedHost{}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "db down") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("domain release failures", func(t *testing.T) {
		for _, name := range []string{"DeleteDomainsForApplication", "DeleteComponentDomainsForResource"} {
			q, _, logger, db := miscjobsDeps(t)
			db.execErr = miscjobsFailOn(errors.New("db down"), name)
			h := &ApplicationDelete{Store: q, Docker: fixedSource{rt: miscjobsDeleteRT()}, HostOps: fixedHost{}, Logger: logger}
			if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
				t.Fatalf("%s failure must fail the job", name)
			}
		}
	})
	t.Run("dependency row failures", func(t *testing.T) {
		for _, name := range []string{"GetDestinationByID", "GetServerByID"} {
			q, _, logger, db := miscjobsDeps(t)
			db.rowErr = miscjobsFailOn(errors.New("no rows"), name)
			h := &ApplicationDelete{Store: q, Logger: logger}
			if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
				t.Fatalf("%s failure must fail the job", name)
			}
		}
	})
	t.Run("host ops not connected", func(t *testing.T) {
		q, _, logger, _ := miscjobsDeps(t)
		h := &ApplicationDelete{Store: q, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: unavailableHost{}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("unavailable host ops must fail")
		}
	})
}

func TestMiscjobsTeardownWorkloadFailurePoints(t *testing.T) {
	ctx := context.Background()
	h := &ApplicationDelete{}

	t.Run("named removal", func(t *testing.T) {
		rt := miscjobsDeleteRT()
		rt.ContainerRemoveFn = func(_ context.Context, name string, _ containertypes.RemoveOptions) error {
			if name == "abc-next" {
				return errors.New("stuck")
			}
			return nil
		}
		if err := h.teardownWorkload(ctx, rt, &hostfake.Ops{}, "abc", nil, false); err == nil ||
			!strings.Contains(err.Error(), "container removal") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("network sweep", func(t *testing.T) {
		rt := miscjobsDeleteRT()
		rt.NetworkListFn = func(context.Context, networktypes.ListOptions) ([]networktypes.Summary, error) {
			return nil, errors.New("daemon down")
		}
		if err := h.teardownWorkload(ctx, rt, &hostfake.Ops{}, "abc", nil, false); err == nil ||
			!strings.Contains(err.Error(), "network sweep") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("directory removal", func(t *testing.T) {
		ops := &hostfake.Ops{RemoveFn: func(context.Context, agentwire.FileRemoveParams) error {
			return errors.New("read-only")
		}}
		if err := h.teardownWorkload(ctx, miscjobsDeleteRT(), ops, "abc", nil, false); err == nil ||
			!strings.Contains(err.Error(), "directory removal") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("preview volume sweep", func(t *testing.T) {
		rt := miscjobsDeleteRT()
		rt.VolumeListFn = func(_ context.Context, opts volumetypes.ListOptions) (volumetypes.ListResponse, error) {
			for _, label := range opts.Filters.Get("label") {
				if label == "akerdock.preview_uuid" {
					return volumetypes.ListResponse{}, errors.New("daemon down")
				}
			}
			return volumetypes.ListResponse{}, nil
		}
		if err := h.teardownWorkload(ctx, rt, &hostfake.Ops{}, "abc", nil, true); err == nil ||
			!strings.Contains(err.Error(), "preview volume sweep") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("volume sweep", func(t *testing.T) {
		rt := miscjobsDeleteRT()
		rt.VolumeListFn = func(_ context.Context, opts volumetypes.ListOptions) (volumetypes.ListResponse, error) {
			if len(opts.Filters.Get("label")) == 1 {
				return volumetypes.ListResponse{}, errors.New("daemon down")
			}
			return volumetypes.ListResponse{}, nil
		}
		if err := h.teardownWorkload(ctx, rt, &hostfake.Ops{}, "abc", nil, true); err == nil ||
			!strings.Contains(err.Error(), "volume sweep") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMiscjobsRemnantInventory(t *testing.T) {
	ctx := context.Background()

	// A populated inventory: containers, volumes and the resource directory.
	rt := miscjobsDeleteRT()
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return []containertypes.Summary{{Names: []string{"/abc"}}, {Names: nil}}, nil
	}
	rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
		return volumetypes.ListResponse{Volumes: []*volumetypes.Volume{nil, {Name: "abc_data"}}}, nil
	}
	ops := &hostfake.Ops{StatFn: func(context.Context, string) (agentwire.FileStatResult, error) {
		return agentwire.FileStatResult{Found: true}, nil
	}}
	inventory := collectRemnants(ctx, rt, ops, "abc")
	if got := inventory["containers"].([]string); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("containers = %v", got)
	}
	if got := inventory["volumes"].([]string); len(got) != 1 || got[0] != "abc_data" {
		t.Fatalf("volumes = %v", got)
	}
	if got := inventory["files"].([]string); len(got) != 1 {
		t.Fatalf("files = %v", got)
	}

	// A volume listing failure downgrades to the unknown-remnants marker.
	rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
		return volumetypes.ListResponse{}, errors.New("daemon down")
	}
	inventory = collectRemnants(ctx, rt, ops, "abc")
	if _, ok := inventory["error"]; !ok {
		t.Fatalf("inventory = %v", inventory)
	}

	// recordRemnants is best-effort: a store failure only logs.
	q, _, logger, db := miscjobsDeps(t)
	db.execErr = miscjobsFailOn(errors.New("db down"), "SetResourceRemnants")
	h := &ApplicationDelete{Store: q, Logger: logger}
	h.recordRemnants(ctx, miscjobsDeleteRT(), &hostfake.Ops{}, 1, "abc")
	db.execErr = nil
	h.recordRemnants(ctx, miscjobsDeleteRT(), &hostfake.Ops{}, 1, "abc")
}
