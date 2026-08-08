package jobs

// Coverage-focused tests for deploymentrun.go. Everything here reuses the
// in-package jobFlow scaffolding (jobFlowDependencies, jobFlowRow/Rows,
// fixedSource/fixedHost, verifyRuntime) and adds a thin steering layer —
// deployrunDB — that intercepts individual queries by their sqlc name while
// delegating everything else to the shared fake. All new top-level
// identifiers are prefixed deployrun.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	networktypes "github.com/docker/docker/api/types/network"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/ssh"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/serverdial"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

const (
	deployrunPreviewUUID = "22222222-2222-4222-8222-222222222222"
	deployrunDeployUUID  = "55555555-5555-4555-8555-555555555555"
	deployrunFixtureSHA  = "0123456789012345678901234567890123456789"
)

// ---------------------------------------------------------------------------
// Steering database: per-query interception over the shared jobFlowDB.
// ---------------------------------------------------------------------------

type deployrunDB struct {
	inner   *jobFlowDB
	rowFor  func(sql string) pgx.Row
	rowsFor func(sql string) (pgx.Rows, error, bool)
	execErr func(sql string) error
	note    func(sql string)
}

func (d *deployrunDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if d.note != nil {
		d.note(sql)
	}
	if d.execErr != nil {
		if err := d.execErr(sql); err != nil {
			return pgconn.CommandTag{}, err
		}
	}
	return d.inner.Exec(ctx, sql, args...)
}

func (d *deployrunDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if d.note != nil {
		d.note(sql)
	}
	if d.rowsFor != nil {
		if rows, err, ok := d.rowsFor(sql); ok {
			return rows, err
		}
	}
	return d.inner.Query(ctx, sql, args...)
}

func (d *deployrunDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if d.note != nil {
		d.note(sql)
	}
	if d.rowFor != nil {
		if row := d.rowFor(sql); row != nil {
			return row
		}
	}
	return d.inner.QueryRow(ctx, sql, args...)
}

var _ store.DBTX = (*deployrunDB)(nil)

// deployrunRow scans one row: overrides win by destination index, everything
// else falls back to the shared fixture filler.
type deployrunRow struct {
	overrides map[int]any
	blob      []byte
	truthy    bool
	err       error
}

func (r deployrunRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		if v, ok := r.overrides[i]; ok {
			if err := deployrunAssign(d, v); err != nil {
				return err
			}
			continue
		}
		if err := fillJobDestination(d, r.blob, r.truthy); err != nil {
			return err
		}
	}
	return nil
}

// deployrunAssign sets *dest = v, wrapping v in a pointer when the
// destination is a pointer to v's type (nullable columns).
func deployrunAssign(dest, v any) error {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return errors.New("deployrunAssign: invalid destination")
	}
	elem := rv.Elem()
	val := reflect.ValueOf(v)
	switch {
	case val.Type().AssignableTo(elem.Type()):
		elem.Set(val)
	case elem.Kind() == reflect.Pointer && val.Type().AssignableTo(elem.Type().Elem()):
		p := reflect.New(elem.Type().Elem())
		p.Elem().Set(val)
		elem.Set(p)
	default:
		return fmt.Errorf("deployrunAssign: cannot assign %T into %T", v, dest)
	}
	return nil
}

// deployrunRows plays a scripted list of override rows.
type deployrunRows struct {
	rows   []map[int]any
	blob   []byte
	truthy bool
	idx    int
	cur    bool
}

func (r *deployrunRows) Close()                                     { r.idx = len(r.rows); r.cur = false }
func (*deployrunRows) Err() error                                   { return nil }
func (*deployrunRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (*deployrunRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (*deployrunRows) Values() ([]any, error)                       { return nil, nil }
func (*deployrunRows) RawValues() [][]byte                          { return nil }
func (*deployrunRows) Conn() *pgx.Conn                              { return nil }

func (r *deployrunRows) Next() bool {
	if r.idx >= len(r.rows) {
		r.cur = false
		return false
	}
	r.idx++
	r.cur = true
	return true
}

func (r *deployrunRows) Scan(dest ...any) error {
	if !r.cur {
		return errors.New("Scan called before Next")
	}
	return deployrunRow{overrides: r.rows[r.idx-1], blob: r.blob, truthy: r.truthy}.Scan(dest...)
}

var _ pgx.Rows = (*deployrunRows)(nil)

// deployrunSQLLog records executed statements so tests can assert on side
// effects (artifact rows, log flushes) without real persistence.
type deployrunSQLLog struct {
	mu   sync.Mutex
	sqls []string
}

func (l *deployrunSQLLog) add(sql string) {
	l.mu.Lock()
	l.sqls = append(l.sqls, sql)
	l.mu.Unlock()
}

func (l *deployrunSQLLog) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.sqls {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Scripted SSH server with per-command output AND exit codes.
// ---------------------------------------------------------------------------

type deployrunSSHServer struct {
	listener net.Listener
	config   *ssh.ServerConfig
	mu       sync.Mutex
	conns    []net.Conn
	respond  func(command string) (string, uint32)
}

func deployrunNewSSHServer(t *testing.T, respond func(string) (string, uint32)) *deployrunSSHServer {
	t.Helper()
	if respond == nil {
		respond = func(command string) (string, uint32) { return jobCommandOutput(command), 0 }
	}
	signerSeed := newJobSSHServer(t) // reuse the shared host-key plumbing? no: build our own below
	signerSeed.close()

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	material, err := sshexecTestSigner()
	if err != nil {
		t.Fatal(err)
	}
	config.AddHostKey(material)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &deployrunSSHServer{listener: listener, config: config, respond: respond}
	go server.accept()
	t.Cleanup(server.close)
	return server
}

// sshexecTestSigner builds a throwaway host key signer.
func sshexecTestSigner() (ssh.Signer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(key)
}

func (s *deployrunSSHServer) accept() {
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

func (s *deployrunSSHServer) serve(raw net.Conn) {
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

func (s *deployrunSSHServer) handleChannel(incoming ssh.NewChannel) {
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
			request.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		_ = ssh.Unmarshal(request.Payload, &payload)
		request.Reply(true, nil)
		_, _ = io.Copy(io.Discard, channel)
		stdout, status := s.respond(payload.Command)
		if stdout != "" {
			_, _ = channel.Write([]byte(stdout))
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		_ = channel.Close()
		return
	}
}

func (s *deployrunSSHServer) address(t *testing.T) (string, int) {
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

func (s *deployrunSSHServer) close() {
	_ = s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, connection := range s.conns {
		_ = connection.Close()
	}
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type deployrunEnv struct {
	q       *store.Queries
	keyring keyringLike
	inner   *jobFlowDB
	db      *deployrunDB
	rt      *fake.Runtime
	ops     *hostfake.Ops
	h       *DeploymentRun
	log     *deployrunSQLLog
	host    string
	port    int
}

// keyringLike keeps the field usable without re-importing envelope in every
// signature below.
type keyringLike = deployrunKeyring

type deployrunKeyring interface {
	Encrypt(table, column, uuid string, plaintext []byte) ([]byte, error)
	Decrypt(table, column, uuid string, blob []byte) ([]byte, error)
}

func deployrunSetup(t *testing.T, respond func(string) (string, uint32)) *deployrunEnv {
	t.Helper()
	_, keyring, _, logger, inner := jobFlowDependencies(t)
	log := &deployrunSQLLog{}
	db := &deployrunDB{inner: inner, note: log.add}
	q := store.New(db)
	sshServer := deployrunNewSSHServer(t, respond)
	host, port := sshServer.address(t)
	inner.host, inner.port = host, port
	rt := deployrunRuntime()
	ops := &hostfake.Ops{}
	env := &deployrunEnv{q: q, keyring: keyring, inner: inner, db: db, rt: rt, ops: ops, log: log, host: host, port: port}
	env.h = &DeploymentRun{
		Store: q, Keyring: keyring, Audit: &audit.Recorder{Store: q, Logger: logger},
		Logger: logger, Docker: fixedSource{rt: rt}, HostOps: fixedHost{ops: ops},
	}
	return env
}

func deployrunRuntime() *fake.Runtime {
	rt := verifyRuntime(jobFixtureUUID + " " + deployrunPreviewUUID + " http://10.0.0.9:8080")
	rt.NetworkInspectFn = func(context.Context, string, networktypes.InspectOptions) (networktypes.Inspect, error) {
		return networktypes.Inspect{}, nil
	}
	rt.ImageInspectFn = func(context.Context, string, ...client.ImageInspectOption) (imagetypes.InspectResponse, error) {
		return imagetypes.InspectResponse{ID: "sha256:local", RepoDigests: []string{"registry.example/app@sha256:feed"}}, nil
	}
	rt.ImagePullFn = func(context.Context, string, imagetypes.PullOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"status":"Pulling","id":"a"}` + "\n")), nil
	}
	rt.ImagePushFn = func(context.Context, string, imagetypes.PushOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"status":"Pushing"}` + "\n")), nil
	}
	rt.ImageRemoveFn = func(context.Context, string, imagetypes.RemoveOptions) ([]imagetypes.DeleteResponse, error) {
		return nil, nil
	}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return deployrunInspect("running", "", "10.0.0.9"), nil
	}
	rt.ContainerLogsFn = func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
		return volumetypes.Volume{}, nil
	}
	rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
		return volumetypes.ListResponse{}, nil
	}
	return rt
}

func deployrunProxyOutputs(rt *fake.Runtime, outputs ...string) {
	scripted := verifyRuntime(outputs...)
	rt.ContainerExecCreateFn = scripted.ContainerExecCreateFn
	rt.ContainerExecAttachFn = scripted.ContainerExecAttachFn
	rt.ContainerExecInspectFn = scripted.ContainerExecInspectFn
}

func deployrunInspect(status, health, ip string) containertypes.InspectResponse {
	st := &containertypes.State{Status: status, Running: status == "running"}
	if health != "" {
		st.Health = &containertypes.Health{Status: health}
	}
	resp := containertypes.InspectResponse{
		ContainerJSONBase: &containertypes.ContainerJSONBase{State: st},
	}
	if ip != "" {
		resp.NetworkSettings = &containertypes.NetworkSettings{
			Networks: map[string]*networktypes.EndpointSettings{"unit-net": {IPAddress: ip}},
		}
	}
	return resp
}

func deployrunFastTimers(t *testing.T) {
	t.Helper()
	oldStable, oldPoll, oldCleanup := deploymentStablePeriod, deploymentHealthPoll, deploymentCleanupPollInterval
	deploymentStablePeriod, deploymentHealthPoll, deploymentCleanupPollInterval = time.Millisecond, time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		deploymentStablePeriod, deploymentHealthPoll, deploymentCleanupPollInterval = oldStable, oldPoll, oldCleanup
	})
}

func deployrunApp(t *testing.T) store.GetApplicationByIDRow {
	t.Helper()
	return store.GetApplicationByIDRow{
		Resource: store.Resource{
			ID: 1, Uuid: mustUUID(t, jobFixtureUUID), TeamID: 1, EnvironmentID: 1,
			DestinationID: 1, ResourceType: store.ResourceTypeApplication, Name: "unit-app",
		},
		Application: store.Application{
			PreviewProtection:  store.PreviewProtectionNone,
			AccessProtection:   store.PreviewProtectionNone,
			AccessPublicRoutes: []byte("[]"),
		},
		BuildConfig:   store.BuildConfig{BuildPack: store.BuildPackImage},
		RuntimeConfig: store.RuntimeConfig{StopGracePeriodSeconds: 1, ForceHttps: true},
	}
}

