package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/internal/contracts"
)

const (
	perfBenchmarkUpSQL = "UPDATE public.bench_orders SET amount_cents = amount_cents + 100, loyalty_tier = 'GOLD', updated_flag = TRUE WHERE status = 'SETTLED';"
	perfBenchmarkDown  = "UPDATE public.bench_orders SET amount_cents = amount_cents - 100, loyalty_tier = 'STANDARD', updated_flag = FALSE WHERE status = 'SETTLED';"
)

// BenchmarkRunMetrics captures the timing, row counts, batch counts, and peak
// worker memory metrics required by PERF-01.
type BenchmarkRunMetrics struct {
	Label               string
	TotalTableRows      int64
	AffectedRows        int64
	CommittedBatches    int
	BatchSize           int
	StatementDuration   time.Duration
	TotalCommitDuration time.Duration
	AvgBatchDuration    time.Duration
	PeakHeapAllocBytes  uint64
	PeakSysBytes        uint64
	FinalChecksumCents  int64
}

func resolveBenchmarkTargetDSN() string {
	for _, key := range []string{
		"TARGET_TEST_DATABASE_URL",
		"TEST_TARGET_DATABASE_URL",
		"BENCHMARK_DATABASE_URL",
	} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

func seedBenchmarkOrders(ctx context.Context, db *sql.DB, totalRows int64) error {
	const ddl = `
		DROP TABLE IF EXISTS public.bench_orders;

		CREATE TABLE public.bench_orders (
			id BIGINT PRIMARY KEY,
			customer_code TEXT NOT NULL,
			amount_cents BIGINT NOT NULL,
			status TEXT NOT NULL,
			loyalty_tier TEXT NOT NULL DEFAULT 'STANDARD',
			updated_flag BOOLEAN NOT NULL DEFAULT FALSE
		);
	`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("recreate bench_orders: %w", err)
	}

	const insertSQL = `
		INSERT INTO public.bench_orders (
			id,
			customer_code,
			amount_cents,
			status,
			loyalty_tier,
			updated_flag
		)
		SELECT
			gs AS id,
			'CUST-' || lpad(((gs % 100000) + 1)::text, 6, '0') AS customer_code,
			((gs * 37) % 500000) + 1000 AS amount_cents,
			CASE
				WHEN gs % 10 = 0 THEN 'CANCELLED'
				ELSE 'SETTLED'
			END AS status,
			'STANDARD' AS loyalty_tier,
			FALSE AS updated_flag
		FROM generate_series(1, $1::bigint) AS gs
	`
	if _, err := db.ExecContext(ctx, insertSQL, totalRows); err != nil {
		return fmt.Errorf("seed bench_orders (%d rows): %w", totalRows, err)
	}

	const indexSQL = `
		CREATE INDEX bench_orders_status_id_idx
			ON public.bench_orders (status, id);

		CREATE INDEX bench_orders_loyalty_tier_idx
			ON public.bench_orders (loyalty_tier, customer_code);
	`
	if _, err := db.ExecContext(ctx, indexSQL); err != nil {
		return fmt.Errorf("index bench_orders: %w", err)
	}

	if _, err := db.ExecContext(ctx, `VACUUM ANALYZE public.bench_orders`); err != nil {
		return fmt.Errorf("vacuum analyze bench_orders: %w", err)
	}
	return nil
}

func resetBenchmarkTargetMetadata(ctx context.Context, db *sql.DB) error {
	const resetMetaSQL = `
		DROP SCHEMA IF EXISTS if3110_platform CASCADE;
	`
	_, err := db.ExecContext(ctx, resetMetaSQL)
	return err
}

func queryBenchmarkFinalChecksum(ctx context.Context, db *sql.DB) (updatedRows int64, totalCents int64, err error) {
	const q = `
		SELECT
			COUNT(*) FILTER (WHERE updated_flag = TRUE AND loyalty_tier = 'GOLD'),
			COALESCE(SUM(amount_cents), 0)
		FROM public.bench_orders
	`
	if err := db.QueryRowContext(ctx, q).Scan(&updatedRows, &totalCents); err != nil {
		return 0, 0, err
	}
	return updatedRows, totalCents, nil
}

