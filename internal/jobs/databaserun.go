package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/go-connections/nat"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/pki"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// Database job types (§6.2): provisioning converges the container, the
// lifecycle types drive it. All are idempotent.
const (
	TypeDatabaseProvision = "database.provision"
	TypeDatabaseStart     = "database.start"
	TypeDatabaseStop      = "database.stop"
	TypeDatabaseRestart   = "database.restart"
	TypeDatabaseDelete    = "database.delete"
)

// DatabasePayload references the managed database (never a credential).
type DatabasePayload struct {
	ResourceID    int64  `json:"resource_id"`
	Action        string `json:"action"` // provision|start|stop|restart|delete
	DeleteVolumes bool   `json:"delete_volumes,omitempty"`
}

// DefaultDatabaseImage is the pinned default when none is configured (§6.2).
const DefaultDatabaseImage = "postgres:16-alpine"

// databaseReadyTimeout bounds the §6.2 readiness wait; databaseReadyPoll is
// the inspect cadence (vars so tests can shrink them).
var (
	databaseReadyTimeout = 120 * time.Second
	databaseReadyPoll    = 2 * time.Second
)

// DatabaseRun provisions and drives managed databases. The data volume is
// always kept unless the deletion explicitly asks otherwise (INV-008).
// Container operations go through the agent channel (ADR-052) and the host
// material — config/TLS files, directories, proxy route files — through its
// file primitives (ADR-054); SSH remains only for the proxy bootstrap
// convergence.
type DatabaseRun struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Docker  dockerruntime.Source
	HostOps hostops.Source
	Logger  *slog.Logger
	// ControlPlanePort is the published port of this instance (AKERDOCK_PORT),
	// used to route the instance FQDN on the server that hosts it (§14.2).
	ControlPlanePort int
}

// Execute runs one attempt of a database job.
func (h *DatabaseRun) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload DatabasePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	row, err := h.Store.GetDatabaseByID(ctx, payload.ResourceID)
	if err != nil {
		if payload.Action == "delete" {
			return map[string]any{"status": "already deleted"}, nil
		}
		return nil, fmt.Errorf("database not found: %w", err)
	}
	dbUUID := pguuid.String(row.Resource.Uuid)

	server, err := h.Store.GetServerByID(ctx, row.Database.ServerID)
	if err != nil {
		return nil, err
	}
	dest, err := h.Store.GetDestinationByID(ctx, row.Resource.DestinationID)
	if err != nil {
		return nil, err
	}

	rec.Start(ctx, payload.Action)
	rt, err := h.Docker.Runtime(ctx, server.ID)
	if err != nil {
		rec.Fail(ctx, "the server's agent is not connected")
		return nil, err
	}

	switch payload.Action {
	case "provision":
		var ops hostops.Ops
		ops, err = h.HostOps.HostOps(ctx, server.ID)
		if err != nil {
			rec.Fail(ctx, "the server's agent is not connected")
			return nil, err
		}
		// The one SSH remnant: converging a proxy that may need recreating
		// (bootstrap family, ADR-054).
		var client *sshexec.Client
		client, err = h.dialServer(ctx, server)
		if err != nil {
			rec.Fail(ctx, "SSH connection failed")
			return nil, err
		}
		defer func() { _ = client.Close() }()
		err = h.provision(ctx, rt, ops, client, row, dest.Network, dbUUID)
	case "start", "stop", "restart":
		err = h.lifecycle(ctx, rt, payload.Action, dbUUID, row.Resource.ID)
	case "delete":
		var ops hostops.Ops
		ops, err = h.HostOps.HostOps(ctx, server.ID)
		if err != nil {
			rec.Fail(ctx, "the server's agent is not connected")
			return nil, err
		}
		err = h.delete(ctx, rt, ops, row, dbUUID, payload.DeleteVolumes)
	default:
		err = fmt.Errorf("unknown database action %q", payload.Action)
	}
	if err != nil {
		rec.Fail(ctx, firstLine(err.Error()))
		return nil, err
	}
	rec.Succeed(ctx, payload.Action+" completed")
	h.Logger.Info("database job done", "action", payload.Action, "db_uuid", dbUUID)
	return map[string]any{"action": payload.Action, "database_uuid": dbUUID}, nil
}

