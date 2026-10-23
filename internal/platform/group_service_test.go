package platform

import (
	"context"
	"errors"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/contracts"
)

type fakeGroups struct {
	admin   bool
	called  bool
	name    string
	members map[int64]bool
	problem error
}

func (f *fakeGroups) allowed() error {
	if f.problem != nil {
		return f.problem
	}
	if !f.admin {
		return contracts.ErrForbidden
	}
	return nil
}
func (f *fakeGroups) listGroups(context.Context, int64) ([]Group, error) {
	if err := f.allowed(); err != nil {
		return nil, err
	}
	return []Group{{ID: "2", Name: f.name}}, nil
}
func (f *fakeGroups) createGroup(_ context.Context, _ int64, name string) (contracts.GroupID, error) {
	if err := f.allowed(); err != nil {
		return "", err
	}
	f.called, f.name = true, name
	return "2", nil
}
func (f *fakeGroups) renameGroup(_ context.Context, _, _ int64, name string) error {
	if err := f.allowed(); err != nil {
		return err
	}
	if name == "taken" {
		return ErrConflict
	}
	f.called, f.name = true, name
	return nil
}
func (f *fakeGroups) deleteGroup(context.Context, int64, int64) error {
	if err := f.allowed(); err != nil {
		return err
	}
	f.called = true
	return nil
}
func (f *fakeGroups) listMembers(context.Context, int64, int64) ([]User, error) {
	if err := f.allowed(); err != nil {
		return nil, err
	}
	return []User{{ID: "3", Username: "member"}}, nil
}
func (f *fakeGroups) setMembership(_ context.Context, _, user, _ int64, member bool) error {
	if err := f.allowed(); err != nil {
		return err
	}
	f.called = true
	if f.members == nil {
		f.members = make(map[int64]bool)
	}
	if member {
		f.members[user] = true
	} else {
		delete(f.members, user)
	}
	return nil
}

func TestGroupCRUDRequiresCurrentAdmin(t *testing.T) {
	ctx := context.Background()
	f := &fakeGroups{}
	s := &Service{groups: f}
	operations := []func() error{
		func() error { _, err := s.ListGroups(ctx, "1"); return err },
		func() error { _, err := s.CreateGroup(ctx, "1", "Editors"); return err },
		func() error { return s.RenameGroup(ctx, "1", "2", "Writers") },
		func() error { return s.DeleteGroup(ctx, "1", "2") },
		func() error { _, err := s.ListMembers(ctx, "1", "2"); return err },
		func() error { return s.SetMembership(ctx, "1", "3", "2", true) },
	}
	for _, operation := range operations {
		if err := operation(); !errors.Is(err, contracts.ErrForbidden) || f.called {
			t.Fatalf("non-admin group operation: %v, changed=%t", err, f.called)
		}
	}
	f.admin = true
	id, err := s.CreateGroup(ctx, "1", "  Editors  ")
	if err != nil || id != "2" || f.name != "Editors" {
		t.Fatalf("create group: id=%q, name=%q, error=%v", id, f.name, err)
	}
	if err := s.RenameGroup(ctx, "1", id, " Writers "); err != nil || f.name != "Writers" {
		t.Fatalf("rename group: name=%q, error=%v", f.name, err)
	}
	groups, err := s.ListGroups(ctx, "1")
	if err != nil || len(groups) != 1 || groups[0].Name != "Writers" {
		t.Fatalf("list groups: %v, %v", groups, err)
	}
	f.admin = false
	if err := s.DeleteGroup(ctx, "1", id); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("revoked admin still has access: %v", err)
	}
}

func TestRenameGroupRejectsBlankOrDuplicateName(t *testing.T) {
	ctx := context.Background()
	f := &fakeGroups{admin: true}
	s := &Service{groups: f}
	for _, name := range []string{"  ", strings.Repeat("a", 65)} {
		if err := s.RenameGroup(ctx, "1", "2", name); !errors.Is(err, ErrInvalidInput) || f.called {
			t.Fatalf("invalid group name accepted: %v", err)
		}
	}
	if err := s.RenameGroup(ctx, "1", "2", "taken"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := s.CreateGroup(ctx, "1", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("blank create: %v", err)
	}
	if _, err := s.ListGroups(ctx, "0"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid actor: %v", err)
	}
	if err := s.DeleteGroup(ctx, "1", "bad"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid group ID: %v", err)
	}
}

func TestSetMembershipAddsRemovesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := &fakeGroups{admin: true}
	s := &Service{groups: f}
	for _, member := range []bool{true, true, false, false} {
		if err := s.SetMembership(ctx, "1", "3", "2", member); err != nil {
			t.Fatal(err)
		}
		if f.members[3] != member {
			t.Fatalf("member after %t: %v", member, f.members)
		}
	}
	for _, ids := range [][2]contracts.UserID{{"0", "2"}, {"3", "bad"}} {
		if err := s.SetMembership(ctx, "1", ids[0], contracts.GroupID(ids[1]), true); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid membership ID: %v", err)
		}
	}
}

func TestListMembersReturnsNoHash(t *testing.T) {
	users, err := (&Service{groups: &fakeGroups{admin: true}}).ListMembers(context.Background(), "1", "2")
	if err != nil || len(users) != 1 || users[0].Username != "member" {
		t.Fatalf("members: %v, %v", users, err)
	}
	// Public User has no password field; the repository query is checked by the PostgreSQL test.
}
