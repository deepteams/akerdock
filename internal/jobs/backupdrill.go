package jobs

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeBackupDrill restores the latest dump into a disposable database and
// checks what came back (ADR-014, §20.5).
const TypeBackupDrill = "backup.drill"

// drillBoot bounds the wait for the throwaway PostgreSQL to accept connections.
const drillBoot = 90 * time.Second

// Drill runs one restore drill.
//
// The drill answers a question no backup job can answer about itself: is this
// dump restorable? It restores into a container of its own — never into the
// live database, which is the whole point: a drill that touched production
// would be an outage waiting for a cron.
//
// It fails LOUDLY. A plan whose drill fails publishes backup.drill_failed.v1
// (critical by suffix, ADR-019), because the failure mode this feature exists
// to catch is precisely the one nobody looks for: backups that ran green for
// months and restore into nothing.
func (h *BackupRun) drill(ctx context.Context, rec *queue.StepRecorder, client *sshexec.Client,
	plan store.DatabaseBackupPlan, row store.GetDatabaseByIDRow,
) (any, error) {
	exec, err := h.Store.GetLatestSuccessfulBackupExecution(ctx, plan.ID)
	if err != nil {
		return nil, fmt.Errorf("this plan has no successful backup to restore — there is nothing to prove yet")
	}
	drill, err := h.Store.CreateRestoreDrill(ctx, store.CreateRestoreDrillParams{
		PlanID: plan.ID, ExecutionID: &exec.ID, TablesExpected: exec.TableCount,
	})
	if err != nil {
		return nil, err
	}

	// A drill that fails is a RESULT, not a job to retry: the dump will not
	// become restorable by trying again in a minute, and the alert has already
	// been raised. So `fail` returns a verdict, never an error — returning one
	// would dead-letter a job whose conclusion is already recorded.
	fail := func(step string, cause error) any {
		msg := firstLine(cause.Error())
		_ = h.Store.FinishRestoreDrill(ctx, store.FinishRestoreDrillParams{
			ID: drill.ID, Status: store.RestoreDrillStatusFailed, ErrorMessage: &msg,
		})
		_ = h.Store.SetPlanDrillResult(ctx, store.SetPlanDrillResultParams{
			ID: plan.ID, LastDrillStatus: ptrOf(store.RestoreDrillStatusFailed),
		})
		h.publishDrill(ctx, row, plan, "backup.drill_failed.v1", map[string]any{
			"reason": msg, "step": step,
		})
		rec.Fail(ctx, step+": "+msg)
		return map[string]any{"status": "failed", "reason": msg}
	}

	dbUUID := pguuid.String(row.Resource.Uuid)
	scratch := "akerdock-drill-" + pguuid.String(drill.Uuid)[:12]
	// Destroyed whatever happens — including on a failure, where the temptation
	// to "leave it for inspection" would leave a stray database with production
	// data on the server.
	defer func() {
		if _, err := client.Run(context.WithoutCancel(ctx), "docker rm -f "+scratch+" >/dev/null 2>&1"); err != nil {
			h.Logger.Warn("the drill container could not be removed", "container", scratch, "error", err)
		}
	}()

	if exec.Filename == nil {
		return fail("prepare", fmt.Errorf("the backup produced no dump")), nil
	}
	// The local dump may be gone (s3_only, retention): fetch it back. The
	// checksum below verifies whatever came back, exactly as a restore does.
	if exec.LocalDeletedAt.Valid {
		if exec.S3Key == nil || exec.S3DeletedAt.Valid {
			return fail("fetch", fmt.Errorf("the dump is gone — no local copy, and none left in the bucket")), nil
		}
		rec.Start(ctx, "fetch_s3")
		if err := h.fetchFromS3(ctx, client, plan, exec); err != nil {
			return fail("fetch", err), nil
		}
		rec.Succeed(ctx, "dump downloaded from "+*exec.S3Key)
	}

	rec.Start(ctx, "verify_checksum")
	if exec.ChecksumSha256 != nil {
		res, err := client.Run(ctx, "sha256sum "+*exec.Filename+" | cut -d' ' -f1")
		if err != nil {
			return fail("verify_checksum", err), nil
		}
		if got := firstLine(res.Stdout); got != *exec.ChecksumSha256 {
			return fail("verify_checksum", fmt.Errorf("checksum mismatch: the dump is corrupted (expected %s, got %s)",
				*exec.ChecksumSha256, got)), nil
		}
	}
	rec.Succeed(ctx, "dump integrity verified")

	// A disposable instance: no volume, no published port, its own password.
	//
	// It runs the SAME image as the source, and — this is what the first version
	// of the drill got wrong — the same ROLE and database NAME. A plain pg_dump
	// carries `ALTER TABLE … OWNER TO app` and GRANTs: restored into a database
	// whose only role is some `drill` user, every one of those statements errors
	// out. The drill would then report a corrupted backup for a backup that is
	// perfectly fine, which is worse than not drilling at all.
	rec.Start(ctx, "boot_scratch")
	password, err := drillPassword()
	if err != nil {
		return fail("boot_scratch", err), nil
	}
	image := "postgres:16-alpine"
	if row.Database.Image != nil && *row.Database.Image != "" {
		image = *row.Database.Image
	}
	login := credentialsOf(row)
	res, err := client.Run(ctx, fmt.Sprintf(
		"docker run -d --rm --name %s -e POSTGRES_PASSWORD=%s -e POSTGRES_USER=%s -e POSTGRES_DB=%s %s",
		scratch, shellQuote(password), shellQuote(login.User), shellQuote(login.DB), shellQuote(image)))
	if err != nil {
		return fail("boot_scratch", err), nil
	}
	if res.ExitCode != 0 {
		return fail("boot_scratch", fmt.Errorf("cannot start the disposable database: %s", firstLine(res.Stderr))), nil
	}
	if err := h.waitReady(ctx, client, scratch, login.User); err != nil {
		return fail("boot_scratch", err), nil
	}
	rec.Succeed(ctx, "disposable database ready")

	rec.Start(ctx, "restore")
	// ON_ERROR_STOP is what turns "the restore printed some errors" into "the
	// restore failed". Without it psql exits 0 on a dump it only half applied.
	res, err = client.Run(ctx, fmt.Sprintf(
		"gunzip -c %s | docker exec -i %s psql -U %s -d %s -v ON_ERROR_STOP=1 >/dev/null",
		*exec.Filename, scratch, shellQuote(login.User), shellQuote(login.DB)))
	if err != nil {
		return fail("restore", err), nil
	}
	if res.ExitCode != 0 {
		return fail("restore", fmt.Errorf("the dump did not restore: %s", firstLine(res.Stderr))), nil
	}
	rec.Succeed(ctx, "dump restored into the disposable database")

	rec.Start(ctx, "verify_content")
	restored, err := h.countTables(ctx, client, scratch, login)
	if err != nil {
		return fail("verify_content", err), nil
	}
	// The comparison is the drill. Without a reference count (a backup taken
	// before this feature existed), an empty restore cannot be told from an
	// empty database — so it is called out rather than passed.
	if exec.TableCount == nil {
		if restored == 0 {
			return fail("verify_content", fmt.Errorf("the restored database is empty and the backup recorded no table count — the dump cannot be vouched for")), nil
		}
	} else if restored != *exec.TableCount {
		return fail("verify_content", fmt.Errorf("the restore came back with %d tables, the source had %d at dump time",
			restored, *exec.TableCount)), nil
	}

	if err := h.Store.FinishRestoreDrill(ctx, store.FinishRestoreDrillParams{
		ID: drill.ID, Status: store.RestoreDrillStatusSucceeded, TablesRestored: &restored,
	}); err != nil {
		return nil, err
	}
	if err := h.Store.SetPlanDrillResult(ctx, store.SetPlanDrillResultParams{
		ID: plan.ID, LastDrillStatus: ptrOf(store.RestoreDrillStatusSucceeded),
	}); err != nil {
		return nil, err
	}
	rec.Succeed(ctx, fmt.Sprintf("%d tables restored and verified", restored))
	h.Logger.Info("restore drill succeeded", "db_uuid", dbUUID, "tables", restored)
	return map[string]any{
		"status": "succeeded", "tables_restored": restored,
		"drill_uuid": pguuid.String(drill.Uuid),
	}, nil
}

