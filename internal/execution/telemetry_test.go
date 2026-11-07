package execution

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/telemetry"
)

func TestWorkerContinuesDurableTraceAcrossOutcomes(t *testing.T) {
	for _, status := range []contracts.JobStatus{contracts.JobStatusSuccess, contracts.JobStatusFailed, contracts.JobStatusRetryWait} {
		t.Run(string(status), func(t *testing.T) {
			var logs bytes.Buffer
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			previous, previousLogger := otel.GetTracerProvider(), slog.Default()
			otel.SetTracerProvider(provider)
			logger := telemetry.NewLogger(&logs)
			slog.SetDefault(logger)
			t.Cleanup(func() {
				_ = provider.Shutdown(context.Background())
				otel.SetTracerProvider(previous)
				slog.SetDefault(previousLogger)
			})
			producerCtx, producer := telemetry.StartSpan(telemetry.Extract(context.Background(), nil), "queue.publish")
			job := makeTestJob("job-trace", "12", "")
			job.Plan.UpSQL = "SELECT 'private-sql-literal'"
			job.Plan.PlanChecksum = job.Plan.ComputeChecksum()
			job.TraceCarrier = telemetry.Inject(producerCtx)
			producer.End()
			queue := newMemoryJobQueue(job)
			worker, err := NewWorker(WorkerConfig{
				WorkerID: "trace-worker", Queue: queue, ConnProvider: &fakeConnProvider{server: &fakeLockServer{}}, Logger: logger,
				Executor: PlanExecutorFunc(func(ctx context.Context, env ExecutionEnv) (ExecutionResult, error) {
					fields := telemetry.ContextFields(ctx)
					if fields.JobID != string(job.ID) || fields.ActorID != string(job.ActorID) || fields.DatabaseID != "12" || fields.Attempt != 1 || fields.RequestID != job.TraceCarrier["x-request-id"] || trace.SpanContextFromContext(ctx).TraceID() != producer.SpanContext().TraceID() {
						t.Fatal("worker lost propagated execution context")
					}
					result := ExecutionResult{JobStatus: status, AttemptStatus: contracts.AttemptStatusCommitted, ExecutedSQL: env.Claimed.Job.Plan.UpSQL}
					if status != contracts.JobStatusSuccess {
						result.AttemptStatus = contracts.AttemptStatusFailed
						result.ErrorCode = "SQL_FAILED"
						result.TransientRetryable = status == contracts.JobStatusRetryWait
						return result, errors.New("private-error-details")
					}
					return result, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			if processed, err := worker.ProcessOnce(context.Background(), "trace-worker"); !processed || err != nil {
				t.Fatalf("process attempt: processed=%v error=%v", processed, err)
			}
			if queue.getJob(job.ID).Status != status || strings.Contains(logs.String(), "private-") {
				t.Fatal("worker outcome changed or secrets leaked")
			}
			var attemptFound, lockFound, executeFound bool
			for _, span := range exporter.GetSpans() {
				if span.SpanContext.TraceID() != producer.SpanContext().TraceID() {
					t.Fatal("worker span disconnected from durable parent")
				}
				attemptFound = attemptFound || span.Name == "worker.attempt" && span.Parent.SpanID() == producer.SpanContext().SpanID()
				if span.Name == "worker.attempt" && (span.Status.Code == codes.Error) != (status != contracts.JobStatusSuccess) {
					t.Fatal("attempt span did not reflect the execution outcome")
				}
				lockFound = lockFound || span.Name == "worker.lock.acquire"
				executeFound = executeFound || span.Name == "worker.execute"
			}
			if !attemptFound || !lockFound || !executeFound {
				t.Fatal("worker/lock/executor spans missing")
			}
		})
	}
}

func TestTracePropagationAndJSONActivityLogsForBatchAndOrdinarySQL(t *testing.T) {
	// TRACE-01 & LOG-01: Verify full W3C trace propagation across batch execution
	// (execution.batch -> sql.query -> sql.commit) and ordinary execution rollback
	// (sql.query -> sql.rollback), plus redacted JSON activity logs with required fields.
	var logs bytes.Buffer
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	prevTP, prevLogger := otel.GetTracerProvider(), slog.Default()
	otel.SetTracerProvider(provider)
	logger := telemetry.NewLogger(&logs)
	slog.SetDefault(logger)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		slog.SetDefault(prevLogger)
	})

	rootCtx := telemetry.WithFields(telemetry.Extract(context.Background(), nil), telemetry.Fields{
		ActorID:     "user-kevin",
		DatabaseID:  "db-1",
		MigrationID: "mig-batch-1",
		RevisionID:  "rev-batch-1",
		JobID:       "job-batch-telemetry",
		ExecutionID: "exec-batch-1",
		AttemptID:   "att-batch-1",
		Attempt:     1,
		Direction:   string(contracts.DirectionApply),
	})
	batchParentCtx, rootSpan := telemetry.StartSpan(rootCtx, "worker.execute")

	// 1. Run a 2-batch execution through BatchExecutor and verify child spans & JSON logs.
	engine := newFakeBatchEngineDB(map[int64]string{
		10: "PENDING",
		20: "PENDING",
		30: "PENDING",
	})
	batchSQLDB := sql.OpenDB(fakeBatchEngineConnector{db: engine})
	t.Cleanup(func() { _ = batchSQLDB.Close() })

	batchConn, err := batchSQLDB.Conn(batchParentCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = batchConn.Close() }()

	batchExec := NewBatchExecutor(newMemoryGateStore(), nil)
	batchPlan := sampleBatchPlan(contracts.DirectionApply)
	envBatch := buildExecutionEnv(batchConn, batchPlan, "job-batch-telemetry", "att-batch-1")
	resBatch, err := batchExec.Execute(batchParentCtx, envBatch)
	if err != nil || resBatch.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("BatchExecutor.Execute() = %+v, err = %v", resBatch, err)
	}

	// 2. Run a failing ordinary SQL statement with a sensitive literal to verify sql.rollback & redaction.
	ordDB := newFakeOrdinaryTargetDB()
	ordDB.failNextUserSQL = &pgconn.PgError{
		Code:    "23505",
		Message: "duplicate key value violates unique constraint (secret-customer-token)",
	}
	ordSQLDB := sql.OpenDB(fakeOrdinaryConnector{db: ordDB})
	t.Cleanup(func() { _ = ordSQLDB.Close() })

	ordConn, err := ordSQLDB.Conn(batchParentCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ordConn.Close() }()

	ordPlan := contracts.ExecutionPlan{
		RequestID:                "req-ord-fail",
		ExecutionID:              "exec-ord-fail",
		MigrationID:              "mig-ord-1",
		RevisionID:               "rev-ord-1",
		Version:                  1,
		Direction:                contracts.DirectionApply,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-kevin",
		TargetDatabaseID:         "db-1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "",
		PolicyVersion:            1,
		UpSQL:                    "INSERT INTO users (email, api_secret) VALUES ('alice@example.com', 'top-secret-password-999');",
		DownSQL:                  "DELETE FROM users;",
	}
	ordPlan.PlanChecksum = ordPlan.ComputeChecksum()
	ordExec := NewOrdinaryExecutor()
	resOrd, err := ordExec.Execute(batchParentCtx, buildExecutionEnv(ordConn, ordPlan, "job-ord-fail", "att-ord-1"))
	if err != nil || resOrd.JobStatus != contracts.JobStatusFailed {
		t.Fatalf("OrdinaryExecutor.Execute(fail) = %+v, err = %v", resOrd, err)
	}
	rootSpan.End()

	// Verify OpenTelemetry spans share root TraceID and include execution.batch, sql.query, sql.commit, sql.rollback.
	wantTraceID := rootSpan.SpanContext().TraceID()
	spanCounts := make(map[string]int)
	for _, s := range exporter.GetSpans() {
		if s.SpanContext.TraceID() != wantTraceID {
			t.Fatalf("span %q has TraceID %s, want %s", s.Name, s.SpanContext.TraceID(), wantTraceID)
		}
		spanCounts[s.Name]++
	}
	for _, requiredSpan := range []string{"execution.batch", "sql.query", "sql.commit", "sql.rollback"} {
		if spanCounts[requiredSpan] == 0 {
			t.Fatalf("expected OpenTelemetry span %q to be recorded, got counts=%v", requiredSpan, spanCounts)
		}
	}
	if spanCounts["execution.batch"] != 2 {
		t.Fatalf("execution.batch span count = %d, want 2", spanCounts["execution.batch"])
	}

	// Verify JSON logs do NOT leak sensitive literals and contain required structured fields.
	rawLogs := logs.String()
	for _, secret := range []string{"top-secret-password-999", "alice@example.com", "secret-customer-token"} {
		if strings.Contains(rawLogs, secret) {
			t.Fatalf("JSON activity logs leaked sensitive value %q:\n%s", secret, rawLogs)
		}
	}

	seenEvents := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(rawLogs), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid JSON log line %q: %v", line, err)
		}
		ev, _ := entry["event"].(string)
		seenEvents[ev] = true
		if entry["trace_id"] != wantTraceID.String() {
			t.Fatalf("log event %q trace_id = %v, want %s", ev, entry["trace_id"], wantTraceID.String())
		}
		for _, key := range []string{"time", "level", "actor_id", "database_id", "request_id", "job_id", "execution_id", "direction"} {
			if _, ok := entry[key]; !ok {
				t.Fatalf("log event %q missing required field %q: %v", ev, key, entry)
			}
		}
		if ev == "sql.result" || ev == "tx.commit" || ev == "tx.rollback" {
			for _, key := range []string{"query", "duration_ms", "affected_rows", "sqlstate"} {
				if _, ok := entry[key]; !ok {
					t.Fatalf("log event %q missing SQL telemetry field %q: %v", ev, key, entry)
				}
			}
		}
	}

	for _, wantEv := range []string{"batch.start", "batch.result", "sql.start", "sql.result", "tx.commit", "tx.rollback"} {
		if !seenEvents[wantEv] {
			t.Fatalf("expected structured JSON log event %q, seen=%v", wantEv, seenEvents)
		}
	}
}
