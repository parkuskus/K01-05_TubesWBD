package platform

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"if3110.local/db-platform/internal/contracts"
)

type fakeGrants struct {
	admin   bool
	called  bool
	problem error
	items   map[contracts.DatabaseID]contracts.AccessLevel
}

func (f *fakeGrants) listGrants(_ context.Context, _, group int64) ([]Grant, error) {
	if f.problem != nil {
		return nil, f.problem
	}
	if !f.admin {
		return nil, contracts.ErrForbidden
	}
	f.called = true
	var grants []Grant
	for database, level := range f.items {
		grants = append(grants, Grant{GroupID: contracts.GroupID(strconv.FormatInt(group, 10)), DatabaseID: database, Level: level})
	}
	return grants, nil
}

func (f *fakeGrants) setGrant(_ context.Context, _, group, database int64, level contracts.AccessLevel) error {
	if f.problem != nil {
		return f.problem
	}
	if !f.admin {
		return contracts.ErrForbidden
	}
	f.called = true
	if f.items == nil {
		f.items = make(map[contracts.DatabaseID]contracts.AccessLevel)
	}
	id := contracts.DatabaseID(strconv.FormatInt(database, 10))
	if level == contracts.AccessNone {
		delete(f.items, id)
	} else {
		f.items[id] = level
	}
	return nil
}

func TestSetGrantCreatesUpdatesAndRemoves(t *testing.T) {
	ctx := context.Background()
	f := &fakeGrants{admin: true}
	s := &Service{grants: f}
	for _, tc := range []struct{ level, want contracts.AccessLevel }{
		{contracts.AccessViewOnly, contracts.AccessViewOnly},
		{contracts.AccessViewEdit, contracts.AccessViewEdit},
		{contracts.AccessNone, contracts.AccessNone},
	} {
		if err := s.SetGrant(ctx, "1", "2", "3", tc.level); err != nil {
			t.Fatal(err)
		}
		grants, err := s.ListGrants(ctx, "1", "2")
		if err != nil {
			t.Fatal(err)
		}
		if tc.want == contracts.AccessNone {
			if len(grants) != 0 {
				t.Fatalf("AccessNone persisted: %v", grants)
			}
		} else if len(grants) != 1 || grants[0] != (Grant{GroupID: "2", DatabaseID: "3", Level: tc.want}) {
			t.Fatalf("grant after %d: %v", tc.level, grants)
		}
	}
}

func TestGrantRejectsNonAdminUnknownLevelAndInvalidIDs(t *testing.T) {
	ctx := context.Background()
	f := &fakeGrants{}
	s := &Service{grants: f}
	if _, err := s.ListGrants(ctx, "1", "2"); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("non-admin list: %v", err)
	}
	if err := s.SetGrant(ctx, "1", "2", "3", contracts.AccessViewOnly); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("non-admin set: %v", err)
	}
	if f.called {
		t.Fatal("non-admin changed grants")
	}
	f.admin = true
	for _, level := range []contracts.AccessLevel{3, 255} {
		if err := s.SetGrant(ctx, "1", "2", "3", level); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("unknown level %d: %v", level, err)
		}
	}
	for _, ids := range [][3]string{{"0", "2", "3"}, {"1", "bad", "3"}, {"1", "2", "-1"}, {"1", "2", "9223372036854775808"}} {
		if err := s.SetGrant(ctx, contracts.UserID(ids[0]), contracts.GroupID(ids[1]), contracts.DatabaseID(ids[2]), contracts.AccessViewOnly); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid IDs %v: %v", ids, err)
		}
	}
	if _, err := s.ListGrants(ctx, "bad", "2"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid actor: %v", err)
	}
	if _, err := s.ListGrants(ctx, "1", "0"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid group: %v", err)
	}
	if f.called {
		t.Fatal("invalid input reached repository")
	}
	f.problem = errors.New("storage failure")
	if err := s.SetGrant(ctx, "1", "2", "3", contracts.AccessNone); err == nil || errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("storage error: %v", err)
	}
}

func TestListGrantsReturnsGroupDatabaseAndLevelOnly(t *testing.T) {
	f := &fakeGrants{admin: true, items: map[contracts.DatabaseID]contracts.AccessLevel{"3": contracts.AccessViewOnly}}
	grants, err := (&Service{grants: f}).ListGrants(context.Background(), "1", "2")
	if err != nil || len(grants) != 1 || grants[0] != (Grant{GroupID: "2", DatabaseID: "3", Level: contracts.AccessViewOnly}) {
		t.Fatalf("list grants: %v, %v", grants, err)
	}
	if reflect.TypeOf(grants[0]).NumField() != 3 {
		t.Fatal("grant exposes fields beyond group, database, level")
	}
}
