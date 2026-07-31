package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/s3"
	"github.com/deepteams/akerdock/internal/store"
)

// Backup job types (§7.3, §20.5).
const (
	TypeBackupExecute = "backup.execute"
	TypeBackupRestore = "backup.restore"
)

// BackupPayload references the plan (and the execution, for a restore).
type BackupPayload struct {
	PlanID      int64 `json:"plan_id"`
	ExecutionID int64 `json:"execution_id,omitempty"`
}

// BackupRun executes database backups and restores through the agent channel
// (ADR-054 pipes): the dump runs inside the database container and streams
// to the host file agent-side — the payload never crosses the control plane,
// and the credential stays in the container environment, never argv
// (INV-003). The Keyring remains for the S3 credentials, which are only ever
// exchanged for presigned URLs.
type BackupRun struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Docker  dockerruntime.Source
	HostOps hostops.Source
	Audit   *audit.Recorder
	Logger  *slog.Logger
}

// backupDir is the remote backup root (deployment-engine §5.1).
const backupDir = "/var/lib/akerdock/backups"

// backupTarget abstracts what the job dumps: the container of a managed
// database, or the database container of a compose stack component
// (compose-spec §10). Everything downstream — dump, restore, drill,
// retention — speaks in terms of this target.
type backupTarget struct {
	// container is the Docker name the dump execs into.
	container string
	// key names the backup directory and the S3 prefix: the database resource
	// uuid, or the component uuid — stable across redeployments.
	key      string
	serverID int64
	teamID   int64
	// resourceUUID is the event subject: the database resource, or the stack.
	resourceUUID pgtype.UUID
	// login is known for a managed database (credential row). nil for a
	// component: its credentials live in ITS container environment only, and
	// the role/database names are read from there when needed — the password
	// never leaves the container (INV-003).
	login *dbLogin
	// image boots the drill's disposable instance ("" = engine default).
	image string
}

// Execute runs one backup or restore attempt.
func (h *BackupRun) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload BackupPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	plan, err := h.Store.GetBackupPlanByID(ctx, payload.PlanID)
	if err != nil {
		return nil, fmt.Errorf("backup plan not found: %w", err)
	}
	target, err := h.resolveTarget(ctx, plan)
	if err != nil {
		return nil, err
	}

	rt, err := h.Docker.Runtime(ctx, target.serverID)
	if err != nil {
		rec.Fail(ctx, "the server's agent is not connected")
		return nil, err
	}
	ops, err := h.HostOps.HostOps(ctx, target.serverID)
	if err != nil {
		rec.Fail(ctx, "the server's agent is not connected")
		return nil, err
	}

	switch job.JobType {
	case TypeBackupRestore:
		return h.restore(ctx, rec, rt, ops, plan, payload.ExecutionID, target)
	case TypeBackupDrill:
		return h.drill(ctx, rec, rt, ops, plan, target)
	}
	return h.backup(ctx, rec, rt, ops, plan, target)
}

