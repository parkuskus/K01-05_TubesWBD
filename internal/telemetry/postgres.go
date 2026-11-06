package telemetry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func OpenPlatform(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid platform database configuration")
	}
	cfg.Tracer = &QueryTracer{DatabaseID: "platform", Role: "platform"}
	return stdlib.OpenDB(*cfg), nil
}

type QueryTracer struct {
	DatabaseID string
	Role       string
}

type queryKey struct{}

type queryState struct {
	started time.Time
	event   string
	span    trace.Span
}

func (t *QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx = WithFields(ctx, Fields{DatabaseID: t.DatabaseID})
	if ContextFields(ctx).RequestID == "" {
		ctx = WithFields(ctx, Fields{RequestID: requestID("")})
	}
	query := RedactSQL(data.SQL)
	event := "sql.query"
	words := strings.Fields(query)
	if len(words) > 0 {
		switch strings.ToUpper(words[0]) {
		case "BEGIN", "START":
			event = "sql.transaction.begin"
		case "COMMIT", "END":
			event = "sql.transaction.commit"
		case "ROLLBACK", "ABORT":
			event = "sql.transaction.rollback"
		}
	}
	count := 0
	for _, arg := range data.Args {
		switch arg.(type) {
		case pgx.QueryExecMode, pgx.QueryResultFormats, pgx.QueryResultFormatsByOID:
		default:
			count++
		}
	}
	sum := sha256.Sum256([]byte(data.SQL))
	checksum := hex.EncodeToString(sum[:])
	ctx, span := StartSpan(ctx, event, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("db.system.name", "postgresql"), attribute.String("db.role", t.Role),
		attribute.String("db.query.text", query), attribute.String("query_checksum", checksum),
		attribute.Int("parameter_count", count),
	))
	slog.LogAttrs(ctx, slog.LevelInfo, event+".start",
		slog.String("status", "STARTED"), slog.String("db_role", t.Role),
		slog.String("query", query), slog.String("query_checksum", checksum),
		slog.String("parameters", redacted), slog.Int("parameter_count", count),
	)
	return context.WithValue(ctx, queryKey{}, queryState{started: time.Now(), event: event, span: span})
}

func (t *QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	state, ok := ctx.Value(queryKey{}).(queryState)
	if !ok {
		return
	}
	err := data.Err
	if state.event == "sql.transaction.commit" && data.CommandTag.String() == "ROLLBACK" && err == nil {
		err = pgx.ErrTxCommitRollback
	}
	status, level := "SUCCESS", slog.LevelInfo
	if err != nil {
		status, level = "FAILED", slog.LevelError
	}
	attrs := []slog.Attr{
		slog.String("status", status), slog.String("db_role", t.Role),
		slog.Float64("duration_ms", float64(time.Since(state.started))/float64(time.Millisecond)),
	}
	if data.CommandTag.String() != "" {
		attrs = append(attrs, slog.Int64("rows_affected", data.CommandTag.RowsAffected()))
		state.span.SetAttributes(attribute.Int64("db.response.returned_rows", data.CommandTag.RowsAffected()))
	}
	if err != nil {
		attrs = append(attrs, slog.Any("error", err), slog.String("sqlstate", SQLState(err)))
		state.span.SetAttributes(attribute.String("db.response.status_code", SQLState(err)))
	}
	slog.LogAttrs(ctx, level, state.event+".result", attrs...)
	FinishSpan(state.span, err)
}
