package jobs

// The refusal paths of adoption.go (§20.7, ADR-013/ADR-023). Adoption is the
// one flow whose whole promise is that nothing on the server moves: a scan
// that cannot read the inventory, a candidate whose container vanished
// between the scan and the adoption, a disown whose routing will not detach —
// each must stop with a reason, never half-way through.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	adoptionmodel "github.com/deepteams/akerdock/internal/adoption"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshkey"
	"github.com/deepteams/akerdock/internal/store"
)

// adoptcovNotFound is the daemon's typed answer for a container that is gone —
// the one error the scan and the adoption treat as information rather than
// failure.
func adoptcovNotFound() error { return composecovNotFound("container") }

// adoptcovUnique is a PostgreSQL 23505, which adoption reads as "a previous
// attempt already committed this row".
func adoptcovUnique() error { return &pgconn.PgError{Code: "23505"} }

// adoptcovInspect is one unmanaged standalone container.
func adoptcovInspect(labels map[string]string) containertypes.InspectResponse {
	return containertypes.InspectResponse{
		ContainerJSONBase: &containertypes.ContainerJSONBase{
			ID: "0123456789abcdef", Name: "/legacy-api",
			State: &containertypes.State{Status: "running"},
		},
		Config: &containertypes.Config{
			Image: "example/api:1", Env: []string{"BASE=1", "CUSTOM=2"}, Labels: labels,
		},
	}
}

// adoptcovRuntime lists one container and inspects it as unmanaged.
func adoptcovRuntime() *fake.Runtime {
	rt := &fake.Runtime{}
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return []containertypes.Summary{{ID: "0123456789abcdef"}}, nil
	}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return adoptcovInspect(map[string]string{}), nil
	}
	rt.ImageInspectFn = func(context.Context, string, ...client.ImageInspectOption) (imagetypes.InspectResponse, error) {
		return imagetypes.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{
			ImageConfig: ocispec.ImageConfig{Env: []string{"BASE=1"}},
		}}, nil
	}
	return rt
}

func adoptcovScanJob() store.Job {
	return store.Job{ID: 71, JobType: TypeAdoptionScan, Payload: []byte(`{"scan_id":1}`)}
}

// Everything a scan can fail on. A failed scan writes its reason on the scan
// row: the operator reads it there, not in the worker's log.
func TestAdoptcovScanFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("an invalid payload never reaches the server", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
		j := store.Job{ID: 71, JobType: TypeAdoptionScan, Payload: []byte(`{`)}
		if _, err := h.ExecuteScan(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want a payload error")
		}
	})

	t.Run("a scan row that vanished", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["GetAdoptionScanByID"] = pgx.ErrNoRows
		h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
		j := adoptcovScanJob()
		if _, err := h.ExecuteScan(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "vanished") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a server that vanished fails the scan row", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["GetServerByID"] = pgx.ErrNoRows
		h := &Adoption{Store: q, Keyring: keyring, Docker: fixedSource{rt: adoptcovRuntime()}, Logger: logger}
		j := adoptcovScanJob()
		if _, err := h.ExecuteScan(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want the server failure")
		}
	})

	t.Run("no agent channel means no inventory", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		h := &Adoption{Store: q, Keyring: keyring, Docker: unavailableDocker{}, Logger: logger}
		j := adoptcovScanJob()
		if _, err := h.ExecuteScan(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want the agent failure")
		}
	})

	t.Run("a listing that fails fails the scan", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		rt := adoptcovRuntime()
		rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
			return nil, fmt.Errorf("daemon busy")
		}
		h := &Adoption{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, Logger: logger}
		j := adoptcovScanJob()
		if _, err := h.ExecuteScan(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want the listing failure")
		}
	})

	t.Run("a container removed between the list and the inspect is skipped", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		rt := adoptcovRuntime()
		rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return containertypes.InspectResponse{}, adoptcovNotFound()
		}
		h := &Adoption{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, Logger: logger}
		j := adoptcovScanJob()
		result, err := h.ExecuteScan(ctx, j, queue.NewStepRecorder(q, j))
		if err != nil {
			t.Fatal(err)
		}
		if result.(map[string]any)["candidates"] != 0 {
			t.Fatalf("result = %#v, want an empty inventory", result)
		}
	})

	t.Run("an inspect that fails for any other reason fails the scan", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		rt := adoptcovRuntime()
		rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return containertypes.InspectResponse{}, fmt.Errorf("daemon busy")
		}
		h := &Adoption{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, Logger: logger}
		j := adoptcovScanJob()
		if _, err := h.ExecuteScan(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want the inspect failure")
		}
	})

	t.Run("the live-resource lookup failing fails the scan", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["ListLiveResourceUUIDs"] = fmt.Errorf("db gone")
		rt := adoptcovRuntime()
		rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return adoptcovInspect(map[string]string{"akerdock.resource_uuid": jobFixtureUUID}), nil
		}
		h := &Adoption{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, Logger: logger}
		j := adoptcovScanJob()
		if _, err := h.ExecuteScan(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want the lookup failure")
		}
	})

	t.Run("a scan that cannot be stored is a job failure", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["CompleteAdoptionScan"] = fmt.Errorf("db gone")
		h := &Adoption{Store: q, Keyring: keyring, Docker: fixedSource{rt: adoptcovRuntime()}, Logger: logger}
		j := adoptcovScanJob()
		if _, err := h.ExecuteScan(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want the write failure")
		}
	})
}

