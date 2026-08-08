package jobs

import (
	"context"
	"strings"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	adoptionmodel "github.com/deepteams/akerdock/internal/adoption"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

func TestAdoptionScanInventoriesStandaloneContainer(t *testing.T) {
	q, keyring, logger, _ := miscjobsDeps(t)
	rt := &fake.Runtime{}
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return []containertypes.Summary{{ID: "0123456789abcdef"}}, nil
	}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{
			ContainerJSONBase: &containertypes.ContainerJSONBase{
				ID: "0123456789abcdef", Name: "/legacy-api",
				State: &containertypes.State{Status: "running"},
			},
			Config: &containertypes.Config{
				Image: "example/api:1", Env: []string{"BASE=1", "CUSTOM=2"},
				Labels: map[string]string{},
			},
		}, nil
	}
	rt.ImageInspectFn = func(context.Context, string, ...client.ImageInspectOption) (imagetypes.InspectResponse, error) {
		return imagetypes.InspectResponse{Config: &dockerspec.DockerOCIImageConfig{
			ImageConfig: ocispec.ImageConfig{Env: []string{"BASE=1"}},
		}}, nil
	}
	h := &Adoption{
		Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, Logger: logger,
	}
	job := store.Job{ID: 71, JobType: TypeAdoptionScan, Payload: []byte(`{"scan_id":1}`)}
	result, err := h.ExecuteScan(context.Background(), job, queue.NewStepRecorder(q, job))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	out := result.(map[string]any)
	if out["candidates"] != 1 || out["adoptable"] != 1 || out["scan_uuid"] != jobFixtureUUID {
		t.Fatalf("result = %#v", out)
	}
	if calls := rt.CallNames(); len(calls) != 3 || calls[0] != "ContainerList" || calls[1] != "ContainerInspect" || calls[2] != "ImageInspect" {
		t.Fatalf("runtime calls = %v", calls)
	}
}

