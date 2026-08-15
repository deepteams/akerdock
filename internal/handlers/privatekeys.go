package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/sshkey"
	"github.com/deepteams/akerdock/internal/store"
)

// privateKeyToAPI renders key metadata and the public half — the whole of
// what the API ever serves about a key: private material is write-only for
// the platform's own SSH use (ADR-075). inUse tells whether a server or an
// application's deploy key references the key — a key in use cannot be
// deleted (§19.2).
func privateKeyToAPI(k store.PrivateKey, inUse bool) api.PrivateKey {
	return api.PrivateKey{
		Uuid:        ptr(uuidString(k.Uuid)),
		Name:        k.Name,
		Description: k.Description,
		Fingerprint: ptr(k.FingerprintSha256),
		PublicKey:   ptr(k.PublicKey),
		InUse:       ptr(inUse),
		Version:     ptr(int(k.Version)),
		CreatedAt:   timePtr(k.CreatedAt),
		UpdatedAt:   timePtr(k.UpdatedAt),
	}
}

// keysInUse answers the question for a batch of keys in one round trip. On a
// query error it reports "not in use" rather than failing the read: in_use is
// informational, deletion is guarded by its own check.
func (a *API) keysInUse(r *http.Request, ids []int64) map[int64]bool {
	used := make(map[int64]bool, len(ids))
	if len(ids) == 0 {
		return used
	}
	rows, err := a.Store.ListPrivateKeyIDsInUse(r.Context(), ids)
	if err != nil {
		a.Logger.Warn("could not resolve key usage", "error", err)
		return used
	}
	for _, id := range rows {
		used[id] = true
	}
	return used
}

func (a *API) resolvePrivateKey(w http.ResponseWriter, r *http.Request, id *auth.Identity, keyUUID string) (store.PrivateKey, bool) {
	u, ok := a.scanUUID(w, r, keyUUID, "private key")
	if !ok {
		return store.PrivateKey{}, false
	}
	key, err := a.Store.GetPrivateKeyByUUID(r.Context(), store.GetPrivateKeyByUUIDParams{Uuid: u, TeamID: ptr(id.TeamID)})
	return resolveRow(a, w, r, "private key", key, err)
}

