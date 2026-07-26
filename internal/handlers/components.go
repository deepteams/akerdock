// Components of a compose stack (compose-spec.md, data dictionary §9.2):
// the per-service sub-containers of an application built with the compose
// build pack, synchronized by the deployment engine.
package handlers

import (
	"net/http"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

// buildPackToAPI renders the stored build pack for the contract; the image
// pack is not a build pack in the API (it is the docker_image source type).
func buildPackToAPI(pack store.BuildPack) *api.ApplicationBuildPack {
	switch pack {
	case store.BuildPackDockerfile, store.BuildPackNixpacks, store.BuildPackStatic, store.BuildPackCompose:
		return ptr(api.ApplicationBuildPack(pack))
	default:
		return nil
	}
}

func componentToAPI(c store.ServiceComponent) api.ServiceComponent {
	out := api.ServiceComponent{
		Uuid:           ptr(uuidString(c.Uuid)),
		Name:           ptr(c.Name),
		Image:          c.Image,
		IsDatabase:     ptr(c.IsDatabase),
		ExcludeFromHc:  ptr(c.ExcludeFromHc),
		ObservedStatus: api.ObservedStatus(c.ObservedStatus),
		ObservedAt:     timePtr(c.ObservedAt),
		CreatedAt:      timePtr(c.CreatedAt),
	}
	if c.DatabaseEngine != nil {
		out.DatabaseEngine = ptr(api.ServiceComponentDatabaseEngine(*c.DatabaseEngine))
	}
	return out
}

// ListApplicationComponents implements GET /applications/{uuid}/components
// (permission: read). Empty for non-compose build packs — an application
// with one container has no sub-components to report.
func (a *API) ListApplicationComponents(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	id, ok := a.require(w, r, auth.PermApplicationsRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	components, err := a.Store.ListServiceComponents(r.Context(), row.Resource.ID)
	if err != nil {
		a.internalError(w, r, "list components", err)
		return
	}
	data := make([]api.ServiceComponent, 0, len(components))
	for _, c := range components {
		data = append(data, componentToAPI(c))
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}