// imageEnvs only asks about the images it will actually diff against: a
// compose member or an already-managed container is not one of them.
func TestAdoptcovImageEnvs(t *testing.T) {
	q, keyring, logger, _ := prevjobsDeps(t)
	inspected := 0
	rt := adoptcovRuntime()
	rt.ImageInspectFn = func(_ context.Context, image string, _ ...client.ImageInspectOption) (imagetypes.InspectResponse, error) {
		inspected++
		if image == "missing/image" {
			return imagetypes.InspectResponse{}, fmt.Errorf("no such image")
		}
		return imagetypes.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{
			ImageConfig: ocispec.ImageConfig{Env: []string{"BASE=1"}},
		}}, nil
	}
	h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
	containers := []adoptionmodel.Inspect{
		{Config: adoptionmodel.InspectConfig{Image: "stack/web", Labels: map[string]string{
			"com.docker.compose.project": "stack",
		}}},
		{Config: adoptionmodel.InspectConfig{Image: "managed/api", Labels: map[string]string{
			"akerdock.resource_uuid": jobFixtureUUID,
		}}},
		{Config: adoptionmodel.InspectConfig{Image: "missing/image", Labels: map[string]string{}}},
		{Config: adoptionmodel.InspectConfig{Image: "example/api:1", Labels: map[string]string{}}},
	}
	envs := h.imageEnvs(context.Background(), rt, containers, map[string]bool{jobFixtureUUID: true})
	if inspected != 2 {
		t.Fatalf("%d images inspected, want only the two unmanaged standalone ones", inspected)
	}
	if len(envs) != 1 || len(envs["example/api:1"]) != 1 {
		t.Fatalf("envs = %#v", envs)
	}
}

// The one scan step that still needs SSH. A server that will not answer does
// not fail the scan: every stack reports its own unreadable definition.
func TestAdoptcovComposeFilesUnreachableServer(t *testing.T) {
	q, keyring, logger, _ := prevjobsDeps(t)
	h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
	containers := []adoptionmodel.Inspect{
		{Config: adoptionmodel.InspectConfig{Labels: map[string]string{
			"com.docker.compose.project":              "shop",
			"com.docker.compose.project.config_files": "/srv/compose.yml",
		}}},
		// Same project, already resolved: the label is read once per stack.
		{Config: adoptionmodel.InspectConfig{Labels: map[string]string{
			"com.docker.compose.project":              "shop",
			"com.docker.compose.project.config_files": "/srv/other.yml",
		}}},
		// Managed by this instance: not part of the unmanaged remainder.
		{Config: adoptionmodel.InspectConfig{Labels: map[string]string{
			"com.docker.compose.project": "managed",
			"akerdock.resource_uuid":     jobFixtureUUID,
		}}},
	}
	// The private key does not decrypt, so the dial fails before any network.
	files := h.composeFiles(context.Background(), store.Server{ID: 1},
		containers, map[string]bool{jobFixtureUUID: true})
	if len(files) != 1 || !strings.Contains(files["shop"].Err, "server unreachable over SSH") {
		t.Fatalf("files = %#v", files)
	}
}

