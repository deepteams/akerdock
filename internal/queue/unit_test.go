package queue

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/deepteams/akerdock/internal/store"
)

type fakeQueueStore struct {
	mu sync.Mutex

	enqueueArg store.EnqueueJobParams
	enqueueJob store.Job
	enqueueErr error
	idemJob    store.Job
	idemErr    error

	stepUpdates []store.UpdateJobStepsParams
	stepErr     error

	promoteCalls int
	promoteErr   error
	reaped       []store.ReapExpiredLeasesRow
	reapErr      error
	dequeue      func(context.Context, store.DequeueJobParams) (store.Job, error)

	markRows int64
	markErr  error
	hbErr    error

	succeedArgs []store.SucceedJobParams
	succeedRows int64
	succeedErr  error
	failArgs    []store.FailJobParams
	failErr     error
}

func (f *fakeQueueStore) EnqueueJob(_ context.Context, arg store.EnqueueJobParams) (store.Job, error) {
	f.enqueueArg = arg
	return f.enqueueJob, f.enqueueErr
}

func (f *fakeQueueStore) GetJobByIdempotencyKey(context.Context, *string) (store.Job, error) {
	return f.idemJob, f.idemErr
}

func (f *fakeQueueStore) UpdateJobSteps(_ context.Context, arg store.UpdateJobStepsParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	arg.Steps = append([]byte(nil), arg.Steps...)
	f.stepUpdates = append(f.stepUpdates, arg)
	return f.stepErr
}

func (f *fakeQueueStore) PromoteWaitingJobs(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promoteCalls++
	return 0, f.promoteErr
}

func (f *fakeQueueStore) ReapExpiredLeases(context.Context, int32) ([]store.ReapExpiredLeasesRow, error) {
	return f.reaped, f.reapErr
}

func (f *fakeQueueStore) DequeueJob(ctx context.Context, arg store.DequeueJobParams) (store.Job, error) {
	if f.dequeue != nil {
		return f.dequeue(ctx, arg)
	}
	return store.Job{}, pgx.ErrNoRows
}

func (f *fakeQueueStore) MarkJobRunning(context.Context, store.MarkJobRunningParams) (int64, error) {
	return f.markRows, f.markErr
}

func (f *fakeQueueStore) HeartbeatJob(context.Context, store.HeartbeatJobParams) (int64, error) {
	return 1, f.hbErr
}

func (f *fakeQueueStore) SucceedJob(_ context.Context, arg store.SucceedJobParams) (int64, error) {
	f.succeedArgs = append(f.succeedArgs, arg)
	return f.succeedRows, f.succeedErr
}

