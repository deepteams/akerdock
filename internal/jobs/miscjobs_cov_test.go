// Coverage companions for the miscellaneous job handlers. Everything here
// reuses the in-package job-flow scaffolding (jobFlowDependencies and the
// protocol-level pgx fake) plus a thin steering wrapper, miscjobsDB, that adds
// per-query errors and post-scan column overrides. All identifiers are
// prefixed miscjobs — several coverage agents write into this package at once.
package jobs

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	imagetypes "github.com/docker/docker/api/types/image"
	networktypes "github.com/docker/docker/api/types/network"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/deepteams/akerdock/internal/agent"
	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/envelope"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/serverdial"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// miscjobsStdcopy frames a string as a stdcopy stdout stream, the way the
// daemon multiplexes non-TTY logs.
func miscjobsStdcopy(out string) string {
	var b strings.Builder
	_, _ = stdcopy.NewStdWriter(&b, stdcopy.Stdout).Write([]byte(out))
	return b.String()
}

// miscjobsOpenSSH dials the scripted loopback SSH server as a real client.
func miscjobsOpenSSH(ctx context.Context, q *store.Queries, keyring *envelope.Keyring, server store.Server) (*sshexec.Client, error) {
	return serverdial.Open(ctx, q, keyring, server)
}

// miscjobsDB layers steering on the shared jobFlowDB: per-query-name errors,
// post-scan column overrides, and replacement row sets for list queries.
type miscjobsDB struct {
	*jobFlowDB
	rowErr   func(sql string) error
	execErr  func(sql string) error
	override func(sql string, index int, dest any)
	rows     func(sql string) pgx.Rows
}

func (db *miscjobsDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if db.execErr != nil {
		if err := db.execErr(sql); err != nil {
			return pgconn.CommandTag{}, err
		}
	}
	return db.jobFlowDB.Exec(ctx, sql, args...)
}

func (db *miscjobsDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if db.rowErr != nil {
		if err := db.rowErr(sql); err != nil {
			return nil, err
		}
	}
	if db.rows != nil {
		if r := db.rows(sql); r != nil {
			return r, nil
		}
	}
	return db.jobFlowDB.Query(ctx, sql, args...)
}

func (db *miscjobsDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return miscjobsRow{inner: db.jobFlowDB.QueryRow(ctx, sql, args...), sql: sql, db: db}
}

type miscjobsRow struct {
	inner pgx.Row
	sql   string
	db    *miscjobsDB
}

func (r miscjobsRow) Scan(dest ...any) error {
	if r.db.rowErr != nil {
		if err := r.db.rowErr(r.sql); err != nil {
			return err
		}
	}
	if err := r.inner.Scan(dest...); err != nil {
		return err
	}
	if r.db.override != nil {
		for i, d := range dest {
			r.db.override(r.sql, i, d)
		}
	}
	return nil
}

// miscjobsListRows is jobFlowRows plus a per-row override hook.
type miscjobsListRows struct {
	jobFlowRows
	row      int
	override func(row, index int, dest any)
}

func (r *miscjobsListRows) Scan(dest ...any) error {
	if err := r.jobFlowRows.Scan(dest...); err != nil {
		return err
	}
	if r.override != nil {
		for i, d := range dest {
			r.override(r.row, i, d)
		}
	}
	r.row++
	return nil
}

// miscjobsDeps wraps jobFlowDependencies with the steering DB.
func miscjobsDeps(t *testing.T) (*store.Queries, *envelope.Keyring, *slog.Logger, *miscjobsDB) {
	t.Helper()
	_, keyring, _, logger, inner := jobFlowDependencies(t)
	db := &miscjobsDB{jobFlowDB: inner}
	return store.New(db), keyring, logger, db
}

// miscjobsFailOn answers err for any SQL whose name matches one fragment.
func miscjobsFailOn(err error, names ...string) func(string) error {
	return func(sql string) error {
		for _, n := range names {
			if strings.Contains(sql, "-- name: "+n+" ") {
				return err
			}
		}
		return nil
	}
}

// miscjobsEnum swaps one jobEnumValues fixture for the test's duration.
func miscjobsEnum(t *testing.T, name, value string) {
	t.Helper()
	previous, existed := jobEnumValues[name]
	jobEnumValues[name] = value
	t.Cleanup(func() {
		if existed {
			jobEnumValues[name] = previous
		} else {
			delete(jobEnumValues, name)
		}
	})
}

// miscjobsShortVerify shrinks the proxy verification budget so failing
// verifications conclude immediately.
func miscjobsShortVerify(t *testing.T) {
	t.Helper()
	previous := verifyTimeout
	verifyTimeout = 0
	t.Cleanup(func() { verifyTimeout = previous })
}

// --- agenttoken.go -----------------------------------------------------------

