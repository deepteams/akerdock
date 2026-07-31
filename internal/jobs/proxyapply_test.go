package jobs

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/store"
)

// verifyRuntime scripts the applier's verification exec: each call answers
// the next output in sequence (the last repeats), exit 0.
func verifyRuntime(outputs ...string) *fake.Runtime {
	rt := &fake.Runtime{}
	call := 0
	rt.ContainerExecCreateFn = func(context.Context, string, containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
		return containertypes.ExecCreateResponse{ID: "verify"}, nil
	}
	rt.ContainerExecAttachFn = func(context.Context, string, containertypes.ExecAttachOptions) (types.HijackedResponse, error) {
		out := outputs[min(call, len(outputs)-1)]
		call++
		var buf bytes.Buffer
		_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stdout).Write([]byte(out))
		client, server := net.Pipe()
		go func() {
			_, _ = server.Write(buf.Bytes())
			_ = server.Close()
		}()
		return types.HijackedResponse{Conn: client, Reader: bufio.NewReader(client)}, nil
	}
	rt.ContainerExecInspectFn = func(context.Context, string) (containertypes.ExecInspect, error) {
		return containertypes.ExecInspect{ExitCode: 0}, nil
	}
	return rt
}

// TestProxyApplierUploadWritesAtomically pins §6.2: the dynamic file rides
// the channel as an atomic 0600 write, the proxy is attached to the network
// best-effort first, and an empty content removes the file.
func TestProxyApplierUploadWritesAtomically(t *testing.T) {
	rt := &fake.Runtime{}
	ops := &hostfake.Ops{}
	p := &ProxyApplier{Docker: rt, Host: ops, Network: "akerdock-net"}

	if err := p.upload(context.Background(), "app", "routing: yes"); err != nil {
		t.Fatal(err)
	}
	writes := ops.CallsTo(agentwire.MethodFileWrite)
	if len(writes) != 1 {
		t.Fatalf("writes = %v", writes)
	}
	w := writes[0].(agentwire.FileWriteParams)
	if w.Path != "/var/lib/akerdock/proxy/dynamic/app.yaml" || !w.Atomic || w.Mode != 0o600 || string(w.Content) != "routing: yes" {
		t.Fatalf("write = %+v", w)
	}
	if calls := rt.CallNames(); len(calls) != 1 || calls[0] != "NetworkConnect" {
		t.Fatalf("docker calls = %v — the proxy must be attached before the file lands", calls)
	}

	if err := p.upload(context.Background(), "app", ""); err != nil {
		t.Fatal(err)
	}
	removes := ops.CallsTo(agentwire.MethodFileRemove)
	if len(removes) != 1 || removes[0].(agentwire.FileRemoveParams).Path != "/var/lib/akerdock/proxy/dynamic/app.yaml" {
		t.Fatalf("removes = %v", removes)
	}
}

// TestProxyApplierVerifyVerdicts pins §6.3/§6.5 on the typed exec poll: an
// apply succeeds once the API exposes the scope (and the expected endpoint),
// a removal once it no longer does, and the budget expiring names the scope.
func TestProxyApplierVerifyVerdicts(t *testing.T) {
	oldTimeout := verifyTimeout
	verifyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { verifyTimeout = oldTimeout })

	p := &ProxyApplier{Docker: verifyRuntime(`{"app-svc":{"serverStatus":{"http://10.0.0.9:80":"UP"}}}`)}
	if err := p.verify(context.Background(), "app", "routing", "10.0.0.9"); err != nil {
		t.Fatalf("apply verify = %v", err)
	}
	p = &ProxyApplier{Docker: verifyRuntime(`{}`)}
	if err := p.verify(context.Background(), "app", "", ""); err != nil {
		t.Fatalf("removal verify = %v", err)
	}
	p = &ProxyApplier{Docker: verifyRuntime(`{}`)}
	err := p.verify(context.Background(), "app", "routing", "")
	if err == nil || !strings.Contains(err.Error(), "did not load the routing of app") {
		t.Fatalf("timeout verdict = %v", err)
	}
}

// TestProvisionAgentGuards pins the ADR-054 tranche-B preconditions: no
// release image and no resolvable enrollment are named remediations, not
// timeouts — both refuse before touching the server.
func TestProvisionAgentGuards(t *testing.T) {
	q, keyring, _, logger, _ := jobFlowDependencies(t)
	h := &ServerValidate{Store: q, Keyring: keyring, Logger: logger}
	if _, _, err := h.provisionAgent(context.Background(), nil, store.Server{}); err == nil ||
		!strings.Contains(err.Error(), "AKERDOCK_IMAGE") {
		t.Fatalf("missing image = %v", err)
	}
	h.AgentImage = "akerdock:test"
	if _, _, err := h.provisionAgent(context.Background(), nil, store.Server{}); err == nil ||
		!strings.Contains(err.Error(), "enrollment") {
		t.Fatalf("missing enrollment = %v", err)
	}
}
