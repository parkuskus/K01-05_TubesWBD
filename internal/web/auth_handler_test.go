package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
)

type fakeAuthenticator struct {
	username string
	password string
	userID   contracts.UserID
	called   int
}

func (a *fakeAuthenticator) AuthenticateForSession(_ context.Context, username, password string) (auth.LoginProof, error) {
	a.called++
	if username != a.username || password != a.password {
		return auth.LoginProof{}, auth.ErrInvalidCredentials
	}
	return auth.LoginProof{UserID: a.userID}, nil
}

type fakeSessions struct {
	authenticated bool
	createdFor    contracts.UserID
	logoutCalls   int
	createErr     error
}

func (s *fakeSessions) Create(_ http.ResponseWriter, _ *http.Request, proof auth.LoginProof) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.authenticated = true
	s.createdFor = proof.UserID
	return nil
}

func TestStaleLoginProofDoesNotRedirectOrIssueSession(t *testing.T) {
	handler, _, sessions := newTestServer(t)
	sessions.createErr = auth.ErrUnauthenticated
	csrfCookies, token := getCSRFToken(t, handler, "/login")
	response := postForm(handler, "/login", url.Values{
		"username": {"admin"}, "password": {"secret-password"}, "gorilla.csrf.Token": {token},
	}, csrfCookies)
	if response.Code != http.StatusUnauthorized || sessions.authenticated || response.Header().Get("Location") != "" {
		t.Fatalf("stale login = status %d, authenticated %t, location %q", response.Code, sessions.authenticated, response.Header().Get("Location"))
	}
}

func (s *fakeSessions) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *fakeSessions) Logout(_ http.ResponseWriter, _ *http.Request) error {
	s.logoutCalls++
	s.authenticated = false
	return nil
}

func newTestServer(t *testing.T) (http.Handler, *fakeAuthenticator, *fakeSessions) {
	t.Helper()
	authenticator := &fakeAuthenticator{username: "admin", password: "secret-password", userID: "42"}
	sessions := &fakeSessions{}
	handler, err := New(Config{
		Authenticator: authenticator,
		Sessions:      sessions,
		CSRFKey:       []byte("01234567890123456789012345678901"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, authenticator, sessions
}

func TestLoginPageProvidesUsernamePasswordAndCSRFToken(t *testing.T) {
	handler, _, _ := newTestServer(t)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.com/login", nil))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `name="username"`) || !strings.Contains(body, `name="password"`) {
		t.Fatalf("login page status/body = %d/%s", response.Code, body)
	}
	if !strings.Contains(body, `name="gorilla.csrf.Token" value="`) {
		t.Fatalf("login page is missing its CSRF token: %s", body)
	}
}

func TestInvalidLoginDoesNotEchoSubmittedCredentials(t *testing.T) {
	handler, authenticator, _ := newTestServer(t)
	csrfCookies, token := getCSRFToken(t, handler, "/login")
	response := postForm(handler, "/login", url.Values{
		"username":           {"private-user"},
		"password":           {"private-password"},
		"gorilla.csrf.Token": {token},
	}, csrfCookies)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid login status = %d, want 401", response.Code)
	}
	if strings.Contains(response.Body.String(), "private-user") || strings.Contains(response.Body.String(), "private-password") {
		t.Fatalf("login response echoed submitted credentials: %s", response.Body.String())
	}
	if authenticator.called != 1 {
		t.Fatalf("Authenticate calls = %d, want 1", authenticator.called)
	}
}

func TestSuccessfulLoginCreatesSessionAndRedirects(t *testing.T) {
	handler, _, sessions := newTestServer(t)
	csrfCookies, token := getCSRFToken(t, handler, "/login")
	response := postForm(handler, "/login", url.Values{
		"username":           {"admin"},
		"password":           {"secret-password"},
		"gorilla.csrf.Token": {token},
	}, csrfCookies)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
		t.Fatalf("successful login response = %d, location %q; want 303 to /", response.Code, response.Header().Get("Location"))
	}
	if !sessions.authenticated || sessions.createdFor != "42" {
		t.Fatalf("session after login = authenticated %t, user %q", sessions.authenticated, sessions.createdFor)
	}
}

func TestProtectedHomeAndLogoutRequireSessionAndCSRF(t *testing.T) {
	handler, _, sessions := newTestServer(t)
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous home status = %d, want 401", unauthorized.Code)
	}

	sessions.authenticated = true
	csrfCookies, token := getCSRFToken(t, handler, "/login")
	missingCSRF := postForm(handler, "/logout", url.Values{}, csrfCookies)
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF status = %d, want 403", missingCSRF.Code)
	}
	if sessions.logoutCalls != 0 {
		t.Fatal("logout ran without a valid CSRF token")
	}

	logout := postForm(handler, "/logout", url.Values{"gorilla.csrf.Token": {token}}, csrfCookies)
	if logout.Code != http.StatusSeeOther || logout.Header().Get("Location") != "/login" {
		t.Fatalf("logout response = %d, location %q; want 303 to /login", logout.Code, logout.Header().Get("Location"))
	}
	if sessions.authenticated || sessions.logoutCalls != 1 {
		t.Fatalf("logout state = authenticated %t, calls %d", sessions.authenticated, sessions.logoutCalls)
	}
}

func TestLoginPostWithoutCSRFDoesNotAuthenticate(t *testing.T) {
	handler, authenticator, _ := newTestServer(t)
	response := postForm(handler, "/login", url.Values{
		"username": {"admin"},
		"password": {"secret-password"},
	}, nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("login without CSRF status = %d, want 403", response.Code)
	}
	if authenticator.called != 0 {
		t.Fatal("login authenticated before CSRF validation")
	}
}

func TestLoginRejectsCredentialsInURL(t *testing.T) {
	handler, authenticator, _ := newTestServer(t)
	csrfCookies, token := getCSRFToken(t, handler, "/login")
	response := postForm(handler, "/login?username=admin&password=secret-password", url.Values{
		"gorilla.csrf.Token": {token},
	}, csrfCookies)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("login with credentials in URL status = %d, want 400", response.Code)
	}
	if authenticator.called != 0 {
		t.Fatal("login accepted credentials from the URL")
	}
}

func getCSRFToken(t *testing.T, handler http.Handler, path string) ([]*http.Cookie, string) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil))
	match := regexp.MustCompile(`name="gorilla.csrf.Token" value="([^"]+)"`).FindStringSubmatch(response.Body.String())
	if len(match) != 2 {
		t.Fatalf("CSRF token not found in response: %s", response.Body.String())
	}
	return response.Result().Cookies(), match[1]
}

func postForm(handler http.Handler, path string, form url.Values, cookies []*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://example.com"+path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
