package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

type grantAuthorizer struct {
	grants  map[contracts.UserID]map[databaseID]contracts.AccessLevel
	err     error
	actions []contracts.DatabaseAction
}

func (a *grantAuthorizer) RequirePlatformAdmin(ctx context.Context, user contracts.UserID) error {
	return (fakeAuthorizer{}).RequirePlatformAdmin(ctx, user)
}
func (a *grantAuthorizer) RequireDatabaseAccess(_ context.Context, user contracts.UserID, id databaseID, action contracts.DatabaseAction) error {
	a.actions = append(a.actions, action)
	if a.err != nil {
		return a.err
	}
	if !a.grants[user][id].Allows(action) {
		return contracts.ErrForbidden
	}
	return nil
}

type countingProvider struct {
	calls int
	db    *sql.DB
	err   error
}

func (p *countingProvider) Pool(context.Context, databaseID) (*sql.DB, error) {
	p.calls++
	return p.db, p.err
}

func TestAccessibleFiltersGrantsAndDoesNotTreatAdminAsGrant(t *testing.T) {
	m, s := testManager(t)
	one, err := m.Create(context.Background(), "admin", validInput())
	if err != nil {
		t.Fatal(err)
	}
	in := validInput()
	in.Name = "Hidden"
	_, err = m.Create(context.Background(), "admin", in)
	if err != nil {
		t.Fatal(err)
	}
	a := &grantAuthorizer{grants: map[contracts.UserID]map[databaseID]contracts.AccessLevel{"viewer": {one.ID: contracts.AccessViewOnly}}}
	m.authorizer = a
	items, err := m.Accessible(context.Background(), "viewer")
	if err != nil || len(items) != 1 || items[0].ID != one.ID {
		t.Fatal("incorrect accessible databases")
	}
	items, err = m.Accessible(context.Background(), "admin")
	if err != nil || len(items) != 0 {
		t.Fatal("admin received implicit database access")
	}
	before := s.calls
	if _, err := m.Accessible(context.Background(), ""); err != contracts.ErrForbidden || s.calls != before {
		t.Fatal("anonymous access reached storage")
	}
	a.err = errors.New("sensitive authorizer error")
	if _, err := m.Accessible(context.Background(), "viewer"); err != ErrUnavailable {
		t.Fatal("authorization failure was not fail-closed")
	}
}

func TestViewerAuthorizationBeforeProvider(t *testing.T) {
	a := &grantAuthorizer{}
	p := &countingProvider{err: errors.New("private DSN password")}
	v, err := NewViewer(a, p)
	if err != nil {
		t.Fatal(err)
	}
	operations := []struct {
		action contracts.DatabaseAction
		call   func(contracts.UserID) error
	}{
		{contracts.ActionViewSchema, func(u contracts.UserID) error { _, e := v.Schemas(context.Background(), u, "1"); return e }},
		{contracts.ActionViewSchema, func(u contracts.UserID) error { _, e := v.Tables(context.Background(), u, "1", "public"); return e }},
		{contracts.ActionViewSchema, func(u contracts.UserID) error {
			_, e := v.Details(context.Background(), u, "1", "public", "items")
			return e
		}},
		{contracts.ActionViewData, func(u contracts.UserID) error {
			_, e := v.Data(context.Background(), u, "1", "public", "items", 1)
			return e
		}},
	}
	for _, op := range operations {
		for _, user := range []contracts.UserID{"", "admin", "stranger"} {
			if e := op.call(user); e != contracts.ErrForbidden {
				t.Fatalf("denied access: %v", e)
			}
		}
	}
	if p.calls != 0 {
		t.Fatal("unauthorized request reached provider")
	}
	a.grants = map[contracts.UserID]map[databaseID]contracts.AccessLevel{"viewer": {"1": contracts.AccessViewOnly}, "editor": {"1": contracts.AccessViewEdit}}
	for _, user := range []contracts.UserID{"viewer", "editor"} {
		for _, op := range operations {
			if e := op.call(user); e != ErrConnection {
				t.Fatal("provider error leaked")
			}
			if a.actions[len(a.actions)-1] != op.action {
				t.Fatal("wrong authorization action")
			}
		}
	}
	if p.calls != 8 {
		t.Fatal("allowed request did not reach provider")
	}
	a.err = errors.New("secret")
	if _, e := v.Schemas(context.Background(), "viewer", "1"); e != ErrUnavailable {
		t.Fatal("authorizer error leaked")
	}
}

func TestViewerObjectNamesAndErrors(t *testing.T) {
	for _, name := range []string{"public", "weird\";name", "a/b?c", "日本語"} {
		if !visibleSchema(name) {
			t.Fatalf("valid name rejected: %q", name)
		}
	}
	for _, name := range []string{"", "pg_catalog", "pg_temp_1", "information_schema", "if3110_platform", "bad\x00name"} {
		if visibleSchema(name) {
			t.Fatalf("invalid schema accepted: %q", name)
		}
	}
	for _, test := range []struct{ err, want error }{
		{&pgconn.PgError{Code: "42501", Message: "secret"}, contracts.ErrForbidden},
		{&pgconn.PgError{Code: "42P01"}, ErrNotFound},
		{&pgconn.PgError{Code: "57014"}, context.DeadlineExceeded},
		{errors.New("secret"), ErrUnavailable},
	} {
		if got := viewerError(test.err); got != test.want {
			t.Fatalf("error = %v", got)
		}
	}
	if (Cell{Null: true}).String() != "NULL" || (Cell{Text: "long", Truncated: true}).String() != "long…" {
		t.Fatal("cell formatting")
	}
}