func deployrunServer(env *deployrunEnv) store.Server {
	return store.Server{
		ID: 1, Host: env.host, Port: int32(env.port), SshUser: "unit", SshTimeoutSeconds: 2,
		PrivateKeyID: 1, ProxyType: store.ProxyTypeTraefik, Status: store.ServerStatusReady,
	}
}

func deployrunDeployment(t *testing.T) store.Deployment {
	t.Helper()
	return store.Deployment{
		ID: 1, Uuid: mustUUID(t, deployrunDeployUUID), ResourceID: 1, ServerID: 1,
		Status: store.DeploymentStatusQueued, Trigger: store.DeploymentTriggerManual,
	}
}

func deployrunNewRun(env *deployrunEnv, d store.Deployment, app store.GetApplicationByIDRow) *deploymentRun {
	return &deploymentRun{
		h: env.h, jobID: 1, d: d, app: app, server: deployrunServer(env),
		dest: store.Destination{ID: 1, ServerID: 1, Network: "unit-net"}, teamUUID: jobFixtureUUID,
	}
}

func deployrunEncrypt(t *testing.T, env *deployrunEnv, table, column, value string) []byte {
	t.Helper()
	blob, err := env.keyring.Encrypt(table, column, jobFixtureUUID, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// ---------------------------------------------------------------------------
// Full pipeline flows driven through execute()
// ---------------------------------------------------------------------------

// A skip_build deployment redeploys the running artifact: no clone, no
// build, no push, no artifact record, no image reclamation — and both hooks
// run around a non-rolling switch (ADR-048, §7.4, §10).
func TestDeployrunSkipBuildFlowRunsHooksAndSwitchesNonRolling(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	deployrunProxyOutputs(env.rt, `{}`)
	app := deployrunApp(t)
	app.RuntimeConfig.MemoryLimit = ptr("64m")
	app.RuntimeConfig.PreDeploymentCommand = ptr("echo before")
	app.RuntimeConfig.PostDeploymentCommand = ptr("echo after")
	d := deployrunDeployment(t)
	d.SkipBuild = true
	d.ImageName, d.ImageTag = ptr("akerdock/app"), ptr("v1")
	d.CommitSha, d.CommitAuthor, d.GitBranch = ptr(deployrunFixtureSHA), ptr("Ada"), ptr("main")

	r := deployrunNewRun(env, d, app)
	err := r.execute(context.Background())
	r.close()
	if err != nil {
		t.Fatalf("skip_build execute: %v", err)
	}
	if r.rolling {
		t.Fatal("no health check must mean a non-rolling deployment")
	}
	if env.log.count("-- name: CreateDeploymentArtifact ") != 0 {
		t.Fatal("a skip_build deployment must not record a new artifact")
	}
	// The container ran the typed create with the reused image reference.
	created := false
	for _, c := range env.rt.Calls() {
		if c.Method != "ContainerCreate" {
			continue
		}
		created = true
		cfg := c.Args[0].(*containertypes.Config)
		if cfg.Image != "akerdock/app:v1" {
			t.Fatalf("candidate image = %q", cfg.Image)
		}
		host := c.Args[1].(*containertypes.HostConfig)
		if host.Memory != 64*1024*1024 {
			t.Fatalf("memory limit = %d", host.Memory)
		}
	}
	if !created {
		t.Fatal("no candidate container was created")
	}
}

func TestDeployrunInvalidMemoryLimitFailsBeforeStart(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	app := deployrunApp(t)
	app.RuntimeConfig.MemoryLimit = ptr("not-a-size")
	d := deployrunDeployment(t)
	d.SkipBuild = true
	d.ImageName, d.ImageTag = ptr("akerdock/app"), ptr("v1")

	r := deployrunNewRun(env, d, app)
	err := r.execute(context.Background())
	r.close()
	if err == nil || !strings.Contains(err.Error(), "invalid memory limit") {
		t.Fatalf("error = %v", err)
	}
}

// A rollback replays a pinned registry digest; when the image vanished from
// the server the verification fails with the rollback-specific message, and
// markFailed applies the compensation without touching the serving container.
func TestDeployrunRollbackMissingArtifactFailsAndMarksFailed(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	env.rt.ImageInspectFn = func(context.Context, string, ...client.ImageInspectOption) (imagetypes.InspectResponse, error) {
		return imagetypes.InspectResponse{}, fmt.Errorf("no such image: %w", cerrdefs.ErrNotFound)
	}
	app := deployrunApp(t)
	d := deployrunDeployment(t)
	d.IsRollback = true
	d.ImageName = ptr("akerdock/app")
	d.ImageDigest = ptr("registry.example/app@sha256:old")

	r := deployrunNewRun(env, d, app)
	err := r.execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no longer present") {
		t.Fatalf("error = %v", err)
	}
	r.markFailed(context.Background(), err)
	r.close()
	if r.d.ErrorMessage == nil || !strings.Contains(*r.d.ErrorMessage, "no longer present") {
		t.Fatalf("deployment error = %v", r.d.ErrorMessage)
	}
	if env.log.count("-- name: SetDeploymentError ") == 0 {
		t.Fatal("markFailed must persist the error message")
	}
	// The candidate (and only the candidate) was removed.
	removedCandidate := false
	for _, c := range env.rt.Calls() {
		if c.Method == "ContainerRemove" && c.Args[0].(string) == jobFixtureUUID+"-next" {
			removedCandidate = true
		}
	}
	if !removedCandidate {
		t.Fatal("compensation must remove the candidate container")
	}
}

func TestDeployrunMarkFailedNotifiesPreview(t *testing.T) {
	env := deployrunSetup(t, nil)
	app := deployrunApp(t)
	d := deployrunDeployment(t)
	r := deployrunNewRun(env, d, app)
	r.rt = env.rt
	fqdn := "pr-9.example.test"
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, deployrunPreviewUUID), PrID: 9, Fqdn: &fqdn}
	r.markFailed(context.Background(), errors.New("boom"))
	if env.log.count("-- name: SetPreviewStatus ") == 0 {
		t.Fatal("a failed preview deployment must mark the preview failed")
	}
}

// The image build pack pulls with a per-request registry credential, resolves
// the digest, and — with a working health check — performs the §7.2 rolling
// switch: route to the candidate IP, stop the old container, promote.
func TestDeployrunImagePullRollingSwitch(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	regBlob := deployrunEncrypt(t, env, "registry_credentials", "password_enc", "hunter2")
	env.db.rowFor = func(sql string) pgx.Row {
		switch {
		case strings.Contains(sql, "-- name: GetHealthCheck "):
			return deployrunRow{overrides: map[int]any{2: true}, blob: env.inner.blob}
		case strings.Contains(sql, "-- name: GetRegistryCredentialByID "):
			return deployrunRow{overrides: map[int]any{6: regBlob}, blob: env.inner.blob}
		}
		return nil
	}
	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		switch {
		case strings.Contains(sql, "-- name: ListDomainsForApplication "):
			return &jobFlowRows{remaining: 1, blob: env.inner.blob}, nil, true
		case strings.Contains(sql, "-- name: ListServiceComponents "):
			return &jobFlowRows{remaining: 0}, nil, true
		case strings.Contains(sql, "-- name: ListAppArtifactsOnServer "):
			return &deployrunRows{rows: []map[int]any{
				{0: int64(10), 1: "akerdock/app", 2: "new"},
				{0: int64(11), 1: "akerdock/app", 2: "old"},
				{0: int64(12), 1: "akerdock/app", 2: "new"},
			}, blob: env.inner.blob}, nil, true
		}
		return nil, nil, false
	}
	env.rt.ContainerInspectFn = func(_ context.Context, name string) (containertypes.InspectResponse, error) {
		return deployrunInspect("running", "healthy", "10.0.0.9"), nil
	}
	app := deployrunApp(t)
	app.RuntimeConfig.PortsExposes = ptr("8080, 9090")
	app.BuildConfig.RegistryCredentialID = ptrOf(int64(1))
	d := deployrunDeployment(t)
	d.ImageName, d.ImageTag = ptr("nginx"), ptr("1.27")

	r := deployrunNewRun(env, d, app)
	err := r.execute(context.Background())
	r.close()
	if err != nil {
		t.Fatalf("rolling image deployment: %v", err)
	}
	if !r.rolling {
		t.Fatal("a working health check must select the rolling path")
	}
	if r.digest != "registry.example/app@sha256:feed" {
		t.Fatalf("digest = %q", r.digest)
	}
	renamed := false
	for _, c := range env.rt.Calls() {
		if c.Method == "ContainerRename" {
			renamed = true
			if c.Args[0].(string) != jobFixtureUUID+"-next" || c.Args[1].(string) != jobFixtureUUID {
				t.Fatalf("rename %v", c.Args)
			}
		}
	}
	if !renamed {
		t.Fatal("the switch must promote the candidate by rename")
	}
	// Retention: keep=1 keeps the newest; the artifact sharing its ref is
	// dropped without an rmi; the distinct older one is reclaimed.
	if got := env.log.count("-- name: DeleteDeploymentArtifact "); got != 2 {
		t.Fatalf("artifact pointer drops = %d, want 2", got)
	}
	rmis := 0
	for _, c := range env.rt.Calls() {
		if c.Method == "ImageRemove" && c.Args[0].(string) == "akerdock/app:old" {
			rmis++
		}
	}
	if rmis != 1 {
		t.Fatalf("image reclamations = %d, want 1", rmis)
	}
}

// A single-container preview deploys next to production under its own uuid,
// with the dedicated env set, preview routing and the preview lifecycle
// bookkeeping (§20.4, INV-010/011).
func TestDeployrunPreviewImageFlow(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	authBlob := deployrunEncrypt(t, env, "environment_variables", "value_enc", "alice:s3cret")
	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		if strings.Contains(sql, "-- name: ListPreviewEnvVars ") {
			return &deployrunRows{rows: []map[int]any{
				{3: "AKERDOCK_PREVIEW_BASIC_AUTH", 4: authBlob},
			}, blob: env.inner.blob}, nil, true
		}
		return nil, nil, false
	}
	app := deployrunApp(t)
	app.Application.PreviewProtection = store.PreviewProtectionBasicAuth
	d := deployrunDeployment(t)
	d.ImageName, d.ImageTag = ptr("nginx"), ptr("1.27")
	fqdn := "pr-8.example.test"
	branch := "feat/preview"
	r := deployrunNewRun(env, d, app)
	r.preview = &store.Preview{
		ID: 1, Uuid: mustUUID(t, deployrunPreviewUUID), Provider: store.GitProviderGithub,
		PrID: 8, Fqdn: &fqdn, SourceBranch: &branch, Status: store.PreviewStatusDeploying,
	}

	err := r.execute(context.Background())
	r.close()
	if err != nil {
		t.Fatalf("preview deployment: %v", err)
	}
	if !strings.HasPrefix(r.previewAuth, "alice:$2") {
		t.Fatalf("preview auth hash = %q", r.previewAuth)
	}
	if env.log.count("-- name: SetPreviewDeployed ") == 0 {
		t.Fatal("a successful preview deployment must stamp last_deployed_at")
	}
	// The candidate carried the preview identity, not the resource's.
	for _, c := range env.rt.Calls() {
		if c.Method == "ContainerCreate" {
			if name := c.Args[4].(string); !strings.HasPrefix(name, deployrunPreviewUUID) {
				t.Fatalf("candidate name = %q", name)
			}
		}
	}
}