// A compose definition the server refuses to read is reported per stack, with
// the reason on the stack rather than as a failed scan.
func TestAdoptcovComposeFilesUnreadableDefinition(t *testing.T) {
	q, keyring, logger, db := prevjobsDeps(t)
	material, err := sshkey.GenerateEd25519("adoptcov")
	if err != nil {
		t.Fatal(err)
	}
	db.blobs["GetPrivateKeyByID"] = prevjobsEncrypt(t, keyring,
		"private_keys", "private_key_enc", []byte(material.PrivatePEM))
	host, port := deployrunNewSSHServer(t, func(string) (string, uint32) {
		return "", 1 // head(1) refuses: no such file
	}).address(t)
	h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
	server := store.Server{
		ID: 1, Uuid: mustUUID(t, jobFixtureUUID), Host: host, Port: int32(port),
		SshUser: "unit", SshTimeoutSeconds: 5, PrivateKeyID: 1,
	}
	containers := []adoptionmodel.Inspect{
		{Config: adoptionmodel.InspectConfig{Labels: map[string]string{
			"com.docker.compose.project":              "shop",
			"com.docker.compose.project.config_files": "/srv/compose.yml",
		}}},
	}
	files := h.composeFiles(context.Background(), server, containers, nil)
	if len(files) != 1 || files["shop"].Content != "" {
		t.Fatalf("files = %#v, want the read reported as an error", files)
	}
}

