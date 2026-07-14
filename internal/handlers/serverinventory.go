package handlers

import (
	"net/http"
	"strconv"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

// ListServerResources implements GET /servers/{server_uuid}/resources
// (permission: read): the managed resources deployed on the server.
// Unmanaged Docker objects never appear here (INV-015).
func (a *API) ListServerResources(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid, params api.ListServerResourcesParams) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
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
	rows, err := a.Store.ListServerResourcesPage(r.Context(), store.ListServerResourcesPageParams{
		ServerID: server.ID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list server resources", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(row store.ListServerResourcesPageRow) int64 { return row.ID })

	data := make([]api.ServerResource, 0, len(rows))
	for _, row := range rows {
		data = append(data, api.ServerResource{
			Uuid:            uuidString(row.Uuid),
			Type:            api.ServerResourceType(row.ResourceType),
			Name:            row.Name,
			ProjectUuid:     ptr(uuidString(row.ProjectUuid)),
			EnvironmentUuid: ptr(uuidString(row.EnvironmentUuid)),
			Status:          api.ObservedStatus(row.ObservedStatus),
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.ServerResource `json:"data"`
		NextCursor *string              `json:"next_cursor"`
	}{data, cursor})
}

// ListServerDomains implements GET /servers/{server_uuid}/domains
// (permission: read): the domains routed by this server's proxy, grouped
// by resource.
func (a *API) ListServerDomains(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	rows, err := a.Store.ListServerDomains(r.Context(), server.ID)
	if err != nil {
		a.internalError(w, r, "list server domains", err)
		return
	}

	// Group by resource, preserving the query's stable order.
	type group struct {
		kind    string
		domains []string
	}
	order := make([]string, 0, len(rows))
	groups := map[string]*group{}
	for _, row := range rows {
		key := uuidString(row.ResourceUuid)
		g, ok := groups[key]
		if !ok {
			g = &group{kind: string(row.ResourceType)}
			groups[key] = g
			order = append(order, key)
		}
		domain := row.Fqdn
		if row.TargetPort != nil {
			domain += ":" + strconv.Itoa(int(*row.TargetPort))
		}
		if row.Path != "/" {
			domain += row.Path
		}
		g.domains = append(g.domains, domain)
	}

	data := make([]api.ServerDomain, 0, len(order))
	for _, key := range order {
		g := groups[key]
		data = append(data, api.ServerDomain{
			ResourceUuid: key,
			ResourceType: api.ServerDomainResourceType(g.kind),
			Domains:      g.domains,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data []api.ServerDomain `json:"data"`
	}{data})
}
