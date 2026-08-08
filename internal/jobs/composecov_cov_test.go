// Coverage tests for composedeploy.go / composecreate.go (compose pipeline).
// All identifiers are prefixed composecov: several agents extend this package
// concurrently and never touch each other's files.
package jobs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	networktypes "github.com/docker/docker/api/types/network"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/jackc/pgx/v5"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/ssh"

	"github.com/deepteams/akerdock/internal/compose"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/envelope"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/sshkey"
	"github.com/deepteams/akerdock/internal/store"
)

const (
	composecovAppUUID    = "44444444-4444-4444-8444-444444444444"
	composecovDeployUUID = "55555555-5555-4555-8555-555555555555"
)

// ---------------------------------------------------------------------------
// database double: jobFlowDB with per-query overrides
// ---------------------------------------------------------------------------

// composecovDB wraps the shared jobFlowDB fake and lets one test override the
// answer of specific queries (matched by their sqlc name comment substring).
type composecovDB struct {
	base *jobFlowDB
	// rows overrides Query: substring -> rows factory.
	rows map[string]func() pgx.Rows
	// queryErrs fails Query per substring.
	queryErrs map[string]error
	// rowFns overrides QueryRow: substring -> row factory.
	rowFns map[string]func() pgx.Row
	// execTags overrides Exec's command tag per substring.
	execTags map[string]string
	// execErrs fails Exec per substring.
	execErrs map[string]error
}

func (db *composecovDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	for sub, err := range db.execErrs {
		if strings.Contains(sql, sub) {
			return pgconn.CommandTag{}, err
		}
	}
	for sub, tag := range db.execTags {
		if strings.Contains(sql, sub) {
			return pgconn.NewCommandTag(tag), nil
		}
	}
	return db.base.Exec(ctx, sql, args...)
}

func (db *composecovDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	for sub, err := range db.queryErrs {
		if strings.Contains(sql, sub) {
			return nil, err
		}
	}
	for sub, fn := range db.rows {
		if strings.Contains(sql, sub) {
			return fn(), nil
		}
	}
	return db.base.Query(ctx, sql, args...)
}

func (db *composecovDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	for sub, fn := range db.rowFns {
		if strings.Contains(sql, sub) {
			return fn()
		}
	}
	return db.base.QueryRow(ctx, sql, args...)
}

// composecovRows plays n generically-filled rows, with an optional per-row
// override applied after the generic fill.
type composecovRows struct {
	remaining int
	current   bool
	blob      []byte
	truthy    bool
	override  func(dest []any)
}

func (r *composecovRows) Close()                                     { r.remaining = 0 }
func (*composecovRows) Err() error                                   { return nil }
func (*composecovRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (*composecovRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (*composecovRows) Values() ([]any, error)                       { return nil, nil }
func (*composecovRows) RawValues() [][]byte                          { return nil }
func (*composecovRows) Conn() *pgx.Conn                              { return nil }
func (r *composecovRows) Next() bool {
	if r.remaining == 0 {
		r.current = false
		return false
	}
	r.remaining--
	r.current = true
	return true
}

func (r *composecovRows) Scan(dest ...any) error {
	if !r.current {
		return errors.New("Scan called before Next")
	}
	for _, d := range dest {
		if err := fillJobDestination(d, r.blob, r.truthy); err != nil {
			return err
		}
	}
	if r.override != nil {
		r.override(dest)
	}
	return nil
}

// composecovRow is a scriptable pgx.Row.
type composecovRow struct {
	err  error
	fill func(dest []any) error
}

func (r composecovRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.fill != nil {
		return r.fill(dest)
	}
	return nil
}

// composecovPortRows returns n Domain-style rows whose *int32 pointer columns
// (target_port) are set to port instead of NULL.
func composecovPortRows(n int, port int32) func() pgx.Rows {
	return func() pgx.Rows {
		return &composecovRows{remaining: n, override: func(dest []any) {
			for _, d := range dest {
				if p, ok := d.(**int32); ok {
					v := port
					*p = &v
				}
			}
		}}
	}
}

// composecovComponentRows returns n ServiceComponent rows with valid (empty
// JSON) public routes and, when port > 0, a default route port.
func composecovComponentRows(n int, port int32) func() pgx.Rows {
	return func() pgx.Rows {
		return &composecovRows{remaining: n, blob: []byte("[]"), override: func(dest []any) {
			if port <= 0 {
				return
			}
			for _, d := range dest {
				if p, ok := d.(**int32); ok {
					v := port
					*p = &v
				}
			}
		}}
	}
}

func composecovDeps(t *testing.T) (*store.Queries, *envelope.Keyring, *slog.Logger, *composecovDB) {
	t.Helper()
	_, keyring, _, logger, base := jobFlowDependencies(t)
	db := &composecovDB{
		base:      base,
		rows:      map[string]func() pgx.Rows{},
		queryErrs: map[string]error{},
		rowFns:    map[string]func() pgx.Row{},
		execTags:  map[string]string{},
		execErrs:  map[string]error{},
	}
	return store.New(db), keyring, logger, db
}

// ---------------------------------------------------------------------------
// SSH double with scriptable outputs and exit codes
// ---------------------------------------------------------------------------

type composecovSSH struct {
	listener net.Listener
	config   *ssh.ServerConfig
	mu       sync.Mutex
	conns    []net.Conn
	handler  func(cmd string) (string, uint32)
}

func composecovNewSSH(t *testing.T, handler func(cmd string) (string, uint32)) *composecovSSH {
	t.Helper()
	material, err := sshkey.GenerateEd25519("composecov")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(material.PrivatePEM))
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &composecovSSH{listener: listener, config: config, handler: handler}
	go server.accept()
	t.Cleanup(server.close)
	return server
}

func (s *composecovSSH) accept() {
	for {
		raw, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, raw)
		s.mu.Unlock()
		go s.serve(raw)
	}
}

