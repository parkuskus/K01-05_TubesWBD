package execution

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/jobs"
)

type memoryJobQueue struct {
	mu            sync.Mutex
	jobs          []jobs.Job
	attempts      []jobs.JobAttempt
	heartbeats    atomic.Int32
	nextAttemptNo map[contracts.JobID]int
}

func newMemoryJobQueue(initial ...jobs.Job) *memoryJobQueue {
	q := &memoryJobQueue{
		jobs:          append([]jobs.Job(nil), initial...),
		nextAttemptNo: make(map[contracts.JobID]int),
	}
	return q
}

func (q *memoryJobQueue) ClaimNextDue(_ context.Context, workerID string, leaseDuration time.Duration) (jobs.ClaimedJob, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	activeDB := make(map[contracts.DatabaseID]bool)
	for _, j := range q.jobs {
		if j.Status == contracts.JobStatusRunning {
			activeDB[j.DatabaseID] = true
		}
	}

	for i := range q.jobs {
		j := &q.jobs[i]
		if j.Status != contracts.JobStatusPending && j.Status != contracts.JobStatusRetryWait {
			continue
		}
		if activeDB[j.DatabaseID] {
			continue
		}
		now := time.Now().UTC()
		exp := now.Add(leaseDuration)
		j.Status = contracts.JobStatusRunning
		j.LeaseOwner = workerID
		j.LeaseToken = "lease-" + string(j.ID) + "-" + workerID
		j.LeaseExpiresAt = &exp
		j.UpdatedAt = now

		q.nextAttemptNo[j.ID]++
		attNo := q.nextAttemptNo[j.ID]
		att := jobs.JobAttempt{
			ID:          contracts.AttemptID("att-" + string(j.ID)),
			JobID:       j.ID,
			ExecutionID: j.ExecutionID,
			AttemptNo:   attNo,
			WorkerID:    workerID,
			Status:      contracts.AttemptStatusStarted,
			StartedAt:   now,
		}
		q.attempts = append(q.attempts, att)
		return jobs.ClaimedJob{Job: *j, Attempt: att}, nil
	}
	return jobs.ClaimedJob{}, jobs.ErrNoJobAvailable
}

func (q *memoryJobQueue) Heartbeat(_ context.Context, jobID contracts.JobID, leaseToken string, leaseDuration time.Duration) (time.Time, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.jobs {
		if q.jobs[i].ID == jobID && q.jobs[i].LeaseToken == leaseToken && q.jobs[i].Status == contracts.JobStatusRunning {
			q.heartbeats.Add(1)
			exp := time.Now().UTC().Add(leaseDuration)
			q.jobs[i].LeaseExpiresAt = &exp
			return exp, nil
		}
	}
	return time.Time{}, jobs.ErrLeaseMismatch
}

func (q *memoryJobQueue) CompleteAttempt(_ context.Context, in jobs.CompleteAttemptInput) (jobs.Job, jobs.JobAttempt, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.jobs {
		if q.jobs[i].ID == in.JobID && q.jobs[i].LeaseToken == in.LeaseToken && q.jobs[i].Status == contracts.JobStatusRunning {
			now := time.Now().UTC()
			q.jobs[i].Status = in.JobStatus
			q.jobs[i].LeaseOwner = ""
			q.jobs[i].LeaseToken = ""
			q.jobs[i].LeaseExpiresAt = nil
			q.jobs[i].LastErrorCode = in.ErrorCode
			q.jobs[i].LastErrorMessage = in.ErrorMessage
			q.jobs[i].CommittedBatches = in.CommittedBatches
			q.jobs[i].CommittedRows = in.CommittedRows
			q.jobs[i].TotalScopeRows = in.TotalScopeRows
			q.jobs[i].LastCommittedCursor = in.LastCommittedCursor
			q.jobs[i].UpdatedAt = now
			if in.JobStatus.IsTerminal() {
				q.jobs[i].CompletedAt = &now
			}

			var updatedAtt jobs.JobAttempt
			for k := range q.attempts {
				if q.attempts[k].ID == in.AttemptID {
					q.attempts[k].Status = in.AttemptStatus
					q.attempts[k].ExecutedSQL = in.ExecutedSQL
					q.attempts[k].ErrorCode = in.ErrorCode
					q.attempts[k].ErrorMessage = in.ErrorMessage
					q.attempts[k].FinishedAt = &now
					updatedAtt = q.attempts[k]
				}
			}
			return q.jobs[i], updatedAtt, nil
		}
	}
	return jobs.Job{}, jobs.JobAttempt{}, jobs.ErrLeaseMismatch
}

