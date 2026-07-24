package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeApplyRouting regenerates the proxy routing of an application
// immediately after a domain change (OpenAPI updateApplication: "routage
// régénéré immédiatement").
const TypeApplyRouting = "application.apply_routing"

// ApplyRoutingPayload references the resource whose routing changed.
type ApplyRoutingPayload struct {
	ResourceID int64 `json:"resource_id"`
	Revision   int64 `json:"revision"` // resources.version at enqueue time
}

// ApplyRouting uploads (or removes) the application's Traefik dynamic file
// outside of any deployment.
type ApplyRouting struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
}

// Execute converges the routing file with the current domains set.
func (h *ApplyRouting) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload ApplyRoutingPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	app, err := h.Store.GetApplicationByID(ctx, payload.ResourceID)
	if err != nil {
		return map[string]any{"status": "resource deleted, nothing to do"}, nil
	}
	dest, err := h.Store.GetDestinationByID(ctx, app.Resource.DestinationID)
	if err != nil {
		return nil, err
	}
	server, err := h.Store.GetServerByID(ctx, dest.ServerID)
	if err != nil {
		return nil, err
	}
	if server.ProxyType != store.ProxyTypeTraefik {
		return map[string]any{"status": "server has no managed proxy"}, nil
	}
	key, err := h.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return nil, err
	}
	pem, err := h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return nil, err
	}

	rec.Start(ctx, "apply_routing")
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem), time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
	if err != nil {
		rec.Fail(ctx, "SSH connection failed")
		return nil, err
	}
	defer func() { _ = client.Close() }()

	appUUID := pguuid.String(app.Resource.Uuid)
	content, err := RenderRoutingFile(ctx, h.Store, app, payload.Revision)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	applier := &ProxyApplier{Store: h.Store, Client: client, Server: server, Network: dest.Network}
	if err := applier.Apply(ctx, appUUID, content, ""); err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	rec.Succeed(ctx, "routing converged")
	return map[string]any{"app_uuid": appUUID, "routed": content != ""}, nil
}

// RenderRoutingFile builds the Traefik dynamic file content for the
// application's current domains, targeting the container by name; "" means
// no routing (file removal).
func RenderRoutingFile(ctx context.Context, q *store.Queries, app store.GetApplicationByIDRow, revision int64) (string, error) {
	return RenderRoutingFileTo(ctx, q, app, revision, "")
}

// RenderRoutingFileTo targets an explicit endpoint — the candidate IP during
// a rolling switch (§7.2 step 2), the container name once stable (step 7).
func RenderRoutingFileTo(ctx context.Context, q *store.Queries, app store.GetApplicationByIDRow, revision int64, endpoint string) (string, error) {
	return RenderRoutingFileWithComponentEndpoints(ctx, q, app, revision, endpoint, nil)
}

