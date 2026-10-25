package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/database"
	"if3110.local/db-platform/internal/migration"
	"if3110.local/db-platform/internal/platform"
)

func isolatedPostgresDB(t *testing.T, envVar string) (*sql.DB, *pgx.ConnConfig, string) {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(envVar))
	if raw == "" {
		t.Skipf("set %s in the process environment to run PostgreSQL end-to-end HTTP tests", envVar)
	}
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatalf("invalid %s configuration", envVar)
	}
	adminDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = adminDB.Close() })

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "web_e2e_" + hex.EncodeToString(suffix[:])
	if _, err := adminDB.ExecContext(context.Background(), `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("could not create isolated test schema for %s", envVar)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	})

	scopedCfg := cfg.Copy()
	if scopedCfg.RuntimeParams == nil {
		scopedCfg.RuntimeParams = make(map[string]string)
	}
	scopedCfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*scopedCfg)
	t.Cleanup(func() { _ = db.Close() })
	return db, cfg, schema
}

type testClient struct {
	t       *testing.T
	handler http.Handler
	cookies map[string]*http.Cookie
}

func newTestClient(t *testing.T, handler http.Handler) *testClient {
	return &testClient{t: t, handler: handler, cookies: make(map[string]*http.Cookie)}
}

func (c *testClient) saveCookies(resp *http.Response) {
	for _, cookie := range resp.Cookies() {
		if cookie.MaxAge < 0 || cookie.Value == "" {
			delete(c.cookies, cookie.Name)
		} else {
			c.cookies[cookie.Name] = cookie
		}
	}
}

func (c *testClient) get(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil)
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	c.saveCookies(rec.Result())
	return rec
}

func (c *testClient) csrfFrom(path string) string {
	c.t.Helper()
	rec := c.get(path)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("GET %s returned %d, want 200", path, rec.Code)
	}
	_, token := extractCSRF(c.t, rec)
	return token
}

func (c *testClient) post(path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://example.com"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	c.saveCookies(rec.Result())
	return rec
}

func extractCSRF(t *testing.T, rec *httptest.ResponseRecorder) ([]*http.Cookie, string) {
	t.Helper()
	const marker = `name="gorilla.csrf.Token" value="`
	body := rec.Body.String()
	idx := strings.Index(body, marker)
	if idx == -1 {
		t.Fatalf("CSRF token input not found in body: %s", body)
	}
	rest := body[idx+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end == -1 || end == 0 {
		t.Fatalf("malformed CSRF token input in body: %s", body)
	}
	return rec.Result().Cookies(), rest[:end]
}

func TestPostgresEndToEndWebFlow(t *testing.T) {
	platformDB, _, _ := isolatedPostgresDB(t, "PLATFORM_TEST_DATABASE_URL")
	targetDB, targetCfg, targetSchema := isolatedPostgresDB(t, "TARGET_TEST_DATABASE_URL")
	if targetCfg.Password == "" {
		t.Fatal("TARGET_TEST_DATABASE_URL must use password authentication")
	}

	ctx := context.Background()
	var existingHistory sql.NullString
	if err := targetDB.QueryRowContext(ctx, `SELECT to_regnamespace('if3110_platform')::text`).Scan(&existingHistory); err != nil {
		t.Fatal(err)
	}
	if existingHistory.Valid {
		t.Skip("target database already contains if3110_platform schema; refusing to modify it")
	}
	t.Cleanup(func() {
		_, _ = targetDB.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS if3110_platform CASCADE`)
	})

	if err := migrations.Apply(ctx, platformDB); err != nil {
		t.Fatal(err)
	}
	authenticator := auth.NewAuthenticator(platformDB)
	if err := authenticator.EnsureInitialAdmin(ctx, "admin", "admin-secret-pass"); err != nil {
		t.Fatal(err)
	}
	sessions, err := auth.NewSessionManager(platformDB, auth.SessionConfig{SecureCookie: false})
	if err != nil {
		t.Fatal(err)
	}
	platformService := platform.NewService(platformDB)
	dbManager, err := database.NewManager(platformDB, platformService, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer dbManager.Close()
	migrationService, err := migration.NewService(platformService, dbManager)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(Config{
		Authenticator: authenticator,
		Sessions:      sessions,
		Authorizer:    platformService,
		Platform:      platformService,
		Databases:     dbManager,
		Migrations:    migrationService,
		CSRFKey:       bytes.Repeat([]byte{7}, 32),
		SecureCookies: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Admin logs in with real session cookie and CSRF token.
	adminClient := newTestClient(t, app)
	loginToken := adminClient.csrfFrom("/login")
	loginResp := adminClient.post("/login", url.Values{
		"username":           {"admin"},
		"password":           {"admin-secret-pass"},
		"gorilla.csrf.Token": {loginToken},
	})
	if loginResp.Code != http.StatusSeeOther || loginResp.Header().Get("Location") != "/" {
		t.Fatalf("admin login status=%d location=%q", loginResp.Code, loginResp.Header().Get("Location"))
	}

	// 2. Admin creates viewer and editor users, groups, memberships, connection, and grants.
	usersToken := adminClient.csrfFrom("/platform/users")
	if resp := adminClient.post("/platform/users", url.Values{
		"username":           {"viewer_user"},
		"password":           {"viewer-secret-pass"},
		"gorilla.csrf.Token": {usersToken},
	}); resp.Code != http.StatusSeeOther {
		t.Fatalf("create viewer_user status=%d", resp.Code)
	}
	usersToken = adminClient.csrfFrom("/platform/users")
	if resp := adminClient.post("/platform/users", url.Values{
		"username":           {"editor_user"},
		"password":           {"editor-secret-pass"},
		"gorilla.csrf.Token": {usersToken},
	}); resp.Code != http.StatusSeeOther {
		t.Fatalf("create editor_user status=%d", resp.Code)
	}

	groupsToken := adminClient.csrfFrom("/platform/groups")
	if resp := adminClient.post("/platform/groups", url.Values{
		"name":               {"Viewers"},
		"gorilla.csrf.Token": {groupsToken},
	}); resp.Code != http.StatusSeeOther {
		t.Fatalf("create Viewers group status=%d", resp.Code)
	}
	groupsToken = adminClient.csrfFrom("/platform/groups")
	if resp := adminClient.post("/platform/groups", url.Values{
		"name":               {"Editors"},
		"gorilla.csrf.Token": {groupsToken},
	}); resp.Code != http.StatusSeeOther {
		t.Fatalf("create Editors group status=%d", resp.Code)
	}

	// Register target database connection through HTTP form.
	sslMode := "disable"
	if targetCfg.TLSConfig != nil {
		sslMode = "verify-full"
		if targetCfg.TLSConfig.InsecureSkipVerify {
			sslMode = "require"
		}
	}
	connToken := adminClient.csrfFrom("/databases/connections/new")
	connResp := adminClient.post("/databases/connections/", url.Values{
		"name":               {"Inventory Target"},
		"host":               {targetCfg.Host},
		"port":               {itoa(int(targetCfg.Port))},
		"database":           {targetCfg.Database},
		"username":           {targetCfg.User},
		"password":           {targetCfg.Password},
		"sslmode":            {sslMode},
		"gorilla.csrf.Token": {connToken},
	})
	if connResp.Code != http.StatusSeeOther {
		t.Fatalf("create connection status=%d body=%s", connResp.Code, connResp.Body.String())
	}
	connList := adminClient.get("/databases/connections/")
	if connList.Code != http.StatusOK || strings.Contains(connList.Body.String(), targetCfg.Password) {
		t.Fatalf("connections page status=%d or leaked password", connList.Code)
	}

	// Admin without group grant cannot view target database schemas.
	if resp := adminClient.get("/databases/1/schemas"); resp.Code != http.StatusForbidden {
		t.Fatalf("admin without grant GET /databases/1/schemas = %d, want 403", resp.Code)
	}

	// Assign memberships and grants (group 1 = Viewers -> user 2, level 1; group 2 = Editors -> user 3, level 2).
	g1Token := adminClient.csrfFrom("/platform/groups/1")
	if resp := adminClient.post("/platform/groups/1/members", url.Values{
		"user_id":            {"2"},
		"member":             {"true"},
		"gorilla.csrf.Token": {g1Token},
	}); resp.Code != http.StatusSeeOther {
		t.Fatalf("add viewer_user to Viewers status=%d", resp.Code)
	}
	g1Token = adminClient.csrfFrom("/platform/groups/1")
	if resp := adminClient.post("/platform/groups/1/grants", url.Values{
		"database_id":        {"1"},
		"level":              {"1"},
		"gorilla.csrf.Token": {g1Token},
	}); resp.Code != http.StatusSeeOther {
		t.Fatalf("grant View Only to Viewers status=%d", resp.Code)
	}

	g2Token := adminClient.csrfFrom("/platform/groups/2")
	if resp := adminClient.post("/platform/groups/2/members", url.Values{
		"user_id":            {"3"},
		"member":             {"true"},
		"gorilla.csrf.Token": {g2Token},
	}); resp.Code != http.StatusSeeOther {
		t.Fatalf("add editor_user to Editors status=%d", resp.Code)
	}
	g2Token = adminClient.csrfFrom("/platform/groups/2")
	if resp := adminClient.post("/platform/groups/2/grants", url.Values{
		"database_id":        {"1"},
		"level":              {"2"},
		"gorilla.csrf.Token": {g2Token},
	}); resp.Code != http.StatusSeeOther {
		t.Fatalf("grant View + Edit to Editors status=%d", resp.Code)
	}

	// 3. Editor logs in, creates draft migration, executes it in target schema, and tests rollback on failure.
	editorClient := newTestClient(t, app)
	editorLoginToken := editorClient.csrfFrom("/login")
	if resp := editorClient.post("/login", url.Values{
		"username":           {"editor_user"},
		"password":           {"editor-secret-pass"},
		"gorilla.csrf.Token": {editorLoginToken},
	}); resp.Code != http.StatusSeeOther {
		t.Fatalf("editor login status=%d", resp.Code)
	}
	// Non-admin editor is forbidden from platform admin routes.
	if resp := editorClient.get("/platform/users"); resp.Code != http.StatusForbidden {
		t.Fatalf("editor GET /platform/users = %d, want 403", resp.Code)
	}

	migNewToken := editorClient.csrfFrom("/databases/1/migrations/new")
	qualifiedTable := pgx.Identifier{targetSchema, "products"}.Sanitize()
	draftResp := editorClient.post("/databases/1/migrations", url.Values{
		"title":              {"create products table"},
		"sql_text":           {"CREATE TABLE " + qualifiedTable + " (id INT PRIMARY KEY, name TEXT NOT NULL); INSERT INTO " + qualifiedTable + " VALUES (1, 'Laptop');"},
		"action":             {"draft"},
		"gorilla.csrf.Token": {migNewToken},
	})
	if draftResp.Code != http.StatusSeeOther || draftResp.Header().Get("Location") != "/databases/1/migrations/1" {
		t.Fatalf("create draft status=%d location=%q", draftResp.Code, draftResp.Header().Get("Location"))
	}

	execToken := editorClient.csrfFrom("/databases/1/migrations/1")
	detailHTML := editorClient.get("/databases/1/migrations/1").Body.String()
	const reviewField = `name="reviewed_draft" value="`
	start := strings.Index(detailHTML, reviewField)
	if start < 0 {
		t.Fatal("draft review field missing from detail page")
	}
	review := strings.SplitN(detailHTML[start+len(reviewField):], `"`, 2)[0]
	execResp := editorClient.post("/databases/1/migrations/1/execute", url.Values{
		"confirm_execute":    {"true"},
		"reviewed_draft":     {review},
		"gorilla.csrf.Token": {execToken},
	})
	if execResp.Code != http.StatusSeeOther || execResp.Header().Get("Location") != "/databases/1/migrations/1" {
		t.Fatalf("execute draft status=%d location=%q body=%s", execResp.Code, execResp.Header().Get("Location"), execResp.Body.String())
	}

	// Generated SQL must go through the same editable draft and reviewed execution path.
	referenceSchema, destinationSchema := targetSchema+"_ref", targetSchema+"_dst"
	refTable := pgx.Identifier{referenceSchema, "items"}.Sanitize()
	dstTable := pgx.Identifier{destinationSchema, "items"}.Sanitize()
	if _, err := targetDB.ExecContext(ctx, `CREATE SCHEMA `+pgx.Identifier{referenceSchema}.Sanitize()+`; CREATE SCHEMA `+pgx.Identifier{destinationSchema}.Sanitize()+`; CREATE TABLE `+refTable+` (id integer, label text); CREATE TABLE `+dstTable+` (id integer)`); err != nil {
		t.Fatal("could not create disposable diff fixtures")
	}
	t.Cleanup(func() {
		for _, schema := range []string{referenceSchema, destinationSchema} {
			if _, err := targetDB.ExecContext(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
				t.Error("could not remove disposable diff fixture")
			}
		}
	})
	diffPath := "/databases/1/schemas/diff?reference=" + url.QueryEscape(referenceSchema) + "&target=" + url.QueryEscape(destinationSchema)
	diffPage := editorClient.get(diffPath)
	if diffPage.Code != http.StatusOK || !strings.Contains(diffPage.Body.String(), "Buat draft migration") || !strings.Contains(diffPage.Body.String(), "ADD COLUMN") {
		t.Fatalf("editor diff page status=%d or missing candidate/draft action", diffPage.Code)
	}
	_, diffToken := extractCSRF(t, diffPage)
	generated := editorClient.post("/databases/1/schemas/diff", url.Values{
		"reference": {referenceSchema}, "target": {destinationSchema}, "gorilla.csrf.Token": {diffToken},
	})
	if generated.Code != http.StatusSeeOther || generated.Header().Get("Location") != "/databases/1/migrations/2" {
		t.Fatalf("generated draft status=%d location=%q", generated.Code, generated.Header().Get("Location"))
	}
	generatedPath := generated.Header().Get("Location")
	detail := editorClient.get(generatedPath)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "ADD COLUMN") || !strings.Contains(detail.Body.String(), "DRAFT") {
		t.Fatalf("generated draft detail status=%d or SQL/status missing", detail.Code)
	}
	_, reviewToken := extractCSRF(t, detail)
	reviewStart := strings.Index(detail.Body.String(), reviewField)
	if reviewStart < 0 {
		t.Fatal("generated draft review field missing")
	}
	oldReview := strings.SplitN(detail.Body.String()[reviewStart+len(reviewField):], `"`, 2)[0]
	if resp := editorClient.post(generatedPath+"/execute", url.Values{
		"reviewed_draft": {oldReview}, "gorilla.csrf.Token": {reviewToken},
	}); resp.Code != http.StatusBadRequest {
		t.Fatalf("generated draft without confirmation status=%d", resp.Code)
	}
	var columnCount int
	countColumns := func() int {
		t.Helper()
		if err := targetDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname='items' AND a.attname='label' AND a.attnum>0 AND NOT a.attisdropped`, destinationSchema).Scan(&columnCount); err != nil {
			t.Fatal(err)
		}
		return columnCount
	}
	if countColumns() != 0 {
		t.Fatal("diff GET/draft changed destination table")
	}
	// A later version cannot overtake the generated draft.
	newToken := editorClient.csrfFrom("/databases/1/migrations/new")
	third := editorClient.post("/databases/1/migrations", url.Values{"title": {"later"}, "sql_text": {"SELECT 1;"}, "action": {"draft"}, "gorilla.csrf.Token": {newToken}})
	if third.Code != http.StatusSeeOther || third.Header().Get("Location") != "/databases/1/migrations/3" {
		t.Fatalf("later draft status=%d", third.Code)
	}
	thirdDetail := editorClient.get("/databases/1/migrations/3")
	_, thirdToken := extractCSRF(t, thirdDetail)
	thirdStart := strings.Index(thirdDetail.Body.String(), reviewField)
	if thirdStart < 0 {
		t.Fatal("later draft review field missing")
	}
	thirdReview := strings.SplitN(thirdDetail.Body.String()[thirdStart+len(reviewField):], `"`, 2)[0]
	if resp := editorClient.post("/databases/1/migrations/3/execute", url.Values{"confirm_execute": {"true"}, "reviewed_draft": {thirdReview}, "gorilla.csrf.Token": {thirdToken}}); resp.Code != http.StatusConflict {
		t.Fatalf("out-of-order execution status=%d", resp.Code)
	}
	// Editing after review makes the original hash stale, even if SQL is otherwise executable.
	update := editorClient.post(generatedPath+"/edit", url.Values{"title": {"reviewed schema diff"}, "sql_text": {"ALTER TABLE " + dstTable + " ADD COLUMN label text;"}, "gorilla.csrf.Token": {reviewToken}})
	if update.Code != http.StatusSeeOther {
		t.Fatalf("generated draft edit status=%d", update.Code)
	}
	if resp := editorClient.post(generatedPath+"/execute", url.Values{"confirm_execute": {"true"}, "reviewed_draft": {oldReview}, "gorilla.csrf.Token": {reviewToken}}); resp.Code != http.StatusConflict {
		t.Fatalf("stale review status=%d", resp.Code)
	}
	if countColumns() != 0 {
		t.Fatal("stale review changed destination table")
	}
	detail = editorClient.get(generatedPath)
	_, reviewToken = extractCSRF(t, detail)
	reviewStart = strings.Index(detail.Body.String(), reviewField)
	if reviewStart < 0 {
		t.Fatal("edited draft review field missing")
	}
	newReview := strings.SplitN(detail.Body.String()[reviewStart+len(reviewField):], `"`, 2)[0]
	if resp := editorClient.post(generatedPath+"/execute", url.Values{"confirm_execute": {"true"}, "reviewed_draft": {newReview}, "gorilla.csrf.Token": {reviewToken}}); resp.Code != http.StatusSeeOther {
		t.Fatalf("generated migration execution status=%d", resp.Code)
	}
	if countColumns() != 1 {
		t.Fatal("generated migration did not change destination table")
	}
	if detail = editorClient.get(generatedPath); detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "APPLIED") || !strings.Contains(detail.Body.String(), "Dieksekusi oleh:") || !strings.Contains(detail.Body.String(), "Waktu eksekusi:") {
		t.Fatalf("generated migration history status=%d", detail.Code)
	}
	if resp := editorClient.post("/databases/1/migrations/3/execute", url.Values{"confirm_execute": {"true"}, "reviewed_draft": {thirdReview}, "gorilla.csrf.Token": {thirdToken}}); resp.Code != http.StatusSeeOther {
		t.Fatalf("later migration execution status=%d", resp.Code)
	}

	// A generated draft edited to fail must roll back both DDL and earlier DML.
	if _, err := targetDB.ExecContext(ctx, `ALTER TABLE `+refTable+` ADD COLUMN note text`); err != nil {
		t.Fatal("could not extend disposable reference table")
	}
	failDiff := editorClient.get(diffPath)
	_, failToken := extractCSRF(t, failDiff)
	failDraft := editorClient.post("/databases/1/schemas/diff", url.Values{
		"reference": {referenceSchema}, "target": {destinationSchema}, "gorilla.csrf.Token": {failToken},
	})
	if failDraft.Code != http.StatusSeeOther || failDraft.Header().Get("Location") != "/databases/1/migrations/4" {
		t.Fatalf("failing generated draft status=%d location=%q", failDraft.Code, failDraft.Header().Get("Location"))
	}
	failPath := failDraft.Header().Get("Location")
	failToken = editorClient.csrfFrom(failPath)
	if resp := editorClient.post(failPath+"/edit", url.Values{
		"title":              {"broken generated migration"},
		"sql_text":           {"ALTER TABLE " + dstTable + " ADD COLUMN note text; INSERT INTO " + qualifiedTable + " VALUES (2, 'ShouldRollback'); SELECT * FROM non_existent_table_xyz;"},
		"gorilla.csrf.Token": {failToken},
	}); resp.Code != http.StatusSeeOther {
		t.Fatalf("edit failing generated draft status=%d", resp.Code)
	}
	failDetail := editorClient.get(failPath)
	_, failToken = extractCSRF(t, failDetail)
	failStart := strings.Index(failDetail.Body.String(), reviewField)
	if failStart < 0 {
		t.Fatal("failing draft review field missing")
	}
	failReview := strings.SplitN(failDetail.Body.String()[failStart+len(reviewField):], `"`, 2)[0]
	failResp := editorClient.post(failPath+"/execute", url.Values{
		"confirm_execute": {"true"}, "reviewed_draft": {failReview}, "gorilla.csrf.Token": {failToken},
	})
	if failResp.Code != http.StatusUnprocessableEntity || !strings.Contains(failResp.Body.String(), "FAILED") {
		t.Fatalf("failing migration status=%d", failResp.Code)
	}
	if err := targetDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname='items' AND a.attname='note' AND a.attnum>0 AND NOT a.attisdropped`, destinationSchema).Scan(&columnCount); err != nil || columnCount != 0 {
		t.Fatalf("failed generated SQL did not roll back DDL: count=%d err=%v", columnCount, err)
	}

	// 4. Viewer logs in, browses schema/tables/structure/data and migration history, but cannot create or run migrations.
	viewerClient := newTestClient(t, app)
	viewerLoginToken := viewerClient.csrfFrom("/login")
	if resp := viewerClient.post("/login", url.Values{
		"username":           {"viewer_user"},
		"password":           {"viewer-secret-pass"},
		"gorilla.csrf.Token": {viewerLoginToken},
	}); resp.Code != http.StatusSeeOther {
		t.Fatalf("viewer login status=%d", resp.Code)
	}

	dbListResp := viewerClient.get("/databases/")
	if dbListResp.Code != http.StatusOK || !strings.Contains(dbListResp.Body.String(), "Inventory Target") || !strings.Contains(dbListResp.Body.String(), "View Only") {
		t.Fatalf("viewer /databases/ status=%d body=%s", dbListResp.Code, dbListResp.Body.String())
	}
	dataResp := viewerClient.get("/databases/1/data?schema=" + url.QueryEscape(targetSchema) + "&table=products&page=1")
	if dataResp.Code != http.StatusOK || !strings.Contains(dataResp.Body.String(), "Laptop") || strings.Contains(dataResp.Body.String(), "ShouldRollback") {
		t.Fatalf("viewer data page status=%d body=%s", dataResp.Code, dataResp.Body.String())
	}
	historyResp := viewerClient.get("/databases/1/migrations")
	if historyResp.Code != http.StatusOK || !strings.Contains(historyResp.Body.String(), "create products table") || !strings.Contains(historyResp.Body.String(), "broken generated migration") {
		t.Fatalf("viewer migration history status=%d body=%s", historyResp.Code, historyResp.Body.String())
	}
	if resp := viewerClient.get("/databases/1/migrations/new"); resp.Code != http.StatusForbidden {
		t.Fatalf("view-only GET /databases/1/migrations/new = %d, want 403", resp.Code)
	}
	viewerDiff := viewerClient.get(diffPath)
	if viewerDiff.Code != http.StatusOK || strings.Contains(viewerDiff.Body.String(), "Buat draft migration") {
		t.Fatalf("view-only diff status=%d or exposes draft action", viewerDiff.Code)
	}
	_, viewerDiffToken := extractCSRF(t, viewerDiff)
	if resp := viewerClient.post("/databases/1/schemas/diff", url.Values{
		"reference": {referenceSchema}, "target": {destinationSchema}, "gorilla.csrf.Token": {viewerDiffToken},
	}); resp.Code != http.StatusForbidden {
		t.Fatalf("view-only diff draft status=%d", resp.Code)
	}
	viewerCSRF := viewerClient.csrfFrom("/")
	if resp := viewerClient.post("/databases/1/migrations", url.Values{
		"title":              {"unauthorized"},
		"sql_text":           {"SELECT 1;"},
		"action":             {"run"},
		"confirm_execute":    {"true"},
		"gorilla.csrf.Token": {viewerCSRF},
	}); resp.Code != http.StatusForbidden {
		t.Fatalf("view-only POST /databases/1/migrations = %d, want 403", resp.Code)
	}

	// 5. Admin revokes Viewers grant -> the viewer's session is immediately invalidated.
	revokeToken := adminClient.csrfFrom("/platform/groups/1")
	if resp := adminClient.post("/platform/groups/1/grants", url.Values{
		"database_id":        {"1"},
		"level":              {"0"},
		"confirm":            {"true"},
		"gorilla.csrf.Token": {revokeToken},
	}); resp.Code != http.StatusSeeOther {
		t.Fatalf("revoke grant status=%d", resp.Code)
	}
	if resp := viewerClient.get("/databases/1/data?schema=" + url.QueryEscape(targetSchema) + "&table=products&page=1"); resp.Code != http.StatusUnauthorized {
		t.Fatalf("revoked viewer GET data = %d, want 401", resp.Code)
	}

	// 6. Logout an active editor session -> home redirects to login.
	logoutToken := editorClient.csrfFrom("/")
	if resp := editorClient.post("/logout", url.Values{"gorilla.csrf.Token": {logoutToken}}); resp.Code != http.StatusSeeOther {
		t.Fatalf("editor logout status=%d", resp.Code)
	}
	if resp := editorClient.get("/"); resp.Code != http.StatusSeeOther || resp.Header().Get("Location") != "/login" {
		t.Fatalf("logged-out GET / = %d location %q, want 303 to /login", resp.Code, resp.Header().Get("Location"))
	}
	// A grant revoked after review invalidates the editor session before execution.
	editorLoginToken = editorClient.csrfFrom("/login")
	if resp := editorClient.post("/login", url.Values{"username": {"editor_user"}, "password": {"editor-secret-pass"}, "gorilla.csrf.Token": {editorLoginToken}}); resp.Code != http.StatusSeeOther {
		t.Fatalf("editor re-login status=%d", resp.Code)
	}
	newToken = editorClient.csrfFrom("/databases/1/migrations/new")
	pending := editorClient.post("/databases/1/migrations", url.Values{"title": {"pending review"}, "sql_text": {"SELECT 1;"}, "action": {"draft"}, "gorilla.csrf.Token": {newToken}})
	if pending.Code != http.StatusSeeOther || pending.Header().Get("Location") != "/databases/1/migrations/5" {
		t.Fatalf("pending draft status=%d", pending.Code)
	}
	pendingDetail := editorClient.get(pending.Header().Get("Location"))
	_, pendingToken := extractCSRF(t, pendingDetail)
	pendingStart := strings.Index(pendingDetail.Body.String(), reviewField)
	if pendingStart < 0 {
		t.Fatal("pending draft review field missing")
	}
	pendingReview := strings.SplitN(pendingDetail.Body.String()[pendingStart+len(reviewField):], `"`, 2)[0]
	revokeToken = adminClient.csrfFrom("/platform/groups/2")
	if resp := adminClient.post("/platform/groups/2/grants", url.Values{"database_id": {"1"}, "level": {"0"}, "confirm": {"true"}, "gorilla.csrf.Token": {revokeToken}}); resp.Code != http.StatusSeeOther {
		t.Fatalf("revoke editor grant status=%d", resp.Code)
	}
	if resp := editorClient.post("/databases/1/migrations/5/execute", url.Values{"confirm_execute": {"true"}, "reviewed_draft": {pendingReview}, "gorilla.csrf.Token": {pendingToken}}); resp.Code != http.StatusUnauthorized {
		t.Fatalf("revoked editor execution status=%d", resp.Code)
	}
}

func itoa(n int) string {
	return (&url.Values{"n": {string(rune('0'))}}).Get("") + strconvItoa(n)
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}
