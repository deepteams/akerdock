package queue

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepteams/akerdock/internal/store"
)

// The queue is the spine of the system (ADR-002) and its guarantees —
// exactly-one-worker-per-job, mutual exclusion by lock_key, lease reclaim
// after a crash — are properties of the SQL, not of the Go around it. Mocking
// the database would test the mock. So these run against a real PostgreSQL.
//
// AKERDOCK_TEST_DATABASE_URL points at a throwaway database; without it the
// tests skip rather than fail, so `go test ./...` stays usable on a laptop
// with no database. CI sets it (see .github/workflows/ci.yml).

func testPool(t *testing.T) (*pgxpool.Pool, *store.Queries) {
	t.Helper()
	url := os.Getenv("AKERDOCK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AKERDOCK_TEST_DATABASE_URL is not set — skipping the queue integration tests")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("cannot connect: %v", err)
	}
	t.Cleanup(pool.Close)

	q := store.New(pool)
	// Each test starts from an empty queue: leftovers from a previous run would
	// make dequeue order non-deterministic.
	if _, err := pool.Exec(context.Background(), "DELETE FROM jobs"); err != nil {
		t.Fatalf("cannot clean the jobs table (are migrations applied?): %v", err)
	}
	return pool, q
}

func enqueue(t *testing.T, q *store.Queries, opts ...func(*EnqueueOptions)) store.Job {
	t.Helper()
	o := EnqueueOptions{Queue: "default", Type: "test.job"}
	for _, fn := range opts {
		fn(&o)
	}
	job, err := Enqueue(context.Background(), q, o)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return job
}

func dequeue(t *testing.T, q *store.Queries, worker string) (store.Job, bool) {
	t.Helper()
	job, err := q.DequeueJob(context.Background(), store.DequeueJobParams{
		Queues: KnownQueues, WorkerID: &worker, LeaseSeconds: 90,
	})
	if isNoRows(err) {
		return store.Job{}, false
	}
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	return job, true
}

// The core promise: a job is handed to exactly one worker. Two workers racing
// on one job must not both get it — that is what FOR UPDATE SKIP LOCKED buys,
// and it is worth proving rather than trusting.
func TestDequeueGivesEachJobToOneWorkerOnly(t *testing.T) {
	_, q := testPool(t)
	const jobs = 20
	for range jobs {
		enqueue(t, q)
	}

	var mu sync.Mutex
	seen := map[int64]string{}
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			for {
				job, ok := dequeue(t, q, worker)
				if !ok {
					return
				}
				mu.Lock()
				if other, dup := seen[job.ID]; dup {
					t.Errorf("job %d was dequeued twice: by %s and %s", job.ID, other, worker)
				}
				seen[job.ID] = worker
				mu.Unlock()
			}
		}(string(rune('a' + w)))
	}
	wg.Wait()

	if len(seen) != jobs {
		t.Errorf("%d jobs dequeued, want %d", len(seen), jobs)
	}
}