func (h *DatabaseRun) dialServer(ctx context.Context, server store.Server) (*sshexec.Client, error) {
	key, err := h.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return nil, err
	}
	pem, err := h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return nil, err
	}
	return sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
}

// provision converges the database container: data volume, custom config,
// then an idempotent remove+create+start through the agent channel. The
// password travels in the typed create body over the encrypted channel —
// never on any argv, and no longer through an env file on the host
// (INV-003 as clarified by ADR-051).
// sshClient is the concrete connection the proxy convergence needs; nil in
// unit tests, which stop at the route file.
func (h *DatabaseRun) provision(ctx context.Context, rt dockerruntime.Runtime, ops hostops.Ops, sshClient *sshexec.Client, row store.GetDatabaseByIDRow, network, dbUUID string) error {
	password, err := h.Keyring.Decrypt("database_credentials", "password_enc",
		pguuid.String(row.DatabaseCredential.Uuid), row.DatabaseCredential.PasswordEnc)
	if err != nil {
		return err
	}
	dbName := row.DatabaseCredential.Username
	if row.DatabaseCredential.DbName != nil && *row.DatabaseCredential.DbName != "" {
		dbName = *row.DatabaseCredential.DbName
	}
	env := []string{
		"POSTGRES_USER=" + row.DatabaseCredential.Username,
		"POSTGRES_PASSWORD=" + string(password),
		"POSTGRES_DB=" + dbName,
	}
	if row.Database.InitdbArgs != nil && *row.Database.InitdbArgs != "" {
		env = append(env, "POSTGRES_INITDB_ARGS="+*row.Database.InitdbArgs)
	}

	image := DefaultDatabaseImage
	if row.Database.Image != nil && *row.Database.Image != "" {
		image = *row.Database.Image
		if row.Database.ImageTag != nil && *row.Database.ImageTag != "" && !strings.Contains(image, ":") {
			image += ":" + *row.Database.ImageTag
		}
	}
	dir := "/var/lib/akerdock/databases/" + dbUUID
	volumeName := dbUUID + "_data"

	binds := []string{volumeName + ":/var/lib/postgresql/data"}
	var args []string

	// Custom postgresql.conf, mounted read-only when present (§6.2) — host
	// material, deposited through the agent (ADR-054).
	if row.Database.CustomConfig != nil && *row.Database.CustomConfig != "" {
		if err := ops.WriteFile(ctx, agentwire.FileWriteParams{
			Path: dir + "/postgresql.conf", Content: []byte(*row.Database.CustomConfig),
			Mode: 0o600, MakeDirs: true, DirMode: 0o700,
		}); err != nil {
			return fmt.Errorf("uploading the custom configuration failed: %s", firstLine(err.Error()))
		}
		binds = append(binds, dir+"/postgresql.conf:/etc/postgresql/postgresql.conf:ro")
		args = append(args, "-c", "config_file=/etc/postgresql/postgresql.conf")
	}

	// TLS with a certificate signed by the server's CA (§6.3). The key is
	// written 0600 and chowned to the postgres uid: PostgreSQL REFUSES to start
	// with a key any other user can read — which is the right call, and the
	// reason this cannot be left to a default.
	if row.Database.SslEnabled {
		leaf, err := h.databaseCertificate(ctx, row, dbUUID)
		if err != nil {
			return err
		}
		// The certificate is public: 0644, and every uid can read it.
		if err := ops.WriteFile(ctx, agentwire.FileWriteParams{
			Path: dir + "/server.crt", Content: leaf.CertPEM,
			Mode: 0o644, MakeDirs: true, DirMode: 0o700,
		}); err != nil {
			return fmt.Errorf("uploading the database certificate failed: %s", firstLine(err.Error()))
		}
		if err := ops.WriteFile(ctx, agentwire.FileWriteParams{
			Path: dir + "/server.key", Content: leaf.KeyPEM, Mode: 0o600,
		}); err != nil {
			return fmt.Errorf("uploading the database key failed: %s", firstLine(err.Error()))
		}
		// The postgres uid is READ FROM THE IMAGE, not assumed: it is 999 on the
		// Debian images and 70 on the Alpine ones. Guessing it produces a
		// database that starts, fails to read its own key, and restarts forever.
		uid := probePostgresUID(ctx, rt, image)
		if err := ops.Chown(ctx, agentwire.FileChownParams{
			Path: dir + "/server.key", UID: uid, GID: uid,
		}); err != nil {
			return fmt.Errorf("chowning the database key failed: %s", firstLine(err.Error()))
		}
		binds = append(binds,
			dir+"/server.crt:/etc/postgresql/server.crt:ro",
			dir+"/server.key:/etc/postgresql/server.key:ro")
		args = append(args, "-c", "ssl=on", "-c", "ssl_cert_file=/etc/postgresql/server.crt", "-c", "ssl_key_file=/etc/postgresql/server.key")
	}

	team := ""
	if t, err := h.Store.GetTeamByID(ctx, row.Resource.TeamID); err == nil {
		team = pguuid.String(t.Uuid)
	}
	labels := map[string]string{
		"akerdock.managed":       "true",
		"akerdock.resource_uuid": dbUUID,
		"akerdock.type":          "database",
		"akerdock.team_uuid":     team,
	}

	if _, err := rt.VolumeInspect(ctx, volumeName); err != nil {
		if !dockerruntime.IsNotFound(err) {
			return err
		}
		volumeLabels := map[string]string{
			"akerdock.managed": "true", "akerdock.resource_uuid": dbUUID, "akerdock.team_uuid": team,
		}
		if _, err := rt.VolumeCreate(ctx, volume.CreateOptions{Name: volumeName, Labels: volumeLabels}); err != nil {
			return err
		}
	}

	config := &container.Config{
		Image: image, Env: env, Cmd: args, Labels: labels,
		Healthcheck: &container.HealthConfig{
			Test:        []string{"CMD-SHELL", "pg_isready -U " + row.DatabaseCredential.Username},
			Interval:    5 * time.Second,
			Retries:     5,
			StartPeriod: 10 * time.Second,
		},
	}
	host := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		NetworkMode:   container.NetworkMode(network),
		Binds:         binds,
	}
	// port_mapping publishes the port on the database container: changing it
	// means recreating that container, which drops every open connection.
	// tcp_proxy publishes nothing here — the proxy listens instead, and the
	// database never restarts for a port change (§6.2).
	if row.Database.IsPublic && row.Database.PublicPort != nil && !tcpProxied(row.Database) {
		config.ExposedPorts = nat.PortSet{"5432/tcp": struct{}{}}
		host.PortBindings = nat.PortMap{"5432/tcp": {{HostPort: strconv.Itoa(int(*row.Database.PublicPort))}}}
	}

	if err := removeNamedContainers(ctx, rt, false, dbUUID); err != nil {
		return err
	}
	if _, err := rt.ContainerCreate(ctx, config, host, nil, nil, dbUUID); err != nil {
		return fmt.Errorf("creating the database container failed: %s", firstLine(err.Error()))
	}
	if err := rt.ContainerStart(ctx, dbUUID, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting the database container failed: %s", firstLine(err.Error()))
	}

	// Wait for readiness: a database is only usable once it accepts
	// connections (§6.2). The poll runs here, not as a remote shell loop.
	if err := h.waitHealthy(ctx, rt, dbUUID); err != nil {
		return err
	}

	// The route is applied AFTER the database is healthy: opening a public port
	// onto something that is not answering yet is a port that accepts a
	// connection and then hangs.
	if err := h.applyTCPRoute(ctx, rt, ops, sshClient, row, dbUUID); err != nil {
		return err
	}

	_ = h.Store.SetResourceDesiredStatus(ctx, store.SetResourceDesiredStatusParams{
		ID: row.Resource.ID, DesiredStatus: store.ResourceDesiredStatusRunning,
	})
	_ = h.Store.SetResourceObservedStatus(ctx, store.SetResourceObservedStatusParams{
		ID: row.Resource.ID, ObservedStatus: store.ResourceObservedStatusHealthy,
	})
	return nil
}

