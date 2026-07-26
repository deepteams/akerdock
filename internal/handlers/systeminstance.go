package handlers

import (
	"encoding/json"
	"net/http"
	"net/mail"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

// Instance identity (§14.2): the FQDN and the ACME contact. The environment
// variables only seed these at first boot (§6.2) — afterwards the database is
// authoritative, and this endpoint is the ONLY way to change them without
// psql. A non-empty FQDN implies an HTTPS instance: session cookies become
// Secure (at the next binary restart) and every public URL is built https://.

// fqdnPattern is a bare hostname: labels of [a-z0-9-], at least one dot, no
// scheme, no port, no path. Anything else (an URL pasted in, an IP with a
// port) must be refused HERE — a malformed FQDN poisons invitation links,
// OAuth callbacks and the WebAuthn relying party all at once.
var fqdnPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func (a *API) instanceIdentity(r *http.Request) (api.InstanceIdentity, error) {
	settings, err := a.Settings.Get(r.Context())
	if err != nil {
		return api.InstanceIdentity{}, err
	}
	return api.InstanceIdentity{
		Fqdn:       settings.Fqdn,
		AcmeEmail:  settings.AcmeEmail,
		Timezone:   ptr(settings.Timezone),
		ApiEnabled: ptr(settings.ApiEnabled),
	}, nil
}

// GetInstanceSettings implements GET /system/instance.
func (a *API) GetInstanceSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireInstanceRoot(w, r); !ok {
		return
	}
	out, err := a.instanceIdentity(r)
	if err != nil {
		a.internalError(w, r, "get instance settings", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
}

// SetInstanceSettings implements PUT /system/instance.
func (a *API) SetInstanceSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := a.requireInstanceRoot(w, r)
	if !ok {
		return
	}
	var body api.InstanceIdentityUpdate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	fqdn, detail := normalizeFqdn(body.Fqdn)
	if detail != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{*detail})
		return
	}
	acme, detail := normalizeAcmeEmail(body.AcmeEmail)
	if detail != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{*detail})
		return
	}

	if _, err := a.Store.SetInstanceIdentity(r.Context(), store.SetInstanceIdentityParams{
		Fqdn: fqdn, AcmeEmail: acme,
	}); err != nil {
		a.internalError(w, r, "set instance settings", err)
		return
	}
	a.Settings.Invalidate()
	a.recordAudit(r, id, "instance.identity_updated", "instance", pgtype.UUID{})

	out, err := a.instanceIdentity(r)
	if err != nil {
		a.internalError(w, r, "set instance settings", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
}

// normalizeFqdn turns the request field into the stored value: nil or empty
// clears, anything else must be a bare lowercase hostname.
func normalizeFqdn(raw *string) (*string, *api.ErrorDetail) {
	if raw == nil {
		return nil, nil
	}
	value := strings.ToLower(strings.TrimSpace(*raw))
	if value == "" {
		return nil, nil
	}
	if strings.Contains(value, "://") || strings.ContainsAny(value, "/:") {
		return nil, &api.ErrorDetail{
			Field: ptr("fqdn"), Code: ptr("invalid"),
			Message: "a bare hostname is expected (deploy.example.com) — no scheme, port or path",
		}
	}
	if !fqdnPattern.MatchString(value) {
		return nil, &api.ErrorDetail{
			Field: ptr("fqdn"), Code: ptr("invalid"),
			Message: "not a valid hostname: labels of [a-z0-9-], at least one dot",
		}
	}
	return &value, nil
}

func normalizeAcmeEmail(raw *string) (*string, *api.ErrorDetail) {
	if raw == nil {
		return nil, nil
	}
	value := strings.TrimSpace(*raw)
	if value == "" {
		return nil, nil
	}
	if _, err := mail.ParseAddress(value); err != nil {
		return nil, &api.ErrorDetail{
			Field: ptr("acme_email"), Code: ptr("invalid"),
			Message: "not a valid email address — Let's Encrypt refuses to issue without one (§4.3)",
		}
	}
	return &value, nil
}