func TestDeployrunForkPreviewWithoutApprovalIsRefused(t *testing.T) {
	env := deployrunSetup(t, nil)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, deployrunPreviewUUID), IsFork: true}
	err := r.execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "INV-010") {
		t.Fatalf("error = %v", err)
	}
}

// A scale-to-zero preview routes through the waker: the helper is provisioned
// before the routing flips to it (ADR-036/037).
func TestDeployrunPreviewScaleToZeroProvisionsWaker(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	env.h.AgentImage = "akerdock/akerdock:test"
	env.h.InstanceURL = "http://cp.example.test"
	app := deployrunApp(t)
	app.Application.PreviewScaleToZero = true
	d := deployrunDeployment(t)
	d.ImageName, d.ImageTag = ptr("nginx"), ptr("1.27")
	fqdn := "pr-3.example.test"
	r := deployrunNewRun(env, d, app)
	r.preview = &store.Preview{
		ID: 1, Uuid: mustUUID(t, deployrunPreviewUUID), Provider: store.GitProviderGithub,
		PrID: 3, Fqdn: &fqdn,
	}

	err := r.execute(context.Background())
	r.close()
	if err != nil {
		t.Fatalf("scale-to-zero preview: %v", err)
	}
	// The waker routing table was deposited through the channel.
	deposited := false
	for _, p := range env.ops.CallsTo(agentwire.MethodFileWrite) {
		if strings.Contains(p.(agentwire.FileWriteParams).Path, "waker") ||
			strings.Contains(string(p.(agentwire.FileWriteParams).Content), deployrunPreviewUUID) {
			deposited = true
		}
	}
	if !deposited {
		t.Fatal("the waker config must be deposited before routing flips")
	}
}

// A scale-to-zero application skips the candidate-IP routing step and points
// the stable routing at the waker (ADR-037).
func TestDeployrunApplicationScaleToZeroRouting(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	env.h.AgentImage = "akerdock/akerdock:test"
	env.h.InstanceURL = "http://cp.example.test"
	env.db.rowFor = func(sql string) pgx.Row {
		if strings.Contains(sql, "-- name: GetHealthCheck ") {
			return deployrunRow{overrides: map[int]any{2: true}, blob: env.inner.blob}
		}
		return nil
	}
	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		switch {
		case strings.Contains(sql, "-- name: ListDomainsForApplication "):
			return &jobFlowRows{remaining: 1, blob: env.inner.blob}, nil, true
		case strings.Contains(sql, "-- name: ListServiceComponents "):
			return &jobFlowRows{remaining: 0}, nil, true
		}
		return nil, nil, false
	}
	env.rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return deployrunInspect("running", "healthy", "10.0.0.9"), nil
	}
	app := deployrunApp(t)
	app.Application.ScaleToZero = true
	d := deployrunDeployment(t)
	d.ImageName, d.ImageTag = ptr("nginx"), ptr("1.27")

	r := deployrunNewRun(env, d, app)
	err := r.execute(context.Background())
	r.close()
	if err != nil {
		t.Fatalf("scale-to-zero application: %v", err)
	}
	if !r.rolling {
		t.Fatal("expected the rolling path (health check present)")
	}
}

// The inline-Dockerfile build pack writes the Dockerfile through the channel
// and runs the typed BuildKit build with plain vars as args and secret vars
// as session secrets (§5.2/§5.3, ADR-055).
func TestDeployrunInlineDockerfileBuildFlow(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	plainBlob := deployrunEncrypt(t, env, "environment_variables", "value_enc", "hello")
	secretBlob := deployrunEncrypt(t, env, "environment_variables", "value_enc", "s3cret")
	runtimeBlob := deployrunEncrypt(t, env, "environment_variables", "value_enc", "runtime-only")
	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		switch {
		case strings.Contains(sql, "-- name: ListEnvVarsForDeploy "):
			return &deployrunRows{rows: []map[int]any{
				{3: "PLAIN", 4: plainBlob, 5: false, 6: true, 7: false},
				{3: "SECRET", 4: secretBlob, 5: true, 6: true, 7: false},
				{3: "RUNTIME", 4: runtimeBlob, 5: false, 6: false, 7: false},
			}, blob: env.inner.blob}, nil, true
		case strings.Contains(sql, "-- name: ListDomainsForApplication "):
			return &jobFlowRows{remaining: 1, blob: env.inner.blob}, nil, true
		case strings.Contains(sql, "-- name: ListServiceComponents "):
			return &jobFlowRows{remaining: 0}, nil, true
		}
		return nil, nil, false
	}
	app := deployrunApp(t)
	app.BuildConfig.BuildPack = store.BuildPackDockerfile
	app.BuildConfig.DockerfileContent = ptr("FROM scratch\n")
	d := deployrunDeployment(t)

	r := deployrunNewRun(env, d, app)
	err := r.execute(context.Background())
	r.close()
	if err != nil {
		t.Fatalf("inline dockerfile deployment: %v", err)
	}
	if r.digest != "sha256:local" {
		t.Fatalf("local digest = %q", r.digest)
	}
	builds := env.ops.CallsTo(agentwire.MethodImageBuild)
	if len(builds) != 1 {
		t.Fatalf("builds = %d", len(builds))
	}
	p := builds[0].(agentwire.ImageBuildParams)
	if p.BuildArgs["PLAIN"] != "hello" {
		t.Fatalf("build args = %v", p.BuildArgs)
	}
	if string(p.Secrets["SECRET"]) != "s3cret" {
		t.Fatal("the secret variable must ride as a BuildKit secret")
	}
	if _, leaked := p.BuildArgs["SECRET"]; leaked {
		t.Fatal("a secret variable must never become a build arg")
	}
	// The runtime env reached the typed create, deployment identity included.
	for _, c := range env.rt.Calls() {
		if c.Method != "ContainerCreate" {
			continue
		}
		envList := strings.Join(c.Args[0].(*containertypes.Config).Env, "\n")
		if !strings.Contains(envList, "RUNTIME=runtime-only") ||
			!strings.Contains(envList, "AKERDOCK_FQDN=unit") ||
			!strings.Contains(envList, "AKERDOCK_URL=https://unit") {
			t.Fatalf("candidate env = %q", envList)
		}
	}
	if env.log.count("-- name: CreateDeploymentArtifact ") != 1 {
		t.Fatal("a fresh build must record a rollback artifact")
	}
}

func TestDeployrunInlineDockerfileWithoutContentFails(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	app := deployrunApp(t)
	app.BuildConfig.BuildPack = store.BuildPackDockerfile
	r := deployrunNewRun(env, deployrunDeployment(t), app)
	err := r.execute(context.Background())
	r.close()
	if err == nil || !strings.Contains(err.Error(), "no Dockerfile content") {
		t.Fatalf("error = %v", err)
	}
}

func TestDeployrunInlineDockerfileWriteFailure(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	env.ops.WriteFileFn = func(context.Context, agentwire.FileWriteParams) error {
		return errors.New("disk full")
	}
	app := deployrunApp(t)
	app.BuildConfig.BuildPack = store.BuildPackDockerfile
	app.BuildConfig.DockerfileContent = ptr("FROM scratch\n")
	r := deployrunNewRun(env, deployrunDeployment(t), app)
	err := r.execute(context.Background())
	r.close()
	if err == nil || !strings.Contains(err.Error(), "writing the Dockerfile failed") {
		t.Fatalf("error = %v", err)
	}
}

// A preview git build pins the announced PR head: the clone fetches the base
// repository's pull ref, verify_head proves the SHA, and the commit metadata
// is recorded best-effort (§20.4.8).
func TestDeployrunPreviewGitBuildVerifiesHeadAndRecordsMeta(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, func(command string) (string, uint32) {
		if strings.Contains(command, "git log -1") {
			return "Ada Lovelace\x1fFix the flux capacitor\n", 0
		}
		return jobCommandOutput(command), 0
	})
	app := deployrunApp(t)
	app.BuildConfig.BuildPack = store.BuildPackDockerfile
	app.BuildConfig.DockerfilePath = ptr("./docker/Dockerfile")
	app.Application.GitRepositoryUrl = ptr("https://example.test/acme/app.git")
	app.Application.GitBranch = ptr("main")
	d := deployrunDeployment(t)
	fqdn := "pr-7.example.test"
	sha := deployrunFixtureSHA
	branch := "feat/head"
	r := deployrunNewRun(env, d, app)
	r.preview = &store.Preview{
		ID: 1, Uuid: mustUUID(t, deployrunPreviewUUID), Provider: store.GitProviderGithub,
		PrID: 7, HeadSha: &sha, SourceBranch: &branch, Fqdn: &fqdn,
	}

	err := r.execute(context.Background())
	r.close()
	if err != nil {
		t.Fatalf("preview git deployment: %v", err)
	}
	if env.log.count("-- name: SetDeploymentCommitMeta ") == 0 {
		t.Fatal("the commit author/subject must be recorded")
	}
	if r.d.CommitAuthor == nil || *r.d.CommitAuthor != "Ada Lovelace" {
		t.Fatalf("commit author = %v", r.d.CommitAuthor)
	}
	builds := env.ops.CallsTo(agentwire.MethodImageBuild)
	if len(builds) != 1 || builds[0].(agentwire.ImageBuildParams).Dockerfile != "docker/Dockerfile" {
		t.Fatalf("build calls = %v", builds)
	}
}

func TestDeployrunPreviewGitHeadMismatchFails(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, func(command string) (string, uint32) {
		if strings.Contains(command, "git rev-parse HEAD") {
			return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", 0
		}
		return jobCommandOutput(command), 0
	})
	app := deployrunApp(t)
	app.BuildConfig.BuildPack = store.BuildPackDockerfile
	app.Application.GitRepositoryUrl = ptr("https://example.test/acme/app.git")
	sha := deployrunFixtureSHA
	fqdn := "pr-7.example.test"
	r := deployrunNewRun(env, deployrunDeployment(t), app)
	r.preview = &store.Preview{
		ID: 1, Uuid: mustUUID(t, deployrunPreviewUUID), Provider: store.GitProviderGithub,
		PrID: 7, HeadSha: &sha, Fqdn: &fqdn,
	}
	err := r.execute(context.Background())
	r.close()
	if err == nil || !strings.Contains(err.Error(), "the pull request moved") {
		t.Fatalf("error = %v", err)
	}
}

func TestDeployrunGitBranchResolutionFailure(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, func(command string) (string, uint32) {
		if strings.Contains(command, "git ls-remote") {
			return "nothing-useful\n", 0
		}
		return jobCommandOutput(command), 0
	})
	app := deployrunApp(t)
	app.BuildConfig.BuildPack = store.BuildPackDockerfile
	app.Application.GitRepositoryUrl = ptr("https://example.test/acme/app.git")
	r := deployrunNewRun(env, deployrunDeployment(t), app)
	err := r.execute(context.Background())
	r.close()
	if err == nil || !strings.Contains(err.Error(), "not found on") {
		t.Fatalf("error = %v", err)
	}
}