// miscjobsEnrollStore is a scriptable AgentEnrollmentStore.
type miscjobsEnrollStore struct {
	settings    store.InstanceSetting
	settingsErr error
	token       store.AgentToken
	tokenErr    error
	created     *store.CreateAgentTokenParams
	createErr   error
}

func (s *miscjobsEnrollStore) GetInstanceSettings(context.Context) (store.InstanceSetting, error) {
	return s.settings, s.settingsErr
}

func (s *miscjobsEnrollStore) GetAgentTokenByServerID(context.Context, int64) (store.AgentToken, error) {
	return s.token, s.tokenErr
}

func (s *miscjobsEnrollStore) CreateAgentToken(_ context.Context, p store.CreateAgentTokenParams) (store.AgentToken, error) {
	s.created = &p
	return store.AgentToken{}, s.createErr
}

func TestMiscjobsEnsureAgentTokenLifecycle(t *testing.T) {
	_, keyring, _, _, _ := jobFlowDependencies(t)
	ctx := context.Background()

	// First use: no row yet — a fresh token is minted, hashed and encrypted.
	q := &miscjobsEnrollStore{tokenErr: errors.New("no rows")}
	plain, err := EnsureAgentToken(ctx, q, keyring, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, agentTokenScheme) || q.created == nil {
		t.Fatalf("plain = %q, created = %#v", plain, q.created)
	}
	if q.created.ServerID != 7 || len(q.created.TokenHash) != 64 || len(q.created.TokenEnc) == 0 {
		t.Fatalf("create params = %#v", q.created)
	}

	// Second use: the SAME plaintext comes back from the ciphertext.
	q2 := &miscjobsEnrollStore{token: store.AgentToken{
		Uuid: mustUUID(t, q.created.Uuid.String()), TokenEnc: q.created.TokenEnc,
	}}
	again, err := EnsureAgentToken(ctx, q2, keyring, 7)
	if err != nil || again != plain {
		t.Fatalf("again = %q, %v (want %q)", again, err, plain)
	}

	// A row whose ciphertext cannot be decrypted is an error, never a rotation.
	q3 := &miscjobsEnrollStore{token: store.AgentToken{
		Uuid: mustUUID(t, jobFixtureUUID), TokenEnc: []byte("garbage"),
	}}
	if _, err := EnsureAgentToken(ctx, q3, keyring, 7); err == nil ||
		!strings.Contains(err.Error(), "agent token decrypt") {
		t.Fatalf("decrypt error = %v", err)
	}

	// A failing insert propagates.
	q4 := &miscjobsEnrollStore{tokenErr: errors.New("no rows"), createErr: errors.New("insert failed")}
	if _, err := EnsureAgentToken(ctx, q4, keyring, 7); err == nil ||
		!strings.Contains(err.Error(), "insert failed") {
		t.Fatalf("create error = %v", err)
	}
}

