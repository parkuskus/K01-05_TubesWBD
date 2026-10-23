package migration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
)

// fake service in-memory yang pakai fakeAuthorizer buat ngetes http handler
type fakeMigrationService struct {
	authz   *fakeAuthorizer
	records map[contracts.DatabaseID][]MigrationRecord
	failSQL map[string]error
	calls   int
}

func newFakeMigrationService(authz *fakeAuthorizer) *fakeMigrationService {
	return &fakeMigrationService{
		authz:   authz,
		records: make(map[contracts.DatabaseID][]MigrationRecord),
		failSQL: make(map[string]error),
	}
}

func (f *fakeMigrationService) RequireCreateAccess(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID) error {
	return f.authz.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionCreateMigration)
}

func (f *fakeMigrationService) ListHistory(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID) ([]MigrationRecord, error) {
	f.calls++
	if err := f.authz.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionViewMigrationHistory); err != nil {
		return nil, err
	}
	list := f.records[dbID]
	out := make([]MigrationRecord, len(list))
	copy(out, list)
	return out, nil
}

func (f *fakeMigrationService) GetMigration(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, version int64) (MigrationRecord, error) {
	f.calls++
	if err := f.authz.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionViewMigrationHistory); err != nil {
		return MigrationRecord{}, err
	}
	for _, rec := range f.records[dbID] {
		if rec.Version == version {
			return rec, nil
		}
	}
	return MigrationRecord{}, ErrMigrationNotFound
}

func (f *fakeMigrationService) CreateDraft(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, title, sqlText string) (MigrationRecord, error) {
	f.calls++
	if err := f.authz.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionCreateMigration); err != nil {
		return MigrationRecord{}, err
	}
	cleanedSQL, err := normalizeSQL(sqlText)
	if err != nil {
		return MigrationRecord{}, err
	}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	nextVersion := int64(len(f.records[dbID]) + 1)
	rec := MigrationRecord{
		ID:         nextVersion,
		DatabaseID: dbID,
		Version:    nextVersion,
		Title:      normalizeTitle(title),
		SQLText:    cleanedSQL,
		Status:     StatusDraft,
		CreatedBy:  userID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	f.records[dbID] = append(f.records[dbID], rec)
	return rec, nil
}

func (f *fakeMigrationService) UpdateDraft(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, version int64, title, sqlText string) (MigrationRecord, error) {
	f.calls++
	if err := f.authz.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionEditMigration); err != nil {
		return MigrationRecord{}, err
	}
	cleanedSQL, err := normalizeSQL(sqlText)
	if err != nil {
		return MigrationRecord{}, err
	}
	list := f.records[dbID]
	for i := range list {
		if list[i].Version == version {
			if list[i].Status != StatusDraft {
				return MigrationRecord{}, ErrMigrationAlreadyApplied
			}
			list[i].Title = normalizeTitle(title)
			list[i].SQLText = cleanedSQL
			list[i].UpdatedAt = time.Date(2026, 10, 23, 12, 5, 0, 0, time.UTC)
			return list[i], nil
		}
	}
	return MigrationRecord{}, ErrMigrationNotFound
}

func (f *fakeMigrationService) ExecuteMigration(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, version int64) (MigrationRecord, error) {
	f.calls++
	if err := f.authz.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionRunMigration); err != nil {
		return MigrationRecord{}, err
	}
	list := f.records[dbID]
	for i := range list {
		if list[i].Version == version {
			if list[i].Status != StatusDraft {
				return MigrationRecord{}, ErrMigrationAlreadyApplied
			}
			now := time.Date(2026, 10, 23, 12, 10, 0, 0, time.UTC)
			list[i].ExecutedBy = userID
			list[i].UpdatedAt = now
			list[i].ExecutedAt = &now
			if execErr, shouldFail := f.failSQL[list[i].SQLText]; shouldFail {
				list[i].Status = StatusFailed
				list[i].ErrorMessage = execErr.Error()
				return list[i], execErr
			}
			list[i].Status = StatusApplied
			list[i].ErrorMessage = ""
			return list[i], nil
		}
	}
	return MigrationRecord{}, ErrMigrationNotFound
}

