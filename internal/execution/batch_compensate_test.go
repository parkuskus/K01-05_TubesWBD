package execution

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/contracts"
)

func TestBuildBatchCompensateSQLRejectsMismatchedUpdateTarget(t *testing.T) {
	cfg := contracts.BatchConfig{TableSchema: "public", TableName: "orders", PrimaryKeyColumn: "id", BatchSize: 10}
	if _, err := BuildBatchCompensateSQL("UPDATE public.payments SET status = 'PENDING'", cfg); !errors.Is(err, ErrInvalidBatchUpdateSQL) {
		t.Fatalf("mismatched compensation target error = %v, want ErrInvalidBatchUpdateSQL", err)
	}
}

func TestBatchCompensateRevertsCommittedBatchesInReverseOrder(t *testing.T) {
	// BATCH-04: Batch 1..2 committed before batch 3 failed; compensation reverts
	// batch 2 then batch 1 in reverse order, clears PARTIAL gate, and keeps
	// active_head at its pre-batch position.
	engine := newFakeBatchEngineDB(map[int64]string{
		10: "PENDING",
		20: "PENDING",
		30: "PENDING",
		40: "PENDING",
		50: "PENDING",
	})
	engine.head = contracts.ActiveHeadState{
		MigrationID: "mig-prev-0",
		ExecutionID: "exec-prev-0",
		Version:     0,
	}
	// Fail on batch 3 so batches 1 (10,20) and 2 (30,40) commit first.
	engine.failOnBatchNo = 3
	engine.failBatchErr = errors.New("simulated batch 3 failure")

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

	applyPlan := sampleBatchPlan(contracts.DirectionApply)
	applyPlan.ExpectedActiveHead = "exec-prev-0"
	applyPlan.PlanChecksum = applyPlan.ComputeChecksum()

	envApply := buildExecutionEnv(conn, applyPlan, "job-batch-up", "att-batch-up")
	resApply, err := batchExec.Execute(ctx, envApply)
	if err != nil {
		t.Fatalf("Execute(APPLY) error = %v", err)
	}
	if resApply.JobStatus != contracts.JobStatusPartial {
		t.Fatalf("Execute(APPLY) status = %s, want PARTIAL", resApply.JobStatus)
	}
	if engine.tableRows[10].Status != "MIGRATED" || engine.tableRows[40].Status != "MIGRATED" {
		t.Fatalf("expected rows 10..40 to be MIGRATED after batch 1..2 committed")
	}

	// Execute COMPENSATE plan referencing origin_execution_id = "exec-batch-1".
	compPlan := sampleBatchPlan(contracts.DirectionCompensate)
	compPlan.ExpectedActiveHead = "exec-prev-0"
	compPlan.PlanChecksum = compPlan.ComputeChecksum()

	envComp := buildExecutionEnv(conn, compPlan, "job-batch-comp", "att-batch-comp")
	resComp, err := batchExec.Execute(ctx, envComp)
	if err != nil {
		t.Fatalf("Execute(COMPENSATE) error = %v", err)
	}
	if resComp.JobStatus != contracts.JobStatusSuccess || resComp.AttemptStatus != contracts.AttemptStatusCommitted {
		t.Fatalf("Execute(COMPENSATE) result = %+v, want SUCCESS/COMMITTED", resComp)
	}

	// Assert reverse order: batch 2 must be compensated before batch 1!
	wantOrder := []int{2, 1}
	if !reflect.DeepEqual(engine.compensatedBatchOrder, wantOrder) {
		t.Fatalf("compensatedBatchOrder = %v, want %v (reverse batch order N-1..1)",
			engine.compensatedBatchOrder, wantOrder)
	}

	// Verify all rows 10..50 are back to their pre-batch state ("PENDING").
	for _, id := range []int64{10, 20, 30, 40, 50} {
		if engine.tableRows[id].Status != "PENDING" {
			t.Fatalf("row %d status after compensation = %q, want PENDING", id, engine.tableRows[id].Status)
		}
	}

	// Verify PARTIAL gate is cleared in both control-plane and target DB,
	// and active_head remains at its pre-batch position ("exec-prev-0").
	if _, gated := gates.gates["db-1"]; gated {
		t.Fatalf("expected control-plane gate to be cleared after compensation")
	}
	if engine.head.UnresolvedReason != "" || engine.head.UnresolvedExecutionID != "" {
		t.Fatalf("expected target DB gate to be cleared, got %+v", engine.head)
	}
	if engine.head.ExecutionID != "exec-prev-0" {
		t.Fatalf("active_head after compensation = %q, want exec-prev-0 (pre-batch head)", engine.head.ExecutionID)
	}
}

