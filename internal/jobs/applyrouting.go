package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/accessroute"
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
	Store            *store.Queries
	Keyring          *envelope.Keyring
	Logger           *slog.Logger
	ControlPlanePort int
}

// Execute converges the routing file with the current domains set.
func (h *ApplyRouting) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload ApplyRoutingPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	var service *store.Service
	app, err := h.Store.GetApplicationByID(ctx, payload.ResourceID)
	if err != nil {
		resource, resourceErr := h.Store.GetResourceByID(ctx, payload.ResourceID)
		if resourceErr != nil {
			return map[string]any{"status": "resource deleted, nothing to do"}, nil
		}
		if resource.ResourceType != store.ResourceTypeService {
			return map[string]any{"status": "resource is not routable"}, nil
		}
		stack, stackErr := h.Store.GetServiceByID(ctx, payload.ResourceID)
		if stackErr != nil {
			return nil, stackErr
		}
		service = &stack
		app = store.GetApplicationByIDRow{Resource: resource}
		app.BuildConfig.BuildPack = store.BuildPackCompose
		app.RuntimeConfig.ForceHttps = true
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
	access, err := resourceAccessPolicy(ctx, h.Store, h.Keyring, app, service, server, h.ControlPlanePort)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	content, err := RenderRoutingFile(ctx, h.Store, app, payload.Revision, access)
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
func RenderRoutingFile(ctx context.Context, q *store.Queries, app store.GetApplicationByIDRow, revision int64, access *proxy.AccessPolicy) (string, error) {
	return RenderRoutingFileTo(ctx, q, app, revision, "", access)
}

// RenderRoutingFileTo targets an explicit endpoint — the candidate IP during
// a rolling switch (§7.2 step 2), the container name once stable (step 7).
func RenderRoutingFileTo(ctx context.Context, q *store.Queries, app store.GetApplicationByIDRow, revision int64, endpoint string, access *proxy.AccessPolicy) (string, error) {
	return RenderRoutingFileWithComponentEndpoints(ctx, q, app, revision, endpoint, nil, access)
}

// RenderRoutingFileWithComponentEndpoints additionally points named compose
// components at explicit endpoints — the candidate IP during a per-service
// zero-downtime switch (compose-spec §8.2 step 4).
func RenderRoutingFileWithComponentEndpoints(ctx context.Context, q *store.Queries, app store.GetApplicationByIDRow, revision int64, endpoint string, componentEndpoints map[string]string, access *proxy.AccessPolicy) (string, error) {
	rg, ok, err := applicationRouteGroup(ctx, q, app, endpoint, componentEndpoints)
	if err != nil || !ok {
		return "", err
	}
	rg.Access = access
	return proxy.GenerateDynamic(rg, revision), nil
}

// applicationRouteGroup builds an application's RouteGroup (domains, compose
// component targets, wildcard/DNS resolver) before rendering. ok is false when
// the app has no routable domain. Exposed so scale-to-zero can repoint the group
// at the waker (ADR-037) while reusing the exact same routing resolution.
func applicationRouteGroup(ctx context.Context, q *store.Queries, app store.GetApplicationByIDRow, endpoint string, componentEndpoints map[string]string) (proxy.RouteGroup, bool, error) {
	domains, err := q.ListDomainsForApplication(ctx, &app.Resource.ID)
	if err != nil {
		return proxy.RouteGroup{}, false, err
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
	applicationPublicRoutes, err := decodeStoredPublicRoutes(app.Application.AccessPublicRoutes)
	if err != nil {
		return proxy.RouteGroup{}, false, fmt.Errorf("decode application public routes: %w", err)
	}
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
		return proxy.RouteGroup{}, false, err
	}
	for _, d := range domains {
		port := defaultPort
		if d.TargetPort != nil {
			port = int(*d.TargetPort)
		}
		route := proxy.Route{
			FQDN: d.Fqdn, Path: d.Path, TargetPort: port,
			PublicRoutes: applicationPublicRoutes,
		}
		if len(components) > 0 {
			c, err := resolveWebComponent(components, d.TargetPort)
			if err != nil {
				return proxy.RouteGroup{}, false, fmt.Errorf("domain %s: %w", d.Fqdn, err)
			}
			if d.TargetPort == nil && c.DefaultRoutePort != nil {
				route.TargetPort = int(*c.DefaultRoutePort)
			}
			route.Endpoint = appUUID + "-" + c.Name
			route.PublicRoutes, err = decodeStoredPublicRoutes(c.AccessPublicRoutes)
			if err != nil {
				return proxy.RouteGroup{}, false, fmt.Errorf("decode public routes for component %s: %w", c.Name, err)
			}
			if override, ok := componentEndpoints[c.Name]; ok && override != "" {
				route.Endpoint = override
			}
		}
		rg.Routes = append(rg.Routes, route)
	}
	// Compose stacks route per component (compose-spec §6): each domain of a
	// component targets that component's own container.
	if err := appendComponentRoutes(ctx, q, components, appUUID, &rg, componentEndpoints); err != nil {
		return proxy.RouteGroup{}, false, err
	}
	if len(rg.Routes) == 0 {
		return proxy.RouteGroup{}, false, nil
	}
	return rg, true, nil
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
			publicRoutes, err := decodeStoredPublicRoutes(c.AccessPublicRoutes)
			if err != nil {
				return fmt.Errorf("decode public routes for component %s: %w", c.Name, err)
			}
			rg.Routes = append(rg.Routes, proxy.Route{
				FQDN: d.Fqdn, Path: d.Path, TargetPort: port,
				Endpoint: endpoint, PublicRoutes: publicRoutes,
			})
		}
	}
	return nil
}