// The nixpacks static mode builds a builder image, then packages the produced
// directory into nginx and removes the intermediate (§5.5).
func TestDeployrunNixpacksStaticPublishFlow(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	deployrunProxyOutputs(env.rt, `{}`)
	app := deployrunApp(t)
	app.BuildConfig.BuildPack = store.BuildPackNixpacks
	app.BuildConfig.PublishDirectory = ptr("dist")
	app.BuildConfig.CustomNginxConfig = ptr("server { listen 80; }")
	app.Application.GitRepositoryUrl = ptr("https://example.test/acme/site.git")
	d := deployrunDeployment(t)
	d.ForceRebuild = true

	r := deployrunNewRun(env, d, app)
	err := r.execute(context.Background())
	r.close()
	if err != nil {
		t.Fatalf("nixpacks static deployment: %v", err)
	}
	var wroteNginx, wroteDockerfile bool
	for _, p := range env.ops.CallsTo(agentwire.MethodFileWrite) {
		w := p.(agentwire.FileWriteParams)
		if strings.Contains(string(w.Content), "server { listen 80; }") {
			wroteNginx = true
		}
		if strings.Contains(string(w.Content), "COPY --from=build /app/dist") {
			wroteDockerfile = true
		}
	}
	if !wroteNginx || !wroteDockerfile {
		t.Fatalf("packaging writes: nginx=%v dockerfile=%v", wroteNginx, wroteDockerfile)
	}
	removedBuilder := false
	for _, c := range env.rt.Calls() {
		if c.Method == "ImageRemove" && strings.HasSuffix(c.Args[0].(string), "-builder") {
			removedBuilder = true
		}
	}
	if !removedBuilder {
		t.Fatal("the intermediate builder image must be reclaimed")
	}
}

// The static build pack synthesizes the Dockerfile next to the sources.
func TestDeployrunWriteStaticDockerfile(t *testing.T) {
	env := deployrunSetup(t, nil)
	app := deployrunApp(t)
	r := deployrunNewRun(env, deployrunDeployment(t), app)
	r.hops = env.ops

	if err := r.writeStaticDockerfile(context.Background(), "/src", ""); err != nil {
		t.Fatal(err)
	}
	writes := env.ops.CallsTo(agentwire.MethodFileWrite)
	if len(writes) != 2 {
		t.Fatalf("writes = %d", len(writes))
	}
	if !strings.Contains(string(writes[0].(agentwire.FileWriteParams).Content), "try_files") {
		t.Fatal("default nginx config must serve a SPA")
	}
	if !strings.Contains(string(writes[1].(agentwire.FileWriteParams).Content), "COPY . /usr/share/nginx/html") {
		t.Fatalf("dockerfile = %s", writes[1].(agentwire.FileWriteParams).Content)
	}

	// Custom publish directory and nginx config.
	app.BuildConfig.PublishDirectory = ptr("public")
	app.BuildConfig.CustomNginxConfig = ptr("server { }")
	r2 := deployrunNewRun(env, deployrunDeployment(t), app)
	ops2 := &hostfake.Ops{}
	r2.hops = ops2
	if err := r2.writeStaticDockerfile(context.Background(), "/src", "web"); err != nil {
		t.Fatal(err)
	}
	writes2 := ops2.CallsTo(agentwire.MethodFileWrite)
	if string(writes2[0].(agentwire.FileWriteParams).Content) != "server { }" {
		t.Fatal("custom nginx config must be used verbatim")
	}
	if !strings.Contains(string(writes2[1].(agentwire.FileWriteParams).Content), "COPY ./public /usr/share/nginx/html") {
		t.Fatalf("dockerfile = %s", writes2[1].(agentwire.FileWriteParams).Content)
	}

	// Both write failures surface as errors.
	failing := &hostfake.Ops{WriteFileFn: func(_ context.Context, p agentwire.FileWriteParams) error {
		return errors.New("readonly filesystem")
	}}
	r3 := deployrunNewRun(env, deployrunDeployment(t), app)
	r3.hops = failing
	if err := r3.writeStaticDockerfile(context.Background(), "/src", ""); err == nil ||
		!strings.Contains(err.Error(), "nginx config failed") {
		t.Fatalf("error = %v", err)
	}
	count := 0
	selective := &hostfake.Ops{WriteFileFn: func(_ context.Context, p agentwire.FileWriteParams) error {
		count++
		if count == 2 {
			return errors.New("disk full")
		}
		return nil
	}}
	r4 := deployrunNewRun(env, deployrunDeployment(t), app)
	r4.hops = selective
	if err := r4.writeStaticDockerfile(context.Background(), "/src", ""); err == nil ||
		!strings.Contains(err.Error(), "static Dockerfile failed") {
		t.Fatalf("error = %v", err)
	}
}

// A build-server deployment builds elsewhere, pushes to the registry, and the
// target pulls BY DIGEST (§3.4, ADR-055).
func TestDeployrunBuildServerPushAndPullByDigest(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	deployrunProxyOutputs(env.rt, `{}`)
	env.inner.assignBuildServerBlocks = 1 // one wait cycle before the reservation lands
	regBlob := deployrunEncrypt(t, env, "registry_credentials", "password_enc", "push-secret")
	env.db.rowFor = func(sql string) pgx.Row {
		if strings.Contains(sql, "-- name: GetRegistryCredentialByID ") {
			return deployrunRow{overrides: map[int]any{6: regBlob}, blob: env.inner.blob}
		}
		return nil
	}
	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		if strings.Contains(sql, "-- name: ListReadyBuildServers ") {
			return &deployrunRows{rows: []map[int]any{
				{5: env.host, 6: int32(env.port), 7: "unit", 9: int32(2), 10: int64(1)},
			}, blob: env.inner.blob}, nil, true
		}
		return nil, nil, false
	}
	env.rt.ImageInspectFn = func(_ context.Context, ref string, _ ...client.ImageInspectOption) (imagetypes.InspectResponse, error) {
		repo := ref
		if i := strings.LastIndex(ref, ":"); i > 0 {
			repo = ref[:i]
		}
		return imagetypes.InspectResponse{ID: "sha256:local", RepoDigests: []string{repo + "@sha256:pushed"}}, nil
	}
	app := deployrunApp(t)
	app.BuildConfig.BuildPack = store.BuildPackDockerfile
	app.BuildConfig.DockerfileContent = ptr("FROM scratch\n")
	app.BuildConfig.UseBuildServer = true
	app.BuildConfig.PushRegistryCredentialID = ptrOf(int64(1))
	d := deployrunDeployment(t)

	r := deployrunNewRun(env, d, app)
	err := r.execute(context.Background())
	r.close()
	if err != nil {
		t.Fatalf("build-server deployment: %v", err)
	}
	if r.builder == nil {
		t.Fatal("a build server must have been dialled")
	}
	wantDigest := "unit/akerdock/" + jobFixtureUUID + "@sha256:pushed"
	if r.digest != wantDigest {
		t.Fatalf("digest = %q, want %q", r.digest, wantDigest)
	}
	if r.d.ImageName == nil || *r.d.ImageName != "unit/akerdock/"+jobFixtureUUID {
		t.Fatalf("image name = %v", r.d.ImageName)
	}
	if env.inner.assignBuildServerCalls < 2 {
		t.Fatalf("reservation attempts = %d, want a wait cycle", env.inner.assignBuildServerCalls)
	}
	// bc/bcrt/bhostops answer the builder while one is dialled.
	if r.bc() != r.builder || !reflect.DeepEqual(r.bcrt(), r.brt) {
		t.Fatal("build-side accessors must answer the build server")
	}
}