// waitHealthy polls the container's healthcheck until it reports healthy or
// the §6.2 budget runs out — with the container's last lines attached, the
// only thing that explains a database that never came up.
func (h *DatabaseRun) waitHealthy(ctx context.Context, rt dockerruntime.Runtime, dbUUID string) error {
	deadline := time.Now().Add(databaseReadyTimeout)
	for {
		resp, err := rt.ContainerInspect(ctx, dbUUID)
		if err == nil && resp.State != nil && resp.State.Health != nil && resp.State.Health.Status == "healthy" {
			return nil
		}
		if time.Now().After(deadline) {
			detail := ""
			if out, lerr := containerLogsTail(ctx, rt, dbUUID, 100); lerr == nil && out != "" {
				detail = "\n" + out
			}
			return fmt.Errorf("the database did not become ready within %s%s", databaseReadyTimeout, detail)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(databaseReadyPoll):
		}
	}
}

// lifecycle drives start/stop/restart with the §6.2 grace of 30 s.
func (h *DatabaseRun) lifecycle(ctx context.Context, rt dockerruntime.Runtime, action, dbUUID string, resourceID int64) error {
	if err := containerLifecycle(ctx, rt, action, dbUUID, 30); err != nil {
		if dockerruntime.IsNotFound(err) {
			return fmt.Errorf("no container exists for this database — provision it first")
		}
		return err
	}
	desired, observed := store.ResourceDesiredStatusRunning, store.ResourceObservedStatusHealthy
	if action == "stop" {
		desired, observed = store.ResourceDesiredStatusStopped, store.ResourceObservedStatusExited
	}
	_ = h.Store.SetResourceDesiredStatus(ctx, store.SetResourceDesiredStatusParams{ID: resourceID, DesiredStatus: desired})
	_ = h.Store.SetResourceObservedStatus(ctx, store.SetResourceObservedStatusParams{ID: resourceID, ObservedStatus: observed})
	return nil
}

