package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/api"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/platform"
)

func TestPostgresPlatformAPI(t *testing.T) {
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
	authAPI, err := api.NewAuthHandler(api.AuthConfig{Authenticator: authenticator, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	platformAPI, err := api.NewPlatformHandler(service, service, sessions)
	if err != nil {
		t.Fatal(err)
	}
	routes := http.NewServeMux()
	routes.Handle("/api/v1/users", platformAPI)
	routes.Handle("/api/v1/users/", platformAPI)
	routes.Handle("/api/v1/groups", platformAPI)
	routes.Handle("/api/v1/groups/", platformAPI)
	routes.Handle("/api/", authAPI)
	handler, err := New(Config{Authenticator: authenticator, Sessions: sessions, API: routes, CSRFKey: []byte("01234567890123456789012345678901"), FrontendOrigins: []string{"http://localhost:3000"}})
	if err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, handler)
	var bootstrap struct {
		Data struct {
			Token string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(client.get("/api/v1/csrf").Body.Bytes(), &bootstrap); err != nil || bootstrap.Data.Token == "" {
		t.Fatal("csrf bootstrap failed")
	}
	request := func(method, path, body, token, origin string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("X-CSRF-Token", token)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		for _, cookie := range client.cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		client.saveCookies(w.Result())
		return w
	}
	check := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want || want != 204 && !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("status=%d want=%d body=%s", w.Code, want, w.Body.String())
		}
	}
	check(client.get("/api/v1/users"), 401)
	check(request("POST", "/api/v1/auth/login", `{"username":"Admin","password":"first-password"}`, bootstrap.Data.Token, "http://localhost:3000"), 200)
	check(request("POST", "/api/v1/users", `{"username":"Rina","password":"second-password"}`, "", "http://localhost:3000"), 403)
	check(request("POST", "/api/v1/users", `{"username":"Rina","password":"second-password"}`, bootstrap.Data.Token, "http://evil.example"), 403)
	check(request("DELETE", "/api/v1/users/1", "", bootstrap.Data.Token, "http://localhost:3000"), 409)
	created := request("POST", "/api/v1/users", `{"username":"Rina","password":"second-password"}`, bootstrap.Data.Token, "http://localhost:3000")
	check(created, 201)
	var user struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &user); err != nil || user.Data.ID == "" || strings.Contains(created.Body.String(), "second-password") {
		t.Fatal("unsafe user response")
	}
	group := request("POST", "/api/v1/groups", `{"name":"Ops"}`, bootstrap.Data.Token, "http://localhost:3000")
	check(group, 201)
	var grp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(group.Body.Bytes(), &grp); err != nil || grp.Data.ID == "" {
		t.Fatal("group creation failed")
	}
	memberPath := "/api/v1/groups/" + grp.Data.ID + "/members/" + user.Data.ID
	check(request("PUT", memberPath, "", bootstrap.Data.Token, "http://localhost:3000"), 204)
	if w := client.get("/api/v1/groups/" + grp.Data.ID + "/members"); w.Code != 200 || !strings.Contains(w.Body.String(), `"username":"Rina"`) {
		t.Fatalf("members: %d %s", w.Code, w.Body.String())
	}
	check(request("DELETE", memberPath, "", bootstrap.Data.Token, "http://localhost:3000"), 204)
	var databaseID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO database_connections
		(name, host, port, database_name, username, ssl_mode, password_ciphertext)
		VALUES ('test-target', 'localhost', 5432, 'testdb', 'testuser', 'disable', $1) RETURNING id`, make([]byte, 29)).Scan(&databaseID); err != nil {
		t.Fatal(err)
	}
	grantPath := "/api/v1/groups/" + grp.Data.ID + "/grants/" + strconv.FormatInt(databaseID, 10)
	check(request("PUT", grantPath, `{"level":2}`, bootstrap.Data.Token, "http://localhost:3000"), 204)
	if w := client.get("/api/v1/groups/" + grp.Data.ID + "/grants"); w.Code != 200 || !strings.Contains(w.Body.String(), `"level":2`) {
		t.Fatalf("grants: %d %s", w.Code, w.Body.String())
	}
	check(request("DELETE", grantPath, "", bootstrap.Data.Token, "http://localhost:3000"), 204)
	if w := client.get("/api/v1/groups/" + grp.Data.ID + "/grants"); w.Code != 200 || !strings.Contains(w.Body.String(), `"grants":[]`) {
		t.Fatalf("removed grant: %d %s", w.Code, w.Body.String())
	}
	leaderPath := "/api/v1/groups/" + grp.Data.ID + "/leader"
	check(request("PUT", leaderPath, `{"user_id":"`+user.Data.ID+`"}`, bootstrap.Data.Token, "http://localhost:3000"), 409)
	check(request("PUT", memberPath, "", bootstrap.Data.Token, "http://localhost:3000"), 204)
	check(request("PUT", leaderPath, `{"user_id":"`+user.Data.ID+`"}`, "", "http://localhost:3000"), 403)
	check(request("PUT", leaderPath, `{"user_id":"`+user.Data.ID+`"}`, bootstrap.Data.Token, "http://localhost:3000"), 204)
	if w := client.get("/api/v1/groups/" + grp.Data.ID); w.Code != 200 || !strings.Contains(w.Body.String(), `"leader_user_id":"`+user.Data.ID+`"`) {
		t.Fatalf("leader response: %d %s", w.Code, w.Body.String())
	}
	check(request("DELETE", memberPath, "", bootstrap.Data.Token, "http://localhost:3000"), 409)
	// Keep a second active session for the target user and verify password rotation revokes it.
	other := newTestClient(t, handler)
	other.cookies = make(map[string]*http.Cookie)
	var otherCSRF struct {
		Data struct {
			Token string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(other.get("/api/v1/csrf").Body.Bytes(), &otherCSRF); err != nil {
		t.Fatal(err)
	}
	loginOther := httptest.NewRequest("POST", "http://example.com/api/v1/auth/login", strings.NewReader(`{"username":"Rina","password":"second-password"}`))
	loginOther.Header.Set("Content-Type", "application/json")
	loginOther.Header.Set("Origin", "http://localhost:3000")
	loginOther.Header.Set("X-CSRF-Token", otherCSRF.Data.Token)
	for _, cookie := range other.cookies {
		loginOther.AddCookie(cookie)
	}
	otherResponse := httptest.NewRecorder()
	handler.ServeHTTP(otherResponse, loginOther)
	other.saveCookies(otherResponse.Result())
	check(otherResponse, 200)
	check(other.get("/api/v1/auth/me"), 200)
	check(request("PUT", "/api/v1/users/"+user.Data.ID+"/password", `{"password":"changed-password"}`, bootstrap.Data.Token, "http://localhost:3000"), 204)
	check(other.get("/api/v1/auth/me"), 401)
	if w := client.get("/api/v1/users"); w.Code != 200 || strings.Contains(w.Body.String(), "password") {
		t.Fatalf("unsafe users: %d %s", w.Code, w.Body.String())
	}
	// A non-admin session cannot read or mutate platform administration data.
	check(request("POST", "/api/v1/auth/logout", "", bootstrap.Data.Token, "http://localhost:3000"), 204)
	check(request("POST", "/api/v1/auth/login", `{"username":"Rina","password":"changed-password"}`, bootstrap.Data.Token, "http://localhost:3000"), 200)
	check(client.get("/api/v1/users"), 403)
	check(request("DELETE", "/api/v1/groups/"+grp.Data.ID, "", bootstrap.Data.Token, "http://localhost:3000"), 403)
}