// resolveTarget maps the plan onto its dump target. The three-way CHECK of
// database_backup_plans guarantees exactly one branch.
func (h *BackupRun) resolveTarget(ctx context.Context, plan store.DatabaseBackupPlan) (backupTarget, error) {
	switch {
	case plan.DatabaseID != nil:
		row, err := h.Store.GetDatabaseByID(ctx, *plan.DatabaseID)
		if err != nil {
			return backupTarget{}, fmt.Errorf("database not found: %w", err)
		}
		login := credentialsOf(row)
		t := backupTarget{
			container: pguuid.String(row.Resource.Uuid), key: pguuid.String(row.Resource.Uuid),
			serverID: row.Database.ServerID, teamID: row.Resource.TeamID,
			resourceUUID: row.Resource.Uuid, login: &login,
		}
		if row.Database.Image != nil {
			t.image = *row.Database.Image
		}
		return t, nil
	case plan.ServiceComponentID != nil:
		row, err := h.Store.GetComponentBackupTarget(ctx, *plan.ServiceComponentID)
		if err != nil {
			return backupTarget{}, fmt.Errorf("stack component not found: %w", err)
		}
		sc := row.ServiceComponent
		// Validated at plan creation and re-checked here: a compose edit can
		// swap the image under an existing plan (compose-spec §10).
		if !sc.IsDatabase || sc.DatabaseEngine == nil || *sc.DatabaseEngine != store.DbEnginePostgresql {
			return backupTarget{}, fmt.Errorf("component %q is not a supported database (postgresql only in v1, compose-spec §10)", sc.Name)
		}
		t := backupTarget{
			// The component container name derives from the stack (§2.2).
			container: pguuid.String(row.StackUuid) + "-" + sc.Name,
			key:       pguuid.String(sc.Uuid),
			serverID:  row.ServerID, teamID: row.TeamID,
			resourceUUID: row.StackUuid,
		}
		if sc.Image != nil {
			t.image = *sc.Image
		}
		return t, nil
	}
	return backupTarget{}, fmt.Errorf("this plan has no dump target")
}