func TestDeployrunDialBuildServerFailures(t *testing.T) {
	env := deployrunSetup(t, nil)
	app := deployrunApp(t)
	app.BuildConfig.UseBuildServer = true

	t.Run("none ready", func(t *testing.T) {
		env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
			if strings.Contains(sql, "-- name: ListReadyBuildServers ") {
				return &deployrunRows{}, nil, true
			}
			return nil, nil, false
		}
		r := deployrunNewRun(env, deployrunDeployment(t), app)
		if err := r.dialBuildServer(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "none is ready") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("list failure", func(t *testing.T) {
		env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
			if strings.Contains(sql, "-- name: ListReadyBuildServers ") {
				return nil, errors.New("database unavailable"), true
			}
			return nil, nil, false
		}
		r := deployrunNewRun(env, deployrunDeployment(t), app)
		if err := r.dialBuildServer(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "database unavailable") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("architecture mismatch", func(t *testing.T) {
		env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
			if strings.Contains(sql, "-- name: ListReadyBuildServers ") {
				return &deployrunRows{rows: []map[int]any{
					{5: env.host, 6: int32(env.port), 7: "unit", 9: int32(2), 15: "arm64"},
				}, blob: env.inner.blob}, nil, true
			}
			return nil, nil, false
		}
		r := deployrunNewRun(env, deployrunDeployment(t), app)
		r.server.Architecture = ptr("amd64")
		if err := r.dialBuildServer(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "would not run there") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestDeployrunPushBuiltImageWithoutRegistryFails(t *testing.T) {
	env := deployrunSetup(t, nil)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	if _, err := r.pushBuiltImage(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no push registry is configured") {
		t.Fatalf("error = %v", err)
	}
}

func TestDeployrunRegistryAuthErrors(t *testing.T) {
	env := deployrunSetup(t, nil)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))

	if auth, err := r.registryAuth(context.Background(), nil); err != nil || auth != "" {
		t.Fatalf("nil credential = %q, %v", auth, err)
	}
	env.db.rowFor = func(sql string) pgx.Row {
		if strings.Contains(sql, "-- name: GetRegistryCredentialByID ") {
			return deployrunRow{err: errors.New("gone")}
		}
		return nil
	}
	if _, err := r.registryAuth(context.Background(), ptrOf(int64(1))); err == nil ||
		!strings.Contains(err.Error(), "is gone") {
		t.Fatalf("error = %v", err)
	}
	// The default fixture blob is encrypted under a different AAD: decrypt fails.
	env.db.rowFor = nil
	if _, err := r.registryAuth(context.Background(), ptrOf(int64(1))); err == nil ||
		!strings.Contains(err.Error(), "cannot decrypt") {
		t.Fatalf("error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Crash recovery (§2.5): the resume inspection decides from what exists.
// ---------------------------------------------------------------------------

func TestDeployrunResumeBranches(t *testing.T) {
	appUUID := jobFixtureUUID
	candidate := appUUID + "-next"
	inspectBy := func(oldState, nextState string) func(context.Context, string) (containertypes.InspectResponse, error) {
		return func(_ context.Context, name string) (containertypes.InspectResponse, error) {
			state := oldState
			if name == candidate {
				state = nextState
			}
			if state == "absent" {
				return containertypes.InspectResponse{}, fmt.Errorf("no such container: %w", cerrdefs.ErrNotFound)
			}
			return deployrunInspect(state, "", ""), nil
		}
	}

	cases := map[string]struct {
		old, next string
		wantErr   string
		renames   int
	}{
		"switch completed": {old: "running", next: "absent"},
		"resume at rename": {old: "absent", next: "running", renames: 1},
		"both alive":       {old: "running", next: "running", renames: 1},
		"nothing usable":   {old: "absent", next: "absent", wantErr: "neither container is usable"},
		"candidate dead":   {old: "exited", next: "exited", wantErr: "neither container is usable"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := deployrunSetup(t, nil)
			env.rt.ContainerInspectFn = inspectBy(tc.old, tc.next)
			d := deployrunDeployment(t)
			d.Status = store.DeploymentStatusSwitching
			r := deployrunNewRun(env, d, deployrunApp(t))
			r.server.ProxyType = store.ProxyType("none")
			r.rt = env.rt

			done, err := r.resume(context.Background(), appUUID, appUUID, candidate)
			if !done {
				t.Fatal("a switching resume must be terminal here")
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("resume: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v", err)
			}
			renames := 0
			for _, c := range env.rt.Calls() {
				if c.Method == "ContainerRename" {
					renames++
				}
			}
			if renames != tc.renames {
				t.Fatalf("renames = %d, want %d", renames, tc.renames)
			}
		})
	}

	t.Run("earlier states replay", func(t *testing.T) {
		env := deployrunSetup(t, nil)
		for _, status := range []store.DeploymentStatus{store.DeploymentStatusQueued, store.DeploymentStatusBuilding} {
			d := deployrunDeployment(t)
			d.Status = status
			r := deployrunNewRun(env, d, deployrunApp(t))
			done, err := r.resume(context.Background(), appUUID, appUUID, candidate)
			if done || err != nil {
				t.Fatalf("%s: done=%v err=%v", status, done, err)
			}
		}
	})

	t.Run("inspection failure propagates", func(t *testing.T) {
		env := deployrunSetup(t, nil)
		env.rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return containertypes.InspectResponse{}, errors.New("daemon down")
		}
		d := deployrunDeployment(t)
		d.Status = store.DeploymentStatusFinishing
		r := deployrunNewRun(env, d, deployrunApp(t))
		r.rt = env.rt
		if _, err := r.resume(context.Background(), appUUID, appUUID, candidate); err == nil ||
			!strings.Contains(err.Error(), "daemon down") {
			t.Fatalf("error = %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Hooks (§10)
// ---------------------------------------------------------------------------

func TestDeployrunRunHook(t *testing.T) {
	env := deployrunSetup(t, nil)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))

	if err := r.runHook(context.Background(), "pre_deployment", nil, "c"); err != nil {
		t.Fatalf("no command: %v", err)
	}
	empty := "   "
	if err := r.runHook(context.Background(), "pre_deployment", &empty, "c"); err != nil {
		t.Fatalf("blank command: %v", err)
	}

	cmd := "echo run"
	t.Run("runtime unavailable", func(t *testing.T) {
		broken := *env.h
		broken.Docker = unavailableDocker{}
		r2 := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
		r2.h = &broken
		if err := r2.runHook(context.Background(), "pre_deployment", &cmd, "c"); err == nil {
			t.Fatal("a missing agent channel must fail the hook")
		}
	})

	t.Run("pre skips absent container", func(t *testing.T) {
		env.rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return containertypes.InspectResponse{}, fmt.Errorf("gone: %w", cerrdefs.ErrNotFound)
		}
		if err := r.runHook(context.Background(), "pre_deployment", &cmd, "c"); err != nil {
			t.Fatalf("absent container: %v", err)
		}
	})

	t.Run("pre inspect failure propagates", func(t *testing.T) {
		env.rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return containertypes.InspectResponse{}, errors.New("daemon down")
		}
		if err := r.runHook(context.Background(), "pre_deployment", &cmd, "c"); err == nil {
			t.Fatal("inspect failure must fail the hook")
		}
	})

	t.Run("pre skips stopped container", func(t *testing.T) {
		env.rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return deployrunInspect("exited", "", ""), nil
		}
		if err := r.runHook(context.Background(), "pre_deployment", &cmd, "c"); err != nil {
			t.Fatalf("stopped container: %v", err)
		}
	})

	t.Run("post runs in the candidate", func(t *testing.T) {
		env.rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return deployrunInspect("running", "", ""), nil
		}
		if err := r.runHook(context.Background(), "post_deployment", &cmd, "c"); err != nil {
			t.Fatalf("post hook: %v", err)
		}
	})

	t.Run("non-zero exit fails the step", func(t *testing.T) {
		env.rt.ContainerExecInspectFn = func(context.Context, string) (containertypes.ExecInspect, error) {
			return containertypes.ExecInspect{ExitCode: 5}, nil
		}
		err := r.runHook(context.Background(), "post_deployment", &cmd, "c")
		if err == nil || !strings.Contains(err.Error(), "exit code 5") {
			t.Fatalf("error = %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Preview protections (§20.4.4, ADR-030)
// ---------------------------------------------------------------------------

func TestDeployrunPreviewAuthHash(t *testing.T) {
	env := deployrunSetup(t, nil)
	mk := func() *deploymentRun {
		r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
		r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, deployrunPreviewUUID)}
		return r
	}
	rows := func(overrides map[int]any) func(string) (pgx.Rows, error, bool) {
		return func(sql string) (pgx.Rows, error, bool) {
			if strings.Contains(sql, "-- name: ListPreviewEnvVars ") {
				return &deployrunRows{rows: []map[int]any{overrides}, blob: env.inner.blob}, nil, true
			}
			return nil, nil, false
		}
	}

	authBlob := deployrunEncrypt(t, env, "environment_variables", "value_enc", "alice:pw")
	env.db.rowsFor = rows(map[int]any{3: "AKERDOCK_PREVIEW_BASIC_AUTH", 4: authBlob})
	r := mk()
	hash := r.previewAuthHash(context.Background())
	if !strings.HasPrefix(hash, "alice:$2") {
		t.Fatalf("hash = %q", hash)
	}
	if again := r.previewAuthHash(context.Background()); again != hash {
		t.Fatal("the hash must be cached per run")
	}

	badBlob := deployrunEncrypt(t, env, "environment_variables", "value_enc", "no-colon")
	env.db.rowsFor = rows(map[int]any{3: "AKERDOCK_PREVIEW_BASIC_AUTH", 4: badBlob})
	if got := mk().previewAuthHash(context.Background()); got != "" {
		t.Fatalf("malformed secret = %q", got)
	}

	env.db.rowsFor = rows(map[int]any{3: "AKERDOCK_PREVIEW_BASIC_AUTH", 4: []byte("junk")})
	if got := mk().previewAuthHash(context.Background()); got != "" {
		t.Fatalf("undecryptable secret = %q", got)
	}

	env.db.rowsFor = rows(map[int]any{3: "OTHER"})
	if got := mk().previewAuthHash(context.Background()); got != "" {
		t.Fatalf("no matching key = %q", got)
	}

	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		if strings.Contains(sql, "-- name: ListPreviewEnvVars ") {
			return nil, errors.New("db down"), true
		}
		return nil, nil, false
	}
	if got := mk().previewAuthHash(context.Background()); got != "" {
		t.Fatalf("list failure = %q", got)
	}
}

func TestDeployrunPreviewSSOAuthURL(t *testing.T) {
	env := deployrunSetup(t, nil)
	app := deployrunApp(t)
	r := deployrunNewRun(env, deployrunDeployment(t), app)

	if url, err := r.previewSSOAuthURL(context.Background()); err != nil || url != "" {
		t.Fatalf("non-sso = %q, %v", url, err)
	}

	r.app.Application.PreviewProtection = store.PreviewProtectionSso
	if _, err := r.previewSSOAuthURL(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "instance FQDN") {
		t.Fatalf("missing fqdn error = %v", err)
	}

	env.db.rowFor = func(sql string) pgx.Row {
		if strings.Contains(sql, "-- name: GetInstanceSettings ") {
			return deployrunRow{overrides: map[int]any{1: "cp.example.test"}, blob: env.inner.blob}
		}
		return nil
	}
	url, err := r.previewSSOAuthURL(context.Background())
	if err != nil || url != "https://cp.example.test/webhooks/previews/forward-auth" {
		t.Fatalf("public url = %q, %v", url, err)
	}

	r.server.IsLocalhost = true
	r.h.ControlPlanePort = 9443
	url, err = r.previewSSOAuthURL(context.Background())
	if err != nil || url != "http://host.docker.internal:9443/webhooks/previews/forward-auth" {
		t.Fatalf("gateway url = %q, %v", url, err)
	}
	r.h.ControlPlanePort = 0

	env.db.rowFor = func(sql string) pgx.Row {
		if strings.Contains(sql, "-- name: GetInstanceSettings ") {
			return deployrunRow{err: errors.New("settings gone")}
		}
		return nil
	}
	if _, err := r.previewSSOAuthURL(context.Background()); err == nil {
		t.Fatal("settings failure must propagate")
	}
}

// ---------------------------------------------------------------------------
// setStatus wait loop, cancellation, benign state changes (§2.6, §21.1)
// ---------------------------------------------------------------------------

func TestDeployrunExecuteHonoursCancellation(t *testing.T) {
	env := deployrunSetup(t, nil)
	previous := jobEnumValues["DeploymentStatus"]
	jobEnumValues["DeploymentStatus"] = string(store.DeploymentStatusQueued)
	defer func() { jobEnumValues["DeploymentStatus"] = previous }()
	env.db.rowFor = func(sql string) pgx.Row {
		if strings.Contains(sql, "-- name: IsJobCancelRequested ") {
			return deployrunRow{overrides: map[int]any{0: true}}
		}
		return nil
	}
	j := store.Job{ID: 9, JobType: TypeDeploymentRun, Payload: []byte(`{"deployment_id":1}`)}
	result, err := env.h.Execute(context.Background(), j, nil)
	if err != nil {
		t.Fatalf("cancelled execute: %v", err)
	}
	if result.(map[string]any)["deployment_status"] != "cancelled" {
		t.Fatalf("result = %#v", result)
	}
}

func TestDeployrunMarkCancelledRemovesCandidate(t *testing.T) {
	env := deployrunSetup(t, nil)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	r.rt = env.rt
	r.markCancelled(context.Background())
	removed := false
	for _, c := range env.rt.Calls() {
		if c.Method == "ContainerRemove" && c.Args[0].(string) == jobFixtureUUID+"-next" {
			removed = true
		}
	}
	if !removed {
		t.Fatal("markCancelled must remove the candidate")
	}
}

func TestDeployrunExecuteReportsBenignStateChange(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	previous := jobEnumValues["DeploymentStatus"]
	jobEnumValues["DeploymentStatus"] = string(store.DeploymentStatusQueued)
	defer func() { jobEnumValues["DeploymentStatus"] = previous }()
	env.inner.startDeploymentBlocks = 1
	superseded := store.DeploymentStatusSuperseded
	env.inner.deploymentStatus = &superseded

	j := store.Job{ID: 10, JobType: TypeDeploymentRun, Payload: []byte(`{"deployment_id":1}`)}
	result, err := env.h.Execute(context.Background(), j, nil)
	if err != nil {
		t.Fatalf("state-changed execute: %v", err)
	}
	if result.(map[string]any)["deployment_status"] != "superseded" {
		t.Fatalf("result = %#v", result)
	}
	// The error text itself is part of the contract.
	e := &deploymentStateChangedError{status: superseded}
	if !strings.Contains(e.Error(), "superseded") {
		t.Fatalf("Error() = %q", e.Error())
	}
}

func TestDeployrunSetStatusWaitsOutServerCleanup(t *testing.T) {
	deployrunFastTimers(t)
	env := deployrunSetup(t, nil)
	env.inner.startDeploymentBlocks = 1
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	if err := r.setStatus(context.Background(), store.DeploymentStatusPreparing); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	if env.inner.startDeploymentCalls != 2 {
		t.Fatalf("start attempts = %d, want 2", env.inner.startDeploymentCalls)
	}
}

func TestDeployrunSetStatusWaitStopsOnContextCancel(t *testing.T) {
	old := deploymentCleanupPollInterval
	deploymentCleanupPollInterval = time.Hour
	t.Cleanup(func() { deploymentCleanupPollInterval = old })
	env := deployrunSetup(t, nil)
	env.inner.startDeploymentBlocks = 5
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	if err := r.setStatus(ctx, store.DeploymentStatusPreparing); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestDeployrunReserveBuildServerStopsOnContextCancel(t *testing.T) {
	old := deploymentCleanupPollInterval
	deploymentCleanupPollInterval = time.Hour
	t.Cleanup(func() { deploymentCleanupPollInterval = old })
	env := deployrunSetup(t, nil)
	env.inner.assignBuildServerBlocks = 5
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	if err := r.reserveBuildServer(ctx, deployrunServer(env)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Execute-level error paths
// ---------------------------------------------------------------------------

func TestDeployrunExecuteErrorPaths(t *testing.T) {
	previous := jobEnumValues["DeploymentStatus"]
	jobEnumValues["DeploymentStatus"] = string(store.DeploymentStatusQueued)
	defer func() { jobEnumValues["DeploymentStatus"] = previous }()
	j := store.Job{ID: 11, JobType: TypeDeploymentRun, Payload: []byte(`{"deployment_id":1}`)}

	t.Run("agent channel unavailable marks failed", func(t *testing.T) {
		env := deployrunSetup(t, nil)
		env.h.Docker = unavailableDocker{}
		result, err := env.h.Execute(context.Background(), j, nil)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		m := result.(map[string]any)
		if m["deployment_status"] != "failed" || !strings.Contains(m["error"].(string), "agent is not connected") {
			t.Fatalf("result = %#v", m)
		}
	})

	t.Run("host ops unavailable marks failed", func(t *testing.T) {
		env := deployrunSetup(t, nil)
		env.h.HostOps = unavailableHost{}
		result, err := env.h.Execute(context.Background(), j, nil)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if result.(map[string]any)["deployment_status"] != "failed" {
			t.Fatalf("result = %#v", result)
		}
	})

	failOn := func(env *deployrunEnv, names ...string) {
		env.db.rowFor = func(sql string) pgx.Row {
			for _, n := range names {
				if strings.Contains(sql, "-- name: "+n+" ") {
					return deployrunRow{err: errors.New(n + " unavailable")}
				}
			}
			return nil
		}
	}

	for _, tc := range []struct {
		name    string
		queries []string
		wantErr string
		preview bool
	}{
		{name: "deployment vanished", queries: []string{"GetDeploymentByID"}, wantErr: "deployment not found"},
		{name: "application vanished", queries: []string{"GetApplicationByID", "GetResourceByID"}, wantErr: "application vanished"},
		{name: "resource is no service", queries: []string{"GetApplicationByID"}, wantErr: "application vanished"},
		{name: "server vanished", queries: []string{"GetServerByID"}, wantErr: "server vanished"},
		{name: "destination vanished", queries: []string{"GetDestinationByID"}, wantErr: "destination vanished"},
		{name: "team vanished", queries: []string{"GetTeamByID"}, wantErr: "team vanished"},
		{name: "preview vanished", queries: []string{"GetPreviewByID"}, wantErr: "preview vanished", preview: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := deployrunSetup(t, nil)
			env.inner.preview = tc.preview
			failOn(env, tc.queries...)
			_, err := env.h.Execute(context.Background(), j, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	t.Run("service resource takes the compose pipeline", func(t *testing.T) {
		env := deployrunSetup(t, nil)
		env.h.Docker = unavailableDocker{} // fail fast once the service run reaches the channel
		prevType := jobEnumValues["ResourceType"]
		jobEnumValues["ResourceType"] = string(store.ResourceTypeService)
		defer func() { jobEnumValues["ResourceType"] = prevType }()
		failOn(env, "GetApplicationByID")
		result, err := env.h.Execute(context.Background(), j, nil)
		if err != nil {
			t.Fatalf("service execute: %v", err)
		}
		if result.(map[string]any)["deployment_status"] != "failed" {
			t.Fatalf("result = %#v", result)
		}
	})
}

// ---------------------------------------------------------------------------
// Storage convergence (§8)
// ---------------------------------------------------------------------------

func TestDeployrunPrepareStorages(t *testing.T) {
	env := deployrunSetup(t, nil)
	storageRows := func(rows ...map[int]any) func(string) (pgx.Rows, error, bool) {
		return func(sql string) (pgx.Rows, error, bool) {
			if strings.Contains(sql, "-- name: ListStoragesForResource ") {
				return &deployrunRows{rows: rows, blob: env.inner.blob}, nil, true
			}
			return nil, nil, false
		}
	}
	env.rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
		return volumetypes.Volume{}, fmt.Errorf("no such volume: %w", cerrdefs.ErrNotFound)
	}
	client, err := serverdial.Open(context.Background(), env.q, env.h.Keyring, deployrunServer(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	env.db.rowsFor = storageRows(
		map[int]any{3: store.StorageKindVolume, 4: "data", 6: "/data"},
		map[int]any{3: store.StorageKindVolume, 4: "adopted", 6: "/adopted", 16: "legacy-vol"},
		map[int]any{3: store.StorageKindBind, 5: "/srv/files", 6: "/files"},
		map[int]any{3: store.StorageKindVolume, 6: "/skipped"}, // no name
		map[int]any{3: store.StorageKindBind, 6: "/skipped"},   // no host path
	)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	r.rt, r.client = env.rt, client
	binds, err := r.prepareStorages(context.Background(), jobFixtureUUID, "")
	if err != nil {
		t.Fatalf("prepareStorages: %v", err)
	}
	want := []string{jobFixtureUUID + "_data:/data", "legacy-vol:/adopted", "/srv/files:/files"}
	if !reflect.DeepEqual(binds, want) {
		t.Fatalf("binds = %v, want %v", binds, want)
	}

	// A preview never mounts adopted volumes or host binds (INV-010).
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, deployrunPreviewUUID)}
	binds, err = r.prepareStorages(context.Background(), deployrunPreviewUUID, "")
	if err != nil {
		t.Fatalf("preview prepareStorages: %v", err)
	}
	if len(binds) != 1 || binds[0] != deployrunPreviewUUID+"_data:/data" {
		t.Fatalf("preview binds = %v", binds)
	}

	// Failures: listing, volume creation, bind directory creation.
	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		if strings.Contains(sql, "-- name: ListStoragesForResource ") {
			return nil, errors.New("db down"), true
		}
		return nil, nil, false
	}
	r.preview = nil
	if _, err := r.prepareStorages(context.Background(), jobFixtureUUID, ""); err == nil {
		t.Fatal("list failure must propagate")
	}

	env.db.rowsFor = storageRows(map[int]any{3: store.StorageKindVolume, 4: "data", 6: "/data"})
	env.rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
		return volumetypes.Volume{}, errors.New("daemon down")
	}
	if _, err := r.prepareStorages(context.Background(), jobFixtureUUID, ""); err == nil {
		t.Fatal("volume failure must propagate")
	}

	failingSSH := deployrunNewSSHServer(t, func(command string) (string, uint32) {
		if strings.Contains(command, "mkdir -p /srv/files") {
			return "", 1
		}
		return jobCommandOutput(command), 0
	})
	host, port := failingSSH.address(t)
	server := deployrunServer(env)
	server.Host, server.Port = host, int32(port)
	failClient, err := serverdial.Open(context.Background(), env.q, env.h.Keyring, server)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = failClient.Close() })
	env.db.rowsFor = storageRows(map[int]any{3: store.StorageKindBind, 5: "/srv/files", 6: "/files"})
	r.client = failClient
	if _, err := r.prepareStorages(context.Background(), jobFixtureUUID, ""); err == nil ||
		!strings.Contains(err.Error(), "bind-mount directories failed") {
		t.Fatalf("error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Environment rendering (§5.2, §5.4, ADR-057)
// ---------------------------------------------------------------------------

func TestDeployrunRenderBuildEnv(t *testing.T) {
	env := deployrunSetup(t, nil)
	plainBlob := deployrunEncrypt(t, env, "environment_variables", "value_enc", "plain-value")
	secretBlob := deployrunEncrypt(t, env, "environment_variables", "value_enc", "secret-value")
	literalBlob := deployrunEncrypt(t, env, "environment_variables", "value_enc", "{{team.RAW}}")
	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		if strings.Contains(sql, "-- name: ListEnvVarsForDeploy ") {
			return &deployrunRows{rows: []map[int]any{
				{3: "PLAIN", 4: plainBlob, 5: false, 6: true, 7: false},
				{3: "SECRET", 4: secretBlob, 5: true, 6: true, 7: false},
				{3: "LITERAL", 4: literalBlob, 5: false, 6: true, 7: true},
				{3: "RUNTIME_ONLY", 4: plainBlob, 5: false, 6: false, 7: false},
			}, blob: env.inner.blob}, nil, true
		}
		return nil, nil, false
	}
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	script, inputs, err := r.renderBuildEnv(context.Background())
	if err != nil {
		t.Fatalf("renderBuildEnv: %v", err)
	}
	if !strings.Contains(script, "PLAIN='plain-value'") || !strings.Contains(script, "LITERAL='{{team.RAW}}'") {
		t.Fatalf("script = %q", script)
	}
	if strings.Contains(script, "RUNTIME_ONLY") {
		t.Fatal("a runtime-only variable must not enter build.env")
	}
	if inputs.argValues["PLAIN"] != "plain-value" || string(inputs.secretValues["SECRET"]) != "secret-value" {
		t.Fatalf("inputs = %#v", inputs)
	}

	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		if strings.Contains(sql, "-- name: ListEnvVarsForDeploy ") {
			return &deployrunRows{rows: []map[int]any{{3: "BROKEN", 4: []byte("junk"), 6: true}}, blob: env.inner.blob}, nil, true
		}
		return nil, nil, false
	}
	if _, _, err := r.renderBuildEnv(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "decrypt variable BROKEN") {
		t.Fatalf("error = %v", err)
	}

	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		if strings.Contains(sql, "-- name: ListEnvVarsForDeploy ") {
			return nil, errors.New("db down"), true
		}
		return nil, nil, false
	}
	if _, _, err := r.renderBuildEnv(context.Background()); err == nil {
		t.Fatal("list failure must propagate")
	}

	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		if strings.Contains(sql, "SharedVariables") {
			return nil, errors.New("shared down"), true
		}
		return nil, nil, false
	}
	if _, _, err := r.renderBuildEnv(context.Background()); err == nil {
		t.Fatal("shared resolution failure must propagate")
	}
}

func TestDeployrunRenderRuntimeEnv(t *testing.T) {
	env := deployrunSetup(t, nil)
	valueBlob := deployrunEncrypt(t, env, "environment_variables", "value_enc", "runtime-value")
	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		switch {
		case strings.Contains(sql, "-- name: ListEnvVarsForDeploy "), strings.Contains(sql, "-- name: ListPreviewEnvVars "):
			return &deployrunRows{rows: []map[int]any{{3: "KEY", 4: valueBlob}}, blob: env.inner.blob}, nil, true
		case strings.Contains(sql, "-- name: ListDomainsForApplication "):
			return &jobFlowRows{remaining: 1, blob: env.inner.blob}, nil, true
		}
		return nil, nil, false
	}
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	entries, err := r.renderRuntimeEnv(context.Background())
	if err != nil {
		t.Fatalf("renderRuntimeEnv: %v", err)
	}
	joined := strings.Join(entries, "\n")
	if !strings.Contains(joined, "KEY=runtime-value") ||
		!strings.Contains(joined, "AKERDOCK_FQDN=unit") ||
		!strings.Contains(joined, "AKERDOCK_URL=https://unit") {
		t.Fatalf("entries = %v", entries)
	}

	// A preview reads its dedicated set and gets AKERDOCK_PR_ID.
	fqdn := "pr-4.example.test"
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, deployrunPreviewUUID), PrID: 4, Fqdn: &fqdn}
	entries, err = r.renderRuntimeEnv(context.Background())
	if err != nil {
		t.Fatalf("preview renderRuntimeEnv: %v", err)
	}
	joined = strings.Join(entries, "\n")
	if !strings.Contains(joined, "AKERDOCK_PR_ID=4") || !strings.Contains(joined, "AKERDOCK_FQDN="+fqdn) {
		t.Fatalf("preview entries = %v", entries)
	}

	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		if strings.Contains(sql, "EnvVars") {
			return &deployrunRows{rows: []map[int]any{{3: "BROKEN", 4: []byte("junk")}}, blob: env.inner.blob}, nil, true
		}
		return nil, nil, false
	}
	r.preview = nil
	if _, err := r.renderRuntimeEnv(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "decrypt variable BROKEN") {
		t.Fatalf("error = %v", err)
	}

	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		if strings.Contains(sql, "EnvVars") {
			return nil, errors.New("db down"), true
		}
		return nil, nil, false
	}
	if _, err := r.renderRuntimeEnv(context.Background()); err == nil {
		t.Fatal("list failure must propagate")
	}
}

