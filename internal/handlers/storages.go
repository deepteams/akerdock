package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// volumeNameFormat bounds the logical volume name; the real Docker name is
// derived deterministically from it (INV-011).
var volumeNameFormat = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func storageToAPI(s store.PersistentStorage, resourceUUID string) api.PersistentStorage {
	out := api.PersistentStorage{
		Uuid:        ptr(uuidString(s.Uuid)),
		Kind:        api.PersistentStorageKind(s.Kind),
		Name:        s.Name,
		HostPath:    s.HostPath,
		MountPath:   s.MountPath,
		IsGenerated: ptr(s.IsGenerated),
		CreatedAt:   timePtr(s.CreatedAt),
	}
	if s.Kind == store.StorageKindVolume && s.Name != nil {
		out.DockerVolumeName = ptr(jobs.DockerVolumeName(resourceUUID, *s.Name))
	}
	return out
}

// ListApplicationStorages implements GET /applications/{uuid}/storages
// (permission: read).
func (a *API) ListApplicationStorages(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	rows, err := a.Store.ListStoragesForResource(r.Context(), row.Resource.ID)
	if err != nil {
		a.internalError(w, r, "list storages", err)
		return
	}
	resourceUUID := uuidString(row.Resource.Uuid)
	data := make([]api.PersistentStorage, 0, len(rows))
	for _, s := range rows {
		data = append(data, storageToAPI(s, resourceUUID))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data []api.PersistentStorage `json:"data"`
	}{data})
}

// CreateApplicationStorage implements POST /applications/{uuid}/storages
// (permission: write). The remote volume is created idempotently at the
// next deployment.
func (a *API) CreateApplicationStorage(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.CreateApplicationStorageParams) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	var body api.PersistentStorageCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	var details []api.ErrorDetail
	if !isSafeMountPath(body.MountPath) {
		details = append(details, api.ErrorDetail{Field: ptr("mount_path"), Code: ptr("invalid"), Message: "mount_path must be an absolute path without '..'"})
	}
	kind := store.StorageKind(body.Kind)
	switch body.Kind {
	case api.PersistentStorageCreateKindVolume:
		if body.Name == nil || !volumeNameFormat.MatchString(*body.Name) {
			details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "a volume requires a name matching [A-Za-z0-9][A-Za-z0-9._-]*"})
		}
		body.HostPath = nil
	case api.PersistentStorageCreateKindBind:
		if body.HostPath == nil || !isSafeMountPath(*body.HostPath) {
			details = append(details, api.ErrorDetail{Field: ptr("host_path"), Code: ptr("required"), Message: "a bind mount requires an absolute host_path without '..'"})
		}
		body.Name = nil
	default:
		details = append(details, api.ErrorDetail{Field: ptr("kind"), Code: ptr("out_of_range"), Message: "kind must be volume or bind"})
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create storage", err)
		return
	}
	created, err := a.Store.CreateStorage(r.Context(), store.CreateStorageParams{
		Uuid: u, ResourceID: row.Resource.ID, Kind: kind,
		Name: body.Name, HostPath: body.HostPath, MountPath: body.MountPath,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a storage is already mounted at this path")
			return
		}
		a.internalError(w, r, "create storage", err)
		return
	}
	a.recordAudit(r, id, "storage.create", "application", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, storageToAPI(created, uuidString(row.Resource.Uuid)))
}

// DeleteApplicationStorage implements DELETE
// /applications/{uuid}/storages/{storage_uuid} (permission: write). The
// remote data is never destroyed by this call (INV-008).
func (a *API) DeleteApplicationStorage(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, storageUuid api.StorageUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	var u pgtype.UUID
	if err := u.Scan(storageUuid); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "storage not found")
		return
	}
	s, err := a.Store.GetStorageByUUID(r.Context(), store.GetStorageByUUIDParams{Uuid: u, ResourceID: row.Resource.ID})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "storage not found")
		return
	}
	if rows, err := a.Store.DeleteStorage(r.Context(), s.ID); err != nil || rows == 0 {
		a.internalError(w, r, "delete storage", err)
		return
	}
	a.recordAudit(r, id, "storage.delete", "application", row.Resource.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// isSafeMountPath enforces absolute paths without traversal (§23.3).
func isSafeMountPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.Contains(p, "..") && !strings.ContainsAny(p, " \t\n'\"$`;&|")
}