// ptrOf is the local generic pointer helper (the store takes pointers for
// nullable enums).
func ptrOf[T any](v T) *T { return &v }

// drillPassword mints a throwaway credential for a container that lives for the
// length of one drill and publishes no port. It is still random: a predictable
// password on a database holding production data is a bad idea even for 30
// seconds.
func drillPassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// waitReady polls the disposable instance until it accepts connections.
func (h *BackupRun) waitReady(ctx context.Context, client *sshexec.Client, container, user string) error {
	deadline := time.Now().Add(drillBoot)
	for time.Now().Before(deadline) {
		res, err := client.Run(ctx, fmt.Sprintf("docker exec %s pg_isready -U %s", container, shellQuote(user)))
		if err != nil {
			return err
		}
		if res.ExitCode == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("the disposable database never became ready within %s", drillBoot)
}

func (h *BackupRun) publishDrill(ctx context.Context, row store.GetDatabaseByIDRow, plan store.DatabaseBackupPlan, event string, payload map[string]any) {
	if h.Audit == nil {
		return
	}
	var teamUUID pgtype.UUID
	if team, err := h.Store.GetTeamByID(ctx, row.Resource.TeamID); err == nil {
		teamUUID = team.Uuid
	}
	payload["plan_uuid"] = pguuid.String(plan.Uuid)
	payload["database_uuid"] = pguuid.String(row.Resource.Uuid)
	h.Audit.Outbox(ctx, h.Store, event, teamUUID, row.Resource.Uuid,
		"backup_plan:"+pguuid.String(plan.Uuid), payload)
}
