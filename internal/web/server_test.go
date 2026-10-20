package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHomePageRenders(t *testing.T) {
	handler, _, sessions := newTestServer(t)
	sessions.authenticated = true
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/", nil))

	if recorder.Code != 200 {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "Database Management Platform") {
		t.Fatalf("response body does not contain app title: %s", recorder.Body.String())
	}
}
