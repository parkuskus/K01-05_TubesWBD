package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCSRFProtectionAllowsSafeRequestAndRequiresTokenForLoginPost(t *testing.T) {
	key := make([]byte, 32)
	var token string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = CSRFToken(r)
		w.WriteHeader(http.StatusNoContent)
	})
	protected, err := CSRFProtection(key, false, next)
	if err != nil {
		t.Fatal(err)
	}

	getResponse := httptest.NewRecorder()
	protected.ServeHTTP(getResponse, httptest.NewRequest(http.MethodGet, "/login", nil))
	if getResponse.Code != http.StatusNoContent || token == "" {
		t.Fatalf("safe GET status/token = %d/%q, want 204/non-empty", getResponse.Code, token)
	}

	postWithoutToken := httptest.NewRequest(http.MethodPost, "/login", nil)
	postWithoutToken.Header.Set("Origin", "http://example.com")
	for _, cookie := range getResponse.Result().Cookies() {
		postWithoutToken.AddCookie(cookie)
	}
	postResponse := httptest.NewRecorder()
	protected.ServeHTTP(postResponse, postWithoutToken)
	if postResponse.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF token status = %d, want 403", postResponse.Code)
	}

	validPost := httptest.NewRequest(http.MethodPost, "/login", nil)
	for _, cookie := range getResponse.Result().Cookies() {
		validPost.AddCookie(cookie)
	}
	validPost.Header.Set("Origin", "http://example.com")
	validPost.Header.Set("X-CSRF-Token", token)
	validResponse := httptest.NewRecorder()
	protected.ServeHTTP(validResponse, validPost)
	if validResponse.Code != http.StatusNoContent {
		t.Fatalf("POST with CSRF token status = %d, want 204; response: %s", validResponse.Code, validResponse.Body.String())
	}
}

func TestCSRFProtectionChecksOriginAndToken(t *testing.T) {
	for _, test := range []struct {
		name      string
		secure    bool
		url       string
		origin    string
		tokenMode string
		want      int
	}{
		{"local HTTP", false, "http://localhost:8080/login", "http://localhost:8080", "valid", http.StatusNoContent},
		{"foreign origin", false, "http://localhost:8080/login", "http://attacker.example", "valid", http.StatusForbidden},
		{"different port", false, "http://localhost:8080/login", "http://localhost:8081", "valid", http.StatusForbidden},
		{"missing token", false, "http://localhost:8080/login", "http://localhost:8080", "missing", http.StatusForbidden},
		{"invalid token", false, "http://localhost:8080/login", "http://localhost:8080", "invalid", http.StatusForbidden},
		{"HTTPS", true, "https://example.com/login", "https://example.com", "valid", http.StatusNoContent},
		{"HTTPS rejects HTTP origin", true, "https://example.com/login", "http://example.com", "valid", http.StatusForbidden},
		{"TLS with insecure cookies", false, "https://example.com/login", "https://example.com", "valid", http.StatusNoContent},
		{"TLS rejects HTTP origin with insecure cookies", false, "https://example.com/login", "http://example.com", "valid", http.StatusForbidden},
		{"HTTPS behind proxy", true, "http://example.com/login", "https://example.com", "valid", http.StatusNoContent},
		{"proxy rejects HTTP origin", true, "http://example.com/login", "http://example.com", "valid", http.StatusForbidden},
		{"HTTPS requires origin or referer", true, "http://example.com/login", "", "valid", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			var token string
			protected, err := CSRFProtection(make([]byte, 32), test.secure, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				token = CSRFToken(r)
				w.WriteHeader(http.StatusNoContent)
			}))
			if err != nil {
				t.Fatal(err)
			}
			get := httptest.NewRecorder()
			protected.ServeHTTP(get, httptest.NewRequest(http.MethodGet, test.url, nil))
			if get.Code != http.StatusNoContent || token == "" {
				t.Fatal("GET did not issue a CSRF token")
			}
			request := httptest.NewRequest(http.MethodPost, test.url, nil)
			request.Header.Set("Origin", test.origin)
			for _, cookie := range get.Result().Cookies() {
				request.AddCookie(cookie)
			}
			switch test.tokenMode {
			case "valid":
				request.Header.Set("X-CSRF-Token", token)
			case "invalid":
				request.Header.Set("X-CSRF-Token", "invalid-token")
			}
			response := httptest.NewRecorder()
			protected.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("POST status = %d, want %d; body: %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestCSRFProtectionRejectsInvalidKey(t *testing.T) {
	if _, err := CSRFProtection([]byte("too short"), false, http.NotFoundHandler()); err == nil {
		t.Fatal("CSRF protection accepted a short authentication key")
	}
}
