// Coverage for the routing render/apply pipeline and the database job, plus
// small top-ups on the lifecycle and exec helpers. All identifiers are
// prefixed miscjobs (concurrent coverage agents share this package).
package jobs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	networktypes "github.com/docker/docker/api/types/network"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/jackc/pgx/v5"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/deepteams/akerdock/internal/agent"
	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/pki"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// --- applyrouting.go: Execute ------------------------------------------------

func miscjobsRoutingJob() store.Job {
	return store.Job{ID: 17, JobType: TypeApplyRouting, Payload: []byte(`{"resource_id":1,"revision":3}`)}
}

// miscjobsDomainRows makes ListDomainsForApplication answer n generic rows and
// ListServiceComponents answer none — a plain application.
func miscjobsDomainRows(n int) func(string) pgx.Rows {
	return func(sql string) pgx.Rows {
		switch {
		case strings.Contains(sql, "-- name: ListDomainsForApplication "):
			return &jobFlowRows{remaining: n}
		case strings.Contains(sql, "-- name: ListServiceComponents "):
			return &jobFlowRows{remaining: 0}
		}
		return nil
	}
}

func TestMiscjobsApplyRoutingConvergesADomain(t *testing.T) {
	q, keyring, logger, db := miscjobsDeps(t)
	db.rows = miscjobsDomainRows(1)
	rt := verifyRuntime(jobFixtureUUID)
	ops := &hostfake.Ops{}
	h := &ApplyRouting{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{ops: ops}, Logger: logger}
	j := miscjobsRoutingJob()
	result, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j))
	if err != nil {
		t.Fatalf("apply routing: %v", err)
	}
	out := result.(map[string]any)
	if out["app_uuid"] != jobFixtureUUID || out["routed"] != true {
		t.Fatalf("result = %#v", out)
	}
	writes := ops.CallsTo(agentwire.MethodFileWrite)
	if len(writes) != 1 {
		t.Fatalf("writes = %v", writes)
	}
	if content := string(writes[0].(agentwire.FileWriteParams).Content); !strings.Contains(content, jobFixtureUUID) {
		t.Fatalf("routing content = %s", content)
	}
}

func TestMiscjobsApplyRoutingRemovesWithoutDomains(t *testing.T) {
	q, keyring, logger, _ := miscjobsDeps(t)
	ops := &hostfake.Ops{}
	h := &ApplyRouting{Store: q, Keyring: keyring, Docker: fixedSource{rt: verifyRuntime("")}, HostOps: fixedHost{ops: ops}, Logger: logger}
	j := miscjobsRoutingJob()
	result, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j))
	if err != nil {
		t.Fatalf("apply routing: %v", err)
	}
	if result.(map[string]any)["routed"] != false {
		t.Fatalf("result = %#v", result)
	}
	if removes := ops.CallsTo(agentwire.MethodFileRemove); len(removes) != 1 {
		t.Fatalf("removes = %v", removes)
	}
}

func TestMiscjobsApplyRoutingStackPath(t *testing.T) {
	q, keyring, logger, db := miscjobsDeps(t)
	db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetApplicationByID")
	miscjobsEnum(t, "ResourceType", string(store.ResourceTypeService))
	h := &ApplyRouting{Store: q, Keyring: keyring, Docker: fixedSource{rt: verifyRuntime("")}, HostOps: fixedHost{}, Logger: logger}
	j := miscjobsRoutingJob()
	result, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j))
	if err != nil {
		t.Fatalf("stack routing: %v", err)
	}
	if result.(map[string]any)["routed"] != false {
		t.Fatalf("result = %#v", result)
	}
}

