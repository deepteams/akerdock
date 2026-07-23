package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/envelope"
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

// DatabaseRun provisions and drives managed databases. The data volume is
// always kept unless the deletion explicitly asks otherwise (INV-008).
type DatabaseRun struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
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
	key, err := h.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return nil, err
	}
	pem, err := h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return nil, err
	}

	rec.Start(ctx, payload.Action)
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
	if err != nil {
		rec.Fail(ctx, "SSH connection failed")
		return nil, err
	}
	defer func() { _ = client.Close() }()

	switch payload.Action {
	case "provision":
		err = h.provision(ctx, client, row, dest.Network, dbUUID)
	case "start":
		err = h.simple(ctx, client, "docker start "+dbUUID, row.Resource.ID,
			store.ResourceDesiredStatusRunning, store.ResourceObservedStatusHealthy)
	case "stop":
		err = h.simple(ctx, client, "docker stop -t 30 "+dbUUID, row.Resource.ID,
			store.ResourceDesiredStatusStopped, store.ResourceObservedStatusExited)
	case "restart":
		err = h.simple(ctx, client, "docker restart -t 30 "+dbUUID, row.Resource.ID,
			store.ResourceDesiredStatusRunning, store.ResourceObservedStatusHealthy)
	case "delete":
		err = h.delete(ctx, client, row, dbUUID, payload.DeleteVolumes)
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

// provision converges the database container: data volume, custom config,
// then an idempotent create+start. The password reaches the container
// through an env-file uploaded over stdin — never through argv (INV-003).
func (h *DatabaseRun) provision(ctx context.Context, client *sshexec.Client, row store.GetDatabaseByIDRow, network, dbUUID string) error {
	password, err := h.Keyring.Decrypt("database_credentials", "password_enc",
		pguuid.String(row.DatabaseCredential.Uuid), row.DatabaseCredential.PasswordEnc)
	if err != nil {
		return err
	}
	dbName := row.DatabaseCredential.Username
	if row.DatabaseCredential.DbName != nil && *row.DatabaseCredential.DbName != "" {
		dbName = *row.DatabaseCredential.DbName
	}
	env := fmt.Sprintf("POSTGRES_USER=%s\nPOSTGRES_PASSWORD=%s\nPOSTGRES_DB=%s\n",
		row.DatabaseCredential.Username, string(password), dbName)
	if row.Database.InitdbArgs != nil && *row.Database.InitdbArgs != "" {
		env += "POSTGRES_INITDB_ARGS=" + *row.Database.InitdbArgs + "\n"
	}

	image := DefaultDatabaseImage
	if row.Database.Image != nil && *row.Database.Image != "" {
		image = *row.Database.Image
		if row.Database.ImageTag != nil && *row.Database.ImageTag != "" && !strings.Contains(image, ":") {
			image += ":" + *row.Database.ImageTag
		}
	}
	dir := "/var/lib/akerdock/databases/" + dbUUID
	volume := dbUUID + "_data"

	if res, err := client.RunInput(ctx, fmt.Sprintf(
		"mkdir -p %s && chmod 700 %s && umask 077 && cat > %s/db.env", dir, dir, dir), env); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("preparing the database directory failed")
	}
	// Custom postgresql.conf, mounted read-only when present (§6.2).
	confMount := ""
	if row.Database.CustomConfig != nil && *row.Database.CustomConfig != "" {
		if res, err := client.RunInput(ctx, "umask 077 && cat > "+dir+"/postgresql.conf",
			*row.Database.CustomConfig); err != nil || res.ExitCode != 0 {
			return fmt.Errorf("uploading the custom configuration failed")
		}
		confMount = fmt.Sprintf(" -v %s/postgresql.conf:/etc/postgresql/postgresql.conf:ro", dir)
	}

	// port_mapping publishes the port on the database container: changing it
	// means recreating that container, which drops every open connection.
	// tcp_proxy publishes nothing here — the proxy listens instead, and the
	// database never restarts for a port change (§6.2).
	ports := ""
	if row.Database.IsPublic && row.Database.PublicPort != nil && !tcpProxied(row.Database) {
		ports = fmt.Sprintf(" -p %d:5432", *row.Database.PublicPort)
	}
	cmd := ""
	if confMount != "" {
		cmd = " -c config_file=/etc/postgresql/postgresql.conf"
	}

	// TLS with a certificate signed by the server's CA (§6.3). The key is
	// written 0600 and chowned to the postgres uid: PostgreSQL REFUSES to start
	// with a key any other user can read — which is the right call, and the
	// reason this cannot be left to a default.
	tlsMount := ""
	if row.Database.SslEnabled {
		leaf, err := h.databaseCertificate(ctx, row, dbUUID)
		if err != nil {
			return err
		}
		// The certificate is public: 0644, and every uid can read it. The KEY is
		// not: PostgreSQL refuses to start with a key any other user can read —
		// the right call, and the reason this cannot be left to a default.
		if res, err := client.RunInput(ctx, fmt.Sprintf("cat > %s/server.crt && chmod 644 %s/server.crt", dir, dir), string(leaf.CertPEM)); err != nil || res.ExitCode != 0 {
			return fmt.Errorf("uploading the database certificate failed")
		}
		// The postgres uid is READ FROM THE IMAGE, not assumed: it is 999 on the
		// Debian images and 70 on the Alpine ones. Guessing it produces a
		// database that starts, fails to read its own key, and restarts forever.
		writeKey := fmt.Sprintf(
			"umask 077 && cat > %s/server.key && chmod 600 %s/server.key"+
				" && uid=$(docker run --rm --entrypoint id %s -u postgres 2>/dev/null || echo 999)"+
				" && chown \"$uid:$uid\" %s/server.key",
			dir, dir, shellQuote(image), dir)
		if res, err := client.RunInput(ctx, writeKey, string(leaf.KeyPEM)); err != nil || res.ExitCode != 0 {
			return fmt.Errorf("uploading the database key failed")
		}
		tlsMount = fmt.Sprintf(" -v %s/server.crt:/etc/postgresql/server.crt:ro -v %s/server.key:/etc/postgresql/server.key:ro", dir, dir)
		cmd += " -c ssl=on -c ssl_cert_file=/etc/postgresql/server.crt -c ssl_key_file=/etc/postgresql/server.key"
	}
	team := ""
	if t, err := h.Store.GetTeamByID(ctx, row.Resource.TeamID); err == nil {
		team = pguuid.String(t.Uuid)
	}

	create := fmt.Sprintf(
		"docker volume inspect %s >/dev/null 2>&1 || docker volume create --label akerdock.managed=true --label akerdock.resource_uuid=%s --label akerdock.team_uuid=%s %s >/dev/null; "+
			"docker rm -f %s >/dev/null 2>&1 || true; "+
			"docker create --name %s --restart unless-stopped --network %s --env-file %s/db.env "+
			"-v %s:/var/lib/postgresql/data%s%s%s "+
			"--label akerdock.managed=true --label akerdock.resource_uuid=%s --label akerdock.type=database --label akerdock.team_uuid=%s "+
			"--health-cmd 'pg_isready -U %s' --health-interval 5s --health-retries 5 --health-start-period 10s "+
			"%s%s >/dev/null && docker start %s",
		volume, dbUUID, team, volume,
		dbUUID,
		dbUUID, network, dir,
		volume, confMount, tlsMount, ports,
		dbUUID, team,
		row.DatabaseCredential.Username,
		image, cmd, dbUUID)
	if res, err := client.Run(ctx, create); err != nil || res.ExitCode != 0 {
		detail := ""
		if res != nil {
			detail = firstLine(res.Stderr)
		}
		return fmt.Errorf("creating the database container failed: %s", detail)
	}

	// Wait for readiness: a database is only usable once it accepts
	// connections (§6.2).
	res, err := client.Run(ctx, fmt.Sprintf(
		"deadline=$(( $(date +%%s) + 120 )); while [ $(date +%%s) -lt $deadline ]; do "+
			"st=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' %s 2>/dev/null); "+
			"[ \"$st\" = healthy ] && { echo healthy; exit 0; }; sleep 2; done; echo timeout; exit 1", dbUUID))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		logs, _ := client.Run(ctx, "docker logs --tail 100 "+dbUUID+" 2>&1")
		detail := ""
		if logs != nil {
			detail = "\n" + logs.Stdout
		}
		return fmt.Errorf("the database did not become ready within 120s%s", detail)
	}

	// The route is applied AFTER the database is healthy: opening a public port
	// onto something that is not answering yet is a port that accepts a
	// connection and then hangs.
	if err := h.applyTCPRoute(ctx, client, row, dbUUID); err != nil {
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

func (h *DatabaseRun) simple(ctx context.Context, client *sshexec.Client, command string, resourceID int64,
	desired store.ResourceDesiredStatus, observed store.ResourceObservedStatus,
) error {
	res, err := client.Run(ctx, command)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		msg := firstLine(res.Stderr)
		if strings.Contains(msg, "No such container") {
			msg = "no container exists for this database — provision it first"
		}
		return fmt.Errorf("%s", msg)
	}
	_ = h.Store.SetResourceDesiredStatus(ctx, store.SetResourceDesiredStatusParams{ID: resourceID, DesiredStatus: desired})
	_ = h.Store.SetResourceObservedStatus(ctx, store.SetResourceObservedStatusParams{ID: resourceID, ObservedStatus: observed})
	return nil
}

