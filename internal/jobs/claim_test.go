package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/contracts"
)

func planForDatabase(reqID, execID string, dbID contracts.DatabaseID) contracts.ExecutionPlan {
	p := sampleExecutionPlan()
	p.RequestID = contracts.OperationRequestID(reqID)
	p.ExecutionID = contracts.ExecutionID(execID)
	p.TargetDatabaseID = dbID
	p.PlanChecksum = p.ComputeChecksum()
	return p
}

// TestClaimPerDatabaseWithoutStarvation verifies QUEUE-02:
// When database A has a RUNNING job, a second queued job for database A is skipped
// while a queued job for database B is immediately claimed by the next worker.
func TestClaimPerDatabaseWithoutStarvation(t *testing.T) {
	backingDB := &fakeJobsDB{}
	sqlDB := sql.OpenDB(fakeJobsConnector{db: backingDB})
	t.Cleanup(func() { _ = sqlDB.Close() })

	now := time.Date(2026, 11, 4, 13, 0, 0, 0, time.UTC)
	seq := 0
	store := NewPostgresJobStore(sqlDB).
		WithClock(func() time.Time { return now }).
		WithIDGenerator(func(prefix string) (string, error) {
			seq++
			return fmt.Sprintf("%s-%03d", prefix, seq), nil
		})

	ctx := context.Background()

	jobA1, err := store.Publish(ctx, PublishInput{
		Plan:           planForDatabase("req-a1", "exec-a1", "db-A"),
		IdempotencyKey: "idem-a1",
	})
	if err != nil {
		t.Fatalf("Publish A1: %v", err)
	}

	now = now.Add(1 * time.Millisecond)
	jobA2, err := store.Publish(ctx, PublishInput{
		Plan:           planForDatabase("req-a2", "exec-a2", "db-A"),
		IdempotencyKey: "idem-a2",
	})
	if err != nil {
		t.Fatalf("Publish A2: %v", err)
	}

	now = now.Add(1 * time.Millisecond)
	jobB1, err := store.Publish(ctx, PublishInput{
		Plan:           planForDatabase("req-b1", "exec-b1", "db-B"),
		IdempotencyKey: "idem-b1",
	})
	if err != nil {
		t.Fatalf("Publish B1: %v", err)
	}

	// Worker 1 claims the earliest due job (jobA1 on db-A).
	claimed1, err := store.ClaimNextDue(ctx, "worker-1", 30*time.Second)
	if err != nil {
		t.Fatalf("Worker 1 ClaimNextDue: %v", err)
	}
	if claimed1.Job.ID != jobA1.ID || claimed1.Attempt.AttemptNo != 1 {
		t.Fatalf("Worker 1 claimed %s (attempt %d), want %s (attempt 1)", claimed1.Job.ID, claimed1.Attempt.AttemptNo, jobA1.ID)
	}

	// Worker 2 must NOT be starved by jobA2 (which is on busy db-A); it must claim jobB1 on db-B!
	claimed2, err := store.ClaimNextDue(ctx, "worker-2", 30*time.Second)
	if err != nil {
		t.Fatalf("Worker 2 ClaimNextDue: %v", err)
	}
	if claimed2.Job.ID != jobB1.ID {
		t.Fatalf("Worker 2 claimed %s, want %s (db-B)", claimed2.Job.ID, jobB1.ID)
	}

	// Worker 3 sees both db-A and db-B busy with active RUNNING leases -> ErrNoJobAvailable.
	if _, err := store.ClaimNextDue(ctx, "worker-3", 30*time.Second); !errors.Is(err, ErrNoJobAvailable) {
		t.Fatalf("Worker 3 ClaimNextDue = %v, want ErrNoJobAvailable", err)
	}

	// Once Worker 1 completes jobA1, jobA2 on db-A becomes claimable!
	_, _, err = store.CompleteAttempt(ctx, CompleteAttemptInput{
		JobID:         claimed1.Job.ID,
		AttemptID:     claimed1.Attempt.ID,
		LeaseToken:    claimed1.Job.LeaseToken,
		JobStatus:     contracts.JobStatusSuccess,
		AttemptStatus: contracts.AttemptStatusCommitted,
		ExecutedSQL:   claimed1.Job.Plan.UpSQL,
	})
	if err != nil {
		t.Fatalf("CompleteAttempt A1: %v", err)
	}

	claimed3, err := store.ClaimNextDue(ctx, "worker-1", 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimNextDue for A2 after A1 completed: %v", err)
	}
	if claimed3.Job.ID != jobA2.ID {
		t.Fatalf("claimed %s, want %s", claimed3.Job.ID, jobA2.ID)
	}
}

