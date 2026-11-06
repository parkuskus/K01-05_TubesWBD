package telemetry

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func HTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx = WithFields(ctx, Fields{RequestID: requestID(r.Header.Get("X-Request-ID"))})
		ctx, span := StartSpan(ctx, "http.request", trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method)))
		w.Header().Set("X-Request-ID", ContextFields(ctx).RequestID)
		writer := &responseWriter{ResponseWriter: w}
		started := time.Now()
		slog.InfoContext(ctx, "http.request.start", "method", r.Method, "status", "STARTED")
		defer func() {
			recovered := recover()
			status := writer.status
			if status == 0 {
				status = http.StatusOK
			}
			if recovered != nil {
				status = http.StatusInternalServerError
			}
			span.SetAttributes(attribute.Int("http.response.status_code", status))
			level := slog.LevelInfo
			if status >= 500 {
				level = slog.LevelError
				span.SetStatus(codes.Error, "HTTP server error")
			} else if status >= 400 {
				level = slog.LevelWarn
			}
			slog.LogAttrs(ctx, level, "http.request.result", slog.String("method", r.Method),
				slog.Int("status", status), slog.Int("response_bytes", writer.bytes),
				slog.Float64("duration_ms", float64(time.Since(started))/float64(time.Millisecond)))
			span.End()
			if recovered != nil {
				panic(http.ErrAbortHandler)
			}
		}()
		next.ServeHTTP(writer, r.WithContext(ctx))
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	w.bytes += n
	return n, err
}

func (w *responseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		return fmt.Errorf("flush response: %w", err)
	}
	return nil
}
