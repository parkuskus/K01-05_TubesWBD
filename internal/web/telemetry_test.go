package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/api"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
)

type telemetryAuthenticator struct{ fakeAuthenticator }

func (*telemetryAuthenticator) Identity(context.Context, contracts.UserID) (auth.Identity, error) {
	return auth.Identity{}, auth.ErrUnauthenticated
}

type telemetrySessions struct{ fakeSessions }

func (*telemetrySessions) RequireSessionWithError(_ http.Handler, onError func(http.ResponseWriter, *http.Request, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { onError(w, r, auth.ErrUnauthenticated) })
}

func TestCorrelationCoversAPIRejectionsAndResponses(t *testing.T) {
	for _, tc := range []struct {
		method string
		path   string
		status int
	}{
		{"GET", "/api/v1/unknown", 404},
		{"POST", "/api/v1/auth/login", 403},
		{"GET", "/api/v1/auth/me", 401},
		{"GET", "/api/v1/csrf", 200},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			authAPI, err := api.NewAuthHandler(api.AuthConfig{Authenticator: &telemetryAuthenticator{}, Sessions: &telemetrySessions{}})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := New(Config{Authenticator: &fakeAuthenticator{}, Sessions: &fakeSessions{},
				CSRFKey: bytes.Repeat([]byte{1}, 32), API: authAPI})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			request.Header.Set("X-Request-ID", "ab11389a-1b17-489c-9774-031d3e178005")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var envelope map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if response.Code != tc.status || envelope["request_id"] != response.Header().Get("X-Request-ID") || envelope["request_id"] != request.Header.Get("X-Request-ID") {
				t.Fatalf("missing API correlation on status %d", response.Code)
			}
		})
	}
}
