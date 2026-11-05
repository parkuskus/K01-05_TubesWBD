package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/auth"
)

func TestAPIProxyCSRFRequiresExactFrontendOrigin(t *testing.T) {
	for _, origin := range []string{"https://workspace.example", "http://workspace.example", "https://attacker.example"} {
		t.Run(origin, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					_, _ = w.Write([]byte(auth.CSRFToken(r)))
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			handler, err := protectRoutes(Config{
				API:             next,
				CSRFKey:         []byte("01234567890123456789012345678901"),
				FrontendOrigins: []string{"https://workspace.example"},
			}, next)
			if err != nil {
				t.Fatal(err)
			}
			bootstrap := httptest.NewRecorder()
			handler.ServeHTTP(bootstrap, httptest.NewRequest("GET", "http://backend.internal/api/v1/csrf", nil))
			request := httptest.NewRequest("POST", "http://backend.internal/api/v1/auth/login", strings.NewReader(`{}`))
			request.Header.Set("Origin", origin)
			request.Header.Set("X-CSRF-Token", bootstrap.Body.String())
			for _, cookie := range bootstrap.Result().Cookies() {
				request.AddCookie(cookie)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if origin == "https://workspace.example" {
				if response.Code != 204 {
					t.Fatalf("trusted proxy origin rejected: %d", response.Code)
				}
			} else {
				if response.Code != 403 {
					t.Fatalf("untrusted origin accepted: %d", response.Code)
				}
				var body struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != "CSRF_INVALID" {
					t.Fatal("CSRF response was not JSON")
				}
			}
		})
	}
}
