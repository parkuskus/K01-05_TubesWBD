package execution

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

// TestBatchPartialFailureKeepsCommittedBatchesAndSetsPartialGate verifies BATCH-01:
// When batch 2 fails:
//  1. Batch 1 remains committed in the target database (rows 10 and 20 stay MIGRATED).
//  2. Batch 2 rolls back cleanly (rows 30 and 40 stay PENDING).
//  3. The job transitions to PARTIAL with CommittedBatches=1, CommittedRows=2, LastCommittedCursor="20".
//  4. Both platform and target execution gates are set to PARTIAL, and active_head is not advanced.
func TestBatchPartialFailureKeepsCommittedBatchesAndSetsPartialGate(t *testing.T) {
	engine := newFakeBatchEngineDB(map[int64]string{
		10: "PENDING",
		20: "PENDING",
		30: "PENDING",
		40: "PENDING",
		50: "PENDING",
	})
	engine.failOnBatchNo = 2
	engine.failBatchErr = &pgconn.PgError{
		Code:    "23514",
		Message: "new row for relation \"orders\" violates check constraint",
	}

	sqlDB := sql.OpenDB(fakeBatchEngineConnector{db: engine})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	gates := newMemoryGateStore()
	progress := &recordingProgressUpdater{}
	batchExec := NewBatchExecutor(gates, progress)

	plan := sampleBatchPlan(contracts.DirectionApply)
	env := buildExecutionEnv(conn, plan, "job-batch-partial", "att-batch-partial-1")
	env.Claimed.Job.LeaseToken = "lease-partial-1"

	res, err := batchExec.Execute(ctx, env)
	if err != nil {
		t.Fatalf("Execute() unexpected Go error = %v", err)
	}
	if res.JobStatus != contracts.JobStatusPartial || res.AttemptStatus != contracts.AttemptStatusFailed {
		t.Fatalf("result = %+v, want PARTIAL/FAILED", res)
	}
	if res.CommittedBatches != 1 || res.CommittedRows != 2 || res.TotalScopeRows != 5 || res.LastCommittedCursor != "20" {
		t.Fatalf("unexpected PARTIAL progress counters: %+v", res)
	}
	if res.ErrorCode != "23514" {
		t.Fatalf("ErrorCode = %q, want 23514", res.ErrorCode)
	}

	// Verify Batch 1 rows [10, 20] remained committed while Batch 2 rows [30, 40] rolled back!
	if engine.tableRows[10].Status != "MIGRATED" || engine.tableRows[20].Status != "MIGRATED" {
		t.Fatalf("Batch 1 rows were not preserved: 10=%q 20=%q", engine.tableRows[10].Status, engine.tableRows[20].Status)
	}
	for _, id := range []int64{30, 40, 50} {
		if engine.tableRows[id].Status != "PENDING" {
			t.Fatalf("uncommitted row %d status = %q, want PENDING", id, engine.tableRows[id].Status)
		}
	}

	// Verify PARTIAL gate is set on both platform and target DB, and active_head has NOT advanced.
	gateRec, hasGate, _ := gates.GetGate(ctx, "db-1")
	if !hasGate || gateRec.Reason != contracts.GateReasonPartial || gateRec.ExecutionID != "exec-batch-1" {
		t.Fatalf("expected PARTIAL gate on platform DB, got hasGate=%v gate=%+v", hasGate, gateRec)
	}
	if engine.head.UnresolvedReason != contracts.GateReasonPartial || engine.head.UnresolvedExecutionID != "exec-batch-1" {
		t.Fatalf("expected target PARTIAL gate, got head=%+v", engine.head)
	}
	if engine.head.ExecutionID != "" || len(engine.history) != 0 {
		t.Fatalf("active_head or migration_history_v2 must not advance on PARTIAL failure: head=%+v hist=%d",
			engine.head, len(engine.history))
	}
}

// TestBatchSafePauseAtBoundaryHaltsCleanlyBetweenBatches verifies BATCH-02:
// When a pause is requested via BatchControlRegistry before batch 3 begins, batches 1 and 2
// remain committed and execution halts cleanly at the boundary with PARTIAL status.
func TestBatchSafePauseAtBoundaryHaltsCleanlyBetweenBatches(t *testing.T) {
	engine := newFakeBatchEngineDB(map[int64]string{
		10: "PENDING",
		20: "PENDING",
		30: "PENDING",
		40: "PENDING",
		50: "PENDING",
	})
	sqlDB := sql.OpenDB(fakeBatchEngineConnector{db: engine})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	gates := newMemoryGateStore()
	controls := NewBatchControlRegistry()
	batchExec := NewBatchExecutor(gates, nil).WithPauseChecker(func(ctx context.Context, jobID contracts.JobID, nextBatchNo int) bool {
		if nextBatchNo == 3 {
			controls.RequestPause(jobID)
		}
		return controls.ShouldPause(ctx, jobID, nextBatchNo)
	})

	plan := sampleBatchPlan(contracts.DirectionApply)
	env := buildExecutionEnv(conn, plan, "job-batch-pause", "att-batch-pause-1")

	res, err := batchExec.Execute(ctx, env)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.JobStatus != contracts.JobStatusPartial || res.ErrorCode != "BATCH_PAUSED_AT_BOUNDARY" {
		t.Fatalf("expected PARTIAL with BATCH_PAUSED_AT_BOUNDARY, got %+v", res)
	}
	if res.CommittedBatches != 2 || res.CommittedRows != 4 || res.LastCommittedCursor != "40" {
		t.Fatalf("unexpected paused checkpoint: %+v", res)
	}
	if engine.tableRows[40].Status != "MIGRATED" || engine.tableRows[50].Status != "PENDING" {
		t.Fatalf("expected row 40 MIGRATED and row 50 PENDING at batch 2 boundary, got 40=%q 50=%q",
			engine.tableRows[40].Status, engine.tableRows[50].Status)
	}
}