func sampleMemoryBytes() (heapAlloc uint64, sysBytes uint64) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc, ms.Sys
}

func runSingleUnbatchedBaseline(
	ctx context.Context,
	db *sql.DB,
	label string,
	totalRows int64,
) (BenchmarkRunMetrics, error) {
	if err := seedBenchmarkOrders(ctx, db, totalRows); err != nil {
		return BenchmarkRunMetrics{}, err
	}

	runtime.GC()
	heapBefore, sysBefore := sampleMemoryBytes()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkRunMetrics{}, err
	}
	defer func() { _ = tx.Rollback() }()

	totalStart := time.Now()
	stmtStart := time.Now()
	res, err := tx.ExecContext(ctx, perfBenchmarkUpSQL)
	if err != nil {
		return BenchmarkRunMetrics{}, err
	}
	stmtDur := time.Since(stmtStart)

	affected, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return BenchmarkRunMetrics{}, err
	}
	totalDur := time.Since(totalStart)

	heapAfter, sysAfter := sampleMemoryBytes()
	peakHeap := heapAfter
	if heapBefore > peakHeap {
		peakHeap = heapBefore
	}
	peakSys := sysAfter
	if sysBefore > peakSys {
		peakSys = sysBefore
	}

	_, checksumCents, err := queryBenchmarkFinalChecksum(ctx, db)
	if err != nil {
		return BenchmarkRunMetrics{}, err
	}

	return BenchmarkRunMetrics{
		Label:               label,
		TotalTableRows:      totalRows,
		AffectedRows:        affected,
		CommittedBatches:    1,
		BatchSize:           int(affected),
		StatementDuration:   stmtDur,
		TotalCommitDuration: totalDur,
		AvgBatchDuration:    totalDur,
		PeakHeapAllocBytes:  peakHeap,
		PeakSysBytes:        peakSys,
		FinalChecksumCents:  checksumCents,
	}, nil
}

