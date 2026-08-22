package handlers

// The server's Hugging Face surface (ADR-081): a write-only per-server
// token, and the shared weights cache listed and pruned through typed
// one-shots on the agent channel.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/store"
)

// SetServerHFToken implements PUT /servers/{server_uuid}/hf-token
// (permission: servers:manage). Write-only, the ADR-075 stance: stored
// enveloped, replaced or cleared, never read back.
func (a *API) SetServerHFToken(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid) {
	id, ok := a.require(w, r, auth.PermServersManage)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	var enc []byte
	if body.Token != "" {
		var err error
		enc, err = a.Keyring.Encrypt("servers", "hf_token_enc", uuidString(server.Uuid), []byte(body.Token))
		if err != nil {
			a.internalError(w, r, "store hf token", err)
			return
		}
	}
	if err := a.Store.SetServerHFToken(r.Context(), store.SetServerHFTokenParams{
		ID: server.ID, HfTokenEnc: enc,
	}); err != nil {
		a.internalError(w, r, "store hf token", err)
		return
	}
	action := "server.hf_token.set"
	if body.Token == "" {
		action = "server.hf_token.clear"
	}
	a.recordAudit(r, id, action, "server", server.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// ListServerHFCache implements GET /servers/{server_uuid}/hf-cache
// (permission: servers:read): the shared weights cache, by model, largest
// first — read through a one-shot on the server's agent channel.
func (a *API) ListServerHFCache(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid) {
	id, ok := a.require(w, r, auth.PermServersRead)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	rt, ok := a.agentRuntime(w, r, server.ID)
	if !ok {
		return
	}
	entries, err := jobs.HFCacheList(r.Context(), rt)
	if err != nil {
		// An operational failure ON the server (image pull, one-shot exit),
		// not a bug here: name it, the operator can act on it.
		a.Logger.Warn("hf cache listing failed", "server_id", server.ID, "error", err)
		httpapi.WriteError(w, r, http.StatusBadGateway, httpapi.CodeInternal,
			"reading the cache on the server failed: "+firstLineOf(err))
		return
	}
	type entry struct {
		ModelID string `json:"model_id"`
		SizeMB  int    `json:"size_mb"`
	}
	data := make([]entry, 0, len(entries))
	total := 0
	for _, e := range entries {
		data = append(data, entry{ModelID: e.ModelID, SizeMB: e.SizeMB})
		total += e.SizeMB
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data    []entry `json:"data"`
		TotalMB int     `json:"total_mb"`
	}{data, total})
}

// DeleteServerHFCache implements DELETE /servers/{server_uuid}/hf-cache
// (permission: servers:maintain): one model's weights by reference, or the
// whole cache with all=true — exactly one of the two, always explicit.
func (a *API) DeleteServerHFCache(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid, params api.DeleteServerHFCacheParams) {
	id, ok := a.require(w, r, auth.PermServersMaintain)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	all := params.All != nil && *params.All
	modelID := ""
	if params.ModelId != nil {
		modelID = *params.ModelId
	}
	if all == (modelID != "") {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest,
			"pass exactly one of model_id or all=true — deleting is always explicit")
		return
	}
	rt, ok := a.agentRuntime(w, r, server.ID)
	if !ok {
		return
	}
	if all {
		if err := jobs.HFCachePurge(r.Context(), rt); err != nil {
			a.Logger.Warn("hf cache purge failed", "server_id", server.ID, "error", err)
			httpapi.WriteError(w, r, http.StatusBadGateway, httpapi.CodeInternal,
				"emptying the cache on the server failed: "+firstLineOf(err))
			return
		}
		a.recordAudit(r, id, "server.hf_cache.purge", "server", server.Uuid)
	} else {
		if err := jobs.HFCacheDelete(r.Context(), rt, modelID); err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("model_id"), Code: ptr("invalid"), Message: err.Error(),
			}})
			return
		}
		a.recordAudit(r, id, "server.hf_cache.delete", "server", server.Uuid)
	}
	w.WriteHeader(http.StatusNoContent)
}

// firstLineOf keeps an operational error presentable in an API message.
func firstLineOf(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}
