package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

func (a *API) serverToAPI(r *http.Request, s store.Server, privateKeyUUID string, dnsCredentialUUID *string) api.Server {
	var arch *api.ServerArchitecture
	if s.Architecture != nil {
		arch = ptr(api.ServerArchitecture(*s.Architecture))
	}
	// Agent presence (ADR-040/041): live connection from the in-memory
	// registry, durable trace from the token row. Best-effort — a server
	// without a token simply reads as never seen.
	var agentSeen *time.Time
	if token, err := a.Store.GetAgentTokenByServerID(r.Context(), s.ID); err == nil {
		agentSeen = timePtr(token.LastSeenAt)
	}
	// The ADR-077 edge designation, as its public uuid. Best effort like the
	// key: "" would only happen if the edge row vanished mid-request.
	var edgeUUID *string
	if s.EdgeServerID != nil {
		if edge, err := a.Store.GetServerByID(r.Context(), *s.EdgeServerID); err == nil {
			edgeUUID = ptr(uuidString(edge.Uuid))
		}
	}
	var gpuMemoryMB *int
	if s.GpuMemoryMb != nil {
		gpuMemoryMB = ptr(int(*s.GpuMemoryMb))
	}
	return api.Server{
		AgentConnected:    ptr(a.Agents.Connected(s.ID)),
		AgentSeenAt:       agentSeen,
		Uuid:              ptr(uuidString(s.Uuid)),
		Name:              s.Name,
		Description:       s.Description,
		Host:              s.Host,
		Port:              int(s.Port),
		User:              s.SshUser,
		UseSudo:           ptr(s.UseSudo),
		EdgeServerUuid:    edgeUUID,
		PrivateKeyUuid:    privateKeyUUID,
		SshTimeoutSeconds: ptr(int(s.SshTimeoutSeconds)),
		IsBuildServer:     ptr(s.IsBuildServer),
		IsLocalhost:       ptr(s.IsLocalhost),
		WildcardDomain:    s.WildcardDomain,
		DnsCredentialUuid: dnsCredentialUUID,
		ProxyType:         ptr(api.ServerProxyType(s.ProxyType)),
		ProxyHttpPort:     ptr(int(s.ProxyHttpPort)),
		ProxyHttpsPort:    ptr(int(s.ProxyHttpsPort)),
		ProxyDesiredState: ptr(api.ServerProxyDesiredState(s.ProxyDesiredState)),
		// Intent and observation, side by side and never merged: a desired
		// "running" says nothing about what is actually up (§19.2).
		ProxyObservedStatus:  ptr(api.ObservedStatus(s.ProxyObservedStatus)),
		Status:               ptr(api.ServerStatus(s.Status)),
		IsReachable:          ptr(s.Status == store.ServerStatusReady),
		ObservedAt:           timePtr(s.ObservedAt),
		Architecture:         arch,
		DockerVersion:        s.DockerVersion,
		GpuName:              s.GpuName,
		GpuMemoryMb:          gpuMemoryMB,
		CleanupEnabled:       ptr(s.CleanupEnabled),
		CleanupCron:          s.CleanupCron,
		CleanupPruneVolumes:  ptr(s.CleanupPruneVolumes),
		CleanupPruneNetworks: ptr(s.CleanupPruneNetworks),
		CleanupLastRunAt:     timePtr(s.CleanupLastRunAt),
		Version:              ptr(int(s.Version)),
		CreatedAt:            timePtr(s.CreatedAt),
		UpdatedAt:            timePtr(s.UpdatedAt),
	}
}

func (a *API) resolveServer(w http.ResponseWriter, r *http.Request, id *auth.Identity, serverUUID string) (store.Server, bool) {
	u, ok := a.scanUUID(w, r, serverUUID, "server")
	if !ok {
		return store.Server{}, false
	}
	server, err := a.Store.GetServerByUUID(r.Context(), store.GetServerByUUIDParams{Uuid: u, TeamID: id.TeamID})
	return resolveRow(a, w, r, "server", server, err)
}

