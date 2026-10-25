package web

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/database"
	"if3110.local/db-platform/internal/migration"
	"if3110.local/db-platform/internal/platform"
)

type fakeWebAuthorizer struct {
	admins map[contracts.UserID]bool
	grants map[contracts.UserID]map[contracts.DatabaseID]contracts.AccessLevel
}

func (f *fakeWebAuthorizer) RequirePlatformAdmin(_ context.Context, userID contracts.UserID) error {
	if f.admins[userID] {
		return nil
	}
	return contracts.ErrForbidden
}

func (f *fakeWebAuthorizer) RequireDatabaseAccess(_ context.Context, userID contracts.UserID, dbID contracts.DatabaseID, action contracts.DatabaseAction) error {
	level := f.grants[userID][dbID]
	if level.Allows(action) {
		return nil
	}
	return contracts.ErrForbidden
}

type fakeAdminService struct {
	users  []platform.User
	groups []platform.Group
}

func (f *fakeAdminService) ListUsers(context.Context, contracts.UserID) ([]platform.User, error) {
	return f.users, nil
}
func (f *fakeAdminService) CreateUser(context.Context, contracts.UserID, string, string) (contracts.UserID, error) {
	return "2", nil
}
func (f *fakeAdminService) UpdateUsername(context.Context, contracts.UserID, contracts.UserID, string) error {
	return nil
}
func (f *fakeAdminService) SetPassword(context.Context, contracts.UserID, contracts.UserID, string) error {
	return nil
}
func (f *fakeAdminService) SetPlatformAdmin(context.Context, contracts.UserID, contracts.UserID, bool) error {
	return nil
}
func (f *fakeAdminService) DeleteUser(context.Context, contracts.UserID, contracts.UserID) error {
	return nil
}
func (f *fakeAdminService) ListGroups(context.Context, contracts.UserID) ([]platform.Group, error) {
	return f.groups, nil
}
func (f *fakeAdminService) CreateGroup(context.Context, contracts.UserID, string) (contracts.GroupID, error) {
	return "1", nil
}
func (f *fakeAdminService) RenameGroup(context.Context, contracts.UserID, contracts.GroupID, string) error {
	return nil
}
func (f *fakeAdminService) DeleteGroup(context.Context, contracts.UserID, contracts.GroupID) error {
	return nil
}
func (f *fakeAdminService) ListMembers(context.Context, contracts.UserID, contracts.GroupID) ([]platform.User, error) {
	return nil, nil
}
func (f *fakeAdminService) SetMembership(context.Context, contracts.UserID, contracts.UserID, contracts.GroupID, bool) error {
	return nil
}
func (f *fakeAdminService) ListGrants(context.Context, contracts.UserID, contracts.GroupID) ([]platform.Grant, error) {
	return nil, nil
}
func (f *fakeAdminService) SetGrant(context.Context, contracts.UserID, contracts.GroupID, contracts.DatabaseID, contracts.AccessLevel) error {
	return nil
}

type fakeMigrationService struct {
	authorizer contracts.Authorizer
	records    []migration.MigrationRecord
}

func (f *fakeMigrationService) RequireCreateAccess(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID) error {
	return f.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionCreateMigration)
}

func (f *fakeMigrationService) ListHistory(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID) ([]migration.MigrationRecord, error) {
	if err := f.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionViewMigrationHistory); err != nil {
		return nil, err
	}
	return f.records, nil
}

func (f *fakeMigrationService) GetMigration(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, version int64) (migration.MigrationRecord, error) {
	if err := f.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionViewMigrationHistory); err != nil {
		return migration.MigrationRecord{}, err
	}
	return migration.MigrationRecord{Version: version, Title: "init", SQLText: "SELECT 1;", Status: migration.StatusDraft, CreatedBy: userID}, nil
}

func (f *fakeMigrationService) CreateDraft(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, title, sqlText string) (migration.MigrationRecord, error) {
	if err := f.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionCreateMigration); err != nil {
		return migration.MigrationRecord{}, err
	}
	return migration.MigrationRecord{Version: 1, Title: title, SQLText: sqlText, Status: migration.StatusDraft, CreatedBy: userID}, nil
}

