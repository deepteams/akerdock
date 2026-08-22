package jobs

// The ADR-077 relay, at the three joints that carry it: the syncer that
// rebuilds an origin's relay file on its edge, the hook that refreshes it
// after every routing apply, and the trust computation the origin's static
// config consumes.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/agentwire"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

const edgeOriginUUID = "9f8e7d6c-5b4a-4392-8110-ffeeddccbbaa"

// edgeOrigin is a LAN-only server relaying through edge server id 1.
func edgeOrigin() store.Server {
	one := int64(1)
	return store.Server{
		ID: 7, Uuid: pguuid.MustParse(edgeOriginUUID), Host: "192.168.10.20",
		ProxyHttpPort: 80, ProxyHttpsPort: 443, EdgeServerID: &one,
	}
}

// shortVerify lets the applier's first poll decide instead of a 30 s loop.
func shortVerify(t *testing.T) {
	t.Helper()
	previous := verifyTimeout
	verifyTimeout = time.Second
	t.Cleanup(func() { verifyTimeout = previous })
}

func TestEdgeSyncerSyncBuildsTheRelayOnTheEdge(t *testing.T) {
	shortVerify(t)
	q, _, logger, db := prevjobsDeps(t)
	db.rows["ListServerRelayFQDNs"] = 1 // one fqdn, filled "unit"
	edgeOps := &hostfake.Ops{}
	scope := proxy.EdgeScope(edgeOriginUUID)
	syncer := &EdgeSyncer{
		Store:  q,
		Docker: fixedSource{rt: verifyRuntime(`{"` + scope + `": {}}`)},
		Host:   fixedHost{ops: edgeOps},
		Logger: logger,
	}

	if err := syncer.Sync(context.Background(), edgeOrigin()); err != nil {
		t.Fatal(err)
	}
	writes := edgeOps.CallsTo(agentwire.MethodFileWrite)
	if len(writes) != 1 {
		t.Fatalf("edge writes = %v", writes)
	}
	w := writes[0].(agentwire.FileWriteParams)
	if w.Path != "/var/lib/akerdock/proxy/dynamic/"+scope+".yaml" || !w.Atomic {
		t.Fatalf("edge write = %+v", w)
	}
	content := string(w.Content)
	// The relay's whole contract in four substrings: SNI matched without
	// terminating, PROXY protocol toward the origin, the 80 hop for ACME,
	// and the origin's address as the only backend.
	for _, want := range []string{
		"HostSNI(`unit`)", "passthrough: true", "proxyProtocol:\n          version: 2",
		`address: "192.168.10.20:443"`, "Host(`unit`)", `url: "http://192.168.10.20:80"`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("relay file misses %q:\n%s", want, content)
		}
	}
}

func TestEdgeSyncerSyncWithoutEdgeOrFQDNs(t *testing.T) {
	shortVerify(t)
	q, _, logger, db := prevjobsDeps(t)

	t.Run("a server serving its own routes is a no-op", func(t *testing.T) {
		edgeOps := &hostfake.Ops{}
		syncer := &EdgeSyncer{Store: q, Docker: fixedSource{rt: verifyRuntime("{}")}, Host: fixedHost{ops: edgeOps}, Logger: logger}
		origin := edgeOrigin()
		origin.EdgeServerID = nil
		if err := syncer.Sync(context.Background(), origin); err != nil {
			t.Fatal(err)
		}
		if calls := edgeOps.CallsTo(agentwire.MethodFileWrite); len(calls) != 0 {
			t.Fatalf("a no-edge server wrote a relay file: %v", calls)
		}
	})

	t.Run("no public fqdn removes the relay file", func(t *testing.T) {
		db.rows["ListServerRelayFQDNs"] = 0
		t.Cleanup(func() { delete(db.rows, "ListServerRelayFQDNs") })
		edgeOps := &hostfake.Ops{}
		// The verification must see the scope ABSENT for a removal to pass.
		syncer := &EdgeSyncer{Store: q, Docker: fixedSource{rt: verifyRuntime("{}")}, Host: fixedHost{ops: edgeOps}, Logger: logger}
		if err := syncer.Sync(context.Background(), edgeOrigin()); err != nil {
			t.Fatal(err)
		}
		removes := edgeOps.CallsTo(agentwire.MethodFileRemove)
		if len(removes) != 1 {
			t.Fatalf("removes = %v", removes)
		}
	})
}