func TestMiscjobsApplyRoutingVerdicts(t *testing.T) {
	ctx := context.Background()
	j := miscjobsRoutingJob()

	t.Run("resource deleted", func(t *testing.T) {
		q, keyring, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetApplicationByID", "GetResourceByID")
		h := &ApplyRouting{Store: q, Keyring: keyring, Logger: logger}
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["status"] != "resource deleted, nothing to do" {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("resource not routable", func(t *testing.T) {
		q, keyring, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetApplicationByID")
		h := &ApplyRouting{Store: q, Keyring: keyring, Logger: logger}
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["status"] != "resource is not routable" {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("stack row vanished", func(t *testing.T) {
		q, keyring, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetApplicationByID", "GetServiceByID")
		miscjobsEnum(t, "ResourceType", string(store.ResourceTypeService))
		h := &ApplyRouting{Store: q, Keyring: keyring, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("missing service row must fail")
		}
	})
	t.Run("unmanaged proxy", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		miscjobsEnum(t, "ProxyType", "none")
		h := &ApplyRouting{Store: q, Keyring: keyring, Logger: logger}
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["status"] != "server has no managed proxy" {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("agent not connected", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		h := &ApplyRouting{Store: q, Keyring: keyring, Docker: unavailableDocker{}, HostOps: unavailableHost{}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("unavailable agent must fail")
		}
	})
	t.Run("host ops not connected", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		h := &ApplyRouting{Store: q, Keyring: keyring, Docker: fixedSource{rt: &fake.Runtime{}},
			HostOps: fixedHost{err: errors.New("not connected")}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("unavailable host ops must fail")
		}
	})
	t.Run("unrenderable wall fails closed", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		miscjobsEnum(t, "PreviewProtection", "weird")
		h := &ApplyRouting{Store: q, Keyring: keyring, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "unsupported access_protection") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unrenderable routing fails", func(t *testing.T) {
		// One domain, one component exposing no port: the deterministic
		// compose_routable_port_unresolved error stops the job.
		q, keyring, logger, db := miscjobsDeps(t)
		db.rows = func(sql string) pgx.Rows {
			if strings.Contains(sql, "-- name: ListDomainsForApplication ") {
				return &jobFlowRows{remaining: 1}
			}
			return nil
		}
		h := &ApplyRouting{Store: q, Keyring: keyring, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "compose_routable_port_unresolved") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("apply failure", func(t *testing.T) {
		miscjobsShortVerify(t)
		q, keyring, logger, _ := miscjobsDeps(t)
		ops := &hostfake.Ops{RemoveFn: func(context.Context, agentwire.FileRemoveParams) error {
			return errors.New("agent hiccup")
		}}
		h := &ApplyRouting{Store: q, Keyring: keyring, Docker: fixedSource{rt: verifyRuntime("")}, HostOps: fixedHost{ops: ops}, Logger: logger}
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("apply failure must fail the job")
		}
	})
	t.Run("dependency row failures", func(t *testing.T) {
		for _, name := range []string{"GetDestinationByID", "GetServerByID"} {
			q, keyring, logger, db := miscjobsDeps(t)
			db.rowErr = miscjobsFailOn(errors.New("no rows"), name)
			h := &ApplyRouting{Store: q, Keyring: keyring, Logger: logger}
			if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
				t.Fatalf("%s failure must fail the job", name)
			}
		}
	})
}

// --- applyrouting.go: route-group resolution --------------------------------

func TestMiscjobsApplicationRouteGroupPlainApp(t *testing.T) {
	q, _, _, db := miscjobsDeps(t)
	ctx := context.Background()
	app, err := q.GetApplicationByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	ports := "8080, 9090"
	app.RuntimeConfig.PortsExposes = &ports
	wildcard := "*.apps.example.test"
	credID := int64(1)
	db.override = func(sql string, index int, dest any) {
		if strings.Contains(sql, "-- name: GetServerByID ") {
			switch index {
			case 19:
				value := wildcard
				*(dest.(**string)) = &value
			case 49:
				*(dest.(**int64)) = &credID
			}
		}
	}
	db.rows = func(sql string) pgx.Rows {
		switch {
		case strings.Contains(sql, "-- name: ListDomainsForApplication "):
			return &miscjobsListRows{
				jobFlowRows: jobFlowRows{remaining: 2},
				override: func(row, _ int, dest any) {
					if p, ok := dest.(**int32); ok && row == 1 {
						port := int32(9443)
						*p = &port
					}
				},
			}
		case strings.Contains(sql, "-- name: ListServiceComponents "):
			return &jobFlowRows{remaining: 0}
		}
		return nil
	}
	rg, ok, err := applicationRouteGroup(ctx, q, app, "", nil)
	if err != nil || !ok {
		t.Fatalf("route group: ok=%v, %v", ok, err)
	}
	if rg.WildcardDomain != wildcard || rg.DNSProvider != "unit" {
		t.Fatalf("wildcard = %q/%q", rg.WildcardDomain, rg.DNSProvider)
	}
	if len(rg.Routes) != 2 || rg.Routes[0].TargetPort != 8080 || rg.Routes[1].TargetPort != 9443 {
		t.Fatalf("routes = %#v", rg.Routes)
	}
	if rg.Endpoint != jobFixtureUUID {
		t.Fatalf("endpoint = %q — Docker DNS by container name", rg.Endpoint)
	}
}

func TestMiscjobsApplicationRouteGroupComposeComponents(t *testing.T) {
	q, _, _, db := miscjobsDeps(t)
	ctx := context.Background()
	db.blob = []byte("[]") // components carry empty public-route JSON
	app, err := q.GetApplicationByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	port := int32(3000)
	db.rows = func(sql string) pgx.Rows {
		componentPort := func(_, _ int, dest any) {
			if p, ok := dest.(**int32); ok {
				value := port
				*p = &value
			}
		}
		switch {
		case strings.Contains(sql, "-- name: ListDomainsForApplication "):
			return &jobFlowRows{remaining: 1}
		case strings.Contains(sql, "-- name: ListServiceComponents "):
			return &miscjobsListRows{jobFlowRows: jobFlowRows{remaining: 1, blob: db.blob}, override: componentPort}
		case strings.Contains(sql, "-- name: ListServiceComponentDomains "):
			return &jobFlowRows{remaining: 1, blob: db.blob}
		}
		return nil
	}
	rg, ok, err := applicationRouteGroup(ctx, q, app, "", map[string]string{"unit": "10.0.0.9"})
	if err != nil || !ok {
		t.Fatalf("route group: ok=%v, %v", ok, err)
	}
	// The application-level domain resolved to the stack's web component and
	// took the endpoint override; the component's own domain did too.
	if len(rg.Routes) != 2 {
		t.Fatalf("routes = %#v", rg.Routes)
	}
	for _, route := range rg.Routes {
		if route.Endpoint != "10.0.0.9" || route.TargetPort != int(port) {
			t.Fatalf("route = %#v", route)
		}
	}

	// Without the override the component routes by its container name.
	rg, _, err = applicationRouteGroup(ctx, q, app, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range rg.Routes {
		if route.Endpoint != jobFixtureUUID+"-unit" {
			t.Fatalf("component endpoint = %q", route.Endpoint)
		}
	}
}

func TestMiscjobsApplicationRouteGroupFailures(t *testing.T) {
	ctx := context.Background()

	load := func(t *testing.T) (*store.Queries, store.GetApplicationByIDRow, *miscjobsDB) {
		t.Helper()
		q, _, _, db := miscjobsDeps(t)
		app, err := q.GetApplicationByID(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		return q, app, db
	}

	t.Run("domain listing failure", func(t *testing.T) {
		q, app, db := load(t)
		db.rowErr = miscjobsFailOn(errors.New("db down"), "ListDomainsForApplication")
		if _, _, err := applicationRouteGroup(ctx, q, app, "", nil); err == nil {
			t.Fatal("domain listing failure must surface")
		}
	})
	t.Run("component listing failure", func(t *testing.T) {
		q, app, db := load(t)
		db.rowErr = miscjobsFailOn(errors.New("db down"), "ListServiceComponents")
		if _, _, err := applicationRouteGroup(ctx, q, app, "", nil); err == nil {
			t.Fatal("component listing failure must surface")
		}
	})
	t.Run("component domain listing failure", func(t *testing.T) {
		q, app, db := load(t)
		db.blob = []byte("[]")
		db.rowErr = miscjobsFailOn(errors.New("db down"), "ListServiceComponentDomains")
		if _, _, err := applicationRouteGroup(ctx, q, app, "", nil); err == nil {
			t.Fatal("component domain failure must surface")
		}
	})
	t.Run("application public routes must decode", func(t *testing.T) {
		q, app, _ := load(t)
		app.Application.AccessPublicRoutes = []byte("{corrupt")
		if _, _, err := applicationRouteGroup(ctx, q, app, "", nil); err == nil ||
			!strings.Contains(err.Error(), "decode application public routes") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("component public routes must decode", func(t *testing.T) {
		// The generic byte fixture is not JSON: the component's stored routes
		// fail to decode and the resolution stops.
		q, app, db := load(t)
		db.rows = func(sql string) pgx.Rows {
			if strings.Contains(sql, "-- name: ListServiceComponentDomains ") {
				return &miscjobsListRows{
					jobFlowRows: jobFlowRows{remaining: 1},
					override: func(_, _ int, dest any) {
						if p, ok := dest.(**int32); ok {
							value := int32(3000)
							*p = &value
						}
					},
				}
			}
			return nil
		}
		if _, _, err := applicationRouteGroup(ctx, q, app, "", nil); err == nil ||
			!strings.Contains(err.Error(), "decode public routes for component") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("component domain without a resolvable port", func(t *testing.T) {
		q, app, db := load(t)
		db.blob = []byte("[]")
		db.rows = func(sql string) pgx.Rows {
			if strings.Contains(sql, "-- name: ListServiceComponentDomains ") {
				return &jobFlowRows{remaining: 1, blob: db.blob}
			}
			return nil
		}
		if _, _, err := applicationRouteGroup(ctx, q, app, "", nil); err == nil ||
			!strings.Contains(err.Error(), "compose_routable_port_unresolved") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMiscjobsAppendComponentRoutesPortResolution(t *testing.T) {
	q, _, _, db := miscjobsDeps(t)
	ctx := context.Background()
	db.blob = []byte("[]")
	defaultPort := int32(8081)
	explicit := int32(9091)
	db.rows = func(sql string) pgx.Rows {
		if strings.Contains(sql, "-- name: ListServiceComponentDomains ") {
			return &miscjobsListRows{
				jobFlowRows: jobFlowRows{remaining: 2, blob: db.blob},
				override: func(row, _ int, dest any) {
					if p, ok := dest.(**int32); ok && row == 0 {
						value := explicit
						*p = &value
					}
				},
			}
		}
		return nil
	}
	components := []store.ServiceComponent{{ID: 1, Name: "web", DefaultRoutePort: &defaultPort, AccessPublicRoutes: []byte("[]")}}
	rg := &proxy.RouteGroup{}
	if err := appendComponentRoutes(ctx, q, components, "app", rg, map[string]string{"web": "10.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	if len(rg.Routes) != 2 || rg.Routes[0].TargetPort != int(explicit) || rg.Routes[1].TargetPort != int(defaultPort) {
		t.Fatalf("routes = %#v", rg.Routes)
	}
	for _, route := range rg.Routes {
		if route.Endpoint != "10.1.1.1" {
			t.Fatalf("endpoint = %q", route.Endpoint)
		}
	}
}

func TestMiscjobsDecodeStoredPublicRoutes(t *testing.T) {
	if routes, err := decodeStoredPublicRoutes(nil); routes != nil || err != nil {
		t.Fatalf("empty = %#v, %v", routes, err)
	}
	routes, err := decodeStoredPublicRoutes([]byte(`[{"path":"/health","match":"exact","methods":["GET"]}]`))
	if err != nil || len(routes) != 1 || routes[0].Path != "/health" {
		t.Fatalf("valid = %#v, %v", routes, err)
	}
	if _, err := decodeStoredPublicRoutes([]byte(`[{"path":"/x","match":"bogus","methods":["GET"]}]`)); err == nil ||
		!strings.Contains(err.Error(), "route 0") {
		t.Fatalf("invalid route = %v", err)
	}
	if _, err := decodeStoredPublicRoutes([]byte("{corrupt")); err == nil {
		t.Fatal("corrupt JSON must surface")
	}
}

// --- applyrouting.go: preview rendering -------------------------------------

func TestMiscjobsRenderPreviewRoutingFile(t *testing.T) {
	q, _, _, _ := miscjobsDeps(t)
	ctx := context.Background()
	app, err := q.GetApplicationByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	app.Application.AccessPublicRoutes = []byte("[]")
	preview := store.Preview{Uuid: mustUUID(t, "22222222-2222-4222-8222-222222222222")}

	// No FQDN: the preview runs unrouted.
	content, err := RenderPreviewRoutingFile(app, preview, 1, "", "", "")
	if err != nil || content != "" {
		t.Fatalf("unrouted preview = %q, %v", content, err)
	}

	// Routed, behind basic auth, on the template's port.
	fqdn := "pr-42.example.test"
	preview.Fqdn = &fqdn
	app.Application.PreviewUrlTemplates = []byte(`[{"host":"pr-{{pr_id}}.example.test","port":4000}]`)
	app.Application.PreviewProtection = store.PreviewProtectionBasicAuth
	content, err = RenderPreviewRoutingFile(app, preview, 1, "", "user:$2y$05$hash", "")
	if err != nil || !strings.Contains(content, fqdn) || !strings.Contains(content, ":4000") {
		t.Fatalf("routed preview = %q, %v", content, err)
	}

	// The ports_exposes fallback drives the port when no template names one.
	ports := "3000,4000"
	app.Application.PreviewUrlTemplates = nil
	app.Application.PreviewUrlTemplate = ""
	app.RuntimeConfig.PortsExposes = &ports
	content, err = RenderPreviewRoutingFile(app, preview, 1, "", "user:hash", "")
	if err != nil || !strings.Contains(content, ":3000") {
		t.Fatalf("fallback port preview = %q, %v", content, err)
	}

	// Corrupt stored public routes stop the render.
	app.Application.AccessPublicRoutes = []byte("{corrupt")
	if _, err := RenderPreviewRoutingFile(app, preview, 1, "", "", ""); err == nil ||
		!strings.Contains(err.Error(), "decode preview public routes") {
		t.Fatalf("err = %v", err)
	}
}

func TestMiscjobsInjectPreviewSSOCallbackGuards(t *testing.T) {
	content := "http:\n  routers:\n  services:\n"
	if got := injectPreviewSSOCallback(content, "p", nil, "https://cp.example.test"); got != content {
		t.Fatalf("no hosts must be a no-op, got %q", got)
	}
	if got := injectPreviewSSOCallback(content, "p", []string{"pr.example.test"}, ""); got != content {
		t.Fatalf("no instance URL must be a no-op, got %q", got)
	}
}

// --- databaserun.go ----------------------------------------------------------

// miscjobsDatabaseRT scripts a healthy provisioning runtime: absent volume,
// healthy container, a one-shot uid probe answering `uid`.
func miscjobsDatabaseRT(uid string) *fake.Runtime {
	rt := &fake.Runtime{}
	rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
		return volumetypes.Volume{}, miscjobsNotFound("volume")
	}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: true, Health: &containertypes.Health{Status: "healthy"}},
		}}, nil
	}
	rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispec.Platform, string) (containertypes.CreateResponse, error) {
		return containertypes.CreateResponse{ID: "created"}, nil
	}
	rt.ContainerWaitFn = func(context.Context, string, containertypes.WaitCondition) (<-chan containertypes.WaitResponse, <-chan error) {
		waitCh := make(chan containertypes.WaitResponse, 1)
		waitCh <- containertypes.WaitResponse{StatusCode: 0}
		return waitCh, make(chan error, 1)
	}
	rt.ContainerLogsFn = func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(miscjobsStdcopy(uid + "\n"))), nil
	}
	return rt
}

func TestMiscjobsDatabaseProvisionWithTLSAndCustomConfig(t *testing.T) {
	q, keyring, logger, _ := miscjobsDeps(t)
	ctx := context.Background()
	row, err := q.GetDatabaseByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	custom := "shared_buffers = '1GB'"
	initdb := "--data-checksums"
	image := "postgres"
	tag := "16-alpine"
	row.Database.CustomConfig = &custom
	row.Database.InitdbArgs = &initdb
	row.Database.Image = &image
	row.Database.ImageTag = &tag
	row.Database.SslEnabled = true

	rt := miscjobsDatabaseRT("70")
	ops := &hostfake.Ops{}
	h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
	if err := h.provision(ctx, rt, ops, nil, row, "akerdock-net", "dbuuid"); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// The key was chowned to the uid the image itself reported.
	chowns := ops.CallsTo(agentwire.MethodFileChown)
	if len(chowns) != 1 || chowns[0].(agentwire.FileChownParams).UID != 70 {
		t.Fatalf("chowns = %v", chowns)
	}
	// Config + cert + key deposited through the channel.
	var paths []string
	for _, c := range ops.CallsTo(agentwire.MethodFileWrite) {
		paths = append(paths, c.(agentwire.FileWriteParams).Path)
	}
	for _, want := range []string{"postgresql.conf", "server.crt", "server.key"} {
		found := false
		for _, p := range paths {
			if strings.HasSuffix(p, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s in %v", want, paths)
		}
	}
	// The image:tag derivation and the TLS/init args reached the create.
	for _, c := range rt.Calls() {
		if c.Method != "ContainerCreate" {
			continue
		}
		cfg := c.Args[0].(*containertypes.Config)
		if cfg.Image == "postgres:16-alpine" && c.Args[4].(string) == "dbuuid" {
			joined := strings.Join(cfg.Cmd, " ")
			if !strings.Contains(joined, "ssl=on") || !strings.Contains(joined, "config_file=") {
				t.Fatalf("cmd = %v", cfg.Cmd)
			}
			found := false
			for _, e := range cfg.Env {
				if e == "POSTGRES_INITDB_ARGS="+initdb {
					found = true
				}
			}
			if !found {
				t.Fatalf("env = %v", cfg.Env)
			}
			return
		}
	}
	t.Fatal("no database ContainerCreate observed")
}

func TestMiscjobsDatabaseProvisionPublishesThePortMapping(t *testing.T) {
	q, keyring, logger, _ := miscjobsDeps(t)
	ctx := context.Background()
	row, err := q.GetDatabaseByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	port := int32(5433)
	mode := store.PublicAccessModePortMapping
	row.Database.IsPublic = true
	row.Database.PublicPort = &port
	row.Database.PublicAccessMode = &mode

	rt := miscjobsDatabaseRT("999")
	h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
	if err := h.provision(ctx, rt, &hostfake.Ops{}, nil, row, "net", "dbuuid"); err != nil {
		t.Fatalf("provision: %v", err)
	}
	for _, c := range rt.Calls() {
		if c.Method == "ContainerCreate" && c.Args[4].(string) == "dbuuid" {
			host := c.Args[1].(*containertypes.HostConfig)
			bindings := host.PortBindings["5432/tcp"]
			if len(bindings) != 1 || bindings[0].HostPort != "5433" {
				t.Fatalf("port bindings = %v", host.PortBindings)
			}
			return
		}
	}
	t.Fatal("no database ContainerCreate observed")
}

func TestMiscjobsDatabaseProvisionFailurePoints(t *testing.T) {
	ctx := context.Background()

	load := func(t *testing.T) (*DatabaseRun, store.GetDatabaseByIDRow, *miscjobsDB) {
		t.Helper()
		q, keyring, logger, db := miscjobsDeps(t)
		row, err := q.GetDatabaseByID(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		return &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}, row, db
	}

	t.Run("password refuses to decrypt", func(t *testing.T) {
		h, row, _ := load(t)
		row.DatabaseCredential.PasswordEnc = []byte("garbage")
		if err := h.provision(ctx, miscjobsDatabaseRT("999"), &hostfake.Ops{}, nil, row, "net", "dbuuid"); err == nil {
			t.Fatal("a corrupt credential must fail the provision")
		}
	})
	t.Run("custom config upload failure", func(t *testing.T) {
		h, row, _ := load(t)
		custom := "x = 1"
		row.Database.CustomConfig = &custom
		ops := &hostfake.Ops{WriteFileFn: func(context.Context, agentwire.FileWriteParams) error {
			return errors.New("agent gone")
		}}
		if err := h.provision(ctx, miscjobsDatabaseRT("999"), ops, nil, row, "net", "dbuuid"); err == nil ||
			!strings.Contains(err.Error(), "custom configuration failed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("certificate upload failure", func(t *testing.T) {
		h, row, _ := load(t)
		row.Database.SslEnabled = true
		ops := &hostfake.Ops{WriteFileFn: func(_ context.Context, p agentwire.FileWriteParams) error {
			if strings.HasSuffix(p.Path, "server.crt") {
				return errors.New("agent gone")
			}
			return nil
		}}
		if err := h.provision(ctx, miscjobsDatabaseRT("999"), ops, nil, row, "net", "dbuuid"); err == nil ||
			!strings.Contains(err.Error(), "database certificate failed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("key upload failure", func(t *testing.T) {
		h, row, _ := load(t)
		row.Database.SslEnabled = true
		ops := &hostfake.Ops{WriteFileFn: func(_ context.Context, p agentwire.FileWriteParams) error {
			if strings.HasSuffix(p.Path, "server.key") {
				return errors.New("agent gone")
			}
			return nil
		}}
		if err := h.provision(ctx, miscjobsDatabaseRT("999"), ops, nil, row, "net", "dbuuid"); err == nil ||
			!strings.Contains(err.Error(), "database key failed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("chown failure", func(t *testing.T) {
		h, row, _ := load(t)
		row.Database.SslEnabled = true
		ops := &hostfake.Ops{ChownFn: func(context.Context, agentwire.FileChownParams) error {
			return errors.New("agent gone")
		}}
		if err := h.provision(ctx, miscjobsDatabaseRT("999"), ops, nil, row, "net", "dbuuid"); err == nil ||
			!strings.Contains(err.Error(), "chowning the database key failed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("volume inspect breakage", func(t *testing.T) {
		h, row, _ := load(t)
		rt := miscjobsDatabaseRT("999")
		rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
			return volumetypes.Volume{}, errors.New("daemon down")
		}
		if err := h.provision(ctx, rt, &hostfake.Ops{}, nil, row, "net", "dbuuid"); err == nil ||
			!strings.Contains(err.Error(), "daemon down") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("volume create failure", func(t *testing.T) {
		h, row, _ := load(t)
		rt := miscjobsDatabaseRT("999")
		rt.VolumeCreateFn = func(context.Context, volumetypes.CreateOptions) (volumetypes.Volume, error) {
			return volumetypes.Volume{}, errors.New("no space")
		}
		if err := h.provision(ctx, rt, &hostfake.Ops{}, nil, row, "net", "dbuuid"); err == nil ||
			!strings.Contains(err.Error(), "no space") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("container create failure", func(t *testing.T) {
		h, row, _ := load(t)
		rt := miscjobsDatabaseRT("999")
		rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispec.Platform, string) (containertypes.CreateResponse, error) {
			return containertypes.CreateResponse{}, errors.New("create refused")
		}
		if err := h.provision(ctx, rt, &hostfake.Ops{}, nil, row, "net", "dbuuid"); err == nil ||
			!strings.Contains(err.Error(), "creating the database container failed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("container start failure", func(t *testing.T) {
		h, row, _ := load(t)
		rt := miscjobsDatabaseRT("999")
		rt.ContainerStartFn = func(context.Context, string, containertypes.StartOptions) error {
			return errors.New("start refused")
		}
		if err := h.provision(ctx, rt, &hostfake.Ops{}, nil, row, "net", "dbuuid"); err == nil ||
			!strings.Contains(err.Error(), "starting the database container failed") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMiscjobsDatabaseCertificateCABranches(t *testing.T) {
	ctx := context.Background()

	t.Run("existing CA is reused", func(t *testing.T) {
		q, keyring, logger, db := miscjobsDeps(t)
		ca, err := pki.NewCA("unit-server")
		if err != nil {
			t.Fatal(err)
		}
		enc, err := keyring.Encrypt("servers", "ca_key_enc", jobFixtureUUID, ca.KeyPEM)
		if err != nil {
			t.Fatal(err)
		}
		db.override = func(sql string, index int, dest any) {
			if strings.Contains(sql, "-- name: GetServerByID ") {
				switch index {
				case 38: // ca_cert
					value := string(ca.CertPEM)
					*(dest.(**string)) = &value
				case 39: // ca_key_enc
					*(dest.(*[]byte)) = append([]byte(nil), enc...)
				}
			}
		}
		h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
		row, err := q.GetDatabaseByID(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := h.databaseCertificate(ctx, row, "dbuuid")
		if err != nil || len(leaf.CertPEM) == 0 || len(leaf.KeyPEM) == 0 {
			t.Fatalf("leaf = %#v, %v", leaf, err)
		}
	})
	t.Run("existing CA that cannot decrypt", func(t *testing.T) {
		q, keyring, logger, db := miscjobsDeps(t)
		db.override = func(sql string, index int, dest any) {
			if strings.Contains(sql, "-- name: GetServerByID ") && index == 38 {
				value := "-----BEGIN CERTIFICATE-----"
				*(dest.(**string)) = &value
			}
		}
		h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
		row, err := q.GetDatabaseByID(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		// The generic fixture bytes are bound to another column: decrypt fails.
		if _, err := h.databaseCertificate(ctx, row, "dbuuid"); err == nil {
			t.Fatal("an undecryptable CA key must fail, never re-mint silently")
		}
	})
	t.Run("server vanished", func(t *testing.T) {
		q, keyring, logger, db := miscjobsDeps(t)
		row, err := q.GetDatabaseByID(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetServerByID")
		h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
		if _, err := h.databaseCertificate(ctx, row, "dbuuid"); err == nil {
			t.Fatal("a vanished server must fail the certificate")
		}
	})
}

func TestMiscjobsProbePostgresUID(t *testing.T) {
	ctx := context.Background()
	if uid := probePostgresUID(ctx, miscjobsDatabaseRT("70"), "postgres:16-alpine"); uid != 70 {
		t.Fatalf("uid = %d", uid)
	}
	if uid := probePostgresUID(ctx, miscjobsDatabaseRT("not-a-number"), "postgres"); uid != 999 {
		t.Fatalf("garbage output uid = %d", uid)
	}
	rt := miscjobsDatabaseRT("70")
	rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispec.Platform, string) (containertypes.CreateResponse, error) {
		return containertypes.CreateResponse{}, errors.New("create refused")
	}
	if uid := probePostgresUID(ctx, rt, "postgres"); uid != 999 {
		t.Fatalf("probe failure uid = %d", uid)
	}
}

func TestMiscjobsDatabaseLifecycleAndDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("stop converges the statuses", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
		if err := h.lifecycle(ctx, &fake.Runtime{}, "stop", "dbuuid", 1); err != nil {
			t.Fatalf("stop: %v", err)
		}
	})
	t.Run("lifecycle without a container names the fix", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		rt := &fake.Runtime{}
		rt.ContainerStartFn = func(context.Context, string, containertypes.StartOptions) error {
			return miscjobsNotFound("container")
		}
		h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
		if err := h.lifecycle(ctx, rt, "start", "dbuuid", 1); err == nil ||
			!strings.Contains(err.Error(), "provision it first") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("lifecycle daemon error", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		rt := &fake.Runtime{}
		rt.ContainerRestartFn = func(context.Context, string, containertypes.StopOptions) error {
			return errors.New("daemon down")
		}
		h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
		if err := h.lifecycle(ctx, rt, "restart", "dbuuid", 1); err == nil ||
			!strings.Contains(err.Error(), "daemon down") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("delete failure points", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		row, err := q.GetDatabaseByID(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
		ops := &hostfake.Ops{RemoveFn: func(context.Context, agentwire.FileRemoveParams) error {
			return errors.New("read-only")
		}}
		if err := h.delete(ctx, &fake.Runtime{}, ops, row, "dbuuid", false); err == nil ||
			!strings.Contains(err.Error(), "directory removal") {
			t.Fatalf("err = %v", err)
		}
		rt := &fake.Runtime{}
		rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
			return volumetypes.ListResponse{}, errors.New("daemon down")
		}
		if err := h.delete(ctx, rt, &hostfake.Ops{}, row, "dbuuid", true); err == nil ||
			!strings.Contains(err.Error(), "daemon down") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMiscjobsDatabaseApplyTCPRoute(t *testing.T) {
	ctx := context.Background()

	load := func(t *testing.T) (*DatabaseRun, store.GetDatabaseByIDRow, *miscjobsDB) {
		t.Helper()
		q, keyring, logger, db := miscjobsDeps(t)
		row, err := q.GetDatabaseByID(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		return &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}, row, db
	}
	proxied := func(row *store.GetDatabaseByIDRow) {
		port := int32(15432)
		mode := store.PublicAccessModeTcpProxy
		row.Database.IsPublic = true
		row.Database.PublicPort = &port
		row.Database.PublicAccessMode = &mode
	}

	t.Run("route deposited hot", func(t *testing.T) {
		h, row, _ := load(t)
		proxied(&row)
		ops := &hostfake.Ops{}
		if err := h.applyTCPRoute(ctx, &fake.Runtime{}, ops, nil, row, "dbuuid"); err != nil {
			t.Fatalf("apply: %v", err)
		}
		writes := ops.CallsTo(agentwire.MethodFileWrite)
		if len(writes) != 1 {
			t.Fatalf("writes = %v", writes)
		}
		w := writes[0].(agentwire.FileWriteParams)
		if w.Path != "/var/lib/akerdock/proxy/dynamic/dbuuid.yaml" || !w.Atomic ||
			!strings.Contains(string(w.Content), "15432") {
			t.Fatalf("route write = %+v", w)
		}
	})
	t.Run("route write failure", func(t *testing.T) {
		h, row, _ := load(t)
		proxied(&row)
		ops := &hostfake.Ops{WriteFileFn: func(context.Context, agentwire.FileWriteParams) error {
			return errors.New("agent gone")
		}}
		if err := h.applyTCPRoute(ctx, &fake.Runtime{}, ops, nil, row, "dbuuid"); err == nil ||
			!strings.Contains(err.Error(), "writing the TCP route failed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unproxied removal failure", func(t *testing.T) {
		h, row, _ := load(t)
		ops := &hostfake.Ops{RemoveFn: func(context.Context, agentwire.FileRemoveParams) error {
			return errors.New("read-only")
		}}
		if err := h.applyTCPRoute(ctx, &fake.Runtime{}, ops, nil, row, "dbuuid"); err == nil ||
			!strings.Contains(err.Error(), "read-only") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("server vanished", func(t *testing.T) {
		h, row, db := load(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetServerByID")
		if err := h.applyTCPRoute(ctx, &fake.Runtime{}, &hostfake.Ops{}, nil, row, "dbuuid"); err == nil {
			t.Fatal("a vanished server must fail the route")
		}
	})
}

func TestMiscjobsDatabaseWaitHealthyHonorsCancellation(t *testing.T) {
	oldTimeout, oldPoll := databaseReadyTimeout, databaseReadyPoll
	databaseReadyTimeout = time.Hour
	databaseReadyPoll = time.Hour
	t.Cleanup(func() { databaseReadyTimeout, databaseReadyPoll = oldTimeout, oldPoll })

	ctx, cancel := context.WithCancel(context.Background())
	rt := &fake.Runtime{}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		cancel() // never healthy; the caller gives up
		return containertypes.InspectResponse{
			ContainerJSONBase: &containertypes.ContainerJSONBase{},
		}, nil
	}
	q, keyring, logger, _ := miscjobsDeps(t)
	h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
	if err := h.waitHealthy(ctx, rt, "dbuuid"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func miscjobsDatabaseJob(action, extra string) store.Job {
	return store.Job{ID: 18, JobType: "database." + action,
		Payload: []byte(`{"resource_id":1,"action":"` + action + `"` + extra + `}`)}
}

func TestMiscjobsDatabaseExecuteVerdicts(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown action", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		h := &DatabaseRun{Store: q, Keyring: keyring, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsDatabaseJob("explode", "")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "unknown database action") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("delete of a deleted database", func(t *testing.T) {
		q, keyring, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetDatabaseByID")
		h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
		j := miscjobsDatabaseJob("delete", "")
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["status"] != "already deleted" {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
	t.Run("start of a deleted database", func(t *testing.T) {
		q, keyring, logger, db := miscjobsDeps(t)
		db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetDatabaseByID")
		h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
		j := miscjobsDatabaseJob("start", "")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "database not found") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("dependency row failures", func(t *testing.T) {
		for _, name := range []string{"GetServerByID", "GetDestinationByID"} {
			q, keyring, logger, db := miscjobsDeps(t)
			db.rowErr = miscjobsFailOn(errors.New("no rows"), name)
			h := &DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
			j := miscjobsDatabaseJob("start", "")
			if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
				t.Fatalf("%s failure must fail the job", name)
			}
		}
	})
	t.Run("host ops unavailable for provision and delete", func(t *testing.T) {
		for _, action := range []string{"provision", "delete"} {
			q, keyring, logger, _ := miscjobsDeps(t)
			h := &DatabaseRun{Store: q, Keyring: keyring, Docker: fixedSource{rt: &fake.Runtime{}},
				HostOps: unavailableHost{}, Logger: logger}
			j := miscjobsDatabaseJob(action, "")
			if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
				t.Fatalf("%s without host ops must fail", action)
			}
		}
	})
	t.Run("provision needs SSH for the proxy convergence", func(t *testing.T) {
		q, keyring, logger, db := miscjobsDeps(t)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		host, port, _ := net.SplitHostPort(listener.Addr().String())
		_ = listener.Close()
		db.host = host
		fmt.Sscan(port, &db.port)
		h := &DatabaseRun{Store: q, Keyring: keyring, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsDatabaseJob("provision", "")
		if _, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("a dead SSH endpoint must fail the provision")
		}
	})
	t.Run("stop end to end", func(t *testing.T) {
		q, keyring, logger, _ := miscjobsDeps(t)
		h := &DatabaseRun{Store: q, Keyring: keyring, Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{}, Logger: logger}
		j := miscjobsDatabaseJob("stop", "")
		result, err := h.Execute(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil || result.(map[string]any)["action"] != "stop" {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})
}

// --- lifecycle / exec / certificate top-ups ---------------------------------

func TestMiscjobsContainerLifecycleUnknownAction(t *testing.T) {
	if err := containerLifecycle(context.Background(), &fake.Runtime{}, "explode", "c", 1); err == nil ||
		!strings.Contains(err.Error(), "unknown lifecycle action") {
		t.Fatalf("err = %v", err)
	}
}

func TestMiscjobsStackLifecycleReportsTheFirstFailure(t *testing.T) {
	rt := &fake.Runtime{}
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return []containertypes.Summary{{Names: []string{"/a"}}, {Names: []string{"/b"}}}, nil
	}
	var restarted []string
	rt.ContainerRestartFn = func(_ context.Context, name string, _ containertypes.StopOptions) error {
		restarted = append(restarted, name)
		return errors.New("restart of " + name + " failed")
	}
	err := stackLifecycle(context.Background(), rt, "restart", stackFilter(store.Resource{}, "abc"), 1)
	if err == nil || !strings.Contains(err.Error(), "restart of a failed") {
		t.Fatalf("err = %v", err)
	}
	if len(restarted) != 2 {
		t.Fatalf("restarted = %v — a failing container must not stop the sweep", restarted)
	}
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return nil, errors.New("daemon down")
	}
	if err := stackLifecycle(context.Background(), rt, "restart", stackFilter(store.Resource{}, "abc"), 1); err == nil {
		t.Fatal("a failing list must surface")
	}
}

// miscjobsErrReader always fails, exercising the demux error path.
type miscjobsErrReader struct{}

func (miscjobsErrReader) Read([]byte) (int, error) { return 0, errors.New("stream torn") }

func TestMiscjobsExecCaptureErrorPaths(t *testing.T) {
	ctx := context.Background()

	// Attach failure.
	rt := &fake.Runtime{}
	rt.ContainerExecCreateFn = func(context.Context, string, containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
		return containertypes.ExecCreateResponse{ID: "exec"}, nil
	}
	rt.ContainerExecAttachFn = func(context.Context, string, containertypes.ExecAttachOptions) (types.HijackedResponse, error) {
		return types.HijackedResponse{}, errors.New("attach refused")
	}
	if _, _, err := execCapture(ctx, rt, "c", []string{"true"}); err == nil ||
		!strings.Contains(err.Error(), "attach refused") {
		t.Fatalf("attach err = %v", err)
	}

	// A torn stream surfaces from the demux.
	rt.ContainerExecAttachFn = func(context.Context, string, containertypes.ExecAttachOptions) (types.HijackedResponse, error) {
		client, server := net.Pipe()
		_ = server.Close()
		return types.HijackedResponse{Conn: client, Reader: bufio.NewReader(miscjobsErrReader{})}, nil
	}
	if _, _, err := execCapture(ctx, rt, "c", []string{"true"}); err == nil ||
		!strings.Contains(err.Error(), "stream torn") {
		t.Fatalf("demux err = %v", err)
	}

	// The exit-code inspect failing keeps the captured output.
	rt.ContainerExecAttachFn = func(context.Context, string, containertypes.ExecAttachOptions) (types.HijackedResponse, error) {
		var buf bytes.Buffer
		buf.WriteString(miscjobsStdcopy("partial"))
		client, server := net.Pipe()
		go func() {
			_, _ = server.Write(buf.Bytes())
			_ = server.Close()
		}()
		return types.HijackedResponse{Conn: client, Reader: bufio.NewReader(client)}, nil
	}
	rt.ContainerExecInspectFn = func(context.Context, string) (containertypes.ExecInspect, error) {
		return containertypes.ExecInspect{}, errors.New("inspect refused")
	}
	out, _, err := execCapture(ctx, rt, "c", []string{"true"})
	if err == nil || !strings.Contains(err.Error(), "inspect refused") || out != "partial" {
		t.Fatalf("inspect err = %q, %v", out, err)
	}
}

func TestMiscjobsCertificateRenewSurvivesABackupFailure(t *testing.T) {
	q, _, logger, _ := miscjobsDeps(t)
	rt := &fake.Runtime{}
	rt.ContainerRestartFn = func(context.Context, string, containertypes.StopOptions) error { return nil }
	ops := &hostfake.Ops{CopyFileFn: func(context.Context, agentwire.FileCopyParams) error {
		return errors.New("acme.json does not exist yet")
	}}
	h := &CertificateSync{Store: q, Docker: fixedSource{rt: rt}, HostOps: fixedHost{ops: ops}, Logger: logger}
	j := miscjobsCertJob(TypeCertificateRenew, `{"server_id":1,"certificate_id":1}`)
	if _, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j)); err != nil {
		t.Fatalf("renew with missing store: %v", err)
	}
}

func TestMiscjobsEnsureAgentRunFailure(t *testing.T) {
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
	_ = client.Close() // a dead connection: the deploy command cannot run
	if err := ensureAgent(ctx, client, &hostfake.Ops{}, "net", "img", "res", agent.Config{}, AgentEnv{}); err == nil {
		t.Fatal("a dead SSH client must fail the deploy")
	}
}
