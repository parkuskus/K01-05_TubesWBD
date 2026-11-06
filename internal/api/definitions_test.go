package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/definition"
)

type definitionFixture struct {
	err   error
	calls []string
}

func (f *definitionFixture) List(_ context.Context, actor contracts.UserID, id contracts.DatabaseID) ([]definition.Definition, error) {
	f.calls = append(f.calls, "list:"+string(actor)+":"+string(id))
	return []definition.Definition{{ID: "3", DatabaseID: id, Version: 1, Revision: 1, Input: definition.Input{Title: "First", UpSQL: "SELECT 1", DownSQL: "SELECT 2", Mode: "NORMAL"}}}, f.err
}
func (f *definitionFixture) Get(_ context.Context, _ contracts.UserID, id contracts.DatabaseID, migrationID string) (definition.Definition, error) {
	f.calls = append(f.calls, "get:"+migrationID)
	return definition.Definition{ID: migrationID, DatabaseID: id, Revision: 1, Input: definition.Input{Title: "First", UpSQL: "SELECT 1", DownSQL: "SELECT 2", Mode: "NORMAL"}}, f.err
}
func (f *definitionFixture) Create(_ context.Context, _ contracts.UserID, id contracts.DatabaseID, in definition.Input) (definition.Definition, error) {
	f.calls = append(f.calls, "create:"+in.UpSQL)
	return definition.Definition{ID: "3", DatabaseID: id, Revision: 1, Input: in}, f.err
}
func (f *definitionFixture) Revise(_ context.Context, _ contracts.UserID, id contracts.DatabaseID, migrationID string, expected int, in definition.Input) (definition.Definition, error) {
	f.calls = append(f.calls, "revise:"+migrationID+":"+strconv.Itoa(expected))
	return definition.Definition{ID: migrationID, DatabaseID: id, Revision: expected + 1, Input: in}, f.err
}
func (f *definitionFixture) Validate(_ context.Context, _ contracts.UserID, _ contracts.DatabaseID, migrationID string) (definition.ValidationReport, error) {
	f.calls = append(f.calls, "validate:"+migrationID)
	return definition.ValidationReport{MigrationID: migrationID, Revision: 2, Checksum: "abc", Valid: true, Errors: []definition.ValidationIssue{}, Warnings: []definition.ValidationIssue{{Code: "COMPATIBILITY_UNVERIFIED", Message: "Review compatibility."}}, CompatibilityNotes: "Coordinate release."}, f.err
}

func TestDefinitionAPIRoutes(t *testing.T) {
	in := `{"title":"First","up_sql":"SELECT 1","down_sql":"SELECT 2","mode":"NORMAL"}`
	for _, tc := range []struct {
		method, path, body, call, contains string
		status                             int
	}{
		{"GET", "/api/v1/databases/2/migrations", "", "list:1:2", `"migrations"`, 200},
		{"GET", "/api/v1/databases/2/migrations/3", "", "get:3", `"id":"3"`, 200},
		{"POST", "/api/v1/databases/2/migrations", in, "create:SELECT 1", `"id":"3"`, 201},
		{"PATCH", "/api/v1/databases/2/migrations/3", `{"expected_revision":1,"title":"First","up_sql":"SELECT 1","down_sql":"SELECT 2","mode":"NORMAL"}`, "revise:3:1", `"revision":2`, 200},
		{"POST", "/api/v1/databases/2/migrations/3/validate", "", "validate:3", `"compatibility_notes":"Coordinate release."`, 200},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			f := &definitionFixture{}
			h, err := NewDefinitionHandler(f, &testSessions{user: "1"})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.contains) || len(f.calls) != 1 || f.calls[0] != tc.call {
				t.Fatalf("%d %s %v", w.Code, w.Body.String(), f.calls)
			}
		})
	}
}

func TestDefinitionAPIRejectsInvalidAndLeaksNoErrors(t *testing.T) {
	for _, tc := range []struct {
		user               contracts.UserID
		method, path, body string
		status             int
	}{
		{"", "GET", "/api/v1/databases/2/migrations", "", 401},
		{"1", "GET", "/api/v1/databases/0/migrations", "", 400},
		{"1", "GET", "/api/v1/databases/2/migrations/garbage", "", 400},
		{"1", "POST", "/api/v1/databases/2/migrations/not-id/validate", "", 400},
		{"1", "GET", "/api/v1/databases/2/migrations/3/validate", "", 405},
		{"1", "PATCH", "/api/v1/databases/2/migrations/3", `{"expected_revision":0}`, 400},
		{"1", "POST", "/api/v1/databases/2/migrations", `{"up_sql":null}`, 400},
		{"1", "POST", "/api/v1/databases/2/migrations", `{"up_sql":"SELECT 1","up_sql":"SELECT 2"}`, 400},
		{"1", "GET", "/api/v1/databases/2/migrations?extra=x", "", 400},
	} {
		f := &definitionFixture{}
		h, err := NewDefinitionHandler(f, &testSessions{user: tc.user})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status || len(f.calls) != 0 {
			t.Fatalf("%d %s calls=%v", w.Code, w.Body.String(), f.calls)
		}
	}
	f := &definitionFixture{err: errors.New("secret DSN")}
	h, _ := NewDefinitionHandler(f, &testSessions{user: "1"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/databases/2/migrations", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("error leaked: %d %s", w.Code, w.Body.String())
	}
	f.err = contracts.ErrForbidden
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/databases/2/migrations", nil))
	if w.Code != 403 {
		t.Fatalf("denied: %d", w.Code)
	}
	f.err = definition.ErrConflict
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/databases/2/migrations", nil))
	if w.Code != 409 {
		t.Fatalf("conflict: %d", w.Code)
	}
}

func TestDefinitionAPIAcceptsLongSQL(t *testing.T) {
	f := &definitionFixture{}
	h, err := NewDefinitionHandler(f, &testSessions{user: "1"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/v1/databases/2/migrations", strings.NewReader(`{"title":"Large","up_sql":"`+strings.Repeat("x", 20000)+`","down_sql":"SELECT 1","mode":"NORMAL"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 201 || len(f.calls) != 1 {
		t.Fatalf("long SQL rejected: %d %s", w.Code, w.Body.String())
	}
}
