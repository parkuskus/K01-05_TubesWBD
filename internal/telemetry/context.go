package telemetry

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Fields struct {
	RequestID   string
	ActorID     string
	DatabaseID  string
	MigrationID string
	RevisionID  string
	JobID       string
	ExecutionID string
	AttemptID   string
	Direction   string
	Attempt     int
	Batch       int
}

type fieldsKey struct{}

func WithFields(ctx context.Context, fields Fields) context.Context {
	previous := ContextFields(ctx)
	merge := func(dst *string, value string) {
		if value != "" {
			*dst = value
		}
	}
	merge(&previous.RequestID, fields.RequestID)
	merge(&previous.ActorID, fields.ActorID)
	merge(&previous.DatabaseID, fields.DatabaseID)
	merge(&previous.MigrationID, fields.MigrationID)
	merge(&previous.RevisionID, fields.RevisionID)
	merge(&previous.JobID, fields.JobID)
	merge(&previous.ExecutionID, fields.ExecutionID)
	merge(&previous.AttemptID, fields.AttemptID)
	merge(&previous.Direction, fields.Direction)
	if fields.Attempt > 0 {
		previous.Attempt = fields.Attempt
	}
	if fields.Batch > 0 {
		previous.Batch = fields.Batch
	}
	return context.WithValue(ctx, fieldsKey{}, previous)
}

func ContextFields(ctx context.Context) Fields {
	fields, _ := ctx.Value(fieldsKey{}).(Fields)
	return fields
}

func (f Fields) attrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 11)
	for _, field := range []struct{ key, value string }{
		{"request_id", f.RequestID}, {"actor_id", f.ActorID}, {"database_id", f.DatabaseID},
		{"migration_id", f.MigrationID}, {"revision_id", f.RevisionID}, {"job_id", f.JobID},
		{"execution_id", f.ExecutionID}, {"attempt_id", f.AttemptID}, {"direction", f.Direction},
	} {
		if field.value != "" {
			attrs = append(attrs, slog.String(field.key, field.value))
		}
	}
	if f.Attempt > 0 {
		attrs = append(attrs, slog.Int("attempt", f.Attempt))
	}
	if f.Batch > 0 {
		attrs = append(attrs, slog.Int("batch", f.Batch))
	}
	return attrs
}

func requestID(value string) string {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return uuid.NewString()
	}
	return id.String()
}

func Inject(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	if id := ContextFields(ctx).RequestID; id != "" {
		carrier["x-request-id"] = requestID(id)
	}
	return carrier
}

func Extract(ctx context.Context, carrier map[string]string) context.Context {
	ctx = propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier(carrier))
	return WithFields(ctx, Fields{RequestID: requestID(carrier["x-request-id"])})
}

func StartSpan(ctx context.Context, name string, options ...trace.SpanStartOption) (context.Context, trace.Span) {
	attrs := make([]attribute.KeyValue, 0, 11)
	for _, attr := range ContextFields(ctx).attrs() {
		if attr.Value.Kind() == slog.KindInt64 {
			attrs = append(attrs, attribute.Int64(attr.Key, attr.Value.Int64()))
		} else {
			attrs = append(attrs, attribute.String(attr.Key, attr.Value.String()))
		}
	}
	options = append(options, trace.WithAttributes(attrs...))
	return otel.Tracer("if3110.local/db-platform").Start(ctx, name, options...)
}

func FinishSpan(span trace.Span, err error) {
	RecordError(span, err)
	span.End()
}

func RecordError(span trace.Span, err error) {
	if err != nil {
		safe := safeError(err)
		span.RecordError(safe)
		span.SetStatus(codes.Error, safe.Error())
	}
}
