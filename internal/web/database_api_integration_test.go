package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/api"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/database"
	"if3110.local/db-platform/internal/platform"
)

func TestPostgresDatabaseAPI(t *testing.T) {
	db, _, _ := isolatedPostgresDB(t, "PLATFORM_TEST_DATABASE_URL")
	ctx := context.Background()
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	authenticator := auth.NewAuthenticator(db)
	if err := authenticator.EnsureInitialAdmin(ctx, "Admin", "first-password"); err != nil {
		t.Fatal(err)
	}
	sessions, err := auth.NewSessionManager(db, auth.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	service := platform.NewService(db)
	manager, err := database.NewManager(db, service, bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	viewer, err := database.NewViewer(service, manager)
	if err != nil {
		t.Fatal(err)
	}
	apiHandler, err := api.NewDatabaseHandler(manager, viewer, service, sessions)
	if err != nil {
		t.Fatal(err)
	}
	authHandler, err := api.NewAuthHandler(api.AuthConfig{Authenticator: authenticator, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	routes := http.NewServeMux()
	routes.Handle("/api/v1/connections", apiHandler)
	routes.Handle("/api/v1/connections/", apiHandler)
	routes.Handle("/api/v1/databases", apiHandler)
	routes.Handle("/api/v1/databases/", apiHandler)
	routes.Handle("/api/", authHandler)
	handler, err := New(Config{Authenticator: authenticator, Sessions: sessions, API: routes, CSRFKey: []byte("01234567890123456789012345678901"), FrontendOrigins: []string{"http://localhost:3000"}})
	if err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, handler)
	var token struct {
		Data struct {
			Token string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(client.get("/api/v1/csrf").Body.Bytes(), &token); err != nil || token.Data.Token == "" {
		t.Fatal("csrf bootstrap failed")
	}
	request := func(method, path, body, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://localhost:3000")
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		for _, cookie := range client.cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		client.saveCookies(w.Result())
		return w
	}
	if w := request("GET", "/api/v1/connections", "", ""); w.Code != 401 {
		t.Fatalf("anonymous: %d", w.Code)
	}
	if w := request("POST", "/api/v1/auth/login", `{"username":"Admin","password":"first-password"}`, token.Data.Token); w.Code != 200 {
		t.Fatalf("login: %d", w.Code)
	}
	if w := request("POST", "/api/v1/connections", `{"name":"Target"}`, ""); w.Code != 403 {
		t.Fatalf("missing csrf: %d", w.Code)
	}
	if w := request("GET", "/api/v1/databases", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"databases":[]`) {
		t.Fatalf("admin should have no grant: %d %s", w.Code, w.Body.String())
	}
	if w := request("POST", "/api/v1/connections", `{"name":"Target","host":"localhost","port":5432,"database":"platform_test","username":"test","ssl_mode":"disable","password":"private-secret","extra":"x"}`, token.Data.Token); w.Code != 400 {
		t.Fatalf("unknown field: %d", w.Code)
	}
	created := request("POST", "/api/v1/connections", `{"name":"Target","host":"127.0.0.1","port":1,"database":"target","username":"test","ssl_mode":"disable","password":"private-secret"}`, token.Data.Token)
	var result struct {
		Data struct {
			Connection struct {
				ID string `json:"id"`
			} `json:"connection"`
		} `json:"data"`
	}
	if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &result) != nil || result.Data.Connection.ID == "" || strings.Contains(created.Body.String(), "private-secret") {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	id := result.Data.Connection.ID
	if w := request("GET", "/api/v1/connections/"+id, "", ""); w.Code != 200 || strings.Contains(w.Body.String(), "private-secret") {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	if w := request("GET", "/api/v1/databases/"+id+"/schemas", "", ""); w.Code != 403 {
		t.Fatalf("admin without target grant: %d", w.Code)
	}
	if w := request("POST", "/api/v1/connections/"+id+"/test", "", token.Data.Token); w.Code != 503 || strings.Contains(w.Body.String(), "private-secret") {
		t.Fatalf("unavailable test: %d %s", w.Code, w.Body.String())
	}
	if w := request("DELETE", "/api/v1/connections/"+id, "", token.Data.Token); w.Code != 204 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
}