func (q *memoryJobQueue) ScheduleRetry(_ context.Context, in jobs.ScheduleRetryInput) (jobs.Job, jobs.JobAttempt, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.jobs {
		if q.jobs[i].ID == in.JobID && q.jobs[i].LeaseToken == in.LeaseToken && q.jobs[i].Status == contracts.JobStatusRunning {
			now := time.Now().UTC()
			q.jobs[i].LeaseOwner = ""
			q.jobs[i].LeaseToken = ""
			q.jobs[i].LeaseExpiresAt = nil
			q.jobs[i].LastErrorCode = in.ErrorCode
			q.jobs[i].LastErrorMessage = in.ErrorMessage
			q.jobs[i].UpdatedAt = now
			if q.jobs[i].RetryCount < q.jobs[i].MaxRetries {
				q.jobs[i].RetryCount++
				q.jobs[i].Status = contracts.JobStatusRetryWait
			} else {
				q.jobs[i].Status = contracts.JobStatusFailed
				q.jobs[i].CompletedAt = &now
			}

			var updatedAtt jobs.JobAttempt
			for k := range q.attempts {
				if q.attempts[k].ID == in.AttemptID {
					q.attempts[k].Status = contracts.AttemptStatusFailed
					q.attempts[k].ExecutedSQL = in.ExecutedSQL
					q.attempts[k].ErrorCode = in.ErrorCode
					q.attempts[k].ErrorMessage = in.ErrorMessage
					q.attempts[k].FinishedAt = &now
					updatedAtt = q.attempts[k]
				}
			}
			return q.jobs[i], updatedAtt, nil
		}
	}
	return jobs.Job{}, jobs.JobAttempt{}, jobs.ErrLeaseMismatch
}

func (q *memoryJobQueue) getJob(id contracts.JobID) jobs.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.ID == id {
			return j
		}
	}
	return jobs.Job{}
}

type fakeConnProvider struct {
	server    *fakeLockServer
	revisions map[contracts.DatabaseID]int64
	dbNames   map[contracts.DatabaseID]string
}

func (p *fakeConnProvider) AcquireExecutorConn(ctx context.Context, databaseID contracts.DatabaseID) (*sql.Conn, int64, error) {
	dbName := p.dbNames[databaseID]
	if dbName == "" {
		dbName = string(databaseID)
	}
	rev := p.revisions[databaseID]
	if rev == 0 {
		rev = 1
	}
	connector := &fakeLockConnector{
		server: p.server,
		dbName: dbName,
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, 0, err
	}
	return conn, rev, nil
}

type staticHeadReader struct {
	heads map[string]contracts.ActiveHeadState
}

func (r staticHeadReader) ReadActiveHeadOnConn(ctx context.Context, conn *sql.Conn) (contracts.ActiveHeadState, error) {
	var dbName string
	if err := conn.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		return contracts.ActiveHeadState{}, err
	}
	return r.heads[dbName], nil
}

type funcRevalidator func(ctx context.Context, plan contracts.ExecutionPlan, currentHead contracts.ActiveHeadState) error

func (fn funcRevalidator) Revalidate(ctx context.Context, plan contracts.ExecutionPlan, currentHead contracts.ActiveHeadState) error {
	return fn(ctx, plan, currentHead)
}

func makeTestJob(jobID string, dbID contracts.DatabaseID, expectedHead contracts.ExecutionID) jobs.Job {
	plan := contracts.ExecutionPlan{
		RequestID:                contracts.OperationRequestID("req-" + jobID),
		ExecutionID:              contracts.ExecutionID("exec-" + jobID),
		MigrationID:              contracts.MigrationID("mig-" + jobID),
		RevisionID:               "rev-1",
		Version:                  1,
		Direction:                contracts.DirectionApply,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-1",
		TargetDatabaseID:         dbID,
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       expectedHead,
		PolicyVersion:            1,
		UpSQL:                    "CREATE TABLE items (id INT PRIMARY KEY);",
		DownSQL:                  "DROP TABLE items;",
	}
	plan.PlanChecksum = plan.ComputeChecksum()
	return jobs.Job{
		ID:                 contracts.JobID(jobID),
		RequestID:          plan.RequestID,
		ExecutionID:        plan.ExecutionID,
		DatabaseID:         dbID,
		ConnectionRevision: 1,
		Direction:          plan.Direction,
		Mode:               plan.Mode,
		Status:             contracts.JobStatusPending,
		ActorID:            plan.ActorID,
		IdempotencyKey:     "idem-" + jobID,
		Plan:               plan,
		MaxRetries:         2,
	}
}

