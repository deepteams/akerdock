package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/notify"
)

// Transactional email of the instance (§14.2, amendement de spec n°20): the
// relay that carries invitations. It reuses the notification transports rather
// than growing a second mail client — one SMTP implementation, one place where
// "do not send a password in the clear" is decided.

// instanceEmail is what is stored, encrypted, in instance_settings.
type instanceEmail struct {
	Kind   string        `json:"kind"` // smtp | resend
	Config notify.Config `json:"config"`
	From   string        `json:"from"`
}

// transactionalEmail decrypts the instance mail configuration. A missing or
// undecryptable configuration is "not configured": the caller then falls back
// to handing the link over, never to failing silently.
func (a *API) transactionalEmail(r *http.Request) (*instanceEmail, bool) {
	settings, err := a.Settings.Get(r.Context())
	if err != nil || len(settings.TransactionalEmailConfigEnc) == 0 {
		return nil, false
	}
	raw, err := a.Keyring.Decrypt("instance_settings", "transactional_email_config_enc", "1", settings.TransactionalEmailConfigEnc)
	if err != nil {
		return nil, false
	}
	var cfg instanceEmail
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, false
	}
	return &cfg, true
}

// GetTransactionalEmail implements GET /system/email.
func (a *API) GetTransactionalEmail(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireInstanceRoot(w, r); !ok {
		return
	}
	out := api.TransactionalEmail{Configured: ptr(false)}
	if cfg, ok := a.transactionalEmail(r); ok {
		// The credentials are never rendered back — not even to root. What an
		// operator needs to know is that a relay is configured and which address
		// it sends from; the password is only ever useful to the sender.
		out = api.TransactionalEmail{Configured: ptr(true), Kind: ptr(api.TransactionalEmailKind(cfg.Kind)), From: ptr(cfg.From)}
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
}

// SetTransactionalEmail implements PUT /system/email.
func (a *API) SetTransactionalEmail(w http.ResponseWriter, r *http.Request) {
	id, ok := a.requireInstanceRoot(w, r)
	if !ok {
		return
	}
	var body api.TransactionalEmailSet
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	cfg := channelConfig(nil, body.Smtp, body.Resend, nil, nil)
	kind := string(body.Kind)
	if err := notify.ValidateConfig(kind, cfg); err != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("config"), Code: ptr("invalid"), Message: err.Error()}})
		return
	}

	from := ""
	switch {
	case cfg.SMTP != nil:
		from = cfg.SMTP.From
	case cfg.Resend != nil:
		from = cfg.Resend.From
	}

	// Verified BEFORE it is accepted. A relay that cannot be reached is refused
	// here, where an operator is looking, rather than at the first invitation —
	// where the only symptom would be a mail that never arrives.
	if err := notify.NewSystem().Send(r.Context(), kind, cfg, notify.Event{
		Type:     "instance.email_test.v1",
		Resource: "akerdock",
	}); err != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("config"), Code: ptr("unreachable"),
			Message: "the relay refused the test message: " + err.Error(),
		}})
		return
	}

	raw, err := json.Marshal(instanceEmail{Kind: kind, Config: cfg, From: from})
	if err != nil {
		a.internalError(w, r, "set transactional email", err)
		return
	}
	enc, err := a.Keyring.Encrypt("instance_settings", "transactional_email_config_enc", "1", raw)
	if err != nil {
		a.internalError(w, r, "set transactional email", err)
		return
	}
	if err := a.Store.SetTransactionalEmailConfig(r.Context(), enc); err != nil {
		a.internalError(w, r, "set transactional email", err)
		return
	}
	a.Settings.Invalidate()
	a.recordAudit(r, id, "instance.email_configured", "instance", pgtype.UUID{})

	httpapi.WriteJSON(w, http.StatusOK, api.TransactionalEmail{
		Configured: ptr(true), Kind: ptr(api.TransactionalEmailKind(kind)), From: ptr(from),
	})
}
