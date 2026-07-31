package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/s3"
	"github.com/deepteams/akerdock/internal/store"
)

// checkTimeout bounds the connectivity round trip performed inside the
// request: a hanging endpoint must not hold the HTTP handler open.
const checkTimeout = 15 * time.Second

// s3StorageToAPI renders a storage. The credentials are never part of the
// representation — not even redacted, not even with read:sensitive: nothing
// consumes them outside the instance (INV-003).
func s3StorageToAPI(s store.S3Storage) api.S3Storage {
	out := api.S3Storage{
		Uuid:           uuidString(s.Uuid),
		Name:           s.Name,
		Endpoint:       s.Endpoint,
		Region:         s.Region,
		Bucket:         s.Bucket,
		PathPrefix:     s.PathPrefix,
		IsUsable:       s.IsUsable,
		LastCheckError: s.LastCheckError,
		CreatedAt:      s.CreatedAt.Time.UTC(),
		UpdatedAt:      timePtr(s.UpdatedAt),
		Version:        int(s.Version),
	}
	if s.SseAlgorithm != nil {
		enc := api.S3StorageServerSideEncryption(*s.SseAlgorithm)
		out.ServerSideEncryption = &enc
	}
	return out
}

// s3ClientFor decrypts the credentials of a storage and builds a client.
func (a *API) s3ClientFor(s store.S3Storage) (*s3.Client, error) {
	uuid := uuidString(s.Uuid)
	access, err := a.Keyring.Decrypt("s3_storages", "access_key_enc", uuid, s.AccessKeyEnc)
	if err != nil {
		return nil, err
	}
	secret, err := a.Keyring.Decrypt("s3_storages", "secret_key_enc", uuid, s.SecretKeyEnc)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if s.PathPrefix != nil {
		prefix = *s.PathPrefix
	}
	region := ""
	if s.Region != nil {
		region = *s.Region
	}
	sse := ""
	if s.SseAlgorithm != nil {
		sse = *s.SseAlgorithm
	}
	return s3.New(s3.Config{
		Endpoint: s.Endpoint, Region: region, Bucket: s.Bucket, PathPrefix: prefix,
		AccessKey: string(access), SecretKey: string(secret), SSEAlgorithm: sse,
	}), nil
}

// checkStorage runs the write/read/delete round trip and records the outcome.
// A storage that fails is kept — with is_usable=false and the reason — rather
// than rejected: the operator fixes the bucket policy and revalidates.
func (a *API) checkStorage(ctx context.Context, s store.S3Storage) (bool, *string) {
	client, err := a.s3ClientFor(s)
	if err != nil {
		return false, ptr("could not decrypt the stored credentials")
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if err := client.Check(ctx); err != nil {
		return false, ptr(err.Error())
	}
	return true, nil
}

// validateS3Endpoint keeps an unusable endpoint out of the database.
func validateS3Endpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") &&
		u.Host != "" && u.User == nil
}

func (a *API) resolveS3Storage(w http.ResponseWriter, r *http.Request, id *auth.Identity, storageUUID string) (store.S3Storage, bool) {
	u, ok := a.scanUUID(w, r, storageUUID, "s3 storage")
	if !ok {
		return store.S3Storage{}, false
	}
	s, err := a.Store.GetS3StorageByUUID(r.Context(), store.GetS3StorageByUUIDParams{Uuid: u, TeamID: id.TeamID})
	return resolveRow(a, w, r, "s3 storage", s, err)
}

// ListS3Storages implements GET /s3-storages (permission: read).
func (a *API) ListS3Storages(w http.ResponseWriter, r *http.Request, params api.ListS3StoragesParams) {
	id, ok := a.require(w, r, auth.PermCloudRead)
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
	rows, err := a.Store.ListS3StoragesPage(r.Context(), store.ListS3StoragesPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list s3 storages", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(s store.S3Storage) int64 { return s.ID })

	data := make([]api.S3Storage, 0, len(rows))
	for _, s := range rows {
		data = append(data, s3StorageToAPI(s))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.S3Storage `json:"data"`
		NextCursor *string         `json:"next_cursor"`
	}{data, cursor})
}