func TestMiscjobsAgentInstanceURL(t *testing.T) {
	ctx := context.Background()
	fqdn := "akerdock.example.test"
	empty := ""
	tests := []struct {
		name  string
		store *miscjobsEnrollStore
		srv   store.Server
		port  int
		url   string
		want  string
	}{
		{"explicit override wins", &miscjobsEnrollStore{}, store.Server{IsLocalhost: true}, 8080,
			"https://override.example.test", "https://override.example.test"},
		{"localhost gateway", &miscjobsEnrollStore{}, store.Server{IsLocalhost: true}, 9080,
			"", "http://host.docker.internal:9080"},
		{"instance fqdn", &miscjobsEnrollStore{settings: store.InstanceSetting{Fqdn: &fqdn}},
			store.Server{}, 0, "", "https://" + fqdn},
		{"no fqdn", &miscjobsEnrollStore{settings: store.InstanceSetting{Fqdn: &empty}},
			store.Server{}, 0, "", ""},
		{"settings error", &miscjobsEnrollStore{settingsErr: errors.New("down")},
			store.Server{}, 0, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentInstanceURL(ctx, tc.store, tc.srv, tc.port, tc.url); got != tc.want {
				t.Fatalf("url = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- agentprovision.go -------------------------------------------------------

func TestMiscjobsAgentEnvForServer(t *testing.T) {
	_, keyring, _, logger, _ := jobFlowDependencies(t)
	ctx := context.Background()
	srv := store.Server{ID: 3, IsLocalhost: true}

	// No reachable URL: waker-only, silently.
	if env := AgentEnvForServer(ctx, &miscjobsEnrollStore{}, keyring, logger, store.Server{}, 0, ""); env != (AgentEnv{}) {
		t.Fatalf("env without URL = %#v", env)
	}

	// Token minting failure degrades to waker-only, with and without a logger.
	failing := &miscjobsEnrollStore{tokenErr: errors.New("no rows"), createErr: errors.New("insert failed")}
	if env := AgentEnvForServer(ctx, failing, keyring, logger, srv, 9080, ""); env != (AgentEnv{}) {
		t.Fatalf("env with failing token = %#v", env)
	}
	if env := AgentEnvForServer(ctx, failing, keyring, nil, srv, 9080, ""); env != (AgentEnv{}) {
		t.Fatalf("env with nil logger = %#v", env)
	}

	// Nominal enrollment: URL plus a freshly minted token.
	ok := &miscjobsEnrollStore{tokenErr: errors.New("no rows")}
	env := AgentEnvForServer(ctx, ok, keyring, logger, srv, 9080, "")
	if env.InstanceURL != "http://host.docker.internal:9080" || !strings.HasPrefix(env.Token, agentTokenScheme) {
		t.Fatalf("env = %#v", env)
	}
}

func TestMiscjobsWakerRoutesFileRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg := agent.Config{
		Routes:    []agent.Route{{Host: "a.example.test", ResourceUUID: "keep", Container: "keep", Port: 80}},
		Resources: []agent.Resource{{UUID: "keep", Containers: []string{"keep"}}},
	}
	raw, err := agent.MarshalConfig(mergeWakerConfig(cfg, "gone", agent.Config{
		Routes:    []agent.Route{{Host: "b.example.test", ResourceUUID: "gone", Container: "gone", Port: 80}},
		Resources: []agent.Resource{{UUID: "gone"}},
	}))
	if err != nil {
		t.Fatal(err)
	}

	// readWakerConfig: absent, unreadable, invalid and valid tables.
	ops := &hostfake.Ops{}
	if got := readWakerConfig(ctx, ops); len(got.Routes) != 0 {
		t.Fatalf("absent table = %#v", got)
	}
	ops = &hostfake.Ops{ReadFileFn: func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error) {
		return agentwire.FileReadResult{}, errors.New("agent gone")
	}}
	if got := readWakerConfig(ctx, ops); len(got.Routes) != 0 {
		t.Fatalf("unreadable table = %#v", got)
	}
	ops = &hostfake.Ops{ReadFileFn: func(context.Context, agentwire.FileReadParams) (agentwire.FileReadResult, error) {
		return agentwire.FileReadResult{Found: true, Content: []byte("{corrupt")}, nil
	}}
	if got := readWakerConfig(ctx, ops); len(got.Routes) != 0 {
		t.Fatalf("corrupt table = %#v", got)
	}
	ops = &hostfake.Ops{ReadFileFn: func(_ context.Context, p agentwire.FileReadParams) (agentwire.FileReadResult, error) {
		if p.Path != wakerDir+"/"+agent.RoutesFile {
			t.Fatalf("read path = %q", p.Path)
		}
		return agentwire.FileReadResult{Found: true, Content: raw}, nil
	}}

	// removeWakerRoutes drops one resource and rewrites the table atomically.
	if err := removeWakerRoutes(ctx, ops, "gone"); err != nil {
		t.Fatal(err)
	}
	writes := ops.CallsTo(agentwire.MethodFileWrite)
	if len(writes) != 1 {
		t.Fatalf("writes = %v", writes)
	}
	w := writes[0].(agentwire.FileWriteParams)
	if !w.Atomic || w.Mode != 0o600 || !w.MakeDirs || strings.Contains(string(w.Content), "gone") ||
		!strings.Contains(string(w.Content), "keep") {
		t.Fatalf("deposit = %+v", w)
	}

	// depositWakerRoutes surfaces a write failure with its first line only.
	bad := &hostfake.Ops{WriteFileFn: func(context.Context, agentwire.FileWriteParams) error {
		return errors.New("disk full\nsecond line")
	}}
	if err := depositWakerRoutes(ctx, bad, cfg); err == nil ||
		!strings.Contains(err.Error(), "disk full") || strings.Contains(err.Error(), "second") {
		t.Fatalf("deposit error = %v", err)
	}
}

func TestMiscjobsEnsureAgentGuardsBeforeSSH(t *testing.T) {
	ctx := context.Background()
	// No image: a configuration error before anything is touched.
	if err := ensureAgent(ctx, nil, &hostfake.Ops{}, "net", "", "res", agent.Config{}, AgentEnv{}); err == nil ||
		!strings.Contains(err.Error(), "AKERDOCK_IMAGE") {
		t.Fatalf("missing image = %v", err)
	}
	// A failing routes deposit stops before the SSH deploy (client unused).
	bad := &hostfake.Ops{WriteFileFn: func(context.Context, agentwire.FileWriteParams) error {
		return errors.New("no space")
	}}
	if err := ensureAgent(ctx, nil, bad, "net", "img", "res", agent.Config{}, AgentEnv{}); err == nil ||
		!strings.Contains(err.Error(), "no space") {
		t.Fatalf("deposit failure = %v", err)
	}
}

func TestMiscjobsEnsureAgentDeploysOverSSH(t *testing.T) {
	q, keyring, _, db := miscjobsDeps(t)
	db.host, db.port = newJobSSHServer(t).address(t)
	ctx := context.Background()
	server, err := q.GetServerByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	client, err := miscjobsOpenSSH(ctx, q, keyring, server)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	ops := &hostfake.Ops{}
	if err := ensureAgent(ctx, client, ops, "akerdock-net", "akerdock:unit", "res",
		agent.Config{Routes: []agent.Route{{Host: "h", ResourceUUID: "res", Container: "c", Port: 80}}},
		AgentEnv{InstanceURL: "https://cp.example.test", Token: "akda_x"}); err != nil {
		t.Fatalf("ensureAgent = %v", err)
	}
	if len(ops.CallsTo(agentwire.MethodFileWrite)) != 1 {
		t.Fatal("the routing table was not merged before the deploy")
	}
}

// --- accesspolicy.go ---------------------------------------------------------

func TestMiscjobsResourceAccessPolicySSO(t *testing.T) {
	q, keyring, _, db := miscjobsDeps(t)
	ctx := context.Background()
	fqdn := "akerdock.example.test"
	db.override = func(sql string, index int, dest any) {
		if strings.Contains(sql, "-- name: GetInstanceSettings ") && index == 1 {
			value := fqdn
			*(dest.(**string)) = &value
		}
	}
	app := store.GetApplicationByIDRow{
		Resource:    store.Resource{Uuid: mustUUID(t, jobFixtureUUID)},
		Application: store.Application{AccessProtection: store.PreviewProtectionSso},
	}

	policy, err := resourceAccessPolicy(ctx, q, keyring, app, nil, store.Server{}, 9080)
	if err != nil || policy == nil || policy.Mode != "sso" ||
		!strings.HasPrefix(policy.ForwardAuthURL, "https://"+fqdn+"/webhooks/applications/forward-auth?resource=") ||
		policy.CallbackURL != "https://"+fqdn {
		t.Fatalf("policy = %#v, %v", policy, err)
	}

	// The localhost server routes the wall through the host gateway.
	policy, err = resourceAccessPolicy(ctx, q, keyring, app, nil, store.Server{IsLocalhost: true}, 9080)
	if err != nil || policy == nil || policy.CallbackURL != "http://host.docker.internal:9080" {
		t.Fatalf("localhost policy = %#v, %v", policy, err)
	}

	// Without an instance FQDN the sso wall fails closed.
	db.override = nil
	if _, err := resourceAccessPolicy(ctx, q, keyring, app, nil, store.Server{}, 0); err == nil ||
		!strings.Contains(err.Error(), "instance FQDN") {
		t.Fatalf("missing FQDN = %v", err)
	}

	// A failing settings read propagates.
	db.rowErr = miscjobsFailOn(errors.New("settings down"), "GetInstanceSettings")
	if _, err := resourceAccessPolicy(ctx, q, keyring, app, nil, store.Server{}, 0); err == nil ||
		!strings.Contains(err.Error(), "settings down") {
		t.Fatalf("settings error = %v", err)
	}

	// An unknown protection is refused, never rendered public.
	app.Application.AccessProtection = store.PreviewProtection("weird")
	if _, err := resourceAccessPolicy(ctx, q, keyring, app, nil, store.Server{}, 0); err == nil ||
		!strings.Contains(err.Error(), "unsupported access_protection") {
		t.Fatalf("unknown protection = %v", err)
	}
}

func TestMiscjobsResourceAccessPolicyServiceCredentials(t *testing.T) {
	_, keyring, _, _ := miscjobsDeps(t)
	ctx := context.Background()
	uuid := mustUUID(t, jobFixtureUUID)
	enc, err := keyring.Encrypt("services", "access_basic_auth_enc", jobFixtureUUID, []byte("admin:hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	app := store.GetApplicationByIDRow{Resource: store.Resource{Uuid: uuid}}
	service := &store.Service{AccessProtection: store.PreviewProtectionBasicAuth, AccessBasicAuthEnc: enc}
	policy, err := resourceAccessPolicy(ctx, nil, keyring, app, service, store.Server{}, 0)
	if err != nil || policy == nil || policy.Mode != "basic_auth" ||
		!strings.HasPrefix(policy.BasicAuthHash, "admin:") {
		t.Fatalf("service policy = %#v, %v", policy, err)
	}

	// Credentials that decrypt but are not user:password fail closed.
	bad, err := keyring.Encrypt("services", "access_basic_auth_enc", jobFixtureUUID, []byte("no-colon"))
	if err != nil {
		t.Fatal(err)
	}
	service.AccessBasicAuthEnc = bad
	if _, err := resourceAccessPolicy(ctx, nil, keyring, app, service, store.Server{}, 0); err == nil ||
		!strings.Contains(err.Error(), "user:password") {
		t.Fatalf("malformed credentials = %v", err)
	}
}

// --- sharedvars.go -----------------------------------------------------------

func TestMiscjobsResolveSharedEnv(t *testing.T) {
	q, keyring, _, db := miscjobsDeps(t)
	ctx := context.Background()
	enc, err := keyring.Encrypt("shared_variables", "value_enc", jobFixtureUUID, []byte("secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	db.rows = func(sql string) pgx.Rows {
		if !strings.Contains(sql, "-- name: ListSharedVariablesForResource ") {
			return nil
		}
		return &miscjobsListRows{
			jobFlowRows: jobFlowRows{remaining: 2, blob: enc},
			override: func(row, _ int, dest any) {
				if scope, ok := dest.(*store.SharedVariableScope); ok && row == 1 {
					*scope = store.SharedVariableScopeServer
				}
			},
		}
	}
	env, err := resolveSharedEnv(ctx, q, keyring, 1)
	if err != nil {
		t.Fatal(err)
	}
	if env.refs["team.unit"] != "secret-value" || env.server["unit"] != "secret-value" {
		t.Fatalf("env = %#v", env)
	}
	if got := env.interpolate("x={{team.unit}}"); got != "x=secret-value" {
		t.Fatalf("interpolate = %q", got)
	}

	// A ciphertext bound to another column refuses to decrypt.
	db.rows = func(sql string) pgx.Rows {
		if !strings.Contains(sql, "-- name: ListSharedVariablesForResource ") {
			return nil
		}
		return &jobFlowRows{remaining: 1, blob: []byte("garbage")}
	}
	if _, err := resolveSharedEnv(ctx, q, keyring, 1); err == nil ||
		!strings.Contains(err.Error(), "decrypt shared variable") {
		t.Fatalf("decrypt error = %v", err)
	}

	// The indexed query failing propagates.
	db.rows = nil
	db.rowErr = miscjobsFailOn(errors.New("query down"), "ListSharedVariablesForResource")
	if _, err := resolveSharedEnv(ctx, q, keyring, 1); err == nil ||
		!strings.Contains(err.Error(), "query down") {
		t.Fatalf("list error = %v", err)
	}
}

// --- dockersweep.go ----------------------------------------------------------

func miscjobsNotFound(what string) error {
	return fmt.Errorf("no such %s: %w", what, cerrdefs.ErrNotFound)
}

func TestMiscjobsSweepVolumes(t *testing.T) {
	ctx := context.Background()
	f := filters.NewArgs(filters.Arg("label", "akerdock.resource_uuid=abc"))

	rt := &fake.Runtime{}
	rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
		return volumetypes.ListResponse{Volumes: []*volumetypes.Volume{
			nil, {Name: "vanished"}, {Name: "kept"},
		}}, nil
	}
	rt.VolumeRemoveFn = func(_ context.Context, name string, force bool) error {
		if !force {
			t.Fatal("volume sweep must force-remove")
		}
		if name == "vanished" {
			return miscjobsNotFound("volume")
		}
		return nil
	}
	if err := sweepVolumes(ctx, rt, f); err != nil {
		t.Fatal(err)
	}

	rt.VolumeRemoveFn = func(context.Context, string, bool) error { return errors.New("in use") }
	if err := sweepVolumes(ctx, rt, f); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("remove error = %v", err)
	}
	rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
		return volumetypes.ListResponse{}, errors.New("daemon down")
	}
	if err := sweepVolumes(ctx, rt, f); err == nil || !strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("list error = %v", err)
	}
}

func TestMiscjobsSweepNetworks(t *testing.T) {
	ctx := context.Background()
	f := filters.NewArgs(filters.Arg("label", "akerdock.resource_uuid=abc"))

	rt := &fake.Runtime{}
	rt.NetworkListFn = func(context.Context, networktypes.ListOptions) ([]networktypes.Summary, error) {
		return []networktypes.Summary{{ID: "gone"}, {ID: "net2"}}, nil
	}
	rt.NetworkRemoveFn = func(_ context.Context, id string) error {
		if id == "gone" {
			return miscjobsNotFound("network")
		}
		return nil
	}
	if err := sweepNetworks(ctx, rt, f); err != nil {
		t.Fatal(err)
	}

	rt.NetworkRemoveFn = func(context.Context, string) error { return errors.New("has endpoints") }
	if err := sweepNetworks(ctx, rt, f); err == nil || !strings.Contains(err.Error(), "has endpoints") {
		t.Fatalf("remove error = %v", err)
	}
	rt.NetworkListFn = func(context.Context, networktypes.ListOptions) ([]networktypes.Summary, error) {
		return nil, errors.New("daemon down")
	}
	if err := sweepNetworks(ctx, rt, f); err == nil || !strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("list error = %v", err)
	}
}

func TestMiscjobsSweepImagesByReference(t *testing.T) {
	ctx := context.Background()
	rt := &fake.Runtime{}
	rt.ImageListFn = func(_ context.Context, opts imagetypes.ListOptions) ([]imagetypes.Summary, error) {
		if got := opts.Filters.Get("reference"); len(got) != 1 || got[0] != "registry.example/app" {
			t.Fatalf("reference filter = %v", got)
		}
		return []imagetypes.Summary{{ID: "gone"}, {ID: "img2"}}, nil
	}
	rt.ImageRemoveFn = func(_ context.Context, id string, opts imagetypes.RemoveOptions) ([]imagetypes.DeleteResponse, error) {
		if !opts.Force {
			t.Fatal("image sweep must force-remove")
		}
		if id == "gone" {
			return nil, miscjobsNotFound("image")
		}
		return nil, nil
	}
	if err := sweepImagesByReference(ctx, rt, "registry.example/app"); err != nil {
		t.Fatal(err)
	}

	rt.ImageRemoveFn = func(context.Context, string, imagetypes.RemoveOptions) ([]imagetypes.DeleteResponse, error) {
		return nil, errors.New("image in use")
	}
	if err := sweepImagesByReference(ctx, rt, "registry.example/app"); err == nil ||
		!strings.Contains(err.Error(), "image in use") {
		t.Fatalf("remove error = %v", err)
	}
	rt.ImageListFn = func(context.Context, imagetypes.ListOptions) ([]imagetypes.Summary, error) {
		return nil, errors.New("daemon down")
	}
	if err := sweepImagesByReference(ctx, rt, "registry.example/app"); err == nil ||
		!strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("list error = %v", err)
	}
}

