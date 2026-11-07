package execution

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

// TestMultiWorkerSameDatabaseSerializesAndDifferentDatabasesRunInParallel (LOCK-01)
// launches 2 Worker instances concurrently and verifies:
//  1. Two jobs targeting the SAME database (db-A) never overlap in execution.
//  2. Two jobs targeting DIFFERENT databases (db-A and db-B) execute in parallel.
func TestMultiWorkerSameDatabaseSerializesAndDifferentDatabasesRunInParallel(t *testing.T) {
	ctx := context.Background()

	// Part 1: Two jobs on the SAME database (db-A) across 2 workers must serialize (maxInFlightOnA == 1).
	jobA1 := makeTestJob("job-a1", "db-A", "")
	jobA2 := makeTestJob("job-a2", "db-A", "")
	queueSerial := newMemoryJobQueue(jobA1, jobA2)
	lockServerSerial := &fakeLockServer{}
	providerSerial := &fakeConnProvider{
		server: lockServerSerial,
		revisions: map[contracts.DatabaseID]int64{
			"db-A": 1,
		},
		dbNames: map[contracts.DatabaseID]string{
			"db-A": "phys_db_a",
		},
	}

	var (
		inFlightOnA    atomic.Int32
		maxInFlightOnA atomic.Int32
		completedOnA   atomic.Int32
	)

	serialExec := PlanExecutorFunc(func(_ context.Context, env ExecutionEnv) (ExecutionResult, error) {
		cur := inFlightOnA.Add(1)
		for {
			prev := maxInFlightOnA.Load()
			if cur <= prev || maxInFlightOnA.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		inFlightOnA.Add(-1)
		completedOnA.Add(1)
		return ExecutionResult{
			JobStatus:     contracts.JobStatusSuccess,
			AttemptStatus: contracts.AttemptStatusCommitted,
			ExecutedSQL:   env.Claimed.Job.Plan.UpSQL,
		}, nil
	})

	worker1, err := NewWorker(WorkerConfig{
		WorkerID:          "worker-1",
		Concurrency:       1,
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     200 * time.Millisecond,
		HeartbeatInterval: 40 * time.Millisecond,
		Queue:             queueSerial,
		ConnProvider:      providerSerial,
		Executor:          serialExec,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker2, err := NewWorker(WorkerConfig{
		WorkerID:          "worker-2",
		Concurrency:       1,
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     200 * time.Millisecond,
		HeartbeatInterval: 40 * time.Millisecond,
		Queue:             queueSerial,
		ConnProvider:      providerSerial,
		Executor:          serialExec,
	})
	if err != nil {
		t.Fatal(err)
	}

	runCtx1, cancel1 := context.WithTimeout(ctx, 2*time.Second)
	defer cancel1()
	var wgSerial sync.WaitGroup
	wgSerial.Add(2)
	go func() {
		defer wgSerial.Done()
		_ = worker1.Run(runCtx1)
	}()
	go func() {
		defer wgSerial.Done()
		_ = worker2.Run(runCtx1)
	}()

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if queueSerial.getJob("job-a1").Status == contracts.JobStatusSuccess &&
			queueSerial.getJob("job-a2").Status == contracts.JobStatusSuccess {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel1()
	wgSerial.Wait()

	if completedOnA.Load() != 2 {
		t.Fatalf("completedOnA = %d, want 2", completedOnA.Load())
	}
	if maxInFlightOnA.Load() != 1 {
		t.Fatalf("maxInFlightOnA = %d, want 1 (same database jobs must never overlap across workers)", maxInFlightOnA.Load())
	}

	// Part 2: Two jobs on DIFFERENT databases (db-A and db-B) across 2 workers must overlap in parallel!
	jobParA := makeTestJob("job-par-a", "db-A", "")
	jobParB := makeTestJob("job-par-b", "db-B", "")
	queuePar := newMemoryJobQueue(jobParA, jobParB)
	lockServerPar := &fakeLockServer{}
	providerPar := &fakeConnProvider{
		server: lockServerPar,
		revisions: map[contracts.DatabaseID]int64{
			"db-A": 1,
			"db-B": 1,
		},
		dbNames: map[contracts.DatabaseID]string{
			"db-A": "phys_db_a",
			"db-B": "phys_db_b",
		},
	}

	var (
		inFlightPar    atomic.Int32
		maxInFlightPar atomic.Int32
		bothEntered    = make(chan struct{})
		bothOnce       sync.Once
	)

	parExec := PlanExecutorFunc(func(_ context.Context, env ExecutionEnv) (ExecutionResult, error) {
		cur := inFlightPar.Add(1)
		for {
			prev := maxInFlightPar.Load()
			if cur <= prev || maxInFlightPar.CompareAndSwap(prev, cur) {
				break
			}
		}
		if cur >= 2 {
			bothOnce.Do(func() { close(bothEntered) })
		}
		select {
		case <-bothEntered:
		case <-time.After(500 * time.Millisecond):
		}
		inFlightPar.Add(-1)
		return ExecutionResult{
			JobStatus:     contracts.JobStatusSuccess,
			AttemptStatus: contracts.AttemptStatusCommitted,
			ExecutedSQL:   env.Claimed.Job.Plan.UpSQL,
		}, nil
	})

	wPar1, err := NewWorker(WorkerConfig{
		WorkerID:     "worker-par-1",
		Concurrency:  1,
		PollInterval: 5 * time.Millisecond,
		Queue:        queuePar,
		ConnProvider: providerPar,
		Executor:     parExec,
	})
	if err != nil {
		t.Fatal(err)
	}
	wPar2, err := NewWorker(WorkerConfig{
		WorkerID:     "worker-par-2",
		Concurrency:  1,
		PollInterval: 5 * time.Millisecond,
		Queue:        queuePar,
		ConnProvider: providerPar,
		Executor:     parExec,
	})
	if err != nil {
		t.Fatal(err)
	}

	var wgPar sync.WaitGroup
	wgPar.Add(2)
	go func() {
		defer wgPar.Done()
		_, _ = wPar1.ProcessOnce(ctx, "worker-par-1")
	}()
	go func() {
		defer wgPar.Done()
		_, _ = wPar2.ProcessOnce(ctx, "worker-par-2")
	}()
	wgPar.Wait()

	if maxInFlightPar.Load() < 2 {
		t.Fatalf("maxInFlightPar = %d, want 2 (Target A and Target B must execute concurrently across workers)", maxInFlightPar.Load())
	}
}

// TestMultiWorkerExpiredLeaseCannotDoubleExecuteWhileOldSessionHoldsLockAndAliasesShareLock (LOCK-02)
// verifies:
//  1. If Worker 1's queue lease expires while its PostgreSQL session still holds
//     the session advisory lock, Worker 2 claiming the job is blocked by the
//     target advisory lock (schedules RETRY_WAIT) and NEVER executes SQL twice.
//  2. Two different database connection IDs (db-alias-1 and db-alias-2) pointing
//     to the same physical database (phys_shared) contend on the same advisory lock.
func TestMultiWorkerExpiredLeaseCannotDoubleExecuteWhileOldSessionHoldsLockAndAliasesShareLock(t *testing.T) {
	ctx := context.Background()
	lockServer := &fakeLockServer{}
	provider := &fakeConnProvider{
		server: lockServer,
		revisions: map[contracts.DatabaseID]int64{
			"db-alias-1": 1,
			"db-alias-2": 1,
		},
		dbNames: map[contracts.DatabaseID]string{
			"db-alias-1": "phys_shared",
			"db-alias-2": "phys_shared",
		},
	}

	// Simulate Worker 1 still holding the PostgreSQL session advisory lock on db-alias-1 ("phys_shared"),
	// even though its queue lease has expired.
	zombieConn, _, err := provider.AcquireExecutorConn(ctx, "db-alias-1")
	if err != nil {
		t.Fatal(err)
	}
	zombieLock, err := AcquireTargetSessionLock(ctx, zombieConn)
	if err != nil {
		t.Fatal(err)
	}

	expiredPast := time.Now().UTC().Add(-10 * time.Second)
	staleRunningJob := makeTestJob("job-zombie", "db-alias-1", "")
	staleRunningJob.Status = contracts.JobStatusRetryWait
	staleRunningJob.LeaseOwner = "worker-zombie-1"
	staleRunningJob.LeaseToken = "token-expired-1"
	staleRunningJob.LeaseExpiresAt = &expiredPast

	// Also prepare a second job on db-alias-2 (different connection ID, same physical DB "phys_shared").
	aliasJob := makeTestJob("job-alias-2", "db-alias-2", "")

	queueZombie := newMemoryJobQueue(staleRunningJob)
	queueAlias := newMemoryJobQueue(aliasJob)

	var sqlExecutions atomic.Int32
	execFn := PlanExecutorFunc(func(_ context.Context, env ExecutionEnv) (ExecutionResult, error) {
		sqlExecutions.Add(1)
		return ExecutionResult{
			JobStatus:     contracts.JobStatusSuccess,
			AttemptStatus: contracts.AttemptStatusCommitted,
			ExecutedSQL:   env.Claimed.Job.Plan.UpSQL,
		}, nil
	})

	workerZombie, err := NewWorker(WorkerConfig{
		WorkerID:     "worker-replacement-2",
		Concurrency:  1,
		Queue:        queueZombie,
		ConnProvider: provider,
		Executor:     execFn,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerAlias, err := NewWorker(WorkerConfig{
		WorkerID:     "worker-alias-2",
		Concurrency:  1,
		Queue:        queueAlias,
		ConnProvider: provider,
		Executor:     execFn,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Both job-zombie (on db-alias-1) and job-alias-2 (on db-alias-2) must be rejected by the
	// session advisory lock held on "phys_shared" without executing SQL!
	if processed, err := workerZombie.ProcessOnce(ctx, "worker-replacement-2"); err != nil || !processed {
		t.Fatalf("workerZombie.ProcessOnce while zombie lock held: processed=%v err=%v", processed, err)
	}
	if processed, err := workerAlias.ProcessOnce(ctx, "worker-alias-2"); err != nil || !processed {
		t.Fatalf("workerAlias.ProcessOnce while zombie lock held: processed=%v err=%v", processed, err)
	}
	if sqlExecutions.Load() != 0 {
		t.Fatalf("sqlExecutions while old session held lock = %d, want 0 (double execution prevented)", sqlExecutions.Load())
	}
	if queueZombie.getJob("job-zombie").LastErrorCode != "TARGET_LOCK_BUSY" {
		t.Fatalf("job-zombie error code = %q, want TARGET_LOCK_BUSY", queueZombie.getJob("job-zombie").LastErrorCode)
	}
	if queueAlias.getJob("job-alias-2").LastErrorCode != "TARGET_LOCK_BUSY" {
		t.Fatalf("job-alias-2 error code = %q, want TARGET_LOCK_BUSY (alias pointing to same physical DB must contend)",
			queueAlias.getJob("job-alias-2").LastErrorCode)
	}

	// Now release the old zombie session lock and verify replacement workers succeed cleanly!
	if err := zombieLock.Release(ctx); err != nil {
		t.Fatalf("zombieLock.Release() error = %v", err)
	}
	if processed, err := workerZombie.ProcessOnce(ctx, "worker-replacement-2"); err != nil || !processed {
		t.Fatalf("workerZombie.ProcessOnce after lock release: processed=%v err=%v", processed, err)
	}
	if processed, err := workerAlias.ProcessOnce(ctx, "worker-alias-2"); err != nil || !processed {
		t.Fatalf("workerAlias.ProcessOnce after lock release: processed=%v err=%v", processed, err)
	}
	if sqlExecutions.Load() != 2 {
		t.Fatalf("sqlExecutions after lock release = %d, want 2", sqlExecutions.Load())
	}
}
