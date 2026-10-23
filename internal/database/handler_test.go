package database

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
)

func testHandlers(t *testing.T) (*handlers, http.Handler, *memoryStore) {
	t.Helper()
	m, s := testManager(t)
	v, _ := NewViewer(m.authorizer, m)
	h := &handlers{manager: m, viewer: v, layout: func(_ string, c templ.Component) templ.Component { return c }, currentUser: func(r *http.Request) (contracts.UserID, bool) {
		u := contracts.UserID(r.Header.Get("X-Test-User"))
		return u, u != ""
	}}
	protected, err := auth.CSRFProtection(bytes.Repeat([]byte{1}, 32), false, h.routes())
	if err != nil {
		t.Fatal(err)
	}
	return h, protected, s
}

func requestPage(handler http.Handler, path, user string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://example.com"+path, nil)
	r.Header.Set("X-Test-User", user)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}
func formToken(t *testing.T, handler http.Handler, path, user string) ([]*http.Cookie, string) {
	t.Helper()
	w := requestPage(handler, path, user)
	match := regexp.MustCompile(`name="gorilla.csrf.Token" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if w.Code != 200 || len(match) != 2 {
		t.Fatalf("GET %s status=%d; no CSRF form", path, w.Code)
	}
	return w.Result().Cookies(), match[1]
}
func submitForm(handler http.Handler, path, user string, form url.Values, cookies []*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://example.com"+path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://example.com")
	r.Header.Set("X-Test-User", user)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}
func inputForm(token string) url.Values {
	return url.Values{"name": {"Inventory"}, "host": {"localhost"}, "port": {"5432"}, "database": {"inventory"}, "username": {"db_user"}, "password": {"do-not-echo-secret"}, "sslmode": {"disable"}, "gorilla.csrf.Token": {token}}
}

func TestConnectionHTTPCRUD(t *testing.T) {
	h, handler, s := testHandlers(t)
	cookies, token := formToken(t, handler, "/databases/connections/new", "admin")
	w := submitForm(handler, "/databases/connections/", "admin", inputForm(token), cookies)
	if w.Code != 303 || len(s.rows) != 1 {
		t.Fatalf("create = %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Location"), "result=created") {
		t.Fatal("missing success redirect")
	}
	oldCipher := append([]byte(nil), s.rows["1"].ciphertext...)
	list := requestPage(handler, "/databases/connections/", "admin")
	if list.Code != 200 || !strings.Contains(list.Body.String(), "Inventory") || strings.Contains(list.Body.String(), "do-not-echo-secret") {
		t.Fatal("list output")
	}
	edit := requestPage(handler, "/databases/connections/1/edit", "admin")
	if edit.Code != 200 || strings.Contains(edit.Body.String(), "do-not-echo-secret") || strings.Contains(edit.Body.String(), `name="password" value=`) {
		t.Fatal("edit leaked password")
	}
	form := inputForm(token)
	form.Set("password", "")
	form.Set("name", "Renamed")
	w = submitForm(handler, "/databases/connections/1/edit", "admin", form, cookies)
	if w.Code != 303 || s.rows["1"].Name != "Renamed" || !bytes.Equal(oldCipher, s.rows["1"].ciphertext) {
		t.Fatal("edit failed to retain password")
	}
	confirm := requestPage(handler, "/databases/connections/1/delete", "admin")
	if confirm.Code != 200 || len(s.rows) != 1 {
		t.Fatal("GET confirmation deleted connection")
	}
	w = submitForm(handler, "/databases/connections/1/delete", "admin", url.Values{"gorilla.csrf.Token": {token}}, cookies)
	if w.Code != 303 || len(s.rows) != 0 {
		t.Fatal("delete failed")
	}
	if w := requestPage(handler, "/databases/connections/1/edit", "admin"); w.Code != 404 {
		t.Fatal("missing connection response")
	}
	if w := requestPage(handler, "/databases/connections/", "admin"); w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive page is cacheable")
	}
	if _, err := NewHandler(h.manager, nil); err != ErrInvalidConfig {
		t.Fatal("missing layout accepted")
	}
}

func TestConnectionHTTPRejectsAccessAndCSRF(t *testing.T) {
	h, handler, s := testHandlers(t)
	cookies, token := formToken(t, handler, "/databases/connections/new", "admin")
	for _, path := range []string{"/databases/connections/", "/databases/connections/new", "/databases/connections/1/edit", "/databases/connections/1/delete"} {
		if w := requestPage(handler, path, ""); w.Code != 401 {
			t.Fatalf("anonymous GET %s: %d", path, w.Code)
		}
		if w := requestPage(handler, path, "viewer"); w.Code != 403 {
			t.Fatalf("non-admin GET %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/databases/connections/", "/databases/connections/1/edit", "/databases/connections/1/delete"} {
		for _, test := range []struct {
			user, token string
			want        int
		}{{"admin", "", 403}, {"admin", "bad-token", 403}, {"viewer", token, 403}, {"", token, 401}} {
			if w := submitForm(handler, path, test.user, inputForm(test.token), cookies); w.Code != test.want {
				t.Fatalf("POST %s: %d want %d", path, w.Code, test.want)
			}
		}
		if w := submitForm(h.routes(), path, "admin", inputForm(token), cookies); w.Code != 403 {
			t.Fatal("mutation allowed without CSRF middleware")
		}
	}
	if s.calls != 0 {
		t.Fatal("denied requests reached storage")
	}
}

func TestConnectionHTTPInvalidInputAndErrorsDoNotLeakPassword(t *testing.T) {
	_, handler, s := testHandlers(t)
	cookies, token := formToken(t, handler, "/databases/connections/new", "admin")
	form := inputForm(token)
	form.Set("port", "oops")
	form.Set("name", "<script>alert(1)</script>")
	w := submitForm(handler, "/databases/connections/", "admin", form, cookies)
	if w.Code != 400 || strings.Contains(w.Body.String(), "do-not-echo-secret") || strings.Contains(w.Body.String(), "<script>") {
		t.Fatal("invalid form leaked password or markup")
	}
	if s.calls != 0 {
		t.Fatal("invalid form reached storage")
	}
	s.err = errors.New("driver error with do-not-echo-secret")
	w = submitForm(handler, "/databases/connections/", "admin", inputForm(token), cookies)
	if w.Code != 503 || strings.Contains(w.Body.String(), "do-not-echo-secret") {
		t.Fatal("storage error leaked")
	}
	if w := submitForm(handler, "/databases/connections/?password=private", "admin", inputForm(token), cookies); w.Code != 400 {
		t.Fatal("query credentials accepted")
	}
}

func TestViewerHTTPDeniesBeforeConnecting(t *testing.T) {
	h, handler, _ := testHandlers(t)
	p := &countingProvider{}
	h.viewer, _ = NewViewer(&grantAuthorizer{}, p)
	for _, path := range []string{"/databases/1/schemas", "/databases/1/tables?schema=public", "/databases/1/table?schema=public&table=items", "/databases/1/data?schema=public&table=items"} {
		for _, user := range []string{"admin", "stranger"} {
			if w := requestPage(handler, path, user); w.Code != 403 {
				t.Fatalf("%s: %d", path, w.Code)
			}
		}
		if w := requestPage(handler, path, ""); w.Code != 401 {
			t.Fatal("anonymous viewer allowed")
		}
	}
	if p.calls != 0 {
		t.Fatal("unauthorized viewer opened target pool")
	}
}

type stubViewer struct{ err error }

func (v stubViewer) Schemas(context.Context, contracts.UserID, databaseID) ([]string, error) {
	return []string{"public"}, v.err
}
func (v stubViewer) Tables(context.Context, contracts.UserID, databaseID, string) ([]Table, error) {
	return []Table{{Name: "items", Kind: "Table"}}, v.err
}
func (v stubViewer) Details(context.Context, contracts.UserID, databaseID, string, string) (TableDetails, error) {
	return TableDetails{Schema: "public", Name: "items", Columns: []Column{{Name: "label", Type: "text", Nullable: true}}, Constraints: []Definition{{Name: "label_check", SQL: "CHECK (label <> '')"}}, Indexes: []Definition{{Name: "label_idx", SQL: "CREATE INDEX"}}}, v.err
}
func (v stubViewer) Data(_ context.Context, _ contracts.UserID, _ databaseID, _, _ string, page int) (DataPage, error) {
	if page < 1 || page > 1000000 {
		return DataPage{}, ErrInvalidInput
	}
	return DataPage{Columns: []string{"label", "empty"}, Rows: [][]Cell{{{Text: "<script>alert(1)</script>"}, {Null: true}}}, Page: page, HasNext: true}, v.err
}

func TestViewerHTTPRendersObjectsAndEscapesData(t *testing.T) {
	h, handler, _ := testHandlers(t)
	h.viewer = stubViewer{}
	for _, test := range []struct{ path, text string }{
		{"/databases/1/schemas", "public"},
		{"/databases/1/tables?schema=public", "items"},
		{"/databases/1/table?schema=public&table=items", "label_check"},
		{"/databases/1/data?schema=public&table=items&page=2", "NULL"},
	} {
		w := requestPage(handler, test.path, "viewer")
		if w.Code != 200 || !strings.Contains(w.Body.String(), test.text) {
			t.Fatalf("render %s: %d", test.path, w.Code)
		}
		if strings.Contains(w.Body.String(), "<script>") {
			t.Fatal("unescaped data")
		}
	}
	w := requestPage(handler, "/databases/1/data?schema=public&table=items&page=2", "viewer")
	if !strings.Contains(w.Body.String(), "page=1") || !strings.Contains(w.Body.String(), "page=3") {
		t.Fatal("missing pagination links")
	}
	for _, page := range []string{"bad", "0", "-1", "1000001"} {
		if w := requestPage(handler, "/databases/1/data?schema=public&table=items&page="+page, "viewer"); w.Code != 400 {
			t.Fatal("invalid page accepted")
		}
	}
	h.viewer = stubViewer{err: ErrConnection}
	if w := requestPage(handler, "/databases/1/schemas", "viewer"); w.Code != 503 {
		t.Fatal("connection error response")
	}
	path := viewerURL("1", "data", "a/b?c", "quoted\"table", 1)
	parsed, err := url.Parse(path)
	if err != nil || parsed.Query().Get("schema") != "a/b?c" || parsed.Query().Get("table") != "quoted\"table" {
		t.Fatal("object URL encoding")
	}
}
