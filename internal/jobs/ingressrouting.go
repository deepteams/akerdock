// Ingress endpoint routing (ADR-060): converge the endpoint's pre-provisioned
// Traefik router and the agent's ingress host table with the declaration.
// Runs at declaration, on an access-mode change, and after deletion — never
// at tunnel open/close, which touch no Traefik file at all.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/deepteams/akerdock/internal/agent"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeIngressRouting converges one ingress endpoint's routing.
const TypeIngressRouting = "ingress.apply_routing"

// IngressRoutingPayload carries the UUID and server redundantly with the row:
// after a deletion the row is gone, and the file to remove is named by the
// UUID on a server the payload must still know.
type IngressRoutingPayload struct {
	EndpointID   int64  `json:"endpoint_id"`
	EndpointUUID string `json:"endpoint_uuid"`
	ServerID     int64  `json:"server_id"`
}

// IngressRouting deposits (or removes) the endpoint's dynamic file through
// the ordinary apply/verify cycle, and keeps the agent's ingress host table
// in step.
type IngressRouting struct {
	Store            *store.Queries
	Docker           dockerruntime.Source
	HostOps          hostops.Source
	Logger           *slog.Logger
	ControlPlanePort int
}

// Execute converges the endpoint's routing with its current declaration.
func (h *IngressRouting) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload IngressRoutingPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	server, err := h.Store.GetServerByID(ctx, payload.ServerID)
	if err != nil {
		return nil, err
	}
	if server.ProxyType != store.ProxyTypeTraefik {
		return map[string]any{"status": "server has no managed proxy"}, nil
	}
	dest, err := h.Store.GetDefaultDestination(ctx, server.ID)
	if err != nil {
		return nil, fmt.Errorf("server has no default destination: %w", err)
	}

	rec.Start(ctx, "ingress_routing")
	rt, err := h.Docker.Runtime(ctx, server.ID)
	if err != nil {
		rec.Fail(ctx, "the server's agent is not connected")
		return nil, err
	}
	ops, err := h.HostOps.HostOps(ctx, server.ID)
	if err != nil {
		rec.Fail(ctx, "the server's agent is not connected")
		return nil, err
	}
	applier := &ProxyApplier{Store: h.Store, Docker: rt, Host: ops, Server: server, Network: dest.Network}

	endpoint, err := h.Store.GetIngressEndpointByID(ctx, payload.EndpointID)
	if err != nil {
		// Deleted: remove the router and the agent's host entry. The live
		// session, if any, was already cut over the command channel.
		if err := applier.Apply(ctx, payload.EndpointUUID, "", ""); err != nil {
			rec.Fail(ctx, err.Error())
			return nil, err
		}
		cfg := removeIngressRouteEntry(readWakerConfig(ctx, ops), payload.EndpointUUID)
		if err := depositWakerRoutes(ctx, ops, cfg); err != nil {
			rec.Fail(ctx, err.Error())
			return nil, err
		}
		rec.Succeed(ctx, "ingress routing removed")
		return map[string]any{"status": "removed"}, nil
	}

	access, err := ingressAccessPolicy(ctx, h.Store, endpoint, server, h.ControlPlanePort)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	group := proxy.IngressGroup{
		UUID:   pguuid.String(endpoint.Uuid),
		FQDN:   endpoint.Fqdn,
		Access: access,
	}
	// Same DNS-01 rule as applications (§7.2): a route under the server's
	// wildcard rides the wildcard certificate; otherwise per-router HTTP-01.
	if server.WildcardDomain != nil && *server.WildcardDomain != "" && server.DnsCredentialID != nil {
		if cred, err := h.Store.GetDNSCredentialByID(ctx, *server.DnsCredentialID); err == nil {
			group.WildcardDomain, group.DNSProvider = *server.WildcardDomain, cred.Provider
		}
	}
	content := proxy.GenerateIngress(group, int64(endpoint.Version))
	expect := fmt.Sprintf("http://%s:%d", proxy.AgentContainerName, proxy.AgentPort)
	if err := applier.Apply(ctx, group.UUID, content, expect); err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	cfg := mergeIngressRouteEntry(readWakerConfig(ctx, ops), endpoint.Fqdn, group.UUID)
	if err := depositWakerRoutes(ctx, ops, cfg); err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	rec.Succeed(ctx, "ingress routing converged")
	return map[string]any{"endpoint_uuid": group.UUID, "fqdn": endpoint.Fqdn}, nil
}

// ingressAccessPolicy resolves the endpoint's wall into the proxy IR
// (ADR-060 §5). Fails closed, like resourceAccessPolicy: a claimed wall that
// cannot be rendered is an error, never a publicly rendered endpoint.
func ingressAccessPolicy(ctx context.Context, q *store.Queries, e store.IngressEndpoint, server store.Server, controlPlanePort int) (*proxy.AccessPolicy, error) {
	switch e.Access {
	case store.IngressAccessNone:
		return nil, nil
	case store.IngressAccessBasicAuth:
		if e.BasicAuthHash == nil || *e.BasicAuthHash == "" {
			return nil, fmt.Errorf("ingress basic_auth has no configured credentials")
		}
		return &proxy.AccessPolicy{Mode: "basic_auth", BasicAuthHash: *e.BasicAuthHash}, nil
	case store.IngressAccessSso:
		settings, err := q.GetInstanceSettings(ctx)
		if err != nil {
			return nil, err
		}
		if settings.Fqdn == nil || *settings.Fqdn == "" {
			return nil, fmt.Errorf("ingress sso protection requires the instance FQDN — set it in the instance settings")
		}
		baseURL := "https://" + *settings.Fqdn
		if server.IsLocalhost && controlPlanePort > 0 {
			baseURL = fmt.Sprintf("http://host.docker.internal:%d", controlPlanePort)
		}
		return &proxy.AccessPolicy{
			Mode:           "sso",
			ForwardAuthURL: baseURL + "/webhooks/ingress/forward-auth?endpoint=" + pguuid.String(e.Uuid),
			CallbackURL:    baseURL,
			CallbackPath:   "/.akerdock/ingress-callback",
		}, nil
	default:
		return nil, fmt.Errorf("unsupported ingress access %q", e.Access)
	}
}

// mergeIngressRouteEntry replaces the endpoint's entry in the shared table.
func mergeIngressRouteEntry(base agent.Config, host, endpointUUID string) agent.Config {
	out := removeIngressRouteEntry(base, endpointUUID)
	out.Ingress = append(out.Ingress, agent.IngressRoute{Host: host, EndpointUUID: endpointUUID})
	return out
}

// removeIngressRouteEntry drops the endpoint's entry, leaving the waker's own
// routes and the other endpoints intact.
func removeIngressRouteEntry(base agent.Config, endpointUUID string) agent.Config {
	out := base
	out.Ingress = nil
	for _, r := range base.Ingress {
		if r.EndpointUUID != endpointUUID {
			out.Ingress = append(out.Ingress, r)
		}
	}
	return out
}
