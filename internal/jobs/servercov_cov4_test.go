package jobs

// Coverage tests for backup.go and backupdrill.go: target resolution, the
// dump/upload/retention ladder against a loopback S3, the restore integrity
// checks, and the drill verdict machine.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	networktypes "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/jackc/pgx/v5/pgtype"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/envelope"
	hostfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// servercovExecRuntime scripts container execs with per-call outputs AND
// exit codes (verifyRuntime always answers exit 0).
func servercovExecRuntime(outputs []string, exits []int) *fake.Runtime {
	rt := &fake.Runtime{}
	attach, inspect := 0, 0
	rt.ContainerExecCreateFn = func(context.Context, string, containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
		return containertypes.ExecCreateResponse{ID: "exec"}, nil
	}
	rt.ContainerExecAttachFn = func(context.Context, string, containertypes.ExecAttachOptions) (types.HijackedResponse, error) {
		out := outputs[min(attach, len(outputs)-1)]
		attach++
		var buf bytes.Buffer
		_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stdout).Write([]byte(out))
		client, server := net.Pipe()
		go func() {
			_, _ = server.Write(buf.Bytes())
			_ = server.Close()
		}()
		return types.HijackedResponse{Conn: client, Reader: bufio.NewReader(client)}, nil
	}
	rt.ContainerExecInspectFn = func(context.Context, string) (containertypes.ExecInspect, error) {
		exit := exits[min(inspect, len(exits)-1)]
		inspect++
		return containertypes.ExecInspect{ExitCode: exit}, nil
	}
	rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispec.Platform, string) (containertypes.CreateResponse, error) {
		return containertypes.CreateResponse{ID: "scratch"}, nil
	}
	return rt
}

// servercovBackupOps is the happy-path host side of a backup: a dump that
// lands with a known size and checksum, matching hash verification.
func servercovBackupOps() *hostfake.Ops {
	return &hostfake.Ops{
		ExecToFileFn: func(context.Context, agentwire.ExecToFileParams) (agentwire.ExecToFileResult, error) {
			return agentwire.ExecToFileResult{SizeBytes: 128, SHA256: "0123456789abcdef"}, nil
		},
		HashFileFn: func(context.Context, string) (agentwire.FileHashResult, error) {
			return agentwire.FileHashResult{SHA256: "0123456789abcdef", SizeBytes: 128}, nil
		},
	}
}

// servercovS3Server answers HEAD (size) and DELETE for one bucket. The
// returned mutators steer the answers per test.
type servercovS3State struct {
	headStatus int
	headLength string
	delStatus  int
}

func servercovNewS3(t *testing.T) (*httptest.Server, *servercovS3State) {
	t.Helper()
	state := &servercovS3State{headStatus: http.StatusOK, headLength: "128", delStatus: http.StatusNoContent}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			if state.headStatus != http.StatusOK {
				w.WriteHeader(state.headStatus)
				return
			}
			w.Header().Set("Content-Length", state.headLength)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			w.WriteHeader(state.delStatus)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, state
}

// servercovS3Storage overrides GetS3StorageByID so the decrypted credentials
// and the endpoint point at the loopback bucket.
func servercovS3Storage(t *testing.T, db *servercovDB, keyring *envelope.Keyring, endpoint string) {
	t.Helper()
	access := servercovEncrypt(t, keyring, "s3_storages", "access_key_enc", "AK")
	secret := servercovEncrypt(t, keyring, "s3_storages", "secret_key_enc", "SK")
	db.rowAfter["GetS3StorageByID"] = servercovOverride(map[int]func(any){
		4:  servercovVal(endpoint),
		5:  servercovPtr("us-east-1"),
		8:  servercovBytes(access),
		9:  servercovBytes(secret),
		16: servercovPtr("AES256"),
	})
}

