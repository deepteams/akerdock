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
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// DNS-01 credentials (proxy-contract §7.2, amendement de spec n°21).
//
// The config is a set of environment variables Lego expects. It is write-only:
// stored encrypted, materialized on the server as a 0600 env-file, and never
// rendered back — not to any permission.

// legoProvider is the closed grammar of a provider identifier. It becomes the
// name of a resolver in a generated config file, so nothing else may reach it
// (INV-012).
var legoProvider = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// envVarName bounds what may become a line of the env-file. A name with an `=`
// or a newline in it would inject a second variable — or end the file early.
var envVarName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

func dnsCredentialToAPI(c store.CloudCredential, inUse bool) api.DnsCredential {
	return api.DnsCredential{
		Uuid:      ptr(uuidString(c.Uuid)),
		Name:      c.Name,
		Provider:  c.Provider,
		InUse:     ptr(inUse),
		CreatedAt: timePtr(c.CreatedAt),
	}
}

func (a *API) resolveDNSCredential(w http.ResponseWriter, r *http.Request, id *auth.Identity, credUUID string) (store.CloudCredential, bool) {
	var u pgtype.UUID
	if err := u.Scan(credUUID); err == nil {
		cred, err := a.Store.GetDNSCredentialByUUID(r.Context(), store.GetDNSCredentialByUUIDParams{Uuid: u, TeamID: id.TeamID})
		if err == nil {
			return cred, true
		}
	}
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "DNS credential not found")
	return store.CloudCredential{}, false
}

// ListDnsCredentials implements GET /dns-credentials.
func (a *API) ListDnsCredentials(w http.ResponseWriter, r *http.Request, params api.ListDnsCredentialsParams) {
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
	rows, err := a.Store.ListDNSCredentialsPage(r.Context(), store.ListDNSCredentialsPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list dns credentials", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(c store.CloudCredential) int64 { return c.ID })

	data := make([]api.DnsCredential, 0, len(rows))
	for _, c := range rows {
		n, _ := a.Store.CountDNSCredentialUsage(r.Context(), &c.ID)
		data = append(data, dnsCredentialToAPI(c, n > 0))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.DnsCredential `json:"data"`
		NextCursor *string             `json:"next_cursor"`
	}{data, cursor})
}

// CreateDnsCredential implements POST /dns-credentials.
func (a *API) CreateDnsCredential(w http.ResponseWriter, r *http.Request, params api.CreateDnsCredentialParams) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	var body api.DnsCredentialCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	var details []api.ErrorDetail
	if strings.TrimSpace(body.Name) == "" {
		details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must not be empty"})
	}
	if !legoProvider.MatchString(body.Provider) {
		details = append(details, api.ErrorDetail{
			Field: ptr("provider"), Code: ptr("invalid"),
			Message: "provider must be a Lego provider identifier — lowercase letters, digits and dashes (cloudflare, route53, ovh…)",
		})
	}
	if len(body.Config) == 0 {
		details = append(details, api.ErrorDetail{Field: ptr("config"), Code: ptr("required"), Message: "config must carry at least one environment variable"})
	}
	for name, value := range body.Config {
		if !envVarName.MatchString(name) {
			details = append(details, api.ErrorDetail{
				Field: ptr("config"), Code: ptr("invalid"),
				Message: "invalid environment variable name: " + name,
			})
			break
		}
		// A newline in a value ends the line and starts another variable: what
		// looks like one credential would become two, one of them unintended.
		if strings.ContainsAny(value, "\n\r") {
			details = append(details, api.ErrorDetail{
				Field: ptr("config"), Code: ptr("invalid"),
				Message: "a credential value must not contain a newline",
			})
			break
		}
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create dns credential", err)
		return
	}
	raw, err := json.Marshal(body.Config)
	if err != nil {
		a.internalError(w, r, "create dns credential", err)
		return
	}
	enc, err := a.Keyring.Encrypt("cloud_credentials", "config_enc", uuidString(u), raw)
	if err != nil {
		a.internalError(w, r, "create dns credential", err)
		return
	}
	cred, err := a.Store.CreateDNSCredential(r.Context(), store.CreateDNSCredentialParams{
		Uuid: u, TeamID: id.TeamID, Name: body.Name, Provider: body.Provider, ConfigEnc: enc,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a DNS credential with this name already exists in this team")
			return
		}
		a.internalError(w, r, "create dns credential", err)
		return
	}
	a.recordAudit(r, id, "dns_credential.create", "dns_credential", cred.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, dnsCredentialToAPI(cred, false))
}

// GetDnsCredential implements GET /dns-credentials/{uuid}.
func (a *API) GetDnsCredential(w http.ResponseWriter, r *http.Request, dnsCredentialUuid string) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	cred, ok := a.resolveDNSCredential(w, r, id, dnsCredentialUuid)
	if !ok {
		return
	}
	n, _ := a.Store.CountDNSCredentialUsage(r.Context(), &cred.ID)
	httpapi.WriteJSON(w, http.StatusOK, dnsCredentialToAPI(cred, n > 0))
}

// DeleteDnsCredential implements DELETE /dns-credentials/{uuid}.
func (a *API) DeleteDnsCredential(w http.ResponseWriter, r *http.Request, dnsCredentialUuid string) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	cred, ok := a.resolveDNSCredential(w, r, id, dnsCredentialUuid)
	if !ok {
		return
	}
	// A server still issuing wildcards with this credential would stop being
	// able to renew them — and would discover it at expiry, which is the worst
	// possible moment (§19.2).
	if n, err := a.Store.CountDNSCredentialUsage(r.Context(), &cred.ID); err == nil && n > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
			"a server still uses this credential to issue its wildcard certificates")
		return
	}
	if _, err := a.Store.SoftDeleteDNSCredential(r.Context(), cred.ID); err != nil {
		a.internalError(w, r, "delete dns credential", err)
		return
	}
	a.recordAudit(r, id, "dns_credential.delete", "dns_credential", cred.Uuid)
	w.WriteHeader(http.StatusNoContent)
}
