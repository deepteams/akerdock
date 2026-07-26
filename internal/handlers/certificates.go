package handlers

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

func certificateToAPI(c store.Certificate, serverUUID string) api.Certificate {
	sans := make([]string, 0, len(c.Sans))
	sans = append(sans, c.Sans...)
	return api.Certificate{
		Uuid:       ptr(uuidString(c.Uuid)),
		ServerUuid: ptr(serverUUID),
		Kind:       api.CertificateKind(c.Kind),
		MainDomain: c.MainDomain,
		Sans:       &sans,
		Issuer:     c.Issuer,
		NotBefore:  timePtr(c.NotBefore),
		NotAfter:   timePtr(c.NotAfter),
		Status:     api.CertificateStatus(c.Status),
		LastError:  c.LastError,
		ObservedAt: timePtr(c.ObservedAt),
		CreatedAt:  timePtr(c.CreatedAt),
	}
}

// ListServerCertificates implements GET /servers/{uuid}/certificates
// (permission: read): the certificates served by this server's proxy —
// an observed reflection (§18.3), synchronized from the server.
func (a *API) ListServerCertificates(w http.ResponseWriter, r *http.Request, serverUuid api.ServerUuid, params api.ListServerCertificatesParams) {
	id, ok := a.require(w, r, auth.PermCertificatesRead)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, serverUuid)
	if !ok {
		return
	}
	// Refresh the reflection before answering, so the operator never reads a
	// stale expiry date.
	if server.Status == store.ServerStatusReady && server.ProxyType == store.ProxyTypeTraefik {
		lockKey := "cert:sync:" + uuidString(server.Uuid)
		if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err == nil && active == 0 {
			if _, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
				Queue: "maintenance", Type: jobs.TypeCertificateSync,
				Payload: jobs.CertificatePayload{ServerID: server.ID},
				LockKey: &lockKey, TeamID: ptr(id.TeamID),
			}); err != nil {
				a.Logger.Warn("could not enqueue a certificate sync", "error", err)
			}
		}
	}

	var within *int32
	if params.ExpiringWithinDays != nil {
		within = ptr(int32(*params.ExpiringWithinDays))
	}
	rows, err := a.Store.ListCertificatesForServer(r.Context(), store.ListCertificatesForServerParams{
		ServerID: server.ID, ExpiringWithinDays: within,
	})
	if err != nil {
		a.internalError(w, r, "list certificates", err)
		return
	}
	serverUUID := uuidString(server.Uuid)
	data := make([]api.Certificate, 0, len(rows))
	for _, c := range rows {
		data = append(data, certificateToAPI(c, serverUUID))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data []api.Certificate `json:"data"`
	}{data})
}

// GetCertificate implements GET /certificates/{certificate_uuid}
// (permission: read).
func (a *API) GetCertificate(w http.ResponseWriter, r *http.Request, certificateUuid api.CertificateUuid) {
	id, ok := a.require(w, r, auth.PermCertificatesRead)
	if !ok {
		return
	}
	row, ok := a.resolveCertificate(w, r, id, certificateUuid)
	if !ok {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, certificateToAPI(row.Certificate, uuidString(row.ServerUuid)))
}

// RenewCertificate implements POST /certificates/{uuid}/renew (permission:
// write): forces a re-issuance — 202 + job. The current certificate keeps
// serving until the new one is ready (proxy-contract §7.5).
func (a *API) RenewCertificate(w http.ResponseWriter, r *http.Request, certificateUuid api.CertificateUuid, params api.RenewCertificateParams) {
	id, ok := a.require(w, r, auth.PermCertificatesRenew)
	if !ok {
		return
	}
	row, ok := a.resolveCertificate(w, r, id, certificateUuid)
	if !ok {
		return
	}
	if row.Certificate.Kind == store.CertificateKindCustom {
		httpapi.WriteError(w, r, http.StatusConflict, "invalid_state", "a custom certificate cannot be renewed by AkerDock — upload a new one")
		return
	}

	lockKey := "cert:renew:" + uuidString(row.Certificate.Uuid)
	if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err != nil {
		a.internalError(w, r, "renew certificate", err)
		return
	} else if active > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "operation_in_progress", "a renewal of this certificate is already running")
		return
	}
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue: "maintenance", Type: jobs.TypeCertificateRenew,
		Payload: jobs.CertificatePayload{ServerID: row.Certificate.ServerID, CertificateID: row.Certificate.ID},
		LockKey: &lockKey, TeamID: ptr(id.TeamID),
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "renew certificate", err)
		return
	}
	a.recordAudit(r, id, "certificate.renew", "certificate", row.Certificate.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

func (a *API) resolveCertificate(w http.ResponseWriter, r *http.Request, id *auth.Identity, certUUID string) (store.GetCertificateByUUIDForTeamRow, bool) {
	var u pgtype.UUID
	if err := u.Scan(certUUID); err == nil {
		row, err := a.Store.GetCertificateByUUIDForTeam(r.Context(), store.GetCertificateByUUIDForTeamParams{
			Uuid: u, TeamID: id.TeamID,
		})
		if err == nil {
			return row, true
		}
	}
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "certificate not found")
	return store.GetCertificateByUUIDForTeamRow{}, false
}
