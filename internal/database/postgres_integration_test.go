package database

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
)

// Each test uses a random schema and search_path set for EVERY pool connection.
// These URLs must point to disposable test databases. No .env is read here.
func integrationDatabase(t *testing.T, variable string) (*sql.DB, *pgx.ConnConfig, string) {
	t.Helper()
	dsn := os.Getenv(variable)
	if dsn == "" {
		t.Skip("set " + variable + " to a dedicated PostgreSQL test database")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	admin := stdlib.OpenDB(*cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "connection_test_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		_ = admin.Close()
		t.Fatal("could not create isolated test schema; check database connectivity and CREATE privilege")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("could not clean up test schema")
		}
		_ = admin.Close()
	})
	isolated := cfg.Copy()
	isolated.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*isolated)
	t.Cleanup(func() { _ = db.Close() })
	return db, cfg, schema
}

func integrationManager(t *testing.T, db *sql.DB) *Manager {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if err := migrations.Apply(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	m, err := NewManager(db, fakeAuthorizer{}, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestPostgresConnectionCRUD(t *testing.T) {
	db, _, _ := integrationDatabase(t, "PLATFORM_TEST_DATABASE_URL")
	m := integrationManager(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	in := validInput()
	c, err := m.Create(ctx, "admin", in)
	if err != nil {
		t.Fatal(err)
	}
	if !validID(c.ID) || c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
		t.Fatal("missing generated ID/timestamps")
	}
	var ciphertext []byte
	if err := db.QueryRowContext(ctx, `SELECT password_ciphertext FROM database_connections WHERE id=$1`, c.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte(*in.Password)) {
		t.Fatal("database stored plaintext credentials")
	}
	password, err := m.credentials.open(ciphertext)
	if err != nil || password != *in.Password {
		t.Fatal("stored credentials cannot be recovered")
	}
	in.Name = "target"
	if _, err := m.Create(ctx, "admin", in); err != ErrConflict {
		t.Fatalf("duplicate name error = %v", err)
	}
	items, err := m.List(ctx, "admin")
	if err != nil || len(items) != 1 || items[0].ID != c.ID {
		t.Fatal("list mismatch")
	}
	if got, err := m.Get(ctx, "admin", c.ID); err != nil || got.Name != c.Name {
		t.Fatal("get mismatch")
	}
	for _, op := range adminOperations(m, "non-admin") {
		if err := op(); err == nil {
			t.Fatal("non-admin operation succeeded")
		}
	}
	// A UI metadata-only edit keeps the password unchanged.
	in.Password = nil
	in.Name = "Updated"
	if _, err := m.Update(ctx, "admin", c.ID, in); err != nil {
		t.Fatal(err)
	}
	r, err := m.store.get(ctx, c.ID)
	if err != nil || r.revision != 2 || !bytes.Equal(r.ciphertext, ciphertext) {
		t.Fatal("metadata-only edit changed credentials or missed revision")
	}
	in.Password = stringPointer("replacement'@#$%:/\\")
	if _, err := m.Update(ctx, "admin", c.ID, in); err != nil {
		t.Fatal(err)
	}
	r, err = m.store.get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	password, err = m.credentials.open(r.ciphertext)
	if err != nil || password != *in.Password {
		t.Fatal("replacement password not persisted")
	}
	wrongKey, err := NewManager(db, fakeAuthorizer{}, bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer wrongKey.Close()
	if _, err := wrongKey.Pool(ctx, c.ID); err != ErrCredentials {
		t.Fatalf("wrong key error = %v", err)
	}
	if _, err := m.Update(ctx, "admin", "9223372036854775807", in); err != ErrNotFound {
		t.Fatal(err)
	}
	// Aufar owns the real grant table. This fixture verifies the proposed FK shape.
	if _, err := db.ExecContext(ctx, `CREATE TABLE grant_fixture (
		database_id BIGINT NOT NULL REFERENCES database_connections(id) ON DELETE CASCADE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO grant_fixture VALUES ($1)`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "admin", c.ID); err != nil {
		t.Fatal(err)
	}
	var grants int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM grant_fixture`).Scan(&grants); err != nil || grants != 0 {
		t.Fatal("orphan grants remain")
	}
	if _, err := m.Get(ctx, "admin", c.ID); err != ErrNotFound {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "admin", c.ID); err != ErrNotFound {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(ctx); err != nil {
		t.Fatal("manager closed caller-owned platform database")
	}
}

func TestPostgresTargetPool(t *testing.T) {
	// Target URL must use password authentication and a non-superuser owner.
	// Keeping it separate also tests the platform/target database boundary.
	target, cfg, schema := integrationDatabase(t, "TARGET_TEST_DATABASE_URL")
	db, _, _ := integrationDatabase(t, "PLATFORM_TEST_DATABASE_URL")
	m := integrationManager(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if cfg.Password == "" {
		t.Fatal("target test URL must include a password")
	}
	var superuser bool
	if err := target.QueryRowContext(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&superuser); err != nil {
		t.Fatal("could not check target test role")
	}
	if superuser {
		t.Fatal("target test role must not be a superuser")
	}
	if _, err := target.ExecContext(ctx, `CREATE TABLE sample (id integer PRIMARY KEY, label text); INSERT INTO sample VALUES (1, 'example')`); err != nil {
		t.Fatal(err)
	}
	in := validInput()
	in.Host, in.Port, in.Database, in.Username = cfg.Host, int(cfg.Port), cfg.Database, cfg.User
	in.Password = &cfg.Password
	if cfg.TLSConfig != nil {
		in.SSLMode = "verify-full"
		if cfg.TLSConfig.InsecureSkipVerify {
			in.SSLMode = "require"
		}
	}
	c, err := m.Create(ctx, "admin", in)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := m.Pool(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Stats().MaxOpenConnections != 5 {
		t.Fatal("missing pool limit")
	}
	if again, err := m.Pool(ctx, c.ID); err != nil || again != pool {
		t.Fatal("pool not reused")
	}
	var label, statementTimeout string
	table := pgx.Identifier{schema, "sample"}.Sanitize()
	if err := pool.QueryRowContext(ctx, "SELECT label FROM "+table+" WHERE id=$1", 1).Scan(&label); err != nil || label != "example" {
		t.Fatal("target data not readable")
	}
	if err := pool.QueryRowContext(ctx, "SHOW statement_timeout").Scan(&statementTimeout); err != nil || statementTimeout != "30s" {
		t.Fatal("target query timeout missing")
	}
	queryCtx, queryCancel := context.WithTimeout(ctx, 30*time.Millisecond)
	_, err = pool.ExecContext(queryCtx, "SELECT pg_sleep(2)")
	queryCancel()
	if err == nil {
		t.Fatal("query ignored context deadline")
	}
	// PostgreSQL permission denial remains effective even with a usable pool.
	if _, err := target.ExecContext(ctx, "REVOKE SELECT ON sample FROM "+pgx.Identifier{cfg.User}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRowContext(ctx, "SELECT label FROM "+table).Scan(&label)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatal("target permission denial not enforced")
	}
	if _, err := target.ExecContext(ctx, "GRANT SELECT ON sample TO "+pgx.Identifier{cfg.User}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	// A different manager observes updated revisions and drops its cached pool.
	other, err := NewManager(db, fakeAuthorizer{}, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherPool, err := other.Pool(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	in.Password = stringPointer("incorrect-password-for-test")
	if _, err := m.Update(ctx, "admin", c.ID, in); err != nil {
		t.Fatal(err)
	}
	if err := pool.PingContext(ctx); err == nil {
		t.Fatal("edit left previous pool open")
	}
	if _, err := m.Pool(ctx, c.ID); err != ErrConnection {
		t.Fatalf("bad target password error = %v; target must require password auth", err)
	}
	if _, err := other.Pool(ctx, c.ID); err != ErrConnection {
		t.Fatal("other manager used stale credentials")
	}
	if err := otherPool.PingContext(ctx); err == nil {
		t.Fatal("other manager retained stale pool")
	}
	in.Password = &cfg.Password
	if _, err := m.Update(ctx, "admin", c.ID, in); err != nil {
		t.Fatal(err)
	}
	pool, err = m.Pool(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "admin", c.ID); err != nil {
		t.Fatal(err)
	}
	if err := pool.PingContext(ctx); err == nil {
		t.Fatal("delete left pool open")
	}
	if _, err := m.Pool(ctx, c.ID); err != ErrNotFound {
		t.Fatal(err)
	}
}