func decodeStoredPublicRoutes(raw []byte) ([]accessroute.Route, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var routes []accessroute.Route
	if err := json.Unmarshal(raw, &routes); err != nil {
		return nil, err
	}
	for i, route := range routes {
		normalized, err := accessroute.Validate(route)
		if err != nil {
			return nil, fmt.Errorf("route %d: %w", i, err)
		}
		routes[i] = normalized
	}
	return routes, nil
}

// RenderPreviewRoutingFile builds the Traefik dynamic file of ONE preview
// instance (§20.4.4): its fqdn routes to the preview's own container, behind
// the application's protection policy — basic auth by default, and always
// `X-Robots-Tag: noindex`: a preview is not content to index.
func RenderPreviewRoutingFile(app store.GetApplicationByIDRow, preview store.Preview, revision int64, endpoint, basicAuthHash, ssoAuthURL string) (string, error) {
	rg, ok, err := previewSingleRouteGroup(app, preview, endpoint)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil // no fqdn resolved: the preview runs unrouted
	}
	return renderPreviewContent(rg, pguuid.String(preview.Uuid), revision,
		app.Application.PreviewProtection, basicAuthHash, ssoAuthURL, []string{*preview.Fqdn}), nil
}

// previewSingleRouteGroup builds the RouteGroup of a single-container preview:
// one route (preview.Fqdn) to the preview's own container, on the port from
// PortsExposes or the route table's first row (ADR-035). ok is false when no
// fqdn is resolved (the preview runs unrouted).
func previewSingleRouteGroup(app store.GetApplicationByIDRow, preview store.Preview, endpoint string) (proxy.RouteGroup, bool, error) {
	if preview.Fqdn == nil || *preview.Fqdn == "" {
		return proxy.RouteGroup{}, false, nil
	}
	previewUUID := pguuid.String(preview.Uuid)
	if endpoint == "" {
		endpoint = previewUUID
	}
	publicRoutes, err := decodeStoredPublicRoutes(app.Application.AccessPublicRoutes)
	if err != nil {
		return proxy.RouteGroup{}, false, fmt.Errorf("decode preview public routes: %w", err)
	}
	port := 80
	if p := app.RuntimeConfig.PortsExposes; p != nil {
		first, _, _ := strings.Cut(*p, ",")
		if n, err := strconv.Atoi(strings.TrimSpace(first)); err == nil {
			port = n
		}
	}
	if templates := previewTemplates(app); len(templates) > 0 && templates[0].Port != nil {
		port = *templates[0].Port
	}
	return proxy.RouteGroup{
		AppUUID: previewUUID, Endpoint: endpoint, ForceHTTPS: true,
		Routes: []proxy.Route{{
			FQDN: *preview.Fqdn, Path: "/", TargetPort: port, PublicRoutes: publicRoutes,
		}},
	}, true, nil
}

// renderPreviewContent renders a preview's dynamic file from its RouteGroup and
// attaches the protection policy — basic auth by default, always noindex, and
// the SSO cookie-bootstrap when enabled. Shared by the direct and the
// scale-to-zero (waker-pointed) routing, whose RouteGroups differ only in the
// service target.
func renderPreviewContent(rg proxy.RouteGroup, previewUUID string, revision int64, protection store.PreviewProtection, basicAuthHash, ssoAuthURL string, fqdns []string) string {
	rg.Access = previewAccessPolicy(previewUUID, protection, basicAuthHash, ssoAuthURL)
	content := proxy.GenerateDynamic(rg, revision)
	content = injectPreviewNoindex(content, previewUUID)
	if protection == store.PreviewProtectionSso && ssoAuthURL != "" {
		content = injectPreviewSSOCallback(content, previewUUID, fqdns,
			strings.TrimSuffix(ssoAuthURL, "/webhooks/previews/forward-auth"))
	}
	return content
}

