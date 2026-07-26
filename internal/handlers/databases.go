package handlers

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// dbRow is the common shape of the database join queries.
type dbRow struct {
	Resource           store.Resource
	Database           store.Database
	DatabaseCredential store.DatabaseCredential
	EnvironmentUuid    pgtype.UUID
	ProjectUuid        pgtype.UUID
	ServerUuid         pgtype.UUID
	ServerHost         string
}

// databaseToAPI renders a database. Credentials (password and connection
// URLs) are revealed only with read:sensitive (INV-003).
func (a *API) databaseToAPI(row dbRow, id *auth.Identity) api.Database {
	dbUUID := uuidString(row.Resource.Uuid)
	out := api.Database{
		Uuid:            ptr(dbUUID),
		Name:            row.Resource.Name,
		Description:     row.Resource.Description,
		Engine:          ptr(api.DatabaseEngine(row.Database.Engine)),
		ProjectUuid:     ptr(uuidString(row.ProjectUuid)),
		EnvironmentUuid: ptr(uuidString(row.EnvironmentUuid)),
		ServerUuid:      ptr(uuidString(row.ServerUuid)),
		Image:           row.Database.Image,
		PostgresUser:    ptr(row.DatabaseCredential.Username),
		PostgresDb:      row.DatabaseCredential.DbName,
		IsPublic:        ptr(row.Database.IsPublic),
		DesiredStatus:   api.DesiredStatus(row.Resource.DesiredStatus),
		ObservedStatus:  api.ObservedStatus(row.Resource.ObservedStatus),
		ObservedAt:      timePtr(row.Resource.ObservedAt),
		Version:         ptr(int(row.Resource.Version)),
		CreatedAt:       timePtr(row.Resource.CreatedAt),
		UpdatedAt:       timePtr(row.Resource.UpdatedAt),
		IsRedacted:      ptr(true),
	}
	if row.Database.PublicPort != nil {
		out.PublicPort = ptr(int(*row.Database.PublicPort))
	}
	if row.Database.PublicAccessMode != nil {
		out.PublicAccessMode = ptr(api.DatabasePublicAccessMode(*row.Database.PublicAccessMode))
	}

	if !auth.Has(id.Permissions, auth.PermReadSensitive) {
		return out
	}
	password, err := a.Keyring.Decrypt("database_credentials", "password_enc",
		uuidString(row.DatabaseCredential.Uuid), row.DatabaseCredential.PasswordEnc)
	if err != nil {
		return out
	}
	out.IsRedacted = ptr(false)
	out.PostgresPassword = ptr(string(password))

	// URLs are rebuilt on the fly, never stored assembled (§6.2).
	dbName := row.DatabaseCredential.Username
	if row.DatabaseCredential.DbName != nil && *row.DatabaseCredential.DbName != "" {
		dbName = *row.DatabaseCredential.DbName
	}
	out.InternalUrl = ptr(fmt.Sprintf("postgres://%s:%s@%s:5432/%s",
		row.DatabaseCredential.Username, string(password), dbUUID, dbName))

	// The external URL exists only if the database is actually reachable from
	// outside: public AND with a port bound. Publishing one for a private
	// database would be a connection string that cannot connect — worse than
	// none, because it looks usable.
	if row.Database.IsPublic && row.Database.PublicPort != nil {
		url := fmt.Sprintf("postgres://%s:%s@%s:%d/%s",
			row.DatabaseCredential.Username, string(password), row.ServerHost,
			*row.Database.PublicPort, dbName)
		if row.Database.SslMode != nil {
			url += "?sslmode=" + *row.Database.SslMode
		}
		out.ExternalUrl = ptr(url)
	}
	return out
}

func (a *API) resolveDatabase(w http.ResponseWriter, r *http.Request, id *auth.Identity, dbUUID string) (store.GetDatabaseByUUIDRow, bool) {
	var u pgtype.UUID
	if err := u.Scan(dbUUID); err == nil {
		row, err := a.Store.GetDatabaseByUUID(r.Context(), store.GetDatabaseByUUIDParams{Uuid: u, TeamID: id.TeamID})
		if err == nil {
			return row, true
		}
	}
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "database not found")
	return store.GetDatabaseByUUIDRow{}, false
}

