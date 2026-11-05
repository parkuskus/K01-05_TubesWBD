package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/csrf"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
)

type testAuthenticator struct {
	calls int
	err   error
}

func (a *testAuthenticator) AuthenticateForSession(_ context.Context, username, password string) (auth.LoginProof, error) {
	a.calls++
	if a.err != nil {
		return auth.LoginProof{}, a.err
	}
	if username != "Emilio" || password != "correct-password" {
		return auth.LoginProof{}, auth.ErrInvalidCredentials
	}
	return auth.LoginProof{UserID: "9007199254740993"}, nil
}

func (a *testAuthenticator) Identity(_ context.Context, id contracts.UserID) (auth.Identity, error) {
	return auth.Identity{ID: id, Username: "Emilio", IsPlatformAdmin: true}, nil
}

type testSessions struct {
	user      contracts.UserID
	createErr error
	logoutErr error
	logouts   int
}

func (s *testSessions) Create(_ http.ResponseWriter, _ *http.Request, proof auth.LoginProof) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.user = proof.UserID
	return nil
}

func (s *testSessions) Logout(http.ResponseWriter, *http.Request) error {
	s.logouts++
	if s.logoutErr != nil {
		return s.logoutErr
	}
	s.user = ""
	return nil
}

func (s *testSessions) RequireSessionWithError(next http.Handler, onError func(http.ResponseWriter, *http.Request, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.user == "" {
			onError(w, r, auth.ErrUnauthenticated)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), s.user)))
	})
}

type authTestClient struct {
	handler http.Handler
	cookies map[string]*http.Cookie
	token   string
}

func newAuthTestClient(t *testing.T) (*authTestClient, *testAuthenticator, *testSessions) {
	t.Helper()
	authenticator, sessions := &testAuthenticator{}, &testSessions{}
	handler, err := NewAuthHandler(AuthConfig{Authenticator: authenticator, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	handler, err = auth.CSRFProtection([]byte("01234567890123456789012345678901"), false, handler,
		csrf.ErrorHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			WriteError(w, http.StatusForbidden, "CSRF_INVALID", "Sesi formulir tidak valid.")
		})))
	if err != nil {
		t.Fatal(err)
	}
	client := &authTestClient{handler: handler, cookies: make(map[string]*http.Cookie)}
	response := client.request(http.MethodGet, "/api/v1/csrf", "", "", false)
	var body struct {
		Data struct {
			Token string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Data.Token == "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("CSRF bootstrap failed")
	}
	client.token = body.Data.Token
	return client, authenticator, sessions
}

func (c *authTestClient) request(method, path, body, origin string, token bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if token {
		req.Header.Set("X-CSRF-Token", c.token)
	}
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	c.handler.ServeHTTP(response, req)
	for _, cookie := range response.Result().Cookies() {
		c.cookies[cookie.Name] = cookie
	}
	return response
}

func assertAPIStatus(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status || response.Header().Get("Location") != "" || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status/type/location = %d / %q / %q", response.Code, response.Header().Get("Content-Type"), response.Header().Get("Location"))
	}
	var body struct {
		Error errorDetail `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != code {
		t.Fatalf("error code = %q, want %q", body.Error.Code, code)
	}
}

func TestAuthAPILoginSessionAndLogout(t *testing.T) {
	client, _, sessions := newAuthTestClient(t)
	assertAPIStatus(t, client.request("GET", "/api/v1/auth/me", "", "", false), 401, "UNAUTHENTICATED")
	wrong := client.request("POST", "/api/v1/auth/login", `{"username":"private-user","password":"private-password"}`, "http://example.com", true)
	assertAPIStatus(t, wrong, 401, "INVALID_CREDENTIALS")
	if strings.Contains(wrong.Body.String(), "private-") {
		t.Fatal("credentials leaked in login error")
	}
	response := client.request("POST", "/api/v1/auth/login", `{"username":" Emilio ","password":"correct-password"}`, "http://example.com", true)
	if response.Code != 200 || sessions.user != "9007199254740993" {
		t.Fatalf("login status = %d", response.Code)
	}
	for _, response := range []*httptest.ResponseRecorder{response, client.request("GET", "/api/v1/auth/me", "", "", false)} {
		var body struct {
			Data struct {
				User sessionUser `json:"user"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Data.User.ID != "9007199254740993" || body.Data.User.Username != "Emilio" || !body.Data.User.IsPlatformAdmin {
			t.Fatal("invalid session identity")
		}
		if strings.Contains(response.Body.String(), "password") || strings.Contains(response.Body.String(), "session_id") {
			t.Fatal("session response exposed a secret field")
		}
	}
	assertAPIStatus(t, client.request("POST", "/api/v1/auth/logout", "", "http://example.com", false), 403, "CSRF_INVALID")
	if sessions.logouts != 0 {
		t.Fatal("logout bypassed CSRF")
	}
	response = client.request("POST", "/api/v1/auth/logout", "", "http://example.com", true)
	if response.Code != 204 || response.Body.Len() != 0 || sessions.logouts != 1 {
		t.Fatal("logout did not end session")
	}
	assertAPIStatus(t, client.request("GET", "/api/v1/auth/me", "", "", false), 401, "UNAUTHENTICATED")
}