// privateKeyUUIDByID resolves the public uuid of a server's key for
// responses (best effort — "" only if the key vanished mid-request).
func (a *API) privateKeyUUIDByID(r *http.Request, keyID int64) string {
	key, err := a.Store.GetPrivateKeyByID(r.Context(), keyID)
	if err != nil {
		return ""
	}
	return uuidString(key.Uuid)
}

// resolveEdgeServer resolves and vets an ADR-077 edge designation for origin
// (zero-ID at creation, when self-reference and chains-below are impossible).
// Uniform 404 across the team boundary, named 422s for the eligibility rules:
// an edge must run a managed proxy, and no relay may chain in either
// direction — through an edge that itself relays, or onto a server that
// others already relay through.
func (a *API) resolveEdgeServer(w http.ResponseWriter, r *http.Request, id *auth.Identity, edgeUUID string, origin store.Server) (*int64, bool) {
	edge, ok := a.resolveServer(w, r, id, edgeUUID)
	if !ok {
		return nil, false
	}
	fail := func(message string) (*int64, bool) {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("edge_server_uuid"), Code: ptr("invalid"), Message: message,
		}})
		return nil, false
	}
	if origin.ID != 0 && edge.ID == origin.ID {
		return fail("a server cannot be its own edge")
	}
	if edge.EdgeServerID != nil {
		return fail("the designated edge itself relays through an edge — chains are refused (ADR-077)")
	}
	if edge.ProxyType != store.ProxyTypeTraefik {
		return fail("the designated edge runs no managed proxy — the relay is carried by its Traefik (ADR-077)")
	}
	if origin.ID != 0 {
		if n, err := a.Store.CountServersUsingEdge(r.Context(), &origin.ID); err != nil {
			a.internalError(w, r, "edge designation", err)
			return nil, false
		} else if n > 0 {
			return fail("other servers relay through this one — an edge cannot itself relay (ADR-077)")
		}
	}
	return &edge.ID, true
}

// ListServers implements GET /servers (permission: read).
func (a *API) ListServers(w http.ResponseWriter, r *http.Request, params api.ListServersParams) {
	id, ok := a.require(w, r, auth.PermServersRead)
	if !ok {
		return
	}
	limit, ok := pageLimit(w, r, params.Limit)
	if !ok {
		return
	}
	after, ok := afterID(w, r, params.Cursor)
	if !ok {
		return
	}
	rows, err := a.Store.ListServersPage(r.Context(), store.ListServersPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list servers", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(s store.Server) int64 { return s.ID })

	data := make([]api.Server, 0, len(rows))
	for _, s := range rows {
		data = append(data, a.serverToAPI(r, s, a.privateKeyUUIDByID(r, s.PrivateKeyID), a.dnsCredentialUUIDByID(r, s.DnsCredentialID)))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.Server `json:"data"`
		NextCursor *string      `json:"next_cursor"`
	}{data, cursor})
}