// backup dumps the database, records size, checksum and engine version, then
// applies the local retention. An S3 upload failure yields the explicit
// `partial` status — never a silent success (§20.5).
func (h *BackupRun) backup(ctx context.Context, rec *queue.StepRecorder, rt dockerruntime.Runtime, ops hostops.Ops,
	plan store.DatabaseBackupPlan, target backupTarget,
) (any, error) {
	u, err := pguuid.New()
	if err != nil {
		return nil, err
	}
	exec, err := h.Store.CreateBackupExecution(ctx, store.CreateBackupExecutionParams{
		Uuid: u, BackupPlanID: plan.ID,
	})
	if err != nil {
		return nil, err
	}
	fail := func(step string, cause error) (any, error) {
		msg := firstLine(cause.Error())
		_ = h.Store.FinishBackupExecution(ctx, store.FinishBackupExecutionParams{
			ID: exec.ID, Status: store.BackupExecutionStatusFailed, ErrorMessage: &msg,
		})
		rec.Fail(ctx, step+": "+msg)
		return nil, cause
	}

	rec.Start(ctx, "dump")
	dir := backupDir + "/" + target.key
	stamp := time.Now().UTC().Format("20060102-150405")
	file := fmt.Sprintf("%s/%s-%s.sql.gz", dir, target.key, stamp)

	// The `${VAR:-default}` forms mirror the official postgres image: a
	// compose component often relies on those defaults instead of setting the
	// variables explicitly, and a managed container always sets them. The
	// shell below runs in the DATABASE container, not on the host.
	dumpCmd := "pg_dump -U \"${POSTGRES_USER:-postgres}\" -d \"${POSTGRES_DB:-${POSTGRES_USER:-postgres}}\""
	if plan.DumpAll {
		dumpCmd = "pg_dumpall -U \"${POSTGRES_USER:-postgres}\""
	}
	// The dump runs inside the container and streams to the host file
	// agent-side, gzipped and hashed as written (ADR-054 pipes): the
	// credential stays in the container environment (INV-003), and the size,
	// checksum and emptiness verdicts come typed with the result.
	res, err := ops.ExecToFile(ctx, agentwire.ExecToFileParams{
		Container: target.container, Cmd: []string{"sh", "-c", dumpCmd},
		Path: file, Mode: 0o600, MakeDirs: true, DirMode: 0o700, Gzip: true,
	})
	if err != nil {
		return fail("dump", err)
	}
	if res.ExitCode != 0 {
		return fail("dump", fmt.Errorf("pg_dump exited with code %d: %s", res.ExitCode, firstLine(res.Stderr)))
	}
	if res.SizeBytes == 0 {
		return fail("dump", fmt.Errorf("the dump came out empty"))
	}
	size := &res.SizeBytes
	checksum := ptrStr(res.SHA256)
	// The engine version: integrity metadata verified at restore and during
	// the drills (§20.5). Best-effort, exactly as the old `|| true` was.
	var version *string
	if out, exit, verr := execCapture(ctx, rt, target.container,
		[]string{"sh", "-c", `psql -U "${POSTGRES_USER:-postgres}" -tAc "SHOW server_version"`}); verr == nil && exit == 0 {
		if v := strings.TrimSpace(out); v != "" {
			version = &v
		}
	}

	// How many tables the SOURCE database held at dump time. This is the
	// reference the restore drill compares against: a dump can gunzip cleanly,
	// restore without an error and contain nothing — an empty psql restore
	// exits 0. Without a number to compare, a drill can only prove that
	// nothing crashed.
	if count, err := h.countTables(ctx, rt, target.container, target.login); err == nil {
		if err := h.Store.SetBackupExecutionTableCount(ctx, store.SetBackupExecutionTableCountParams{
			ID: exec.ID, TableCount: &count,
		}); err != nil {
			h.Logger.Warn("cannot record the table count", "error", err)
		}
	}
	rec.Succeed(ctx, "dump written")

	// The dump exists locally: from here on, a failure degrades the result
	// instead of destroying it. An S3 upload that fails yields `partial` —
	// the backup is real, only its copy is missing (§20.5).
	status := store.BackupExecutionStatusSucceeded
	var s3Err, s3Key *string
	uploaded := false
	if plan.S3StorageID != nil {
		rec.Start(ctx, "upload_s3")
		key, err := h.uploadToS3(ctx, ops, plan, target.key, file, size)
		switch {
		case err != nil:
			msg := firstLine(err.Error())
			s3Err, status = &msg, store.BackupExecutionStatusPartial
			rec.Fail(ctx, "upload failed, the dump is kept locally: "+msg)
		default:
			s3Key, uploaded = &key, true
			rec.Succeed(ctx, "uploaded to "+key)
		}
	}

	// s3_only drops the local copy — but only once the object is confirmed in
	// the bucket. A failed upload always keeps the dump on disk (INV-008). The
	// row keeps the path either way: it is where a restore puts the file back.
	localDropped := false
	if plan.S3Only && uploaded {
		if err := ops.Remove(ctx, agentwire.FileRemoveParams{Path: file}); err == nil {
			localDropped = true
		}
	}

	if err := h.Store.FinishBackupExecution(ctx, store.FinishBackupExecutionParams{
		ID: exec.ID, Status: status, Filename: &file, SizeBytes: size,
		ChecksumSha256: checksum, EngineVersion: version,
		UploadedToS3: uploaded, S3UploadError: s3Err, S3Key: s3Key,
	}); err != nil {
		return nil, err
	}
	if localDropped {
		_ = h.Store.MarkBackupLocalDeleted(ctx, exec.ID)
	}

	h.applyRetention(ctx, ops, plan)
	h.Logger.Info("backup completed", "target", target.container, "status", status, "file", file)
	return map[string]any{
		"execution_uuid": pguuid.String(exec.Uuid),
		"status":         string(status),
		"size_bytes":     size,
	}, nil
}

// dbLogin is the role and database a psql call needs. It exists so the drill
// and the backup speak of the same thing.
type dbLogin struct{ User, DB string }

// credentialsOf reads the role and database name of a managed database. The
// database name defaults to the role, which is what PostgreSQL itself does.
func credentialsOf(row store.GetDatabaseByIDRow) dbLogin {
	login := dbLogin{User: row.DatabaseCredential.Username, DB: row.DatabaseCredential.Username}
	if row.DatabaseCredential.DbName != nil && *row.DatabaseCredential.DbName != "" {
		login.DB = *row.DatabaseCredential.DbName
	}
	return login
}