// ListDatabases implements GET /databases (permission: read).
func (a *API) ListDatabases(w http.ResponseWriter, r *http.Request, params api.ListDatabasesParams) {
	id, ok := a.require(w, r, auth.PermDatabasesRead)
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
	rows, err := a.Store.ListDatabasesPage(r.Context(), store.ListDatabasesPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list databases", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(row store.ListDatabasesPageRow) int64 { return row.Resource.ID })

	data := make([]api.Database, 0, len(rows))
	for _, row := range rows {
		data = append(data, a.databaseToAPI(dbRow(row), id))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.Database `json:"data"`
		NextCursor *string        `json:"next_cursor"`
	}{data, cursor})
}

// CreatePostgresqlDatabase implements POST /databases/postgresql
// (permission: write). Omitted credentials are generated (§6.2).
func (a *API) CreatePostgresqlDatabase(w http.ResponseWriter, r *http.Request, params api.CreatePostgresqlDatabaseParams) {
	id, ok := a.require(w, r, auth.PermDatabasesCreate)
	if !ok {
		return
	}
	var body api.DatabaseCreatePostgresql
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	var details []api.ErrorDetail
	if body.Name == "" || len(body.Name) > 255 {
		details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"})
	}
	image := jobs.DefaultDatabaseImage
	if body.Image != nil && *body.Image != "" {
		image = *body.Image
		if !imageWithTag.MatchString(image) {
			details = append(details, api.ErrorDetail{Field: ptr("image"), Code: ptr("invalid"), Message: "invalid image reference"})
		}
	}
	user := "postgres"
	if body.PostgresUser != nil && *body.PostgresUser != "" {
		user = *body.PostgresUser
	}
	if !identifierFormat.MatchString(user) {
		details = append(details, api.ErrorDetail{Field: ptr("postgres_user"), Code: ptr("invalid"), Message: "invalid PostgreSQL user name"})
	}
	dbName := user
	if body.PostgresDb != nil && *body.PostgresDb != "" {
		dbName = *body.PostgresDb
		if !identifierFormat.MatchString(dbName) {
			details = append(details, api.ErrorDetail{Field: ptr("postgres_db"), Code: ptr("invalid"), Message: "invalid database name"})
		}
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	project, ok := a.resolveProject(w, r, id, body.ProjectUuid)
	if !ok {
		return
	}
	env, ok := a.resolveEnvironment(w, r, project, body.EnvironmentUuid)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, body.ServerUuid)
	if !ok {
		return
	}
	if server.Status != store.ServerStatusReady || server.IsBuildServer {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("server_uuid"), Code: ptr("invalid_state"), Message: "the target server must be ready (validated) and not a build server"}})
		return
	}
	dest, err := a.defaultDestination(r, server.ID)
	if err != nil {
		a.internalError(w, r, "create database", err)
		return
	}

	// Public exposure: reserve a port (§6.2). The unique index is the
	// authority against a concurrent allocation (§22.3).
	isPublic := body.IsPublic != nil && *body.IsPublic
	var publicPort *int32
	var accessMode *store.PublicAccessMode
	if isPublic {
		port := int32(0)
		if body.PublicPort != nil {
			port = int32(*body.PublicPort)
		} else {
			next, err := a.Store.NextFreePublicPort(r.Context(), server.ID)
			if err != nil {
				a.internalError(w, r, "allocate public port", err)
				return
			}
			port = next
		}
		publicPort = &port
		accessMode = ptr(store.PublicAccessModePortMapping)
		if body.PublicAccessMode != nil && *body.PublicAccessMode == api.DatabaseCreatePostgresqlPublicAccessModeTcpProxy {
			accessMode = ptr(store.PublicAccessModeTcpProxy)
		}
	}

	password := body.PostgresPassword
	if password == nil || *password == "" {
		generated, err := generatePassword()
		if err != nil {
			a.internalError(w, r, "create database", err)
			return
		}
		password = &generated
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create database", err)
		return
	}
	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		a.internalError(w, r, "create database", err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	qtx := a.Store.WithTx(tx)

	resource, err := qtx.CreateResource(r.Context(), store.CreateResourceParams{
		Uuid: u, TeamID: id.TeamID, EnvironmentID: env.ID, DestinationID: dest.ID,
		ResourceType: store.ResourceTypeDatabase, Name: body.Name, Description: body.Description,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a resource with this name already exists in this environment")
			return
		}
		a.internalError(w, r, "create database", err)
		return
	}
	var sslMode *string
	if body.SslMode != nil {
		sslMode = ptr(string(*body.SslMode))
	}
	if err := qtx.CreateDatabaseRow(r.Context(), store.CreateDatabaseRowParams{
		ID: resource.ID, Engine: store.DbEnginePostgresql, Image: &image,
		CustomConfig: body.PostgresConf, InitdbArgs: body.PostgresInitdbArgs,
		ServerID: server.ID, IsPublic: isPublic, PublicAccessMode: accessMode,
		PublicPort: publicPort, SslMode: sslMode,
		SslEnabled: body.SslEnabled != nil && *body.SslEnabled,
	}); err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this public port is already reserved on this server (§22.3)")
			return
		}
		a.internalError(w, r, "create database", err)
		return
	}

	credUUID, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create database", err)
		return
	}
	enc, err := a.Keyring.Encrypt("database_credentials", "password_enc", pguuid.String(credUUID), []byte(*password))
	if err != nil {
		a.internalError(w, r, "create database", err)
		return
	}
	if _, err := qtx.CreateDatabaseCredential(r.Context(), store.CreateDatabaseCredentialParams{
		Uuid: credUUID, DatabaseID: resource.ID, Username: user, PasswordEnc: enc, DbName: &dbName,
	}); err != nil {
		a.internalError(w, r, "create database", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.internalError(w, r, "create database", err)
		return
	}

	row, err := a.Store.GetDatabaseByUUID(r.Context(), store.GetDatabaseByUUIDParams{Uuid: resource.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "reload database", err)
		return
	}
	if body.InstantStart != nil && *body.InstantStart {
		if _, err := a.enqueueDatabaseJob(r, id, resource.ID, resource.Uuid, jobs.TypeDatabaseProvision, "provision", false); err != nil {
			a.Logger.Warn("instant start failed to enqueue", "error", err)
		}
	}
	a.recordAudit(r, id, "database.create", "database", resource.Uuid)
	w.Header().Set("ETag", etagFor(resource.Version))
	httpapi.WriteJSON(w, http.StatusCreated, a.databaseToAPI(dbRow(row), id))
}