func TestDeployrunDeploymentRefsAndMerge(t *testing.T) {
	env := deployrunSetup(t, nil)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))

	// Preview without an FQDN: pr_id only.
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, deployrunPreviewUUID), PrID: 12}
	refs := r.deploymentRefs(context.Background())
	if refs["deployment.pr_id"] != "12" || refs["deployment.fqdn"] != "" {
		t.Fatalf("refs = %v", refs)
	}

	shared := sharedEnv{}
	r.mergeDeploymentRefs(&shared, nil)
	if shared.refs != nil {
		t.Fatal("empty refs must not allocate")
	}
	r.mergeDeploymentRefs(&shared, refs)
	if shared.refs["deployment.pr_id"] != "12" {
		t.Fatalf("merged refs = %v", shared.refs)
	}
}

// ---------------------------------------------------------------------------
// Health config and candidate verdicts (§4, §5.3.4)
// ---------------------------------------------------------------------------

func TestDeployrunHealthConfig(t *testing.T) {
	env := deployrunSetup(t, nil)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))

	if hc, ok := r.healthConfig(context.Background()); ok || hc != nil {
		t.Fatal("a disabled health check must answer none")
	}

	env.db.rowFor = func(sql string) pgx.Row {
		if strings.Contains(sql, "-- name: GetHealthCheck ") {
			return deployrunRow{overrides: map[int]any{2: true, 5: int32(9000)}, blob: env.inner.blob}
		}
		return nil
	}
	hc, ok := r.healthConfig(context.Background())
	if !ok || hc == nil || !strings.Contains(hc.Test[1], ":9000") {
		t.Fatalf("explicit port config = %#v", hc)
	}
	if r.healthBudget != 33 {
		t.Fatalf("health budget = %d", r.healthBudget)
	}

	env.db.rowFor = func(sql string) pgx.Row {
		if strings.Contains(sql, "-- name: GetHealthCheck ") {
			return deployrunRow{overrides: map[int]any{2: true}, blob: env.inner.blob}
		}
		return nil
	}
	r.app.RuntimeConfig.PortsExposes = ptr("7070,8081")
	hc, ok = r.healthConfig(context.Background())
	if !ok || !strings.Contains(hc.Test[1], ":7070") {
		t.Fatalf("ports_exposes config = %#v", hc)
	}
}

