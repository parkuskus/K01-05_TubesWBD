package platform

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"if3110.local/db-platform/internal/contracts"
)

type fakeUsers struct {
	admin          bool
	users          []User
	username, hash string
	called         bool
	problem        error
	duplicate      bool
}

func (f *fakeUsers) requireAdmin(context.Context, int64) error {
	if f.problem != nil {
		return f.problem
	}
	if !f.admin {
		return contracts.ErrForbidden
	}
	return nil
}
func (f *fakeUsers) listUsers(ctx context.Context, actor int64) ([]User, error) {
	if err := f.requireAdmin(ctx, actor); err != nil {
		return nil, err
	}
	return f.users, nil
}
func (f *fakeUsers) createUser(ctx context.Context, actor int64, username, hash string) (contracts.UserID, error) {
	if err := f.requireAdmin(ctx, actor); err != nil {
		return "", err
	}
	if f.duplicate {
		return "", ErrConflict
	}
	f.called, f.username, f.hash = true, username, hash
	return "2", nil
}
func (f *fakeUsers) updateUsername(ctx context.Context, actor, _ int64, _ string) error {
	if err := f.requireAdmin(ctx, actor); err != nil {
		return err
	}
	f.called = true
	return nil
}
func (f *fakeUsers) setPassword(ctx context.Context, actor, _ int64, _ string) error {
	if err := f.requireAdmin(ctx, actor); err != nil {
		return err
	}
	f.called = true
	return nil
}
func (f *fakeUsers) setPlatformAdmin(ctx context.Context, actor, _ int64, _ bool) error {
	if err := f.requireAdmin(ctx, actor); err != nil {
		return err
	}
	f.called = true
	return nil
}
func (f *fakeUsers) deleteUser(ctx context.Context, actor, _ int64) error {
	if err := f.requireAdmin(ctx, actor); err != nil {
		return err
	}
	f.called = true
	return nil
}

func TestCreateUserNormalizesAndHashesPassword(t *testing.T) {
	f := &fakeUsers{admin: true}
	s := &Service{users: f}
	id, err := s.CreateUser(context.Background(), "1", "  ALIce  ", "safe-password")
	if err != nil || id != "2" || f.username != "alice" || f.hash == "safe-password" || bcrypt.CompareHashAndPassword([]byte(f.hash), []byte("safe-password")) != nil {
		t.Fatalf("create returned id=%q error=%v; normalized/hashed=%t", id, err, f.username == "alice" && f.hash != "safe-password")
	}
}

func TestUserMutationsRejectNonAdminOrInvalidInput(t *testing.T) {
	ctx := context.Background()
	f := &fakeUsers{}
	s := &Service{users: f}
	for _, run := range []func() error{
		func() error { _, err := s.ListUsers(ctx, "1"); return err },
		func() error { _, err := s.CreateUser(ctx, "1", "user", "password"); return err },
		func() error { return s.UpdateUsername(ctx, "1", "2", "name") },
		func() error { return s.SetPassword(ctx, "1", "2", "password") },
		func() error { return s.SetPlatformAdmin(ctx, "1", "2", true) },
		func() error { return s.DeleteUser(ctx, "1", "2") },
	} {
		if err := run(); !errors.Is(err, contracts.ErrForbidden) || f.called {
			t.Fatalf("non-admin result: %v, called=%t", err, f.called)
		}
	}
	f.admin = true
	for _, run := range []func() error{
		func() error { _, err := s.CreateUser(ctx, "1", "  ", "password"); return err },
		func() error { _, err := s.CreateUser(ctx, "1", strings.Repeat("a", 65), "password"); return err },
		func() error { _, err := s.CreateUser(ctx, "1", "user", strings.Repeat("x", 73)); return err },
		func() error { return s.UpdateUsername(ctx, "1", "2", " ") },
		func() error { return s.SetPassword(ctx, "1", "2", "") },
		func() error { return s.DeleteUser(ctx, "1", "0") },
		func() error { return s.SetPlatformAdmin(ctx, "1", "not-id", true) },
	} {
		if err := run(); !errors.Is(err, ErrInvalidInput) || f.called {
			t.Fatalf("invalid input result: %v, called=%t", err, f.called)
		}
	}
	if _, err := s.ListUsers(ctx, "bad-id"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid actor: %v", err)
	}
}

func TestListUsersOmitsPasswordHash(t *testing.T) {
	f := &fakeUsers{admin: true, users: []User{{ID: "2", Username: "alice"}}}
	users, err := (&Service{users: f}).ListUsers(context.Background(), "1")
	if err != nil || len(users) != 1 || users[0].Username != "alice" {
		t.Fatalf("list users: %v, %v", users, err)
	}
	if _, exposed := reflect.TypeOf(users[0]).FieldByName("PasswordHash"); exposed {
		t.Fatal("public user includes password hash")
	}
}

func TestRepositoryErrorsAreNotAccessGranted(t *testing.T) {
	f := &fakeUsers{admin: true, problem: errors.New("storage failure")}
	if _, err := (&Service{users: f}).ListUsers(context.Background(), "1"); err == nil || errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("storage error: %v", err)
	}
}

func TestDuplicateUsernameReturnsConflict(t *testing.T) {
	f := &fakeUsers{admin: true, duplicate: true}
	if _, err := (&Service{users: f}).CreateUser(context.Background(), "1", "alice", "password"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate result: %v", err)
	}
}