// The hook: every successful apply on an edge-routed origin refreshes the
// relay — and the edge's own files never re-trigger it (no recursion).
func TestProxyApplierSyncsTheEdgeAfterApply(t *testing.T) {
	shortVerify(t)
	q, _, logger, db := prevjobsDeps(t)
	db.rows["ListServerRelayFQDNs"] = 1
	scope := proxy.EdgeScope(edgeOriginUUID)

	edgeOps := &hostfake.Ops{}
	applier := &ProxyApplier{
		Store:  q,
		Docker: verifyRuntime(`{"app-scope": {}}`),
		Host:   &hostfake.Ops{},
		Server: edgeOrigin(),
		Edge: &EdgeSyncer{
			Store:  q,
			Docker: fixedSource{rt: verifyRuntime(`{"` + scope + `": {}}`)},
			Host:   fixedHost{ops: edgeOps},
			Logger: logger,
		},
	}
	if err := applier.Apply(context.Background(), "app-scope", "routing: yes", ""); err != nil {
		t.Fatal(err)
	}
	if writes := edgeOps.CallsTo(agentwire.MethodFileWrite); len(writes) != 1 {
		t.Fatalf("the apply did not refresh the edge relay: %v", writes)
	}

	// An apply of the relay file itself must not re-enter the syncer.
	edgeOps2 := &hostfake.Ops{}
	applier.Docker = verifyRuntime(`{"` + scope + `": {}}`)
	applier.Edge.Host = fixedHost{ops: edgeOps2}
	if err := applier.Apply(context.Background(), scope, "routing: yes", ""); err != nil {
		t.Fatal(err)
	}
	if writes := edgeOps2.CallsTo(agentwire.MethodFileWrite); len(writes) != 0 {
		t.Fatalf("the edge scope recursed into the syncer: %v", writes)
	}
}

func TestEdgeSyncJobExecute(t *testing.T) {
	shortVerify(t)
	ctx := context.Background()

	t.Run("designation change removes from the former edge and syncs the new one", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		// The origin row must carry an edge designation.
		db.fillPtr["GetServerByID"] = true
		db.rows["ListServerRelayFQDNs"] = 1
		ops := &hostfake.Ops{}
		// The removal from the former edge verifies ABSENCE first, the sync on
		// the new one verifies PRESENCE second — the scripted runtime answers
		// in that order.
		h := &EdgeSync{Store: q, Docker: fixedSource{rt: verifyRuntime(`{}`, `{"`+proxy.EdgeScope(jobFixtureUUID)+`": {}}`)}, HostOps: fixedHost{ops: ops}, Logger: logger}
		job := store.Job{ID: 1, JobType: TypeEdgeSync, Payload: []byte(`{"server_id":7,"remove_from_server_id":3}`)}
		if _, err := h.Execute(ctx, job, queue.NewStepRecorder(q, job)); err != nil {
			t.Fatal(err)
		}
		// One removal (former edge) + one write (current edge).
		if r, w := ops.CallsTo(agentwire.MethodFileRemove), ops.CallsTo(agentwire.MethodFileWrite); len(r) != 1 || len(w) != 1 {
			t.Fatalf("removes = %v, writes = %v", r, w)
		}
	})

	t.Run("an invalid payload never reaches a server", func(t *testing.T) {
		q, _, logger, _ := prevjobsDeps(t)
		h := &EdgeSync{Store: q, Docker: fixedSource{}, HostOps: fixedHost{}, Logger: logger}
		bad := store.Job{ID: 1, JobType: TypeEdgeSync, Payload: []byte(`{`)}
		if _, err := h.Execute(ctx, bad, queue.NewStepRecorder(q, bad)); err == nil {
			t.Fatal("want a payload error")
		}
	})
}

func TestEdgeTrustedIPs(t *testing.T) {
	q, _, _, db := prevjobsDeps(t)
	ctx := context.Background()

	t.Run("no edge means no trust stanza", func(t *testing.T) {
		ips, err := edgeTrustedIPs(ctx, q, store.Server{})
		if err != nil || ips != nil {
			t.Fatalf("ips = %v, %v", ips, err)
		}
	})

	t.Run("an edge declared by IP is taken verbatim", func(t *testing.T) {
		db.strs["GetServerByID"] = "192.168.10.2"
		t.Cleanup(func() { delete(db.strs, "GetServerByID") })
		one := int64(1)
		ips, err := edgeTrustedIPs(ctx, q, store.Server{EdgeServerID: &one})
		if err != nil || len(ips) != 1 || ips[0] != "192.168.10.2" {
			t.Fatalf("ips = %v, %v", ips, err)
		}
	})

	t.Run("an unresolvable hostname fails loudly, naming the fix", func(t *testing.T) {
		// .invalid is guaranteed to never resolve (RFC 2606). Silence here
		// would break the relay in the worst way: the edge prepends a PROXY
		// header the origin then reads as a corrupt TLS record.
		db.strs["GetServerByID"] = "edge.invalid"
		t.Cleanup(func() { delete(db.strs, "GetServerByID") })
		one := int64(1)
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err := edgeTrustedIPs(ctx, q, store.Server{EdgeServerID: &one})
		if err == nil || !strings.Contains(err.Error(), "ADR-077") {
			t.Fatalf("err = %v", err)
		}
	})
}