func TestAuthAPIRejectsCSRFAndUntrustedOrigin(t *testing.T) {
	for _, test := range []struct {
		name, origin string
		token        bool
	}{
		{"missing", "http://example.com", false},
		{"origin", "https://attacker.example", true},
		{"invalid", "http://example.com", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, authenticator, _ := newAuthTestClient(t)
			if test.name == "invalid" {
				client.token = "invalid-token"
			}
			response := client.request("POST", "/api/v1/auth/login", `{"username":"Emilio","password":"correct-password"}`, test.origin, test.token)
			assertAPIStatus(t, response, 403, "CSRF_INVALID")
			if authenticator.calls != 0 {
				t.Fatal("authentication ran before CSRF validation")
			}
		})
	}
}

func TestAuthAPIRejectsMalformedAndOversizedLogin(t *testing.T) {
	for _, test := range []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"malformed", "/api/v1/auth/login", `{`, 400, "INVALID_REQUEST"},
		{"unknown", "/api/v1/auth/login", `{"username":"Emilio","password":"correct-password","admin":true}`, 400, "INVALID_REQUEST"},
		{"trailing", "/api/v1/auth/login", `{} {}`, 400, "INVALID_REQUEST"},
		{"null", "/api/v1/auth/login", `null`, 400, "INVALID_REQUEST"},
		{"fields", "/api/v1/auth/login", `{}`, 422, "VALIDATION_FAILED"},
		{"password limit", "/api/v1/auth/login", `{"username":"Emilio","password":"` + strings.Repeat("é", 37) + `"}`, 422, "VALIDATION_FAILED"},
		{"query", "/api/v1/auth/login?password=private-password", `{}`, 400, "INVALID_REQUEST"},
		{"size", "/api/v1/auth/login", `{"username":"` + strings.Repeat("x", MaxAuthBodyBytes) + `","password":"private-password"}`, 413, "REQUEST_TOO_LARGE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, authenticator, _ := newAuthTestClient(t)
			response := client.request("POST", test.path, test.body, "http://example.com", true)
			assertAPIStatus(t, response, test.status, test.code)
			if authenticator.calls != 0 || strings.Contains(response.Body.String(), "private-password") {
				t.Fatal("invalid input reached authentication or leaked")
			}
		})
	}
}

func TestAuthAPIRoutesAndServiceErrorsStayJSON(t *testing.T) {
	client, authenticator, sessions := newAuthTestClient(t)
	assertAPIStatus(t, client.request("GET", "/api/v1/missing", "", "", false), 404, "NOT_FOUND")
	response := client.request("GET", "/api/v1/auth/login", "", "", false)
	assertAPIStatus(t, response, 405, "METHOD_NOT_ALLOWED")
	if response.Header().Get("Allow") != "POST" {
		t.Fatal("missing Allow header")
	}
	authenticator.err = errors.New("postgres://private-credential@example.invalid/db")
	response = client.request("POST", "/api/v1/auth/login", `{"username":"Emilio","password":"correct-password"}`, "http://example.com", true)
	assertAPIStatus(t, response, 503, "AUTH_UNAVAILABLE")
	if strings.Contains(response.Body.String(), "private-credential") {
		t.Fatal("service error leaked")
	}
	authenticator.err = nil
	sessions.createErr = auth.ErrUnauthenticated
	assertAPIStatus(t, client.request("POST", "/api/v1/auth/login", `{"username":"Emilio","password":"correct-password"}`, "http://example.com", true), 401, "UNAUTHENTICATED")
	if sessions.user != "" {
		t.Fatal("stale proof created a session")
	}
}
