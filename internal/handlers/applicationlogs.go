package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/sshexec"
)

// GetApplicationLogs implements GET /applications/{application_uuid}/logs
// (permission: read): the last lines of the application's running container,
// read straight from the server with `docker logs` — never stored (§5.7).
// The runtime console is the missing half of debugging: deployment logs stop
// at the switch, and everything the app prints after lands only here.
func (a *API) GetApplicationLogs(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.GetApplicationLogsParams) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	lines := 200
	if params.Lines != nil && *params.Lines > 0 && *params.Lines <= 2000 {
		lines = *params.Lines
	}

	server, err := a.Store.GetServerByID(r.Context(), row.ServerRowID)
	if err != nil {
		a.internalError(w, r, "application logs", err)
		return
	}
	key, err := a.Store.GetPrivateKeyByID(r.Context(), server.PrivateKeyID)
	if err != nil {
		a.internalError(w, r, "application logs", err)
		return
	}
	pem, err := a.Keyring.Decrypt("private_keys", "private_key_enc", uuidString(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		a.internalError(w, r, "application logs", err)
		return
	}
	client, err := sshexec.Dial(r.Context(), server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, jobs.PinnedHostKey(server))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "the server is not reachable over SSH right now")
		return
	}
	defer func() { _ = client.Close() }()

	// The container carries the resource UUID as its name (INV-011). A
	// compose stack has no container of its own: `component` picks the
	// service, and its container is `<uuid>-<service>` (compose-spec §2.2).
	container := uuidString(row.Resource.Uuid)
	if params.Component != nil && *params.Component != "" {
		components, err := a.Store.ListServiceComponents(r.Context(), row.Resource.ID)
		if err != nil {
			a.internalError(w, r, "application logs", err)
			return
		}
		found := false
		for _, c := range components {
			if c.Name == *params.Component {
				found = true
				break
			}
		}
		if !found {
			httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound,
				fmt.Sprintf("unknown component %q — see GET /applications/{uuid}/components", *params.Component))
			return
		}
		container = container + "-" + *params.Component
	}
	res, err := client.Run(r.Context(), fmt.Sprintf("docker logs --tail %d %s 2>&1", lines, container))
	if err != nil {
		a.internalError(w, r, "application logs", err)
		return
	}
	if res.ExitCode != 0 {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
			"the application container does not exist on the server — deploy the application first")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": containerLogLines(res.Stdout)})
}