// delete removes the container and its files; the data volume survives
// unless explicitly destroyed (INV-008).
func (h *DatabaseRun) delete(ctx context.Context, client *sshexec.Client, row store.GetDatabaseByIDRow, dbUUID string, deleteVolumes bool) error {
	cmd := fmt.Sprintf("docker rm -f %s >/dev/null 2>&1 || true; rm -rf /var/lib/akerdock/databases/%s", dbUUID, dbUUID)
	if deleteVolumes {
		cmd += fmt.Sprintf("; docker volume ls -q --filter label=akerdock.resource_uuid=%s | xargs -r docker volume rm -f", dbUUID)
	}
	if res, err := client.Run(ctx, cmd); err != nil || res.ExitCode != 0 {
		if err == nil {
			err = fmt.Errorf("remote cleanup exited with code %d", res.ExitCode)
		}
		return err
	}
	_, err := h.Store.SoftDeleteResource(ctx, row.Resource.ID)
	return err
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
func (h *DatabaseRun) applyTCPRoute(ctx context.Context, client *sshexec.Client, row store.GetDatabaseByIDRow, dbUUID string) error {
	server, err := h.Store.GetServerByID(ctx, row.Database.ServerID)
	if err != nil {
		return err
	}
	// A proxy whose intent is not `running` (never started yet, or deliberately
	// stopped) must not be created or recreated as a side effect of routing a
	// database: the entrypoints are regenerated from the database at the next
	// explicit start, so the route is picked up then.
	converge := func() error {
		if run, _ := proxyBootstrapDecision(server); !run {
			return nil
		}
		return bootstrapProxy(ctx, h.Store, h.Keyring, client, server, true, h.ControlPlanePort)
	}

	if !tcpProxied(row.Database) || row.Database.PublicPort == nil {
		// Not (or no longer) proxied: remove the file, then converge the proxy so
		// the entrypoint disappears with it.
		if _, err := client.Run(ctx, "rm -f /var/lib/akerdock/proxy/dynamic/"+dbUUID+".yaml"); err != nil {
			return err
		}
		return converge()
	}

	content := proxy.GenerateTCP(proxy.TCPRoute{
		ResourceUUID: dbUUID,
		ListenPort:   int(*row.Database.PublicPort),
		TargetPort:   5432,
	}, int64(row.Resource.Version))
	res, err := client.RunInput(ctx, "umask 077 && cat > /var/lib/akerdock/proxy/dynamic/"+dbUUID+".yaml", content)
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("writing the TCP route failed")
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