func TestMiscjobsSweepContainersAndNames(t *testing.T) {
	ctx := context.Background()
	rt := &fake.Runtime{}
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return nil, errors.New("daemon down")
	}
	if err := sweepContainers(ctx, rt, filters.NewArgs(), false); err == nil ||
		!strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("list error = %v", err)
	}
	rt.ContainerRemoveFn = func(context.Context, string, containertypes.RemoveOptions) error {
		return errors.New("permission denied")
	}
	if err := removeNamedContainers(ctx, rt, false, "a"); err == nil ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("named remove error = %v", err)
	}
	if got := containerName(containertypes.Summary{}); got != "" {
		t.Fatalf("nameless summary = %q", got)
	}
	if got := containerName(containertypes.Summary{Names: []string{"/web"}}); got != "web" {
		t.Fatalf("summary name = %q", got)
	}
}

// --- dockerexec.go -----------------------------------------------------------

func TestMiscjobsEnsureNetworkAndVolume(t *testing.T) {
	ctx := context.Background()

	rt := &fake.Runtime{}
	rt.NetworkInspectFn = func(context.Context, string, networktypes.InspectOptions) (networktypes.Inspect, error) {
		return networktypes.Inspect{}, nil
	}
	if err := ensureNetwork(ctx, rt, "net"); err != nil {
		t.Fatalf("existing network = %v", err)
	}
	rt.NetworkInspectFn = func(context.Context, string, networktypes.InspectOptions) (networktypes.Inspect, error) {
		return networktypes.Inspect{}, miscjobsNotFound("network")
	}
	rt.NetworkCreateFn = func(_ context.Context, _ string, opts networktypes.CreateOptions) (networktypes.CreateResponse, error) {
		if opts.Labels["akerdock.managed"] != "true" {
			t.Fatalf("create labels = %v", opts.Labels)
		}
		return networktypes.CreateResponse{}, nil
	}
	if err := ensureNetwork(ctx, rt, "net"); err != nil {
		t.Fatalf("created network = %v", err)
	}
	rt.NetworkCreateFn = func(context.Context, string, networktypes.CreateOptions) (networktypes.CreateResponse, error) {
		return networktypes.CreateResponse{}, fmt.Errorf("already there: %w", cerrdefs.ErrConflict)
	}
	if err := ensureNetwork(ctx, rt, "net"); err != nil {
		t.Fatalf("concurrent create = %v", err)
	}
	rt.NetworkCreateFn = func(context.Context, string, networktypes.CreateOptions) (networktypes.CreateResponse, error) {
		return networktypes.CreateResponse{}, errors.New("create failed")
	}
	if err := ensureNetwork(ctx, rt, "net"); err == nil || !strings.Contains(err.Error(), "create failed") {
		t.Fatalf("create error = %v", err)
	}
	rt.NetworkInspectFn = func(context.Context, string, networktypes.InspectOptions) (networktypes.Inspect, error) {
		return networktypes.Inspect{}, errors.New("daemon down")
	}
	if err := ensureNetwork(ctx, rt, "net"); err == nil || !strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("inspect error = %v", err)
	}

	rt = &fake.Runtime{}
	rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
		return volumetypes.Volume{}, nil
	}
	if err := ensureVolume(ctx, rt, "vol", nil); err != nil {
		t.Fatalf("existing volume = %v", err)
	}
	rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
		return volumetypes.Volume{}, miscjobsNotFound("volume")
	}
	rt.VolumeCreateFn = func(_ context.Context, opts volumetypes.CreateOptions) (volumetypes.Volume, error) {
		if opts.Name != "vol" || opts.Labels["k"] != "v" {
			t.Fatalf("volume create = %+v", opts)
		}
		return volumetypes.Volume{}, nil
	}
	if err := ensureVolume(ctx, rt, "vol", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("created volume = %v", err)
	}
	rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
		return volumetypes.Volume{}, errors.New("daemon down")
	}
	if err := ensureVolume(ctx, rt, "vol", nil); err == nil || !strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("inspect error = %v", err)
	}
}

