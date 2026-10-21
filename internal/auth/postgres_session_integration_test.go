package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/contracts"
)

func TestPostgresSessionStoreLifecycle(t *testing.T) {
	dsn, err := testDatabaseURL(filepath.Join("..", "..", ".env"), os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if dsn == "" {
		t.Skip("set PLATFORM_TEST_DATABASE_URL in the process environment or root .env to a dedicated PostgreSQL test database")
	}

	adminDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := adminDB.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})

	randomName := make([]byte, 8)
	if _, err := rand.Read(randomName); err != nil {
		t.Fatal(err)
	}
	schema := "session_test_" + hex.EncodeToString(randomName)
	if _, err := adminDB.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := adminDB.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	ctx := context.Background()
	if _, err := db.Exec(`SET search_path TO ` + schema); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatalf("reapply migrations: %v", err)
	}

	authenticator := NewAuthenticator(db)
	if err := authenticator.EnsureInitialAdmin(ctx, "", ""); err != ErrBootstrapCredentialsRequired {
		t.Fatalf("bootstrap without credentials error = %v, want ErrBootstrapCredentialsRequired", err)
	}
	if err := authenticator.EnsureInitialAdmin(ctx, " Root ", "initial-password-123"); err != nil {
		t.Fatal(err)
	}
	userID, err := authenticator.Authenticate(ctx, "ROOT", "initial-password-123")
	if err != nil || userID == "" {
		t.Fatalf("Authenticate() = (%q, %v), want a user ID", userID, err)
	}
	if _, err := authenticator.Authenticate(ctx, "root", "wrong-password"); err != ErrInvalidCredentials {
		t.Fatalf("wrong password error = %v, want ErrInvalidCredentials", err)
	}
	if err := authenticator.EnsureInitialAdmin(ctx, "another-admin", "another-password"); err != nil {
		t.Fatal(err)
	}
	var userCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&userCount); err != nil {
		t.Fatal(err)
	}
	if userCount != 1 {
		t.Fatalf("user count after repeated bootstrap = %d, want 1", userCount)
	}
	var storedPassword string
	var isAdmin bool
	if err := db.QueryRowContext(ctx, `SELECT password_hash, is_platform_admin FROM users WHERE lower(username) = 'root'`).Scan(&storedPassword, &isAdmin); err != nil {
		t.Fatal(err)
	}
	if storedPassword == "initial-password-123" || !isAdmin {
		t.Fatalf("stored password hash/admin = (%q, %t), want non-plaintext/admin", storedPassword, isAdmin)
	}

	store := &postgresSessionStore{db: db}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	idleTimeout := 30 * time.Minute
	oldHash := sha256.Sum256([]byte("old-token"))
	created, err := store.Create(ctx, contracts.UserID("test-user"), oldHash[:], nil, now, now.Add(12*time.Hour), idleTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || created.UserID != "test-user" {
		t.Fatalf("created session = %+v", created)
	}

	loaded, err := store.LoadAndTouch(ctx, oldHash[:], now.Add(time.Minute), idleTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.LastSeenAt.Equal(now.Add(time.Minute)) || loaded.TokenIsAlias {
		t.Fatalf("loaded session = %+v, want touched current token", loaded)
	}

	newHash := sha256.Sum256([]byte("new-token"))
	rotatedAt := now.Add(2 * time.Minute)
	if _, err := store.Rotate(ctx, created.ID, oldHash[:], newHash[:], rotatedAt, idleTimeout, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	alias, err := store.LoadAndTouch(ctx, oldHash[:], rotatedAt.Add(time.Second), idleTimeout)
	if err != nil || !alias.TokenIsAlias {
		t.Fatalf("old token in grace period = %+v, %v", alias, err)
	}
	if _, err := store.LoadAndTouch(ctx, oldHash[:], rotatedAt.Add(31*time.Second), idleTimeout); err != ErrUnauthenticated {
		t.Fatalf("expired alias error = %v, want ErrUnauthenticated", err)
	}

	if err := store.RevokeToken(ctx, oldHash[:], rotatedAt.Add(32*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadAndTouch(ctx, newHash[:], rotatedAt.Add(33*time.Second), idleTimeout); err != ErrUnauthenticated {
		t.Fatalf("logout by expired alias error = %v, want ErrUnauthenticated for current token", err)
	}

	secondHash := sha256.Sum256([]byte("user-revocation-token"))
	second, err := store.Create(ctx, contracts.UserID("test-user"), secondHash[:], nil, now, now.Add(12*time.Hour), idleTimeout)
	if err != nil {
		t.Fatal(err)
	}
	futureSeenAt := now.Add(5 * time.Minute)
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = $2 WHERE id = $1`, second.ID, futureSeenAt); err != nil {
		t.Fatal(err)
	}
	loadedAfterOlderRequest, err := store.LoadAndTouch(ctx, secondHash[:], now.Add(2*time.Minute), idleTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if !loadedAfterOlderRequest.LastSeenAt.Equal(futureSeenAt) {
		t.Fatalf("older request moved last_seen_at backward: got %v, want %v", loadedAfterOlderRequest.LastSeenAt, futureSeenAt)
	}

	if err := store.RevokeUser(ctx, "test-user", rotatedAt.Add(32*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadAndTouch(ctx, secondHash[:], rotatedAt.Add(33*time.Second), idleTimeout); err != ErrUnauthenticated {
		t.Fatalf("revoked user session error = %v, want ErrUnauthenticated", err)
	}
}