// ExecuteAdopt's own refusals — the ones that happen before a single row is
// written.
func TestAdoptcovAdoptRefusals(t *testing.T) {
	ctx := context.Background()
	adoptJob := func(candidateID string) store.Job {
		return store.Job{ID: 73, JobType: TypeAdoptionAdopt, Payload: []byte(
			`{"scan_id":1,"environment_id":1,"items":[{"candidate_id":"` + candidateID + `"}]}`)}
	}
	// A completed scan carrying one adoptable standalone container.
	completed := func(t *testing.T) (*Adoption, *store.Queries, *prevjobsDB) {
		t.Helper()
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["AdoptionScanStatus"] = string(store.AdoptionScanStatusCompleted)
		db.blobs["GetAdoptionScanByID"] = []byte(`[{"id":"api","kind":"container","proposed_name":"api",
			"adoptable":true,"containers":[{"container_id":"0123456789ab","container_name":"legacy-api"}]}]`)
		h := &Adoption{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: adoptcovRuntime()},
		}
		return h, q, db
	}

	t.Run("an invalid payload never reaches the server", func(t *testing.T) {
		h, q, _ := completed(t)
		j := store.Job{ID: 73, JobType: TypeAdoptionAdopt, Payload: []byte(`{`)}
		if _, err := h.ExecuteAdopt(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want a payload error")
		}
	})

	t.Run("a scan that is not completed cannot be adopted from", func(t *testing.T) {
		h, q, db := completed(t)
		db.enums["AdoptionScanStatus"] = string(store.AdoptionScanStatusRunning)
		j := adoptJob("api")
		if _, err := h.ExecuteAdopt(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "not completed") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("candidates that cannot be decoded", func(t *testing.T) {
		h, q, db := completed(t)
		db.blobs["GetAdoptionScanByID"] = []byte("not json")
		j := adoptJob("api")
		if _, err := h.ExecuteAdopt(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "scan candidates") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a server without a default destination", func(t *testing.T) {
		h, q, db := completed(t)
		db.errs["GetDefaultDestination"] = pgx.ErrNoRows
		j := adoptJob("api")
		if _, err := h.ExecuteAdopt(ctx, j, queue.NewStepRecorder(q, j)); err == nil ||
			!strings.Contains(err.Error(), "no default destination") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("no agent channel means nothing can be re-inspected", func(t *testing.T) {
		h, q, _ := completed(t)
		h.Docker = unavailableDocker{}
		j := adoptJob("api")
		if _, err := h.ExecuteAdopt(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want the agent failure")
		}
	})

	t.Run("a container gone since the scan asks for a re-scan", func(t *testing.T) {
		h, q, _ := completed(t)
		rt := adoptcovRuntime()
		rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return containertypes.InspectResponse{}, adoptcovNotFound()
		}
		h.Docker = fixedSource{rt: rt}
		j := adoptJob("api")
		_, err := h.ExecuteAdopt(ctx, j, queue.NewStepRecorder(q, j))
		if err == nil || !strings.Contains(err.Error(), "re-scan before adopting") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an inspect that fails for another reason stops the adoption", func(t *testing.T) {
		h, q, _ := completed(t)
		rt := adoptcovRuntime()
		rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
			return containertypes.InspectResponse{}, fmt.Errorf("daemon busy")
		}
		h.Docker = fixedSource{rt: rt}
		j := adoptJob("api")
		if _, err := h.ExecuteAdopt(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want the inspect failure")
		}
	})
}

// The row mapping of a standalone container: every insert can refuse, and a
// domain already routed by this instance is a warning, not a failure — the
// container is adopted either way, it just keeps its old routing.
func TestAdoptcovContainerRowFailures(t *testing.T) {
	ctx := context.Background()
	candidate := adoptionmodel.Candidate{Containers: []adoptionmodel.Container{{
		ContainerID: "0123456789ab", ContainerName: "legacy-api",
		Ports:   []adoptionmodel.Port{{ContainerPort: 8080, Protocol: "tcp"}},
		Mounts:  []adoptionmodel.Mount{{Kind: "volume", Source: "legacy.data", Destination: "/var/lib/data"}},
		Domains: []string{"legacy.example.test"},
	}}}
	current := adoptionmodel.Inspect{
		State:  adoptionmodel.InspectState{Status: "running"},
		Config: adoptionmodel.InspectConfig{Image: "example/api:1.2"},
	}
	env := map[string]string{"A": "one"}

	for _, query := range []string{
		"CreateApplicationRow", "CreateBuildConfig", "CreateRuntimeConfig",
		"CreateEnvVar", "CreateAdoptedStorage", "CreateDomain",
	} {
		t.Run("a refused "+query+" stops the mapping", func(t *testing.T) {
			q, keyring, logger, db := prevjobsDeps(t)
			db.errs[query] = fmt.Errorf("insert refused")
			h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
			if _, err := h.adoptContainerRows(ctx, q, store.Resource{ID: 9}, candidate, current, env); err == nil {
				t.Fatalf("%s failure was swallowed", query)
			}
		})
	}

	t.Run("a domain already routed here is a warning", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["CreateDomain"] = adoptcovUnique()
		h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
		warnings, err := h.adoptContainerRows(ctx, q, store.Resource{ID: 9}, candidate, current, env)
		if err != nil {
			t.Fatal(err)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "legacy.example.test") {
			t.Fatalf("warnings = %v", warnings)
		}
	})
}

