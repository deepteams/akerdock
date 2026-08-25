package jobs

// ADR-077 — the edge relay. A server whose `edge_server_id` is set has its
// public routes answered by that edge: a TCP SNI-passthrough file on the
// edge's proxy, rebuilt WHOLE from placements on every routing apply of the
// origin, so the file can never drift from what the control plane knows. The
// pieces here are the rebuild (EdgeSyncer), the origin-side trust computation
// (edgeTrustedIPs, consumed by the proxy bootstrap's static config), and the
// small job that reacts to the operator changing the designation itself.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sort"

	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// edgeTrustedIPs resolves the addresses of a server's designated edge, for
// the origin's static-config trust stanzas (proxy.GenerateStatic). PROXY
// protocol trust wants addresses, not names: an edge declared by IP is taken
// verbatim, a hostname is resolved here — and a hostname that resolves to
// nothing fails LOUDLY, because silently omitting the stanza would break the
// relay in the worst way (the edge prepends a PROXY header the origin then
// reads as a corrupt TLS record).
func edgeTrustedIPs(ctx context.Context, q *store.Queries, server store.Server) ([]string, error) {
	if server.EdgeServerID == nil {
		return nil, nil
	}
	edge, err := q.GetServerByID(ctx, *server.EdgeServerID)
	if err != nil {
		return nil, fmt.Errorf("edge server fetch: %w", err)
	}
	if ip := net.ParseIP(edge.Host); ip != nil {
		return []string{edge.Host}, nil
	}
	addrs, err := (&net.Resolver{}).LookupHost(ctx, edge.Host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("the edge server's host %q does not resolve — the relay needs the edge's "+
			"address for PROXY protocol trust (ADR-077): declare the edge server with an IP address, or fix DNS (%v)",
			edge.Host, err)
	}
	sort.Strings(addrs)
	return addrs, nil
}

// EdgeSyncer rebuilds an origin server's relay file on its designated edge.
// It is carried by ProxyApplier so every routing apply on an edge-routed
// origin refreshes the edge as a side effect — the origin's own file and the
// edge's stay coherent because they are derived from the same event.
type EdgeSyncer struct {
	Store  *store.Queries
	Docker dockerruntime.Source
	Host   hostops.Source
	Logger *slog.Logger
}

// Sync recomputes the origin's public FQDNs and applies the relay file on its
// edge — or removes it when nothing routes anymore. No-op for a server that
// serves its own routes.
func (s *EdgeSyncer) Sync(ctx context.Context, origin store.Server) error {
	if origin.EdgeServerID == nil {
		return nil
	}
	edge, err := s.Store.GetServerByID(ctx, *origin.EdgeServerID)
	if err != nil {
		return fmt.Errorf("edge server fetch: %w", err)
	}
	fqdns, err := s.Store.ListServerRelayFQDNs(ctx, origin.ID)
	if err != nil {
		return fmt.Errorf("relay fqdn list: %w", err)
	}
	content := ""
	if len(fqdns) > 0 {
		content = proxy.GenerateEdge(pguuid.String(origin.Uuid), origin.Host,
			int(origin.ProxyHttpPort), int(origin.ProxyHttpsPort), fqdns, origin.ID)
	}
	return s.apply(ctx, edge, pguuid.String(origin.Uuid), content)
}

// Remove deletes the origin's relay file from one edge — the OLD edge, when
// the operator re-designates or clears it; Sync only ever talks to the
// current one.
func (s *EdgeSyncer) Remove(ctx context.Context, edgeID int64, originUUID string) error {
	edge, err := s.Store.GetServerByID(ctx, edgeID)
	if err != nil {
		return fmt.Errorf("former edge server fetch: %w", err)
	}
	return s.apply(ctx, edge, originUUID, "")
}

func (s *EdgeSyncer) apply(ctx context.Context, edge store.Server, originUUID, content string) error {
	rt, err := s.Docker.Runtime(ctx, edge.ID)
	if err != nil {
		return fmt.Errorf("edge agent channel: %w", err)
	}
	ops, err := s.Host.HostOps(ctx, edge.ID)
	if err != nil {
		return fmt.Errorf("edge agent channel: %w", err)
	}
	// No Network: the relay targets a LAN address, which the proxy container
	// reaches through ordinary outbound NAT — there is no Docker network to
	// join. The applier is the plain checksummed-revision path (§6.2–6.4);
	// scope carries the reserved edge prefix, so the sync-after-apply hook
	// inside Apply never recurses on it.
	applier := &ProxyApplier{Store: s.Store, Docker: rt, Host: ops, Server: edge}
	return applier.Apply(ctx, proxy.EdgeScope(originUUID), content, "")
}

// TypeEdgeSync reacts to the operator changing a server's edge designation
// (PATCH /servers): remove the relay file from the edge that no longer serves
// it, rebuild it on the one that now does. Route-driven refreshes never ride
// this job — they happen inline in ProxyApplier.Apply.
const TypeEdgeSync = "server.edge_sync"

// EdgeSyncPayload references the origin server, and optionally the edge it
// LEFT (UUIDs never carry secrets; IDs are fine inside the queue, INV-003).
type EdgeSyncPayload struct {
	ServerID int64 `json:"server_id"`
	// RemoveFromServerID is the former edge, when the designation moved or
	// was cleared: its copy of the relay file is stale the moment the row
	// changed.
	RemoveFromServerID *int64 `json:"remove_from_server_id,omitempty"`
}

// EdgeSync is the queue handler wrapping EdgeSyncer for designation changes.
type EdgeSync struct {
	Store   *store.Queries
	Docker  dockerruntime.Source
	HostOps hostops.Source
	Logger  *slog.Logger
}

// Execute converges the relay files after an edge designation change.
func (h *EdgeSync) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload EdgeSyncPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	origin, err := h.Store.GetServerByID(ctx, payload.ServerID)
	if err != nil {
		//nolint:nilerr // a deleted server relays nothing: an expected no-op.
		return map[string]any{"status": "server deleted, nothing to do"}, nil
	}
	syncer := &EdgeSyncer{Store: h.Store, Docker: h.Docker, Host: h.HostOps, Logger: h.Logger}

	if payload.RemoveFromServerID != nil {
		rec.Start(ctx, "remove_stale_relay")
		if err := syncer.Remove(ctx, *payload.RemoveFromServerID, pguuid.String(origin.Uuid)); err != nil {
			rec.Fail(ctx, firstLine(err.Error()))
			return nil, err
		}
		rec.Succeed(ctx, "former edge no longer relays this server")
	}

	if origin.EdgeServerID == nil {
		return map[string]any{"status": "no edge designated"}, nil
	}
	rec.Start(ctx, "sync_relay")
	if err := syncer.Sync(ctx, origin); err != nil {
		rec.Fail(ctx, firstLine(err.Error()))
		return nil, err
	}
	rec.Succeed(ctx, "edge relay file converged")
	return map[string]any{"status": "synced"}, nil
}