// CreateServer implements POST /servers (permission: write): registers the
// server in pending status; validation is a separate 202 job (§20.1).
func (a *API) CreateServer(w http.ResponseWriter, r *http.Request, params api.CreateServerParams) {
	id, ok := a.require(w, r, auth.PermServersManage)
	if !ok {
		return
	}
	var body api.ServerCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	// A wildcard_domain without a DNS credential is a NAMING template, not a
	// wildcard certificate: each assigned host gets its own HTTP-01
	// certificate, per router (proxy-contract §7.2). The trade-offs (public
	// reachability, per-host CA rate limits) are the operator's to weigh —
	// refusing the combination here would force a DNS provider on setups that
	// neither have nor need one.
	var dnsCredentialID *int64
	if body.DnsCredentialUuid != nil && *body.DnsCredentialUuid != "" {
		cred, ok := a.resolveDNSCredential(w, r, id, *body.DnsCredentialUuid)
		if !ok {
			return
		}
		dnsCredentialID = &cred.ID
	}

	var details []api.ErrorDetail
	if body.Name == "" || len(body.Name) > 255 {
		details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"})
	}
	if strings.TrimSpace(body.Host) == "" {
		details = append(details, api.ErrorDetail{Field: ptr("host"), Code: ptr("required"), Message: "host is required"})
	}
	port := 22
	if body.Port != nil {
		if *body.Port < 1 || *body.Port > 65535 {
			details = append(details, api.ErrorDetail{Field: ptr("port"), Code: ptr("out_of_range"), Message: "port must be between 1 and 65535"})
		} else {
			port = *body.Port
		}
	}
	user := "root"
	if body.User != nil && *body.User != "" {
		user = *body.User
	}
	timeout := 30
	if body.SshTimeoutSeconds != nil {
		if *body.SshTimeoutSeconds < 1 || *body.SshTimeoutSeconds > 300 {
			details = append(details, api.ErrorDetail{Field: ptr("ssh_timeout_seconds"), Code: ptr("out_of_range"), Message: "ssh_timeout_seconds must be between 1 and 300"})
		} else {
			timeout = *body.SshTimeoutSeconds
		}
	}
	proxyType := store.ProxyTypeTraefik
	if body.ProxyType != nil {
		switch *body.ProxyType {
		case api.ServerCreateProxyTypeTraefik:
			proxyType = store.ProxyTypeTraefik
		case api.ServerCreateProxyTypeNone:
			proxyType = store.ProxyTypeNone
		default:
			details = append(details, api.ErrorDetail{Field: ptr("proxy_type"), Code: ptr("out_of_range"), Message: "proxy_type must be traefik or none (Caddy arrives in P2)"})
		}
	}
	httpPort, httpsPort := 80, 443
	if body.ProxyHttpPort != nil {
		httpPort = *body.ProxyHttpPort
	}
	if body.ProxyHttpsPort != nil {
		httpsPort = *body.ProxyHttpsPort
	}
	for field, p := range map[string]int{"proxy_http_port": httpPort, "proxy_https_port": httpsPort} {
		if p < 1 || p > 65535 {
			details = append(details, api.ErrorDetail{Field: ptr(field), Code: ptr("out_of_range"), Message: field + " must be between 1 and 65535"})
		}
	}

	// The key must belong to the same team (INV-002) — same uniform 404
	// semantics as any cross-team reference.
	var key store.PrivateKey
	var keyUUID pgtype.UUID
	if err := keyUUID.Scan(body.PrivateKeyUuid); err != nil {
		details = append(details, api.ErrorDetail{Field: ptr("private_key_uuid"), Code: ptr("invalid"), Message: "unknown private key"})
	} else {
		var err error
		key, err = a.Store.GetPrivateKeyByUUID(r.Context(), store.GetPrivateKeyByUUIDParams{Uuid: keyUUID, TeamID: ptr(id.TeamID)})
		if err != nil {
			details = append(details, api.ErrorDetail{Field: ptr("private_key_uuid"), Code: ptr("invalid"), Message: "unknown private key"})
		}
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	var edgeServerID *int64
	if body.EdgeServerUuid != nil && *body.EdgeServerUuid != "" {
		edgeServerID, ok = a.resolveEdgeServer(w, r, id, *body.EdgeServerUuid, store.Server{})
		if !ok {
			return
		}
	}

	isBuild := body.IsBuildServer != nil && *body.IsBuildServer
	useSudo := body.UseSudo != nil && *body.UseSudo
	server, err := a.Store.CreateServer(r.Context(), store.CreateServerParams{
		TeamID: id.TeamID, Name: body.Name, Description: body.Description,
		Host: body.Host, Port: int32(port), SshUser: user, UseSudo: useSudo,
		EdgeServerID:      edgeServerID,
		SshTimeoutSeconds: int32(timeout), PrivateKeyID: key.ID,
		IsBuildServer: isBuild, WildcardDomain: body.WildcardDomain,
		DnsCredentialID: dnsCredentialID,
		ProxyType:       proxyType, ProxyHttpPort: int32(httpPort), ProxyHttpsPort: int32(httpsPort),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a server with this name already exists in this team")
			return
		}
		a.internalError(w, r, "create server", err)
		return
	}
	w.Header().Set("ETag", etagFor(server.Version))
	httpapi.WriteJSON(w, http.StatusCreated, a.serverToAPI(r, server, uuidString(key.Uuid), a.dnsCredentialUUIDByID(r, server.DnsCredentialID)))
}

// GetServer implements GET /servers/{server_uuid} (permission: read).
func (a *API) GetServer(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid) {
	id, ok := a.require(w, r, auth.PermServersRead)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(server.Version))
	httpapi.WriteJSON(w, http.StatusOK, a.serverToAPI(r, server, a.privateKeyUUIDByID(r, server.PrivateKeyID), a.dnsCredentialUUIDByID(r, server.DnsCredentialID)))
}