// The same for a compose stack, whose components carry their own domains.
func TestAdoptcovStackRowFailures(t *testing.T) {
	ctx := context.Background()
	candidate := adoptionmodel.Candidate{
		ComposeContent: "services:\n  web:\n    image: nginx:alpine\n",
		Containers: []adoptionmodel.Container{{
			ContainerName: "legacy-web", ComposeService: "web", Image: "nginx:alpine",
			Ports:   []adoptionmodel.Port{{ContainerPort: 80, Protocol: "tcp"}},
			Domains: []string{"www.example.test"},
		}},
	}

	for _, query := range []string{
		"CreateApplicationRow", "CreateBuildConfig", "CreateRuntimeConfig",
		"CreateServiceRow", "UpsertServiceComponent", "CreateComponentDomain",
	} {
		t.Run("a refused "+query+" stops the mapping", func(t *testing.T) {
			q, keyring, logger, db := prevjobsDeps(t)
			db.errs[query] = fmt.Errorf("insert refused")
			h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
			if _, err := h.adoptStackRows(ctx, q, store.Resource{ID: 11}, candidate); err == nil {
				t.Fatalf("%s failure was swallowed", query)
			}
		})
	}

	t.Run("a component domain already routed here is a warning", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["CreateComponentDomain"] = adoptcovUnique()
		h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
		warnings, err := h.adoptStackRows(ctx, q, store.Resource{ID: 11}, candidate)
		if err != nil {
			t.Fatal(err)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "www.example.test") {
			t.Fatalf("warnings = %v", warnings)
		}
	})
}

// Disown is the reverse of adoption and destroys nothing: it detaches routing
// and tombstones the row. Every failure below leaves the resource owned —
// there is no half-released state.
func TestAdoptcovDisownFailures(t *testing.T) {
	ctx := context.Background()
	job := store.Job{ID: 72, JobType: TypeResourceDisown, Payload: []byte(`{"resource_id":1}`)}

	t.Run("an invalid payload never reaches the server", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
		j := store.Job{ID: 72, JobType: TypeResourceDisown, Payload: []byte(`{`)}
		if _, err := h.ExecuteDisown(ctx, j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want a payload error")
		}
	})

	t.Run("a resource that is already gone is a no-op", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["GetResourceByID"] = pgx.ErrNoRows
		h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
		result, err := h.ExecuteDisown(ctx, job, queue.NewStepRecorder(q, job))
		if err != nil || result.(map[string]any)["status"] != "already gone" {
			t.Fatalf("result = %#v, err = %v", result, err)
		}
	})

	for _, query := range []string{"GetDestinationByID", "GetServerByID", "SoftDeleteResource"} {
		t.Run("a failing "+query+" stops the release", func(t *testing.T) {
			q, keyring, logger, db := prevjobsDeps(t)
			db.enums["ProxyType"] = string(store.ProxyTypeNone)
			db.errs[query] = fmt.Errorf("db gone")
			h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
			if _, err := h.ExecuteDisown(ctx, job, queue.NewStepRecorder(q, job)); err == nil {
				t.Fatalf("%s failure was swallowed", query)
			}
		})
	}

	t.Run("routing that cannot be detached releases nothing", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["ProxyType"] = string(store.ProxyTypeTraefik)
		db.errs["CreateProxyRevision"] = fmt.Errorf("db gone")
		h := &Adoption{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: fixedHost{ops: &hostfake.Ops{}},
		}
		// Routing is detached first and only routing (§20.6 order): a detach
		// that did not happen must leave the resource owned.
		if _, err := h.ExecuteDisown(ctx, job, queue.NewStepRecorder(q, job)); err == nil {
			t.Fatal("a routing that did not detach must not release the resource")
		}
	})

	t.Run("no agent channel means the routing cannot be detached", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["ProxyType"] = string(store.ProxyTypeTraefik)
		h := &Adoption{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: unavailableDocker{}, HostOps: unavailableHost{},
		}
		if _, err := h.ExecuteDisown(ctx, job, queue.NewStepRecorder(q, job)); err == nil {
			t.Fatal("want the agent failure")
		}
	})

	t.Run("the host seam missing stops it just as squarely", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["ProxyType"] = string(store.ProxyTypeTraefik)
		h := &Adoption{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: &fake.Runtime{}}, HostOps: unavailableHost{},
		}
		if _, err := h.ExecuteDisown(ctx, job, queue.NewStepRecorder(q, job)); err == nil {
			t.Fatal("want the host-ops failure")
		}
	})
}