// RenderRoutingFileWithComponentEndpoints additionally points named compose
// components at explicit endpoints — the candidate IP during a per-service
// zero-downtime switch (compose-spec §8.2 step 4).
func RenderRoutingFileWithComponentEndpoints(ctx context.Context, q *store.Queries, app store.GetApplicationByIDRow, revision int64, endpoint string, componentEndpoints map[string]string) (string, error) {
	domains, err := q.ListDomainsForApplication(ctx, &app.Resource.ID)
	if err != nil {
		return "", err
	}
	defaultPort := 80
	if p := app.RuntimeConfig.PortsExposes; p != nil {
		first, _, _ := strings.Cut(*p, ",")
		if n, err := strconv.Atoi(strings.TrimSpace(first)); err == nil {
			defaultPort = n
		}
	}
	appUUID := pguuid.String(app.Resource.Uuid)
	if endpoint == "" {
		endpoint = appUUID // Docker DNS by container name
	}
	rg := proxy.RouteGroup{AppUUID: appUUID, Endpoint: endpoint, ForceHTTPS: app.RuntimeConfig.ForceHttps}
	// A route under the server's wildcard is issued over DNS-01 (§7.2): a
	// wildcard cannot be validated over HTTP-01, and asking anyway would leave
	// the route serving the self-signed fallback forever, without a word.
	if dest, err := q.GetDestinationByID(ctx, app.Resource.DestinationID); err == nil {
		if server, err := q.GetServerByID(ctx, dest.ServerID); err == nil &&
			server.WildcardDomain != nil && *server.WildcardDomain != "" && server.DnsCredentialID != nil {
			if cred, err := q.GetDNSCredentialByID(ctx, *server.DnsCredentialID); err == nil {
				rg.WildcardDomain, rg.DNSProvider = *server.WildcardDomain, cred.Provider
			}
		}
	}
	// A compose stack has no container named after the application: routes
	// must target a COMPONENT container (compose-spec §6). An application-
	// level domain — the UI's Routing field — is resolved to the stack's web
	// service deterministically; pointing it at the group endpoint would 502
	// against a container that does not exist.
	components, err := q.ListServiceComponents(ctx, app.Resource.ID)
	if err != nil {
		return "", err
	}
	for _, d := range domains {
		port := defaultPort
		if d.TargetPort != nil {
			port = int(*d.TargetPort)
		}
		route := proxy.Route{FQDN: d.Fqdn, Path: d.Path, TargetPort: port}
		if len(components) > 0 {
			c, err := resolveWebComponent(components, d.TargetPort)
			if err != nil {
				return "", fmt.Errorf("domain %s: %w", d.Fqdn, err)
			}
			if d.TargetPort == nil && c.DefaultRoutePort != nil {
				route.TargetPort = int(*c.DefaultRoutePort)
			}
			route.Endpoint = appUUID + "-" + c.Name
			if override, ok := componentEndpoints[c.Name]; ok && override != "" {
				route.Endpoint = override
			}
		}
		rg.Routes = append(rg.Routes, route)
	}
	// Compose stacks route per component (compose-spec §6): each domain of a
	// component targets that component's own container.
	if err := appendComponentRoutes(ctx, q, components, appUUID, &rg, componentEndpoints); err != nil {
		return "", err
	}
	if len(rg.Routes) == 0 {
		return "", nil
	}
	return proxy.GenerateDynamic(rg, revision), nil
}

// resolveWebComponent picks the compose service an application-level domain
// routes to (compose-spec §6): the component whose exposed port matches the
// domain's target port, or the ONLY component exposing a port. Ambiguity is a
// deterministic error naming the fix — never a guessed container.
func resolveWebComponent(components []store.ServiceComponent, targetPort *int32) (store.ServiceComponent, error) {
	var routable []store.ServiceComponent
	for _, c := range components {
		if c.DefaultRoutePort == nil {
			continue
		}
		if targetPort != nil && *c.DefaultRoutePort == *targetPort {
			return c, nil
		}
		routable = append(routable, c)
	}
	if len(routable) == 1 {
		return routable[0], nil
	}
	if len(routable) == 0 {
		return store.ServiceComponent{}, fmt.Errorf("compose_routable_port_unresolved: no service of the stack exposes a port — add `expose` to the web service")
	}
	return store.ServiceComponent{}, fmt.Errorf("compose_routable_component_ambiguous: %d services expose ports — set the domain as fqdn:port with the web service's exposed port", len(routable))
}

// appendComponentRoutes adds the per-component domains of a compose stack.
// The port resolution order is the domain's target_port, then the component's
// default (first `expose`, persisted at validation) — a routed component with
// neither is a deterministic error, never a guessed port (compose-spec §6).
func appendComponentRoutes(ctx context.Context, q *store.Queries, components []store.ServiceComponent, appUUID string, rg *proxy.RouteGroup, endpointOverrides map[string]string) error {
	for _, c := range components {
		domains, err := q.ListServiceComponentDomains(ctx, &c.ID)
		if err != nil {
			return err
		}
		for _, d := range domains {
			port := 0
			switch {
			case d.TargetPort != nil:
				port = int(*d.TargetPort)
			case c.DefaultRoutePort != nil:
				port = int(*c.DefaultRoutePort)
			default:
				return fmt.Errorf("compose_routable_port_unresolved: component %q carries domain %s but exposes no resolvable port", c.Name, d.Fqdn)
			}
			endpoint := appUUID + "-" + c.Name
			if override, ok := endpointOverrides[c.Name]; ok && override != "" {
				endpoint = override
			}
			rg.Routes = append(rg.Routes, proxy.Route{
				FQDN: d.Fqdn, Path: d.Path, TargetPort: port,
				Endpoint: endpoint,
			})
		}
	}
	return nil
}

