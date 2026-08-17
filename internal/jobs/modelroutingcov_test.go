package jobs

// The model routing path (ADR-080 §1 carried by ADR-077): the apply_routing
// job's model branch, the rendered file's shape — container by UUID on the
// fixed engine port, unconditional noindex — and the delete job freeing the
// FQDNs before the workload goes.

import (
	"context"
	"errors"
	"strings"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/jackc/pgx/v5"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// modelDomainRows makes ListDomainsForModel answer n generic rows.
func modelDomainRows(n int) func(string) pgx.Rows {
	return func(sql string) pgx.Rows {
		if strings.Contains(sql, "-- name: ListDomainsForModel ") {
			return &jobFlowRows{remaining: n}
		}
		return nil
	}
}

func TestModelRoutingConvergesADomain(t *testing.T) {
	q, keyring, logger, db := miscjobsDeps(t)
	db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetApplicationByID")
	miscjobsEnum(t, "ResourceType", string(store.ResourceTypeModel))
	db.rows = modelDomainRows(1)
	ops := &hostfake.Ops{}
	h := &ApplyRouting{Store: q, Keyring: keyring, Docker: fixedSource{rt: verifyRuntime(jobFixtureUUID)}, HostOps: fixedHost{ops: ops}, Logger: logger}
	j := miscjobsRoutingJob()
	result, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j))
	if err != nil {
		t.Fatalf("model routing: %v", err)
	}
	out := result.(map[string]any)
	if out["model_uuid"] != jobFixtureUUID || out["routed"] != true {
		t.Fatalf("result = %#v", out)
	}
	writes := ops.CallsTo(agentwire.MethodFileWrite)
	if len(writes) != 1 {
		t.Fatalf("writes = %v", writes)
	}
	content := string(writes[0].(agentwire.FileWriteParams).Content)
	if !strings.Contains(content, jobFixtureUUID+":8000") {
		t.Fatalf("the route must target the model container on the engine port: %s", content)
	}
	if !strings.Contains(content, "noindex") {
		t.Fatalf("noindex is unconditional for a model (ADR-080 §2): %s", content)
	}
}

func TestModelRoutingRemovesWithoutDomains(t *testing.T) {
	q, keyring, logger, db := miscjobsDeps(t)
	db.rowErr = miscjobsFailOn(errors.New("no rows"), "GetApplicationByID")
	miscjobsEnum(t, "ResourceType", string(store.ResourceTypeModel))
	db.rows = modelDomainRows(0)
	ops := &hostfake.Ops{}
	h := &ApplyRouting{Store: q, Keyring: keyring, Docker: fixedSource{rt: verifyRuntime("")}, HostOps: fixedHost{ops: ops}, Logger: logger}
	j := miscjobsRoutingJob()
	result, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j))
	if err != nil {
		t.Fatalf("model routing removal: %v", err)
	}
	if result.(map[string]any)["routed"] != false {
		t.Fatalf("result = %#v", result)
	}
	if removes := ops.CallsTo(agentwire.MethodFileRemove); len(removes) != 1 {
		t.Fatalf("removes = %v", removes)
	}
}

// modelDeleteFixture wires the minimal ModelRun for the delete path — no
// ciphertext needed, delete never renders the command.
func modelDeleteFixture(t *testing.T) (*ModelRun, store.GetModelByIDRow, *fake.Runtime, *prevjobsDB, *hostfake.Ops) {
	t.Helper()
	q, keyring, logger, db := prevjobsDeps(t)
	row := store.GetModelByIDRow{
		Resource: store.Resource{ID: 7, Uuid: pguuid.MustParse(jobFixtureUUID), TeamID: 1, DestinationID: 1},
		Model:    store.Model{ID: 7, Engine: store.InferenceEngineVllm, ServerID: 1},
	}
	// The applier verifies the removal through the proxy container: the fake
	// needs the exec surface too.
	rt := verifyRuntime("")
	rt.ContainerRemoveFn = func(context.Context, string, containertypes.RemoveOptions) error { return nil }
	ops := &hostfake.Ops{}
	h := &ModelRun{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{ops: ops}, Logger: logger}
	return h, row, rt, db, ops
}

// The delete flow: domains rows go first, then the routing file, then the
// container — the FQDNs are freed even though the resource only soft-deletes.
func TestModelDeleteFreesTheRoutingFirst(t *testing.T) {
	h, row, _, db, ops := modelDeleteFixture(t)
	db.rows["ListDomainsForModel"] = 1
	rt := h.Docker.(fixedSource).rt.(*fake.Runtime)
	if err := h.delete(context.Background(), rt, row, jobFixtureUUID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if removes := ops.CallsTo(agentwire.MethodFileRemove); len(removes) != 1 {
		t.Fatalf("the routing file must be removed: %v", removes)
	}
}

// The domains rows must be deleted — a soft-deleted resource must not hold
// the FQDN. Pinned by making that exact statement fail.
func TestModelDeleteInsistsOnFreeingTheFQDN(t *testing.T) {
	h, row, _, db, _ := modelDeleteFixture(t)
	db.rows["ListDomainsForModel"] = 1
	db.errs["DeleteDomainsForModel"] = errors.New("db down")
	rt := h.Docker.(fixedSource).rt.(*fake.Runtime)
	if err := h.delete(context.Background(), rt, row, jobFixtureUUID); err == nil {
		t.Fatal("a failed domain deletion must fail the job, not leak the FQDN")
	}
}

// A model that never had a domain skips the proxy entirely on delete.
func TestModelDeleteWithoutDomainsSkipsTheProxy(t *testing.T) {
	h, row, _, db, ops := modelDeleteFixture(t)
	db.rows["ListDomainsForModel"] = 0
	rt := h.Docker.(fixedSource).rt.(*fake.Runtime)
	if err := h.delete(context.Background(), rt, row, jobFixtureUUID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if calls := ops.CallsTo(agentwire.MethodFileRemove); len(calls) != 0 {
		t.Fatalf("no domain, no proxy call: %v", calls)
	}
}