func (s *composecovSSH) serve(raw net.Conn) {
	connection, channels, requests, err := ssh.NewServerConn(raw, s.config)
	if err != nil {
		_ = raw.Close()
		return
	}
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		go s.handleChannel(incoming)
	}
	_ = connection.Close()
}

func (s *composecovSSH) handleChannel(incoming ssh.NewChannel) {
	if incoming.ChannelType() != "session" {
		_ = incoming.Reject(ssh.UnknownChannelType, "session only")
		return
	}
	channel, requests, err := incoming.Accept()
	if err != nil {
		return
	}
	for request := range requests {
		if request.Type != "exec" {
			_ = request.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		_ = ssh.Unmarshal(request.Payload, &payload)
		_ = request.Reply(true, nil)
		_, _ = io.Copy(io.Discard, channel)
		stdout, exit := "", uint32(0)
		if s.handler != nil {
			stdout, exit = s.handler(payload.Command)
		}
		if stdout != "" {
			_, _ = channel.Write([]byte(stdout))
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exit}))
		_ = channel.Close()
		return
	}
}

func (s *composecovSSH) address(t *testing.T) (string, int) {
	t.Helper()
	host, rawPort, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func (s *composecovSSH) close() {
	_ = s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, connection := range s.conns {
		_ = connection.Close()
	}
}

// composecovDial connects an sshexec client to the scripted server.
func composecovDial(t *testing.T, srv *composecovSSH) *sshexec.Client {
	t.Helper()
	material, err := sshkey.GenerateEd25519("composecov-client")
	if err != nil {
		t.Fatal(err)
	}
	host, port := srv.address(t)
	client, err := sshexec.Dial(context.Background(), host, port, "unit", material.PrivatePEM, 5*time.Second, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// ---------------------------------------------------------------------------
// runtime doubles
// ---------------------------------------------------------------------------

// composecovNotFound mimics the daemon's typed not-found answer.
func composecovNotFound(what string) error {
	return fmt.Errorf("no such %s: %w", what, cerrdefs.ErrNotFound)
}

// composecovRuntime is the pipeline-friendly runtime double: everything
// answers "present and healthy"; individual tests override what matters.
func composecovRuntime() *fake.Runtime {
	rt := &fake.Runtime{}
	rt.NetworkInspectFn = func(context.Context, string, networktypes.InspectOptions) (networktypes.Inspect, error) {
		return networktypes.Inspect{}, nil
	}
	rt.ImageInspectFn = func(context.Context, string, ...client.ImageInspectOption) (imagetypes.InspectResponse, error) {
		return imagetypes.InspectResponse{
			RepoDigests: []string{"registry.example/app@sha256:feed"},
			Config:      &dockerspec.DockerOCIImageConfig{},
		}, nil
	}
	rt.ImagePullFn = func(context.Context, string, imagetypes.PullOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"status":"Pulling"}` + "\n")), nil
	}
	rt.ImageListFn = func(context.Context, imagetypes.ListOptions) ([]imagetypes.Summary, error) {
		return nil, nil
	}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: true, Status: "running"},
		}}, nil
	}
	rt.ContainerLogsFn = func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	rt.ContainerWaitFn = func(context.Context, string, containertypes.WaitCondition) (<-chan containertypes.WaitResponse, <-chan error) {
		waitCh := make(chan containertypes.WaitResponse, 1)
		waitCh <- containertypes.WaitResponse{StatusCode: 0}
		return waitCh, make(chan error, 1)
	}
	rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
		return volumetypes.Volume{}, nil
	}
	rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
		return volumetypes.ListResponse{}, nil
	}
	composecovScriptExec(rt, func([]string) string { return "" })
	return rt
}

// composecovScriptExec scripts every container exec: output(cmd) is framed as
// the exec's stdout, exit 0.
func composecovScriptExec(rt *fake.Runtime, output func(cmd []string) string) {
	var mu sync.Mutex
	pending := map[string]string{}
	seq := 0
	rt.ContainerExecCreateFn = func(_ context.Context, _ string, opts containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
		mu.Lock()
		seq++
		id := fmt.Sprintf("exec-%d", seq)
		pending[id] = output(opts.Cmd)
		mu.Unlock()
		return containertypes.ExecCreateResponse{ID: id}, nil
	}
	rt.ContainerExecAttachFn = func(_ context.Context, execID string, _ containertypes.ExecAttachOptions) (types.HijackedResponse, error) {
		mu.Lock()
		out := pending[execID]
		mu.Unlock()
		var buf bytes.Buffer
		_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stdout).Write([]byte(out))
		clientConn, serverConn := net.Pipe()
		go func() {
			_, _ = serverConn.Write(buf.Bytes())
			_ = serverConn.Close()
		}()
		return types.HijackedResponse{Conn: clientConn, Reader: bufio.NewReader(clientConn)}, nil
	}
	rt.ContainerExecInspectFn = func(context.Context, string) (containertypes.ExecInspect, error) {
		return containertypes.ExecInspect{ExitCode: 0}, nil
	}
}

// composecovRunner assembles a deploymentRun the compose methods can drive
// without going through Execute.
func composecovRunner(t *testing.T, q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger, rt *fake.Runtime) *deploymentRun {
	t.Helper()
	return &deploymentRun{
		h: &DeploymentRun{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: rt}, HostOps: fixedHost{ops: &hostfake.Ops{}},
		},
		jobID: 1,
		d: store.Deployment{
			ID: 1, Uuid: mustUUID(t, composecovDeployUUID),
			Status: store.DeploymentStatusQueued, Trigger: store.DeploymentTriggerManual,
			ResourceID: 1, ServerID: 1,
		},
		app: store.GetApplicationByIDRow{
			Resource:    store.Resource{ID: 1, Uuid: mustUUID(t, composecovAppUUID), TeamID: 1, DestinationID: 1, Name: "composecov"},
			BuildConfig: store.BuildConfig{BuildPack: store.BuildPackCompose},
		},
		server:    store.Server{ID: 1},
		dest:      store.Destination{ServerID: 1, Network: "composecov-net"},
		teamUUID:  "66666666-6666-4666-8666-666666666666",
		rt:        rt,
		hops:      &hostfake.Ops{},
		labelsMap: map[string]string{"akerdock.managed": "true"},
	}
}

// composecovShrinkTimers makes the health/stability waits test-sized.
func composecovShrinkTimers(t *testing.T) {
	t.Helper()
	oldStable, oldPoll, oldVerify := composeStablePeriod, deploymentHealthPoll, verifyTimeout
	composeStablePeriod = time.Millisecond
	deploymentHealthPoll = time.Millisecond
	verifyTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		composeStablePeriod, deploymentHealthPoll, verifyTimeout = oldStable, oldPoll, oldVerify
	})
}

// ---------------------------------------------------------------------------
// composecreate.go — typed create spec
// ---------------------------------------------------------------------------

func TestComposecovBuildCreateSpecRichOptions(t *testing.T) {
	plan := loadPlan(t, `
services:
  web:
    image: nginx:1.27
    restart: "no"
    labels:
      a.label: first
    volumes:
      - data:/data:ro
      - ./config:/config:ro
      - type: tmpfs
        target: /tmp
    ports:
      - "127.0.0.1:8080:80/udp"
      - "9090:90"
    deploy:
      resources:
        limits:
          memory: 128M
          cpus: "0.5"
          pids: 100
        reservations:
          memory: 64M
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://localhost/health"]
      interval: 10s
      timeout: 2s
      retries: 4
      start_period: 5s
    user: "1000:1000"
    working_dir: /app
    init: true
    read_only: true
    extra_hosts:
      - "host.internal:127.0.0.1"
    stop_grace_period: 15s
    stop_signal: SIGINT
    entrypoint: ["sh", "-c"]
    command: ["echo", "hello"]
volumes:
  data: {}
`)
	sp := plan.Services[0]
	sp.OneShot = true
	spec := buildComposeCreateSpec(plan, sp, "/apps/unit", map[string]string{"akerdock.managed": "true"},
		[]string{"KEY=value"}, "nginx:1.27", composeCreateOpts{Name: "web", Aliases: []string{"web", "long-web"}})

	cfg, host := spec.Config, spec.Host
	if cfg.User != "1000:1000" || cfg.WorkingDir != "/app" || cfg.StopSignal != "SIGINT" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Labels["akerdock.oneshot"] != "true" || cfg.Labels["a.label"] != "first" || cfg.Labels["akerdock.component"] != "web" {
		t.Fatalf("labels = %v", cfg.Labels)
	}
	if len(cfg.Entrypoint) != 2 || len(cfg.Cmd) != 2 {
		t.Fatalf("entrypoint/cmd = %v %v", cfg.Entrypoint, cfg.Cmd)
	}
	if cfg.StopTimeout == nil || *cfg.StopTimeout != 15 {
		t.Fatalf("stop timeout = %v", cfg.StopTimeout)
	}
	if cfg.Healthcheck == nil || cfg.Healthcheck.Test[0] != "CMD-SHELL" || cfg.Healthcheck.Retries != 4 {
		t.Fatalf("healthcheck = %+v", cfg.Healthcheck)
	}
	wantBinds := map[string]bool{}
	for _, b := range host.Binds {
		wantBinds[b] = true
	}
	prefix := plan.StackUUID
	if !wantBinds[prefix+"_data:/data:ro"] || !wantBinds["/apps/unit/mounts/config:/config:ro"] {
		t.Fatalf("binds = %v", host.Binds)
	}
	if _, ok := host.Tmpfs["/tmp"]; !ok {
		t.Fatalf("tmpfs = %v", host.Tmpfs)
	}
	if len(host.PortBindings) != 2 {
		t.Fatalf("port bindings = %v", host.PortBindings)
	}
	if host.Memory != 128<<20 || host.MemoryReservation != 64<<20 || host.NanoCPUs != 5e8 {
		t.Fatalf("limits = mem %d res %d cpus %d", host.Memory, host.MemoryReservation, host.NanoCPUs)
	}
	if host.PidsLimit == nil || *host.PidsLimit != 100 {
		t.Fatalf("pids = %v", host.PidsLimit)
	}
	if host.Init == nil || !*host.Init || !host.ReadonlyRootfs {
		t.Fatalf("init/readonly = %v %v", host.Init, host.ReadonlyRootfs)
	}
	if len(host.ExtraHosts) != 1 || host.ExtraHosts[0] != "host.internal:127.0.0.1" {
		t.Fatalf("extra hosts = %v", host.ExtraHosts)
	}
	if host.RestartPolicy.Name != "no" {
		t.Fatalf("restart = %v", host.RestartPolicy)
	}
	aliases := spec.Networking.EndpointsConfig[plan.NetworkName].Aliases
	if len(aliases) != 2 {
		t.Fatalf("aliases = %v", aliases)
	}

	// Disabled healthcheck renders as the NONE test.
	sp.Health = &compose.HealthFlags{Disable: true}
	disabled := buildComposeCreateSpec(plan, sp, "/apps/unit", nil, nil, "nginx",
		composeCreateOpts{Name: "web"})
	if disabled.Config.Healthcheck == nil || disabled.Config.Healthcheck.Test[0] != "NONE" {
		t.Fatalf("disabled healthcheck = %+v", disabled.Config.Healthcheck)
	}

	// Empty restart falls back to compose's default "no".
	sp.Restart = ""
	sp.Health = nil
	fallback := buildComposeCreateSpec(plan, sp, "/apps/unit", nil, nil, "nginx",
		composeCreateOpts{Name: "web"})
	if fallback.Config.Healthcheck != nil || fallback.Host.RestartPolicy.Name != "no" {
		t.Fatalf("fallback spec = %+v", fallback.Config.Healthcheck)
	}
}

func TestComposecovServiceEnvRendersSortedEntries(t *testing.T) {
	plan := loadPlan(t, `
services:
  app:
    image: nginx
    environment:
      ZED: last
      ALPHA: first
      EMPTY:
`)
	entries, keys, v1 := composeServiceEnv(plan.Services[0])
	if len(entries) != 2 || entries[0] != "ALPHA=first" || entries[1] != "ZED=last" {
		t.Fatalf("entries = %v", entries)
	}
	if len(keys) != 2 || keys[0] != "ALPHA" || keys[1] != "ZED" {
		t.Fatalf("keys = %v", keys)
	}
	if !strings.Contains(v1, "export ALPHA='first'\n") || !strings.Contains(v1, "export ZED='last'\n") {
		t.Fatalf("v1 content = %q", v1)
	}
}

func TestComposecovProtocolOrAndMapAppend(t *testing.T) {
	if protocolOr("") != "tcp" || protocolOr("udp") != "udp" {
		t.Fatal("protocolOr defaults wrong")
	}
	m := mapAppend(nil, "k", "v")
	if m["k"] != "v" {
		t.Fatalf("mapAppend nil map = %v", m)
	}
	m = mapAppend(m, "k2", "v2")
	if len(m) != 2 {
		t.Fatalf("mapAppend existing map = %v", m)
	}
}

func TestComposecovEnsureStackNetwork(t *testing.T) {
	// Already present: no create.
	rt := &fake.Runtime{}
	rt.NetworkInspectFn = func(context.Context, string, networktypes.InspectOptions) (networktypes.Inspect, error) {
		return networktypes.Inspect{}, nil
	}
	if err := ensureStackNetwork(context.Background(), rt, "net", nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range rt.CallNames() {
		if c == "NetworkCreate" {
			t.Fatal("existing network must not be recreated")
		}
	}

	// Inspect fails with a real error: propagated.
	rt = &fake.Runtime{}
	rt.NetworkInspectFn = func(context.Context, string, networktypes.InspectOptions) (networktypes.Inspect, error) {
		return networktypes.Inspect{}, errors.New("daemon down")
	}
	if err := ensureStackNetwork(context.Background(), rt, "net", nil); err == nil {
		t.Fatal("inspect error must propagate")
	}

	// Absent: created with the labels.
	rt = &fake.Runtime{}
	rt.NetworkInspectFn = func(context.Context, string, networktypes.InspectOptions) (networktypes.Inspect, error) {
		return networktypes.Inspect{}, composecovNotFound("network")
	}
	if err := ensureStackNetwork(context.Background(), rt, "net", map[string]string{"l": "v"}); err != nil {
		t.Fatal(err)
	}

	// Created concurrently: the conflict is tolerated.
	rt = &fake.Runtime{}
	rt.NetworkInspectFn = func(context.Context, string, networktypes.InspectOptions) (networktypes.Inspect, error) {
		return networktypes.Inspect{}, composecovNotFound("network")
	}
	rt.NetworkCreateFn = func(context.Context, string, networktypes.CreateOptions) (networktypes.CreateResponse, error) {
		return networktypes.CreateResponse{}, fmt.Errorf("already exists: %w", cerrdefs.ErrConflict)
	}
	if err := ensureStackNetwork(context.Background(), rt, "net", nil); err != nil {
		t.Fatalf("conflict must be tolerated: %v", err)
	}

	// Any other create failure propagates.
	rt = &fake.Runtime{}
	rt.NetworkInspectFn = func(context.Context, string, networktypes.InspectOptions) (networktypes.Inspect, error) {
		return networktypes.Inspect{}, composecovNotFound("network")
	}
	rt.NetworkCreateFn = func(context.Context, string, networktypes.CreateOptions) (networktypes.CreateResponse, error) {
		return networktypes.CreateResponse{}, errors.New("boom")
	}
	if err := ensureStackNetwork(context.Background(), rt, "net", nil); err == nil {
		t.Fatal("create error must propagate")
	}
}

func TestComposecovRetireAdoptedProject(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)

	// Listing failure propagates.
	rt := &fake.Runtime{}
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return nil, errors.New("daemon down")
	}
	r := composecovRunner(t, q, keyring, logger, rt)
	if err := r.retireAdoptedProject(context.Background(), "legacy"); err == nil {
		t.Fatal("list error must propagate")
	}

	// Stop/remove failures are warned, never fatal; not-found is silent.
	rt = &fake.Runtime{}
	rt.ContainerListFn = func(_ context.Context, opts containertypes.ListOptions) ([]containertypes.Summary, error) {
		if got := opts.Filters.Get("label"); len(got) != 1 || got[0] != "com.docker.compose.project=legacy" {
			t.Fatalf("filters = %v", got)
		}
		return []containertypes.Summary{
			{ID: "c1", Names: []string{"/legacy-web"}},
			{ID: "c2", Names: []string{"/legacy-db"}},
		}, nil
	}
	rt.ContainerStopFn = func(_ context.Context, id string, _ containertypes.StopOptions) error {
		if id == "c1" {
			return errors.New("still busy")
		}
		return composecovNotFound("container")
	}
	rt.ContainerRemoveFn = func(_ context.Context, id string, _ containertypes.RemoveOptions) error {
		if id == "c1" {
			return composecovNotFound("container")
		}
		return errors.New("rm failed")
	}
	r = composecovRunner(t, q, keyring, logger, rt)
	if err := r.retireAdoptedProject(context.Background(), "legacy"); err != nil {
		t.Fatalf("retire is best-effort: %v", err)
	}
}

func TestComposecovConnectServiceNetworks(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)
	sp := compose.ServicePlan{Name: "web", ExtraNetworks: []string{"extra-1"}}

	// Strict mode with short aliases: extra + destination networks, aliases chosen.
	rt := &fake.Runtime{}
	var connected []string
	rt.NetworkConnectFn = func(_ context.Context, network, name string, cfg *networktypes.EndpointSettings) error {
		connected = append(connected, network+"|"+name+"|"+strings.Join(cfg.Aliases, ","))
		return nil
	}
	r := composecovRunner(t, q, keyring, logger, rt)
	if err := r.connectServiceNetworks(context.Background(), sp, "cont", true, true, false); err != nil {
		t.Fatal(err)
	}
	if len(connected) != 2 || connected[0] != "extra-1|cont|web" || connected[1] != "composecov-net|cont|cont" {
		t.Fatalf("connections = %v", connected)
	}

	// Long alias variant (candidate wiring).
	connected = nil
	if err := r.connectServiceNetworks(context.Background(), sp, "cont-next", false, false, false); err != nil {
		t.Fatal(err)
	}
	if len(connected) != 1 || connected[0] != "extra-1|cont-next|cont-next" {
		t.Fatalf("candidate connections = %v", connected)
	}

	// Already-connected answers converge in strict mode.
	rt.NetworkConnectFn = func(context.Context, string, string, *networktypes.EndpointSettings) error {
		return errors.New("endpoint already exists in network extra-1")
	}
	if err := r.connectServiceNetworks(context.Background(), sp, "cont", true, true, false); err != nil {
		t.Fatalf("already-connected must converge: %v", err)
	}

	// Other errors propagate in strict mode…
	rt.NetworkConnectFn = func(context.Context, string, string, *networktypes.EndpointSettings) error {
		return errors.New("network gone")
	}
	if err := r.connectServiceNetworks(context.Background(), sp, "cont", true, true, false); err == nil {
		t.Fatal("strict connect error must propagate")
	}
	// …and are tolerated in tolerant mode (the unchanged-service path).
	if err := r.connectServiceNetworks(context.Background(), sp, "cont", true, true, true); err != nil {
		t.Fatalf("tolerant connect must swallow: %v", err)
	}
	// The destination connect failure propagates too.
	rt.NetworkConnectFn = func(_ context.Context, network string, _ string, _ *networktypes.EndpointSettings) error {
		if network == "composecov-net" {
			return errors.New("destination gone")
		}
		return nil
	}
	if err := r.connectServiceNetworks(context.Background(), sp, "cont", true, true, false); err == nil {
		t.Fatal("destination connect error must propagate")
	}
}

func TestComposecovSeedPreviewVolumes(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)
	pairs := [][2]string{{"prod_data", "preview_data"}}

	// Missing production volume: the pair is skipped, nothing runs.
	rt := composecovRuntime()
	rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
		return volumetypes.Volume{}, composecovNotFound("volume")
	}
	r := composecovRunner(t, q, keyring, logger, rt)
	if err := r.seedPreviewVolumes(context.Background(), "img", pairs); err != nil {
		t.Fatal(err)
	}
	for _, c := range rt.CallNames() {
		if c == "ContainerCreate" {
			t.Fatal("missing production volume must not seed")
		}
	}

	// Any other inspect failure propagates.
	rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
		return volumetypes.Volume{}, errors.New("daemon down")
	}
	if err := r.seedPreviewVolumes(context.Background(), "img", pairs); err == nil {
		t.Fatal("volume inspect error must propagate")
	}

	// Present: the copy container runs with production read-only.
	rt = composecovRuntime()
	var seedHost *containertypes.HostConfig
	rt.ContainerCreateFn = func(_ context.Context, _ *containertypes.Config, host *containertypes.HostConfig, _ *networktypes.NetworkingConfig, _ *ocispec.Platform, _ string) (containertypes.CreateResponse, error) {
		seedHost = host
		return containertypes.CreateResponse{ID: "seed"}, nil
	}
	r = composecovRunner(t, q, keyring, logger, rt)
	if err := r.seedPreviewVolumes(context.Background(), "img", pairs); err != nil {
		t.Fatal(err)
	}
	if seedHost == nil || len(seedHost.Binds) != 2 || seedHost.Binds[0] != "prod_data:/akerdock-seed-from:ro" {
		t.Fatalf("seed binds = %+v", seedHost)
	}

	// A failing copy fails the deployment with the pair named.
	rt.ContainerWaitFn = func(context.Context, string, containertypes.WaitCondition) (<-chan containertypes.WaitResponse, <-chan error) {
		waitCh := make(chan containertypes.WaitResponse, 1)
		waitCh <- containertypes.WaitResponse{StatusCode: 2}
		return waitCh, make(chan error, 1)
	}
	err := r.seedPreviewVolumes(context.Background(), "img", pairs)
	if err == nil || !strings.Contains(err.Error(), "seeding preview_data from prod_data") {
		t.Fatalf("seed failure = %v", err)
	}
}

func TestComposecovHealthVerdicts(t *testing.T) {
	composecovShrinkTimers(t)
	q, keyring, logger, _ := composecovDeps(t)
	sp := compose.ServicePlan{Name: "web", Health: &compose.HealthFlags{
		Test: []string{"CMD", "true"}, Interval: time.Second, Timeout: time.Second, Retries: 1,
	}}

	verdict := func(rt *fake.Runtime, plan compose.ServicePlan) (string, error) {
		r := composecovRunner(t, q, keyring, logger, rt)
		return r.composeHealthVerdict(context.Background(), plan, "c")
	}

	healthy := &fake.Runtime{}
	healthy.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: true, Health: &containertypes.Health{Status: "healthy"}},
		}}, nil
	}
	if v, err := verdict(healthy, sp); err != nil || v != "healthy" {
		t.Fatalf("healthy verdict = %q %v", v, err)
	}

	unhealthy := &fake.Runtime{}
	unhealthy.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: true, Health: &containertypes.Health{Status: "unhealthy"}},
		}}, nil
	}
	if v, err := verdict(unhealthy, sp); err != nil || v != "unhealthy" {
		t.Fatalf("unhealthy verdict = %q %v", v, err)
	}

	// No healthcheck: running before AND after the stabilization window.
	stable := &fake.Runtime{}
	stable.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: true, Status: "running"},
		}}, nil
	}
	if v, err := verdict(stable, compose.ServicePlan{Name: "web"}); err != nil || v != "running" {
		t.Fatalf("stable verdict = %q %v", v, err)
	}

	// No healthcheck, died inside the window: its status is the verdict.
	dying := &fake.Runtime{}
	calls := 0
	dying.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		calls++
		if calls == 1 {
			return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
				State: &containertypes.State{Running: true, Status: "running"},
			}}, nil
		}
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: false, Status: "exited"},
		}}, nil
	}
	if v, err := verdict(dying, compose.ServicePlan{Name: "web"}); err != nil || v != "exited" {
		t.Fatalf("dying verdict = %q %v", v, err)
	}

	// No healthcheck, vanished inside the window: reported absent.
	vanishing := &fake.Runtime{}
	calls = 0
	vanishing.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		calls++
		if calls == 1 {
			return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
				State: &containertypes.State{Running: true, Status: "running"},
			}}, nil
		}
		return containertypes.InspectResponse{}, composecovNotFound("container")
	}
	if v, err := verdict(vanishing, compose.ServicePlan{Name: "web"}); err != nil || v != "absent" {
		t.Fatalf("vanishing verdict = %q %v", v, err)
	}

	// Cancellation inside the poll loop surfaces the context error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	broken := &fake.Runtime{}
	broken.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{}, composecovNotFound("container")
	}
	r := composecovRunner(t, q, keyring, logger, broken)
	if _, err := r.composeHealthVerdict(ctx, sp, "c"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	// And inside the stabilization wait too.
	r = composecovRunner(t, q, keyring, logger, stable)
	if _, err := r.composeHealthVerdict(ctx, compose.ServicePlan{Name: "web"}, "c"); !errors.Is(err, context.Canceled) {
		t.Fatalf("stabilization cancel error = %v", err)
	}
}

func TestComposecovAwaitHealthyAttachesLogsOnFailure(t *testing.T) {
	composecovShrinkTimers(t)
	q, keyring, logger, _ := composecovDeps(t)
	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: true, Health: &containertypes.Health{Status: "unhealthy"}},
		}}, nil
	}
	rt.ContainerLogsFn = func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
		var buf bytes.Buffer
		_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stderr).Write([]byte("fatal: db unreachable\n"))
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	}
	r := composecovRunner(t, q, keyring, logger, rt)
	sp := compose.ServicePlan{Name: "web", Health: &compose.HealthFlags{
		Test: []string{"CMD", "true"}, Interval: time.Second, Timeout: time.Second, Retries: 1,
	}}
	err := r.composeAwaitHealthy(context.Background(), sp, "c")
	if err == nil || !strings.Contains(err.Error(), "unhealthy") || !strings.Contains(err.Error(), "db unreachable") {
		t.Fatalf("unhealthy error = %v", err)
	}

	// The healthy path closes the step cleanly.
	healthy := &fake.Runtime{}
	healthy.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: true, Health: &containertypes.Health{Status: "healthy"}},
		}}, nil
	}
	r = composecovRunner(t, q, keyring, logger, healthy)
	if err := r.composeAwaitHealthy(context.Background(), sp, "c"); err != nil {
		t.Fatal(err)
	}
}

func TestComposecovContainerConfigState(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)

	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{}, composecovNotFound("container")
	}
	r := composecovRunner(t, q, keyring, logger, rt)
	if state := r.containerConfigState(context.Background(), "c"); state != (composeConfigState{}) {
		t.Fatalf("absent state = %+v", state)
	}

	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{
			ContainerJSONBase: &containertypes.ContainerJSONBase{
				State: &containertypes.State{Status: "running"},
			},
			Config: &containertypes.Config{Labels: map[string]string{
				"akerdock.config_hash":    "v1",
				"akerdock.config_hash_v2": "2:v2",
			}},
		}, nil
	}
	state := r.containerConfigState(context.Background(), "c")
	if state.hashV1 != "v1" || state.hashV2 != "2:v2" || !state.running {
		t.Fatalf("state = %+v", state)	}
}

func TestComposecovPreviewSeedPairs(t *testing.T) {
	q, keyring, logger, _ := composecovDeps(t)
	r := composecovRunner(t, q, keyring, logger, nil)
	plan := &compose.Plan{SeedVolumes: map[string]string{"pv_data": "data"}}
	sp := compose.ServicePlan{Mounts: []compose.MountPlan{
		{Type: "volume", Source: "pv_data", Target: "/data"},
		{Type: "volume", Source: "pv_other", Target: "/other"},
		{Type: "bind", Source: "/host", Target: "/host"},
	}}

	// Production run: seeding is a preview-only contract.
	if pairs := r.previewSeedPairs(plan, sp); pairs != nil {
		t.Fatalf("production pairs = %v", pairs)
	}

	fqdn := "pr-9.example.test"
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, "77777777-7777-4777-8777-777777777777"), PrID: 9, Fqdn: &fqdn}
	pairs := r.previewSeedPairs(plan, sp)
	if len(pairs) != 1 || pairs[0][0] != composecovAppUUID+"_data" || pairs[0][1] != "pv_data" {
		t.Fatalf("preview pairs = %v", pairs)
	}
}
