package execution

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
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

func startBatchSpan(
	ctx context.Context,
	batchNo int,
	batchSize int,
	direction contracts.OperationDirection,
) (context.Context, func(rowsAffected int64, status contracts.JobStatus, errCode string, errMsg string)) {
	ctx = telemetry.WithFields(ctx, telemetry.Fields{
		Batch:     batchNo,
		Direction: string(direction),
	})
	started := time.Now()
	ctx, span := telemetry.StartSpan(
		ctx,
		"execution.batch",
		trace.WithAttributes(
			attribute.Int("batch_no", batchNo),
			attribute.Int("batch_size", batchSize),
			attribute.String("direction", string(direction)),
		),
	)
	slog.LogAttrs(
		ctx,
		slog.LevelInfo,
		"batch.start",
		slog.String("status", "STARTED"),
		slog.Int("batch_size", batchSize),
	)

	return ctx, func(rowsAffected int64, status contracts.JobStatus, errCode string, errMsg string) {
		durationMs := float64(time.Since(started)) / float64(time.Millisecond)
		span.SetAttributes(
			attribute.Int64("affected_rows", rowsAffected),
			attribute.String("status", string(status)),
		)
		var spanErr error
		level := slog.LevelInfo
		if status != contracts.JobStatusSuccess {
			level = slog.LevelError
			if errMsg != "" {
				spanErr = errors.New(errMsg)
			} else {
				spanErr = errors.New("batch execution failed")
			}
		}
		attrs := []slog.Attr{
			slog.String("status", string(status)),
			slog.Int64("affected_rows", rowsAffected),
			slog.Float64("duration_ms", durationMs),
		}
		if errCode != "" {
			attrs = append(attrs, slog.String("error_code", errCode))
		}
		if spanErr != nil {
			attrs = append(attrs, slog.Any("error", spanErr))
		}
		slog.LogAttrs(ctx, level, "batch.result", attrs...)
		telemetry.FinishSpan(span, spanErr)
	}
}

func execUserSQLWithTelemetry(
	ctx context.Context,
	tx *sql.Tx,
	query string,
	args ...any,
) (sql.Result, int64, error) {
	redactedQuery := telemetry.RedactSQL(query)
	started := time.Now()
	sqlCtx, span := telemetry.StartSpan(
		ctx,
		"sql.query",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.String("db.query.text", redactedQuery),
		),
	)
	slog.LogAttrs(
		sqlCtx,
		slog.LevelInfo,
		"sql.start",
		slog.String("status", "STARTED"),
		slog.String("query", redactedQuery),
	)

	res, err := tx.ExecContext(sqlCtx, query, args...)
	durationMs := float64(time.Since(started)) / float64(time.Millisecond)

	var affectedRows int64
	if res != nil {
		if n, rowsErr := res.RowsAffected(); rowsErr == nil && n >= 0 {
			affectedRows = n
		}
	}

	span.SetAttributes(attribute.Int64("affected_rows", affectedRows))

	status := "SUCCESS"
	level := slog.LevelInfo
	sqlState := ""
	if err != nil {
		status = "FAILED"
		level = slog.LevelError
		sqlState = telemetry.SQLState(err)
		if sqlState == "" {
			sqlState = extractSQLState(err, "")
		}
		if sqlState != "" {
			span.SetAttributes(attribute.String("sqlstate", sqlState))
		}
	}

	attrs := []slog.Attr{
		slog.String("status", status),
		slog.String("query", redactedQuery),
		slog.Float64("duration_ms", durationMs),
		slog.Int64("affected_rows", affectedRows),
		slog.String("sqlstate", sqlState),
	}
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
	}
	slog.LogAttrs(sqlCtx, level, "sql.result", attrs...)
	telemetry.FinishSpan(span, err)
	return res, affectedRows, err
}

func commitTxWithTelemetry(
	ctx context.Context,
	tx *sql.Tx,
	query string,
	affectedRows int64,
) error {
	redactedQuery := telemetry.RedactSQL(query)
	started := time.Now()
	commitCtx, span := telemetry.StartSpan(
		ctx,
		"sql.commit",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.Int64("affected_rows", affectedRows),
		),
	)

	err := tx.Commit()
	durationMs := float64(time.Since(started)) / float64(time.Millisecond)

	status := "SUCCESS"
	level := slog.LevelInfo
	sqlState := ""
	if err != nil {
		status = "FAILED"
		level = slog.LevelError
		sqlState = telemetry.SQLState(err)
		if sqlState == "" {
			sqlState = extractSQLState(err, "")
		}
		if sqlState != "" {
			span.SetAttributes(attribute.String("sqlstate", sqlState))
		}
	}

	attrs := []slog.Attr{
		slog.String("status", status),
		slog.String("query", redactedQuery),
		slog.Float64("duration_ms", durationMs),
		slog.Int64("affected_rows", affectedRows),
		slog.String("sqlstate", sqlState),
	}
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
	}
	slog.LogAttrs(commitCtx, level, "tx.commit", attrs...)
	telemetry.FinishSpan(span, err)
	return err
}

func rollbackTxWithTelemetry(
	ctx context.Context,
	tx *sql.Tx,
	query string,
	reasonErr error,
) {
	redactedQuery := telemetry.RedactSQL(query)
	started := time.Now()
	rbCtx, span := telemetry.StartSpan(
		ctx,
		"sql.rollback",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
		),
	)

	_ = tx.Rollback()
	durationMs := float64(time.Since(started)) / float64(time.Millisecond)

	sqlState := telemetry.SQLState(reasonErr)
	if sqlState == "" && reasonErr != nil {
		sqlState = extractSQLState(reasonErr, "")
	}
	if sqlState != "" {
		span.SetAttributes(attribute.String("sqlstate", sqlState))
	}

	attrs := []slog.Attr{
		slog.String("status", "ROLLED_BACK"),
		slog.String("query", redactedQuery),
		slog.Float64("duration_ms", durationMs),
		slog.Int64("affected_rows", 0),
		slog.String("sqlstate", sqlState),
	}
	if reasonErr != nil {
		attrs = append(attrs, slog.Any("error", reasonErr))
	}
	slog.LogAttrs(rbCtx, slog.LevelWarn, "tx.rollback", attrs...)
	telemetry.FinishSpan(span, nil)
}
