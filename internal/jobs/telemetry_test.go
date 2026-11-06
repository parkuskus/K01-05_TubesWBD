package jobs

import (
	"context"
	"database/sql"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"if3110.local/db-platform/internal/telemetry"
)

func TestPublishPersistsCurrentTraceAndDropsUnrelatedCarrierData(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	db := sql.OpenDB(fakeJobsConnector{db: &fakeJobsDB{}})
	defer db.Close()
	ctx := telemetry.Extract(context.Background(), nil)
	ctx, apiSpan := telemetry.StartSpan(ctx, "http.request")
	defer apiSpan.End()
	store := NewPostgresJobStore(db)
	job, err := store.Publish(ctx, PublishInput{Plan: sampleExecutionPlan(), IdempotencyKey: "trace-test",
		TraceCarrier: map[string]string{"cookie": "private-cookie", "baggage": "private-baggage"}})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewPostgresJobStore(db).GetByID(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := trace.SpanContextFromContext(telemetry.Extract(context.Background(), reloaded.TraceCarrier))
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Name != "queue.publish" || spans[0].Parent.SpanID() != apiSpan.SpanContext().SpanID() || parent.SpanID() != spans[0].SpanContext.SpanID() || parent.TraceID() != apiSpan.SpanContext().TraceID() {
		t.Fatal("durable queue did not preserve API to producer trace")
	}
	if len(reloaded.TraceCarrier) != 2 || reloaded.TraceCarrier["x-request-id"] != telemetry.ContextFields(ctx).RequestID {
		t.Fatal("queue carrier leaked unrelated data or lost correlation")
	}
}