// TestBenchmarkRunnerDeterministicEquivalence verifies in-memory that running a
// migration across multiple keyset batches (including a mid-run PARTIAL halt at
// batch 3 followed by RESUME) produces 100% identical final row states and
// committed row counts as a single full run without duplicating any batch.
func TestBenchmarkRunnerDeterministicEquivalence(t *testing.T) {
	initialRows := map[int64]string{
		1:  "PENDING",
		2:  "PENDING",
		3:  "PENDING",
		4:  "PENDING",
		5:  "PENDING",
		6:  "PENDING",
		7:  "PENDING",
		8:  "PENDING",
		9:  "PENDING",
		10: "PENDING",
	}

	// 1. Clean full batch run (5 batches of 2 rows each = 10 rows).
	engineFull := newFakeBatchEngineDB(initialRows)
	sqlDBFull := sql.OpenDB(fakeBatchEngineConnector{db: engineFull})
	t.Cleanup(func() { _ = sqlDBFull.Close() })

	ctx := context.Background()
	connFull, err := sqlDBFull.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connFull.Close() }()

	execFull := NewBatchExecutor(newMemoryGateStore(), nil)
	planFull := sampleBatchPlan(contracts.DirectionApply)
	resFull, err := execFull.Execute(ctx, buildExecutionEnv(connFull, planFull, "job-full", "att-full"))
	if err != nil || resFull.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("full batch run failed: res=%+v err=%v", resFull, err)
	}
	if resFull.CommittedBatches != 5 || resFull.CommittedRows != 10 {
		t.Fatalf("full batch metrics = (%d batches, %d rows), want (5, 10)", resFull.CommittedBatches, resFull.CommittedRows)
	}

	// 2. Identical initial state with a deterministic failure on batch 3 (batches 1..2 committed -> PARTIAL),
	// followed by RESUME for batches 3..5.
	engineResume := newFakeBatchEngineDB(initialRows)
	engineResume.failOnBatchNo = 3
	engineResume.failBatchErr = errors.New("deterministic failpoint on batch 3")

	sqlDBResume := sql.OpenDB(fakeBatchEngineConnector{db: engineResume})
	t.Cleanup(func() { _ = sqlDBResume.Close() })

	connResume, err := sqlDBResume.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connResume.Close() }()

	gates := newMemoryGateStore()
	execResume := NewBatchExecutor(gates, nil)
	resPartial, err := execResume.Execute(ctx, buildExecutionEnv(connResume, planFull, "job-partial", "att-partial-1"))
	if err != nil || resPartial.JobStatus != contracts.JobStatusPartial {
		t.Fatalf("expected PARTIAL on batch 3 failpoint, got res=%+v err=%v", resPartial, err)
	}
	if resPartial.CommittedBatches != 2 || resPartial.CommittedRows != 4 {
		t.Fatalf("partial metrics = (%d batches, %d rows), want (2, 4)", resPartial.CommittedBatches, resPartial.CommittedRows)
	}

	planResume := sampleBatchPlan(contracts.DirectionResume)
	resResumed, err := execResume.Execute(ctx, buildExecutionEnv(connResume, planResume, "job-resume", "att-resume-1"))
	if err != nil || resResumed.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("expected SUCCESS on RESUME, got res=%+v err=%v", resResumed, err)
	}
	if resResumed.CommittedBatches != 5 || resResumed.CommittedRows != 10 {
		t.Fatalf("resumed metrics = (%d batches, %d rows), want (5, 10)", resResumed.CommittedBatches, resResumed.CommittedRows)
	}

	// Verify final table state of full run vs fail+resume run is 100% identical!
	for id := int64(1); id <= 10; id++ {
		if engineFull.tableRows[id] != engineResume.tableRows[id] {
			t.Fatalf("row %d mismatch between full run (%+v) and fail+resume run (%+v)",
				id, engineFull.tableRows[id], engineResume.tableRows[id])
		}
	}
}

