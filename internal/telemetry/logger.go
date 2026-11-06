package telemetry

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

const redacted = "[REDACTED]"

func NewLogger(output io.Writer) *slog.Logger {
	return slog.New(&contextHandler{next: slog.NewJSONHandler(output, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.MessageKey {
				attr.Key = "event"
			}
			return attr
		},
	})})
}

type contextHandler struct{ next slog.Handler }

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	fields := ContextFields(ctx)
	if fields.RequestID == "" {
		fields.RequestID = requestID("")
	}
	clean.AddAttrs(fields.attrs()...)
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		clean.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(redactAttr(attr))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		clean[i] = redactAttr(attr)
	}
	return &contextHandler{next: h.next.WithAttrs(clean)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{next: h.next.WithGroup(name)}
}

func redactAttr(attr slog.Attr) slog.Attr {
	key := strings.ToLower(attr.Key)
	if key == "parameter_count" && attr.Value.Kind() == slog.KindInt64 {
		return attr
	}
	for _, sensitive := range []string{"password", "credential", "secret", "token", "session", "cookie", "authorization", "dsn", "database_url", "body", "payload", "header", "parameter", "args", "key"} {
		if strings.Contains(key, sensitive) {
			return slog.String(attr.Key, redacted)
		}
	}
	attr.Value = attr.Value.Resolve()
	if key == "error_code" && attr.Value.Kind() == slog.KindString {
		value := attr.Value.String()
		if len(value) <= 64 && strings.IndexFunc(value, func(r rune) bool {
			return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_')
		}) == -1 {
			return attr
		}
	}
	if attr.Value.Kind() == slog.KindGroup {
		children := attr.Value.Group()
		clean := make([]slog.Attr, len(children))
		for i, child := range children {
			clean[i] = redactAttr(child)
		}
		attr.Value = slog.GroupValue(clean...)
		return attr
	}
	if strings.Contains(key, "error") {
		if attr.Value.Kind() == slog.KindAny && attr.Value.Any() == nil {
			return slog.Attr{}
		}
		if err, ok := attr.Value.Any().(error); ok {
			return slog.String(attr.Key, safeError(err).Error())
		}
		return slog.String(attr.Key, redacted)
	}
	if key == "query" || key == "sql" || key == "executed_sql" {
		if attr.Value.Kind() != slog.KindString {
			return slog.String(attr.Key, redacted)
		}
		return slog.String(attr.Key, RedactSQL(attr.Value.String()))
	}
	if attr.Value.Kind() == slog.KindAny {
		return slog.String(attr.Key, redacted)
	}
	return attr
}