// delete removes the container and its files; the data volume survives
// unless explicitly destroyed (INV-008).
func (h *DatabaseRun) delete(ctx context.Context, rt dockerruntime.Runtime, ops hostops.Ops, row store.GetDatabaseByIDRow, dbUUID string, deleteVolumes bool) error {
	if err := removeNamedContainers(ctx, rt, false, dbUUID); err != nil {
		return err
	}
	if err := ops.Remove(ctx, agentwire.FileRemoveParams{
		Path: "/var/lib/akerdock/databases/" + dbUUID, Recursive: true,
	}); err != nil {
		return fmt.Errorf("directory removal: %w", err)
	}
	if deleteVolumes {
		f := filters.NewArgs(filters.Arg("label", "akerdock.resource_uuid="+dbUUID))
		if err := sweepVolumes(ctx, rt, f); err != nil {
			return err
		}
	}
	_, err := h.Store.SoftDeleteResource(ctx, row.Resource.ID)
	return err
}

// probePostgresUID asks the image itself which uid `postgres` maps to, with a
// typed one-shot container (ADR-052 — this used to be the last docker CLI use
// of the TLS path). Any failure falls back to 999, the Debian default, exactly
// as the old shell probe did.
func probePostgresUID(ctx context.Context, rt dockerruntime.Runtime, image string) int {
	out, err := runOneShotCapture(ctx, rt, &container.Config{
		Image: image, Entrypoint: []string{"id"}, Cmd: []string{"-u", "postgres"},
	}, nil)
	if err != nil {
		return 999
	}
	uid, err := strconv.Atoi(strings.TrimSpace(firstLine(out)))
	if err != nil || uid < 0 {
		return 999
	}
	return uid
}

// tcpProxied reports whether this database is exposed through the proxy rather
// than by a port published on its own container.
func tcpProxied(db store.Database) bool {
	return db.IsPublic && db.PublicAccessMode != nil && *db.PublicAccessMode == store.PublicAccessModeTcpProxy
}

