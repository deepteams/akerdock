package jobs

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	networktypes "github.com/docker/docker/api/types/network"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/crypto/ssh"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/hostops"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshkey"
	"github.com/deepteams/akerdock/internal/store"
)

const jobFixtureUUID = "33333333-3333-4333-8333-333333333333"

type jobFlowDB struct {
	blob             []byte
	passwordBlob     []byte
	err              error
	host             string
	port             int
	truthy           bool
	preview          bool
	canCleanup       *bool
	cleanupThreshold *int32
	deploymentStatus *store.DeploymentStatus

	startDeploymentBlocks   int
	startDeploymentCalls    int
	assignBuildServerBlocks int
	assignBuildServerCalls  int
}

func (db *jobFlowDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if db.err != nil {
		return pgconn.CommandTag{}, db.err
	}
	if strings.Contains(sql, "-- name: StartDeploymentUnlessCleanupRunning ") {
		db.startDeploymentCalls++
		if db.startDeploymentBlocks > 0 {
			db.startDeploymentBlocks--
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
	}
	if strings.Contains(sql, "-- name: AssignDeploymentBuildServerUnlessCleanupRunning ") {
		db.assignBuildServerCalls++
		if db.assignBuildServerBlocks > 0 {
			db.assignBuildServerBlocks--
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (db *jobFlowDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if db.err != nil {
		return nil, db.err
	}
	remaining := 1
	if strings.Contains(sql, "Domains") || strings.Contains(sql, "ToRotate") ||
		strings.Contains(sql, "EnvVars") || strings.Contains(sql, "SharedVariables") ||
		strings.Contains(sql, "StoragesForResource") {
		remaining = 0 // routing-removal flows should verify immediately
	}
	return &jobFlowRows{remaining: remaining, blob: db.blob, truthy: db.truthy}, nil
}

func (db *jobFlowDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return jobFlowRow{
		blob: db.blob, passwordBlob: db.passwordBlob, err: db.err,
		sql: sql, host: db.host, port: db.port, truthy: db.truthy, preview: db.preview,
		canCleanup: db.canCleanup, cleanupThreshold: db.cleanupThreshold,
		deploymentStatus: db.deploymentStatus,
	}
}

type jobFlowRow struct {
	blob             []byte
	passwordBlob     []byte
	err              error
	sql              string
	host             string
	port             int
	truthy           bool
	preview          bool
	canCleanup       *bool
	cleanupThreshold *int32
	deploymentStatus *store.DeploymentStatus
}

func (r jobFlowRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for index, d := range dest {
		if strings.Contains(r.sql, "-- name: CanStartServerCleanup ") {
			canCleanup := true
			if r.canCleanup != nil {
				canCleanup = *r.canCleanup
			}
			*(d.(*bool)) = canCleanup
			continue
		}
		if strings.Contains(r.sql, "-- name: GetDeploymentStatus ") {
			status := store.DeploymentStatusQueued
			if r.deploymentStatus != nil {
				status = *r.deploymentStatus
			}
			*(d.(*store.DeploymentStatus)) = status
			continue
		}
		if r.preview && strings.Contains(r.sql, "-- name: GetDeploymentByID ") && index == 29 {
			value := int64(1)
			*(d.(**int64)) = &value
			continue
		}
		if r.preview && strings.Contains(r.sql, "-- name: GetPreviewByID ") {
			switch index {
			case 5:
				value := "feature/unit"
				*(d.(**string)) = &value
				continue
			case 6:
				value := "0123456789012345678901234567890123456789"
				*(d.(**string)) = &value
				continue
			case 10:
				value := "pr-1.example.test"
				*(d.(**string)) = &value
				continue
			}
		}
		if strings.Contains(r.sql, "-- name: GetBackupPlanByID ") && index == 2 {
			value := int64(1)
			*(d.(**int64)) = &value
			continue
		}
		if (strings.Contains(r.sql, "-- name: GetBackupExecutionByID ") ||
			strings.Contains(r.sql, "-- name: GetLatestSuccessfulBackupExecution ")) &&
			(index == 5 || index == 7 || index == 11 || index == 17 || index == 18) {
			switch index {
			case 5:
				value := "/var/lib/akerdock/backups/unit.sql.gz"
				*(d.(**string)) = &value
			case 7:
				value := "0123456789abcdef"
				*(d.(**string)) = &value
			case 11, 17:
				reflect.ValueOf(d).Elem().SetZero()
			case 18:
				value := int32(3)
				*(d.(**int32)) = &value
			}
			continue
		}
		if strings.Contains(r.sql, "-- name: GetDatabaseByID ") && index == 40 {
			*(d.(*[]byte)) = append([]byte(nil), r.passwordBlob...)
			continue
		}
		if strings.Contains(r.sql, "-- name: GetDeploymentByID ") && (index == 16 || index == 17) {
			value := "nginx"
			if index == 17 {
				value = "1.27"
			}
			*(d.(**string)) = &value
			continue
		}
		if strings.Contains(r.sql, "-- name: GetApplicationByID ") && index == 24 {
			value := "https://example.test/deepteams/akerdock.git"
			*(d.(**string)) = &value
			continue
		}
		if strings.Contains(r.sql, "-- name: GetApplicationByID ") && index == 56 {
			// applications.access_public_routes is a non-null JSON array. The
			// generic byte-slice fixture is encrypted key material, which is
			// appropriate for *_enc columns but deliberately invalid JSON.
			*(d.(*[]byte)) = []byte("[]")
			continue
		}
		if strings.Contains(r.sql, "-- name: GetServerByID ") {
			switch index {
			case 5:
				*(d.(*string)) = r.host
				continue
			case 6:
				*(d.(*int32)) = int32(r.port)
				continue
			case 7:
				*(d.(*string)) = "unit"
				continue
			case 9:
				*(d.(*int32)) = 2
				continue
			case 28:
				if r.cleanupThreshold != nil {
					value := *r.cleanupThreshold
					*(d.(**int32)) = &value
					continue
				}
			case 48:
				reflect.ValueOf(d).Elem().SetZero()
				continue
			}
		}
		if err := fillJobDestination(d, r.blob, r.truthy); err != nil {
			return err
		}
	}
	return nil
}

type jobFlowRows struct {
	remaining int
	current   bool
	blob      []byte
	truthy    bool
}

func (r *jobFlowRows) Close()                                     { r.remaining = 0 }
func (*jobFlowRows) Err() error                                   { return nil }
func (*jobFlowRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (*jobFlowRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (*jobFlowRows) Values() ([]any, error)                       { return nil, nil }
func (*jobFlowRows) RawValues() [][]byte                          { return nil }
func (*jobFlowRows) Conn() *pgx.Conn                              { return nil }
func (r *jobFlowRows) Next() bool {
	if r.remaining == 0 {
		r.current = false
		return false
	}
	r.remaining--
	r.current = true
	return true
}

func (r *jobFlowRows) Scan(dest ...any) error {
	if !r.current {
		return errors.New("Scan called before Next")
	}
	for _, d := range dest {
		if err := fillJobDestination(d, r.blob, r.truthy); err != nil {
			return err
		}
	}
	return nil
}

var jobEnumValues = map[string]string{
	"ActorKind":                  "system",
	"AdoptionScanStatus":         "completed",
	"ArtifactKind":               "local_image",
	"AuditResult":                "success",
	"BackupExecutionStatus":      "succeeded",
	"BuildPack":                  "image",
	"CertificateKind":            "letsencrypt",
	"CertificateStatus":          "valid",
	"DbEngine":                   "postgresql",
	"DeploymentStatus":           "succeeded",
	"DeploymentStepStatus":       "succeeded",
	"DeploymentTrigger":          "manual",
	"GitProvider":                "github",
	"GitSourceKind":              "public",
	"JobStatus":                  "queued",
	"NotificationChannelKind":    "webhook",
	"NotificationDeliveryStatus": "delivered",
	"NotificationSeverity":       "info",
	"PreviewProtection":          "none",
	"InferenceEngine":            "vllm",
	"PreviewStatus":              "destroyed",
	"ProxyDesiredState":          "running",
	"ProxyRevisionStatus":        "active",
	"ProxyType":                  "traefik",
	"PublicAccessMode":           "tcp_proxy",
	"ResourceDesiredStatus":      "running",
	"ResourceObservedStatus":     "healthy",
	"ResourceType":               "application",
	"RestoreDrillStatus":         "succeeded",
	"ServerStatus":               "ready",
	"SharedVariableScope":        "team",
	"StorageKind":                "volume",
	"TaskExecutionStatus":        "succeeded",
	"TaskKind":                   "container_command",
	"TaskMissedRunPolicy":        "skip",
	"TaskOverlapPolicy":          "forbid",
	"TeamRole":                   "owner",
	"WebhookDeliveryStatus":      "accepted",
	"WebhookProvider":            "github",
}

func fillJobDestination(dest any, blob []byte, truthy bool) error {
	if dest == nil {
		return nil
	}
	switch d := dest.(type) {
	case *time.Time:
		*d = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		return nil
	case *pgtype.UUID:
		return d.Scan(jobFixtureUUID)
	case *pgtype.Timestamptz:
		*d = pgtype.Timestamptz{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true}
		return nil
	case *pgtype.Timestamp:
		*d = pgtype.Timestamp{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true}
		return nil
	case *pgtype.Date:
		*d = pgtype.Date{Time: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Valid: true}
		return nil
	case *pgtype.Time:
		*d = pgtype.Time{Microseconds: int64(time.Hour / time.Microsecond), Valid: true}
		return nil
	case *pgtype.Text:
		*d = pgtype.Text{String: "unit", Valid: true}
		return nil
	case *pgtype.Bool:
		*d = pgtype.Bool{Bool: truthy, Valid: true}
		return nil
	case *pgtype.Int2:
		*d = pgtype.Int2{Int16: 1, Valid: true}
		return nil
	case *pgtype.Int4:
		*d = pgtype.Int4{Int32: 1, Valid: true}
		return nil
	case *pgtype.Int8:
		*d = pgtype.Int8{Int64: 1, Valid: true}
		return nil
	case *pgtype.Float4:
		*d = pgtype.Float4{Float32: 1, Valid: true}
		return nil
	case *pgtype.Float8:
		*d = pgtype.Float8{Float64: 1, Valid: true}
		return nil
	case *pgtype.Numeric:
		*d = pgtype.Numeric{Int: big.NewInt(1), Valid: true}
		return nil
	}
	value := reflect.ValueOf(dest)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return errors.New("invalid scan destination")
	}
	return fillJobValue(value.Elem(), blob, truthy)
}

func fillJobValue(value reflect.Value, blob []byte, truthy bool) error {
	if !value.CanSet() {
		return nil
	}
	if value.Kind() == reflect.Pointer {
		// SQL nullable columns stay NULL by default. Individual scenarios use
		// non-null pgtype values explicitly when the distinction matters.
		value.SetZero()
		return nil
	}
	if fixture, ok := jobEnumValues[value.Type().Name()]; ok && value.Kind() == reflect.String {
		value.SetString(fixture)
		return nil
	}
	switch value.Kind() {
	case reflect.String:
		value.SetString("unit")
	case reflect.Bool:
		value.SetBool(truthy)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(1)
	case reflect.Float32, reflect.Float64:
		value.SetFloat(1)
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			value.SetBytes(append([]byte(nil), blob...))
		} else {
			value.Set(reflect.MakeSlice(value.Type(), 0, 0))
		}
	case reflect.Map:
		value.Set(reflect.MakeMap(value.Type()))
	case reflect.Struct:
		if valid := value.FieldByName("Valid"); valid.IsValid() && valid.CanSet() && valid.Kind() == reflect.Bool {
			valid.SetBool(true)
			for i := 0; i < value.NumField(); i++ {
				if value.Type().Field(i).Name != "Valid" {
					_ = fillJobValue(value.Field(i), blob, truthy)
				}
			}
		}
	}
	return nil
}

func jobFlowDependencies(t *testing.T) (*store.Queries, *envelope.Keyring, *audit.Recorder, *slog.Logger, *jobFlowDB) {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte("1:" + key + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	material, err := sshkey.GenerateEd25519("jobs-unit")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := keyring.Encrypt("private_keys", "private_key_enc", jobFixtureUUID, []byte(material.PrivatePEM))
	if err != nil {
		t.Fatal(err)
	}
	passwordBlob, err := keyring.Encrypt(
		"database_credentials", "password_enc", jobFixtureUUID, []byte("unit-password"),
	)
	if err != nil {
		t.Fatal(err)
	}
	db := &jobFlowDB{blob: blob, passwordBlob: passwordBlob}
	q := store.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return q, keyring, &audit.Recorder{Store: q, Logger: logger}, logger, db
}

type jobSSHServer struct {
	listener net.Listener
	config   *ssh.ServerConfig
	mu       sync.Mutex
	conns    []net.Conn
}

func newJobSSHServer(t *testing.T) *jobSSHServer {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &jobSSHServer{listener: listener, config: config}
	go server.accept()
	t.Cleanup(server.close)
	return server
}

func (s *jobSSHServer) accept() {
	for {
		raw, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, raw)
		s.mu.Unlock()
		go s.serve(raw)
	}
}

func (s *jobSSHServer) serve(raw net.Conn) {
	connection, channels, requests, err := ssh.NewServerConn(raw, s.config)
	if err != nil {
		_ = raw.Close()
		return
	}
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		go s.handleChannel(incoming)
	}
	_ = connection.Close()
}

func (s *jobSSHServer) handleChannel(incoming ssh.NewChannel) {
	if incoming.ChannelType() != "session" {
		_ = incoming.Reject(ssh.UnknownChannelType, "session only")
		return
	}
	channel, requests, err := incoming.Accept()
	if err != nil {
		return
	}
	for request := range requests {
		if request.Type != "exec" {
			request.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		_ = ssh.Unmarshal(request.Payload, &payload)
		request.Reply(true, nil)
		_, _ = io.Copy(io.Discard, channel)
		stdout := jobCommandOutput(payload.Command)
		if stdout != "" {
			_, _ = channel.Write([]byte(stdout))
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		_ = channel.Close()
		return
	}
}

func jobCommandOutput(command string) string {
	switch {
	case strings.Contains(command, "uname -m"):
		return "x86_64\nUnit Linux\n"
	case strings.Contains(command, "git ls-remote"):
		return "0123456789012345678901234567890123456789\trefs/heads/main\n"
	case strings.Contains(command, "git rev-parse HEAD"):
		return "0123456789012345678901234567890123456789\n"
	case strings.Contains(command, "cat ") && strings.Contains(command, "docker-compose.yml"):
		return "services:\n  web:\n    image: nginx:1.27\n"
	case strings.Contains(command, "command -v docker"):
		return "/usr/bin/docker\n"
	case strings.Contains(command, "docker version --format"):
		return "26.1.0\n"
	case strings.Contains(command, nixpacksBin+" --version"):
		return NixpacksVersion + "\n"
	case strings.Contains(command, ".DockerRootDir"):
		return "75\n"
	case strings.Contains(command, "stat -c %s"):
		return "128\n0123456789abcdef\n16.0\n"
	case strings.Contains(command, "sha256sum "):
		return "0123456789abcdef\n"
	case strings.Contains(command, "information_schema.tables"):
		return "3\n"
	case strings.Contains(command, "/api/http/services"):
		return ""
	case strings.Contains(command, "{{.State.Status}}"):
		return "running\n"
	default:
		return ""
	}
}

func (s *jobSSHServer) address(t *testing.T) (string, int) {
	t.Helper()
	host, rawPort, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func (s *jobSSHServer) close() {
	_ = s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, connection := range s.conns {
		_ = connection.Close()
	}
}

// unavailableDocker stands in for the agent channel at the external boundary:
// every runtime resolution answers the mandatory-agent failure (ADR-051).
type unavailableDocker struct{}

func (unavailableDocker) Runtime(context.Context, int64) (dockerruntime.Runtime, error) {
	return nil, agentwire.Unavailable("not connected")
}

// unavailableHost is its ADR-054 twin for the host-ops seam.
type unavailableHost struct{}

func (unavailableHost) HostOps(context.Context, int64) (hostops.Ops, error) {
	return nil, agentwire.Unavailable("not connected")
}

func TestJobFlowsReachExternalBoundary(t *testing.T) {
	q, keyring, recorder, logger, db := jobFlowDependencies(t)
	db.host, db.port = newJobSSHServer(t).address(t)
	job := func(jobType, payload string) store.Job {
		return store.Job{ID: 1, JobType: jobType, Payload: []byte(payload)}
	}
	rec := func(j store.Job) *queue.StepRecorder { return queue.NewStepRecorder(q, j) }

	tests := map[string]func() (any, error){
		"proxy lifecycle": func() (any, error) {
			j := job(TypeProxyStop, `{"server_id":1,"action":"stop"}`)
			return (&ProxyLifecycle{Store: q, Keyring: keyring, Docker: unavailableDocker{}, HostOps: unavailableHost{}, Logger: logger}).Execute(context.Background(), j, rec(j))
		},
		"server cleanup": func() (any, error) {
			j := job(TypeServerCleanup, `{"server_id":1,"reason":"manual"}`)
			return (&ServerCleanup{Store: q, Keyring: keyring, Audit: recorder, Docker: unavailableDocker{}, Logger: logger}).Execute(context.Background(), j, rec(j))
		},
		"application lifecycle": func() (any, error) {
			j := job(TypeApplicationStop, `{"resource_id":1,"action":"stop"}`)
			return (&ApplicationLifecycle{Store: q, Docker: unavailableDocker{}, Logger: logger}).Execute(context.Background(), j, rec(j))
		},
		"database": func() (any, error) {
			j := job(TypeDatabaseStop, `{"resource_id":1,"action":"stop"}`)
			return (&DatabaseRun{Store: q, Keyring: keyring, Docker: unavailableDocker{}, HostOps: unavailableHost{}, Logger: logger}).Execute(context.Background(), j, rec(j))
		},
		"server validation": func() (any, error) {
			j := job(TypeServerValidate, `{"server_id":1}`)
			return (&ServerValidate{Store: q, Keyring: keyring, Docker: unavailableDocker{}, HostOps: unavailableHost{}, Logger: logger}).Execute(context.Background(), j, rec(j))
		},
		"routing": func() (any, error) {
			j := job(TypeApplyRouting, `{"application_id":1,"revision":1}`)
			return (&ApplyRouting{Store: q, Keyring: keyring, Docker: unavailableDocker{}, HostOps: unavailableHost{}, Logger: logger}).Execute(context.Background(), j, rec(j))
		},
		"certificate": func() (any, error) {
			j := job(TypeCertificateSync, `{"server_id":1}`)
			return (&CertificateSync{Store: q, Docker: unavailableDocker{}, HostOps: unavailableHost{}, Logger: logger}).Execute(context.Background(), j, rec(j))
		},
		"preview already destroyed": func() (any, error) {
			j := job(TypePreviewDestroy, `{"preview_id":1}`)
			return (&PreviewDestroy{Store: q, Keyring: keyring, Docker: unavailableDocker{}, HostOps: unavailableHost{}, Logger: logger}).Execute(context.Background(), j, rec(j))
		},
		"backup": func() (any, error) {
			j := job(TypeBackupExecute, `{"plan_id":1}`)
			return (&BackupRun{Store: q, Keyring: keyring, Docker: unavailableDocker{}, HostOps: unavailableHost{}, Audit: recorder, Logger: logger}).Execute(context.Background(), j, rec(j))
		},
		"application delete": func() (any, error) {
			j := job(TypeApplicationDelete, `{"resource_id":1}`)
			return (&ApplicationDelete{Store: q, Docker: unavailableDocker{}, HostOps: unavailableHost{}, Logger: logger}).Execute(context.Background(), j, rec(j))
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			// A successful idempotent short-circuit or an error at the mocked
			// SSH/credential boundary are both valid; a panic is not.
			_, _ = run()
		})
	}
}

func TestJobFlowsPropagateStoreFailure(t *testing.T) {
	q, keyring, _, logger, db := jobFlowDependencies(t)
	db.err = errors.New("database unavailable")
	j := store.Job{ID: 1, JobType: TypeProxyStop, Payload: []byte(`{"server_id":1,"action":"stop"}`)}
	_, err := (&ProxyLifecycle{Store: q, Keyring: keyring, Docker: unavailableDocker{}, HostOps: unavailableHost{}, Logger: logger}).Execute(
		context.Background(), j, queue.NewStepRecorder(q, j))
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func TestRemainingJobFlowsReachTheirDecisionBoundary(t *testing.T) {
	q, keyring, recorder, logger, db := jobFlowDependencies(t)
	db.host, db.port = newJobSSHServer(t).address(t)
	db.truthy = true
	job := func(jobType, payload string) store.Job {
		return store.Job{ID: 2, JobType: jobType, Payload: []byte(payload)}
	}
	rec := func(j store.Job) *queue.StepRecorder { return queue.NewStepRecorder(q, j) }

	tests := map[string]func() (any, error){
		"scheduled task": func() (any, error) {
			j := job(TypeScheduledTaskRun, `{"task_id":1,"execution_id":1}`)
			return (&ScheduledTaskRun{Store: q, Docker: unavailableDocker{}, Audit: recorder, Logger: logger}).
				Execute(context.Background(), j, rec(j))
		},
		"adoption scan": func() (any, error) {
			j := job(TypeAdoptionScan, `{"scan_id":1}`)
			return (&Adoption{Store: q, Keyring: keyring, Docker: unavailableDocker{}, Logger: logger}).
				ExecuteScan(context.Background(), j, rec(j))
		},
		"adoption empty selection": func() (any, error) {
			j := job(TypeAdoptionAdopt, `{"scan_id":1,"environment_id":1,"items":[]}`)
			return (&Adoption{Store: q, Keyring: keyring, Docker: unavailableDocker{}, Logger: logger}).
				ExecuteAdopt(context.Background(), j, rec(j))
		},
		"disown": func() (any, error) {
			j := job(TypeResourceDisown, `{"resource_id":1}`)
			return (&Adoption{Store: q, Keyring: keyring, Docker: unavailableDocker{}, HostOps: unavailableHost{}, Logger: logger}).
				ExecuteDisown(context.Background(), j, rec(j))
		},
		"webhook": func() (any, error) {
			j := job(TypeWebhookProcess, `{"delivery_id":1}`)
			return (&WebhookProcess{Store: q, Keyring: keyring, Logger: logger}).
				Execute(context.Background(), j, rec(j))
		},
		"github push": func() (any, error) {
			j := job(TypeGithubAppPush, `{"delivery_id":1,"github_app_id":1,"repository_external_id":"1"}`)
			return (&GithubAppPush{Store: q, Logger: logger}).Execute(context.Background(), j, rec(j))
		},
		"github pull request": func() (any, error) {
			j := job(TypeGithubAppPullRequest, `{"delivery_id":1,"github_app_id":1}`)
			return (&GithubAppPullRequest{Store: q, Keyring: keyring, Logger: logger}).
				Execute(context.Background(), j, rec(j))
		},
		"github comment": func() (any, error) {
			j := job(TypeGithubAppIssueComment, `{"delivery_id":1,"github_app_id":1}`)
			return (&GithubAppIssueComment{Store: q, Keyring: keyring, Logger: logger}).
				Execute(context.Background(), j, rec(j))
		},
		"terminal deployment": func() (any, error) {
			j := job(TypeDeploymentRun, `{"deployment_id":1}`)
			return (&DeploymentRun{Store: q, Keyring: keyring, Audit: recorder, Logger: logger, Docker: unavailableDocker{}, HostOps: unavailableHost{}}).
				Execute(context.Background(), j, rec(j))
		},
		"encryption rotation without stale rows": func() (any, error) {
			j := job(TypeEncryptionRotate, `{}`)
			return (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
				Execute(context.Background(), j, rec(j))
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			_, _ = run()
		})
	}
}

// deployFakeRuntime is the typed side a deployment state machine touches:
// everything answers "present and running", pulls stream one status line.
func deployFakeRuntime() *fake.Runtime {
	rt := &fake.Runtime{}
	rt.NetworkInspectFn = func(context.Context, string, networktypes.InspectOptions) (networktypes.Inspect, error) {
		return networktypes.Inspect{}, nil
	}
	rt.ImageInspectFn = func(context.Context, string, ...client.ImageInspectOption) (imagetypes.InspectResponse, error) {
		return imagetypes.InspectResponse{RepoDigests: []string{"registry.example/app@sha256:feed"}}, nil
	}
	rt.ImagePullFn = func(context.Context, string, imagetypes.PullOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"status":"Pulling"}` + "\n")), nil
	}
	rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
		return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			State: &containertypes.State{Running: true, Status: "running"},
		}}, nil
	}
	rt.ContainerLogsFn = func(context.Context, string, containertypes.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
		return volumetypes.Volume{}, nil
	}
	rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
		return volumetypes.ListResponse{}, nil
	}
	// The proxy applier's verification exec (wget on the Traefik API): empty
	// output with a clean exit — the routing-removal verdicts these flows
	// reach (no domain rows) succeed on absence.
	rt.ContainerExecCreateFn = func(context.Context, string, containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
		return containertypes.ExecCreateResponse{ID: "verify"}, nil
	}
	rt.ContainerExecAttachFn = func(context.Context, string, containertypes.ExecAttachOptions) (types.HijackedResponse, error) {
		client, server := net.Pipe()
		_ = server.Close()
		return types.HijackedResponse{Conn: client, Reader: bufio.NewReader(client)}, nil
	}
	rt.ContainerExecInspectFn = func(context.Context, string) (containertypes.ExecInspect, error) {
		return containertypes.ExecInspect{ExitCode: 0}, nil
	}
	return rt
}

func TestImageDeploymentRunsThroughCompleteStateMachine(t *testing.T) {
	q, keyring, recorder, logger, db := jobFlowDependencies(t)
	db.host, db.port = newJobSSHServer(t).address(t)
	previous := jobEnumValues["DeploymentStatus"]
	jobEnumValues["DeploymentStatus"] = string(store.DeploymentStatusQueued)
	defer func() { jobEnumValues["DeploymentStatus"] = previous }()
	oldStable := deploymentStablePeriod
	deploymentStablePeriod = time.Millisecond
	defer func() { deploymentStablePeriod = oldStable }()

	j := store.Job{
		ID: 3, JobType: TypeDeploymentRun,
		Payload: []byte(`{"deployment_id":1}`),
	}
	result, err := (&DeploymentRun{
		Store: q, Keyring: keyring, Audit: recorder, Logger: logger,
		Docker: fixedSource{rt: deployFakeRuntime()}, HostOps: fixedHost{},
	}).Execute(context.Background(), j, nil)
	if err != nil {
		t.Fatalf("image deployment: %v", err)
	}
	status := result.(map[string]any)["deployment_status"]
	if status != "succeeded" {
		t.Fatalf("deployment result = %#v", result)
	}
}

func TestComposeDeploymentRunsThroughCompleteStateMachine(t *testing.T) {
	oldStable := composeStablePeriod
	composeStablePeriod = time.Millisecond
	defer func() { composeStablePeriod = oldStable }()
	q, keyring, recorder, logger, db := jobFlowDependencies(t)
	db.host, db.port = newJobSSHServer(t).address(t)
	previousStatus := jobEnumValues["DeploymentStatus"]
	previousPack := jobEnumValues["BuildPack"]
	jobEnumValues["DeploymentStatus"] = string(store.DeploymentStatusQueued)
	jobEnumValues["BuildPack"] = string(store.BuildPackCompose)
	defer func() {
		jobEnumValues["DeploymentStatus"] = previousStatus
		jobEnumValues["BuildPack"] = previousPack
	}()

	j := store.Job{
		ID: 4, JobType: TypeDeploymentRun,
		Payload: []byte(`{"deployment_id":1}`),
	}
	result, err := (&DeploymentRun{
		Store: q, Keyring: keyring, Audit: recorder, Logger: logger,
		Docker: fixedSource{rt: deployFakeRuntime()}, HostOps: fixedHost{},
	}).Execute(context.Background(), j, nil)
	if err != nil {
		t.Fatalf("compose deployment: %v", err)
	}
	status := result.(map[string]any)["deployment_status"]
	if status != "succeeded" {
		t.Fatalf("deployment result = %#v", result)
	}
}

func TestComposePreviewDeploymentRunsThroughCompleteStateMachine(t *testing.T) {
	oldStable := composeStablePeriod
	composeStablePeriod = time.Millisecond
	defer func() { composeStablePeriod = oldStable }()
	q, keyring, recorder, logger, db := jobFlowDependencies(t)
	db.host, db.port = newJobSSHServer(t).address(t)
	db.preview = true
	previousStatus := jobEnumValues["DeploymentStatus"]
	previousPack := jobEnumValues["BuildPack"]
	jobEnumValues["DeploymentStatus"] = string(store.DeploymentStatusQueued)
	jobEnumValues["BuildPack"] = string(store.BuildPackCompose)
	defer func() {
		jobEnumValues["DeploymentStatus"] = previousStatus
		jobEnumValues["BuildPack"] = previousPack
	}()

	j := store.Job{
		ID: 8, JobType: TypeDeploymentRun,
		Payload: []byte(`{"deployment_id":1}`),
	}
	result, err := (&DeploymentRun{
		Store: q, Keyring: keyring, Audit: recorder, Logger: logger,
		Docker: fixedSource{rt: deployFakeRuntime()}, HostOps: fixedHost{},
	}).Execute(context.Background(), j, nil)
	if err != nil {
		t.Fatalf("compose preview deployment: %v", err)
	}
	status := result.(map[string]any)["deployment_status"]
	if status != "succeeded" {
		t.Fatalf("deployment result = %#v", result)
	}
}

func TestSourceBuildPacksRunThroughCompleteStateMachine(t *testing.T) {
	for _, pack := range []store.BuildPack{
		store.BuildPackDockerfile,
		store.BuildPackStatic,
		store.BuildPackNixpacks,
	} {
		t.Run(string(pack), func(t *testing.T) {
			q, keyring, recorder, logger, db := jobFlowDependencies(t)
			db.host, db.port = newJobSSHServer(t).address(t)
			previousStatus := jobEnumValues["DeploymentStatus"]
			previousPack := jobEnumValues["BuildPack"]
			jobEnumValues["DeploymentStatus"] = string(store.DeploymentStatusQueued)
			jobEnumValues["BuildPack"] = string(pack)
			oldStable := deploymentStablePeriod
			deploymentStablePeriod = time.Millisecond
			defer func() { deploymentStablePeriod = oldStable }()
			defer func() {
				jobEnumValues["DeploymentStatus"] = previousStatus
				jobEnumValues["BuildPack"] = previousPack
			}()

			j := store.Job{
				ID: 5, JobType: TypeDeploymentRun,
				Payload: []byte(`{"deployment_id":1}`),
			}
			result, err := (&DeploymentRun{
				Store: q, Keyring: keyring, Audit: recorder, Logger: logger,
				Docker: fixedSource{rt: deployFakeRuntime()}, HostOps: fixedHost{},
			}).Execute(context.Background(), j, nil)
			if err != nil {
				t.Fatalf("%s deployment: %v", pack, err)
			}
			status := result.(map[string]any)["deployment_status"]
			if status != "succeeded" {
				t.Fatalf("deployment result = %#v", result)
			}
		})
	}
}

func TestDatabaseActionsRunThroughTheirCompleteStateMachines(t *testing.T) {
	for _, action := range []string{"provision", "start", "restart", "delete"} {
		t.Run(action, func(t *testing.T) {
			q, keyring, _, logger, db := jobFlowDependencies(t)
			db.host, db.port = newJobSSHServer(t).address(t)
			previousProxyState := jobEnumValues["ProxyDesiredState"]
			jobEnumValues["ProxyDesiredState"] = string(store.ProxyDesiredStateStopped)
			defer func() { jobEnumValues["ProxyDesiredState"] = previousProxyState }()
			// The typed side of the flow: a volume that does not exist yet, a
			// container that reports healthy at once, an empty volume sweep.
			rt := &fake.Runtime{}
			rt.VolumeInspectFn = func(context.Context, string) (volumetypes.Volume, error) {
				return volumetypes.Volume{}, fmt.Errorf("no such volume: %w", cerrdefs.ErrNotFound)
			}
			rt.ContainerInspectFn = func(context.Context, string) (containertypes.InspectResponse, error) {
				return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
					State: &containertypes.State{Running: true, Health: &containertypes.Health{Status: "healthy"}},
				}}, nil
			}
			rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
				return volumetypes.ListResponse{}, nil
			}
			payload := `{"resource_id":1,"action":"` + action + `"`
			if action == "delete" {
				payload += `,"delete_volumes":true`
			}
			payload += `}`
			j := store.Job{ID: 6, JobType: "database." + action, Payload: []byte(payload)}
			result, err := (&DatabaseRun{Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{}, Logger: logger}).
				Execute(context.Background(), j, queue.NewStepRecorder(q, j))
			if err != nil {
				t.Fatalf("%s database: %v", action, err)
			}
			if result.(map[string]any)["action"] != action {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestBackupActionsRunThroughTheirCompleteStateMachines(t *testing.T) {
	for _, tc := range []struct {
		name      string
		jobType   string
		execution int64
	}{
		{name: "backup", jobType: TypeBackupExecute},
		{name: "restore", jobType: TypeBackupRestore, execution: 1},
		{name: "drill", jobType: TypeBackupDrill},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, keyring, recorder, logger, db := jobFlowDependencies(t)
			db.host, db.port = newJobSSHServer(t).address(t)
			payload := `{"plan_id":1`
			if tc.execution != 0 {
				payload += `,"execution_id":1`
			}
			payload += `}`
			// The typed side (ADR-054 pipes): exec answers in call order — the
			// engine-version probe / pg_isready, then the table count — and the
			// pipe results carry the fixture's recorded digest.
			rt := verifyRuntime("16.0\n", "3\n")
			rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispec.Platform, string) (containertypes.CreateResponse, error) {
				return containertypes.CreateResponse{ID: "scratch"}, nil
			}
			ops := &hostfake.Ops{
				ExecToFileFn: func(context.Context, agentwire.ExecToFileParams) (agentwire.ExecToFileResult, error) {
					return agentwire.ExecToFileResult{SizeBytes: 128, SHA256: "0123456789abcdef"}, nil
				},
				HashFileFn: func(context.Context, string) (agentwire.FileHashResult, error) {
					return agentwire.FileHashResult{SHA256: "0123456789abcdef", SizeBytes: 128}, nil
				},
			}
			j := store.Job{ID: 7, JobType: tc.jobType, Payload: []byte(payload)}
			result, err := (&BackupRun{
				Store: q, Keyring: keyring, Docker: fixedSource{rt: rt}, HostOps: fixedHost{ops: ops},
				Audit: recorder, Logger: logger,
			}).Execute(context.Background(), j, queue.NewStepRecorder(q, j))
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if result == nil {
				t.Fatalf("%s returned no result", tc.name)
			}
		})
	}
}

var (
	_ store.DBTX = (*jobFlowDB)(nil)
	_ pgx.Rows   = (*jobFlowRows)(nil)
)