// countTables counts the user tables of a running PostgreSQL container. The
// system schemas are excluded: they exist in every database and would make an
// empty restore look populated.
//
// With a known login (managed database, drill scratch) the role and database
// are passed as pure argv. With none (stack component, compose-spec §10) an
// in-container shell resolves them from the container's own environment —
// nothing is read out of the container.
func (h *BackupRun) countTables(ctx context.Context, rt dockerruntime.Runtime, container string, c *dbLogin) (int32, error) {
	const q = "SELECT count(*) FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema')"
	var cmd []string
	if c != nil {
		cmd = []string{"psql", "-U", c.User, "-d", c.DB, "-tAc", q}
	} else {
		// The query rides double-quoted inside the in-container shell: it
		// contains single quotes, and carries no `$` for the shell to expand.
		cmd = []string{
			"sh", "-c",
			`psql -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB:-${POSTGRES_USER:-postgres}}" -tAc "` + q + `"`,
		}
	}
	out, exit, err := execCapture(ctx, rt, container, cmd)
	if err != nil {
		return 0, err
	}
	if exit != 0 {
		return 0, fmt.Errorf("counting tables failed: %s", firstLine(out))
	}
	n, err := strconv.Atoi(strings.TrimSpace(firstLine(out)))
	if err != nil {
		return 0, err
	}
	return int32(n), nil
}

// applyRetention purges the dumps that fell outside the plan's rules, on the
// server and in the bucket. The two are independent (a plan may keep 3 local
// dumps and 30 in S3), and neither ever removes the last successful backup
// (§7.2). A purge that fails is logged, never fatal: the backup that just
// succeeded must not be reported as failed because an old file resisted.
func (h *BackupRun) applyRetention(ctx context.Context, ops hostops.Ops, plan store.DatabaseBackupPlan) {
	h.applyLocalRetention(ctx, ops, plan)
	h.applyS3Retention(ctx, plan)
}

func (h *BackupRun) applyLocalRetention(ctx context.Context, ops hostops.Ops, plan store.DatabaseBackupPlan) {
	if plan.RetentionLocalMaxCount <= 0 && plan.RetentionLocalMaxDays <= 0 {
		return // 0/0 = unlimited
	}
	expired, err := h.Store.ListExpiredLocalBackups(ctx, store.ListExpiredLocalBackupsParams{
		BackupPlanID: plan.ID,
		KeepCount:    plan.RetentionLocalMaxCount,
		MaxDays:      plan.RetentionLocalMaxDays,
	})
	if err != nil {
		h.Logger.Warn("local retention query failed", "error", err)
		return
	}
	for _, e := range expired {
		if e.Filename == nil {
			continue
		}
		if err := ops.Remove(ctx, agentwire.FileRemoveParams{Path: *e.Filename}); err != nil {
			h.Logger.Warn("could not delete an expired backup", "file", *e.Filename, "error", err)
			continue
		}
		_ = h.Store.MarkBackupLocalDeleted(ctx, e.ID)
	}
}

// applyS3Retention deletes the expired objects from the bucket. These are
// small DELETE calls, so the instance issues them itself rather than routing
// them through the target server.
func (h *BackupRun) applyS3Retention(ctx context.Context, plan store.DatabaseBackupPlan) {
	if plan.S3StorageID == nil || (plan.RetentionS3MaxCount <= 0 && plan.RetentionS3MaxDays <= 0) {
		return
	}
	expired, err := h.Store.ListExpiredS3Backups(ctx, store.ListExpiredS3BackupsParams{
		BackupPlanID: plan.ID,
		KeepCount:    plan.RetentionS3MaxCount,
		MaxDays:      plan.RetentionS3MaxDays,
	})
	if err != nil {
		h.Logger.Warn("s3 retention query failed", "error", err)
		return
	}
	if len(expired) == 0 {
		return
	}
	s3c, err := h.s3ClientFor(ctx, *plan.S3StorageID)
	if err != nil {
		h.Logger.Warn("s3 retention: unusable storage", "error", err)
		return
	}
	for _, e := range expired {
		if e.S3Key == nil {
			continue
		}
		if err := s3c.Delete(ctx, *e.S3Key); err != nil {
			h.Logger.Warn("could not delete an expired object", "key", *e.S3Key, "error", err)
			continue
		}
		_ = h.Store.MarkBackupS3Deleted(ctx, e.ID)
	}
}