// UpdateServer implements PATCH /servers/{server_uuid} (permission: write).
// Sensitive PATCH — If-Match mandatory; changing host, port, user or key
// puts the server back in pending (revalidation required).
func (a *API) UpdateServer(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid, params api.UpdateServerParams) {
	id, ok := a.require(w, r, auth.PermServersManage)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	expected, err := strconv.Atoi(strings.Trim(strings.TrimSpace(params.IfMatch), `"`))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid If-Match header")
		return
	}

	var body api.ServerUpdate
	patch, ok := decodePatch(w, r, &body)
	if !ok {
		return
	}

	next := server // copy, mutate below
	connectivityChanged := false
	if body.Name != nil {
		next.Name = *body.Name
	}
	if patch.Has("description") {
		next.Description = body.Description
	}
	if body.Host != nil && *body.Host != next.Host {
		next.Host, connectivityChanged = *body.Host, true
	}
	if body.Port != nil && int32(*body.Port) != next.Port {
		if *body.Port < 1 || *body.Port > 65535 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("port"), Code: ptr("out_of_range"), Message: "port must be between 1 and 65535"}})
			return
		}
		next.Port, connectivityChanged = int32(*body.Port), true
	}
	if body.User != nil && *body.User != next.SshUser {
		next.SshUser, connectivityChanged = *body.User, true
	}
	// Not connectivity in the TCP sense, but the same consequence: every
	// remote command changes its execution identity (ADR-076), so nothing
	// proven by the last validation still holds.
	if body.UseSudo != nil && *body.UseSudo != next.UseSudo {
		next.UseSudo, connectivityChanged = *body.UseSudo, true
	}
	// The ADR-077 edge designation. Changing it never revalidates (SSH is
	// untouched) but must converge two things the update itself cannot: the
	// relay file moves off the former edge onto the new one, and the origin's
	// static config gains or drops its PROXY protocol trust — both enqueued
	// after the write succeeds.
	edgeChanged := false
	var formerEdgeID *int64
	if body.EdgeServerUuid != nil {
		if *body.EdgeServerUuid == "" {
			if next.EdgeServerID != nil {
				formerEdgeID, next.EdgeServerID, edgeChanged = next.EdgeServerID, nil, true
			}
		} else {
			edgeID, ok := a.resolveEdgeServer(w, r, id, *body.EdgeServerUuid, server)
			if !ok {
				return
			}
			if next.EdgeServerID == nil || *next.EdgeServerID != *edgeID {
				formerEdgeID, next.EdgeServerID, edgeChanged = next.EdgeServerID, edgeID, true
			}
		}
	}
	if body.SshTimeoutSeconds != nil {
		next.SshTimeoutSeconds = int32(*body.SshTimeoutSeconds)
	}
	if body.PrivateKeyUuid != nil {
		var keyUUID pgtype.UUID
		if err := keyUUID.Scan(*body.PrivateKeyUuid); err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("private_key_uuid"), Code: ptr("invalid"), Message: "unknown private key"}})
			return
		}
		key, err := a.Store.GetPrivateKeyByUUID(r.Context(), store.GetPrivateKeyByUUIDParams{Uuid: keyUUID, TeamID: ptr(id.TeamID)})
		if err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("private_key_uuid"), Code: ptr("invalid"), Message: "unknown private key"}})
			return
		}
		if key.ID != next.PrivateKeyID {
			next.PrivateKeyID, connectivityChanged = key.ID, true
		}
	}
	if body.IsBuildServer != nil {
		next.IsBuildServer = *body.IsBuildServer
	}
	if patch.Has("wildcard_domain") {
		next.WildcardDomain = body.WildcardDomain
	}
	if body.ProxyType != nil {
		switch *body.ProxyType {
		case api.ServerUpdateProxyTypeTraefik:
			next.ProxyType = store.ProxyTypeTraefik
		case api.ServerUpdateProxyTypeNone:
			next.ProxyType = store.ProxyTypeNone
		}
	}
	if body.ProxyHttpPort != nil {
		next.ProxyHttpPort = int32(*body.ProxyHttpPort)
	}
	if body.ProxyHttpsPort != nil {
		next.ProxyHttpsPort = int32(*body.ProxyHttpsPort)
	}

	status := server.Status
	if connectivityChanged {
		status = store.ServerStatusPending
	}
	// Same rule as at creation: a wildcard without a credential falls back to
	// per-host HTTP-01 certificates (proxy-contract §7.2). Removing the
	// credential is therefore allowed — routers switch resolver as their
	// dynamic config is regenerated, i.e. at the next deployment of each app.
	if body.DnsCredentialUuid != nil {
		if *body.DnsCredentialUuid == "" {
			next.DnsCredentialID = nil
		} else {
			cred, ok := a.resolveDNSCredential(w, r, id, *body.DnsCredentialUuid)
			if !ok {
				return
			}
			next.DnsCredentialID = &cred.ID
		}
	}

	// Automated cleanup settings (§3.7). The cron is validated with the SAME
	// parser the scheduler runs: an expression the scheduler cannot fire is
	// refused now, never accepted and then silently never run.
	if body.CleanupEnabled != nil {
		next.CleanupEnabled = *body.CleanupEnabled
	}
	if patch.Has("cleanup_cron") {
		if body.CleanupCron != nil && *body.CleanupCron != "" {
			normalized, valid := normalizeCron(*body.CleanupCron)
			if !valid {
				httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
					Field: ptr("cleanup_cron"), Code: ptr("invalid"),
					Message: "cleanup_cron must be a 5-field cron expression or one of: every_minute, hourly, daily, weekly, monthly, yearly",
				}})
				return
			}
			next.CleanupCron = &normalized
		} else {
			next.CleanupCron = nil
		}
	}
	if patch.Has("cleanup_disk_threshold_pct") {
		if body.CleanupDiskThresholdPct != nil && (*body.CleanupDiskThresholdPct < 1 || *body.CleanupDiskThresholdPct > 100) {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("cleanup_disk_threshold_pct"), Code: ptr("out_of_range"),
				Message: "cleanup_disk_threshold_pct must be between 1 and 100",
			}})
			return
		}
		next.CleanupDiskThresholdPct = nil
		if body.CleanupDiskThresholdPct != nil {
			next.CleanupDiskThresholdPct = ptr(int32(*body.CleanupDiskThresholdPct))
		}
	}
	if body.CleanupPruneVolumes != nil {
		next.CleanupPruneVolumes = *body.CleanupPruneVolumes
	}
	if body.CleanupPruneNetworks != nil {
		next.CleanupPruneNetworks = *body.CleanupPruneNetworks
	}

	rows, err := a.Store.UpdateServer(r.Context(), store.UpdateServerParams{
		ID: server.ID, Name: next.Name, Description: next.Description,
		Host: next.Host, Port: next.Port, SshUser: next.SshUser, UseSudo: next.UseSudo,
		EdgeServerID:      next.EdgeServerID,
		SshTimeoutSeconds: next.SshTimeoutSeconds, PrivateKeyID: next.PrivateKeyID,
		IsBuildServer: next.IsBuildServer, WildcardDomain: next.WildcardDomain,
		DnsCredentialID: next.DnsCredentialID,
		ProxyType:       next.ProxyType, ProxyHttpPort: next.ProxyHttpPort,
		ProxyHttpsPort: next.ProxyHttpsPort, Status: status,
		CleanupEnabled:          next.CleanupEnabled,
		CleanupCron:             next.CleanupCron,
		CleanupDiskThresholdPct: next.CleanupDiskThresholdPct,
		CleanupPruneVolumes:     next.CleanupPruneVolumes,
		CleanupPruneNetworks:    next.CleanupPruneNetworks,
		ExpectedVersion:         int32(expected),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a server with this name already exists in this team")
			return
		}
		a.internalError(w, r, "update server", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, server.Version)
		return
	}

	updated, err := a.Store.GetServerByUUID(r.Context(), store.GetServerByUUIDParams{Uuid: server.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "reload server", err)
		return
	}
	if edgeChanged {
		// Two convergences the row change cannot carry by itself (ADR-077),
		// both best-effort like the routing regeneration above: the relay
		// file leaves the former edge and lands on the new one…
		if _, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
			Queue: "deploy", Type: jobs.TypeEdgeSync,
			Payload: jobs.EdgeSyncPayload{ServerID: server.ID, RemoveFromServerID: formerEdgeID},
			TeamID:  ptr(id.TeamID),
		}); err != nil {
			a.Logger.Warn("failed to enqueue edge relay sync", "error", err)
		}
		// …and the ORIGIN's static config gains or drops its PROXY protocol
		// trust — an entrypoint setting only a new proxy container reads
		// (§1.4), which is exactly what a start converges. Only when the
		// operator's intent is running: an explicitly stopped proxy is never
		// repaired behind their back (ADR-062).
		if updated.ProxyType == store.ProxyTypeTraefik && updated.ProxyDesiredState == store.ProxyDesiredStateRunning {
			if _, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
				Queue: "deploy", Type: jobs.TypeProxyStart,
				Payload: jobs.ProxyLifecyclePayload{ServerID: server.ID, Action: "start"},
				TeamID:  ptr(id.TeamID),
			}); err != nil {
				a.Logger.Warn("failed to enqueue proxy convergence after edge change", "error", err)
			}
		}
	}
	w.Header().Set("ETag", etagFor(updated.Version))
	httpapi.WriteJSON(w, http.StatusOK, a.serverToAPI(r, updated, a.privateKeyUUIDByID(r, updated.PrivateKeyID), a.dnsCredentialUUIDByID(r, updated.DnsCredentialID)))
}

