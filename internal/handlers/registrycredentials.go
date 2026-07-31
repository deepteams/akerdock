package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// Private registry credentials (§6.5, amendement de spec n°17).
//
// The password is write-only, at every level: the API accepts it, the keyring
// encrypts it, and nothing ever renders it back — not even to a root token.
// The only thing that can read it is the deployment engine, and the only place
// it goes is the stdin of `docker login --password-stdin` on the target server.

// registryHost is the grammar of what may reach `docker login` (INV-012): a
// host, an optional port, an optional path. Everything else — a space, a
// semicolon, a quote — is refused at the edge rather than quoted and hoped for.
var registryHost = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?(:[0-9]{1,5})?(/[a-zA-Z0-9._/-]+)?$`)

func registryCredentialToAPI(c store.RegistryCredential, inUse bool) api.RegistryCredential {
	return api.RegistryCredential{
		Uuid:        ptr(uuidString(c.Uuid)),
		Name:        c.Name,
		RegistryUrl: c.RegistryUrl,
		Username:    c.Username,
		InUse:       ptr(inUse),
		Version:     ptr(int(c.Version)),
		CreatedAt:   timePtr(c.CreatedAt),
		UpdatedAt:   timePtr(c.UpdatedAt),
	}
}

func (a *API) resolveRegistryCredential(w http.ResponseWriter, r *http.Request, id *auth.Identity, credUUID string) (store.RegistryCredential, bool) {
	u, ok := a.scanUUID(w, r, credUUID, "registry credential")
	if !ok {
		return store.RegistryCredential{}, false
	}
	cred, err := a.Store.GetRegistryCredentialByUUID(r.Context(), store.GetRegistryCredentialByUUIDParams{
		Uuid: u, TeamID: id.TeamID,
	})
	return resolveRow(a, w, r, "registry credential", cred, err)
}

func (a *API) registryInUse(r *http.Request, credID int64) bool {
	n, err := a.Store.CountRegistryCredentialUsage(r.Context(), &credID)
	return err == nil && n > 0
}

// ListRegistryCredentials implements GET /registry-credentials.
func (a *API) ListRegistryCredentials(w http.ResponseWriter, r *http.Request, params api.ListRegistryCredentialsParams) {
	id, ok := a.require(w, r, auth.PermSourcesRead)
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
	rows, err := a.Store.ListRegistryCredentialsPage(r.Context(), store.ListRegistryCredentialsPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list registry credentials", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(c store.RegistryCredential) int64 { return c.ID })

	data := make([]api.RegistryCredential, 0, len(rows))
	for _, c := range rows {
		data = append(data, registryCredentialToAPI(c, a.registryInUse(r, c.ID)))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.RegistryCredential `json:"data"`
		NextCursor *string                  `json:"next_cursor"`
	}{data, cursor})
}

// CreateRegistryCredential implements POST /registry-credentials.
func (a *API) CreateRegistryCredential(w http.ResponseWriter, r *http.Request, params api.CreateRegistryCredentialParams) {
	id, ok := a.require(w, r, auth.PermRegistriesManage)
	if !ok {
		return
	}
	var body api.RegistryCredentialCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	var details []api.ErrorDetail
	if strings.TrimSpace(body.Name) == "" {
		details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must not be empty"})
	}
	if strings.TrimSpace(body.Username) == "" {
		details = append(details, api.ErrorDetail{Field: ptr("username"), Code: ptr("required"), Message: "username must not be empty"})
	}
	if body.Password == "" {
		details = append(details, api.ErrorDetail{Field: ptr("password"), Code: ptr("required"), Message: "password must not be empty"})
	}
	if !registryHost.MatchString(body.RegistryUrl) {
		details = append(details, api.ErrorDetail{
			Field: ptr("registry_url"), Code: ptr("invalid"),
			Message: "registry_url must be a host, with an optional port and path — e.g. ghcr.io or registry.example.com:5000",
		})
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	// The UUID is minted here, not by the database: it is part of the AAD of the
	// envelope encryption (ADR-003), so the ciphertext must be bound to the row
	// it will live in — which means knowing the UUID before encrypting.
	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create registry credential", err)
		return
	}
	enc, err := a.Keyring.Encrypt("registry_credentials", "password_enc", uuidString(u), []byte(body.Password))
	if err != nil {
		a.internalError(w, r, "create registry credential", err)
		return
	}
	cred, err := a.Store.CreateRegistryCredential(r.Context(), store.CreateRegistryCredentialParams{
		Uuid: u, TeamID: id.TeamID, Name: body.Name, RegistryUrl: body.RegistryUrl,
		Username: body.Username, PasswordEnc: enc,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a registry credential with this name already exists in this team")
			return
		}
		a.internalError(w, r, "create registry credential", err)
		return
	}

	a.recordAudit(r, id, "registry_credential.create", "registry_credential", cred.Uuid)
	w.Header().Set("ETag", etagFor(cred.Version))
	httpapi.WriteJSON(w, http.StatusCreated, registryCredentialToAPI(cred, false))
}

// GetRegistryCredential implements GET /registry-credentials/{uuid}.
func (a *API) GetRegistryCredential(w http.ResponseWriter, r *http.Request, registryCredentialUuid api.RegistryCredentialUuid) {
	id, ok := a.require(w, r, auth.PermSourcesRead)
	if !ok {
		return
	}
	cred, ok := a.resolveRegistryCredential(w, r, id, registryCredentialUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(cred.Version))
	httpapi.WriteJSON(w, http.StatusOK, registryCredentialToAPI(cred, a.registryInUse(r, cred.ID)))
}

// UpdateRegistryCredential implements PATCH /registry-credentials/{uuid}.
func (a *API) UpdateRegistryCredential(w http.ResponseWriter, r *http.Request, registryCredentialUuid api.RegistryCredentialUuid, params api.UpdateRegistryCredentialParams) {
	id, ok := a.require(w, r, auth.PermRegistriesManage)
	if !ok {
		return
	}
	cred, ok := a.resolveRegistryCredential(w, r, id, registryCredentialUuid)
	if !ok {
		return
	}
	expected, err := strconv.Atoi(strings.Trim(strings.TrimSpace(params.IfMatch), `"`))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid If-Match header")
		return
	}
	var body api.RegistryCredentialUpdate
	if _, ok := decodePatch(w, r, &body); !ok {
		return
	}
	if body.RegistryUrl != nil && !registryHost.MatchString(*body.RegistryUrl) {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("registry_url"), Code: ptr("invalid"),
			Message: "registry_url must be a host, with an optional port and path",
		}})
		return
	}
	update := store.UpdateRegistryCredentialParams{
		ID: cred.ID, ExpectedVersion: int32(expected),
		Name: body.Name, RegistryUrl: body.RegistryUrl, Username: body.Username,
	}
	if body.Password != nil {
		if *body.Password == "" {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("password"), Code: ptr("invalid"), Message: "password must not be empty",
			}})
			return
		}
		enc, err := a.Keyring.Encrypt("registry_credentials", "password_enc", uuidString(cred.Uuid), []byte(*body.Password))
		if err != nil {
			a.internalError(w, r, "update registry credential", err)
			return
		}
		update.PasswordEnc = enc
	}

	rows, err := a.Store.UpdateRegistryCredential(r.Context(), update)
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a registry credential with this name already exists in this team")
			return
		}
		a.internalError(w, r, "update registry credential", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, cred.Version)
		return
	}
	updated, err := a.Store.GetRegistryCredentialByID(r.Context(), cred.ID)
	if err != nil {
		a.internalError(w, r, "reload registry credential", err)
		return
	}
	// The audit says the password changed, never what it changed to: the audit
	// table is append-only and exportable, so a secret written into it would be
	// a second copy of that secret (INV-003).
	a.recordAudit(r, id, "registry_credential.update", "registry_credential", cred.Uuid)
	w.Header().Set("ETag", etagFor(updated.Version))
	httpapi.WriteJSON(w, http.StatusOK, registryCredentialToAPI(updated, a.registryInUse(r, updated.ID)))
}

// DeleteRegistryCredential implements DELETE /registry-credentials/{uuid}.
func (a *API) DeleteRegistryCredential(w http.ResponseWriter, r *http.Request, registryCredentialUuid api.RegistryCredentialUuid) {
	id, ok := a.require(w, r, auth.PermRegistriesManage)
	if !ok {
		return
	}
	cred, ok := a.resolveRegistryCredential(w, r, id, registryCredentialUuid)
	if !ok {
		return
	}
	// Deleting a credential an application still depends on would leave a
	// deployment that cannot pull its own image — and a rollback that cannot
	// roll back (§19.2).
	if a.registryInUse(r, cred.ID) {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
			"this registry credential is still used by an application or a rollback artifact")
		return
	}
	if _, err := a.Store.SoftDeleteRegistryCredential(r.Context(), cred.ID); err != nil {
		a.internalError(w, r, "delete registry credential", err)
		return
	}
	a.recordAudit(r, id, "registry_credential.delete", "registry_credential", cred.Uuid)
	w.WriteHeader(http.StatusNoContent)
}