func TestAdoptionMapsStandaloneContainerRows(t *testing.T) {
	q, keyring, logger, _ := miscjobsDeps(t)
	h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
	resource := store.Resource{ID: 9}
	candidate := adoptionmodel.Candidate{Containers: []adoptionmodel.Container{{
		ContainerID: "0123456789ab", ContainerName: "legacy-api",
		Ports: []adoptionmodel.Port{
			{ContainerPort: 8080, Protocol: "tcp"},
			{ContainerPort: 53, Protocol: "udp"},
		},
		Mounts: []adoptionmodel.Mount{
			{Kind: "volume", Source: "legacy.data", Destination: "/var/lib/data"},
			{Kind: "bind", Source: "/srv/config", Destination: "/etc/app"},
			{Kind: "tmpfs", Destination: "/tmp"},
		},
		Domains: []string{"legacy.example.test"},
	}}}
	current := adoptionmodel.Inspect{
		State:  adoptionmodel.InspectState{Status: "running"},
		Config: adoptionmodel.InspectConfig{Image: "example/api:1.2", Env: []string{"A=one", "MULTI=line1\nline2"}},
	}
	warnings, err := h.adoptContainerRows(context.Background(), q, resource, candidate, current,
		map[string]string{"A": "one", "MULTI": "line1\nline2"})
	if err != nil {
		t.Fatalf("map container: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestAdoptionMapsComposeStackRows(t *testing.T) {
	q, keyring, logger, _ := miscjobsDeps(t)
	h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
	candidate := adoptionmodel.Candidate{
		ComposeContent: "services:\n  web:\n    image: nginx:alpine\n",
		Containers: []adoptionmodel.Container{
			{ContainerName: "sidecar"},
			{
				ContainerName: "legacy-web", ComposeService: "web", Image: "nginx:alpine",
				Ports:   []adoptionmodel.Port{{ContainerPort: 80, Protocol: "tcp"}},
				Domains: []string{"www.example.test", "admin.example.test"},
			},
		},
	}
	warnings, err := h.adoptStackRows(context.Background(), q, store.Resource{ID: 11}, candidate)
	if err != nil {
		t.Fatalf("map stack: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestAdoptionResolvesLiveResourceLabels(t *testing.T) {
	q, keyring, logger, _ := miscjobsDeps(t)
	h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
	containers := []adoptionmodel.Inspect{
		{Config: adoptionmodel.InspectConfig{Labels: map[string]string{"akerdock.resource_uuid": jobFixtureUUID}}},
		{Config: adoptionmodel.InspectConfig{Labels: map[string]string{"akerdock.resource_uuid": jobFixtureUUID}}},
		{Config: adoptionmodel.InspectConfig{Labels: map[string]string{"akerdock.resource_uuid": "not-a-uuid"}}},
	}
	live, err := h.liveResourceUUIDs(context.Background(), containers)
	if err != nil {
		t.Fatalf("live labels: %v", err)
	}
	if !live[jobFixtureUUID] || len(live) != 1 {
		t.Fatalf("live = %v", live)
	}
}

func TestAdoptionReadsComposeFilesOverSSH(t *testing.T) {
	q, keyring, logger, _ := miscjobsDeps(t)
	sshServer := newJobSSHServer(t)
	host, port := sshServer.address(t)
	h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
	server := store.Server{
		ID: 1, Uuid: mustUUID(t, jobFixtureUUID), Host: host, Port: int32(port),
		SshUser: "unit", SshTimeoutSeconds: 2, PrivateKeyID: 1,
	}
	containers := []adoptionmodel.Inspect{
		{Config: adoptionmodel.InspectConfig{Labels: map[string]string{
			"com.docker.compose.project":              "empty-path",
			"com.docker.compose.project.config_files": "",
		}}},
		{Config: adoptionmodel.InspectConfig{Labels: map[string]string{
			"com.docker.compose.project":              "from-file",
			"com.docker.compose.project.config_files": "/srv/docker-compose.yml,/srv/override.yml",
		}}},
	}
	files := h.composeFiles(context.Background(), server, containers, nil)
	if len(files) != 2 || files["empty-path"].Err != "" || files["from-file"].Err != "" {
		t.Fatalf("files = %#v", files)
	}
}

func TestAdoptionDisownsWithoutTouchingRemoteObjects(t *testing.T) {
	q, keyring, logger, _ := miscjobsDeps(t)
	miscjobsEnum(t, "ProxyType", "none")
	h := &Adoption{Store: q, Keyring: keyring, Logger: logger}
	job := store.Job{ID: 72, JobType: TypeResourceDisown, Payload: []byte(`{"resource_id":1}`)}
	result, err := h.ExecuteDisown(context.Background(), job, queue.NewStepRecorder(q, job))
	if err != nil {
		t.Fatalf("disown: %v", err)
	}
	if got := result.(map[string]any)["disowned"]; got != jobFixtureUUID {
		t.Fatalf("result = %#v", result)
	}
}

func TestAdoptionRejectsUnknownAndBlockedCandidates(t *testing.T) {
	q, keyring, logger, db := miscjobsDeps(t)
	db.override = func(sql string, index int, dest any) {
		if strings.Contains(sql, "-- name: GetAdoptionScanByID ") && index == 6 {
			*dest.(*[]byte) = []byte(`[
				{"id":"available","proposed_name":"app","adoptable":true},
				{"id":"blocked","proposed_name":"unsafe","adoptable":false,"reasons":["privileged"]}
			]`)
		}
	}
	h := &Adoption{Store: q, Keyring: keyring, Docker: fixedSource{rt: &fake.Runtime{}}, Logger: logger}

	for _, candidateID := range []string{"missing", "blocked"} {
		job := store.Job{ID: 73, JobType: TypeAdoptionAdopt, Payload: []byte(
			`{"scan_id":1,"environment_id":1,"items":[{"candidate_id":"` + candidateID + `"}]}`,
		)}
		if _, err := h.ExecuteAdopt(context.Background(), job, queue.NewStepRecorder(q, job)); err == nil {
			t.Fatalf("candidate %q unexpectedly accepted", candidateID)
		}
	}
}