// lock_key is mutual exclusion: two deployments of the same application must
// never run at once, however many workers are free.
func TestLockKeyExcludesConcurrentJobs(t *testing.T) {
	_, q := testPool(t)
	key := "deploy:app:same"
	first := enqueue(t, q, func(o *EnqueueOptions) { o.LockKey = &key })
	enqueue(t, q, func(o *EnqueueOptions) { o.LockKey = &key })

	got, ok := dequeue(t, q, "w1")
	if !ok || got.ID != first.ID {
		t.Fatalf("the first job was not dequeued")
	}
	// The second one shares the lock key: it must stay put while the first is
	// leased, even though a worker is asking for it.
	if _, ok := dequeue(t, q, "w2"); ok {
		t.Fatal("a job sharing a held lock_key was dequeued — mutual exclusion is broken")
	}

	// Once the first finishes, the second becomes available.
	if _, err := q.SucceedJob(context.Background(), store.SucceedJobParams{
		ID: first.ID, WorkerID: ptr("w1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := dequeue(t, q, "w2"); !ok {
		t.Error("the second job was not released once the lock was free")
	}
}

// A worker that dies mid-job leaves its lease to expire. The reaper must give
// the job back — otherwise a crash loses work silently, which is the failure
// ADR-002 exists to prevent.
func TestExpiredLeaseIsReclaimed(t *testing.T) {
	pool, q := testPool(t)
	ctx := context.Background()
	job := enqueue(t, q)

	leased, ok := dequeue(t, q, "doomed-worker")
	if !ok || leased.ID != job.ID {
		t.Fatal("could not lease the job")
	}
	// Simulate the worker dying: the lease is left to expire.
	expire(t, pool, job.ID)

	reclaimed, err := q.ReapExpiredLeases(ctx, maxResumes)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("the reaper reclaimed %d jobs, want 1", len(reclaimed))
	}
	// The reaper does not re-queue: it puts the job into retry_wait with a due
	// date. Promotion is a separate step, so a reclaimed job goes through the
	// same backoff as any other retry instead of hammering a broken target.
	if _, err := q.PromoteWaitingJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := dequeue(t, q, "healthy-worker"); !ok {
		t.Error("a job whose lease expired was not handed back to another worker")
	}
}

// Idempotency: the same key must never create a second job. This is what makes
// a retried HTTP call safe — a double POST must not deploy twice.
func TestIdempotencyKeyReturnsTheSameJob(t *testing.T) {
	_, q := testPool(t)
	key := "idem-1"
	first := enqueue(t, q, func(o *EnqueueOptions) { o.IdempotencyKey = &key })
	second := enqueue(t, q, func(o *EnqueueOptions) { o.IdempotencyKey = &key })

	if first.ID != second.ID {
		t.Errorf("the same idempotency key created two jobs (%d and %d)", first.ID, second.ID)
	}
}

// A queue nobody consumes is a job nobody runs — and no error either, which is
// the worst possible outcome. Enqueue must refuse it outright.
func TestEnqueueRefusesUnknownQueue(t *testing.T) {
	_, q := testPool(t)
	_, err := Enqueue(context.Background(), q, EnqueueOptions{Queue: "nobody-listens", Type: "test.job"})
	if err == nil {
		t.Fatal("a job queued into an unconsumed queue was accepted — it would never run and never fail")
	}
}

// Backoff must grow and stay bounded: an unbounded retry storm on a failing
// remote is how a control plane takes its own targets down.
func TestRetryBackoffGrowsAndIsCapped(t *testing.T) {
	var previous time.Duration
	for attempt := int32(1); attempt <= 6; attempt++ {
		got := retryBackoff(attempt)
		if got > 6*time.Minute {
			t.Errorf("attempt %d: backoff %s exceeds the cap (+jitter)", attempt, got)
		}
		if attempt > 1 && attempt < 5 && got <= previous/2 {
			t.Errorf("attempt %d: backoff %s did not grow past %s", attempt, got, previous)
		}
		previous = got
	}
	// Far out, it must be the cap and not an overflow back to zero.
	if late := retryBackoff(40); late < 3*time.Minute {
		t.Errorf("a late attempt backed off only %s — the shift overflowed", late)
	}
}

func ptr[T any](v T) *T { return &v }

// A crash is not a failure. A deployment gets max_attempts=1 — a FAILED
// deployment is terminal and must never be retried blindly — but a worker that
// DIES has not failed anything: it simply never finished (INV-013). Conflating
// the two dead-lettered every job whose worker crashed, which is precisely the
// work the queue exists to protect.
func TestCrashGivesTheAttemptBack(t *testing.T) {
	pool, q := testPool(t)
	ctx := context.Background()
	job := enqueue(t, q, func(o *EnqueueOptions) { o.MaxAttempts = 1 })

	leased, _ := dequeue(t, q, "doomed")
	if leased.Attempt != 1 {
		t.Fatalf("attempt = %d after the first lease, want 1", leased.Attempt)
	}
	expire(t, pool, job.ID)

	reclaimed, err := q.ReapExpiredLeases(ctx, maxResumes)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Status == store.JobStatusDeadLetter {
		t.Fatalf("a crashed job with max_attempts=1 was dead-lettered instead of resumed: %+v", reclaimed)
	}
	if _, err := q.PromoteWaitingJobs(ctx); err != nil {
		t.Fatal(err)
	}
	again, ok := dequeue(t, q, "resumer")
	if !ok {
		t.Fatal("the crashed job was not handed back")
	}
	// The attempt was given back, so the resume is attempt 1 again — it has not
	// burned the only attempt the deployment is allowed.
	if again.Attempt != 1 {
		t.Errorf("attempt = %d on the resume, want 1 (the crash must not consume it)", again.Attempt)
	}
}

// ...but a job that kills every worker it touches must not cycle forever.
func TestRepeatedCrashesEventuallyDeadLetter(t *testing.T) {
	pool, q := testPool(t)
	ctx := context.Background()
	job := enqueue(t, q, func(o *EnqueueOptions) { o.MaxAttempts = 1 })

	var last store.JobStatus
	for range maxResumes + 1 {
		if _, ok := dequeue(t, q, "doomed"); !ok {
			break
		}
		expire(t, pool, job.ID)
		reclaimed, err := q.ReapExpiredLeases(ctx, maxResumes)
		if err != nil {
			t.Fatal(err)
		}
		if len(reclaimed) == 1 {
			last = reclaimed[0].Status
		}
		if _, err := q.PromoteWaitingJobs(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if last != store.JobStatusDeadLetter {
		t.Errorf("a job that crashed %d times ended as %q, want dead_letter — a poison pill must not loop forever",
			maxResumes+1, last)
	}
}

// expire simulates a worker dying with the job leased.
func expire(t *testing.T, pool *pgxpool.Pool, jobID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE jobs SET lease_expires_at = now() - interval '1 minute' WHERE id = $1", jobID); err != nil {
		t.Fatal(err)
	}
}