func TestDeployrunCandidateFailureAttachesLogs(t *testing.T) {
	env := deployrunSetup(t, nil)
	var framed bytes.Buffer
	_, _ = stdcopy.NewStdWriter(&framed, stdcopy.Stderr).Write([]byte("panic: boom\n"))
	env.rt.ContainerLogsFn = func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(framed.Bytes())), nil
	}
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	r.rt = env.rt
	err := r.candidateFailure(context.Background(), "c", "did not become healthy")
	if err == nil || !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("error = %v", err)
	}

	env.rt.ContainerLogsFn = func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
		return nil, errors.New("logs unavailable")
	}
	err = r.candidateFailure(context.Background(), "c", "did not become healthy")
	if err == nil || err.Error() != "did not become healthy" {
		t.Fatalf("error without logs = %v", err)
	}
}

func TestDeployrunContainerStateEdgeCases(t *testing.T) {
	env := deployrunSetup(t, nil)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	r.rt = env.rt

	env.rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{}}, nil
	}
	if st, err := r.containerState(context.Background(), "c"); err != nil || st != "absent" {
		t.Fatalf("nil state = %q, %v", st, err)
	}

	env.rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{}, errors.New("daemon down")
	}
	if _, err := r.containerState(context.Background(), "c"); err == nil {
		t.Fatal("inspect failure must propagate")
	}
}