// TestLeaseHeartbeatAndStaleWorkerFencing verifies QUEUE-02:
// 1. Heartbeat extends lease_expires_at while the lease token matches.
// 2. When a worker's lease expires, another worker reclaims the job with a new lease_token and attempt_no = 2.
// 3. The stale worker with the old lease_token is fenced out (ErrLeaseMismatch) on Heartbeat, UpdateProgress, CompleteAttempt, and ScheduleRetry.
func TestLeaseHeartbeatAndStaleWorkerFencing(t *testing.T) {
	backingDB := &fakeJobsDB{}
	sqlDB := sql.OpenDB(fakeJobsConnector{db: backingDB})
	t.Cleanup(func() { _ = sqlDB.Close() })

	now := time.Date(2026, 11, 4, 14, 0, 0, 0, time.UTC)
	seq := 0
	store := NewPostgresJobStore(sqlDB).
		WithClock(func() time.Time { return now }).
		WithIDGenerator(func(prefix string) (string, error) {
			seq++
			return fmt.Sprintf("%s-%03d", prefix, seq), nil
		})

	ctx := context.Background()
	_, err := store.Publish(ctx, PublishInput{
		Plan:           planForDatabase("req-lease", "exec-lease", "db-1"),
		IdempotencyKey: "idem-lease",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	claimed1, err := store.ClaimNextDue(ctx, "worker-1", 30*time.Second)
	if err != nil {
		t.Fatalf("Worker 1 ClaimNextDue: %v", err)
	}

	// Advance 10s and heartbeat from worker-1 -> lease extended to now+30s.
	now = now.Add(10 * time.Second)
	newExpiry, err := store.Heartbeat(ctx, claimed1.Job.ID, claimed1.Job.LeaseToken, 30*time.Second)
	if err != nil {
		t.Fatalf("Worker 1 Heartbeat: %v", err)
	}
	if !newExpiry.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("Heartbeat expiry = %v, want %v", newExpiry, now.Add(30*time.Second))
	}

	// Simulate worker-1 stall beyond lease expiration (advance 35s).
	now = now.Add(35 * time.Second)

	// Worker-2 reclaims the expired job -> gets a fresh lease_token and attempt #2.
	claimed2, err := store.ClaimNextDue(ctx, "worker-2", 30*time.Second)
	if err != nil {
		t.Fatalf("Worker 2 reclaim expired job: %v", err)
	}
	if claimed2.Job.ID != claimed1.Job.ID {
		t.Fatalf("reclaimed job ID = %s, want %s", claimed2.Job.ID, claimed1.Job.ID)
	}
	if claimed2.Job.LeaseToken == claimed1.Job.LeaseToken {
		t.Fatalf("expected new lease token on reclaim, got identical %q", claimed2.Job.LeaseToken)
	}
	if claimed2.Attempt.AttemptNo != 2 || claimed2.Job.RetryCount != 1 {
		t.Fatalf("reclaimed attemptNo=%d retryCount=%d, want 2 and 1", claimed2.Attempt.AttemptNo, claimed2.Job.RetryCount)
	}

	// Stale worker-1 wakes up and tries to heartbeat, update progress, complete, or retry with its old token -> all must fail with ErrLeaseMismatch!
	if _, err := store.Heartbeat(ctx, claimed1.Job.ID, claimed1.Job.LeaseToken, 30*time.Second); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("stale Heartbeat = %v, want ErrLeaseMismatch", err)
	}
	if err := store.UpdateProgress(ctx, claimed1.Job.ID, claimed1.Job.LeaseToken, 1, 100, 500, "100"); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("stale UpdateProgress = %v, want ErrLeaseMismatch", err)
	}
	if _, _, err := store.CompleteAttempt(ctx, CompleteAttemptInput{
		JobID:         claimed1.Job.ID,
		AttemptID:     claimed1.Attempt.ID,
		LeaseToken:    claimed1.Job.LeaseToken,
		JobStatus:     contracts.JobStatusSuccess,
		AttemptStatus: contracts.AttemptStatusCommitted,
	}); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("stale CompleteAttempt = %v, want ErrLeaseMismatch", err)
	}
	if _, _, err := store.ScheduleRetry(ctx, ScheduleRetryInput{
		JobID:      claimed1.Job.ID,
		AttemptID:  claimed1.Attempt.ID,
		LeaseToken: claimed1.Job.LeaseToken,
		ErrorCode:  "STALE_ERR",
	}); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("stale ScheduleRetry = %v, want ErrLeaseMismatch", err)
	}

	// Worker-2 completes attempt #2 with its valid lease token -> succeeds.
	doneJob, doneAttempt, err := store.CompleteAttempt(ctx, CompleteAttemptInput{
		JobID:         claimed2.Job.ID,
		AttemptID:     claimed2.Attempt.ID,
		LeaseToken:    claimed2.Job.LeaseToken,
		JobStatus:     contracts.JobStatusSuccess,
		AttemptStatus: contracts.AttemptStatusCommitted,
		ExecutedSQL:   claimed2.Job.Plan.UpSQL,
	})
	if err != nil {
		t.Fatalf("Worker 2 CompleteAttempt: %v", err)
	}
	if doneJob.Status != contracts.JobStatusSuccess || doneJob.CompletedAt == nil || doneAttempt.Status != contracts.AttemptStatusCommitted {
		t.Fatalf("unexpected completed state: job=%+v attempt=%+v", doneJob, doneAttempt)
	}
}