func TestWorkerExecutesAcrossDatabasesInParallelAndHeartbeats(t *testing.T) {
	jobA := makeTestJob("job-A", "db-A", "")
	jobB := makeTestJob("job-B", "db-B", "")
	queue := newMemoryJobQueue(jobA, jobB)
	lockServer := &fakeLockServer{}
	provider := &fakeConnProvider{
		server:    lockServer,
		revisions: map[contracts.DatabaseID]int64{"db-A": 1, "db-B": 1},
		dbNames:   map[contracts.DatabaseID]string{"db-A": "phys_a", "db-B": "phys_b"},
	}

	var entered atomic.Int32
	bothRunning := make(chan struct{})
	var closeOnce sync.Once

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	worker, err := NewWorker(WorkerConfig{
		WorkerID:          "w-parallel",
		Concurrency:       2,
		PollInterval:      10 * time.Millisecond,
		LeaseDuration:     200 * time.Millisecond,
		HeartbeatInterval: 15 * time.Millisecond,
		Queue:             queue,
		ConnProvider:      provider,
		Executor: PlanExecutorFunc(func(execCtx context.Context, env ExecutionEnv) (ExecutionResult, error) {
			if entered.Add(1) == 2 {
				closeOnce.Do(func() { close(bothRunning) })
			}
			// Wait until both db-A and db-B are executing simultaneously!
			select {
			case <-bothRunning:
			case <-execCtx.Done():
				return ExecutionResult{}, execCtx.Err()
			}
			// Sleep briefly so at least one lease heartbeat fires during execution.
			time.Sleep(35 * time.Millisecond)
			return ExecutionResult{
				JobStatus:     contracts.JobStatusSuccess,
				AttemptStatus: contracts.AttemptStatusCommitted,
				ExecutedSQL:   env.Claimed.Job.Plan.UpSQL,
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- worker.Run(ctx)
	}()

	// Wait for both jobs to reach SUCCESS, then trigger graceful shutdown.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a := queue.getJob("job-A")
		b := queue.getJob("job-B")
		if a.Status == contracts.JobStatusSuccess && b.Status == contracts.JobStatusSuccess {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	if err := <-doneCh; err != nil {
		t.Fatalf("worker.Run: %v", err)
	}

	if queue.getJob("job-A").Status != contracts.JobStatusSuccess || queue.getJob("job-B").Status != contracts.JobStatusSuccess {
		t.Fatalf("jobs did not both succeed in parallel: A=%s B=%s", queue.getJob("job-A").Status, queue.getJob("job-B").Status)
	}
	if queue.heartbeats.Load() < 1 {
		t.Fatalf("expected lease heartbeats to fire during execution, got %d", queue.heartbeats.Load())
	}
}

func TestWorkerRevalidationRejectsStaleApprovalHeadMismatchAndGateWithoutSQL(t *testing.T) {
	jobStale := makeTestJob("job-stale", "db-1", "exec-0")
	jobMismatch := makeTestJob("job-mismatch", "db-2", "exec-expected")
	jobGated := makeTestJob("job-gated", "db-3", "")

	queue := newMemoryJobQueue(jobStale, jobMismatch, jobGated)
	lockServer := &fakeLockServer{}
	provider := &fakeConnProvider{
		server:    lockServer,
		revisions: map[contracts.DatabaseID]int64{"db-1": 1, "db-2": 1, "db-3": 1},
		dbNames:   map[contracts.DatabaseID]string{"db-1": "phys_1", "db-2": "phys_2", "db-3": "phys_3"},
	}

	var executedCount atomic.Int32
	worker, err := NewWorker(WorkerConfig{
		WorkerID:     "w-reval",
		Concurrency:  1,
		Queue:        queue,
		ConnProvider: provider,
		HeadReader: staticHeadReader{
			heads: map[string]contracts.ActiveHeadState{
				"phys_1": {ExecutionID: "exec-0"},
				"phys_2": {ExecutionID: "exec-different-actual"},
				"phys_3": {UnresolvedExecutionID: "exec-prev", UnresolvedReason: contracts.GateReasonPartial},
			},
		},
		Revalidator: funcRevalidator(func(_ context.Context, plan contracts.ExecutionPlan, _ contracts.ActiveHeadState) error {
			if plan.TargetDatabaseID == "db-1" {
				return contracts.ErrStaleApproval
			}
			return nil
		}),
		Executor: PlanExecutorFunc(func(_ context.Context, _ ExecutionEnv) (ExecutionResult, error) {
			executedCount.Add(1)
			return ExecutionResult{JobStatus: contracts.JobStatusSuccess, AttemptStatus: contracts.AttemptStatusCommitted}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		processed, err := worker.ProcessOnce(ctx, "w-reval")
		if err != nil || !processed {
			t.Fatalf("ProcessOnce #%d: processed=%v err=%v", i+1, processed, err)
		}
	}

	if executedCount.Load() != 0 {
		t.Fatalf("Executor.Execute was called %d times; want 0 on preflight rejection", executedCount.Load())
	}

	s1 := queue.getJob("job-stale")
	if s1.Status != contracts.JobStatusFailed || s1.LastErrorCode != "STALE_APPROVAL" {
		t.Fatalf("job-stale = %s (%s), want FAILED (STALE_APPROVAL)", s1.Status, s1.LastErrorCode)
	}
	s2 := queue.getJob("job-mismatch")
	if s2.Status != contracts.JobStatusFailed || s2.LastErrorCode != "HEAD_MISMATCH" {
		t.Fatalf("job-mismatch = %s (%s), want FAILED (HEAD_MISMATCH)", s2.Status, s2.LastErrorCode)
	}
	s3 := queue.getJob("job-gated")
	if s3.Status != contracts.JobStatusBlocked || s3.LastErrorCode != "EXECUTION_GATE_BLOCKED" {
		t.Fatalf("job-gated = %s (%s), want BLOCKED (EXECUTION_GATE_BLOCKED)", s3.Status, s3.LastErrorCode)
	}
}

func TestWorkerTargetLockBusySchedulesRetryWithoutExecutingSQL(t *testing.T) {
	job := makeTestJob("job-busy", "db-busy", "")
	queue := newMemoryJobQueue(job)
	lockServer := &fakeLockServer{}
	provider := &fakeConnProvider{
		server:    lockServer,
		revisions: map[contracts.DatabaseID]int64{"db-busy": 1},
		dbNames:   map[contracts.DatabaseID]string{"db-busy": "phys_busy"},
	}

	ctx := context.Background()
	// Hold the session advisory lock on phys_busy from an external session.
	extConn, _, err := provider.AcquireExecutorConn(ctx, "db-busy")
	if err != nil {
		t.Fatal(err)
	}
	extLock, err := AcquireTargetSessionLock(ctx, extConn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = extLock.Release(ctx) }()

	var executed atomic.Int32
	worker, err := NewWorker(WorkerConfig{
		WorkerID:     "w-busy",
		Concurrency:  1,
		Queue:        queue,
		ConnProvider: provider,
		Executor: PlanExecutorFunc(func(context.Context, ExecutionEnv) (ExecutionResult, error) {
			executed.Add(1)
			return ExecutionResult{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	processed, err := worker.ProcessOnce(ctx, "w-busy")
	if err != nil || !processed {
		t.Fatalf("ProcessOnce: processed=%v err=%v", processed, err)
	}
	if executed.Load() != 0 {
		t.Fatalf("executed = %d, want 0 while target lock is busy", executed.Load())
	}
	got := queue.getJob("job-busy")
	if got.Status != contracts.JobStatusRetryWait || got.LastErrorCode != "TARGET_LOCK_BUSY" {
		t.Fatalf("job-busy status=%s code=%s, want RETRY_WAIT and TARGET_LOCK_BUSY", got.Status, got.LastErrorCode)
	}
}