// ListPrivateKeys implements GET /private-keys (permission: read).
// Metadata and public halves only, like every key response (ADR-075).
func (a *API) ListPrivateKeys(w http.ResponseWriter, r *http.Request, params api.ListPrivateKeysParams) {
	id, ok := a.require(w, r, auth.PermKeysRead)
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
	rows, err := a.Store.ListPrivateKeysPage(r.Context(), store.ListPrivateKeysPageParams{
		TeamID: ptr(id.TeamID), AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list private keys", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(k store.PrivateKey) int64 { return k.ID })

	ids := make([]int64, 0, len(rows))
	for _, k := range rows {
		ids = append(ids, k.ID)
	}
	used := a.keysInUse(r, ids)
	data := make([]api.PrivateKey, 0, len(rows))
	for _, k := range rows {
		data = append(data, privateKeyToAPI(k, used[k.ID]))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.PrivateKey `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}{data, cursor})
}

// CreatePrivateKey implements POST /private-keys (permission: write). The
// material is validated, encrypted at rest, and never echoed back (§23.2).
func (a *API) CreatePrivateKey(w http.ResponseWriter, r *http.Request, params api.CreatePrivateKeyParams) {
	id, ok := a.require(w, r, auth.PermKeysManage)
	if !ok {
		return
	}
	var body api.PrivateKeyCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	var details []api.ErrorDetail
	if body.Name == "" || len(body.Name) > 255 {
		details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"})
	}
	if body.PrivateKey == nil || *body.PrivateKey == "" {
		details = append(details, api.ErrorDetail{Field: ptr("private_key"), Code: ptr("required"), Message: "private_key is required"})
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}
	material, err := sshkey.Parse(*body.PrivateKey)
	if err != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("private_key"), Code: ptr("invalid"), Message: sanitizeKeyError(err)}})
		return
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create private key", err)
		return
	}
	enc, err := a.Keyring.Encrypt("private_keys", "private_key_enc", pguuid.String(u), []byte(material.PrivatePEM))
	if err != nil {
		a.internalError(w, r, "create private key", err)
		return
	}
	key, err := a.Store.CreatePrivateKey(r.Context(), store.CreatePrivateKeyParams{
		Uuid: u, TeamID: ptr(id.TeamID), Name: body.Name, Description: body.Description,
		FingerprintSha256: material.Fingerprint, PublicKey: material.PublicKey, PrivateKeyEnc: enc,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a key with the same fingerprint already exists in this team")
			return
		}
		a.internalError(w, r, "create private key", err)
		return
	}
	w.Header().Set("ETag", etagFor(key.Version))
	httpapi.WriteJSON(w, http.StatusCreated, privateKeyToAPI(key, false))
}

// GeneratePrivateKey implements POST /private-keys/generate (permission:
// write). ADR-075: the keypair is born inside the platform — ed25519, no
// algorithm choice — so the private half never exists anywhere else at all.
// The response carries the public key, ready for a server's authorized_keys.
func (a *API) GeneratePrivateKey(w http.ResponseWriter, r *http.Request, params api.GeneratePrivateKeyParams) {
	id, ok := a.require(w, r, auth.PermKeysManage)
	if !ok {
		return
	}
	var body api.PrivateKeyGenerate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	if body.Name == "" || len(body.Name) > 255 {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"}})
		return
	}

	material, err := sshkey.GenerateEd25519(body.Name)
	if err != nil {
		a.internalError(w, r, "generate private key", err)
		return
	}
	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "generate private key", err)
		return
	}
	enc, err := a.Keyring.Encrypt("private_keys", "private_key_enc", pguuid.String(u), []byte(material.PrivatePEM))
	if err != nil {
		a.internalError(w, r, "generate private key", err)
		return
	}
	key, err := a.Store.CreatePrivateKey(r.Context(), store.CreatePrivateKeyParams{
		Uuid: u, TeamID: ptr(id.TeamID), Name: body.Name, Description: body.Description,
		FingerprintSha256: material.Fingerprint, PublicKey: material.PublicKey, PrivateKeyEnc: enc,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a key with the same fingerprint already exists in this team")
			return
		}
		a.internalError(w, r, "generate private key", err)
		return
	}
	w.Header().Set("ETag", etagFor(key.Version))
	httpapi.WriteJSON(w, http.StatusCreated, privateKeyToAPI(key, false))
}

// GetPrivateKey implements GET /private-keys/{uuid} (permission: read).
// There is no reveal: once a key enters the platform its private material
// never leaves it, whatever the permission — only the public half is served
// (ADR-075).
func (a *API) GetPrivateKey(w http.ResponseWriter, r *http.Request, privateKeyUuid api.PrivateKeyUuid) {
	id, ok := a.require(w, r, auth.PermKeysRead)
	if !ok {
		return
	}
	key, ok := a.resolvePrivateKey(w, r, id, privateKeyUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(key.Version))
	httpapi.WriteJSON(w, http.StatusOK, privateKeyToAPI(key, a.keysInUse(r, []int64{key.ID})[key.ID]))
}

// UpdatePrivateKey implements PATCH /private-keys/{uuid} (permission:
// write). Sensitive PATCH — If-Match is mandatory (§24.1); providing
// private_key rotates the material.
func (a *API) UpdatePrivateKey(w http.ResponseWriter, r *http.Request, privateKeyUuid api.PrivateKeyUuid, params api.UpdatePrivateKeyParams) {
	id, ok := a.require(w, r, auth.PermKeysManage)
	if !ok {
		return
	}
	key, ok := a.resolvePrivateKey(w, r, id, privateKeyUuid)
	if !ok {
		return
	}

	expected, err := strconv.Atoi(strings.Trim(strings.TrimSpace(params.IfMatch), `"`))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid If-Match header")
		return
	}

	var body api.PrivateKeyUpdate
	patch, ok := decodePatch(w, r, &body)
	if !ok {
		return
	}

	name, description := key.Name, key.Description
	fingerprint, publicKey, enc := key.FingerprintSha256, key.PublicKey, key.PrivateKeyEnc
	if body.Name != nil {
		if *body.Name == "" || len(*body.Name) > 255 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"}})
			return
		}
		name = *body.Name
	}
	if patch.Has("description") {
		description = body.Description
	}
	if body.PrivateKey != nil {
		material, err := sshkey.Parse(*body.PrivateKey)
		if err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("private_key"), Code: ptr("invalid"), Message: sanitizeKeyError(err)}})
			return
		}
		fingerprint, publicKey = material.Fingerprint, material.PublicKey
		enc, err = a.Keyring.Encrypt("private_keys", "private_key_enc", uuidString(key.Uuid), []byte(material.PrivatePEM))
		if err != nil {
			a.internalError(w, r, "rotate private key", err)
			return
		}
		// TODO(servers): referenced servers go back to pending after a
		// rotation (OpenAPI PrivateKeyUpdate description).
	}

	rows, err := a.Store.UpdatePrivateKey(r.Context(), store.UpdatePrivateKeyParams{
		ID: key.ID, Name: name, Description: description,
		FingerprintSha256: fingerprint, PublicKey: publicKey, PrivateKeyEnc: enc,
		ExpectedVersion: int32(expected),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a key with the same fingerprint already exists in this team")
			return
		}
		a.internalError(w, r, "update private key", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, key.Version)
		return
	}

	updated, err := a.Store.GetPrivateKeyByUUID(r.Context(), store.GetPrivateKeyByUUIDParams{Uuid: key.Uuid, TeamID: ptr(id.TeamID)})
	if err != nil {
		a.internalError(w, r, "reload private key", err)
		return
	}
	w.Header().Set("ETag", etagFor(updated.Version))
	httpapi.WriteJSON(w, http.StatusOK, privateKeyToAPI(updated, a.keysInUse(r, []int64{updated.ID})[updated.ID]))
}