// restore verifies the dump checksum, then replays it into the database.
// The integrity check comes first: a corrupted dump is never restored
// (§20.5).
func (h *BackupRun) restore(ctx context.Context, rec *queue.StepRecorder, _ dockerruntime.Runtime, ops hostops.Ops,
	plan store.DatabaseBackupPlan, executionID int64, target backupTarget,
) (any, error) {
	exec, err := h.Store.GetBackupExecutionByID(ctx, executionID)
	if err != nil {
		return nil, fmt.Errorf("backup execution not found: %w", err)
	}
	if exec.Filename == nil {
		return nil, fmt.Errorf("this backup produced no dump")
	}
	// The local dump may be gone (s3_only, or a retention purge): fetch it
	// back from the bucket. The checksum below then verifies what came back —
	// a corrupted object is caught exactly like a corrupted local file.
	if exec.LocalDeletedAt.Valid {
		if exec.S3Key == nil || exec.S3DeletedAt.Valid {
			return nil, fmt.Errorf("the dump of this backup is gone — no local copy, and none left in the bucket")
		}
		rec.Start(ctx, "fetch_s3")
		if err := h.fetchFromS3(ctx, ops, plan, exec); err != nil {
			rec.Fail(ctx, err.Error())
			return nil, err
		}
		rec.Succeed(ctx, "dump downloaded from "+*exec.S3Key)
	}

	rec.Start(ctx, "verify_checksum")
	if exec.ChecksumSha256 != nil {
		digest, err := ops.HashFile(ctx, *exec.Filename)
		if err != nil {
			return nil, err
		}
		if digest.SHA256 != *exec.ChecksumSha256 {
			rec.Fail(ctx, "checksum mismatch — the dump is corrupted, restore aborted")
			return nil, fmt.Errorf("checksum mismatch: the dump is corrupted (expected %s, got %s)", *exec.ChecksumSha256, digest.SHA256)
		}
	}
	rec.Succeed(ctx, "dump integrity verified")

	rec.Start(ctx, "restore")
	// The dump gunzips agent-side straight into psql's stdin; the in-container
	// shell resolves the login from the container's own environment (INV-003).
	res, err := ops.FileToExec(ctx, agentwire.FileToExecParams{
		Path: *exec.Filename, Gunzip: true, Container: target.container,
		Cmd: []string{"sh", "-c", `psql -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB:-${POSTGRES_USER:-postgres}}" -v ON_ERROR_STOP=1 >/dev/null`},
	})
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	if res.ExitCode != 0 {
		rec.Fail(ctx, "psql restore failed: "+firstLine(res.Output))
		return nil, fmt.Errorf("restore failed with code %d: %s", res.ExitCode, firstLine(res.Output))
	}
	rec.Succeed(ctx, "database restored")
	h.Logger.Info("backup restored", "target", target.container, "execution_uuid", pguuid.String(exec.Uuid))
	return map[string]any{"restored_from": pguuid.String(exec.Uuid)}, nil
}

// presignTTL bounds the life of an upload/download URL. It must outlast the
// transfer of a large dump, not the job.
const presignTTL = 2 * time.Hour