func (f *fakeMigrationService) RunMigration(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, title, sqlText string) (MigrationRecord, error) {
	f.calls++
	if err := f.authz.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionRunMigration); err != nil {
		return MigrationRecord{}, err
	}
	cleanedSQL, err := normalizeSQL(sqlText)
	if err != nil {
		return MigrationRecord{}, err
	}
	now := time.Date(2026, 10, 23, 12, 15, 0, 0, time.UTC)
	nextVersion := int64(len(f.records[dbID]) + 1)
	rec := MigrationRecord{
		ID:         nextVersion,
		DatabaseID: dbID,
		Version:    nextVersion,
		Title:      normalizeTitle(title),
		SQLText:    cleanedSQL,
		Status:     StatusApplied,
		CreatedBy:  userID,
		ExecutedBy: userID,
		CreatedAt:  now,
		UpdatedAt:  now,
		ExecutedAt: &now,
	}
	if execErr, shouldFail := f.failSQL[cleanedSQL]; shouldFail {
		rec.Status = StatusFailed
		rec.ErrorMessage = execErr.Error()
		f.records[dbID] = append(f.records[dbID], rec)
		return rec, execErr
	}
	f.records[dbID] = append(f.records[dbID], rec)
	return rec, nil
}

func setupTestMigrationHandler(t *testing.T, svc MigrationService, activeUser *contracts.UserID, withCSRF bool) http.Handler {
	t.Helper()
	h, err := NewHandler(svc)
	if err != nil {
		t.Fatal(err)
	}
	h.currentUser = func(_ *http.Request) (contracts.UserID, bool) {
		if activeUser == nil || *activeUser == "" {
			return "", false
		}
		return *activeUser, true
	}

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	if !withCSRF {
		return mux
	}
	protected, err := auth.CSRFProtection([]byte("01234567890123456789012345678901"), false, mux)
	if err != nil {
		t.Fatal(err)
	}
	return protected
}

func TestHandlerRejectsUnauthenticatedRequests(t *testing.T) {
	authz := newFakeAuthorizer()
	svc := newFakeMigrationService(authz)
	var currentUser contracts.UserID // kosong = belum login
	router := setupTestMigrationHandler(t, svc, &currentUser, false)

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/migrations?database_id=db-1"},
		{http.MethodGet, "/databases/db-1/migrations"},
		{http.MethodGet, "/databases/db-1/migrations/new"},
		{http.MethodPost, "/databases/db-1/migrations"},
		{http.MethodGet, "/databases/db-1/migrations/1"},
		{http.MethodPost, "/databases/db-1/migrations/1/edit"},
		{http.MethodPost, "/databases/db-1/migrations/1/execute"},
	}

	for _, ep := range endpoints {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(ep.method, "http://example.com"+ep.path, nil)
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want 401 Unauthorized", ep.method, ep.path, rec.Code)
		}
	}
	if svc.calls != 0 {
		t.Fatalf("service called %d times without session, want 0", svc.calls)
	}
}

