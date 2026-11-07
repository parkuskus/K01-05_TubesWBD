package execution

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/jobs"
)

type fakeCommitHistoryRows struct {
	events []contracts.CommitHistoryEvent
	idx    int
}

func (*fakeCommitHistoryRows) Columns() []string {
	return []string{
		"id",
		"execution_id",
		"job_id",
		"request_id",
		"migration_id",
		"revision_id",
		"version",
		"direction",
		"mode",
		"executed_sql",
		"plan_checksum",
		"actor_id",
		"head_before",
		"head_after",
		"reverts_execution_id",
		"committed_at",
	}
}

func (*fakeCommitHistoryRows) Close() error { return nil }

func (r *fakeCommitHistoryRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.events) {
		return io.EOF
	}
	ev := r.events[r.idx]
	r.idx++
	dest[0] = ev.ID
	dest[1] = string(ev.ExecutionID)
	dest[2] = string(ev.JobID)
	dest[3] = string(ev.RequestID)
	dest[4] = string(ev.MigrationID)
	dest[5] = string(ev.RevisionID)
	dest[6] = ev.Version
	dest[7] = string(ev.Direction)
	dest[8] = string(ev.Mode)
	dest[9] = ev.ExecutedSQL
	dest[10] = ev.PlanChecksum
	dest[11] = string(ev.ActorID)
	dest[12] = string(ev.HeadBefore)
	dest[13] = string(ev.HeadAfter)
	dest[14] = string(ev.RevertsExecutionID)
	dest[15] = ev.CommittedAt
	return nil
}

type fakeTargetDBProvider struct {
	pools map[contracts.DatabaseID]*sql.DB
}

func (p *fakeTargetDBProvider) Pool(_ context.Context, databaseID contracts.DatabaseID) (*sql.DB, error) {
	db, ok := p.pools[databaseID]
	if !ok || db == nil {
		return nil, errors.New("database not found")
	}
	return db, nil
}

func buildReaderTestPlan(
	dir contracts.OperationDirection,
	version int64,
	execID contracts.ExecutionID,
	expectedHead contracts.ExecutionID,
	originExecID contracts.ExecutionID,
) contracts.ExecutionPlan {
	p := contracts.ExecutionPlan{
		RequestID:                contracts.OperationRequestID(fmt.Sprintf("req-%s", execID)),
		ExecutionID:              execID,
		MigrationID:              contracts.MigrationID(fmt.Sprintf("mig-%d", version)),
		RevisionID:               contracts.RevisionID(fmt.Sprintf("rev-%d", version)),
		Version:                  version,
		Direction:                dir,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-1",
		TargetDatabaseID:         "db-1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       expectedHead,
		PolicyVersion:            1,
		UpSQL:                    fmt.Sprintf("CREATE TABLE t_%d (id BIGINT PRIMARY KEY);", version),
		DownSQL:                  fmt.Sprintf("DROP TABLE t_%d;", version),
		OriginExecutionID:        originExecID,
	}
	p.PlanChecksum = p.ComputeChecksum()
	return p
}

func TestTargetEvidenceReaderCommitHistoryActiveHeadAndHideRollback(t *testing.T) {
	// HIST-01, HIST-02, HIST-03:
	// E1: APPLY M1
	// E2: APPLY M2
	// E3: ROLLBACK M2 (reverts E2)
	// E4: FORWARD M2 (re-apply M2)
	targetDB := newFakeOrdinaryTargetDB()
	sqlDB := sql.OpenDB(fakeOrdinaryConnector{db: targetDB})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	executor := NewOrdinaryExecutor().WithClock(func() time.Time {
		return time.Date(2026, 4, 15, 11, 0, 0, 0, time.UTC)
	})

	// 1. E1: APPLY M1
	planE1 := buildReaderTestPlan(contracts.DirectionApply, 1, "exec-e1", "", "")
	if res, err := executor.Execute(ctx, buildExecutionEnv(conn, planE1, "job-e1", "att-e1")); err != nil || res.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("E1 failed: res=%+v err=%v", res, err)
	}

	// 2. E2: APPLY M2
	planE2 := buildReaderTestPlan(contracts.DirectionApply, 2, "exec-e2", "exec-e1", "")
	if res, err := executor.Execute(ctx, buildExecutionEnv(conn, planE2, "job-e2", "att-e2")); err != nil || res.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("E2 failed: res=%+v err=%v", res, err)
	}

	// 3. E3: ROLLBACK M2 (reverts E2)
	planE3 := buildReaderTestPlan(contracts.DirectionRollback, 2, "exec-e3", "exec-e2", "exec-e2")
	if res, err := executor.Execute(ctx, buildExecutionEnv(conn, planE3, "job-e3", "att-e3")); err != nil || res.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("E3 failed: res=%+v err=%v", res, err)
	}

	// 4. E4: FORWARD M2
	planE4 := buildReaderTestPlan(contracts.DirectionForward, 2, "exec-e4", "exec-e1", "exec-e3")
	if res, err := executor.Execute(ctx, buildExecutionEnv(conn, planE4, "job-e4", "att-e4")); err != nil || res.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("E4 failed: res=%+v err=%v", res, err)
	}

	provider := &fakeTargetDBProvider{
		pools: map[contracts.DatabaseID]*sql.DB{
			"db-1": sqlDB,
		},
	}
	reader := NewTargetEvidenceReader(provider)

	// Verify ActiveHead returns E4 (version 2).
	head, err := reader.ActiveHead(ctx, "db-1")
	if err != nil {
		t.Fatalf("ActiveHead() error = %v", err)
	}
	if head.ExecutionID != "exec-e4" || head.Version != 2 {
		t.Fatalf("ActiveHead() = %+v, want exec-e4 / version 2", head)
	}

	// Verify ListCommitHistory returns all 4 immutable events in order (E1, E2, E3, E4).
	events, err := reader.ListCommitHistory(ctx, "db-1")
	if err != nil {
		t.Fatalf("ListCommitHistory() error = %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("len(events) = %d, want 4", len(events))
	}
	if events[2].ExecutionID != "exec-e3" || events[2].RevertsExecutionID != "exec-e2" {
		t.Fatalf("expected E3 to record RevertsExecutionID=exec-e2, got %+v", events[2])
	}

	// Verify FilterHideRollbacks hides the reverted pair (E2 + E3) while keeping E1 and E4!
	visible := FilterHideRollbacks(events)
	var visibleIDs []contracts.ExecutionID
	for _, ev := range visible {
		visibleIDs = append(visibleIDs, ev.ExecutionID)
	}
	wantVisible := []contracts.ExecutionID{"exec-e1", "exec-e4"}
	if !reflect.DeepEqual(visibleIDs, wantVisible) {
		t.Fatalf("FilterHideRollbacks() execution IDs = %v, want %v (E2 and E3 hidden, E1 and E4 visible)",
			visibleIDs, wantVisible)
	}
}

