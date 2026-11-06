package telemetry

import (
	"bytes"
	"context"
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
)

func capture(t *testing.T) (*bytes.Buffer, *tracetest.InMemoryExporter) {
	t.Helper()
	var logs bytes.Buffer
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previousProvider, previousLogger := otel.GetTracerProvider(), slog.Default()
	otel.SetTracerProvider(provider)
	slog.SetDefault(NewLogger(&logs))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previousProvider)
		slog.SetDefault(previousLogger)
	})
	return &logs, exporter
}

func records(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid JSON log: %v", err)
		}
		result = append(result, record)
	}
	return result
}

func TestLoggerRedactsSecretsAndCorrelatesContext(t *testing.T) {
	logs, exporter := capture(t)
	ctx := WithFields(context.Background(), Fields{RequestID: requestID(""), ActorID: "7"})
	ctx = WithFields(ctx, Fields{JobID: "job-1", Attempt: 2, Batch: 3})
	ctx, span := StartSpan(ctx, "test.redaction")
	logger := slog.Default().With("password", "private-password").WithGroup("operation")
	logger.ErrorContext(ctx, "sql.failure", "query", "SELECT 'private-literal', 123 /* private-comment */",
		"parameters", []any{"private-parameter"}, "authorization", "private-auth", "error_code", "SQL_ERROR",
		"error", &pgconn.PgError{Code: "23505", Message: "private-error", Detail: "private-detail"},
		"metadata", map[string]any{"cookie": "private-map"},
		slog.Group("nested", "session_id", "private-session"))
	FinishSpan(span, errors.New("private-span-error"))
	if strings.Contains(logs.String(), "private-") {
		t.Fatal("sensitive value leaked into log")
	}
	record := records(t, logs)[0]["operation"].(map[string]any)
	if record["actor_id"] != "7" || record["job_id"] != "job-1" || record["attempt"] != float64(2) || record["batch"] != float64(3) {
		t.Fatalf("missing merged context fields: %v", record)
	}
	if record["trace_id"] != span.SpanContext().TraceID().String() || record["span_id"] != span.SpanContext().SpanID().String() {
		t.Fatal("log and span correlation differs")
	}
	if record["error_code"] != "SQL_ERROR" || !strings.Contains(record["error"].(string), "23505") {
		t.Fatal("safe failure metadata missing")
	}
	data, _ := json.Marshal(exporter.GetSpans())
	if strings.Contains(string(data), "private-") || exporter.GetSpans()[0].Status.Code != codes.Error {
		t.Fatal("span error was not safely recorded")
	}
}

func TestTraceCarrierPreservesParentAndAllowsOnlyCorrelation(t *testing.T) {
	_, exporter := capture(t)
	parent := map[string]string{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"tracestate":  "vendor=value", "x-request-id": "ab11389a-1b17-489c-9774-031d3e178005",
		"baggage": "password=private", "authorization": "private", "cookie": "private",
	}
	ctx := Extract(context.Background(), parent)
	ctx, producer := StartSpan(ctx, "queue.publish", trace.WithSpanKind(trace.SpanKindProducer))
	carrier := Inject(ctx)
	consumerCtx, consumer := StartSpan(Extract(context.Background(), carrier), "worker.attempt", trace.WithSpanKind(trace.SpanKindConsumer))
	_, query := StartSpan(consumerCtx, "sql.query", trace.WithSpanKind(trace.SpanKindClient))
	query.End()
	consumer.End()
	producer.End()
	for key := range carrier {
		if key != "traceparent" && key != "tracestate" && key != "x-request-id" {
			t.Fatalf("unexpected carrier field %q", key)
		}
	}
	if len(carrier) != 3 || ContextFields(consumerCtx).RequestID != parent["x-request-id"] {
		t.Fatal("carrier lost trace state or request ID")
	}
	spans := exporter.GetSpans()
	if spans[0].Parent.SpanID() != consumer.SpanContext().SpanID() || spans[1].Parent.SpanID() != producer.SpanContext().SpanID() || spans[2].Parent.SpanID().String() != "00f067aa0ba902b7" {
		t.Fatal("API/queue/worker/query parent chain broke")
	}
	for _, span := range spans {
		if span.SpanContext.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Fatal("trace ID changed during propagation")
		}
	}
	invalid := Extract(context.Background(), map[string]string{"traceparent": "invalid", "x-request-id": "private-session"})
	if trace.SpanContextFromContext(invalid).IsValid() || ContextFields(invalid).RequestID == "private-session" {
		t.Fatal("invalid trace or request identifier accepted")
	}
}

func TestSetupExportsConsoleSpansOnShutdown(t *testing.T) {
	var logs, traces bytes.Buffer
	previousProvider, previousLogger, previousPropagator, previousErrors := otel.GetTracerProvider(), slog.Default(), otel.GetTextMapPropagator(), otel.GetErrorHandler()
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		slog.SetDefault(previousLogger)
		otel.SetTextMapPropagator(previousPropagator)
		otel.SetErrorHandler(previousErrors)
	})
	shutdown, err := Setup("telemetry-test", &logs, &traces)
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := StartSpan(Extract(context.Background(), nil), "test.console")
	slog.InfoContext(ctx, "test.console", "status", "SUCCESS")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if records(t, &logs)[0]["event"] != "test.console" || !json.Valid(traces.Bytes()) || !strings.Contains(traces.String(), "test.console") {
		t.Fatal("logger or console exporter did not flush")
	}
}