func TestHandlerViewOnlyUserCanViewHistoryButCannotMutate(t *testing.T) {
	authz := newFakeAuthorizer()
	viewer := contracts.UserID("viewer-user")
	editor := contracts.UserID("editor-user")
	dbID := contracts.DatabaseID("db-prod")

	authz.setGrant(viewer, dbID, contracts.AccessViewOnly)
	authz.setGrant(editor, dbID, contracts.AccessViewEdit)

	svc := newFakeMigrationService(authz)
	// siapkan 1 riwayat migration lewat editor
	if _, err := svc.CreateDraft(context.Background(), editor, dbID, "buat tabel awal", "CREATE TABLE items (id INT);"); err != nil {
		t.Fatal(err)
	}

	currentUser := viewer
	router := setupTestMigrationHandler(t, svc, &currentUser, false)

	// 1. GET riwayat migration oleh user view-only -> 200 OK
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "http://example.com/databases/db-prod/migrations", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET history by view-only user status = %d, want 200 OK", listRec.Code)
	}
	body := listRec.Body.String()
	if !strings.Contains(body, "#1") || !strings.Contains(body, "buat tabel awal") || !strings.Contains(body, "DRAFT") {
		t.Fatalf("GET history body missing expected record: %s", body)
	}
	if strings.Contains(body, "Buat Migration Baru") {
		t.Fatal("view-only history offered a migration creation link")
	}

	// 2. GET detail migration oleh user view-only -> 200 OK
	detailRec := httptest.NewRecorder()
	router.ServeHTTP(detailRec, httptest.NewRequest(http.MethodGet, "http://example.com/databases/db-prod/migrations/1", nil))
	if detailRec.Code != http.StatusOK || !strings.Contains(detailRec.Body.String(), "CREATE TABLE items (id INT);") {
		t.Fatalf("GET detail by view-only status/body = %d/%s", detailRec.Code, detailRec.Body.String())
	}

	// 3. GET halaman editor buat baru oleh user view-only -> 403 Forbidden
	newPageRec := httptest.NewRecorder()
	router.ServeHTTP(newPageRec, httptest.NewRequest(http.MethodGet, "http://example.com/databases/db-prod/migrations/new", nil))
	if newPageRec.Code != http.StatusForbidden {
		t.Fatalf("GET /new by view-only user status = %d, want 403 Forbidden", newPageRec.Code)
	}

	// 4. POST create draft, run, edit, dan execute oleh user view-only -> semuanya 403 Forbidden
	createRec := sendPostForm(router, "/databases/db-prod/migrations", url.Values{
		"title":    {"coba buat"},
		"sql_text": {"DROP TABLE items;"},
		"action":   {"draft"},
	}, nil)
	if createRec.Code != http.StatusForbidden {
		t.Fatalf("POST create draft by view-only user status = %d, want 403 Forbidden", createRec.Code)
	}

	runRec := sendPostForm(router, "/databases/db-prod/migrations", url.Values{
		"title":    {"coba run"},
		"sql_text": {"DROP TABLE items;"},
		"action":   {"run"},
	}, nil)
	if runRec.Code != http.StatusForbidden {
		t.Fatalf("POST run migration by view-only user status = %d, want 403 Forbidden", runRec.Code)
	}

	editRec := sendPostForm(router, "/databases/db-prod/migrations/1/edit", url.Values{
		"title":    {"ubah"},
		"sql_text": {"SELECT 1;"},
	}, nil)
	if editRec.Code != http.StatusForbidden {
		t.Fatalf("POST edit draft by view-only user status = %d, want 403 Forbidden", editRec.Code)
	}

	execRec := sendPostForm(router, "/databases/db-prod/migrations/1/execute", url.Values{}, nil)
	if execRec.Code != http.StatusForbidden {
		t.Fatalf("POST execute migration by view-only user status = %d, want 403 Forbidden", execRec.Code)
	}
}