// TestScheduleRetryExponentialBackoffAndMaxRetries verifies QUEUE-02 bounded retries:
// Retries follow exponential backoff (1s, 2s) and transition terminally to FAILED once max_retries is reached.
func TestScheduleRetryExponentialBackoffAndMaxRetries(t *testing.T) {
	backingDB := &fakeJobsDB{}
	sqlDB := sql.OpenDB(fakeJobsConnector{db: backingDB})
	t.Cleanup(func() { _ = sqlDB.Close() })

	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	seq := 0
	store := NewPostgresJobStore(sqlDB).
		WithClock(func() time.Time { return now }).
		WithIDGenerator(func(prefix string) (string, error) {
			seq++
			return fmt.Sprintf("%s-%03d", prefix, seq), nil
		})

	ctx := context.Background()
	_, err := store.Publish(ctx, PublishInput{
		Plan:           planForDatabase("req-retry", "exec-retry", "db-retry"),
		IdempotencyKey: "idem-retry",
		MaxRetries:     2,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Attempt 1 (initial attempt, retry_count = 0)
	c1, err := store.ClaimNextDue(ctx, "worker-1", 30*time.Second)
	if err != nil {
		t.Fatalf("Claim attempt 1: %v", err)
	}
	j1, a1, err := store.ScheduleRetry(ctx, ScheduleRetryInput{
		JobID:               c1.Job.ID,
		AttemptID:           c1.Attempt.ID,
		LeaseToken:          c1.Job.LeaseToken,
		ErrorCode:           "08006",
		ErrorMessage:        "connection failure",
		BaseBackoff:         1 * time.Second,
		MaxBackoff:          10 * time.Second,
		DisableRandomJitter: true,
	})
	if err != nil {
		t.Fatalf("ScheduleRetry #1: %v", err)
	}
	if j1.Status != contracts.JobStatusRetryWait || j1.RetryCount != 1 || !j1.AvailableAt.Equal(now.Add(1*time.Second)) {
		t.Fatalf("after retry #1: status=%s retryCount=%d availableAt=%v", j1.Status, j1.RetryCount, j1.AvailableAt)
	}
	if a1.Status != contracts.AttemptStatusFailed || a1.FinishedAt == nil {
		t.Fatalf("attempt 1 not marked FAILED: %+v", a1)
	}

	// Before 1s backoff elapses, job is not yet due.
	now = now.Add(500 * time.Millisecond)
	if _, err := store.ClaimNextDue(ctx, "worker-1", 30*time.Second); !errors.Is(err, ErrNoJobAvailable) {
		t.Fatalf("Claim before backoff due = %v, want ErrNoJobAvailable", err)
	}

	// Advance past 1s backoff -> Claim attempt 2.
	now = now.Add(600 * time.Millisecond)
	c2, err := store.ClaimNextDue(ctx, "worker-1", 30*time.Second)
	if err != nil {
		t.Fatalf("Claim attempt 2: %v", err)
	}
	if c2.Attempt.AttemptNo != 2 {
		t.Fatalf("attempt 2 AttemptNo = %d, want 2", c2.Attempt.AttemptNo)
	}

	// Fail attempt 2 -> retry_count becomes 2, backoff is 2s (1s * 2^1).
	j2, _, err := store.ScheduleRetry(ctx, ScheduleRetryInput{
		JobID:               c2.Job.ID,
		AttemptID:           c2.Attempt.ID,
		LeaseToken:          c2.Job.LeaseToken,
		ErrorCode:           "08006",
		ErrorMessage:        "connection failure again",
		BaseBackoff:         1 * time.Second,
		MaxBackoff:          10 * time.Second,
		DisableRandomJitter: true,
	})
	if err != nil {
		t.Fatalf("ScheduleRetry #2: %v", err)
	}
	if j2.Status != contracts.JobStatusRetryWait || j2.RetryCount != 2 || !j2.AvailableAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("after retry #2: status=%s retryCount=%d availableAt=%v", j2.Status, j2.RetryCount, j2.AvailableAt)
	}

	// Advance 2s -> Claim attempt 3 (final attempt since retry_count == max_retries == 2).
	now = now.Add(2 * time.Second)
	c3, err := store.ClaimNextDue(ctx, "worker-1", 30*time.Second)
	if err != nil {
		t.Fatalf("Claim attempt 3: %v", err)
	}
	if c3.Attempt.AttemptNo != 3 {
		t.Fatalf("attempt 3 AttemptNo = %d, want 3", c3.Attempt.AttemptNo)
	}

	// Fail attempt 3 -> retry budget exhausted (retry_count 2 == max_retries 2) -> terminal FAILED!
	j3, a3, err := store.ScheduleRetry(ctx, ScheduleRetryInput{
		JobID:               c3.Job.ID,
		AttemptID:           c3.Attempt.ID,
		LeaseToken:          c3.Job.LeaseToken,
		ErrorCode:           "08006",
		ErrorMessage:        "final connection failure",
		BaseBackoff:         1 * time.Second,
		MaxBackoff:          10 * time.Second,
		DisableRandomJitter: true,
	})
	if err != nil {
		t.Fatalf("ScheduleRetry #3 (exhausted): %v", err)
	}
	if j3.Status != contracts.JobStatusFailed || j3.CompletedAt == nil || a3.Status != contracts.AttemptStatusFailed {
		t.Fatalf("expected terminal FAILED job and attempt, got job=%+v attempt=%+v", j3, a3)
	}

	// Even after advancing clock arbitrarily far, terminal FAILED job is never claimed again.
	now = now.Add(1 * time.Hour)
	if _, err := store.ClaimNextDue(ctx, "worker-1", 30*time.Second); !errors.Is(err, ErrNoJobAvailable) {
		t.Fatalf("Claim after terminal FAILED = %v, want ErrNoJobAvailable", err)
	}
}

func TestClaimSkipsGatedDatabaseExceptRecoveryJobs(t *testing.T) {
	backingDB := &fakeJobsDB{
		gates: map[string]string{
			"db-gated": string(contracts.GateReasonPartial),
		},
	}
	sqlDB := sql.OpenDB(fakeJobsConnector{db: backingDB})
	t.Cleanup(func() { _ = sqlDB.Close() })

	now := time.Date(2026, 11, 4, 16, 0, 0, 0, time.UTC)
	seq := 0
	store := NewPostgresJobStore(sqlDB).
		WithClock(func() time.Time { return now }).
		WithIDGenerator(func(prefix string) (string, error) {
			seq++
			return fmt.Sprintf("%s-%03d", prefix, seq), nil
		})

	ctx := context.Background()

	// Normal APPLY job on gated database must be skipped while PARTIAL gate is active.
	_, err := store.Publish(ctx, PublishInput{
		Plan:           planForDatabase("req-blocked", "exec-blocked", "db-gated"),
		IdempotencyKey: "idem-blocked",
	})
	if err != nil {
		t.Fatalf("Publish normal job on gated DB: %v", err)
	}

	if _, err := store.ClaimNextDue(ctx, "worker-1", 30*time.Second); !errors.Is(err, ErrNoJobAvailable) {
		t.Fatalf("ClaimNextDue on gated DB for APPLY job = %v, want ErrNoJobAvailable", err)
	}

	// A RESUME job on the same PARTIAL-gated database IS allowed to be claimed to resolve the gate.
	resumePlan := planForDatabase("req-resume", "exec-resume", "db-gated")
	resumePlan.Direction = contracts.DirectionResume
	resumePlan.OriginExecutionID = "exec-origin-partial"
	resumePlan.PlanChecksum = resumePlan.ComputeChecksum()

	resumeJob, err := store.Publish(ctx, PublishInput{
		Plan:           resumePlan,
		IdempotencyKey: "idem-resume",
	})
	if err != nil {
		t.Fatalf("Publish RESUME job: %v", err)
	}

	claimed, err := store.ClaimNextDue(ctx, "worker-1", 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimNextDue for RESUME job on PARTIAL gated DB: %v", err)
	}
	if claimed.Job.ID != resumeJob.ID {
		t.Fatalf("claimed job = %s, want RESUME job %s", claimed.Job.ID, resumeJob.ID)
	}
}

func TestClaimAndRetryIntegrationPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_PLATFORM_DATABASE_URL"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("PLATFORM_TEST_DATABASE_URL"))
	}
	if dsn == "" {
		t.Skip("TEST_PLATFORM_DATABASE_URL / PLATFORM_TEST_DATABASE_URL not set; skipping live Postgres integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	schemaName := fmt.Sprintf("test_jobs_claim_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+schemaName); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
	}()
	if _, err := db.ExecContext(ctx, "SET search_path TO "+schemaName); err != nil {
		t.Fatalf("set search_path: %v", err)
	}

	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatalf("migrations.Apply: %v", err)
	}

	now := time.Date(2026, 11, 4, 17, 0, 0, 0, time.UTC)
	store := NewPostgresJobStore(db).WithClock(func() time.Time { return now })

	planA := seedApprovedQueueRequest(t, ctx, db, planForDatabase("pg-req-a", "pg-exec-a", "db-A"))
	jobA, err := store.Publish(ctx, PublishInput{
		Plan:           planA,
		IdempotencyKey: "pg-claim-a",
		MaxRetries:     1,
	})
	if err != nil {
		t.Fatalf("Publish jobA: %v", err)
	}
	planB := seedApprovedQueueRequest(t, ctx, db, planForDatabase("pg-req-b", "pg-exec-b", "db-B"))
	jobB, err := store.Publish(ctx, PublishInput{
		Plan:           planB,
		IdempotencyKey: "pg-claim-b",
		MaxRetries:     1,
	})
	if err != nil {
		t.Fatalf("Publish jobB: %v", err)
	}

	cA, err := store.ClaimNextDue(ctx, "w-1", 30*time.Second)
	if err != nil || cA.Job.ID != jobA.ID {
		t.Fatalf("ClaimNextDue #1 = %+v, %v; want %s", cA, err, jobA.ID)
	}
	cB, err := store.ClaimNextDue(ctx, "w-2", 30*time.Second)
	if err != nil || cB.Job.ID != jobB.ID {
		t.Fatalf("ClaimNextDue #2 = %+v, %v; want %s", cB, err, jobB.ID)
	}
}