func (f *fakeQueueStore) FailJob(_ context.Context, arg store.FailJobParams) (int64, error) {
	f.failArgs = append(f.failArgs, arg)
	return 1, f.failErr
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEnqueueDefaultsSchedulesAndIdempotency(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		database := &fakeQueueStore{enqueueJob: store.Job{ID: 1}}
		got, err := Enqueue(context.Background(), database, EnqueueOptions{
			Type: "test", Payload: nil,
		})
		if err != nil || got.ID != 1 {
			t.Fatalf("Enqueue = %#v, %v", got, err)
		}
		if database.enqueueArg.Queue != "default" ||
			database.enqueueArg.Status != store.JobStatusQueued ||
			database.enqueueArg.MaxAttempts != 5 ||
			string(database.enqueueArg.Payload) != "{}" ||
			!database.enqueueArg.Uuid.Valid {
			t.Fatalf("enqueue args = %#v", database.enqueueArg)
		}
	})
	t.Run("scheduled", func(t *testing.T) {
		runAt := time.Now().Add(time.Hour)
		database := &fakeQueueStore{}
		_, err := Enqueue(context.Background(), database, EnqueueOptions{
			Queue: "backup", Type: "backup", Payload: map[string]int{"id": 7},
			RunAt: runAt, MaxAttempts: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		if database.enqueueArg.Status != store.JobStatusScheduled ||
			database.enqueueArg.MaxAttempts != 2 ||
			database.enqueueArg.RunAt.Time != runAt ||
			string(database.enqueueArg.Payload) != `{"id":7}` {
			t.Fatalf("scheduled args = %#v", database.enqueueArg)
		}
	})
	t.Run("idempotent replay", func(t *testing.T) {
		key := "same"
		database := &fakeQueueStore{
			enqueueErr: pgx.ErrNoRows, idemJob: store.Job{ID: 9},
		}
		got, err := Enqueue(context.Background(), database, EnqueueOptions{
			Queue: "default", Type: "test", IdempotencyKey: &key,
		})
		if err != nil || got.ID != 9 {
			t.Fatalf("Enqueue replay = %#v, %v", got, err)
		}
		database.idemErr = errors.New("read failed")
		if _, err := Enqueue(context.Background(), database, EnqueueOptions{
			Queue: "default", Type: "test", IdempotencyKey: &key,
		}); err == nil {
			t.Fatal("idempotency lookup error was hidden")
		}
	})
}

func TestEnqueueRejectsBadInputsAndStoreFailure(t *testing.T) {
	if _, err := Enqueue(context.Background(), &fakeQueueStore{}, EnqueueOptions{
		Queue: "unknown", Type: "test",
	}); err == nil {
		t.Fatal("unknown queue was accepted")
	}
	if _, err := Enqueue(context.Background(), &fakeQueueStore{}, EnqueueOptions{
		Queue: "default", Type: "test", Payload: make(chan int),
	}); err == nil || !strings.Contains(err.Error(), "payload") {
		t.Fatalf("unserializable payload error = %v", err)
	}
	database := &fakeQueueStore{enqueueErr: errors.New("insert failed")}
	if _, err := Enqueue(context.Background(), database, EnqueueOptions{
		Queue: "default", Type: "test",
	}); err == nil || !strings.Contains(err.Error(), "enqueue") {
		t.Fatalf("store error = %v", err)
	}
}

func TestStepRecorderTimeline(t *testing.T) {
	database := &fakeQueueStore{}
	recorder := NewStepRecorder(database, store.Job{ID: 42})
	recorder.Succeed(context.Background(), "nothing")
	recorder.Start(context.Background(), "prepare")
	recorder.Succeed(context.Background(), "ready")
	recorder.Start(context.Background(), "deploy")
	recorder.Fail(context.Background(), "")
	recorder.Skip(context.Background(), "cleanup", "not needed")
	recorder.Flush(context.Background())

	var steps []Step
	last := database.stepUpdates[len(database.stepUpdates)-1]
	if last.ID != 42 || json.Unmarshal(last.Steps, &steps) != nil {
		t.Fatalf("persisted steps = %s", last.Steps)
	}
	if len(steps) != 3 ||
		steps[0].Status != "succeeded" || steps[0].Message == nil ||
		steps[1].Status != "failed" || steps[1].Message != nil ||
		steps[2].Status != "skipped" || steps[2].StartedAt == nil || steps[2].FinishedAt == nil {
		t.Fatalf("steps = %#v", steps)
	}

	database.stepErr = errors.New("best effort")
	recorder.Flush(context.Background())
}

func workerFor(database *fakeQueueStore) *Worker {
	worker := NewWorker(database, 0, discardLogger())
	worker.id = "worker-1"
	return worker
}

func TestNewWorkerRegisterTracerAndWait(t *testing.T) {
	worker := workerFor(&fakeQueueStore{})
	if worker.Concurrency != 1 || len(worker.Queues) != len(KnownQueues) || worker.id == "" {
		t.Fatalf("worker = %#v", worker)
	}
	handler := func(context.Context, store.Job, *StepRecorder) (any, error) { return nil, nil }
	worker.Register("test", handler)
	if worker.handlers["test"] == nil || worker.tracer() == nil {
		t.Fatal("registration or no-op tracer failed")
	}
	worker.Wait(time.Millisecond)
	worker.wg.Add(1)
	worker.Wait(time.Millisecond)
	worker.wg.Done()
	worker.Wait(time.Second)
}

func TestWorkerProcessSuccess(t *testing.T) {
	database := &fakeQueueStore{markRows: 1, succeedRows: 1}
	worker := workerFor(database)
	worker.Register("test", func(_ context.Context, _ store.Job, recorder *StepRecorder) (any, error) {
		recorder.Start(context.Background(), "work")
		recorder.Succeed(context.Background(), "done")
		return map[string]int{"answer": 42}, nil
	})
	job := store.Job{ID: 7, JobType: "test", Queue: "default", Attempt: 1, MaxAttempts: 3}
	worker.process(context.Background(), job)
	if len(database.succeedArgs) != 1 || database.succeedArgs[0].ID != 7 ||
		string(database.succeedArgs[0].Result) != `{"answer":42}` ||
		len(database.stepUpdates) == 0 {
		t.Fatalf("success args=%#v steps=%d", database.succeedArgs, len(database.stepUpdates))
	}
}

func TestWorkerProcessFailureBranches(t *testing.T) {
	errBoom := errors.New("boom")
	t.Run("lost lease", func(t *testing.T) {
		for _, database := range []*fakeQueueStore{
			{markRows: 0}, {markRows: 1, markErr: errBoom},
		} {
			workerFor(database).process(context.Background(), store.Job{ID: 1})
			if len(database.failArgs) != 0 {
				t.Fatal("a job with a lost lease was mutated")
			}
		}
	})
	t.Run("missing handler retries", func(t *testing.T) {
		database := &fakeQueueStore{markRows: 1}
		workerFor(database).process(context.Background(), store.Job{
			ID: 1, JobType: "missing", Attempt: 1, MaxAttempts: 2,
		})
		if len(database.failArgs) != 1 || database.failArgs[0].ToDeadLetter ||
			database.failArgs[0].LastError == nil {
			t.Fatalf("fail args = %#v", database.failArgs)
		}
	})
	t.Run("handler dead letters", func(t *testing.T) {
		database := &fakeQueueStore{markRows: 1}
		worker := workerFor(database)
		worker.Register("test", func(context.Context, store.Job, *StepRecorder) (any, error) {
			return nil, errBoom
		})
		worker.process(context.Background(), store.Job{
			ID: 1, JobType: "test", Attempt: 2, MaxAttempts: 2,
		})
		if len(database.failArgs) != 1 || !database.failArgs[0].ToDeadLetter {
			t.Fatalf("fail args = %#v", database.failArgs)
		}
	})
	t.Run("result marshal and success update failures", func(t *testing.T) {
		for _, tc := range []struct {
			result any
			rows   int64
			err    error
		}{
			{nil, 1, nil},
			{make(chan int), 1, nil},
			{"ok", 0, nil},
			{"ok", 1, errBoom},
		} {
			database := &fakeQueueStore{markRows: 1, succeedRows: tc.rows, succeedErr: tc.err}
			worker := workerFor(database)
			worker.Register("test", func(context.Context, store.Job, *StepRecorder) (any, error) {
				return tc.result, nil
			})
			worker.process(context.Background(), store.Job{ID: 1, JobType: "test"})
			if len(database.succeedArgs) != 1 {
				t.Fatal("success update was not attempted")
			}
			if (tc.result == nil || reflect.TypeOf(tc.result).Kind() == reflect.Chan) &&
				database.succeedArgs[0].Result != nil {
				t.Fatalf("unsafe result was persisted: %s", database.succeedArgs[0].Result)
			}
		}
	})
}

func TestWorkerFailStoreErrorAndDeadLetter(t *testing.T) {
	errBoom := errors.New("write failed")
	for _, tc := range []struct {
		attempt, max int32
		storeErr     error
		dead         bool
	}{
		{1, 3, errBoom, false},
		{3, 3, nil, true},
	} {
		database := &fakeQueueStore{failErr: tc.storeErr}
		worker := workerFor(database)
		worker.fail(context.Background(), discardLogger(), store.Job{
			ID: 1, Attempt: tc.attempt, MaxAttempts: tc.max,
		}, errors.New("job failed"))
		if len(database.failArgs) != 1 || database.failArgs[0].ToDeadLetter != tc.dead {
			t.Fatalf("fail args = %#v", database.failArgs)
		}
	}
	if !isNoRows(pgx.ErrNoRows) || isNoRows(errors.New("other")) {
		t.Fatal("isNoRows classification failed")
	}
}

func TestWorkerRunNoRowsAndMaintenance(t *testing.T) {
	oldMaintenance, oldIdle, oldError := maintenanceInterval, dequeueIdleDelay, dequeueErrorDelay
	maintenanceInterval, dequeueIdleDelay, dequeueErrorDelay = time.Millisecond, time.Millisecond, time.Millisecond
	defer func() {
		maintenanceInterval, dequeueIdleDelay, dequeueErrorDelay = oldMaintenance, oldIdle, oldError
	}()

	ctx, cancel := context.WithCancel(context.Background())
	database := &fakeQueueStore{
		promoteErr: errors.New("promote"),
		reapErr:    errors.New("reap"),
	}
	var calls int
	database.dequeue = func(context.Context, store.DequeueJobParams) (store.Job, error) {
		calls++
		if calls > 2 {
			cancel()
		}
		return store.Job{}, pgx.ErrNoRows
	}
	worker := NewWorker(database, 1, discardLogger())
	worker.Run(ctx)
	worker.Wait(time.Second)
	if calls < 3 {
		t.Fatalf("dequeue calls = %d", calls)
	}
}

func TestWorkerRunDequeueErrorAndJob(t *testing.T) {
	oldIdle, oldError := dequeueIdleDelay, dequeueErrorDelay
	dequeueIdleDelay, dequeueErrorDelay = time.Millisecond, time.Millisecond
	defer func() { dequeueIdleDelay, dequeueErrorDelay = oldIdle, oldError }()

	ctx, cancel := context.WithCancel(context.Background())
	database := &fakeQueueStore{markRows: 1, succeedRows: 1}
	var calls int
	database.dequeue = func(context.Context, store.DequeueJobParams) (store.Job, error) {
		calls++
		switch calls {
		case 1:
			return store.Job{}, errors.New("temporary")
		case 2:
			return store.Job{ID: 1, JobType: "test"}, nil
		default:
			cancel()
			return store.Job{}, pgx.ErrNoRows
		}
	}
	worker := NewWorker(database, 1, discardLogger())
	worker.Register("test", func(context.Context, store.Job, *StepRecorder) (any, error) {
		return "ok", nil
	})
	worker.Run(ctx)
	worker.Wait(time.Second)
	if len(database.succeedArgs) != 1 {
		t.Fatalf("succeeded = %#v", database.succeedArgs)
	}
}
