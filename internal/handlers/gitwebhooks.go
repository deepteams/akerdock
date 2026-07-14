package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/gitwebhook"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// ReceiveGitWebhook implements POST /webhooks/{provider}/{endpoint_uuid}
// (git-webhook-protocols §1.2). It is deliberately NOT an OpenAPI operation:
// the caller is a Git forge, authenticated by its signature, not a token.
//
// The synchronous path stays minimal — persist and answer — because a forge
// that sees slow or failing deliveries disables the hook. Business decisions
// (skip ci, watch paths, auto-deploy off) are NOT delivery errors: they are
// recorded, and answered 200 all the same.
func (a *API) ReceiveGitWebhook(w http.ResponseWriter, r *http.Request) {
	provider := gitwebhook.Provider(chi.URLParam(r, "provider"))
	if !gitwebhook.Supported(string(provider)) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// The signature covers the raw body, so it must be read before anything
	// parses it — and bounded, so an oversized delivery cannot be read into
	// memory at all.
	body, err := io.ReadAll(io.LimitReader(r.Body, gitwebhook.MaxBodyBytes+1))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(body) > gitwebhook.MaxBodyBytes {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	var endpointUUID pgtype.UUID
	if err := endpointUUID.Scan(chi.URLParam(r, "endpoint_uuid")); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	endpoint, err := a.Store.GetWebhookEndpointByUUID(r.Context(), endpointUUID)
	if err != nil {
		// An unknown endpoint and another team's endpoint answer alike (INV-002).
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if store.WebhookProvider(provider) != endpoint.Provider {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	secret, err := a.Keyring.Decrypt("webhook_endpoints", "secret_enc", uuidString(endpoint.Uuid), endpoint.SecretEnc)
	if err != nil {
		a.Logger.Error("webhook secret could not be decrypted", "endpoint", uuidString(endpoint.Uuid))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	deliveryID := gitwebhook.DeliveryID(provider, r.Header)
	if deliveryID == "" {
		// No delivery id means no dedup key, which means no replay protection
		// (INV-009): such a delivery is not accepted at all.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	eventType := gitwebhook.EventType(provider, r.Header)

	sigErr := gitwebhook.VerifySignature(provider, r.Header, body, secret)

	// An invalid signature is still persisted — the audit trail must show that
	// someone tried (§23.4) — but it triggers nothing.
	status := store.WebhookDeliveryStatusReceived
	if sigErr != nil {
		status = store.WebhookDeliveryStatusFailed
	}
	delivery, err := a.Store.CreateWebhookDelivery(r.Context(), store.CreateWebhookDeliveryParams{
		Provider: store.WebhookProvider(provider), DeliveryID: deliveryID,
		WebhookEndpointID: ptr(endpoint.ID), EventType: &eventType,
		SignatureValid: sigErr == nil, Status: status,
		Payload: truncatedPayload(body), TeamID: ptr(endpoint.TeamID),
		ApplicationID: ptr(endpoint.ApplicationID),
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The unique (provider, delivery_id) absorbed a redelivery: answer 200
		// and deploy nothing (INV-009).
		a.Logger.Info("duplicate webhook delivery ignored", "provider", provider, "delivery_id", deliveryID)
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"received": true})
		return
	case err != nil:
		a.internalError(w, r, "receive webhook", err)
		return
	}

	if sigErr != nil {
		// Generic body: it must not tell an attacker whether the secret was
		// wrong or merely absent.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Persisted: the delivery is now durable, so the answer can go out and the
	// work happen asynchronously.
	if _, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:      "webhook",
		Type:       jobs.TypeWebhookProcess,
		Payload:    jobs.WebhookProcessPayload{DeliveryID: delivery.ID},
		TeamID:     ptr(endpoint.TeamID),
		ResourceID: ptr(endpoint.ApplicationID),
	}); err != nil {
		a.internalError(w, r, "receive webhook", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"received": true})
}

// truncatedPayload stores the body for the audit trail, bounded (§1.2). It is
// never a secret — the signature travels in headers, not in the body.
func truncatedPayload(body []byte) []byte {
	const maxStored = 512 << 10
	if len(body) > maxStored {
		return []byte(`{"truncated":true}`)
	}
	if !json.Valid(body) {
		return []byte(`{"unparsable":true}`)
	}
	return body
}

// CreateWebhookEndpoint implements POST /applications/{uuid}/webhook-endpoint
// (permission: write). The secret is generated here and returned exactly once:
// it has to be pasted into the forge's UI, and it is never readable again.
func (a *API) CreateWebhookEndpoint(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	var body api.WebhookEndpointCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	if !gitwebhook.Supported(string(body.Provider)) {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("provider"), Code: ptr("not_implemented"),
			Message: "provider must be github, gitlab or gitea",
		}})
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		a.internalError(w, r, "create webhook endpoint", err)
		return
	}
	secret := hex.EncodeToString(raw)

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create webhook endpoint", err)
		return
	}
	enc, err := a.Keyring.Encrypt("webhook_endpoints", "secret_enc", pguuid.String(u), []byte(secret))
	if err != nil {
		a.internalError(w, r, "create webhook endpoint", err)
		return
	}
	endpoint, err := a.Store.CreateWebhookEndpoint(r.Context(), store.CreateWebhookEndpointParams{
		Uuid: u, ApplicationID: row.Resource.ID,
		Provider: store.WebhookProvider(body.Provider), SecretEnc: enc,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this application already has an endpoint for this provider")
			return
		}
		a.internalError(w, r, "create webhook endpoint", err)
		return
	}

	url := "/webhooks/" + string(body.Provider) + "/" + uuidString(endpoint.Uuid)
	if settings, err := a.Settings.Get(r.Context()); err == nil && settings.Fqdn != nil && *settings.Fqdn != "" {
		url = "https://" + *settings.Fqdn + url
	}
	a.recordAudit(r, id, "webhook_endpoint.create", "application", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, api.WebhookEndpoint{
		Uuid:     uuidString(endpoint.Uuid),
		Provider: api.WebhookEndpointProvider(endpoint.Provider),
		Url:      url,
		// Returned once, never again (§23.2).
		Secret:  ptr(secret),
		Enabled: endpoint.Enabled,
	})
}

// DeleteWebhookEndpoint implements DELETE
// /applications/{uuid}/webhook-endpoint (permission: write).
func (a *API) DeleteWebhookEndpoint(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.DeleteWebhookEndpointParams) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	endpoint, err := a.Store.GetWebhookEndpointForApplication(r.Context(), store.GetWebhookEndpointForApplicationParams{
		ApplicationID: row.Resource.ID, Provider: store.WebhookProvider(params.Provider),
	})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "webhook endpoint not found")
		return
	}
	if _, err := a.Store.DeleteWebhookEndpoint(r.Context(), endpoint.ID); err != nil {
		a.internalError(w, r, "delete webhook endpoint", err)
		return
	}
	a.recordAudit(r, id, "webhook_endpoint.delete", "application", row.Resource.Uuid)
	w.WriteHeader(http.StatusNoContent)
}