// miscjobsOneShotRuntime scripts the create/start/wait/logs cycle of a
// one-shot container.
func miscjobsOneShotRuntime(status int64, logs string) *fake.Runtime {
	rt := &fake.Runtime{}
	rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispec.Platform, string) (containertypes.CreateResponse, error) {
		return containertypes.CreateResponse{ID: "probe"}, nil
	}
	rt.ContainerWaitFn = func(context.Context, string, containertypes.WaitCondition) (<-chan containertypes.WaitResponse, <-chan error) {
		waitCh := make(chan containertypes.WaitResponse, 1)
		waitCh <- containertypes.WaitResponse{StatusCode: status}
		return waitCh, make(chan error, 1)
	}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Status: "exited"},
		}}, nil
	}
	rt.ContainerLogsFn = func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(miscjobsStdcopy(logs))), nil
	}
	return rt
}

func TestMiscjobsOneShotCaptureAndFailures(t *testing.T) {
	ctx := context.Background()

	out, err := runOneShotCapture(ctx, miscjobsOneShotRuntime(0, "70\n"), &containertypes.Config{Image: "postgres"}, nil)
	if err != nil || strings.TrimSpace(out) != "70" {
		t.Fatalf("capture = %q, %v", out, err)
	}

	// A non-zero exit reports with the container's first output line attached.
	_, err = runOneShotCapture(ctx, miscjobsOneShotRuntime(3, "boom happened\nmore\n"), &containertypes.Config{}, nil)
	if err == nil || !strings.Contains(err.Error(), "exited with code 3") || !strings.Contains(err.Error(), "boom happened") {
		t.Fatalf("failure detail = %v", err)
	}

	// The wait error channel surfaces.
	rt := miscjobsOneShotRuntime(0, "")
	rt.ContainerWaitFn = func(context.Context, string, containertypes.WaitCondition) (<-chan containertypes.WaitResponse, <-chan error) {
		errCh := make(chan error, 1)
		errCh <- errors.New("wait broken")
		return make(chan containertypes.WaitResponse, 1), errCh
	}
	if err := runOneShot(ctx, rt, &containertypes.Config{}, nil); err == nil ||
		!strings.Contains(err.Error(), "wait broken") {
		t.Fatalf("wait error = %v", err)
	}

	// A canceled context wins over a wait that never resolves.
	rt = miscjobsOneShotRuntime(0, "")
	rt.ContainerWaitFn = func(context.Context, string, containertypes.WaitCondition) (<-chan containertypes.WaitResponse, <-chan error) {
		return make(chan containertypes.WaitResponse), make(chan error)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := runOneShot(canceled, rt, &containertypes.Config{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait = %v", err)
	}

	// Create and start failures report before any wait.
	rt = miscjobsOneShotRuntime(0, "")
	rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispec.Platform, string) (containertypes.CreateResponse, error) {
		return containertypes.CreateResponse{}, errors.New("create refused")
	}
	if err := runOneShot(ctx, rt, &containertypes.Config{}, nil); err == nil ||
		!strings.Contains(err.Error(), "create refused") {
		t.Fatalf("create error = %v", err)
	}
	rt = miscjobsOneShotRuntime(0, "")
	rt.ContainerStartFn = func(context.Context, string, containertypes.StartOptions) error {
		return errors.New("start refused")
	}
	if err := runOneShot(ctx, rt, &containertypes.Config{}, nil); err == nil ||
		!strings.Contains(err.Error(), "start refused") {
		t.Fatalf("start error = %v", err)
	}
}

func TestMiscjobsContainerLogsTailErrors(t *testing.T) {
	ctx := context.Background()
	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{}, errors.New("no such container")
	}
	if _, err := containerLogsTail(ctx, rt, "c", 10); err == nil ||
		!strings.Contains(err.Error(), "no such container") {
		t.Fatalf("inspect error = %v", err)
	}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{Config: &containertypes.Config{Tty: true}}, nil
	}
	rt.ContainerLogsFn = func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
		return nil, errors.New("logs refused")
	}
	if _, err := containerLogsTail(ctx, rt, "c", 10); err == nil ||
		!strings.Contains(err.Error(), "logs refused") {
		t.Fatalf("logs error = %v", err)
	}
	// A TTY container streams raw output, no stdcopy framing.
	rt.ContainerLogsFn = func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("raw line\n")), nil
	}
	out, err := containerLogsTail(ctx, rt, "c", 10)
	if err != nil || out != "raw line\n" {
		t.Fatalf("tty logs = %q, %v", out, err)
	}
}

