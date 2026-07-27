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
		Fqdn:                  settings.Fqdn,
		AcmeEmail:             settings.AcmeEmail,
		Timezone:              ptr(settings.Timezone),
		ApiEnabled:            ptr(settings.ApiEnabled),
		RegistrationEnabled:   ptr(settings.RegistrationEnabled),
		MfaRequired:           ptr(settings.MfaRequired),
		PasswordLoginDisabled: ptr(settings.PasswordLoginDisabled),
		ImageRetentionCount:   ptr(int(settings.ImageRetentionCount)),
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
	// Self-service signup: only touched when explicitly present. Closed by
	// default; an invitation still authorizes SSO signup regardless (§10.2).
	if body.RegistrationEnabled != nil {
		if _, err := a.Store.SetRegistrationEnabled(r.Context(), *body.RegistrationEnabled); err != nil {
			a.internalError(w, r, "set instance settings", err)
			return
		}
		a.recordAudit(r, id, "instance.registration_enabled_updated", "instance", pgtype.UUID{})
	}
	// MFA requirement is a separate switch: only touched when explicitly present.
	if body.MfaRequired != nil {
		if _, err := a.Store.SetMfaRequired(r.Context(), *body.MfaRequired); err != nil {
			a.internalError(w, r, "set instance settings", err)
			return
		}
		a.recordAudit(r, id, "instance.mfa_required_updated", "instance", pgtype.UUID{})
	}
	// SSO-only mode: enabling it with no OIDC provider would lock everyone but
	// the instance root out — refuse it.
	if body.PasswordLoginDisabled != nil {
		if *body.PasswordLoginDisabled && !a.hasEnabledOAuthProvider(r) {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("password_login_disabled"), Code: ptr("no_provider"),
				Message: "enable at least one OIDC provider before disabling password login",
			}})
			return
		}
		if _, err := a.Store.SetPasswordLoginDisabled(r.Context(), *body.PasswordLoginDisabled); err != nil {
			a.internalError(w, r, "set instance settings", err)
			return
		}
		a.recordAudit(r, id, "instance.password_login_disabled_updated", "instance", pgtype.UUID{})
	}
	// Rollback image retention (ADR-006): at least one, so the live image is
	// always kept; the database CHECK is the backstop.
	if body.ImageRetentionCount != nil {
		if *body.ImageRetentionCount < 1 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("image_retention_count"), Code: ptr("out_of_range"),
				Message: "at least 1 image must be retained",
			}})
			return
		}
		if _, err := a.Store.SetImageRetentionCount(r.Context(), int32(*body.ImageRetentionCount)); err != nil {
			a.internalError(w, r, "set instance settings", err)
			return
		}
		a.recordAudit(r, id, "instance.image_retention_updated", "instance", pgtype.UUID{})
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

// hasEnabledOAuthProvider reports whether at least one OIDC provider is
// configured and enabled — the precondition for SSO-only mode.
func (a *API) hasEnabledOAuthProvider(r *http.Request) bool {
	if a.OAuth == nil {
		return false
	}
	providers, err := a.OAuth.EnabledProviders(r.Context())
	return err == nil && len(providers) > 0
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
