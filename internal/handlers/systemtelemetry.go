package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/telemetry"
)

// Remote telemetry export (§14.2, ADR-008/§27.8): where traces, metrics and
// logs are shipped in OTLP. The config is envelope-encrypted like the email
// relay — the auth headers can carry a bearer token — and read once at boot, so
// a change takes effect at the next restart.

// DecodeOtlpConfig decrypts the stored OTLP configuration. ok=false means
// nothing is stored or it cannot be read — the caller then falls back to the
// OTEL_* environment (telemetry.EnvConfig) or to no export at all.
func DecodeOtlpConfig(enc []byte, keyring *envelope.Keyring) (telemetry.Config, bool) {
	if len(enc) == 0 {
		return telemetry.Config{}, false
	}
	raw, err := keyring.Decrypt("instance_settings", "otlp_config_enc", "1", enc)
	if err != nil {
		return telemetry.Config{}, false
	}
	var cfg telemetry.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return telemetry.Config{}, false
	}
	return cfg, true
}

// GetTelemetry implements GET /system/telemetry.
func (a *API) GetTelemetry(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireInstanceRoot(w, r); !ok {
		return
	}
	out := api.TelemetryConfig{Configured: ptr(false)}
	if settings, err := a.Settings.Get(r.Context()); err == nil {
		if cfg, ok := DecodeOtlpConfig(settings.OtlpConfigEnc, a.Keyring); ok {
			out = telemetryConfigToAPI(cfg)
		}
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
}

// SetTelemetry implements PUT /system/telemetry.
func (a *API) SetTelemetry(w http.ResponseWriter, r *http.Request) {
	id, ok := a.requireInstanceRoot(w, r)
	if !ok {
		return
	}
	var body api.TelemetryConfigSet
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	protocol := "http"
	if body.Protocol != nil && string(*body.Protocol) == "grpc" {
		protocol = "grpc"
	}
	// Signals default to on — an operator who names an endpoint wants its data.
	traces := body.Traces == nil || *body.Traces
	metrics := body.Metrics == nil || *body.Metrics
	logs := body.Logs == nil || *body.Logs

	// Headers omitted → keep what is stored; provided (even empty) → replace.
	var headers map[string]string
	if body.Headers != nil {
		headers = *body.Headers
	} else if settings, err := a.Settings.Get(r.Context()); err == nil {
		if cur, ok := DecodeOtlpConfig(settings.OtlpConfigEnc, a.Keyring); ok {
			headers = cur.Headers
		}
	}
	if len(headers) == 0 {
		headers = nil
	}

	cfg := telemetry.Config{
		Endpoint: strings.TrimSpace(body.Endpoint), Protocol: protocol,
		Headers: headers, Traces: traces, Metrics: metrics, Logs: logs,
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		a.internalError(w, r, "set telemetry", err)
		return
	}
	enc, err := a.Keyring.Encrypt("instance_settings", "otlp_config_enc", "1", raw)
	if err != nil {
		a.internalError(w, r, "set telemetry", err)
		return
	}
	if err := a.Store.SetOtlpConfig(r.Context(), enc); err != nil {
		a.internalError(w, r, "set telemetry", err)
		return
	}
	a.Settings.Invalidate()
	a.recordAudit(r, id, "instance.telemetry_configured", "instance", pgtype.UUID{})

	httpapi.WriteJSON(w, http.StatusOK, telemetryConfigToAPI(cfg))
}

// telemetryConfigToAPI renders the stored config without ever leaking headers.
func telemetryConfigToAPI(cfg telemetry.Config) api.TelemetryConfig {
	protocol := cfg.Protocol
	if protocol == "" {
		protocol = "http"
	}
	return api.TelemetryConfig{
		Configured: ptr(cfg.Endpoint != ""),
		Endpoint:   ptr(cfg.Endpoint),
		Protocol:   ptr(api.TelemetryConfigProtocol(protocol)),
		Traces:     ptr(cfg.Traces),
		Metrics:    ptr(cfg.Metrics),
		Logs:       ptr(cfg.Logs),
		HeadersSet: ptr(len(cfg.Headers) > 0),
	}
}