// CreateS3Storage implements POST /s3-storages (permission: write). The
// connectivity is verified before the storage is announced as usable (§20.5).
func (a *API) CreateS3Storage(w http.ResponseWriter, r *http.Request, params api.CreateS3StorageParams) {
	id, ok := a.require(w, r, auth.PermCloudManage)
	if !ok {
		return
	}
	var body api.S3StorageCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	var details []api.ErrorDetail
	if strings.TrimSpace(body.Name) == "" || len(body.Name) > 255 {
		details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"})
	}
	if !validateS3Endpoint(body.Endpoint) {
		details = append(details, api.ErrorDetail{Field: ptr("endpoint"), Code: ptr("invalid"), Message: "endpoint must be an http(s) URL"})
	}
	if strings.TrimSpace(body.Bucket) == "" {
		details = append(details, api.ErrorDetail{Field: ptr("bucket"), Code: ptr("required"), Message: "bucket is required"})
	}
	if body.AccessKey == "" || body.SecretKey == "" {
		details = append(details, api.ErrorDetail{Field: ptr("secret_key"), Code: ptr("required"), Message: "access_key and secret_key are required"})
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create s3 storage", err)
		return
	}
	accessEnc, err := a.Keyring.Encrypt("s3_storages", "access_key_enc", pguuid.String(u), []byte(body.AccessKey))
	if err != nil {
		a.internalError(w, r, "create s3 storage", err)
		return
	}
	secretEnc, err := a.Keyring.Encrypt("s3_storages", "secret_key_enc", pguuid.String(u), []byte(body.SecretKey))
	if err != nil {
		a.internalError(w, r, "create s3 storage", err)
		return
	}

	createParams := store.CreateS3StorageParams{
		Uuid: u, TeamID: id.TeamID, Name: body.Name, Endpoint: body.Endpoint,
		Region: body.Region, Bucket: body.Bucket, PathPrefix: body.PathPrefix,
		AccessKeyEnc: accessEnc, SecretKeyEnc: secretEnc,
	}
	if body.ServerSideEncryption != nil {
		v := string(*body.ServerSideEncryption)
		createParams.SseAlgorithm = &v
	}
	storage, err := a.Store.CreateS3Storage(r.Context(), createParams)
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a storage with this name already exists in this team")
			return
		}
		a.internalError(w, r, "create s3 storage", err)
		return
	}

	usable, checkErr := a.checkStorage(r.Context(), storage)
	if err := a.Store.SetS3StorageCheck(r.Context(), store.SetS3StorageCheckParams{
		ID: storage.ID, IsUsable: usable, LastCheckError: checkErr,
	}); err != nil {
		a.internalError(w, r, "create s3 storage", err)
		return
	}
	storage.IsUsable, storage.LastCheckError = usable, checkErr

	a.recordAudit(r, id, "s3_storage.create", "s3_storage", storage.Uuid)
	w.Header().Set("ETag", etagFor(storage.Version))
	httpapi.WriteJSON(w, http.StatusCreated, s3StorageToAPI(storage))
}

// GetS3Storage implements GET /s3-storages/{uuid} (permission: read).
func (a *API) GetS3Storage(w http.ResponseWriter, r *http.Request, s3StorageUuid api.S3StorageUuid) {
	id, ok := a.require(w, r, auth.PermCloudRead)
	if !ok {
		return
	}
	storage, ok := a.resolveS3Storage(w, r, id, s3StorageUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(storage.Version))
	httpapi.WriteJSON(w, http.StatusOK, s3StorageToAPI(storage))
}

