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
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/definition"
	"if3110.local/db-platform/internal/platform"
)

func TestPostgresDefinitionAPI(t *testing.T) {
	db, _, _ := isolatedPostgresDB(t, "PLATFORM_TEST_DATABASE_URL")
	ctx := context.Background()
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "Admin", "first-password"); err != nil {
		t.Fatal(err)
	}
	admin, err := login.Authenticate(ctx, "Admin", "first-password")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := auth.NewSessionManager(db, auth.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	p := platform.NewService(db)
	definitions, err := definition.NewService(db, p)
	if err != nil {
		t.Fatal(err)
	}
	group, err := p.CreateGroup(ctx, admin, "Editors")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetMembership(ctx, admin, admin, group, true); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := db.QueryRowContext(ctx, `INSERT INTO database_connections(name,host,port,database_name,username,ssl_mode,password_ciphertext) VALUES('A','localhost',5432,'target','tester','disable',$1) RETURNING id::text`, make([]byte, 29)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := p.SetGrant(ctx, admin, group, contracts.DatabaseID(id), contracts.AccessViewEdit); err != nil {
		t.Fatal(err)
	}
	apiHandler, err := api.NewDefinitionHandler(definitions, sessions)
	if err != nil {
		t.Fatal(err)
	}
	authHandler, err := api.NewAuthHandler(api.AuthConfig{Authenticator: login, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	routes := http.NewServeMux()
	routes.Handle("/api/v1/databases/{id}/migrations", apiHandler)
	routes.Handle("/api/v1/databases/{id}/migrations/", apiHandler)
	routes.Handle("/api/", authHandler)
	handler, err := New(Config{Authenticator: login, Sessions: sessions, API: routes, CSRFKey: []byte("01234567890123456789012345678901"), FrontendOrigins: []string{"http://localhost:3000"}})
	if err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, handler)
	var csrf struct {
		Data struct {
			Token string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(client.get("/api/v1/csrf").Body.Bytes(), &csrf); err != nil || csrf.Data.Token == "" {
		t.Fatal("csrf bootstrap failed")
	}
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://localhost:3000")
		if token != "" {
			r.Header.Set("X-CSRF-Token", token)
		}
		for _, cookie := range client.cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		client.saveCookies(w.Result())
		return w
	}
	path := "/api/v1/databases/" + id + "/migrations"
	if w := request("GET", path, "", ""); w.Code != 401 {
		t.Fatalf("anonymous: %d", w.Code)
	}
	if w := request("POST", "/api/v1/auth/login", `{"username":"Admin","password":"first-password"}`, csrf.Data.Token); w.Code != 200 {
		t.Fatalf("login: %d", w.Code)
	}
	if w := request("POST", path, `{"title":"First"}`, ""); w.Code != 403 {
		t.Fatalf("csrf: %d", w.Code)
	}
	created := request("POST", path, `{"title":"First","up_sql":"SELECT 1","down_sql":"SELECT 2","mode":"NORMAL"}`, csrf.Data.Token)
	var result struct {
		Data struct {
			Migration struct {
				ID       string `json:"id"`
				Revision int    `json:"revision"`
			} `json:"migration"`
		} `json:"data"`
	}
	if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &result) != nil || result.Data.Migration.ID == "" {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	detail := path + "/" + result.Data.Migration.ID
	if w := request("PATCH", detail, `{"expected_revision":1,"title":"Second","up_sql":"SELECT 3","down_sql":"SELECT 4","mode":"NORMAL"}`, csrf.Data.Token); w.Code != 200 || !strings.Contains(w.Body.String(), `"revision":2`) {
		t.Fatalf("revision: %d %s", w.Code, w.Body.String())
	}
	if w := request("PATCH", detail, `{"expected_revision":1,"title":"Third","up_sql":"SELECT 5","down_sql":"SELECT 6","mode":"NORMAL"}`, csrf.Data.Token); w.Code != 409 {
		t.Fatalf("stale: %d %s", w.Code, w.Body.String())
	}
	if w := request("GET", detail, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "SELECT 3") {
		t.Fatalf("detail: %d %s", w.Code, w.Body.String())
	}
}
