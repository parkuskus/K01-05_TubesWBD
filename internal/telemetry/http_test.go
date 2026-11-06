package telemetry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func TestHTTPTracesStatusAndOmitsSensitiveRequestData(t *testing.T) {
	for _, status := range []int{200, 204, 401, 403, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			logs, exporter := capture(t)
			handler := HTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !trace.SpanContextFromContext(r.Context()).IsValid() {
					t.Fatal("request has no span")
				}
				w.WriteHeader(status)
			}))
			r := httptest.NewRequest("POST", "/private-path?password=private-query", strings.NewReader("private-body"))
			r.Header.Set("Authorization", "private-authorization")
			r.Header.Set("Cookie", "session_id=private-cookie")
			r.Header.Set("X-Request-ID", "invalid-private-id")
			r.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != status || w.Header().Get("X-Request-ID") == "" {
				t.Fatal("response status or correlation header changed")
			}
			entries := records(t, logs)
			if len(entries) != 2 || entries[1]["status"] != float64(status) || entries[1]["request_id"] != w.Header().Get("X-Request-ID") {
				t.Fatalf("request result missing: %v", entries)
			}
			if strings.Contains(logs.String(), "private-") {
				t.Fatal("request secrets leaked")
			}
			span := exporter.GetSpans()[0]
			if span.Parent.SpanID().String() != "00f067aa0ba902b7" || (span.Status.Code == codes.Error) != (status >= 500) {
				t.Fatal("HTTP trace parent or error status is incorrect")
			}
		})
	}
}

func TestHTTPPreservesValidRequestIDAndReportsPanicsSafely(t *testing.T) {
	logs, exporter := capture(t)
	handler := HTTP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("private-panic") }))
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-ID", "ab11389a-1b17-489c-9774-031d3e178005")
	w := httptest.NewRecorder()
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Fatal("panic was swallowed or would expose sensitive panic text")
			}
		}()
		handler.ServeHTTP(w, r)
	}()
	if w.Header().Get("X-Request-ID") != r.Header.Get("X-Request-ID") || records(t, logs)[1]["status"] != float64(500) || strings.Contains(logs.String(), "private-panic") || exporter.GetSpans()[0].Status.Code != codes.Error {
		t.Fatal("panic status or correlation was not recorded safely")
	}
}

func TestHTTPWriterSupportsStreamingAndRepeatedHeaders(t *testing.T) {
	logs, _ := capture(t)
	handler := HTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
		w.WriteHeader(http.StatusInternalServerError)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Fatal(err)
		}
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	entry := records(t, logs)[1]
	if entry["status"] != float64(200) || entry["response_bytes"] != float64(2) {
		t.Fatalf("streaming or repeated headers produced an incorrect result: %v", entry)
	}
}