// UpdateS3Storage implements PATCH /s3-storages/{uuid} (permission: write).
// Sensitive PATCH — If-Match mandatory (§24.1); the connectivity is rechecked.
func (a *API) UpdateS3Storage(w http.ResponseWriter, r *http.Request, s3StorageUuid api.S3StorageUuid, params api.UpdateS3StorageParams) {
	id, ok := a.require(w, r, auth.PermCloudManage)
	if !ok {
		return
	}
	storage, ok := a.resolveS3Storage(w, r, id, s3StorageUuid)
	if !ok {
		return
	}
	expected, err := strconv.Atoi(strings.Trim(strings.TrimSpace(params.IfMatch), `"`))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid If-Match header")
		return
	}
	var body api.S3StorageUpdate
	patch, ok := decodePatch(w, r, &body)
	if !ok {
		return
	}

	next := storage
	if body.Name != nil {
		if strings.TrimSpace(*body.Name) == "" || len(*body.Name) > 255 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"}})
			return
		}
		next.Name = *body.Name
	}
	if body.Endpoint != nil {
		if !validateS3Endpoint(*body.Endpoint) {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("endpoint"), Code: ptr("invalid"), Message: "endpoint must be an http(s) URL"}})
			return
		}
		next.Endpoint = *body.Endpoint
	}
	if patch.Has("region") {
		next.Region = body.Region
	}
	if body.Bucket != nil {
		if strings.TrimSpace(*body.Bucket) == "" {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("bucket"), Code: ptr("required"), Message: "bucket is required"}})
			return
		}
		next.Bucket = *body.Bucket
	}
	if patch.Has("path_prefix") {
		next.PathPrefix = body.PathPrefix
	}
	if patch.Has("server_side_encryption") {
		if body.ServerSideEncryption != nil {
			v := string(*body.ServerSideEncryption)
			next.SseAlgorithm = &v
		} else {
			next.SseAlgorithm = nil
		}
	}
	uuid := uuidString(storage.Uuid)
	if body.AccessKey != nil {
		enc, err := a.Keyring.Encrypt("s3_storages", "access_key_enc", uuid, []byte(*body.AccessKey))
		if err != nil {
			a.internalError(w, r, "update s3 storage", err)
			return
		}
		next.AccessKeyEnc = enc
	}
	if body.SecretKey != nil {
		enc, err := a.Keyring.Encrypt("s3_storages", "secret_key_enc", uuid, []byte(*body.SecretKey))
		if err != nil {
			a.internalError(w, r, "update s3 storage", err)
			return
		}
		next.SecretKeyEnc = enc
	}

	// The check runs on the new configuration, before it is persisted, so the
	// stored is_usable always describes what is actually in the row.
	usable, checkErr := a.checkStorage(r.Context(), next)
	rows, err := a.Store.UpdateS3Storage(r.Context(), store.UpdateS3StorageParams{
		ID: storage.ID, Name: next.Name, Endpoint: next.Endpoint, Region: next.Region,
		Bucket: next.Bucket, PathPrefix: next.PathPrefix,
		AccessKeyEnc: next.AccessKeyEnc, SecretKeyEnc: next.SecretKeyEnc,
		IsUsable: usable, LastCheckError: checkErr,
		SseAlgorithm:    next.SseAlgorithm,
		ExpectedVersion: int32(expected),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a storage with this name already exists in this team")
			return
		}
		a.internalError(w, r, "update s3 storage", err)
		return
	}
	if rows == 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "version_conflict", "the storage was modified concurrently")
		return
	}

	updated, err := a.Store.GetS3StorageByUUID(r.Context(), store.GetS3StorageByUUIDParams{Uuid: storage.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "update s3 storage", err)
		return
	}
	a.recordAudit(r, id, "s3_storage.update", "s3_storage", storage.Uuid)
	w.Header().Set("ETag", etagFor(updated.Version))
	httpapi.WriteJSON(w, http.StatusOK, s3StorageToAPI(updated))
}

// DeleteS3Storage implements DELETE /s3-storages/{uuid} (permission: write).
// The objects already in the bucket are never touched (INV-008).
func (a *API) DeleteS3Storage(w http.ResponseWriter, r *http.Request, s3StorageUuid api.S3StorageUuid) {
	id, ok := a.require(w, r, auth.PermCloudManage)
	if !ok {
		return
	}
	storage, ok := a.resolveS3Storage(w, r, id, s3StorageUuid)
	if !ok {
		return
	}
	if count, err := a.Store.CountBackupPlansUsingS3Storage(r.Context(), ptr(storage.ID)); err != nil {
		a.internalError(w, r, "delete s3 storage", err)
		return
	} else if count > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "dependency_exists", "the storage is still referenced by a backup plan (§19.2)")
		return
	}
	if _, err := a.Store.DeleteS3Storage(r.Context(), storage.ID); err != nil {
		a.internalError(w, r, "delete s3 storage", err)
		return
	}
	a.recordAudit(r, id, "s3_storage.delete", "s3_storage", storage.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// ValidateS3Storage implements POST /s3-storages/{uuid}/validate (permission:
// write): replays the round trip and refreshes is_usable.
func (a *API) ValidateS3Storage(w http.ResponseWriter, r *http.Request, s3StorageUuid api.S3StorageUuid) {
	id, ok := a.require(w, r, auth.PermCloudManage)
	if !ok {
		return
	}
	storage, ok := a.resolveS3Storage(w, r, id, s3StorageUuid)
	if !ok {
		return
	}
	usable, checkErr := a.checkStorage(r.Context(), storage)
	if err := a.Store.SetS3StorageCheck(r.Context(), store.SetS3StorageCheckParams{
		ID: storage.ID, IsUsable: usable, LastCheckError: checkErr,
	}); err != nil {
		a.internalError(w, r, "validate s3 storage", err)
		return
	}
	storage.IsUsable, storage.LastCheckError = usable, checkErr
	httpapi.WriteJSON(w, http.StatusOK, s3StorageToAPI(storage))
}