// RenderPreviewRoutingFile builds the Traefik dynamic file of ONE preview
// instance (§20.4.4): its fqdn routes to the preview's own container, behind
// the application's protection policy — basic auth by default, and always
// `X-Robots-Tag: noindex`: a preview is not content to index.
func RenderPreviewRoutingFile(app store.GetApplicationByIDRow, preview store.Preview, revision int64, endpoint, basicAuthHash string) (string, error) {
	if preview.Fqdn == nil || *preview.Fqdn == "" {
		return "", nil // no fqdn resolved: the preview runs unrouted
	}
	previewUUID := pguuid.String(preview.Uuid)
	if endpoint == "" {
		endpoint = previewUUID
	}
	port := 80
	if p := app.RuntimeConfig.PortsExposes; p != nil {
		first, _, _ := strings.Cut(*p, ",")
		if n, err := strconv.Atoi(strings.TrimSpace(first)); err == nil {
			port = n
		}
	}
	rg := proxy.RouteGroup{
		AppUUID: previewUUID, Endpoint: endpoint, ForceHTTPS: true,
		Routes: []proxy.Route{{FQDN: *preview.Fqdn, Path: "/", TargetPort: port}},
	}
	content := proxy.GenerateDynamic(rg, revision)
	return injectPreviewMiddlewares(content, previewUUID, app.Application.PreviewProtection, basicAuthHash), nil
}

// injectPreviewMiddlewares attaches the preview protection to every https
// router of a generated routing file (§20.4.4): X-Robots-Tag noindex always,
// basic auth when the application asks for it — shared by the
// single-container previews and the compose preview stacks.
func injectPreviewMiddlewares(content, previewUUID string, protection store.PreviewProtection, basicAuthHash string) string {
	middlewares := []string{previewUUID + "-noindex"}
	extra := fmt.Sprintf("    %s-noindex:\n      headers:\n        customResponseHeaders:\n          X-Robots-Tag: noindex\n", previewUUID)
	if protection == store.PreviewProtectionBasicAuth && basicAuthHash != "" {
		// The credentials are the application's generated preview secret
		// (AKERDOCK_PREVIEW_BASIC_AUTH in the preview variable set): stable
		// across previews of the application, readable by the team, never in
		// this file in clear text — Traefik gets the bcrypt hash.
		middlewares = append(middlewares, previewUUID+"-auth")
		extra += fmt.Sprintf("    %s-auth:\n      basicAuth:\n        users:\n          - %q\n", previewUUID, basicAuthHash)
	}

	// Attach the middlewares to the https routers, and define them in the
	// middlewares section — which must exist BEFORE the services section: a
	// definition appended at the end of the file would be parsed as a service
	// and Traefik would reject the whole routing file.
	var out []string
	inserted := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "middlewares:" && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   "):
			// The generator's own middlewares section (https-redirect).
			out = append(out, line)
			out = append(out, strings.TrimRight(extra, "\n"))
			inserted = true
			continue
		case trimmed == "services:" && !inserted:
			out = append(out, "  middlewares:")
			out = append(out, strings.TrimRight(extra, "\n"))
			inserted = true
		}
		out = append(out, line)
		if strings.HasPrefix(trimmed, "entryPoints: [websecure]") {
			out = append(out, "      middlewares: ["+strings.Join(middlewares, ", ")+"]")
		}
	}
	return strings.Join(out, "\n")
}