func TestDeployrunAwaitCandidateEdgeCases(t *testing.T) {
	env := deployrunSetup(t, nil)

	t.Run("cancelled during the stable wait", func(t *testing.T) {
		old := deploymentStablePeriod
		deploymentStablePeriod = time.Hour
		t.Cleanup(func() { deploymentStablePeriod = old })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
		r.rt = env.rt
		if _, err := r.awaitCandidate(ctx, "c", false, func(string) {}); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("cancelled during the health poll", func(t *testing.T) {
		old := deploymentHealthPoll
		deploymentHealthPoll = time.Hour
		t.Cleanup(func() { deploymentHealthPoll = old })
		rt := deployrunRuntime()
		rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return deployrunInspect("running", "starting", ""), nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
		r.rt, r.healthBudget = rt, 3600
		if _, err := r.awaitCandidate(ctx, "c", true, func(string) {}); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("budget exhausted is a timeout verdict", func(t *testing.T) {
		r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
		r.rt, r.healthBudget = env.rt, 0
		var out strings.Builder
		ok, err := r.awaitCandidate(context.Background(), "c", true, func(s string) { out.WriteString(s) })
		if err != nil || ok || !strings.Contains(out.String(), "timeout") {
			t.Fatalf("verdict = %v, %v, %q", ok, err, out.String())
		}
	})

	t.Run("stable wait inspect failure", func(t *testing.T) {
		old := deploymentStablePeriod
		deploymentStablePeriod = time.Millisecond
		t.Cleanup(func() { deploymentStablePeriod = old })
		rt := deployrunRuntime()
		rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return containertypes.InspectResponse{}, errors.New("daemon down")
		}
		rt.ContainerLogsFn = func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
			return nil, errors.New("no logs") // also covers the follower's error return
		}
		r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
		r.rt = rt
		if _, err := r.awaitCandidate(context.Background(), "c", false, func(string) {}); err == nil {
			t.Fatal("inspect failure must propagate")
		}
	})
}

// ---------------------------------------------------------------------------
// Steps: live streaming, failure records (§12)
// ---------------------------------------------------------------------------

func TestDeployrunStreamStepPublishesWhileRunning(t *testing.T) {
	env := deployrunSetup(t, nil)
	flushed := make(chan struct{})
	var once sync.Once
	env.db.note = func(sql string) {
		env.log.add(sql)
		if strings.Contains(sql, "-- name: SetDeploymentStepLog ") {
			once.Do(func() { close(flushed) })
		}
	}
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	err := r.streamStep(context.Background(), "live", func(onOutput func(string)) (*sshexec.Result, error) {
		onOutput("line one\nline two\n")
		select {
		case <-flushed:
		case <-time.After(10 * time.Second):
			t.Error("the live flush never published")
		}
		onOutput("tail\n")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("streamStep: %v", err)
	}
	if env.log.count("-- name: SetDeploymentStepLog ") == 0 {
		t.Fatal("the transcript must be published while the command runs")
	}
}

func TestDeployrunStepFailureShapes(t *testing.T) {
	env := deployrunSetup(t, nil)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))

	// A step whose command exits non-zero without an error gets one built
	// from the exit code and stderr.
	err := r.step(context.Background(), "fails", func() (*sshexec.Result, error) {
		return &sshexec.Result{ExitCode: 3, Stderr: "boom\nsecond line"}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "exit code 3: boom") {
		t.Fatalf("error = %v", err)
	}
	err = r.streamStep(context.Background(), "fails-too", func(func(string)) (*sshexec.Result, error) {
		return &sshexec.Result{ExitCode: 4, Stdout: "partial output", Stderr: "bad"}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "exit code 4") {
		t.Fatalf("stream error = %v", err)
	}

	// A failing CreateDeploymentStep aborts both step flavors and skipStep.
	env.db.rowFor = func(sql string) pgx.Row {
		if strings.Contains(sql, "-- name: CreateDeploymentStep ") {
			return deployrunRow{err: errors.New("insert failed")}
		}
		return nil
	}
	if err := r.step(context.Background(), "s", func() (*sshexec.Result, error) { return nil, nil }); err == nil {
		t.Fatal("step must surface the insert failure")
	}
	if err := r.streamStep(context.Background(), "s", func(func(string)) (*sshexec.Result, error) { return nil, nil }); err == nil {
		t.Fatal("streamStep must surface the insert failure")
	}
	r.skipStep(context.Background(), "s", "reason") // must not panic
}

// ---------------------------------------------------------------------------
// Deploy keys and GitHub App tokens (§5.1, protocols §2.2.4)
// ---------------------------------------------------------------------------

func TestDeployrunInstallDeployKey(t *testing.T) {
	env := deployrunSetup(t, nil)
	dial := func(t *testing.T, respond func(string) (string, uint32)) *sshexec.Client {
		t.Helper()
		server := deployrunServer(env)
		if respond != nil {
			failing := deployrunNewSSHServer(t, respond)
			host, port := failing.address(t)
			server.Host, server.Port = host, int32(port)
		}
		client, err := serverdial.Open(context.Background(), env.q, env.h.Keyring, server)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}

	t.Run("no git source", func(t *testing.T) {
		r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
		gitEnv, cleanup, err := r.installDeployKey(context.Background(), "/apps/x")
		if err != nil || gitEnv != "" {
			t.Fatalf("gitEnv = %q, %v", gitEnv, err)
		}
		cleanup()
	})

	t.Run("source vanished", func(t *testing.T) {
		env.db.rowFor = func(sql string) pgx.Row {
			if strings.Contains(sql, "-- name: GetGitSourceByID ") {
				return deployrunRow{err: errors.New("gone")}
			}
			return nil
		}
		app := deployrunApp(t)
		app.Application.GitSourceID = ptrOf(int64(1))
		r := deployrunNewRun(env, deployrunDeployment(t), app)
		if _, _, err := r.installDeployKey(context.Background(), "/apps/x"); err == nil {
			t.Fatal("a vanished source must fail")
		}
	})

	t.Run("public source without key", func(t *testing.T) {
		env.db.rowFor = nil // defaults: PrivateKeyID and GithubAppID both NULL
		app := deployrunApp(t)
		app.Application.GitSourceID = ptrOf(int64(1))
		r := deployrunNewRun(env, deployrunDeployment(t), app)
		gitEnv, cleanup, err := r.installDeployKey(context.Background(), "/apps/x")
		if err != nil || gitEnv != "" {
			t.Fatalf("gitEnv = %q, %v", gitEnv, err)
		}
		cleanup()
	})

	t.Run("deploy key installed and removed", func(t *testing.T) {
		env.db.rowFor = func(sql string) pgx.Row {
			if strings.Contains(sql, "-- name: GetGitSourceByID ") {
				return deployrunRow{overrides: map[int]any{8: int64(1)}, blob: env.inner.blob}
			}
			return nil
		}
		app := deployrunApp(t)
		app.Application.GitSourceID = ptrOf(int64(1))
		r := deployrunNewRun(env, deployrunDeployment(t), app)
		r.client = dial(t, nil)
		gitEnv, cleanup, err := r.installDeployKey(context.Background(), "/apps/x")
		if err != nil || !strings.Contains(gitEnv, "GIT_SSH_COMMAND") || !strings.Contains(gitEnv, "IdentitiesOnly=yes") {
			t.Fatalf("gitEnv = %q, %v", gitEnv, err)
		}
		cleanup()
	})

	t.Run("key vanished", func(t *testing.T) {
		env.db.rowFor = func(sql string) pgx.Row {
			switch {
			case strings.Contains(sql, "-- name: GetGitSourceByID "):
				return deployrunRow{overrides: map[int]any{8: int64(1)}, blob: env.inner.blob}
			case strings.Contains(sql, "-- name: GetPrivateKeyByID "):
				return deployrunRow{err: errors.New("gone")}
			}
			return nil
		}
		app := deployrunApp(t)
		app.Application.GitSourceID = ptrOf(int64(1))
		r := deployrunNewRun(env, deployrunDeployment(t), app)
		if _, _, err := r.installDeployKey(context.Background(), "/apps/x"); err == nil ||
			!strings.Contains(err.Error(), "deploy key vanished") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("upload failure", func(t *testing.T) {
		env.db.rowFor = func(sql string) pgx.Row {
			if strings.Contains(sql, "-- name: GetGitSourceByID ") {
				return deployrunRow{overrides: map[int]any{8: int64(1)}, blob: env.inner.blob}
			}
			return nil
		}
		app := deployrunApp(t)
		app.Application.GitSourceID = ptrOf(int64(1))
		r := deployrunNewRun(env, deployrunDeployment(t), app)
		r.client = dial(t, func(command string) (string, uint32) {
			if strings.Contains(command, "keys") {
				return "", 1
			}
			return jobCommandOutput(command), 0
		})
		if _, _, err := r.installDeployKey(context.Background(), "/apps/x"); err == nil ||
			!strings.Contains(err.Error(), "installing the deploy key failed") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestDeployrunInstallGithubToken(t *testing.T) {
	env := deployrunSetup(t, nil)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})
	appKeyBlob := deployrunEncrypt(t, env, "github_apps", "app_private_key_enc", string(rsaPEM))
	notPEMBlob := deployrunEncrypt(t, env, "github_apps", "app_private_key_enc", "not a pem")

	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/app/installations/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_unit_token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	t.Cleanup(forge.Close)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	appRow := func(blob []byte, apiURL string) map[int]any {
		return map[int]any{4: int64(101), 6: int64(202), 10: blob, 11: apiURL}
	}
	steer := func(githubApp map[int]any, repoName string) {
		env.db.rowFor = func(sql string) pgx.Row {
			switch {
			case strings.Contains(sql, "-- name: GetGithubAppByID "):
				if githubApp == nil {
					return deployrunRow{err: errors.New("app gone")}
				}
				return deployrunRow{overrides: githubApp, blob: env.inner.blob}
			case strings.Contains(sql, "-- name: GetRepositoryByID ") && repoName != "":
				return deployrunRow{overrides: map[int]any{4: repoName}, blob: env.inner.blob}
			}
			return nil
		}
	}
	newRun := func(t *testing.T, sshRespond func(string) (string, uint32)) *deploymentRun {
		t.Helper()
		app := deployrunApp(t)
		app.Application.GitSourceID = ptrOf(int64(1))
		app.Application.RepositoryID = ptrOf(int64(1))
		r := deployrunNewRun(env, deployrunDeployment(t), app)
		server := deployrunServer(env)
		if sshRespond != nil {
			failing := deployrunNewSSHServer(t, sshRespond)
			host, port := failing.address(t)
			server.Host, server.Port = host, int32(port)
		}
		client, err := serverdial.Open(context.Background(), env.q, env.h.Keyring, server)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		r.client = client
		return r
	}

	t.Run("app vanished", func(t *testing.T) {
		steer(nil, "")
		r := newRun(t, nil)
		if _, _, err := r.installGithubToken(context.Background(), "/apps/x", 1); err == nil ||
			!strings.Contains(err.Error(), "github app vanished") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("not installed", func(t *testing.T) {
		env.db.rowFor = func(sql string) pgx.Row {
			if strings.Contains(sql, "-- name: GetGithubAppByID ") {
				return deployrunRow{blob: env.inner.blob} // pointers stay NULL
			}
			return nil
		}
		r := newRun(t, nil)
		if _, _, err := r.installGithubToken(context.Background(), "/apps/x", 1); err == nil ||
			!strings.Contains(err.Error(), "not installed") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("undecryptable key", func(t *testing.T) {
		steer(appRow([]byte("junk"), forge.URL), "")
		r := newRun(t, nil)
		if _, _, err := r.installGithubToken(context.Background(), "/apps/x", 1); err == nil {
			t.Fatal("decrypt failure must propagate")
		}
	})

	t.Run("key is not a PEM", func(t *testing.T) {
		steer(appRow(notPEMBlob, forge.URL), "")
		r := newRun(t, nil)
		if _, _, err := r.installGithubToken(context.Background(), "/apps/x", 1); err == nil ||
			!strings.Contains(err.Error(), "PEM") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("token endpoint failure", func(t *testing.T) {
		steer(appRow(appKeyBlob, broken.URL), "")
		r := newRun(t, nil)
		if _, _, err := r.installGithubToken(context.Background(), "/apps/x", 1); err == nil ||
			!strings.Contains(err.Error(), "github installation token") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("token minted and helper installed", func(t *testing.T) {
		steer(appRow(appKeyBlob, forge.URL), "acme/app")
		r := newRun(t, nil)
		gitEnv, cleanup, err := r.installGithubToken(context.Background(), "/apps/x", 1)
		if err != nil || !strings.Contains(gitEnv, "GIT_ASKPASS=") {
			t.Fatalf("gitEnv = %q, %v", gitEnv, err)
		}
		cleanup()
	})

	t.Run("helper upload failure", func(t *testing.T) {
		steer(appRow(appKeyBlob, forge.URL), "")
		r := newRun(t, func(command string) (string, uint32) {
			if strings.Contains(command, "git_askpass") {
				return "", 1
			}
			return jobCommandOutput(command), 0
		})
		if _, _, err := r.installGithubToken(context.Background(), "/apps/x", 1); err == nil ||
			!strings.Contains(err.Error(), "credential helper failed") {
			t.Fatalf("error = %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Retention, artifacts, small helpers
// ---------------------------------------------------------------------------

func TestDeployrunPruneOldImagesPreviewAndFailures(t *testing.T) {
	env := deployrunSetup(t, nil)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))
	r.rt = env.rt
	r.preview = &store.Preview{ID: 1, Uuid: mustUUID(t, deployrunPreviewUUID)}

	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		if strings.Contains(sql, "-- name: ListPreviewArtifactsOnServer ") {
			return &deployrunRows{rows: []map[int]any{
				{0: int64(1), 1: "akerdock/preview", 2: "new"},
				{0: int64(2), 1: "akerdock/preview", 2: "old"},
			}, blob: env.inner.blob}, nil, true
		}
		return nil, nil, false
	}
	r.pruneOldImages(context.Background())
	if env.log.count("-- name: DeleteDeploymentArtifact ") != 1 {
		t.Fatal("the older preview artifact must be dropped")
	}

	env.db.rowsFor = func(sql string) (pgx.Rows, error, bool) {
		if strings.Contains(sql, "ArtifactsOnServer") {
			return nil, errors.New("db down"), true
		}
		return nil, nil, false
	}
	r.pruneOldImages(context.Background()) // preview list failure: logged, never fatal
	r.preview = nil
	r.pruneOldImages(context.Background()) // app list failure: logged, never fatal

	// A reusing deployment reclaims nothing.
	r.d.SkipBuild = true
	before := env.log.count("-- name: GetInstanceSettings ")
	r.pruneOldImages(context.Background())
	if env.log.count("-- name: GetInstanceSettings ") != before {
		t.Fatal("a skip_build deployment must not consult retention at all")
	}
}

func TestDeployrunRecordArtifactShapes(t *testing.T) {
	env := deployrunSetup(t, nil)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))

	// No image name: nothing to record.
	r.recordArtifact(context.Background())
	if env.log.count("-- name: CreateDeploymentArtifact ") != 0 {
		t.Fatal("no artifact without an image name")
	}

	r.d.ImageName, r.d.ImageTag = ptr("akerdock/app"), ptr("sha12")
	r.digest = "registry.example/app@sha256:x"
	r.recordArtifact(context.Background())
	if env.log.count("-- name: CreateDeploymentArtifact ") != 1 {
		t.Fatal("a fresh artifact must be recorded")
	}

	// Persistence failure is a warning, never an error.
	env.db.execErr = func(sql string) error {
		if strings.Contains(sql, "-- name: CreateDeploymentArtifact ") {
			return errors.New("insert failed")
		}
		return nil
	}
	r.recordArtifact(context.Background())
}

func TestDeployrunSmallAccessors(t *testing.T) {
	env := deployrunSetup(t, nil)
	r := deployrunNewRun(env, deployrunDeployment(t), deployrunApp(t))

	// Without a builder, everything answers the target-side handles.
	r.rt, r.hops = env.rt, env.ops
	if r.bc() != nil {
		t.Fatal("bc without clients must be nil")
	}
	if !reflect.DeepEqual(r.bcrt(), env.rt) || !reflect.DeepEqual(r.bhostops(), env.ops) {
		t.Fatal("build accessors must fall back to the target")
	}
	r.pruneOldSources(context.Background(), "/apps/x") // nil bc: a no-op

	// With a builder, the build-side handles win.
	builderRT := deployrunRuntime()
	builderOps := &hostfake.Ops{}
	r.builder = &sshexec.Client{}
	r.brt, r.bhops = builderRT, builderOps
	if r.bc() != r.builder || !reflect.DeepEqual(r.bcrt(), builderRT) || !reflect.DeepEqual(r.bhostops(), builderOps) {
		t.Fatal("build accessors must answer the build server")
	}
}
