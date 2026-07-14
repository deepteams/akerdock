package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/telemetry"
)

// HandlerFunc executes one job attempt. It must be idempotent: after a
// crash, another worker re-runs the job once the lease expires, and the
// handler inspects the remote effect before redoing work (§21.3, §22.1).
// The returned result is stored on success (never secrets, INV-003).
type HandlerFunc func(ctx context.Context, job store.Job, rec *StepRecorder) (result any, err error)

// Worker consumes the queue with a bounded pool of goroutines.
type Worker struct {
	// Telemetry is optional: a nil Metrics records nothing, so a worker built
	// without telemetry behaves exactly as before (ADR-008).
	Metrics *telemetry.Metrics
	Tracer  trace.Tracer

	Store       *store.Queries
	Concurrency int
	Queues      []string
	Logger      *slog.Logger

	id       string
	handlers map[string]HandlerFunc
	wg       sync.WaitGroup
}

// maxResumes bounds crash recovery (§2.5): a job whose worker dies is given
// its attempt back and resumed — but a job that kills every worker it touches
// is a poison pill, and must end up in the dead letter rather than cycle
// forever.
const maxResumes = 3

// KnownQueues is the set of logical queues a worker consumes. Enqueue refuses
// anything outside it: adding a queue means adding it here, not discovering
// months later that a job type has been silently piling up.
var KnownQueues = []string{"default", "deploy", "backup", "cleanup", "notify", "maintenance", "webhook", "task"}

// NewWorker builds a worker consuming the given logical queues.
func NewWorker(q *store.Queries, concurrency int, logger *slog.Logger) *Worker {
	hostname, _ := os.Hostname()
	suffix, _ := pguuid.New()
	return &Worker{
		Store:       q,
		Concurrency: max(concurrency, 1),
		Queues:      KnownQueues,
		Logger:      logger,
		id:          fmt.Sprintf("%s-%d-%.8s", hostname, os.Getpid(), pguuid.String(suffix)),
		handlers:    map[string]HandlerFunc{},
	}
}

// Register binds a job type to its handler.
func (w *Worker) Register(jobType string, h HandlerFunc) { w.handlers[jobType] = h }

// Run consumes jobs until ctx is cancelled, then waits for in-flight jobs
// (the caller bounds the drain with AKERDOCK_SHUTDOWN_TIMEOUT, §6.5).
func (w *Worker) Run(ctx context.Context) {
	w.Logger.Info("worker started", "worker_id", w.id, "concurrency", w.Concurrency)

	// Maintenance loop: promote retry_wait/scheduled jobs and reap expired
	// leases (INV-013).
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := w.Store.PromoteWaitingJobs(ctx); err != nil && ctx.Err() == nil {
					w.Logger.Warn("promote waiting jobs failed", "error", err)
				}
				reaped, err := w.Store.ReapExpiredLeases(ctx, maxResumes)
				if err != nil && ctx.Err() == nil {
					w.Logger.Warn("lease reaper failed", "error", err)
				}
				for _, j := range reaped {
					w.Logger.Warn("expired lease reaped", "job_uuid", pguuid.String(j.Uuid), "new_status", j.Status)
				}
			}
		}
	}()

	for range w.Concurrency {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			for ctx.Err() == nil {
				job, err := w.Store.DequeueJob(ctx, store.DequeueJobParams{
					Queues:       w.Queues,
					WorkerID:     &w.id,
					LeaseSeconds: int32(LeaseDuration.Seconds()),
				})
				switch {
				case errors.Is(err, pgx.ErrNoRows):
					select {
					case <-ctx.Done():
					case <-time.After(time.Second):
					}
				case err != nil:
					if ctx.Err() == nil {
						w.Logger.Warn("dequeue failed", "error", err)
					}
					select {
					case <-ctx.Done():
					case <-time.After(2 * time.Second):
					}
				default:
					w.process(ctx, job)
				}
			}
		}()
	}

	<-ctx.Done()
}