// applyTCPRoute routes a public database through the proxy (§2.6, §5.6).
//
// Two writes, in this order: the dynamic file (hot, watched by the proxy) and
// then the static entrypoint (which needs a new container). The reverse order
// would open a listener with nothing behind it — a port that accepts a
// connection and hangs.
func (h *DatabaseRun) applyTCPRoute(ctx context.Context, rt dockerruntime.Runtime, ops hostops.Ops, sshClient *sshexec.Client, row store.GetDatabaseByIDRow, dbUUID string) error {
	server, err := h.Store.GetServerByID(ctx, row.Database.ServerID)
	if err != nil {
		return err
	}
	// A proxy whose intent is not `running` (never started yet, or deliberately
	// stopped) must not be created or recreated as a side effect of routing a
	// database: the entrypoints are regenerated from the database at the next
	// explicit start, so the route is picked up then.
	converge := func() error {
		if run, _ := proxyBootstrapDecision(server); !run || sshClient == nil {
			return nil
		}
		return bootstrapProxy(ctx, h.Store, h.Keyring, sshClient, rt, ops, server, true, h.ControlPlanePort)
	}

	routePath := "/var/lib/akerdock/proxy/dynamic/" + dbUUID + ".yaml"
	if !tcpProxied(row.Database) || row.Database.PublicPort == nil {
		// Not (or no longer) proxied: remove the file, then converge the proxy so
		// the entrypoint disappears with it.
		if err := ops.Remove(ctx, agentwire.FileRemoveParams{Path: routePath}); err != nil {
			return err
		}
		return converge()
	}

	content := proxy.GenerateTCP(proxy.TCPRoute{
		ResourceUUID: dbUUID,
		ListenPort:   int(*row.Database.PublicPort),
		TargetPort:   5432,
	}, int64(row.Resource.Version))
	if err := ops.WriteFile(ctx, agentwire.FileWriteParams{
		Path: routePath, Content: []byte(content), Mode: 0o600, Atomic: true,
	}); err != nil {
		return fmt.Errorf("writing the TCP route failed: %s", firstLine(err.Error()))
	}
	// The proxy is recreated because the entrypoint is static. The database is
	// not touched: that is the entire point of this mode.
	return converge()
}

// databaseCertificate mints the TLS certificate of a database, signing it with
// the server's CA — creating that CA the first time a database on this server
// asks for TLS.
//
// The CA's private key is decrypted here and nowhere else, and never leaves the
// control plane: what reaches the server is a leaf and its key. The CA
// certificate is public — it is what a client needs in order to VERIFY, and a
// TLS nobody verifies protects against nothing.
func (h *DatabaseRun) databaseCertificate(ctx context.Context, row store.GetDatabaseByIDRow, dbUUID string) (*pki.Leaf, error) {
	server, err := h.Store.GetServerByID(ctx, row.Database.ServerID)
	if err != nil {
		return nil, err
	}

	var ca *pki.CA
	if server.CaCert != nil && *server.CaCert != "" && len(server.CaKeyEnc) > 0 {
		key, err := h.Keyring.Decrypt("servers", "ca_key_enc", pguuid.String(server.Uuid), server.CaKeyEnc)
		if err != nil {
			return nil, err
		}
		ca = &pki.CA{CertPEM: []byte(*server.CaCert), KeyPEM: key}
	} else {
		if ca, err = pki.NewCA(server.Name); err != nil {
			return nil, err
		}
		enc, err := h.Keyring.Encrypt("servers", "ca_key_enc", pguuid.String(server.Uuid), ca.KeyPEM)
		if err != nil {
			return nil, err
		}
		if err := h.Store.SetServerCA(ctx, store.SetServerCAParams{
			ID: server.ID, CaCert: ptrStr(string(ca.CertPEM)), CaKeyEnc: enc,
		}); err != nil {
			return nil, err
		}
		h.Logger.Info("server CA created", "server", server.Name)
	}

	// Every name a client may legitimately dial: the container name on the
	// Docker network, and the server's host when the database is exposed. A
	// certificate missing the name that was dialled fails verification, and
	// "certificate verify failed" tells the operator nothing about which name.
	hosts := []string{dbUUID, server.Host}
	return pki.SignLeaf(ca, dbUUID, hosts)
}
