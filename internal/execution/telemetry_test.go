package execution

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

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