func TestHandlerViewEditUserCreateEditAndExecuteFlow(t *testing.T) {
	authz := newFakeAuthorizer()
	editor := contracts.UserID("editor-user")
	dbID := contracts.DatabaseID("db-prod")
	authz.setGrant(editor, dbID, contracts.AccessViewEdit)

	svc := newFakeMigrationService(authz)
	currentUser := editor
	router := setupTestMigrationHandler(t, svc, &currentUser, false)

	// 1. buat draft baru lewat POST
	createRec := sendPostForm(router, "/databases/db-prod/migrations", url.Values{
		"title":    {"buat tabel orders"},
		"sql_text": {"CREATE TABLE orders (id INT PRIMARY KEY);"},
		"action":   {"draft"},
	}, nil)
	if createRec.Code != http.StatusSeeOther || createRec.Header().Get("Location") != "/databases/db-prod/migrations/1" {
		t.Fatalf("POST create draft = %d (Location: %q), want 303 to /databases/db-prod/migrations/1", createRec.Code, createRec.Header().Get("Location"))
	}

	// 2. ubah sql draft sebelum dieksekusi
	editRec := sendPostForm(router, "/databases/db-prod/migrations/1/edit", url.Values{
		"title":    {"buat tabel orders lengkap"},
		"sql_text": {"CREATE TABLE orders (id INT PRIMARY KEY, total INT NOT NULL);"},
	}, nil)
	if editRec.Code != http.StatusSeeOther || editRec.Header().Get("Location") != "/databases/db-prod/migrations/1" {
		t.Fatalf("POST edit draft = %d (Location: %q), want 303", editRec.Code, editRec.Header().Get("Location"))
	}

	// 3. eksekusi draft versi 1 -> sukses & redirect
	execRec := sendPostForm(router, "/databases/db-prod/migrations/1/execute", url.Values{}, nil)
	if execRec.Code != http.StatusSeeOther || execRec.Header().Get("Location") != "/databases/db-prod/migrations/1" {
		t.Fatalf("POST execute migration = %d (Location: %q), want 303", execRec.Code, execRec.Header().Get("Location"))
	}

	// 4. periksa halaman detail menampilkan status APPLIED
	detailRec := httptest.NewRecorder()
	router.ServeHTTP(detailRec, httptest.NewRequest(http.MethodGet, "http://example.com/databases/db-prod/migrations/1", nil))
	if detailRec.Code != http.StatusOK || !strings.Contains(detailRec.Body.String(), "APPLIED") {
		t.Fatalf("GET detail after execute = %d, body = %s", detailRec.Code, detailRec.Body.String())
	}

	// 5. uji skenario gagal eksekusi sql -> menampilkan halaman detail dengan status 422 dan pesan rollback
	badSQL := "INSERT INTO tabel_tidak_ada VALUES (1);"
	svc.failSQL[badSQL] = errors.New("relation \"tabel_tidak_ada\" does not exist")
	failRunRec := sendPostForm(router, "/databases/db-prod/migrations", url.Values{
		"title":    {"migrasi gagal"},
		"sql_text": {badSQL},
		"action":   {"run"},
	}, nil)
	if failRunRec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST failed run status = %d, want 422 Unprocessable Entity", failRunRec.Code)
	}
	failBody := failRunRec.Body.String()
	if !strings.Contains(failBody, "FAILED") || !strings.Contains(failBody, "tabel_tidak_ada") {
		t.Fatalf("failed run body missing FAILED status or error message: %s", failBody)
	}
}

func TestHandlerStateChangingPostRequiresCSRFToken(t *testing.T) {
	authz := newFakeAuthorizer()
	editor := contracts.UserID("editor-user")
	dbID := contracts.DatabaseID("db-prod")
	authz.setGrant(editor, dbID, contracts.AccessViewEdit)

	svc := newFakeMigrationService(authz)
	currentUser := editor
	router := setupTestMigrationHandler(t, svc, &currentUser, true)

	// 1. ambil halaman editor buat dapetin cookie dan token CSRF
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "http://example.com/databases/db-prod/migrations/new", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /new status = %d, want 200 OK", getRec.Code)
	}
	match := regexp.MustCompile(`name="gorilla.csrf.Token" value="([^"]+)"`).FindStringSubmatch(getRec.Body.String())
	if len(match) != 2 {
		t.Fatalf("CSRF token not found in editor page: %s", getRec.Body.String())
	}
	csrfCookies := getRec.Result().Cookies()
	csrfToken := match[1]

	// 2. POST tanpa token CSRF -> wajib ditolak 403 Forbidden sebelum masuk ke service
	callsBefore := svc.calls
	noCSRFRec := sendPostForm(router, "/databases/db-prod/migrations", url.Values{
		"title":    {"tanpa csrf"},
		"sql_text": {"CREATE TABLE t (id INT);"},
		"action":   {"draft"},
	}, csrfCookies)
	if noCSRFRec.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF token status = %d, want 403 Forbidden", noCSRFRec.Code)
	}
	if svc.calls != callsBefore {
		t.Fatal("service was called even though CSRF token was missing")
	}

	// 3. POST dengan token CSRF yang valid -> berhasil (303 See Other)
	validCSRFRec := sendPostForm(router, "/databases/db-prod/migrations", url.Values{
		"title":              {"dengan csrf valid"},
		"sql_text":           {"CREATE TABLE t (id INT);"},
		"action":             {"draft"},
		"gorilla.csrf.Token": {csrfToken},
	}, csrfCookies)
	if validCSRFRec.Code != http.StatusSeeOther {
		t.Fatalf("POST with valid CSRF token status = %d, want 303 See Other; body = %s", validCSRFRec.Code, validCSRFRec.Body.String())
	}
}

