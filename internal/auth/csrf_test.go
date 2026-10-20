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
	validPost.Header.Set("Origin", "https://example.com")
	validPost.Header.Set("X-CSRF-Token", token)
	validResponse := httptest.NewRecorder()
	protected.ServeHTTP(validResponse, validPost)
	if validResponse.Code != http.StatusNoContent {
		t.Fatalf("POST with CSRF token status = %d, want 204; response: %s", validResponse.Code, validResponse.Body.String())
	}
}

func TestCSRFProtectionRejectsInvalidKey(t *testing.T) {
	if _, err := CSRFProtection([]byte("too short"), false, http.NotFoundHandler()); err == nil {
		t.Fatal("CSRF protection accepted a short authentication key")
	}
}