func (f *fakeMigrationService) UpdateDraft(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, version int64, title, sqlText string) (migration.MigrationRecord, error) {
	if err := f.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionEditMigration); err != nil {
		return migration.MigrationRecord{}, err
	}
	return migration.MigrationRecord{Version: version, Title: title, SQLText: sqlText, Status: migration.StatusDraft, CreatedBy: userID}, nil
}

func (f *fakeMigrationService) ExecuteMigration(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, version int64, reviewed string) (migration.MigrationRecord, error) {
	if err := f.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionRunMigration); err != nil {
		return migration.MigrationRecord{}, err
	}
	return migration.MigrationRecord{Version: version, Status: migration.StatusApplied, CreatedBy: userID, ExecutedBy: userID}, nil
}

func (f *fakeMigrationService) RunMigration(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, title, sqlText string) (migration.MigrationRecord, error) {
	if err := f.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionRunMigration); err != nil {
		return migration.MigrationRecord{}, err
	}
	return migration.MigrationRecord{Version: 1, Title: title, SQLText: sqlText, Status: migration.StatusApplied, CreatedBy: userID, ExecutedBy: userID}, nil
}

func newFullTestServer(t *testing.T) (http.Handler, *fakeSessions, *fakeWebAuthorizer) {
	t.Helper()
	authz := &fakeWebAuthorizer{
		admins: map[contracts.UserID]bool{"1": true},
		grants: map[contracts.UserID]map[contracts.DatabaseID]contracts.AccessLevel{
			"2": {"10": contracts.AccessViewOnly},
			"3": {"10": contracts.AccessViewEdit},
		},
	}
	sessions := &fakeSessions{}
	dbManager, err := database.NewManager(&sql.DB{}, authz, bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbManager.Close() })
	handler, err := New(Config{
		Authenticator: &fakeAuthenticator{username: "admin", password: "secret-password", userID: "1"},
		Sessions:      sessions,
		Authorizer:    authz,
		Platform:      &fakeAdminService{users: []platform.User{{ID: "1", Username: "admin", IsPlatformAdmin: true}}},
		Databases:     dbManager,
		Migrations:    &fakeMigrationService{authorizer: authz},
		CSRFKey:       bytes.Repeat([]byte{1}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, sessions, authz
}

func TestHomePageRenders(t *testing.T) {
	handler, _, sessions := newTestServer(t)
	sessions.authenticated = true
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/", nil))

	if recorder.Code != 200 {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "Database Management Platform") {
		t.Fatalf("response body does not contain app title: %s", recorder.Body.String())
	}
}

func TestHomePageHidesAdminLinksFromNonAdmin(t *testing.T) {
	handler, sessions, _ := newFullTestServer(t)
	sessions.authenticated = true

	// Non-admin user must not see platform administration links on home.
	sessions.createdFor = "2"
	memberResp := httptest.NewRecorder()
	handler.ServeHTTP(memberResp, httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
	if memberResp.Code != http.StatusOK {
		t.Fatalf("member home status = %d, want 200", memberResp.Code)
	}
	memberBody := memberResp.Body.String()
	if !strings.Contains(memberBody, `href="/databases/"`) {
		t.Fatalf("member home missing /databases/ link: %s", memberBody)
	}
	for _, adminHref := range []string{`/platform/users`, `/platform/groups`, `/databases/connections/`} {
		if strings.Contains(memberBody, adminHref) {
			t.Fatalf("member home leaked admin link %q: %s", adminHref, memberBody)
		}
	}

	// Platform admin sees administration links on home.
	sessions.createdFor = "1"
	adminResp := httptest.NewRecorder()
	handler.ServeHTTP(adminResp, httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
	if adminResp.Code != http.StatusOK {
		t.Fatalf("admin home status = %d, want 200", adminResp.Code)
	}
	adminBody := adminResp.Body.String()
	for _, adminHref := range []string{`/databases/`, `/platform/users`, `/platform/groups`, `/databases/connections/`} {
		if !strings.Contains(adminBody, adminHref) {
			t.Fatalf("admin home missing link %q: %s", adminHref, adminBody)
		}
	}
}

func TestMountedRoutesEnforceSessionCSRFAndMigrationPrecedence(t *testing.T) {
	handler, sessions, _ := newFullTestServer(t)

	// 1. Without an active session, protected GET routes other than home return 401.
	for _, path := range []string{
		"/platform/users",
		"/platform/groups",
		"/databases/",
		"/databases/connections/new",
		"/databases/10/schemas/diff",
		"/databases/10/migrations",
		"/databases/10/migrations/new",
		"/migrations?database_id=10",
	} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil))
		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated GET %s status = %d, want 401", path, resp.Code)
		}
	}

	// 2. Platform Admin can access admin pages and connection form, but not database 10 without grant.
	sessions.authenticated = true
	sessions.createdFor = "1"
	for _, tc := range []struct {
		path       string
		wantStatus int
		wantText   string
	}{
		{"/platform/users", http.StatusOK, "Pengguna platform"},
		{"/platform/groups", http.StatusOK, "Grup platform"},
		{"/databases/connections/new", http.StatusOK, "Konfigurasi koneksi"},
		{"/databases/10/schemas/diff", http.StatusForbidden, ""},
		{"/databases/10/migrations", http.StatusForbidden, ""},
	} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "http://example.com"+tc.path, nil))
		if resp.Code != tc.wantStatus {
			t.Fatalf("admin GET %s status = %d, want %d", tc.path, resp.Code, tc.wantStatus)
		}
		if tc.wantText != "" && !strings.Contains(resp.Body.String(), tc.wantText) {
			t.Fatalf("admin GET %s missing %q", tc.path, tc.wantText)
		}
	}

	// 3. View Only user ("2") can view migration history under /databases/{id}/migrations,
	// proving migration routes take precedence over the /databases/ subtree, and is denied admin/editor routes.
	sessions.createdFor = "2"
	for _, tc := range []struct {
		path       string
		wantStatus int
		wantText   string
	}{
		{"/platform/users", http.StatusForbidden, ""},
		{"/databases/connections/new", http.StatusForbidden, ""},
		{"/databases/10/migrations", http.StatusOK, "Riwayat Migration"},
		{"/databases/10/migrations/new", http.StatusForbidden, ""},
		{"/migrations?database_id=10", http.StatusSeeOther, ""},
	} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "http://example.com"+tc.path, nil))
		if resp.Code != tc.wantStatus {
			t.Fatalf("view-only GET %s status = %d, want %d", tc.path, resp.Code, tc.wantStatus)
		}
		if tc.wantText != "" && !strings.Contains(resp.Body.String(), tc.wantText) {
			t.Fatalf("view-only GET %s missing %q", tc.path, tc.wantText)
		}
	}

	// 4. View + Edit user ("3") can open the migration editor and submit with valid CSRF,
	// while POST without CSRF is rejected with 403.
	sessions.createdFor = "3"
	editorResp := httptest.NewRecorder()
	handler.ServeHTTP(editorResp, httptest.NewRequest(http.MethodGet, "http://example.com/databases/10/migrations/new", nil))
	if editorResp.Code != http.StatusOK || !strings.Contains(editorResp.Body.String(), "Buat migration") {
		t.Fatalf("view-edit GET /databases/10/migrations/new status = %d", editorResp.Code)
	}

	noCSRF := postForm(handler, "/databases/10/migrations", url.Values{
		"title": {"init"}, "sql_text": {"SELECT 1;"}, "action": {"draft"},
	}, nil)
	if noCSRF.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF status = %d, want 403", noCSRF.Code)
	}

	cookies, token := getCSRFToken(t, handler, "/databases/10/migrations/new")
	withCSRF := postForm(handler, "/databases/10/migrations", url.Values{
		"title":              {"init"},
		"sql_text":           {"SELECT 1;"},
		"action":             {"draft"},
		"gorilla.csrf.Token": {token},
	}, cookies)
	if withCSRF.Code != http.StatusSeeOther || withCSRF.Header().Get("Location") != "/databases/10/migrations/1" {
		t.Fatalf("POST draft with CSRF status = %d, location = %q", withCSRF.Code, withCSRF.Header().Get("Location"))
	}
}