// TestBatchResumeContinuesFromLastCursorWithoutRepeatingCommittedBatches verifies BATCH-03:
// After halting in PARTIAL state at batch 1 (cursor 20), an approved RESUME operation
// continues from batch 2 (row_id > 20) without touching rows 10 and 20 again,
// finalizes migration_history_v2 and active_head, and clears the PARTIAL gate.
func TestBatchResumeContinuesFromLastCursorWithoutRepeatingCommittedBatches(t *testing.T) {
	engine := newFakeBatchEngineDB(map[int64]string{
		10: "PENDING",
		20: "PENDING",
		30: "PENDING",
		40: "PENDING",
		50: "PENDING",
	})
	engine.failOnBatchNo = 2
	engine.failBatchErr = errors.New("transient constraint failure on batch 2")

	sqlDB := sql.OpenDB(fakeBatchEngineConnector{db: engine})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	gates := newMemoryGateStore()
	progress := &recordingProgressUpdater{}
	batchExec := NewBatchExecutor(gates, progress)

	// Step 1: Initial APPLY fails on Batch 2 -> Batch 1 [10, 20] is committed, status = PARTIAL.
	applyPlan := sampleBatchPlan(contracts.DirectionApply)
	applyEnv := buildExecutionEnv(conn, applyPlan, "job-batch-init", "att-init-1")
	applyRes, err := batchExec.Execute(ctx, applyEnv)
	if err != nil || applyRes.JobStatus != contracts.JobStatusPartial {
		t.Fatalf("initial APPLY result = %+v, err = %v; want PARTIAL", applyRes, err)
	}

	// Place a sentinel value on row 10 (in already-committed Batch 1).
	// If RESUME ever re-executes Batch 1, this sentinel would be overwritten to "MIGRATED"!
	row10 := engine.tableRows[10]
	row10.Status = "SENTINEL_BATCH_1_DO_NOT_TOUCH"
	engine.tableRows[10] = row10

	// Step 2: Approved RESUME job targeting OriginExecutionID = "exec-batch-1".
	resumePlan := sampleBatchPlan(contracts.DirectionResume)
	resumeEnv := buildExecutionEnv(conn, resumePlan, "job-batch-resume", "att-resume-1")
	resumeRes, err := batchExec.Execute(ctx, resumeEnv)
	if err != nil {
		t.Fatalf("Execute(RESUME) error = %v", err)
	}
	if resumeRes.JobStatus != contracts.JobStatusSuccess || resumeRes.AttemptStatus != contracts.AttemptStatusCommitted {
		t.Fatalf("Execute(RESUME) result = %+v, want SUCCESS/COMMITTED", resumeRes)
	}
	if resumeRes.CommittedBatches != 3 || resumeRes.CommittedRows != 5 || resumeRes.TotalScopeRows != 5 || resumeRes.LastCommittedCursor != "50" {
		t.Fatalf("unexpected final counters after RESUME: %+v", resumeRes)
	}

	// Prove Batch 1 was NOT re-executed (row 10 still has our sentinel!).
	if engine.tableRows[10].Status != "SENTINEL_BATCH_1_DO_NOT_TOUCH" {
		t.Fatalf("BATCH-03 violated: Batch 1 row 10 was re-executed during RESUME (status=%q)", engine.tableRows[10].Status)
	}
	// Prove Batches 2 and 3 [30, 40, 50] WERE executed and committed!
	for _, id := range []int64{30, 40, 50} {
		if engine.tableRows[id].Status != "MIGRATED" {
			t.Fatalf("row %d status after RESUME = %q, want MIGRATED", id, engine.tableRows[id].Status)
		}
	}

	// Prove PARTIAL gate was cleared on both platform and target DBs, and active_head advanced!
	if _, stillGated, _ := gates.GetGate(ctx, "db-1"); stillGated {
		t.Fatal("expected platform PARTIAL gate to be cleared after RESUME completed")
	}
	if engine.head.UnresolvedReason != "" || engine.head.ExecutionID != "exec-batch-1" {
		t.Fatalf("expected target PARTIAL gate cleared and active_head=exec-batch-1, got %+v", engine.head)
	}
	if len(engine.history) != 1 {
		t.Fatalf("expected 1 finalized event in migration_history_v2, got %d", len(engine.history))
	}
}

