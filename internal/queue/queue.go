// Package queue implements the durable PostgreSQL job queue of ADR-002:
// enqueue with idempotency keys, FOR UPDATE SKIP LOCKED dequeue, leases
// with heartbeat, bounded retries with jitter, and a dead letter (§21.3).
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// LeaseDuration is the job lease TTL (deployment-engine §2.5): unfinished
// jobs are picked up by another worker only after it expires, and never
// replayed blindly.
const LeaseDuration = 90 * time.Second

// EnqueueOptions describes a job to enqueue.
type EnqueueOptions struct {
	Queue          string // logical queue, e.g. deploy, backup, maintenance
	Type           string // e.g. server.validate
	Payload        any    // JSON-serializable; never contains secrets (INV-003)
	Priority       int32
	RunAt          time.Time // zero = now
	MaxAttempts    int32     // 0 = default 5
	IdempotencyKey *string
	LockKey        *string // e.g. server:validate:<uuid>
	TeamID         *int64
	ResourceID     *int64
	RetryOfID      *int64
}

type EnqueueStore interface {
	EnqueueJob(context.Context, store.EnqueueJobParams) (store.Job, error)
	GetJobByIdempotencyKey(context.Context, *string) (store.Job, error)
}

// Enqueue inserts a job. When an idempotency key conflicts, the original
// job is returned instead (INV-004).
func Enqueue(ctx context.Context, q EnqueueStore, opts EnqueueOptions) (store.Job, error) {
	// A job queued into a queue nobody consumes is a job that never runs and
	// never errors — the worst possible failure. Refuse it here, loudly, rather
	// than let it sit `queued` forever.
	if opts.Queue == "" {
		opts.Queue = "default"
	}
	if !slices.Contains(KnownQueues, opts.Queue) {
		return store.Job{}, fmt.Errorf("queue: %q is not consumed by any worker (known: %v)", opts.Queue, KnownQueues)
	}
	payload, err := json.Marshal(opts.Payload)
	if err != nil {
		return store.Job{}, fmt.Errorf("queue: payload: %w", err)
	}
	if opts.Payload == nil {
		payload = []byte("{}")
	}
	u, err := pguuid.New()
	if err != nil {
		return store.Job{}, err
	}
	status := store.JobStatusQueued
	runAt := time.Now()
	if !opts.RunAt.IsZero() && opts.RunAt.After(runAt) {
		runAt = opts.RunAt
		status = store.JobStatusScheduled
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	job, err := q.EnqueueJob(ctx, store.EnqueueJobParams{
		Uuid:           u,
		Queue:          opts.Queue,
		JobType:        opts.Type,
		Payload:        payload,
		Priority:       opts.Priority,
		RunAt:          pgtype.Timestamptz{Time: runAt, Valid: true},
		MaxAttempts:    maxAttempts,
		IdempotencyKey: opts.IdempotencyKey,
		LockKey:        opts.LockKey,
		TeamID:         opts.TeamID,
		ResourceID:     opts.ResourceID,
		CorrelationID:  pgtype.UUID{},
		RetryOfID:      opts.RetryOfID,
		Status:         status,
	})
	if err == nil {
		return job, nil
	}
	// ON CONFLICT DO NOTHING yields no row: replay of the same key returns
	// the original job.
	if opts.IdempotencyKey != nil && isNoRows(err) {
		return q.GetJobByIdempotencyKey(ctx, opts.IdempotencyKey)
	}
	return store.Job{}, fmt.Errorf("queue: enqueue: %w", err)
}

// RetryBase is the first retry delay; it doubles at each attempt (§22.1). It is
// a variable, not a constant, so an operator — or a test suite that fails jobs
// on purpose — can tune it without patching the engine.
var RetryBase = 5 * time.Second

// retryBackoff computes the next run delay: exponential from RetryBase, capped
// at 5 minutes, with ±20% jitter (§22.1).
func retryBackoff(attempt int32) time.Duration {
	base := RetryBase << max(attempt-1, 0)
	if base > 5*time.Minute || base <= 0 {
		base = 5 * time.Minute
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(base) * jitter)
}
