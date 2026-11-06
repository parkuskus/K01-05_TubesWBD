package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/database"
)

type databaseFixture struct {
	admin bool
	err   error
	calls []string
}

func (f *databaseFixture) RequirePlatformAdmin(context.Context, contracts.UserID) error {
	if !f.admin {
		return contracts.ErrForbidden
	}
	return nil
}
func (f *databaseFixture) List(context.Context, contracts.UserID) ([]database.Connection, error) {
	f.calls = append(f.calls, "list")
	return []database.Connection{{ID: "2", Name: "Target", Host: "localhost", Port: 5432}}, f.err
}
func (f *databaseFixture) Get(context.Context, contracts.UserID, contracts.DatabaseID) (database.Connection, error) {
	f.calls = append(f.calls, "get")
	return database.Connection{ID: "2", Name: "Target", Host: "localhost"}, f.err
}
func (f *databaseFixture) Create(_ context.Context, _ contracts.UserID, in database.ConnectionInput) (database.Connection, error) {
	f.calls = append(f.calls, "create:"+*in.Password)
	return database.Connection{ID: "2", Name: in.Name}, f.err
}
func (f *databaseFixture) Update(_ context.Context, _ contracts.UserID, _ contracts.DatabaseID, in database.ConnectionInput) (database.Connection, error) {
	f.calls = append(f.calls, "update")
	return database.Connection{ID: "2", Name: in.Name}, f.err
}
func (f *databaseFixture) Delete(context.Context, contracts.UserID, contracts.DatabaseID) error {
	f.calls = append(f.calls, "delete")
	return f.err
}
func (f *databaseFixture) TestConnection(context.Context, contracts.UserID, contracts.DatabaseID) error {
	f.calls = append(f.calls, "test")
	return f.err
}
func (f *databaseFixture) Accessible(context.Context, contracts.UserID) ([]database.DatabaseChoice, error) {
	f.calls = append(f.calls, "accessible")
	return []database.DatabaseChoice{{ID: "2", Name: "Target", Level: contracts.AccessViewOnly}}, f.err
}
func (f *databaseFixture) Describe(context.Context, contracts.UserID, contracts.DatabaseID) (database.DatabaseChoice, error) {
	f.calls = append(f.calls, "describe")
	return database.DatabaseChoice{ID: "2", Name: "Target", Level: contracts.AccessViewOnly}, f.err
}

type viewerFixture struct {
	err   error
	calls []string
}

func (f *viewerFixture) Schemas(context.Context, contracts.UserID, contracts.DatabaseID) ([]string, error) {
	f.calls = append(f.calls, "schemas")
	return []string{"public"}, f.err
}
func (f *viewerFixture) Tables(context.Context, contracts.UserID, contracts.DatabaseID, string) ([]database.Table, error) {
	f.calls = append(f.calls, "tables")
	return []database.Table{{Name: "items", Kind: "r"}}, f.err
}
func (f *viewerFixture) Details(context.Context, contracts.UserID, contracts.DatabaseID, string, string) (database.TableDetails, error) {
	f.calls = append(f.calls, "table")
	return database.TableDetails{Schema: "public", Name: "items"}, f.err
}
func (f *viewerFixture) Data(context.Context, contracts.UserID, contracts.DatabaseID, string, string, int) (database.DataPage, error) {
	f.calls = append(f.calls, "data")
	return database.DataPage{Columns: []string{"x"}, Rows: [][]database.Cell{{{Null: true}}}, Page: 1}, f.err
}
func (f *viewerFixture) Diff(context.Context, contracts.UserID, contracts.DatabaseID, string, string) (database.SchemaDiff, error) {
	f.calls = append(f.calls, "diff")
	return database.SchemaDiff{ReferenceSchema: "a", TargetSchema: "b", Changes: []database.SchemaChange{}, Unsupported: []database.SchemaObject{}}, f.err
}