func TestBatchCompensatePartiallyRevertedAndResumeCompensation(t *testing.T) {
	// BATCH-04: If compensation fails after reverting batch 2 (on batch 1),
	// status and gate transition to PARTIALLY_REVERTED, and a subsequent
	// COMPENSATE resumes from batch 1 without repeating batch 2.
	engine := newFakeBatchEngineDB(map[int64]string{
		10: "PENDING",
		20: "PENDING",
		30: "PENDING",
		40: "PENDING",
		50: "PENDING",
	})
	engine.failOnBatchNo = 3
	engine.failBatchErr = errors.New("simulated batch 3 failure")

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

	applyPlan := sampleBatchPlan(contracts.DirectionApply)
	envApply := buildExecutionEnv(conn, applyPlan, "job-batch-up", "att-batch-up")
	_, _ = batchExec.Execute(ctx, envApply)

	// Inject failure when compensating batch 1 (after batch 2 succeeds).
	engine.failOnBatchNo = 1
	engine.failBatchErr = errors.New("simulated down batch 1 failure")

	compPlan := sampleBatchPlan(contracts.DirectionCompensate)
	envComp1 := buildExecutionEnv(conn, compPlan, "job-batch-comp-1", "att-batch-comp-1")
	resComp1, err := batchExec.Execute(ctx, envComp1)
	if err != nil {
		t.Fatalf("first Execute(COMPENSATE) error = %v", err)
	}
	if resComp1.JobStatus != contracts.JobStatusPartiallyReverted {
		t.Fatalf("first Execute(COMPENSATE) status = %s, want PARTIALLY_REVERTED", resComp1.JobStatus)
	}
	if engine.head.UnresolvedReason != contracts.GateReasonPartiallyReverted {
		t.Fatalf("target gate reason = %s, want PARTIALLY_REVERTED", engine.head.UnresolvedReason)
	}
	// Batch 2 (rows 30, 40) was reverted to PENDING; Batch 1 (rows 10, 20) is still MIGRATED.
	if engine.tableRows[30].Status != "PENDING" || engine.tableRows[40].Status != "PENDING" {
		t.Fatalf("expected batch 2 rows (30,40) to be reverted to PENDING")
	}
	if engine.tableRows[10].Status != "MIGRATED" || engine.tableRows[20].Status != "MIGRATED" {
		t.Fatalf("expected batch 1 rows (10,20) to still be MIGRATED")
	}

	// Resume compensation: should only execute batch 1 (not repeat batch 2).
	envComp2 := buildExecutionEnv(conn, compPlan, "job-batch-comp-2", "att-batch-comp-2")
	resComp2, err := batchExec.Execute(ctx, envComp2)
	if err != nil {
		t.Fatalf("second Execute(COMPENSATE) error = %v", err)
	}
	if resComp2.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("second Execute(COMPENSATE) status = %s, want SUCCESS", resComp2.JobStatus)
	}
	wantOrder := []int{2, 1}
	if !reflect.DeepEqual(engine.compensatedBatchOrder, wantOrder) {
		t.Fatalf("compensatedBatchOrder across halt & resume = %v, want %v (batch 2 not repeated)",
			engine.compensatedBatchOrder, wantOrder)
	}
	for _, id := range []int64{10, 20, 30, 40, 50} {
		if engine.tableRows[id].Status != "PENDING" {
			t.Fatalf("row %d status = %q, want PENDING", id, engine.tableRows[id].Status)
		}
	}
}

func TestBatchCompensateAbortsOnExternalPostImageModificationOrIrreversible(t *testing.T) {
	// BATCH-05: If an external write modified or deleted a post-migration row
	// after the batch committed (current_data != after_data), compensation must
	// abort with UNSAFE_BATCH_COMPENSATION and must NOT overwrite newer data.
	engine := newFakeBatchEngineDB(map[int64]string{
		10: "PENDING",
		20: "PENDING",
		30: "PENDING",
		40: "PENDING",
		50: "PENDING",
	})
	engine.failOnBatchNo = 3
	engine.failBatchErr = errors.New("simulated batch 3 failure")

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

	applyPlan := sampleBatchPlan(contracts.DirectionApply)
	envApply := buildExecutionEnv(conn, applyPlan, "job-batch-up", "att-batch-up")
	_, _ = batchExec.Execute(ctx, envApply)

	// 1. Test irreversible migration rejection first.
	irrevPlan := sampleBatchPlan(contracts.DirectionCompensate)
	irrevPlan.DownSQL = ""
	irrevPlan.IsIrreversible = true
	irrevPlan.IrreversibleReason = "destructive batch update without down script"
	irrevPlan.PlanChecksum = irrevPlan.ComputeChecksum()
	envIrrev := buildExecutionEnv(conn, irrevPlan, "job-batch-irrev", "att-batch-irrev")
	resIrrev, err := batchExec.Execute(ctx, envIrrev)
	if err != nil {
		t.Fatalf("Execute(irreversible COMPENSATE) error = %v", err)
	}
	if resIrrev.JobStatus != contracts.JobStatusFailed || resIrrev.ErrorCode != "IRREVERSIBLE_MIGRATION" {
		t.Fatalf("irreversible compensation result = %+v, want FAILED / IRREVERSIBLE_MIGRATION", resIrrev)
	}

	// 2. Simulate an external write modifying row 30 (in committed batch 2) after batch 2 committed.
	engine.tableRows[30] = fakeBatchRowData{ID: 30, Status: "SHIPPED_BY_EXTERNAL_APP"}

	compPlan := sampleBatchPlan(contracts.DirectionCompensate)
	envComp := buildExecutionEnv(conn, compPlan, "job-batch-unsafe", "att-batch-unsafe")
	resComp, err := batchExec.Execute(ctx, envComp)
	if err != nil {
		t.Fatalf("Execute(unsafe COMPENSATE) error = %v", err)
	}
	if resComp.JobStatus != contracts.JobStatusFailed || resComp.ErrorCode != "UNSAFE_BATCH_COMPENSATION" {
		t.Fatalf("unsafe compensation result = %+v, want FAILED / UNSAFE_BATCH_COMPENSATION", resComp)
	}
	if !strings.Contains(resComp.ErrorMessage, "unsafe batch compensation") {
		t.Fatalf("expected unsafe batch compensation error message, got %q", resComp.ErrorMessage)
	}

	// Verify the externally modified row 30 was NOT overwritten!
	if engine.tableRows[30].Status != "SHIPPED_BY_EXTERNAL_APP" {
		t.Fatalf("row 30 status = %q, want SHIPPED_BY_EXTERNAL_APP (must not overwrite external write)",
			engine.tableRows[30].Status)
	}
	if len(engine.compensatedBatchOrder) != 0 {
		t.Fatalf("expected 0 batches to be compensated on unsafe post-image mismatch, got %v",
			engine.compensatedBatchOrder)
	}
}