func TestServercovBackupResolveTargetBranches(t *testing.T) {
	planWith := func(db *servercovDB, overrides map[int]func(any)) {
		db.rowAfter["GetBackupPlanByID"] = servercovOverride(overrides)
	}
	run := func(t *testing.T, db *servercovDB, q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger) (any, error) {
		job := store.Job{ID: 30, JobType: TypeBackupExecute, Payload: []byte(`{"plan_id":1}`)}
		h := &BackupRun{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: verifyRuntime("16.0\n", "3\n")}, HostOps: fixedHost{ops: servercovBackupOps()},
		}
		return h.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	}

	t.Run("plan not found", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		db.rowErr["GetBackupPlanByID"] = errors.New("gone")
		if _, err := run(t, db, q, keyring, logger); err == nil || !strings.Contains(err.Error(), "backup plan not found") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("database not found", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		db.rowErr["GetDatabaseByID"] = errors.New("gone")
		if _, err := run(t, db, q, keyring, logger); err == nil || !strings.Contains(err.Error(), "database not found") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("component not found", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		planWith(db, map[int]func(any){2: servercovNilPtr[int64](), 3: servercovPtr(int64(1))})
		db.rowErr["GetComponentBackupTarget"] = errors.New("gone")
		if _, err := run(t, db, q, keyring, logger); err == nil || !strings.Contains(err.Error(), "stack component not found") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("component is not a database", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		planWith(db, map[int]func(any){2: servercovNilPtr[int64](), 3: servercovPtr(int64(1))})
		db.rowAfter["GetComponentBackupTarget"] = servercovOverride(map[int]func(any){
			5: servercovVal(false),
		})
		if _, err := run(t, db, q, keyring, logger); err == nil || !strings.Contains(err.Error(), "not a supported database") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("no dump target at all", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		planWith(db, map[int]func(any){2: servercovNilPtr[int64](), 3: servercovNilPtr[int64]()})
		if _, err := run(t, db, q, keyring, logger); err == nil || !strings.Contains(err.Error(), "no dump target") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("component backup succeeds with in-container login", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		planWith(db, map[int]func(any){2: servercovNilPtr[int64](), 3: servercovPtr(int64(1))})
		engine := store.DbEnginePostgresql
		db.rowAfter["GetComponentBackupTarget"] = servercovOverride(map[int]func(any){
			4: servercovPtr("postgres:16-alpine"),
			5: servercovVal(true),
			6: servercovPtr(engine),
		})
		result, err := run(t, db, q, keyring, logger)
		if err != nil {
			t.Fatal(err)
		}
		if result.(map[string]any)["status"] != string(store.BackupExecutionStatusSucceeded) {
			t.Fatalf("result = %#v", result)
		}
	})
}

func TestServercovBackupDumpFailureLadder(t *testing.T) {
	tests := map[string]struct {
		ops  *hostfake.Ops
		prep func(db *servercovDB)
		want string
	}{
		"execution row cannot be created": {
			ops:  servercovBackupOps(),
			prep: func(db *servercovDB) { db.rowErr["CreateBackupExecution"] = errors.New("insert refused") },
			want: "insert refused",
		},
		"dump transport failure": {
			ops: &hostfake.Ops{ExecToFileFn: func(context.Context, agentwire.ExecToFileParams) (agentwire.ExecToFileResult, error) {
				return agentwire.ExecToFileResult{}, errors.New("channel lost")
			}},
			want: "channel lost",
		},
		"dump exits non-zero": {
			ops: &hostfake.Ops{ExecToFileFn: func(context.Context, agentwire.ExecToFileParams) (agentwire.ExecToFileResult, error) {
				return agentwire.ExecToFileResult{ExitCode: 2, Stderr: "role does not exist"}, nil
			}},
			want: "pg_dump exited with code 2",
		},
		"dump is empty": {
			ops: &hostfake.Ops{ExecToFileFn: func(context.Context, agentwire.ExecToFileParams) (agentwire.ExecToFileResult, error) {
				return agentwire.ExecToFileResult{SizeBytes: 0}, nil
			}},
			want: "came out empty",
		},
		"finish cannot be recorded": {
			ops:  servercovBackupOps(),
			prep: func(db *servercovDB) { db.execErr["FinishBackupExecution"] = errors.New("finish refused") },
			want: "finish refused",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			q, keyring, _, logger, db := servercovDeps(t)
			if tc.prep != nil {
				tc.prep(db)
			}
			job := store.Job{ID: 31, JobType: TypeBackupExecute, Payload: []byte(`{"plan_id":1}`)}
			h := &BackupRun{
				Store: q, Keyring: keyring, Logger: logger,
				Docker: fixedSource{rt: verifyRuntime("16.0\n", "3\n")}, HostOps: fixedHost{ops: tc.ops},
			}
			_, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestServercovBackupS3UploadOutcomes(t *testing.T) {
	newHandler := func(t *testing.T, db *servercovDB, q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger, ops *hostfake.Ops) *BackupRun {
		t.Helper()
		return &BackupRun{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: verifyRuntime("16.0\n", "abc\n")}, HostOps: fixedHost{ops: ops},
		}
	}
	execute := func(t *testing.T, h *BackupRun, q *store.Queries) map[string]any {
		t.Helper()
		job := store.Job{ID: 32, JobType: TypeBackupExecute, Payload: []byte(`{"plan_id":1}`)}
		result, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
		if err != nil {
			t.Fatal(err)
		}
		return result.(map[string]any)
	}
	s3Plan := func(db *servercovDB, extra map[int]func(any)) {
		overrides := map[int]func(any){12: servercovPtr(int64(1))}
		for k, v := range extra {
			overrides[k] = v
		}
		db.rowAfter["GetBackupPlanByID"] = servercovOverride(overrides)
	}

	t.Run("s3_only success drops the local dump", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		ts, _ := servercovNewS3(t)
		servercovS3Storage(t, db, keyring, ts.URL)
		s3Plan(db, map[int]func(any){8: servercovVal(true), 13: servercovVal(true)}) // dump_all + s3_only
		ops := servercovBackupOps()
		var uploaded, removed []string
		ops.FileToURLFn = func(_ context.Context, p agentwire.FileToURLParams) error {
			uploaded = append(uploaded, p.URL)
			if len(p.Headers) == 0 {
				t.Error("SSE header was not carried to the upload")
			}
			return nil
		}
		ops.RemoveFn = func(_ context.Context, p agentwire.FileRemoveParams) error {
			removed = append(removed, p.Path)
			return nil
		}
		payload := execute(t, newHandler(t, db, q, keyring, logger, ops), q)
		if payload["status"] != string(store.BackupExecutionStatusSucceeded) {
			t.Fatalf("payload = %#v", payload)
		}
		if len(uploaded) != 1 || len(removed) == 0 {
			t.Fatalf("uploads = %v, removals = %v", uploaded, removed)
		}
	})
	t.Run("upload transport failure degrades to partial", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		ts, _ := servercovNewS3(t)
		servercovS3Storage(t, db, keyring, ts.URL)
		s3Plan(db, nil)
		ops := servercovBackupOps()
		ops.FileToURLFn = func(context.Context, agentwire.FileToURLParams) error {
			return errors.New("AccessDenied")
		}
		payload := execute(t, newHandler(t, db, q, keyring, logger, ops), q)
		if payload["status"] != string(store.BackupExecutionStatusPartial) {
			t.Fatalf("payload = %#v", payload)
		}
	})
	t.Run("truncated object degrades to partial", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		ts, state := servercovNewS3(t)
		state.headLength = "64"
		servercovS3Storage(t, db, keyring, ts.URL)
		s3Plan(db, nil)
		payload := execute(t, newHandler(t, db, q, keyring, logger, servercovBackupOps()), q)
		if payload["status"] != string(store.BackupExecutionStatusPartial) {
			t.Fatalf("payload = %#v", payload)
		}
	})
	t.Run("absent object degrades to partial", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		ts, state := servercovNewS3(t)
		state.headStatus = http.StatusNotFound
		servercovS3Storage(t, db, keyring, ts.URL)
		s3Plan(db, nil)
		payload := execute(t, newHandler(t, db, q, keyring, logger, servercovBackupOps()), q)
		if payload["status"] != string(store.BackupExecutionStatusPartial) {
			t.Fatalf("payload = %#v", payload)
		}
	})
	t.Run("unconfirmable object degrades to partial", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		ts, state := servercovNewS3(t)
		state.headStatus = http.StatusInternalServerError
		servercovS3Storage(t, db, keyring, ts.URL)
		s3Plan(db, nil)
		payload := execute(t, newHandler(t, db, q, keyring, logger, servercovBackupOps()), q)
		if payload["status"] != string(store.BackupExecutionStatusPartial) {
			t.Fatalf("payload = %#v", payload)
		}
	})
	t.Run("unusable storage degrades to partial", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		s3Plan(db, nil) // GetS3StorageByID keeps the garbage default ciphertexts
		payload := execute(t, newHandler(t, db, q, keyring, logger, servercovBackupOps()), q)
		if payload["status"] != string(store.BackupExecutionStatusPartial) {
			t.Fatalf("payload = %#v", payload)
		}
	})
	t.Run("secret key decrypt failure degrades to partial", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		access := servercovEncrypt(t, keyring, "s3_storages", "access_key_enc", "AK")
		db.rowAfter["GetS3StorageByID"] = servercovOverride(map[int]func(any){
			8: servercovBytes(access), // secret stays the garbage default
		})
		s3Plan(db, nil)
		payload := execute(t, newHandler(t, db, q, keyring, logger, servercovBackupOps()), q)
		if payload["status"] != string(store.BackupExecutionStatusPartial) {
			t.Fatalf("payload = %#v", payload)
		}
	})
	t.Run("unsignable endpoint degrades to partial", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		servercovS3Storage(t, db, keyring, "://not-a-url")
		s3Plan(db, nil)
		payload := execute(t, newHandler(t, db, q, keyring, logger, servercovBackupOps()), q)
		if payload["status"] != string(store.BackupExecutionStatusPartial) {
			t.Fatalf("payload = %#v", payload)
		}
	})
	t.Run("storage row missing degrades to partial", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		s3Plan(db, nil)
		db.rowErr["GetS3StorageByID"] = errors.New("gone")
		payload := execute(t, newHandler(t, db, q, keyring, logger, servercovBackupOps()), q)
		if payload["status"] != string(store.BackupExecutionStatusPartial) {
			t.Fatalf("payload = %#v", payload)
		}
	})
}

