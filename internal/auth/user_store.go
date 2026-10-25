package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"if3110.local/db-platform/internal/contracts"
)

var (
	ErrInvalidCredentials           = errors.New("invalid username or password")
	ErrBootstrapCredentialsRequired = errors.New("initial admin credentials are required for an empty user database")
)

type userRecord struct {
	ID              contracts.UserID
	Username        string
	PasswordHash    string
	IsPlatformAdmin bool
}

// LoginProof is valid only while the account's credentials and admin status remain unchanged.
// Its credential snapshot stays inside the auth package.
type LoginProof struct {
	UserID          contracts.UserID
	passwordHash    string
	isPlatformAdmin bool
}

type userStore interface {
	FindByUsername(ctx context.Context, username string) (userRecord, error)
	EnsureInitialAdmin(ctx context.Context, username, passwordHash string) error
}

type Authenticator struct {
	store userStore
}

type postgresUserStore struct {
	db *sql.DB
}

func NewAuthenticator(db *sql.DB) *Authenticator {
	return &Authenticator{store: &postgresUserStore{db: db}}
}

func (a *Authenticator) Authenticate(ctx context.Context, username, password string) (contracts.UserID, error) {
	proof, err := a.AuthenticateForSession(ctx, username, password)
	return proof.UserID, err
}

func (a *Authenticator) AuthenticateForSession(ctx context.Context, username, password string) (LoginProof, error) {
	normalized, err := normalizeUsername(username)
	if err != nil || password == "" || len(password) > 72 {
		return LoginProof{}, ErrInvalidCredentials
	}
	user, err := a.store.FindByUsername(ctx, normalized)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			return LoginProof{}, ErrInvalidCredentials
		}
		return LoginProof{}, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return LoginProof{}, ErrInvalidCredentials
	}
	return LoginProof{UserID: user.ID, passwordHash: user.PasswordHash, isPlatformAdmin: user.IsPlatformAdmin}, nil
}

func (a *Authenticator) EnsureInitialAdmin(ctx context.Context, username, password string) error {
	if username == "" && password == "" {
		return a.store.EnsureInitialAdmin(ctx, "", "")
	}
	normalized, err := normalizeUsername(username)
	if err != nil || password == "" || len(password) > 72 {
		return ErrBootstrapCredentialsRequired
	}
	passwordHash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash initial admin password: %w", err)
	}
	return a.store.EnsureInitialAdmin(ctx, normalized, passwordHash)
}

func normalizeUsername(username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 64 {
		return "", ErrInvalidCredentials
	}
	return username, nil
}

// NormalizeUsername applies the same rules for login, bootstrap, and administration.
func NormalizeUsername(username string) (string, error) { return normalizeUsername(username) }

// HashPassword uses the same bcrypt cost and byte limit as bootstrap.
func HashPassword(password string) (string, error) {
	if password == "" || len(password) > 72 {
		return "", ErrInvalidCredentials
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func (s *postgresUserStore) FindByUsername(ctx context.Context, username string) (userRecord, error) {
	var record userRecord
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, is_platform_admin
		FROM users
		WHERE username = $1`, username).Scan(
		&id, &record.Username, &record.PasswordHash, &record.IsPlatformAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return userRecord{}, ErrInvalidCredentials
	}
	if err != nil {
		return userRecord{}, err
	}
	record.ID = contracts.UserID(strconv.FormatInt(id, 10))
	return record, nil
}

func (s *postgresUserStore) EnsureInitialAdmin(ctx context.Context, username, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `LOCK TABLE users IN EXCLUSIVE MODE`); err != nil {
		return err
	}
	var hasUsers bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&hasUsers); err != nil {
		return err
	}
	if hasUsers {
		return tx.Commit()
	}
	if username == "" || passwordHash == "" {
		return ErrBootstrapCredentialsRequired
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (username, password_hash, is_platform_admin)
		VALUES ($1, $2, TRUE)`, username, passwordHash); err != nil {
		return err
	}
	return tx.Commit()
}