// Wait blocks until in-flight jobs finish or the timeout elapses.
func (w *Worker) Wait(timeout time.Duration) {
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		w.Logger.Warn("worker drain timed out — unfinished jobs will be reclaimed after lease expiry")
	}
}

func (w *Worker) process(ctx context.Context, job store.Job) {
	logger := w.Logger.With("job_uuid", pguuid.String(job.Uuid), "job_type", job.JobType, "attempt", job.Attempt)
	if rows, err := w.Store.MarkJobRunning(ctx, store.MarkJobRunningParams{ID: job.ID, LeasedBy: &w.id}); err != nil || rows == 0 {
		logger.Warn("lost lease before running", "error", err)
		return
	}

	// Heartbeats keep the lease alive during the drain too (§6.5).
	hbCtx, stopHB := context.WithCancel(context.WithoutCancel(ctx))
	defer stopHB()
	go func() {
		ticker := time.NewTicker(LeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if _, err := w.Store.HeartbeatJob(hbCtx, store.HeartbeatJobParams{
					ID: job.ID, LeasedBy: &w.id, LeaseSeconds: int32(LeaseDuration.Seconds()),
				}); err != nil {
					logger.Warn("heartbeat failed", "error", err)
				}
			}
		}
	}()

	handler, ok := w.handlers[job.JobType]
	if !ok {
		w.fail(ctx, logger, job, fmt.Errorf("no handler registered for job type %q", job.JobType))
		return
	}

	// One span per job: the unit an operator actually reasons about (ADR-008).
	// The tracer is a no-op when no OTLP endpoint is configured, so this costs
	// nothing on an instance without a collector.
	started := time.Now()
	ctx, span := w.tracer().Start(ctx, "job "+job.JobType,
		trace.WithAttributes(
			attribute.String("job.type", job.JobType),
			attribute.String("job.queue", job.Queue),
			attribute.Int("job.attempt", int(job.Attempt)),
		))
	defer span.End()

	rec := NewStepRecorder(w.Store, job)
	result, err := handler(ctx, job, rec)
	rec.Flush(ctx)
	if err != nil {
		telemetry.SpanError(span, err)
		span.SetStatus(codes.Error, "job failed")
		status := "failed"
		if job.Attempt >= job.MaxAttempts {
			status = "dead_letter"
		}
		w.Metrics.RecordJob(ctx, job.JobType, status, time.Since(started).Seconds())
		w.fail(ctx, logger, job, err)
		return
	}
	w.Metrics.RecordJob(ctx, job.JobType, "succeeded", time.Since(started).Seconds())

	payload, merr := json.Marshal(result)
	if merr != nil || result == nil {
		payload = nil
	}
	if rows, err := w.Store.SucceedJob(ctx, store.SucceedJobParams{ID: job.ID, Result: payload, WorkerID: &w.id}); err != nil || rows == 0 {
		logger.Warn("could not mark job succeeded (lease lost?)", "error", err)
		return
	}
	logger.Info("job succeeded")
}

func (w *Worker) fail(ctx context.Context, logger *slog.Logger, job store.Job, jobErr error) {
	toDead := job.Attempt >= job.MaxAttempts
	nextRun := time.Now()
	if !toDead {
		nextRun = nextRun.Add(retryBackoff(job.Attempt))
	}
	msg := jobErr.Error()
	if _, err := w.Store.FailJob(ctx, store.FailJobParams{
		ID:           job.ID,
		LastError:    &msg,
		NextRunAt:    pgtype.Timestamptz{Time: nextRun, Valid: true},
		ToDeadLetter: toDead,
		WorkerID:     &w.id,
	}); err != nil {
		logger.Error("could not record job failure", "error", err)
		return
	}
	if toDead {
		logger.Error("job dead-lettered", "error", jobErr)
	} else {
		logger.Warn("job attempt failed, will retry", "error", jobErr, "next_run_at", nextRun)
	}
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// tracer returns the configured tracer, or a no-op one.
func (w *Worker) tracer() trace.Tracer {
	if w.Tracer != nil {
		return w.Tracer
	}
	return noopTracer
}

var noopTracer = otel.Tracer("github.com/deepteams/akerdock/queue")
