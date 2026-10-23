package platform

import (
	"context"
	"errors"
	"testing"

	"if3110.local/db-platform/internal/contracts"
)

type fakeAuthorization struct {
	admins  map[int64]bool
	grants  map[int64]map[int64][]contracts.AccessLevel
	problem error
}

func (f *fakeAuthorization) isPlatformAdmin(_ context.Context, user int64) (bool, error) {
	if f.problem != nil {
		return false, f.problem
	}
	return f.admins[user], nil
}

func (f *fakeAuthorization) databaseAccess(_ context.Context, user, database int64) ([]contracts.AccessLevel, error) {
	if f.problem != nil {
		return nil, f.problem
	}
	return f.grants[user][database], nil
}

func TestRequirePlatformAdmin(t *testing.T) {
	storageFailure := errors.New("storage unavailable")
	for _, tc := range []struct {
		name    string
		user    contracts.UserID
		admin   bool
		problem error
		want    error
	}{
		{"admin", "1", true, nil, nil},
		{"ordinary user", "1", false, nil, contracts.ErrForbidden},
		{"missing user", "9", false, nil, contracts.ErrForbidden},
		{"invalid user", "oops", true, nil, contracts.ErrForbidden},
		{"zero user", "0", true, nil, contracts.ErrForbidden},
		{"storage failure", "1", true, storageFailure, storageFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAuthorization{admins: map[int64]bool{1: tc.admin}, problem: tc.problem}
			err := (&Service{authorization: f}).RequirePlatformAdmin(context.Background(), tc.user)
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if tc.problem != nil && errors.Is(err, contracts.ErrForbidden) {
				t.Fatalf("storage failure hidden: %v", err)
			}
		})
	}
}

func TestRequireDatabaseAccessActions(t *testing.T) {
	read := []contracts.DatabaseAction{contracts.ActionViewSchema, contracts.ActionViewData, contracts.ActionViewMigrationHistory}
	write := []contracts.DatabaseAction{contracts.ActionCreateMigration, contracts.ActionEditMigration, contracts.ActionRunMigration}
	for _, tc := range []struct {
		name       string
		levels     []contracts.AccessLevel
		admin      bool
		view, edit bool
	}{
		{"view only", []contracts.AccessLevel{contracts.AccessViewOnly}, false, true, false},
		{"view and edit", []contracts.AccessLevel{contracts.AccessViewEdit}, false, true, true},
		{"no group", nil, false, false, false},
		{"admin without grant", nil, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAuthorization{admins: map[int64]bool{1: tc.admin}, grants: map[int64]map[int64][]contracts.AccessLevel{1: {2: tc.levels}}}
			s := &Service{authorization: f}
			for _, action := range read {
				err := s.RequireDatabaseAccess(context.Background(), "1", "2", action)
				if tc.view && err != nil || !tc.view && !errors.Is(err, contracts.ErrForbidden) {
					t.Errorf("read %d: %v", action, err)
				}
			}
			for _, action := range write {
				err := s.RequireDatabaseAccess(context.Background(), "1", "2", action)
				if tc.edit && err != nil || !tc.edit && !errors.Is(err, contracts.ErrForbidden) {
					t.Errorf("write %d: %v", action, err)
				}
			}
		})
	}
}

func TestEffectiveAccessStrongestGrantInEitherOrder(t *testing.T) {
	for _, levels := range [][]contracts.AccessLevel{{contracts.AccessViewOnly, contracts.AccessViewEdit}, {contracts.AccessViewEdit, contracts.AccessViewOnly}} {
		f := &fakeAuthorization{grants: map[int64]map[int64][]contracts.AccessLevel{1: {2: levels}}}
		if err := (&Service{authorization: f}).RequireDatabaseAccess(context.Background(), "1", "2", contracts.ActionRunMigration); err != nil {
			t.Fatalf("grants %v: %v", levels, err)
		}
	}
}

func TestUnknownGrantAndActionFailClosed(t *testing.T) {
	f := &fakeAuthorization{grants: map[int64]map[int64][]contracts.AccessLevel{1: {2: {contracts.AccessViewEdit, 255}}}}
	s := &Service{authorization: f}
	for _, action := range []contracts.DatabaseAction{contracts.ActionViewSchema, contracts.ActionRunMigration, 255} {
		if err := s.RequireDatabaseAccess(context.Background(), "1", "2", action); !errors.Is(err, contracts.ErrForbidden) {
			t.Fatalf("invalid grant, action %d: %v", action, err)
		}
	}
	f.grants[1][2] = []contracts.AccessLevel{contracts.AccessViewEdit}
	if err := s.RequireDatabaseAccess(context.Background(), "1", "2", 0); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("invalid action: %v", err)
	}
	for _, ids := range [][2]string{{"", "2"}, {"1", ""}, {"bad", "2"}, {"1", "-1"}, {"9", "2"}, {"1", "9"}} {
		if err := s.RequireDatabaseAccess(context.Background(), contracts.UserID(ids[0]), contracts.DatabaseID(ids[1]), contracts.ActionViewSchema); !errors.Is(err, contracts.ErrForbidden) {
			t.Fatalf("IDs %v: %v", ids, err)
		}
	}
	f.problem = errors.New("database unavailable")
	if err := s.RequireDatabaseAccess(context.Background(), "1", "2", contracts.ActionViewSchema); !errors.Is(err, f.problem) || errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("storage failure hidden: %v", err)
	}
}

func TestRevokingMembershipOrGrantDeniesNextCheck(t *testing.T) {
	f := &fakeAuthorization{grants: map[int64]map[int64][]contracts.AccessLevel{1: {2: {contracts.AccessViewEdit}}}}
	s := &Service{authorization: f}
	check := func(want error) {
		t.Helper()
		err := s.RequireDatabaseAccess(context.Background(), "1", "2", contracts.ActionEditMigration)
		if want == nil && err != nil || want != nil && !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	}
	check(nil)
	delete(f.grants[1], 2) // membership removed
	check(contracts.ErrForbidden)
	f.grants[1][2] = []contracts.AccessLevel{contracts.AccessViewEdit}
	check(nil)
	f.grants[1][2] = nil // grant revoked
	check(contracts.ErrForbidden)
}