func TestTargetEvidenceReaderBatchProgress(t *testing.T) {
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

	batchExec := NewBatchExecutor(newMemoryGateStore(), nil)
	plan := sampleBatchPlan(contracts.DirectionApply)
	env := buildExecutionEnv(conn, plan, "job-batch-1", "att-batch-1")
	_, _ = batchExec.Execute(ctx, env)

	provider := &fakeTargetDBProvider{
		pools: map[contracts.DatabaseID]*sql.DB{
			"db-1": sqlDB,
		},
	}
	reader := NewTargetEvidenceReader(provider)

	prog, err := reader.BatchProgress(ctx, "db-1", "exec-batch-1")
	if err != nil {
		t.Fatalf("BatchProgress() error = %v", err)
	}
	if prog.TotalScopeRows != 5 || prog.CommittedBatches != 2 || prog.CommittedRows != 4 || prog.LastCommittedCursor != "40" {
		t.Fatalf("BatchProgress() = %+v, want TotalScopeRows=5 CommittedBatches=2 CommittedRows=4 LastCommittedCursor=40", prog)
	}
}

func TestTargetEvidenceReaderIsStrictlyReadOnlyOnUninitializedTargetDB(t *testing.T) {
	// Ensure View-Only / uninitialized target DBs with missing if3110_platform (42P01 / 3F000)
	// never execute DDL and return empty/zero results cleanly.
	backend := &fakeTargetSchemaBackend{
		initialized: false,
	}
	sqlDB := sql.OpenDB(fakeTargetSchemaConnector{backend: backend})
	t.Cleanup(func() { _ = sqlDB.Close() })

	provider := &fakeTargetDBProvider{
		pools: map[contracts.DatabaseID]*sql.DB{
			"db-uninit": sqlDB,
		},
	}
	reader := NewTargetEvidenceReader(provider)
	ctx := context.Background()

	head, err := reader.ActiveHead(ctx, "db-uninit")
	if err != nil {
		t.Fatalf("ActiveHead() on uninitialized DB error = %v, want nil", err)
	}
	if head != (contracts.ActiveHeadState{}) {
		t.Fatalf("ActiveHead() = %+v, want zero ActiveHeadState", head)
	}

	events, err := reader.ListCommitHistory(ctx, "db-uninit")
	if err != nil {
		t.Fatalf("ListCommitHistory() on uninitialized DB error = %v, want nil", err)
	}
	if len(events) != 0 {
		t.Fatalf("len(events) = %d, want 0", len(events))
	}

	prog, err := reader.BatchProgress(ctx, "db-uninit", "exec-none")
	if err != nil {
		t.Fatalf("BatchProgress() on uninitialized DB error = %v, want nil", err)
	}
	if prog.CommittedBatches != 0 || prog.CommittedRows != 0 {
		t.Fatalf("BatchProgress() = %+v, want zero progress", prog)
	}

	if len(backend.ddlCalls) != 0 {
		t.Fatalf("ddlCalls = %d, want 0 (TargetEvidenceReader must never run DDL)", len(backend.ddlCalls))
	}
}

func TestJobSummaryCapabilitiesForPartialAndIrreversibleJobs(t *testing.T) {
	revJob := jobs.Job{
		ID:     "job-partial-rev",
		Status: contracts.JobStatusPartial,
		Plan: contracts.ExecutionPlan{
			IsIrreversible: false,
		},
	}
	sumRev := revJob.Summary()
	if !sumRev.Capabilities.RequestResume || !sumRev.Capabilities.RequestCompensation {
		t.Fatalf("reversible PARTIAL job capabilities = %+v, want both RequestResume and RequestCompensation true",
			sumRev.Capabilities)
	}

	irrevJob := jobs.Job{
		ID:     "job-partial-irrev",
		Status: contracts.JobStatusPartiallyReverted,
		Plan: contracts.ExecutionPlan{
			IsIrreversible: true,
		},
	}
	sumIrrev := irrevJob.Summary()
	if !sumIrrev.Capabilities.RequestResume || sumIrrev.Capabilities.RequestCompensation {
		t.Fatalf("irreversible PARTIALLY_REVERTED job capabilities = %+v, want RequestResume=true RequestCompensation=false",
			sumIrrev.Capabilities)
	}
}