// previewAccessPolicy maps the preview-specific protection onto the same proxy
// IR used by production access walls. PublicRoutes can therefore omit only the
// access middleware while retaining every other preview behavior.
func previewAccessPolicy(previewUUID string, protection store.PreviewProtection, basicAuthHash, ssoAuthURL string) *proxy.AccessPolicy {
	switch {
	case protection == store.PreviewProtectionBasicAuth && basicAuthHash != "":
		return &proxy.AccessPolicy{Mode: "basic_auth", BasicAuthHash: basicAuthHash}
	case protection == store.PreviewProtectionSso && ssoAuthURL != "":
		return &proxy.AccessPolicy{
			Mode:           "sso",
			ForwardAuthURL: ssoAuthURL + "?preview=" + previewUUID,
		}
	default:
		return nil
	}
}

// injectPreviewSSOCallback adds the cookie-bootstrap router of the sso mode
// (ADR-030): `/.akerdock/preview-callback` on the PREVIEW's own hosts, routed
// server-side to the control plane (passHostHeader off — the instance's own
// router must match). The token travels in the REQUEST URL, which survives
// every proxy hop — unlike the X-Forwarded-* headers, which intermediate
// entrypoints strip as untrusted. Highest priority and NO auth middleware:
// the callback is what CREATES the authentication.
func injectPreviewSSOCallback(content, previewUUID string, hosts []string, instanceURL string) string {
	if len(hosts) == 0 || instanceURL == "" {
		return content
	}
	rules := make([]string, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, "Host(`"+h+"`)")
	}
	router := fmt.Sprintf(
		"    %s-authcb:\n      entryPoints: [websecure]\n      rule: (%s) && PathPrefix(`/.akerdock/preview-callback`)\n      priority: 1000000\n      service: %s-authcb\n      tls:\n        certResolver: http01\n",
		previewUUID, strings.Join(rules, " || "), previewUUID)
	service := fmt.Sprintf(
		"    %s-authcb:\n      loadBalancer:\n        passHostHeader: false\n        servers:\n          - url: %q\n",
		previewUUID, instanceURL)

	var out []string
	for _, line := range strings.Split(content, "\n") {
		out = append(out, line)
		trimmed := strings.TrimSpace(line)
		if trimmed == "routers:" && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
			out = append(out, strings.TrimRight(router, "\n"))
		}
		if trimmed == "services:" && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
			out = append(out, strings.TrimRight(service, "\n"))
		}
	}
	return strings.Join(out, "\n")
}

// injectPreviewNoindex attaches noindex to every generated https router,
// including public exceptions. It appends to an existing access/wake list
// instead of emitting a duplicate YAML key.
func injectPreviewNoindex(content, previewUUID string) string {
	name := previewUUID + "-noindex"
	definition := fmt.Sprintf("    %s:\n      headers:\n        customResponseHeaders:\n          X-Robots-Tag: noindex\n", name)
	return injectMiddlewares(content, []string{name}, definition)
}

// injectMiddlewares appends names to every https router of the file and defines
// them before the services section. Existing middleware lists are preserved:
// protected preview routers retain access, public routers retain wake, and all
// of them gain noindex.
func injectMiddlewares(content string, names []string, definitions string) string {
	lines := strings.Split(content, "\n")
	insertAfter := map[int]bool{}
	for i, line := range lines {
		if strings.TrimSpace(line) != "entryPoints: [websecure]" {
			continue
		}
		middlewareLine := -1
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "    ") && !strings.HasPrefix(lines[j], "      ") {
				break
			}
			if strings.HasPrefix(lines[j], "      middlewares: [") {
				middlewareLine = j
				break
			}
		}
		if middlewareLine < 0 {
			insertAfter[i] = true
			continue
		}
		lines[middlewareLine] = appendInlineMiddlewares(lines[middlewareLine], names)
	}

	var out []string
	inserted := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "middlewares:" && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   "):
			// The generator's own middlewares section (https-redirect).
			out = append(out, line)
			out = append(out, strings.TrimRight(definitions, "\n"))
			inserted = true
			continue
		case trimmed == "services:" && !inserted:
			out = append(out, "  middlewares:")
			out = append(out, strings.TrimRight(definitions, "\n"))
			inserted = true
		}
		out = append(out, line)
		if insertAfter[i] {
			out = append(out, "      middlewares: ["+strings.Join(names, ", ")+"]")
		}
	}
	return strings.Join(out, "\n")
}

func appendInlineMiddlewares(line string, names []string) string {
	open := strings.Index(line, "[")
	endBracket := strings.LastIndex(line, "]")
	if open < 0 || endBracket < open {
		return line
	}
	existing := strings.TrimSpace(line[open+1 : endBracket])
	seen := map[string]bool{}
	values := make([]string, 0, len(names)+1)
	for _, value := range strings.Split(existing, ",") {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			values = append(values, value)
		}
	}
	for _, name := range names {
		if name != "" && !seen[name] {
			seen[name] = true
			values = append(values, name)
		}
	}
	return line[:open+1] + strings.Join(values, ", ") + line[endBracket:]
}