// GetDatabase implements GET /databases/{database_uuid} (permission: read).
func (a *API) GetDatabase(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid) {
	id, ok := a.require(w, r, auth.PermDatabasesRead)
	if !ok {
		return
	}
	row, ok := a.resolveDatabase(w, r, id, databaseUuid)
	if !ok {
		return
	}
	if auth.Has(id.Permissions, auth.PermReadSensitive) {
		a.recordAudit(r, id, "secret.reveal", "database", row.Resource.Uuid)
	}
	w.Header().Set("ETag", etagFor(row.Resource.Version))
	httpapi.WriteJSON(w, http.StatusOK, a.databaseToAPI(dbRow(row), id))
}

// UpdateDatabase implements PATCH /databases/{database_uuid} (permission:
// write). Image, configuration and credential changes take effect on the
// next restart (restart_required).
func (a *API) UpdateDatabase(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid, params api.UpdateDatabaseParams) {
	id, ok := a.require(w, r, auth.PermDatabasesUpdate)
	if !ok {
		return
	}
	row, ok := a.resolveDatabase(w, r, id, databaseUuid)
	if !ok {
		return
	}
	expected, err := strconv.Atoi(strings.Trim(strings.TrimSpace(params.IfMatch), `"`))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid If-Match header")
		return
	}

	var body api.DatabaseUpdate
	patch, ok := decodePatch(w, r, &body)
	if !ok {
		return
	}

	name := row.Resource.Name
	if body.Name != nil {
		if *body.Name == "" || len(*body.Name) > 255 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"}})
			return
		}
		name = *body.Name
	}
	description := row.Resource.Description
	if patch.Has("description") {
		description = body.Description
	}
	image := row.Database.Image
	if body.Image != nil {
		if !imageWithTag.MatchString(*body.Image) {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("image"), Code: ptr("invalid"), Message: "invalid image reference"}})
			return
		}
		image = body.Image
	}
	customConfig := row.Database.CustomConfig
	if patch.Has("postgres_conf") {
		customConfig = body.PostgresConf
	}
	isPublic := row.Database.IsPublic
	if body.IsPublic != nil {
		isPublic = *body.IsPublic
	}
	publicPort := row.Database.PublicPort
	var accessMode *store.PublicAccessMode
	if isPublic {
		accessMode = ptr(store.PublicAccessModePortMapping)
	}
	if body.PublicPort != nil {
		publicPort = ptr(int32(*body.PublicPort))
	}
	if isPublic && publicPort == nil {
		next, err := a.Store.NextFreePublicPort(r.Context(), row.Database.ServerID)
		if err != nil {
			a.internalError(w, r, "allocate public port", err)
			return
		}
		publicPort = &next
	}
	sslMode := row.Database.SslMode
	if body.SslMode != nil {
		sslMode = ptr(string(*body.SslMode))
	}

	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		a.internalError(w, r, "update database", err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	qtx := a.Store.WithTx(tx)

	rows, err := qtx.UpdateResourceMeta(r.Context(), store.UpdateResourceMetaParams{
		ID: row.Resource.ID, Name: name, Description: description, ExpectedVersion: int32(expected),
	})
	if err != nil {
		a.internalError(w, r, "update database", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, row.Resource.Version)
		return
	}
	if err := qtx.UpdateDatabaseRow(r.Context(), store.UpdateDatabaseRowParams{
		ID: row.Resource.ID, Image: image, ImageTag: row.Database.ImageTag,
		CustomConfig: customConfig, IsPublic: isPublic, PublicAccessMode: accessMode,
		PublicPort: publicPort, SslMode: sslMode,
	}); err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this public port is already reserved on this server (§22.3)")
			return
		}
		a.internalError(w, r, "update database", err)
		return
	}
	if body.PostgresPassword != nil && *body.PostgresPassword != "" {
		enc, err := a.Keyring.Encrypt("database_credentials", "password_enc",
			uuidString(row.DatabaseCredential.Uuid), []byte(*body.PostgresPassword))
		if err != nil {
			a.internalError(w, r, "update database", err)
			return
		}
		if err := qtx.UpdateDatabasePassword(r.Context(), store.UpdateDatabasePasswordParams{
			DatabaseID: row.Resource.ID, PasswordEnc: enc,
		}); err != nil {
			a.internalError(w, r, "update database", err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.internalError(w, r, "update database", err)
		return
	}

	updated, err := a.Store.GetDatabaseByUUID(r.Context(), store.GetDatabaseByUUIDParams{Uuid: row.Resource.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "reload database", err)
		return
	}
	a.recordAudit(r, id, "database.update", "database", row.Resource.Uuid)
	out := a.databaseToAPI(dbRow(updated), id)
	out.RestartRequired = ptr(true)
	w.Header().Set("ETag", etagFor(updated.Resource.Version))
	httpapi.WriteJSON(w, http.StatusOK, out)
}

// DeleteDatabase implements DELETE /databases/{database_uuid} (permission:
// write): asynchronous deletion; the data volume is kept unless
// delete_volumes=true (INV-008).
func (a *API) DeleteDatabase(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid, params api.DeleteDatabaseParams) {
	id, ok := a.require(w, r, auth.PermDatabasesDelete)
	if !ok {
		return
	}
	row, ok := a.resolveDatabase(w, r, id, databaseUuid)
	if !ok {
		return
	}
	deleteVolumes := params.DeleteVolumes != nil && *params.DeleteVolumes
	if err := a.Store.SetResourceDesiredStatus(r.Context(), store.SetResourceDesiredStatusParams{
		ID: row.Resource.ID, DesiredStatus: store.ResourceDesiredStatusDeleting,
	}); err != nil {
		a.internalError(w, r, "delete database", err)
		return
	}
	job, err := a.enqueueDatabaseJob(r, id, row.Resource.ID, row.Resource.Uuid, jobs.TypeDatabaseDelete, "delete", deleteVolumes)
	if err != nil {
		a.internalError(w, r, "delete database", err)
		return
	}
	a.recordAudit(r, id, "database.delete", "database", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

// StartDatabase implements POST /databases/{uuid}/start (permission: deploy).
func (a *API) StartDatabase(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid) {
	a.databaseLifecycle(w, r, databaseUuid, "start", jobs.TypeDatabaseStart)
}

// StopDatabase implements POST /databases/{uuid}/stop (permission: deploy).
func (a *API) StopDatabase(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid) {
	a.databaseLifecycle(w, r, databaseUuid, "stop", jobs.TypeDatabaseStop)
}

// RestartDatabase implements POST /databases/{uuid}/restart (permission:
// deploy). A restart re-provisions the container, so configuration and
// credential changes take effect.
func (a *API) RestartDatabase(w http.ResponseWriter, r *http.Request, databaseUuid api.DatabaseUuid) {
	a.databaseLifecycle(w, r, databaseUuid, "provision", jobs.TypeDatabaseProvision)
}

func (a *API) databaseLifecycle(w http.ResponseWriter, r *http.Request, databaseUuid, action, jobType string) {
	id, ok := a.require(w, r, auth.PermDatabasesLifecycle)
	if !ok {
		return
	}
	row, ok := a.resolveDatabase(w, r, id, databaseUuid)
	if !ok {
		return
	}
	job, err := a.enqueueDatabaseJob(r, id, row.Resource.ID, row.Resource.Uuid, jobType, action, false)
	if err != nil {
		a.internalError(w, r, action+" database", err)
		return
	}
	a.recordAudit(r, id, "database."+action, "database", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

func (a *API) enqueueDatabaseJob(r *http.Request, id *auth.Identity, resourceID int64, resourceUUID pgtype.UUID, jobType, action string, deleteVolumes bool) (store.Job, error) {
	// Same exclusive lock as applications: database operations on one
	// resource are serialized (§3.1).
	lockKey := "deploy:db:" + uuidString(resourceUUID)
	return queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:      "deploy",
		Type:       jobType,
		Payload:    jobs.DatabasePayload{ResourceID: resourceID, Action: action, DeleteVolumes: deleteVolumes},
		LockKey:    &lockKey,
		TeamID:     ptr(id.TeamID),
		ResourceID: ptr(resourceID),
	})
}

// generatePassword produces the 64-character password of §6.2.
func generatePassword() (string, error) {
	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// GetServerCA implements GET /servers/{uuid}/ca: the CA certificate that signs
// the TLS certificates of this server's databases (§6.3).
//
// Public by nature — it is exactly what a client needs in order to VERIFY the
// database it connects to. The CA's private key is another matter entirely: it
// stays encrypted in the control plane and is never returned, by any path.
func (a *API) GetServerCA(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid) {
	id, ok := a.require(w, r, auth.PermServersRead)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	// null, not an error: a server with no TLS database has no CA yet, and that
	// is a state, not a failure.
	httpapi.WriteJSON(w, http.StatusOK, struct {
		CaCert *string `json:"ca_cert"`
	}{server.CaCert})
}