// s3ClientFor decrypts the credentials of a storage and builds a client. The
// credentials stay in this process: what reaches the target server is a
// presigned URL, never a key (INV-003).
func (h *BackupRun) s3ClientFor(ctx context.Context, storageID int64) (*s3.Client, error) {
	storage, err := h.Store.GetS3StorageByID(ctx, storageID)
	if err != nil {
		return nil, fmt.Errorf("s3 storage not found: %w", err)
	}
	uuid := pguuid.String(storage.Uuid)
	access, err := h.Keyring.Decrypt("s3_storages", "access_key_enc", uuid, storage.AccessKeyEnc)
	if err != nil {
		return nil, err
	}
	secret, err := h.Keyring.Decrypt("s3_storages", "secret_key_enc", uuid, storage.SecretKeyEnc)
	if err != nil {
		return nil, err
	}
	cfg := s3.Config{
		Endpoint: storage.Endpoint, Bucket: storage.Bucket,
		AccessKey: string(access), SecretKey: string(secret),
	}
	if storage.Region != nil {
		cfg.Region = *storage.Region
	}
	if storage.PathPrefix != nil {
		cfg.PathPrefix = *storage.PathPrefix
	}
	if storage.SseAlgorithm != nil {
		cfg.SSEAlgorithm = *storage.SseAlgorithm
	}
	return s3.New(cfg), nil
}

// uploadToS3 pushes the dump straight from the target server to the bucket
// and returns the object key.
//
// The transfer never goes through the control plane: a dump can be tens of
// gigabytes, and relaying it would make every backup depend on the
// instance's bandwidth. The instance only signs a URL, which travels to the
// agent in the typed command body over the encrypted channel — a presigned
// URL is a credential for that one object, so it must not land in argv, in
// `ps`, or in a shell history (INV-003).
//
// The upload is confirmed by asking the bucket for the object's size: a
// clean PUT only says the request was sent.
func (h *BackupRun) uploadToS3(ctx context.Context, ops hostops.Ops,
	plan store.DatabaseBackupPlan, dbUUID, file string, size *int64,
) (string, error) {
	s3c, err := h.s3ClientFor(ctx, *plan.S3StorageID)
	if err != nil {
		return "", err
	}
	key := s3c.Key(dbUUID + "/" + filepath.Base(file))
	url, err := s3c.PresignPut(key, presignTTL)
	if err != nil {
		return "", err
	}
	// When the storage requests server-side encryption, the matching header
	// must be sent — it was signed into the presigned URL.
	headers := map[string]string{}
	if sseHeader, ok := s3c.SSEHeader(); ok {
		if name, value, found := strings.Cut(sseHeader, ": "); found {
			headers[name] = value
		}
	}
	if err := ops.FileToURL(ctx, agentwire.FileToURLParams{Path: file, URL: url, Headers: headers}); err != nil {
		// The body of an S3 error names the cause (AccessDenied, NoSuchBucket…)
		// and carries no credential — the URL never appears in it.
		return "", fmt.Errorf("upload failed: %s", firstLine(err.Error()))
	}

	remote, exists, err := s3c.Size(ctx, key)
	if err != nil {
		return "", fmt.Errorf("could not confirm the upload: %w", err)
	}
	if !exists {
		return "", fmt.Errorf("the object is absent from the bucket after the upload")
	}
	if size != nil && remote != *size {
		return "", fmt.Errorf("the uploaded object is truncated (%d bytes in the bucket, %d locally)", remote, *size)
	}
	return key, nil
}

// fetchFromS3 downloads an object back onto the target server, at the path
// the execution recorded. Used when the local dump is gone (s3_only, or a
// retention purge) — the checksum is verified afterwards, as for any restore.
func (h *BackupRun) fetchFromS3(ctx context.Context, ops hostops.Ops,
	plan store.DatabaseBackupPlan, exec store.BackupExecution,
) error {
	if exec.S3Key == nil || plan.S3StorageID == nil {
		return fmt.Errorf("this backup has no copy in a bucket")
	}
	s3c, err := h.s3ClientFor(ctx, *plan.S3StorageID)
	if err != nil {
		return err
	}
	url, err := s3c.PresignGet(*exec.S3Key, presignTTL)
	if err != nil {
		return err
	}
	if err := ops.URLToFile(ctx, agentwire.URLToFileParams{
		URL: url, Path: *exec.Filename, Mode: 0o600, MakeDirs: true, DirMode: 0o700,
	}); err != nil {
		return fmt.Errorf("download failed: %s", firstLine(err.Error()))
	}
	return nil
}
