package execution

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/jobs"
	"if3110.local/db-platform/internal/telemetry"
)

func (w *Worker) completeAttempt(ctx context.Context, input jobs.CompleteAttemptInput) (jobs.Job, jobs.JobAttempt, error) {
	attemptSpan := trace.SpanFromContext(ctx)
	ctx, span := telemetry.StartSpan(ctx, "worker.complete")
	job, attempt, err := w.cfg.Queue.CompleteAttempt(ctx, input)
	outcome := err
	if outcome == nil && input.JobStatus != contracts.JobStatusSuccess {
		outcome = errors.New("attempt did not succeed")
	}
	level := slog.LevelInfo
	if outcome != nil {
		level = slog.LevelError
	}
	w.cfg.Logger.Log(ctx, level, "worker.attempt.result", "status", string(input.JobStatus),
		"persisted", err == nil, "error_code", input.ErrorCode, "error", err,
		"executed_sql", telemetry.RedactSQL(input.ExecutedSQL), "committed_batches", input.CommittedBatches, "committed_rows", input.CommittedRows)
	telemetry.FinishSpan(span, outcome)
	telemetry.RecordError(attemptSpan, outcome)
	return job, attempt, err
}

func (w *Worker) scheduleRetry(ctx context.Context, input jobs.ScheduleRetryInput) (jobs.Job, jobs.JobAttempt, error) {
	attemptSpan := trace.SpanFromContext(ctx)
	ctx, span := telemetry.StartSpan(ctx, "worker.retry")
	job, attempt, err := w.cfg.Queue.ScheduleRetry(ctx, input)
	w.cfg.Logger.WarnContext(ctx, "worker.retry.result", "status", string(job.Status), "persisted", err == nil,
		"error_code", input.ErrorCode, "error", err, "retry_count", job.RetryCount, "available_at", job.AvailableAt,
		"executed_sql", telemetry.RedactSQL(input.ExecutedSQL))
	telemetry.FinishSpan(span, err)
	telemetry.RecordError(attemptSpan, errors.New("attempt requires retry"))
	return job, attempt, err
}
