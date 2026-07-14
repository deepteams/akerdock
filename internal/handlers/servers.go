package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

func serverToAPI(s store.Server, privateKeyUUID string, dnsCredentialUUID *string) api.Server {
	var arch *api.ServerArchitecture
	if s.Architecture != nil {
		arch = ptr(api.ServerArchitecture(*s.Architecture))
	}
	return api.Server{
		Uuid:              ptr(uuidString(s.Uuid)),
		Name:              s.Name,
		Description:       s.Description,
		Host:              s.Host,
		Port:              int(s.Port),
		User:              s.SshUser,
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
	var u pgtype.UUID
	if err := u.Scan(serverUUID); err == nil {
		server, err := a.Store.GetServerByUUID(r.Context(), store.GetServerByUUIDParams{Uuid: u, TeamID: id.TeamID})
		if err == nil {
			return server, true
		}
	}
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "server not found")
	return store.Server{}, false
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

// ListServers implements GET /servers (permission: read).
func (a *API) ListServers(w http.ResponseWriter, r *http.Request, params api.ListServersParams) {
	id, ok := a.require(w, r, auth.PermRead)
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
		data = append(data, serverToAPI(s, a.privateKeyUUIDByID(r, s.PrivateKeyID), a.dnsCredentialUUIDByID(r, s.DnsCredentialID)))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.Server `json:"data"`
		NextCursor *string      `json:"next_cursor"`
	}{data, cursor})
}

// CreateServer implements POST /servers (permission: write): registers the
// server in pending status; validation is a separate 202 job (§20.1).
func (a *API) CreateServer(w http.ResponseWriter, r *http.Request, params api.CreateServerParams) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	var body api.ServerCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	// A wildcard cannot be validated over HTTP-01: the CA has no single host to
	// ask. Accepting a wildcard_domain without a DNS credential would leave
	// every preview URL serving the self-signed fallback, forever, with nothing
	// anywhere saying why (proxy-contract §7.2).
	var dnsCredentialID *int64
	if body.DnsCredentialUuid != nil && *body.DnsCredentialUuid != "" {
		cred, ok := a.resolveDNSCredential(w, r, id, *body.DnsCredentialUuid)
		if !ok {
			return
		}
		dnsCredentialID = &cred.ID
	}

	var details []api.ErrorDetail
	if body.WildcardDomain != nil && *body.WildcardDomain != "" && dnsCredentialID == nil {
		details = append(details, api.ErrorDetail{
			Field: ptr("dns_credential_uuid"), Code: ptr("required"),
			Message: "a wildcard domain needs a DNS-01 credential: a wildcard certificate cannot be issued over HTTP-01 (proxy-contract §7.2)",
		})
	}
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

	isBuild := body.IsBuildServer != nil && *body.IsBuildServer
	server, err := a.Store.CreateServer(r.Context(), store.CreateServerParams{
		TeamID: id.TeamID, Name: body.Name, Description: body.Description,
		Host: body.Host, Port: int32(port), SshUser: user,
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
	httpapi.WriteJSON(w, http.StatusCreated, serverToAPI(server, uuidString(key.Uuid), a.dnsCredentialUUIDByID(r, server.DnsCredentialID)))
}

// GetServer implements GET /servers/{server_uuid} (permission: read).
func (a *API) GetServer(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(server.Version))
	httpapi.WriteJSON(w, http.StatusOK, serverToAPI(server, a.privateKeyUUIDByID(r, server.PrivateKeyID), a.dnsCredentialUUIDByID(r, server.DnsCredentialID)))
}

// UpdateServer implements PATCH /servers/{server_uuid} (permission: write).
// Sensitive PATCH — If-Match mandatory; changing host, port, user or key
// puts the server back in pending (revalidation required).
func (a *API) UpdateServer(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid, params api.UpdateServerParams) {
	id, ok := a.require(w, r, auth.PermWrite)
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
	// Same rule as at creation, and it must hold on a PATCH too: removing the
	// credential while a wildcard stands would leave the renewals to fail, and
	// the failure would surface at expiry (proxy-contract §7.2).
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
	if next.WildcardDomain != nil && *next.WildcardDomain != "" && next.DnsCredentialID == nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("dns_credential_uuid"), Code: ptr("required"),
			Message: "a wildcard domain needs a DNS-01 credential: a wildcard certificate cannot be issued over HTTP-01 (proxy-contract §7.2)",
		}})
		return
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
		Host: next.Host, Port: next.Port, SshUser: next.SshUser,
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
	w.Header().Set("ETag", etagFor(updated.Version))
	httpapi.WriteJSON(w, http.StatusOK, serverToAPI(updated, a.privateKeyUUIDByID(r, updated.PrivateKeyID), a.dnsCredentialUUIDByID(r, updated.DnsCredentialID)))
}

// DeleteServer implements DELETE /servers/{server_uuid} (permission:
// write). INV-008: removes the server from AkerDock, never destroys the
// machine. Refused once resources are deployed on it (with resources).
func (a *API) DeleteServer(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
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
	id, ok := a.require(w, r, auth.PermWrite)
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
