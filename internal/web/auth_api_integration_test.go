package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/api"
	"if3110.local/db-platform/internal/auth"
)

func TestPostgresAuthAPI(t *testing.T) {
	db, _, _ := isolatedPostgresDB(t, "PLATFORM_TEST_DATABASE_URL")
	ctx := context.Background()
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	authenticator := auth.NewAuthenticator(db)
	if err := authenticator.EnsureInitialAdmin(ctx, "Emilio", "test-password"); err != nil {
		t.Fatal(err)
	}
	sessions, err := auth.NewSessionManager(db, auth.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	apiHandler, err := api.NewAuthHandler(api.AuthConfig{Authenticator: authenticator, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Authenticator:   authenticator,
		Sessions:        sessions,
		API:             apiHandler,
		CSRFKey:         []byte("01234567890123456789012345678901"),
		FrontendOrigins: []string{"http://localhost:3000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, handler)
	var csrfBody struct {
		Data struct {
			Token string `json:"csrf_token"`
		} `json:"data"`
	}
	bootstrap := client.get("/api/v1/csrf")
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &csrfBody); err != nil || csrfBody.Data.Token == "" {
		t.Fatal("CSRF bootstrap failed")
	}
	post := func(path, body string, token bool) *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", "http://example.com"+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://localhost:3000")
		if token {
			request.Header.Set("X-CSRF-Token", csrfBody.Data.Token)
		}
		for _, cookie := range client.cookies {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		client.saveCookies(response.Result())
		return response
	}
	assertStatus := func(response *httptest.ResponseRecorder, status int) {
		t.Helper()
		if response.Code != status || response.Header().Get("Location") != "" {
			t.Fatalf("status/location = %d / %q, want %d without redirect", response.Code, response.Header().Get("Location"), status)
		}
		if status != 204 && !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
			t.Fatal("response was not JSON")
		}
	}
	assertStatus(client.get("/api/v1/auth/me"), 401)
	assertStatus(post("/api/v1/auth/login", `{"username":"Emilio","password":"test-password"}`, false), 403)
	assertStatus(post("/api/v1/auth/login", `{"username":"Emilio","password":"wrong-password"}`, true), 401)
	var sessionCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionCount); err != nil || sessionCount != 0 {
		t.Fatal("invalid login created a session")
	}
	login := func() {
		t.Helper()
		response := post("/api/v1/auth/login", `{"username":"emilio","password":"test-password"}`, true)
		assertStatus(response, 200)
		cookie := client.cookies[auth.DefaultSessionCookie]
		if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
			t.Fatal("session cookie protections were lost")
		}
		var body struct {
			Data struct {
				User struct {
					ID, Username string
					Admin        bool `json:"is_platform_admin"`
				} `json:"user"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Data.User.ID == "" || body.Data.User.Username != "Emilio" || !body.Data.User.Admin {
			t.Fatal("invalid login identity")
		}
		if strings.Contains(response.Body.String(), "password") || strings.Contains(response.Body.String(), cookie.Value) {
			t.Fatal("login response exposed a secret")
		}
	}
	login()
	assertStatus(client.get("/api/v1/auth/me"), 200)
	oldCookie := client.cookies[auth.DefaultSessionCookie].Value
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET rotated_at = NOW() - INTERVAL '16 minutes'`); err != nil {
		t.Fatal(err)
	}
	assertStatus(client.get("/api/v1/auth/me"), 200)
	if client.cookies[auth.DefaultSessionCookie].Value == oldCookie {
		t.Fatal("API session did not rotate")
	}
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = NOW() - INTERVAL '31 minutes'`); err != nil {
		t.Fatal(err)
	}
	assertStatus(client.get("/api/v1/auth/me"), 401)
	if client.cookies[auth.DefaultSessionCookie] != nil {
		t.Fatal("expired session cookie was not cleared")
	}
	login()
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET revoked_at = NOW()`); err != nil {
		t.Fatal(err)
	}
	assertStatus(client.get("/api/v1/auth/me"), 401)
	login()
	assertStatus(post("/api/v1/auth/logout", "", false), 403)
	assertStatus(client.get("/api/v1/auth/me"), 200)
	replay := *client.cookies[auth.DefaultSessionCookie]
	assertStatus(post("/api/v1/auth/logout", "", true), 204)
	assertStatus(client.get("/api/v1/auth/me"), 401)
	client.cookies[auth.DefaultSessionCookie] = &replay
	assertStatus(client.get("/api/v1/auth/me"), 401)
}