func TestMiscjobsStreamPullProgressErrors(t *testing.T) {
	var lines []string
	err := streamPullProgress(strings.NewReader(
		`{"status":"Pulling","id":"a"}`+"\n"+`{"error":"manifest unknown"}`+"\n"), func(s string) {
		lines = append(lines, s)
	})
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("stream error = %v", err)
	}
	if len(lines) != 1 || lines[0] != "a: Pulling\n" {
		t.Fatalf("lines = %v", lines)
	}
	if err := streamPullProgress(strings.NewReader("{not json"), func(string) {}); err == nil {
		t.Fatal("a corrupt stream must surface")
	}
}

// --- proxyapply.go -----------------------------------------------------------

func TestMiscjobsProxyApplyRollbackVerdicts(t *testing.T) {
	miscjobsShortVerify(t)
	ctx := context.Background()

	// Verification fails, the previous applied revision is restored.
	q, _, _, db := miscjobsDeps(t)
	ops := &hostfake.Ops{}
	p := &ProxyApplier{Store: q, Docker: verifyRuntime(""), Host: ops, Server: store.Server{ID: 1}, Network: "net"}
	err := p.Apply(ctx, "app", "routing: v2", "")
	if err == nil || !strings.Contains(err.Error(), "routing rolled back to revision") {
		t.Fatalf("rollback verdict = %v", err)
	}
	writes := ops.CallsTo(agentwire.MethodFileWrite)
	if len(writes) != 2 || string(writes[1].(agentwire.FileWriteParams).Content) != "unit" {
		t.Fatalf("rollback writes = %v", writes)
	}

	// No previous revision: the faulty file is removed instead.
	db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetLastAppliedProxyRevision")
	ops2 := &hostfake.Ops{}
	p.Host = ops2
	err = p.Apply(ctx, "app", "routing: v2", "")
	if err == nil || !strings.Contains(err.Error(), "no previous revision exists") {
		t.Fatalf("first-apply verdict = %v", err)
	}
	if removes := ops2.CallsTo(agentwire.MethodFileRemove); len(removes) != 1 {
		t.Fatalf("faulty file removal = %v", removes)
	}

	// Apply AND rollback failing is a named routing anomaly (§6.4.4).
	db.rowErr = nil
	p.Host = &hostfake.Ops{WriteFileFn: func(context.Context, agentwire.FileWriteParams) error {
		return errors.New("disk full")
	}}
	err = p.Apply(ctx, "app", "routing: v2", "")
	if err == nil || !strings.Contains(err.Error(), "routing anomaly") {
		t.Fatalf("anomaly verdict = %v", err)
	}

	// The revision insert failing stops everything.
	db.rowErr = miscjobsFailOn(errors.New("insert down"), "CreateProxyRevision")
	if err := p.Apply(ctx, "app", "x", ""); err == nil || !strings.Contains(err.Error(), "proxy revision") {
		t.Fatalf("revision verdict = %v", err)
	}
}

