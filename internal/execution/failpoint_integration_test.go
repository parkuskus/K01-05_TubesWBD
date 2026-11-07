package execution

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"if3110.local/db-platform/internal/contracts"
)

// TestFailpointAfterBatchDataUpdateBeforeCheckpointRollsBackBatchAtomically
// verifies that if a failure occurs AFTER the batch UPDATE has modified target
// rows inside the batch transaction (specifically while writing batch_checkpoints),
// the entire batch transaction rolls back atomically so no rows from the failed
// batch remain modified while earlier committed batches stay intact (PARTIAL).
func TestFailpointAfterBatchDataUpdateBeforeCheckpointRollsBackBatchAtomically(t *testing.T) {
	engine := newFakeBatchEngineDB(map[int64]string{
		10: "PENDING",
		20: "PENDING",
		30: "PENDING",
		40: "PENDING",
	})
	// Fail on batch 2 specifically during UPDATE batch_checkpoints (after rows 30, 40 were updated in tx).
	engine.failOnCheckpointBatchNo = 2

	sqlDB := sql.OpenDB(fakeBatchEngineConnector{db: engine})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	gates := newMemoryGateStore()
	batchExec := NewBatchExecutor(gates, nil)

	plan := sampleBatchPlan(contracts.DirectionApply)
	res, err := batchExec.Execute(ctx, buildExecutionEnv(conn, plan, "job-fp-ckpt", "att-fp-ckpt"))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.JobStatus != contracts.JobStatusPartial {
		t.Fatalf("JobStatus = %s, want PARTIAL", res.JobStatus)
	}
	if res.CommittedBatches != 1 || res.CommittedRows != 2 || res.LastCommittedCursor != "20" {
		t.Fatalf("result counters = %+v, want CommittedBatches=1 CommittedRows=2 LastCommittedCursor=20", res)
	}

	// Batch 1 (rows 10, 20) must remain committed ("MIGRATED").
	for _, id := range []int64{10, 20} {
		if engine.tableRows[id].Status != "MIGRATED" {
			t.Fatalf("batch 1 row %d status = %q, want MIGRATED", id, engine.tableRows[id].Status)
		}
	}
	// Batch 2 (rows 30, 40) had its data updated before the checkpoint failure,
	// so the transaction rollback MUST have restored them to "PENDING"!
	for _, id := range []int64{30, 40} {
		if engine.tableRows[id].Status != "PENDING" {
			t.Fatalf("batch 2 row %d status = %q, want PENDING (must roll back together with failed checkpoint)",
				id, engine.tableRows[id].Status)
		}
	}
}

// TestFailpointWorkerCrashAfterTargetCommitBeforeQueueUpdateReconcilesWithoutReexecution
// simulates a worker crashing immediately after tx.Commit() succeeds on the target
// database (leaving a durable commit proof in if3110_platform.execution_commits)
// before the platform jobs table could be updated. When the replacement worker
// claims the job and runs RecoveryExecutor, it reconciles the job to SUCCESS
// without re-executing the migration SQL.
func TestFailpointWorkerCrashAfterTargetCommitBeforeQueueUpdateReconcilesWithoutReexecution(t *testing.T) {
	targetDB := newFakeOrdinaryTargetDB()
	sqlDB := sql.OpenDB(fakeOrdinaryConnector{db: targetDB})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn1, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}

	plan := contracts.ExecutionPlan{
		RequestID:                "req-crash-after-commit",
		ExecutionID:              "exec-crash-after-commit",
		MigrationID:              "mig-crash-1",
		RevisionID:               "rev-crash-1",
		Version:                  1,
		Direction:                contracts.DirectionApply,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-1",
		TargetDatabaseID:         "db-1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "",
		PolicyVersion:            1,
		UpSQL:                    "INSERT INTO ledger_entries (code) VALUES ('ONCE_ONLY');",
		DownSQL:                  "DELETE FROM ledger_entries WHERE code = 'ONCE_ONLY';",
	}
	plan.PlanChecksum = plan.ComputeChecksum()

	// 1. Worker 1 executes and commits on the target DB, then crashes before updating the platform queue.
	ordExec := NewOrdinaryExecutor()
	res1, err := ordExec.Execute(ctx, buildExecutionEnv(conn1, plan, "job-crash-1", "att-crash-1"))
	if err != nil || res1.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("Worker 1 target commit failed: res=%+v err=%v", res1, err)
	}
	_ = conn1.Close() // Worker 1 process dies, releasing its PostgreSQL session.

	if len(targetDB.executedUserSQL) != 1 {
		t.Fatalf("len(executedUserSQL) after Worker 1 commit = %d, want 1", len(targetDB.executedUserSQL))
	}

	// 2. Worker 2 claims the job on a new connection and runs through RecoveryExecutor (NewDispatchExecutor).
	conn2, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn2.Close() }()

	gates := newMemoryGateStore()
	dispatchExec := NewDispatchExecutor(gates, nil)
	res2, err := dispatchExec.Execute(ctx, buildExecutionEnv(conn2, plan, "job-crash-1", "att-crash-2"))
	if err != nil {
		t.Fatalf("Worker 2 RecoveryExecutor error = %v", err)
	}
	if res2.JobStatus != contracts.JobStatusSuccess || res2.AttemptStatus != contracts.AttemptStatusCommitted {
		t.Fatalf("Worker 2 reconciliation result = %+v, want SUCCESS/COMMITTED", res2)
	}

	// Verify the user SQL was NOT executed a second time!
	if len(targetDB.executedUserSQL) != 1 {
		t.Fatalf("len(executedUserSQL) after Worker 2 recovery = %d, want 1 (must not re-execute committed SQL)",
			len(targetDB.executedUserSQL))
	}
}