func TestHandlerUsesSharedLayoutForMigrationPages(t *testing.T) {
	authz := newFakeAuthorizer()
	authz.setGrant("editor", "db-prod", contracts.AccessViewEdit)
	svc := newFakeMigrationService(authz)
	if _, err := svc.CreateDraft(context.Background(), "editor", "db-prod", "judul", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	user := contracts.UserID("editor")
	router := setupTestMigrationHandler(t, svc, &user, false)
	for _, path := range []string{
		"/databases/db-prod/migrations",
		"/databases/db-prod/migrations/new",
		"/databases/db-prod/migrations/1",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			body := rec.Body.String()
			if rec.Code != http.StatusOK || strings.Count(body, "<html") != 1 || strings.Count(body, "<main") != 1 || !strings.Contains(body, `<main><section class="migration-shell">`) || !strings.Contains(body, `href="/static/styles.css"`) {
				t.Fatalf("page does not use the shared layout: status=%d body=%s", rec.Code, body)
			}
		})
	}
}

func TestHandlerRejectsServiceWithoutEditorAuthorization(t *testing.T) {
	svc := newFakeMigrationService(newFakeAuthorizer())
	withoutCheck := struct{ MigrationService }{svc}
	if _, err := NewHandler(withoutCheck); !errors.Is(err, ErrInvalidHandlerConfig) {
		t.Fatalf("NewHandler accepted service without editor access check: %v", err)
	}
}

func TestHandlerViewOnlyDetailDoesNotOfferMutationForms(t *testing.T) {
	authz := newFakeAuthorizer()
	authz.setGrant("editor", "db-prod", contracts.AccessViewEdit)
	authz.setGrant("viewer", "db-prod", contracts.AccessViewOnly)
	svc := newFakeMigrationService(authz)
	if _, err := svc.CreateDraft(context.Background(), "editor", "db-prod", "judul", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	user := contracts.UserID("viewer")
	router := setupTestMigrationHandler(t, svc, &user, false)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/databases/db-prod/migrations/1", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SELECT 1") || strings.Contains(rec.Body.String(), `method="post"`) {
		t.Fatalf("view-only detail exposed mutation controls: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerFailedSQLDoesNotRenderRawDatabaseError(t *testing.T) {
	authz := newFakeAuthorizer()
	authz.setGrant("editor", "db-prod", contracts.AccessViewEdit)
	svc := newFakeMigrationService(authz)
	svc.failSQL["SELECT 1"] = errors.New("private-database-diagnostic")
	user := contracts.UserID("editor")
	router := setupTestMigrationHandler(t, svc, &user, false)
	rec := sendPostForm(router, "/databases/db-prod/migrations", url.Values{"title": {"judul"}, "sql_text": {"SELECT 1"}, "action": {"run"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "FAILED") || strings.Contains(rec.Body.String(), "private-database-diagnostic") {
		t.Fatalf("failed migration exposed database error: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerEscapesDatabaseIDInEditorFormURL(t *testing.T) {
	authz := newFakeAuthorizer()
	authz.setGrant("editor", "db?private=1", contracts.AccessViewEdit)
	svc := newFakeMigrationService(authz)
	user := contracts.UserID("editor")
	router := setupTestMigrationHandler(t, svc, &user, false)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/databases/db%3Fprivate%3D1/migrations/new", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `action="/databases/db%3Fprivate=1/migrations"`) {
		t.Fatalf("editor form has unsafe database URL: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func sendPostForm(handler http.Handler, path string, form url.Values, cookies []*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://example.com"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
