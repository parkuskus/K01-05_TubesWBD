package auth

import (
	"context"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"if3110.local/db-platform/internal/contracts"
)

type fakeUserStore struct {
	users map[string]userRecord
}

func (s *fakeUserStore) FindByUsername(_ context.Context, username string) (userRecord, error) {
	record, ok := s.users[username]
	if !ok {
		return userRecord{}, ErrInvalidCredentials
	}
	return record, nil
}

func (s *fakeUserStore) EnsureInitialAdmin(_ context.Context, username, passwordHash string) error {
	if len(s.users) != 0 {
		return nil
	}
	if username == "" || passwordHash == "" {
		return ErrBootstrapCredentialsRequired
	}
	s.users[username] = userRecord{
		ID:              "1",
		Username:        username,
		PasswordHash:    passwordHash,
		IsPlatformAdmin: true,
	}
	return nil
}

func TestAuthenticatorRequiresExactUsernameCase(t *testing.T) {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("correct horse battery staple"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeUserStore{users: map[string]userRecord{
		"admin": {ID: "42", Username: "admin", PasswordHash: string(passwordHash), IsPlatformAdmin: true},
	}}
	authenticator := &Authenticator{store: store}

	userID, err := authenticator.Authenticate(context.Background(), "  admin ", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if userID != contracts.UserID("42") {
		t.Fatalf("user ID = %q, want %q", userID, "42")
	}
	if _, err := authenticator.Authenticate(context.Background(), "  ADMIN ", "correct horse battery staple"); err != ErrInvalidCredentials {
		t.Fatalf("case-mismatched username error = %v, want ErrInvalidCredentials", err)
	}
}

func TestAuthenticatorRejectsWrongPassword(t *testing.T) {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("correct password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	authenticator := &Authenticator{store: &fakeUserStore{users: map[string]userRecord{
		"admin": {ID: "42", Username: "admin", PasswordHash: string(passwordHash), IsPlatformAdmin: true},
	}}}

	if _, err := authenticator.Authenticate(context.Background(), "admin", "wrong password"); err != ErrInvalidCredentials {
		t.Fatalf("Authenticate() error = %v, want ErrInvalidCredentials", err)
	}
}

func TestInitialAdminPasswordIsHashedAndBootstrapIsIdempotent(t *testing.T) {
	store := &fakeUserStore{users: make(map[string]userRecord)}
	authenticator := &Authenticator{store: store}
	password := "initial admin password"
	if err := authenticator.EnsureInitialAdmin(context.Background(), "Root", password); err != nil {
		t.Fatal(err)
	}
	if err := authenticator.EnsureInitialAdmin(context.Background(), "Other", "other password"); err != nil {
		t.Fatal(err)
	}

	record := store.users["Root"]
	if record.PasswordHash == password {
		t.Fatal("bootstrap password was stored in plaintext")
	}
	if !record.IsPlatformAdmin {
		t.Fatal("initial user is not a platform admin")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(record.PasswordHash), []byte(password)); err != nil {
		t.Fatalf("stored password hash does not match bootstrap password: %v", err)
	}
	if _, exists := store.users["Other"]; exists {
		t.Fatal("bootstrap replaced or added an account after the first user existed")
	}
}

func TestInitialAdminRequiresCredentialsForEmptyStore(t *testing.T) {
	authenticator := &Authenticator{store: &fakeUserStore{users: make(map[string]userRecord)}}
	if err := authenticator.EnsureInitialAdmin(context.Background(), "", ""); err != ErrBootstrapCredentialsRequired {
		t.Fatalf("EnsureInitialAdmin() error = %v, want ErrBootstrapCredentialsRequired", err)
	}
}
