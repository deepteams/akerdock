package handlers

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
)

// GetHealth implements GET /health: unauthenticated liveness — 200 means
// process alive, database reachable, master key loaded (§6.6; the key is
// loaded before the listener starts, so only the database is probed here).
func (a *API) GetHealth(w http.ResponseWriter, r *http.Request) {
	if err := a.Pool.Ping(r.Context()); err != nil {
		a.Logger.Warn("health check failed", "error", err)
		httpapi.WriteError(w, r, http.StatusServiceUnavailable, httpapi.CodeInternal, "unavailable")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, api.HealthStatus{Status: api.Ok})
}

// GetVersion implements GET /version (permission: read).
func (a *API) GetVersion(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.require(w, r, auth.PermRead); !ok {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, api.VersionInfo{Version: a.Version})
}

// EnableApi implements POST /system/api/enable (permission: root). Exempt
// from the api_enabled gate so a root token can re-enable the API.
func (a *API) EnableApi(w http.ResponseWriter, r *http.Request) {
	a.setAPIEnabled(w, r, true)
}

// DisableApi implements POST /system/api/disable (permission: root).
func (a *API) DisableApi(w http.ResponseWriter, r *http.Request) {
	a.setAPIEnabled(w, r, false)
}

func (a *API) setAPIEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	id, ok := a.requireInstanceRoot(w, r)
	if !ok {
		return
	}
	settings, err := a.Store.SetApiEnabled(r.Context(), enabled)
	if err != nil {
		a.Logger.Error("failed to update api_enabled", "error", err)
		httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "internal error")
		return
	}
	a.Settings.Invalidate()
	action := "system.api_disable"
	if enabled {
		action = "system.api_enable"
	}
	a.recordAudit(r, id, action, "instance", pgtype.UUID{})
	a.Logger.Info("api_enabled changed", "enabled", enabled, "token_uuid", id.TokenUUID)
	httpapi.WriteJSON(w, http.StatusOK, api.ApiState{ApiEnabled: settings.ApiEnabled})
}