// TestFailpointMidReverseCompensationAndResumeToCompletion verifies that a
// failure halfway through reverse batch compensation sets PARTIALLY_REVERTED
// and a subsequent compensation resumes from the remaining batch to completion.
func TestFailpointMidReverseCompensationAndResumeToCompletion(t *testing.T) {
	engine := newFakeBatchEngineDB(map[int64]string{
		10: "PENDING",
		20: "PENDING",
		30: "PENDING",
		40: "PENDING",
		50: "PENDING",
		60: "PENDING",
	})
	// Fail on forward batch 4 (doesn't exist; let's fail on batch 3 so batches 1 and 2 commit).
	engine.failOnBatchNo = 3
	engine.failBatchErr = errors.New("forward batch 3 failpoint")

	sqlDB := sql.OpenDB(fakeBatchEngineConnector{db: engine})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	gates := newMemoryGateStore()
	dispatch := NewDispatchExecutor(gates, nil)

	applyPlan := sampleBatchPlan(contracts.DirectionApply)
	resUp, err := dispatch.Execute(ctx, buildExecutionEnv(conn, applyPlan, "job-up", "att-up"))
	if err != nil || resUp.JobStatus != contracts.JobStatusPartial {
		t.Fatalf("forward apply expected PARTIAL, got res=%+v err=%v", resUp, err)
	}

	// Fail reverse compensation on batch 1 (after batch 2 is reverted) -> PARTIALLY_REVERTED.
	engine.failOnBatchNo = 1
	engine.failBatchErr = errors.New("mid-compensation failpoint on batch 1")

	compPlan := sampleBatchPlan(contracts.DirectionCompensate)
	resComp1, err := dispatch.Execute(ctx, buildExecutionEnv(conn, compPlan, "job-comp-1", "att-comp-1"))
	if err != nil || resComp1.JobStatus != contracts.JobStatusPartiallyReverted {
		t.Fatalf("expected PARTIALLY_REVERTED on mid-compensation failure, got res=%+v err=%v", resComp1, err)
	}

	// Resume compensation -> finishes reverting batch 1 without repeating batch 2.
	resComp2, err := dispatch.Execute(ctx, buildExecutionEnv(conn, compPlan, "job-comp-2", "att-comp-2"))
	if err != nil || resComp2.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("expected SUCCESS on resumed compensation, got res=%+v err=%v", resComp2, err)
	}

	if !reflect.DeepEqual(engine.compensatedBatchOrder, []int{2, 1}) {
		t.Fatalf("compensatedBatchOrder = %v, want [2 1]", engine.compensatedBatchOrder)
	}
}