func TestServercovBackupRetentionBranches(t *testing.T) {
	t.Run("local expired dumps are removed and stubborn ones logged", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		db.queryFor["ListExpiredLocalBackups"] = [][]func([]any){{
			servercovFill(map[int]func(any){5: servercovNilPtr[string]()}),
			servercovFill(map[int]func(any){5: servercovPtr("/var/lib/akerdock/backups/expired.sql.gz")}),
			servercovFill(map[int]func(any){5: servercovPtr("/var/lib/akerdock/backups/stubborn.sql.gz")}),
		}}
		ops := servercovBackupOps()
		ops.RemoveFn = func(_ context.Context, p agentwire.FileRemoveParams) error {
			if strings.Contains(p.Path, "stubborn") {
				return errors.New("permission denied")
			}
			return nil
		}
		h := &BackupRun{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: verifyRuntime("16.0\n", "3\n")}, HostOps: fixedHost{ops: ops},
		}
		job := store.Job{ID: 33, JobType: TypeBackupExecute, Payload: []byte(`{"plan_id":1}`)}
		if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("retention disabled skips the purge", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		db.rowAfter["GetBackupPlanByID"] = servercovOverride(map[int]func(any){
			15: servercovVal(int32(0)), 16: servercovVal(int32(0)),
		})
		db.queryErr["ListExpiredLocalBackups"] = errors.New("must not be queried")
		h := &BackupRun{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: verifyRuntime("16.0\n", "3\n")}, HostOps: fixedHost{ops: servercovBackupOps()},
		}
		job := store.Job{ID: 34, JobType: TypeBackupExecute, Payload: []byte(`{"plan_id":1}`)}
		if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("local retention query failure is logged not fatal", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		db.queryErr["ListExpiredLocalBackups"] = errors.New("query refused")
		h := &BackupRun{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: verifyRuntime("16.0\n", "3\n")}, HostOps: fixedHost{ops: servercovBackupOps()},
		}
		job := store.Job{ID: 35, JobType: TypeBackupExecute, Payload: []byte(`{"plan_id":1}`)}
		if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("s3 retention deletes expired objects", func(t *testing.T) {
		for name, prep := range map[string]func(db *servercovDB, state *servercovS3State){
			"delete succeeds": func(*servercovDB, *servercovS3State) {},
			"delete refused": func(_ *servercovDB, state *servercovS3State) {
				state.delStatus = http.StatusInternalServerError
			},
			"query refused": func(db *servercovDB, _ *servercovS3State) {
				db.queryErr["ListExpiredS3Backups"] = errors.New("query refused")
			},
			"no expired objects": func(db *servercovDB, _ *servercovS3State) {
				db.queryFor["ListExpiredS3Backups"] = [][]func([]any){{}}
			},
		} {
			t.Run(name, func(t *testing.T) {
				q, keyring, _, logger, db := servercovDeps(t)
				ts, state := servercovNewS3(t)
				servercovS3Storage(t, db, keyring, ts.URL)
				db.rowAfter["GetBackupPlanByID"] = servercovOverride(map[int]func(any){12: servercovPtr(int64(1))})
				db.queryFor["ListExpiredS3Backups"] = [][]func([]any){{
					servercovFill(map[int]func(any){11: servercovNilPtr[string]()}),
					servercovFill(map[int]func(any){11: servercovPtr("unit/expired.sql.gz")}),
				}}
				prep(db, state)
				ops := servercovBackupOps()
				ops.FileToURLFn = func(context.Context, agentwire.FileToURLParams) error { return nil }
				h := &BackupRun{
					Store: q, Keyring: keyring, Logger: logger,
					Docker: fixedSource{rt: verifyRuntime("16.0\n", "3\n")}, HostOps: fixedHost{ops: ops},
				}
				job := store.Job{ID: 36, JobType: TypeBackupExecute, Payload: []byte(`{"plan_id":1}`)}
				if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("s3 retention with unusable storage is logged not fatal", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		db.rowAfter["GetBackupPlanByID"] = servercovOverride(map[int]func(any){12: servercovPtr(int64(1))})
		db.queryFor["ListExpiredS3Backups"] = [][]func([]any){{
			servercovFill(map[int]func(any){11: servercovPtr("unit/expired.sql.gz")}),
		}}
		ops := servercovBackupOps()
		ops.FileToURLFn = func(context.Context, agentwire.FileToURLParams) error { return errors.New("upload refused") }
		h := &BackupRun{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: verifyRuntime("16.0\n", "3\n")}, HostOps: fixedHost{ops: ops},
		}
		job := store.Job{ID: 37, JobType: TypeBackupExecute, Payload: []byte(`{"plan_id":1}`)}
		if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err != nil {
			t.Fatal(err)
		}
	})
}

func servercovRestoreJob() store.Job {
	return store.Job{ID: 38, JobType: TypeBackupRestore, Payload: []byte(`{"plan_id":1,"execution_id":1}`)}
}

func TestServercovRestoreBranches(t *testing.T) {
	localGone := servercovVal(pgtype.Timestamptz{Time: time.Now(), Valid: true})

	newHandler := func(q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger, ops *hostfake.Ops) *BackupRun {
		return &BackupRun{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: verifyRuntime("")}, HostOps: fixedHost{ops: ops},
		}
	}

	t.Run("execution vanished", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		db.rowErr["GetBackupExecutionByID"] = errors.New("gone")
		_, err := newHandler(q, keyring, logger, servercovBackupOps()).
			Execute(context.Background(), servercovRestoreJob(), queue.NewStepRecorder(q, servercovRestoreJob()))
		if err == nil || !strings.Contains(err.Error(), "backup execution not found") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("no dump recorded", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		db.rowAfter["GetBackupExecutionByID"] = servercovOverride(map[int]func(any){5: servercovNilPtr[string]()})
		_, err := newHandler(q, keyring, logger, servercovBackupOps()).
			Execute(context.Background(), servercovRestoreJob(), queue.NewStepRecorder(q, servercovRestoreJob()))
		if err == nil || !strings.Contains(err.Error(), "produced no dump") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("dump gone everywhere", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		db.rowAfter["GetBackupExecutionByID"] = servercovOverride(map[int]func(any){11: localGone})
		_, err := newHandler(q, keyring, logger, servercovBackupOps()).
			Execute(context.Background(), servercovRestoreJob(), queue.NewStepRecorder(q, servercovRestoreJob()))
		if err == nil || !strings.Contains(err.Error(), "gone") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("fetch back from the bucket then restore", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		ts, _ := servercovNewS3(t)
		servercovS3Storage(t, db, keyring, ts.URL)
		db.rowAfter["GetBackupPlanByID"] = servercovOverride(map[int]func(any){12: servercovPtr(int64(1))})
		db.rowAfter["GetBackupExecutionByID"] = servercovOverride(map[int]func(any){
			11: localGone, 16: servercovPtr("unit/back.sql.gz"),
		})
		ops := servercovBackupOps()
		fetched := false
		ops.URLToFileFn = func(_ context.Context, p agentwire.URLToFileParams) error {
			fetched = true
			if p.URL == "" || p.Path == "" {
				t.Error("fetch without URL or path")
			}
			return nil
		}
		ops.FileToExecFn = func(context.Context, agentwire.FileToExecParams) (agentwire.FileToExecResult, error) {
			return agentwire.FileToExecResult{ExitCode: 0}, nil
		}
		result, err := newHandler(q, keyring, logger, ops).
			Execute(context.Background(), servercovRestoreJob(), queue.NewStepRecorder(q, servercovRestoreJob()))
		if err != nil || !fetched {
			t.Fatalf("restore = %#v, %v (fetched=%v)", result, err, fetched)
		}
	})
	t.Run("fetch download failure", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		ts, _ := servercovNewS3(t)
		servercovS3Storage(t, db, keyring, ts.URL)
		db.rowAfter["GetBackupPlanByID"] = servercovOverride(map[int]func(any){12: servercovPtr(int64(1))})
		db.rowAfter["GetBackupExecutionByID"] = servercovOverride(map[int]func(any){
			11: localGone, 16: servercovPtr("unit/back.sql.gz"),
		})
		ops := servercovBackupOps()
		ops.URLToFileFn = func(context.Context, agentwire.URLToFileParams) error { return errors.New("timeout") }
		_, err := newHandler(q, keyring, logger, ops).
			Execute(context.Background(), servercovRestoreJob(), queue.NewStepRecorder(q, servercovRestoreJob()))
		if err == nil || !strings.Contains(err.Error(), "download failed") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("fetch with an unsignable endpoint", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		servercovS3Storage(t, db, keyring, "://not-a-url")
		db.rowAfter["GetBackupPlanByID"] = servercovOverride(map[int]func(any){12: servercovPtr(int64(1))})
		db.rowAfter["GetBackupExecutionByID"] = servercovOverride(map[int]func(any){
			11: localGone, 16: servercovPtr("unit/back.sql.gz"),
		})
		_, err := newHandler(q, keyring, logger, servercovBackupOps()).
			Execute(context.Background(), servercovRestoreJob(), queue.NewStepRecorder(q, servercovRestoreJob()))
		if err == nil {
			t.Fatal("unsignable endpoint was accepted")
		}
	})
	t.Run("fetch without a bucket copy", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		db.rowAfter["GetBackupExecutionByID"] = servercovOverride(map[int]func(any){
			11: localGone, 16: servercovPtr("unit/back.sql.gz"),
		})
		// plan.S3StorageID stays nil: fetchFromS3's own guard refuses.
		_, err := newHandler(q, keyring, logger, servercovBackupOps()).
			Execute(context.Background(), servercovRestoreJob(), queue.NewStepRecorder(q, servercovRestoreJob()))
		if err == nil || !strings.Contains(err.Error(), "no copy in a bucket") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("checksum mismatch aborts", func(t *testing.T) {
		q, keyring, _, logger, _ := servercovDeps(t)
		ops := servercovBackupOps()
		ops.HashFileFn = func(context.Context, string) (agentwire.FileHashResult, error) {
			return agentwire.FileHashResult{SHA256: "feedfacefeedface"}, nil
		}
		_, err := newHandler(q, keyring, logger, ops).
			Execute(context.Background(), servercovRestoreJob(), queue.NewStepRecorder(q, servercovRestoreJob()))
		if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("hash failure aborts", func(t *testing.T) {
		q, keyring, _, logger, _ := servercovDeps(t)
		ops := servercovBackupOps()
		ops.HashFileFn = func(context.Context, string) (agentwire.FileHashResult, error) {
			return agentwire.FileHashResult{}, errors.New("hash refused")
		}
		_, err := newHandler(q, keyring, logger, ops).
			Execute(context.Background(), servercovRestoreJob(), queue.NewStepRecorder(q, servercovRestoreJob()))
		if err == nil || !strings.Contains(err.Error(), "hash refused") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("without a checksum the restore still runs", func(t *testing.T) {
		q, keyring, _, logger, db := servercovDeps(t)
		db.rowAfter["GetBackupExecutionByID"] = servercovOverride(map[int]func(any){7: servercovNilPtr[string]()})
		ops := servercovBackupOps()
		ops.HashFileFn = func(context.Context, string) (agentwire.FileHashResult, error) {
			return agentwire.FileHashResult{}, errors.New("must not hash")
		}
		if _, err := newHandler(q, keyring, logger, ops).
			Execute(context.Background(), servercovRestoreJob(), queue.NewStepRecorder(q, servercovRestoreJob())); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("psql transport failure", func(t *testing.T) {
		q, keyring, _, logger, _ := servercovDeps(t)
		ops := servercovBackupOps()
		ops.FileToExecFn = func(context.Context, agentwire.FileToExecParams) (agentwire.FileToExecResult, error) {
			return agentwire.FileToExecResult{}, errors.New("pipe broke")
		}
		_, err := newHandler(q, keyring, logger, ops).
			Execute(context.Background(), servercovRestoreJob(), queue.NewStepRecorder(q, servercovRestoreJob()))
		if err == nil || !strings.Contains(err.Error(), "pipe broke") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("psql non-zero exit", func(t *testing.T) {
		q, keyring, _, logger, _ := servercovDeps(t)
		ops := servercovBackupOps()
		ops.FileToExecFn = func(context.Context, agentwire.FileToExecParams) (agentwire.FileToExecResult, error) {
			return agentwire.FileToExecResult{ExitCode: 3, Output: "syntax error"}, nil
		}
		_, err := newHandler(q, keyring, logger, ops).
			Execute(context.Background(), servercovRestoreJob(), queue.NewStepRecorder(q, servercovRestoreJob()))
		if err == nil || !strings.Contains(err.Error(), "restore failed with code 3") {
			t.Fatalf("error = %v", err)
		}
	})
}

func servercovDrillJob() store.Job {
	return store.Job{ID: 39, JobType: TypeBackupDrill, Payload: []byte(`{"plan_id":1}`)}
}

func servercovRunDrill(t *testing.T, q *store.Queries, h *BackupRun) (map[string]any, error) {
	t.Helper()
	job := servercovDrillJob()
	result, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err != nil {
		return nil, err
	}
	return result.(map[string]any), nil
}

func TestServercovDrillVerdicts(t *testing.T) {
	localGone := servercovVal(pgtype.Timestamptz{Time: time.Now(), Valid: true})

	type drillCase struct {
		prep       func(t *testing.T, db *servercovDB, h *BackupRun)
		wantStatus string
		wantReason string
		wantErr    string
	}
	tests := map[string]drillCase{
		"no successful backup yet": {
			prep: func(_ *testing.T, db *servercovDB, _ *BackupRun) {
				db.rowErr["GetLatestSuccessfulBackupExecution"] = errors.New("no rows")
			},
			wantErr: "nothing to prove",
		},
		"drill row cannot be created": {
			prep: func(_ *testing.T, db *servercovDB, _ *BackupRun) {
				db.rowErr["CreateRestoreDrill"] = errors.New("insert refused")
			},
			wantErr: "insert refused",
		},
		"no dump file": {
			prep: func(_ *testing.T, db *servercovDB, _ *BackupRun) {
				db.rowAfter["GetLatestSuccessfulBackupExecution"] = servercovOverride(map[int]func(any){
					5: servercovNilPtr[string](),
				})
			},
			wantStatus: "failed", wantReason: "produced no dump",
		},
		"dump gone everywhere": {
			prep: func(_ *testing.T, db *servercovDB, _ *BackupRun) {
				db.rowAfter["GetLatestSuccessfulBackupExecution"] = servercovOverride(map[int]func(any){
					11: localGone,
				})
				db.rowErr["GetTeamByID"] = errors.New("team lookup refused")
			},
			wantStatus: "failed", wantReason: "gone",
		},
		"fetch failure": {
			prep: func(t *testing.T, db *servercovDB, h *BackupRun) {
				db.rowAfter["GetLatestSuccessfulBackupExecution"] = servercovOverride(map[int]func(any){
					11: localGone, 16: servercovPtr("unit/back.sql.gz"),
				})
				db.rowAfter["GetBackupPlanByID"] = servercovOverride(map[int]func(any){12: servercovPtr(int64(1))})
				ts, _ := servercovNewS3(t)
				servercovS3Storage(t, db, servercovFixtureKeyring(t), ts.URL)
				ops := servercovBackupOps()
				ops.URLToFileFn = func(context.Context, agentwire.URLToFileParams) error { return errors.New("timeout") }
				h.HostOps = fixedHost{ops: ops}
			},
			wantStatus: "failed", wantReason: "download failed",
		},
		"checksum mismatch": {
			prep: func(_ *testing.T, _ *servercovDB, h *BackupRun) {
				ops := servercovBackupOps()
				ops.HashFileFn = func(context.Context, string) (agentwire.FileHashResult, error) {
					return agentwire.FileHashResult{SHA256: "feedfacefeedface"}, nil
				}
				h.HostOps = fixedHost{ops: ops}
			},
			wantStatus: "failed", wantReason: "checksum mismatch",
		},
		"hash failure": {
			prep: func(_ *testing.T, _ *servercovDB, h *BackupRun) {
				ops := servercovBackupOps()
				ops.HashFileFn = func(context.Context, string) (agentwire.FileHashResult, error) {
					return agentwire.FileHashResult{}, errors.New("hash refused")
				}
				h.HostOps = fixedHost{ops: ops}
			},
			wantStatus: "failed", wantReason: "hash refused",
		},
		"scratch cannot be created": {
			prep: func(_ *testing.T, _ *servercovDB, h *BackupRun) {
				rt := servercovExecRuntime([]string{""}, []int{0})
				rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispec.Platform, string) (containertypes.CreateResponse, error) {
					return containertypes.CreateResponse{}, errors.New("daemon refused")
				}
				h.Docker = fixedSource{rt: rt}
			},
			wantStatus: "failed", wantReason: "cannot create the disposable database",
		},
		"restore transport failure": {
			prep: func(_ *testing.T, _ *servercovDB, h *BackupRun) {
				ops := servercovBackupOps()
				ops.FileToExecFn = func(context.Context, agentwire.FileToExecParams) (agentwire.FileToExecResult, error) {
					return agentwire.FileToExecResult{}, errors.New("pipe broke")
				}
				h.HostOps = fixedHost{ops: ops}
			},
			wantStatus: "failed", wantReason: "pipe broke",
		},
		"restore exits non-zero": {
			prep: func(_ *testing.T, _ *servercovDB, h *BackupRun) {
				ops := servercovBackupOps()
				ops.FileToExecFn = func(context.Context, agentwire.FileToExecParams) (agentwire.FileToExecResult, error) {
					return agentwire.FileToExecResult{ExitCode: 1, Output: "owner does not exist"}, nil
				}
				h.HostOps = fixedHost{ops: ops}
			},
			wantStatus: "failed", wantReason: "did not restore",
		},
		"table count cannot be read": {
			prep: func(_ *testing.T, _ *servercovDB, h *BackupRun) {
				h.Docker = fixedSource{rt: servercovExecRuntime([]string{"", "boom"}, []int{0, 1})}
			},
			wantStatus: "failed", wantReason: "counting tables failed",
		},
		"table count mismatch": {
			prep: func(_ *testing.T, _ *servercovDB, h *BackupRun) {
				h.Docker = fixedSource{rt: servercovExecRuntime([]string{"", "2\n"}, []int{0, 0})}
			},
			wantStatus: "failed", wantReason: "2 tables",
		},
		"empty restore without a reference count": {
			prep: func(_ *testing.T, db *servercovDB, h *BackupRun) {
				db.rowAfter["GetLatestSuccessfulBackupExecution"] = servercovOverride(map[int]func(any){
					18: servercovNilPtr[int32](),
				})
				h.Docker = fixedSource{rt: servercovExecRuntime([]string{"", "0\n"}, []int{0, 0})}
			},
			wantStatus: "failed", wantReason: "cannot be vouched for",
		},
		"populated restore without a reference count succeeds": {
			prep: func(_ *testing.T, db *servercovDB, h *BackupRun) {
				db.rowAfter["GetLatestSuccessfulBackupExecution"] = servercovOverride(map[int]func(any){
					18: servercovNilPtr[int32](),
				})
			},
			wantStatus: "succeeded",
		},
		"success": {
			prep:       func(*testing.T, *servercovDB, *BackupRun) {},
			wantStatus: "succeeded",
		},
		"fetch back from the bucket then drill": {
			prep: func(t *testing.T, db *servercovDB, h *BackupRun) {
				db.rowAfter["GetLatestSuccessfulBackupExecution"] = servercovOverride(map[int]func(any){
					11: localGone, 16: servercovPtr("unit/back.sql.gz"),
				})
				db.rowAfter["GetBackupPlanByID"] = servercovOverride(map[int]func(any){12: servercovPtr(int64(1))})
				ts, _ := servercovNewS3(t)
				servercovS3Storage(t, db, servercovFixtureKeyring(t), ts.URL)
				ops := servercovBackupOps()
				ops.URLToFileFn = func(context.Context, agentwire.URLToFileParams) error { return nil }
				h.HostOps = fixedHost{ops: ops}
			},
			wantStatus: "succeeded",
		},
		"scratch never becomes ready": {
			prep: func(t *testing.T, _ *servercovDB, h *BackupRun) {
				oldBoot := drillBoot
				drillBoot = 0
				t.Cleanup(func() { drillBoot = oldBoot })
			},
			wantStatus: "failed", wantReason: "never became ready",
		},
		"component login cannot be read": {
			prep: func(_ *testing.T, db *servercovDB, h *BackupRun) {
				engine := store.DbEnginePostgresql
				db.rowAfter["GetBackupPlanByID"] = servercovOverride(map[int]func(any){
					2: servercovNilPtr[int64](), 3: servercovPtr(int64(1)),
				})
				db.rowAfter["GetComponentBackupTarget"] = servercovOverride(map[int]func(any){
					5: servercovVal(true), 6: servercovPtr(engine),
				})
				h.Docker = fixedSource{rt: servercovExecRuntime([]string{"denied"}, []int{1})}
			},
			wantStatus: "failed", wantReason: "cannot read the component",
		},
		"drill result cannot be recorded": {
			prep: func(_ *testing.T, db *servercovDB, _ *BackupRun) {
				db.execErr["FinishRestoreDrill"] = errors.New("finish refused")
			},
			wantErr: "finish refused",
		},
		"plan verdict cannot be recorded": {
			prep: func(_ *testing.T, db *servercovDB, _ *BackupRun) {
				db.execErr["SetPlanDrillResult"] = errors.New("verdict refused")
			},
			wantErr: "verdict refused",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			q, keyring, recorder, logger, db := servercovDeps(t)
			h := &BackupRun{
				Store: q, Keyring: keyring, Audit: recorder, Logger: logger,
				Docker:  fixedSource{rt: servercovExecRuntime([]string{"", "3\n"}, []int{0, 0})},
				HostOps: fixedHost{ops: servercovBackupOps()},
			}
			tc.prep(t, db, h)
			payload, err := servercovRunDrill(t, q, h)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if payload["status"] != tc.wantStatus {
				t.Fatalf("payload = %#v, want status %q", payload, tc.wantStatus)
			}
			if tc.wantReason != "" && !strings.Contains(payload["reason"].(string), tc.wantReason) {
				t.Fatalf("reason = %q, want %q", payload["reason"], tc.wantReason)
			}
		})
	}
}

func TestServercovDrillWithoutAuditStaysSilent(t *testing.T) {
	q, keyring, _, logger, db := servercovDeps(t)
	db.rowAfter["GetLatestSuccessfulBackupExecution"] = servercovOverride(map[int]func(any){
		5: servercovNilPtr[string](),
	})
	h := &BackupRun{
		Store: q, Keyring: keyring, Logger: logger, // no Audit
		Docker:  fixedSource{rt: servercovExecRuntime([]string{""}, []int{0})},
		HostOps: fixedHost{ops: servercovBackupOps()},
	}
	payload, err := servercovRunDrill(t, q, h)
	if err != nil || payload["status"] != "failed" {
		t.Fatalf("payload = %#v, %v", payload, err)
	}
}

func TestServercovDrillScratchImageFollowsTheSource(t *testing.T) {
	q, keyring, recorder, logger, db := servercovDeps(t)
	db.rowAfter["GetDatabaseByID"] = servercovOverride(map[int]func(any){
		23: servercovPtr("postgres:15.4"),
	})
	rt := servercovExecRuntime([]string{"", "3\n"}, []int{0, 0})
	var created []string
	base := rt.ContainerCreateFn
	rt.ContainerCreateFn = func(ctx context.Context, cfg *containertypes.Config, host *containertypes.HostConfig, netCfg *networktypes.NetworkingConfig, platform *ocispec.Platform, name string) (containertypes.CreateResponse, error) {
		created = append(created, cfg.Image)
		return base(ctx, cfg, host, netCfg, platform, name)
	}
	var removeErrs int
	rt.ContainerRemoveFn = func(context.Context, string, containertypes.RemoveOptions) error {
		removeErrs++
		return errors.New("still busy")
	}
	h := &BackupRun{
		Store: q, Keyring: keyring, Audit: recorder, Logger: logger,
		Docker: fixedSource{rt: rt}, HostOps: fixedHost{ops: servercovBackupOps()},
	}
	payload, err := servercovRunDrill(t, q, h)
	if err != nil || payload["status"] != "succeeded" {
		t.Fatalf("payload = %#v, %v", payload, err)
	}
	if len(created) != 1 || created[0] != "postgres:15.4" {
		t.Fatalf("scratch image = %v, want the source database's", created)
	}
	if removeErrs != 1 {
		t.Fatalf("cleanup attempts = %d", removeErrs)
	}
}

func TestServercovBootScratchBranches(t *testing.T) {
	h := &BackupRun{}
	login := dbLogin{User: "app", DB: "appdb"}

	t.Run("image present", func(t *testing.T) {
		rt := servercovExecRuntime([]string{""}, []int{0})
		if err := h.bootScratch(context.Background(), rt, "scratch", "postgres:16", "pw", login); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("pull then create", func(t *testing.T) {
		rt := servercovExecRuntime([]string{""}, []int{0})
		creates := 0
		rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispec.Platform, string) (containertypes.CreateResponse, error) {
			creates++
			if creates == 1 {
				return containertypes.CreateResponse{}, fmt.Errorf("no such image: %w", cerrdefs.ErrNotFound)
			}
			return containertypes.CreateResponse{ID: "scratch"}, nil
		}
		rt.ImagePullFn = func(context.Context, string, imagetypes.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("{}")), nil
		}
		if err := h.bootScratch(context.Background(), rt, "scratch", "postgres:16", "pw", login); err != nil {
			t.Fatal(err)
		}
		if creates != 2 {
			t.Fatalf("creates = %d", creates)
		}
	})
	t.Run("pull refused", func(t *testing.T) {
		rt := servercovExecRuntime([]string{""}, []int{0})
		rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispec.Platform, string) (containertypes.CreateResponse, error) {
			return containertypes.CreateResponse{}, fmt.Errorf("no such image: %w", cerrdefs.ErrNotFound)
		}
		rt.ImagePullFn = func(context.Context, string, imagetypes.PullOptions) (io.ReadCloser, error) {
			return nil, errors.New("registry down")
		}
		if err := h.bootScratch(context.Background(), rt, "scratch", "postgres:16", "pw", login); err == nil ||
			!strings.Contains(err.Error(), "cannot pull") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("create still refused after pull", func(t *testing.T) {
		rt := servercovExecRuntime([]string{""}, []int{0})
		rt.ContainerCreateFn = func(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispec.Platform, string) (containertypes.CreateResponse, error) {
			return containertypes.CreateResponse{}, fmt.Errorf("no such image: %w", cerrdefs.ErrNotFound)
		}
		rt.ImagePullFn = func(context.Context, string, imagetypes.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("{}")), nil
		}
		if err := h.bootScratch(context.Background(), rt, "scratch", "postgres:16", "pw", login); err == nil ||
			!strings.Contains(err.Error(), "cannot create") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("start refused", func(t *testing.T) {
		rt := servercovExecRuntime([]string{""}, []int{0})
		rt.ContainerStartFn = func(context.Context, string, containertypes.StartOptions) error {
			return errors.New("cgroup exploded")
		}
		if err := h.bootScratch(context.Background(), rt, "scratch", "postgres:16", "pw", login); err == nil ||
			!strings.Contains(err.Error(), "cannot start") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestServercovDrillLoginBranches(t *testing.T) {
	h := &BackupRun{}
	known := dbLogin{User: "app", DB: "appdb"}

	if got, err := h.drillLogin(context.Background(), nil, backupTarget{login: &known}); err != nil || got != known {
		t.Fatalf("known login = %#v, %v", got, err)
	}
	if got, err := h.drillLogin(context.Background(),
		servercovExecRuntime([]string{"app\nappdb\n"}, []int{0}), backupTarget{container: "c"}); err != nil ||
		got.User != "app" || got.DB != "appdb" {
		t.Fatalf("component login = %#v, %v", got, err)
	}
	if _, err := h.drillLogin(context.Background(),
		servercovExecRuntime([]string{"denied"}, []int{1}), backupTarget{container: "c"}); err == nil ||
		!strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("error = %v", err)
	}
	if _, err := h.drillLogin(context.Background(),
		servercovExecRuntime([]string{"\n\n"}, []int{0}), backupTarget{container: "c"}); err == nil ||
		!strings.Contains(err.Error(), "did not expose") {
		t.Fatalf("error = %v", err)
	}
	broken := servercovExecRuntime(nil, nil)
	broken.ContainerExecCreateFn = func(context.Context, string, containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
		return containertypes.ExecCreateResponse{}, errors.New("exec refused")
	}
	if _, err := h.drillLogin(context.Background(), broken, backupTarget{container: "c"}); err == nil {
		t.Fatal("exec failure was hidden")
	}
}

func TestServercovWaitReadyBranches(t *testing.T) {
	h := &BackupRun{}

	t.Run("ready after one poll", func(t *testing.T) {
		oldBoot, oldPoll := drillBoot, drillBootPoll
		drillBoot, drillBootPoll = servercovTimeout, time.Millisecond
		defer func() { drillBoot, drillBootPoll = oldBoot, oldPoll }()
		rt := servercovExecRuntime([]string{"", ""}, []int{1, 0})
		if err := h.waitReady(context.Background(), rt, "scratch", "app"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("boot budget exhausted", func(t *testing.T) {
		oldBoot := drillBoot
		drillBoot = 0
		defer func() { drillBoot = oldBoot }()
		rt := servercovExecRuntime([]string{""}, []int{1})
		if err := h.waitReady(context.Background(), rt, "scratch", "app"); err == nil ||
			!strings.Contains(err.Error(), "never became ready") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("canceled context stops the wait", func(t *testing.T) {
		oldBoot := drillBoot
		drillBoot = servercovTimeout
		defer func() { drillBoot = oldBoot }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rt := servercovExecRuntime([]string{""}, []int{1})
		if err := h.waitReady(ctx, rt, "scratch", "app"); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("exec failure surfaces", func(t *testing.T) {
		rt := servercovExecRuntime(nil, nil)
		rt.ContainerExecCreateFn = func(context.Context, string, containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
			return containertypes.ExecCreateResponse{}, errors.New("exec refused")
		}
		if err := h.waitReady(context.Background(), rt, "scratch", "app"); err == nil {
			t.Fatal("exec failure was hidden")
		}
	})
}

func TestServercovCountTablesBranches(t *testing.T) {
	h := &BackupRun{}
	login := dbLogin{User: "app", DB: "appdb"}

	if n, err := h.countTables(context.Background(),
		servercovExecRuntime([]string{" 42 \n"}, []int{0}), "c", &login); err != nil || n != 42 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if _, err := h.countTables(context.Background(),
		servercovExecRuntime([]string{"fatal: connection refused"}, []int{2}), "c", nil); err == nil ||
		!strings.Contains(err.Error(), "counting tables failed") {
		t.Fatalf("error = %v", err)
	}
	if _, err := h.countTables(context.Background(),
		servercovExecRuntime([]string{"not-a-number\n"}, []int{0}), "c", &login); err == nil {
		t.Fatal("garbage count was accepted")
	}
	broken := servercovExecRuntime(nil, nil)
	broken.ContainerExecCreateFn = func(context.Context, string, containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
		return containertypes.ExecCreateResponse{}, errors.New("exec refused")
	}
	if _, err := h.countTables(context.Background(), broken, "c", nil); err == nil {
		t.Fatal("exec failure was hidden")
	}
}

func TestServercovCredentialsOf(t *testing.T) {
	dbName := "orders"
	row := store.GetDatabaseByIDRow{DatabaseCredential: store.DatabaseCredential{Username: "app", DbName: &dbName}}
	if got := credentialsOf(row); got.User != "app" || got.DB != "orders" {
		t.Fatalf("credentials = %#v", got)
	}
	row.DatabaseCredential.DbName = nil
	if got := credentialsOf(row); got.DB != "app" {
		t.Fatalf("defaulted credentials = %#v", got)
	}
}

func TestServercovDrillPassword(t *testing.T) {
	first, err := drillPassword()
	if err != nil || first == "" {
		t.Fatalf("password = %q, %v", first, err)
	}
	second, err := drillPassword()
	if err != nil || second == first {
		t.Fatalf("passwords repeat: %q", second)
	}
}