// DeletePrivateKey implements DELETE /private-keys/{uuid} (permission:
// write). RESTRICT: refused while referenced (enforced when servers land).
func (a *API) DeletePrivateKey(w http.ResponseWriter, r *http.Request, privateKeyUuid api.PrivateKeyUuid) {
	id, ok := a.require(w, r, auth.PermKeysManage)
	if !ok {
		return
	}
	key, ok := a.resolvePrivateKey(w, r, id, privateKeyUuid)
	if !ok {
		return
	}
	if count, err := a.Store.CountServersUsingPrivateKey(r.Context(), key.ID); err != nil {
		a.internalError(w, r, "delete private key", err)
		return
	} else if count > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "dependency_exists", "the key is still referenced by a server — detach it first (§19.2)")
		return
	}
	if count, err := a.Store.CountApplicationsUsingPrivateKey(r.Context(), ptr(key.ID)); err != nil {
		a.internalError(w, r, "delete private key", err)
		return
	} else if count > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "dependency_exists", "the key is still used as a deploy key by an application (§19.2)")
		return
	}
	rows, err := a.Store.DeletePrivateKey(r.Context(), key.ID)
	if err != nil || rows == 0 {
		a.internalError(w, r, "delete private key", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sanitizeKeyError keeps key-material errors generic enough for a response
// body (never echoes material, §24.1).
func sanitizeKeyError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "passphrase") {
		return "passphrase-protected keys are not supported"
	}
	return "not a valid PEM/OpenSSH private key"
}