func TestMiscjobsProxyVerifyHonorsCancellation(t *testing.T) {
	previous := verifyTimeout
	verifyTimeout = time.Minute
	t.Cleanup(func() { verifyTimeout = previous })

	// The exec itself failing under a canceled context returns the context's
	// error, never a fabricated verdict.
	ctx, cancel := context.WithCancel(context.Background())
	rt := &fake.Runtime{}
	rt.ContainerExecCreateFn = func(c context.Context, _ string, _ containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
		cancel()
		return containertypes.ExecCreateResponse{}, c.Err()
	}
	p := &ProxyApplier{Docker: rt, Host: &hostfake.Ops{}, Server: store.Server{ID: 1}}
	if err := p.verify(ctx, "app", "content", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled verify = %v", err)
	}

	// A poll that never converges stops at the sleep once the context dies.
	ctx2, cancel2 := context.WithCancel(context.Background())
	rt2 := verifyRuntime("nothing relevant")
	inner := rt2.ContainerExecInspectFn
	rt2.ContainerExecInspectFn = func(c context.Context, id string) (containertypes.ExecInspect, error) {
		cancel2()
		return inner(c, id)
	}
	p2 := &ProxyApplier{Docker: rt2, Host: &hostfake.Ops{}, Server: store.Server{ID: 1}}
	if err := p2.verify(ctx2, "app", "content", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled poll = %v", err)
	}
}

// --- certificates.go helpers -------------------------------------------------

// miscjobsSelfSigned mints a throwaway certificate chain, base64-encoded the
// way Traefik stores it in acme.json.
func miscjobsSelfSigned(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "unit.example.test"},
		Issuer:       pkix.Name{CommonName: "Unit CA"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return base64.StdEncoding.EncodeToString(pemBytes)
}