func TestDatabaseJSONRoutes(t *testing.T) {
	input := `{"name":"Target","host":"localhost","port":5432,"database":"db","username":"u","ssl_mode":"disable","password":"secret"}`
	for _, tc := range []struct {
		method, path, body, call, contains string
		status                             int
	}{
		{"GET", "/api/v1/connections", "", "list", `"connections"`, 200},
		{"GET", "/api/v1/connections/2", "", "get", `"connection"`, 200},
		{"POST", "/api/v1/connections", input, "create:secret", `"id":"2"`, 201},
		{"PATCH", "/api/v1/connections/2", input, "update", `"connection"`, 200},
		{"DELETE", "/api/v1/connections/2", "", "delete", "", 204},
		{"POST", "/api/v1/connections/2/test", "", "test", "", 204},
		{"GET", "/api/v1/databases", "", "accessible", `"databases"`, 200},
		{"GET", "/api/v1/databases/2", "", "describe", `"level":1`, 200},
		{"GET", "/api/v1/databases/2/schemas", "", "schemas", `"public"`, 200},
		{"GET", "/api/v1/databases/2/tables?schema=public", "", "tables", `"items"`, 200},
		{"GET", "/api/v1/databases/2/table?schema=public&table=items", "", "table", `"columns":[]`, 200},
		{"GET", "/api/v1/databases/2/data?schema=public&table=items&page=1", "", "data", `"null":true`, 200},
		{"GET", "/api/v1/databases/2/schema-diff?reference_schema=a&target_schema=b", "", "diff", `"reference_schema":"a"`, 200},
	} {
		t.Run(tc.path+tc.method, func(t *testing.T) {
			f := &databaseFixture{admin: true}
			v := &viewerFixture{}
			h, err := NewDatabaseHandler(f, v, f, &testSessions{user: "1"})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.contains) || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), `"host":"localhost"`) && strings.HasPrefix(tc.path, "/api/v1/databases") {
				t.Fatalf("%d %s calls=%v %v", w.Code, w.Body.String(), f.calls, v.calls)
			}
			calls := append(f.calls, v.calls...)
			if len(calls) != 1 || calls[0] != tc.call {
				t.Fatalf("calls=%v want %s", calls, tc.call)
			}
		})
	}
}

func TestDatabaseJSONRejectsUnsafeRequests(t *testing.T) {
	for _, tc := range []struct {
		method, path, body string
		admin              bool
		user               contracts.UserID
		status             int
	}{
		{"GET", "/api/v1/connections", "", true, "", 401},
		{"GET", "/api/v1/connections", "", false, "1", 403},
		{"POST", "/api/v1/connections", `{"password":"secret","password":"other"}`, true, "1", 400},
		{"PATCH", "/api/v1/connections/2", `{"password":null}`, true, "1", 400},
		{"GET", "/api/v1/databases/0", "", true, "1", 400},
		{"GET", "/api/v1/databases/2/data?schema=public&table=items&page=0", "", true, "1", 400},
		{"GET", "/api/v1/databases/2/data?schema=public&table=items&page=1&page=2", "", true, "1", 400},
		{"GET", "/api/v1/databases/2/schema-diff?reference_schema=a&target_schema=a", "", true, "1", 400},
		{"GET", "/api/v1/databases/2/schemas?extra=x", "", true, "1", 400},
	} {
		t.Run(tc.path+tc.body, func(t *testing.T) {
			f := &databaseFixture{admin: tc.admin}
			v := &viewerFixture{}
			h, err := NewDatabaseHandler(f, v, f, &testSessions{user: tc.user})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || len(f.calls)+len(v.calls) != 0 {
				t.Fatalf("%d %s calls=%v %v", w.Code, w.Body.String(), f.calls, v.calls)
			}
		})
	}
	f := &databaseFixture{admin: true, err: errors.New("secret DSN")}
	h, _ := NewDatabaseHandler(f, &viewerFixture{}, f, &testSessions{user: "1"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/connections", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("leaked error: %d %s", w.Code, w.Body.String())
	}
}

func TestDatabaseViewerJSONShape(t *testing.T) {
	if got := safeDetails(database.TableDetails{Schema: "public", Name: "items", Columns: []database.Column{{Name: "id", Type: "integer", Nullable: false}}}); got == nil {
		t.Fatal("missing details")
	}
	f := &databaseFixture{admin: true}
	v := &viewerFixture{}
	h, err := NewDatabaseHandler(f, v, f, &testSessions{user: "1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, want string }{
		{"/api/v1/databases/2/tables?schema=public", `"name":"items","kind":"r"`},
		{"/api/v1/databases/2/data?schema=public&table=items&page=1", `"rows":[[{"text":"","null":true,"truncated":false}]]`},
		{"/api/v1/databases/2/schema-diff?reference_schema=a&target_schema=b", `"unsupported":[]`},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
}

func TestDatabaseConnectionValidationFields(t *testing.T) {
	f := &databaseFixture{admin: true}
	h, err := NewDatabaseHandler(f, &viewerFixture{}, f, &testSessions{user: "1"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/v1/connections", strings.NewReader(`{"name":"Target","host":"https://secret:password@host/db","port":0,"database":"db","username":"user","ssl_mode":"disable","password":"private-secret"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 422 || !strings.Contains(w.Body.String(), `"host"`) || !strings.Contains(w.Body.String(), `"port"`) || strings.Contains(w.Body.String(), "private-secret") || strings.Contains(w.Body.String(), "https://") || len(f.calls) != 0 {
		t.Fatalf("unsafe validation: %d %s calls=%v", w.Code, w.Body.String(), f.calls)
	}
}