// DeleteServer implements DELETE /servers/{server_uuid} (permission:
// write). INV-008: removes the server from AkerDock, never destroys the
// machine. Refused once resources are deployed on it (with resources).
func (a *API) DeleteServer(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid) {
	id, ok := a.require(w, r, auth.PermServersManage)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	if count, err := a.Store.CountResourcesOnServer(r.Context(), server.ID); err != nil {
		a.internalError(w, r, "delete server", err)
		return
	} else if count > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "dependency_exists", "resources are still deployed on this server — delete them first (INV-008)")
		return
	}
	rows, err := a.Store.SoftDeleteServer(r.Context(), server.ID)
	if err != nil || rows == 0 {
		a.internalError(w, r, "delete server", err)
		return
	}
	a.recordAudit(r, id, "server.delete", "server", server.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// ValidateServer implements POST /servers/{server_uuid}/validate
// (permission: write): long operation — 202 with a tracking job (§20.1).
func (a *API) ValidateServer(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid, params api.ValidateServerParams) {
	id, ok := a.require(w, r, auth.PermServersManage)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}

	lockKey := "server:validate:" + uuidString(server.Uuid)
	if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err != nil {
		a.internalError(w, r, "validate server", err)
		return
	} else if active > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "operation_in_progress", "a validation of this server is already in progress")
		return
	}

	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:          "maintenance",
		Type:           jobs.TypeServerValidate,
		Payload:        jobs.ServerValidatePayload{ServerID: server.ID},
		LockKey:        &lockKey,
		TeamID:         ptr(id.TeamID),
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "enqueue server validation", err)
		return
	}

	a.recordAudit(r, id, "server.validate", "server", server.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

var _ = pguuid.String // referenced via helpers

// dnsCredentialUUIDByID renders which DNS credential a server issues its
// wildcards with. The credential's CONTENT is never exposed; its identity is,
// because an operator has to be able to see what a server is configured with.
func (a *API) dnsCredentialUUIDByID(r *http.Request, id *int64) *string {
	if id == nil {
		return nil
	}
	cred, err := a.Store.GetDNSCredentialByID(r.Context(), *id)
	if err != nil {
		return nil
	}
	return ptr(uuidString(cred.Uuid))
}