// TestPerf01BatchVsSingleUpdateBenchmarkIntegration executes the live PostgreSQL
// PERF-01 benchmark when TARGET_TEST_DATABASE_URL (or BENCHMARK_DATABASE_URL) is set:
//   - Calibrates dataset size N (starting at 1,000,000 rows) so a single UPDATE
//     exceeds 1 second without pg_sleep.
//   - Runs 3 unbatched single-UPDATE baselines from identical initial states.
//   - Runs Mode Batch (25,000 rows/batch) from the same initial state and records
//     statement duration, commit time, batch count, and peak memory.
//   - Runs Mode Batch with a mid-execution pause/failpoint after batch 4 (PARTIAL)
//     followed by RESUME, verifying 0 duplicate updates and 100% identical final checksum.
func TestPerf01BatchVsSingleUpdateBenchmarkIntegration(t *testing.T) {
	dsn := resolveBenchmarkTargetDSN()
	if dsn == "" {
		t.Skip("TARGET_TEST_DATABASE_URL / BENCHMARK_DATABASE_URL not set; skipping live Postgres >1s benchmark")
	}
	if os.Getenv("RUN_PERF_BENCHMARK") == "" && testing.Short() {
		t.Skip("skipping >1s dataset benchmark in -short mode")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open benchmark db: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = db.ExecContext(cleanupCtx, `DROP SCHEMA IF EXISTS if3110_platform CASCADE; DROP TABLE IF EXISTS public.bench_orders CASCADE;`)
		_ = db.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// Calibrate row count so single UPDATE exceeds 1 second when RUN_PERF_BENCHMARK=1;
	// otherwise use a compact deterministic dataset (2,500 rows / 250 batch size = 10 batches)
	// so CI integration suites execute the full live PostgreSQL benchmark flow without skipping.
	totalRows := int64(2_500)
	batchSize := 250
	if os.Getenv("RUN_PERF_BENCHMARK") == "1" {
		totalRows = 1_000_000
		batchSize = 25_000
		const minSingleUpdateDuration = 1050 * time.Millisecond
		for attempt := 0; attempt < 4; attempt++ {
			probe, err := runSingleUnbatchedBaseline(ctx, db, "Calibration-Probe", totalRows)
			if err != nil {
				t.Fatalf("calibration probe failed: %v", err)
			}
			if probe.TotalCommitDuration >= minSingleUpdateDuration || totalRows >= 8_000_000 {
				t.Logf("calibrated dataset: rows=%d single_update_duration=%s", totalRows, probe.TotalCommitDuration)
				break
			}
			ratio := float64(1250*time.Millisecond) / float64(probe.TotalCommitDuration+1*time.Millisecond)
			if ratio < 1.5 {
				ratio = 1.5
			}
			totalRows = int64(float64(totalRows) * ratio)
		}
	}

	// 1. Run 3x Single UPDATE Baselines from identical initial states.
	var baselines [3]BenchmarkRunMetrics
	for i := 0; i < 3; i++ {
		m, err := runSingleUnbatchedBaseline(ctx, db, fmt.Sprintf("Baseline-%d (Unbatched)", i+1), totalRows)
		if err != nil {
			t.Fatalf("baseline %d failed: %v", i+1, err)
		}
		baselines[i] = m
		t.Logf("[PERF-01] %s: rows=%d affected=%d stmt_ms=%.2f commit_ms=%.2f heap_mb=%.2f",
			m.Label,
			m.TotalTableRows,
			m.AffectedRows,
			float64(m.StatementDuration)/float64(time.Millisecond),
			float64(m.TotalCommitDuration)/float64(time.Millisecond),
			float64(m.PeakHeapAllocBytes)/(1024*1024),
		)
	}

	// 2. Run Mode Batch on the exact same initial dataset state.
	if err := seedBenchmarkOrders(ctx, db, totalRows); err != nil {
		t.Fatalf("seed for batch run: %v", err)
	}
	if err := resetBenchmarkTargetMetadata(ctx, db); err != nil {
		t.Fatalf("reset metadata for batch run: %v", err)
	}

	connBatch, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire conn for batch run: %v", err)
	}
	defer func() { _ = connBatch.Close() }()

	runtime.GC()
	heapBefore, sysBefore := sampleMemoryBytes()

	batchPlan := contracts.ExecutionPlan{
		RequestID:                "req-perf-batch-1",
		ExecutionID:              "exec-perf-batch-1",
		MigrationID:              "mig-perf-batch-1",
		RevisionID:               "rev-perf-batch-1",
		Version:                  1,
		Direction:                contracts.DirectionApply,
		Mode:                     contracts.ModeBatch,
		ActorID:                  "1",
		TargetDatabaseID:         "1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "",
		PolicyVersion:            1,
		UpSQL:                    perfBenchmarkUpSQL,
		DownSQL:                  perfBenchmarkDown,
		Batch: contracts.BatchConfig{
			TableSchema:      "public",
			TableName:        "bench_orders",
			PrimaryKeyColumn: "id",
			ScopeWhere:       "status = 'SETTLED'",
			BatchSize:        batchSize,
		},
	}
	batchPlan.PlanChecksum = batchPlan.ComputeChecksum()

	var peakHeap, peakSys uint64
	progressTracker := &recordingProgressUpdater{}
	batchExec := NewBatchExecutor(newMemoryGateStore(), progressTracker)

	batchStart := time.Now()
	resBatch, err := batchExec.Execute(ctx, buildExecutionEnv(connBatch, batchPlan, "job-perf-batch", "att-perf-batch"))
	batchDur := time.Since(batchStart)
	if err != nil || resBatch.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("batch run failed: res=%+v err=%v", resBatch, err)
	}

	heapAfter, sysAfter := sampleMemoryBytes()
	peakHeap = maxUint64(heapBefore, heapAfter)
	peakSys = maxUint64(sysBefore, sysAfter)

	batchUpdatedRows, batchChecksumCents, err := queryBenchmarkFinalChecksum(ctx, db)
	if err != nil {
		t.Fatalf("checksum after batch run: %v", err)
	}
	if batchUpdatedRows != baselines[0].AffectedRows || batchChecksumCents != baselines[0].FinalChecksumCents {
		t.Fatalf("batch final state (%d rows, %d cents) != baseline (%d rows, %d cents)",
			batchUpdatedRows, batchChecksumCents, baselines[0].AffectedRows, baselines[0].FinalChecksumCents)
	}

	avgBatchDur := time.Duration(0)
	if resBatch.CommittedBatches > 0 {
		avgBatchDur = batchDur / time.Duration(resBatch.CommittedBatches)
	}
	t.Logf("[PERF-01] Mode Batch (size=%d): batches=%d rows=%d total_ms=%.2f avg_batch_ms=%.2f heap_mb=%.2f sys_mb=%.2f",
		batchSize,
		resBatch.CommittedBatches,
		resBatch.CommittedRows,
		float64(batchDur)/float64(time.Millisecond),
		float64(avgBatchDur)/float64(time.Millisecond),
		float64(peakHeap)/(1024*1024),
		float64(peakSys)/(1024*1024),
	)

	// 3. Run Batch Halt at Batch 5 (PARTIAL after 4 batches) + RESUME to completion.
	if err := seedBenchmarkOrders(ctx, db, totalRows); err != nil {
		t.Fatalf("seed for fail+resume run: %v", err)
	}
	if err := resetBenchmarkTargetMetadata(ctx, db); err != nil {
		t.Fatalf("reset metadata for fail+resume run: %v", err)
	}

	gates := newMemoryGateStore()
	pauseExec := NewBatchExecutor(gates, nil).WithPauseChecker(func(_ context.Context, _ contracts.JobID, nextBatchNo int) bool {
		return nextBatchNo == 5
	})
	resHalt, err := pauseExec.Execute(ctx, buildExecutionEnv(connBatch, batchPlan, "job-perf-halt", "att-perf-halt"))
	if err != nil || resHalt.JobStatus != contracts.JobStatusPartial || resHalt.CommittedBatches != 4 {
		t.Fatalf("expected PARTIAL after 4 committed batches, got res=%+v err=%v", resHalt, err)
	}

	resumePlan := batchPlan
	resumePlan.Direction = contracts.DirectionResume
	resumePlan.OriginExecutionID = batchPlan.ExecutionID
	resumePlan.ExecutionID = "exec-perf-batch-resume"
	resumePlan.PlanChecksum = resumePlan.ComputeChecksum()

	resumeExec := NewBatchExecutor(gates, nil)
	resResume, err := resumeExec.Execute(ctx, buildExecutionEnv(connBatch, resumePlan, "job-perf-resume", "att-perf-resume"))
	if err != nil || resResume.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("expected SUCCESS on RESUME, got res=%+v err=%v", resResume, err)
	}

	resumedUpdatedRows, resumedChecksumCents, err := queryBenchmarkFinalChecksum(ctx, db)
	if err != nil {
		t.Fatalf("checksum after fail+resume run: %v", err)
	}
	if resumedUpdatedRows != baselines[0].AffectedRows || resumedChecksumCents != baselines[0].FinalChecksumCents {
		t.Fatalf("fail+resume final state (%d rows, %d cents) != baseline (%d rows, %d cents)",
			resumedUpdatedRows, resumedChecksumCents, baselines[0].AffectedRows, baselines[0].FinalChecksumCents)
	}
	t.Logf("[PERF-01] Batch Halt@4 + Resume: total_batches=%d total_rows=%d checksum_cents=%d (100%% match with baseline)",
		resResume.CommittedBatches, resResume.CommittedRows, resumedChecksumCents)
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
