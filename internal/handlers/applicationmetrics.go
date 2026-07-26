package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// GetApplicationMetrics implements GET /applications/{application_uuid}/metrics
// (permission: read): a live CPU/RAM snapshot per compose service, read on
// demand with `docker stats` over the runtime SSH connection and never stored
// (ADR-034). Empty for non-compose build packs.
func (a *API) GetApplicationMetrics(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	id, ok := a.require(w, r, auth.PermMetricsRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	a.writeComponentMetrics(w, r, row.ServerRowID, row.Resource.ID, uuidString(row.Resource.Uuid))
}

// GetPreviewMetrics implements GET
// /applications/{application_uuid}/previews/{preview_uuid}/metrics: the same
// live snapshot for a preview instance's containers (INV-011).
func (a *API) GetPreviewMetrics(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, previewUuid string) {
	id, ok := a.require(w, r, auth.PermPreviewsRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	preview, ok := a.resolvePreview(w, r, id, row.Resource.ID, previewUuid)
	if !ok {
		return
	}
	if preview.Status == store.PreviewStatusDestroyed || preview.Status == store.PreviewStatusDestroying {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this preview is destroyed")
		return
	}
	// Preview containers derive from the PREVIEW uuid (INV-011).
	a.writeComponentMetrics(w, r, row.ServerRowID, row.Resource.ID, uuidString(preview.Uuid))
}

// writeComponentMetrics dials the server, reads one docker stats snapshot and
// maps it onto the resource's compose services (container `<base>-<service>`).
func (a *API) writeComponentMetrics(w http.ResponseWriter, r *http.Request, serverID, resourceID int64, base string) {
	components, err := a.Store.ListServiceComponents(r.Context(), resourceID)
	if err != nil {
		a.internalError(w, r, "metrics", err)
		return
	}
	server, err := a.Store.GetServerByID(r.Context(), serverID)
	if err != nil {
		a.internalError(w, r, "metrics", err)
		return
	}
	key, err := a.Store.GetPrivateKeyByID(r.Context(), server.PrivateKeyID)
	if err != nil {
		a.internalError(w, r, "metrics", err)
		return
	}
	pem, err := a.Keyring.Decrypt("private_keys", "private_key_enc", uuidString(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		a.internalError(w, r, "metrics", err)
		return
	}
	client, err := sshexec.Dial(r.Context(), server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, jobs.PinnedHostKey(server))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "the server is not reachable over SSH right now")
		return
	}
	defer func() { _ = client.Close() }()

	// One snapshot of every running container; we filter to this stack below. A
	// stopped service simply does not appear → running=false, no error.
	res, err := client.Run(r.Context(), "docker stats --no-stream --format '{{json .}}'")
	if err != nil {
		a.internalError(w, r, "metrics", err)
		return
	}
	if res.ExitCode != 0 {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "could not read container stats on the server")
		return
	}
	byName := parseDockerStats(res.Stdout)

	out := make([]api.ComponentMetric, 0, len(components)+1)
	if len(components) == 0 {
		// Single-container build pack (docker image / dockerfile / nixpacks /
		// static): the container IS the resource uuid (INV-011), reported under
		// an empty component name — "the app itself".
		out = append(out, componentStat(byName, base, ""))
	} else {
		for _, c := range components {
			out = append(out, componentStat(byName, base+"-"+c.Name, c.Name))
		}
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

// componentStat maps one container's stats onto a ComponentMetric; a missing
// container (stopped) yields running=false with nil numbers.
func componentStat(byName map[string]dockerStat, container, component string) api.ComponentMetric {
	m := api.ComponentMetric{Component: ptr(component), Running: ptr(false)}
	if s, found := byName[container]; found {
		m.Running = ptr(true)
		m.CpuPercent = s.cpu
		m.MemoryBytes = s.memBytes
		m.MemoryLimitBytes = s.memLimit
		m.MemoryPercent = s.memPercent
	}
	return m
}

// dockerStat holds the parsed numbers of one `docker stats` row.
type dockerStat struct {
	cpu        *float64
	memBytes   *int64
	memLimit   *int64
	memPercent *float64
}

// parseDockerStats parses the `{{json .}}` lines of `docker stats`, keyed by
// container name. Unparseable fields are left nil rather than guessed.
func parseDockerStats(stdout string) map[string]dockerStat {
	out := map[string]dockerStat{}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row struct {
			Name     string `json:"Name"`
			CPUPerc  string `json:"CPUPerc"`
			MemUsage string `json:"MemUsage"`
			MemPerc  string `json:"MemPerc"`
		}
		if json.Unmarshal([]byte(line), &row) != nil || row.Name == "" {
			continue
		}
		stat := dockerStat{
			cpu:        parsePercent(row.CPUPerc),
			memPercent: parsePercent(row.MemPerc),
		}
		// MemUsage is "used / limit", e.g. "25.6MiB / 1.944GiB".
		if used, limit, ok := strings.Cut(row.MemUsage, "/"); ok {
			stat.memBytes = parseDockerSize(strings.TrimSpace(used))
			stat.memLimit = parseDockerSize(strings.TrimSpace(limit))
		}
		out[row.Name] = stat
	}
	return out
}

// parsePercent turns "12.34%" into 12.34; nil if it does not parse.
func parsePercent(s string) *float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

// sizeUnits maps docker's size suffixes to their byte multiplier (binary IEC
// and decimal SI both appear across engine versions).
var sizeUnits = []struct {
	suffix string
	mult   float64
}{
	{"TiB", 1 << 40},
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
	{"TB", 1e12},
	{"GB", 1e9},
	{"MB", 1e6},
	{"kB", 1e3},
	{"KB", 1e3},
	{"B", 1},
}

// parseDockerSize turns "25.6MiB" into a byte count; nil if it does not parse.
func parseDockerSize(s string) *int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, u := range sizeUnits {
		if strings.HasSuffix(s, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			v, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return nil
			}
			b := int64(v * u.mult)
			return &b
		}
	}
	return nil
}
