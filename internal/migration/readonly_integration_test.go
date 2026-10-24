package migration

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/internal/contracts"
)

// This test never falls back to .env and will not touch an existing history schema.
func TestPostgresViewOnlyHistoryIsReadOnly(t *testing.T) {
	dsn := os.Getenv("MIGRATION_READONLY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MIGRATION_READONLY_TEST_DATABASE_URL to a disposable PostgreSQL database without if3110_platform")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	var schema sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regnamespace('if3110_platform')::text`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if schema.Valid {
		t.Skip("history schema already exists; refusing to modify it")
	}
	created := false
	t.Cleanup(func() {
		if created {
			if _, err := db.ExecContext(context.Background(), `DROP SCHEMA if3110_platform CASCADE`); err != nil {
				t.Errorf("drop test-owned history schema: %v", err)
			}
		}
	})

	authz := newFakeAuthorizer()
	authz.setGrant("viewer", "db-1", contracts.AccessViewOnly)
	authz.setGrant("editor", "db-1", contracts.AccessViewEdit)
	readConfig := config.Copy()
	if readConfig.RuntimeParams == nil {
		readConfig.RuntimeParams = make(map[string]string)
	}
	readConfig.RuntimeParams["default_transaction_read_only"] = "on"
	readDB := stdlib.OpenDB(*readConfig)
	t.Cleanup(func() { _ = readDB.Close() })
	var readOnly string
	if err := readDB.QueryRowContext(ctx, `SHOW default_transaction_read_only`).Scan(&readOnly); err != nil || readOnly != "on" {
		t.Fatalf("read-only test connection is not read-only: %v", err)
	}
	reader, err := NewService(authz, &fakeDatabaseProvider{db: readDB})
	if err != nil {
		t.Fatal(err)
	}
	items, err := reader.ListHistory(ctx, "viewer", "db-1")
	if err != nil || len(items) != 0 {
		t.Fatalf("empty history: %d records, %v", len(items), err)
	}
	if _, err := reader.GetMigration(ctx, "viewer", "db-1", 1); !errors.Is(err, ErrMigrationNotFound) {
		t.Fatalf("missing migration: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT to_regnamespace('if3110_platform')::text`).Scan(&schema); err != nil || schema.Valid {
		t.Fatalf("read created history schema: %v", err)
	}

	writer, err := NewService(authz, &fakeDatabaseProvider{db: db})
	if err != nil {
		t.Fatal(err)
	}
	// Mark cleanup before the first write so partial schema creation is also cleaned up.
	created = true
	if _, err := writer.CreateDraft(ctx, "editor", "db-1", "first", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	items, err = reader.ListHistory(ctx, "viewer", "db-1")
	if err != nil || len(items) != 1 || items[0].Version != 1 {
		t.Fatalf("read existing history: %d records, %v", len(items), err)
	}
	if record, err := reader.GetMigration(ctx, "viewer", "db-1", 1); err != nil || record.SQLText != "SELECT 1" {
		t.Fatalf("read existing detail: %+v, %v", record, err)
	}
}

func TestPostgresStaleDraftFailureCannotOverwriteApplied(t *testing.T) {
	dsn := os.Getenv("MIGRATION_READONLY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MIGRATION_READONLY_TEST_DATABASE_URL to a disposable PostgreSQL database without if3110_platform")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	var schema sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regnamespace('if3110_platform')::text`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if schema.Valid {
		t.Skip("history schema already exists; refusing to modify it")
	}
	created := false
	t.Cleanup(func() {
		if created {
			if _, err := db.ExecContext(context.Background(), `DROP SCHEMA if3110_platform CASCADE`); err != nil {
				t.Errorf("drop test-owned history schema: %v", err)
			}
		}
	})
	created = true
	store, err := NewPostgresStore(db, "db-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	draft, err := store.CreateDraft(ctx, "safe", "SELECT 1", "editor", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecuteDraft(ctx, draft.Version, "editor", time.Now(), reviewedDraft(draft)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.markDraftFailed(ctx, draft.Version, "late-failure", "stale SQL error", time.Now()); !errors.Is(err, ErrMigrationAlreadyApplied) {
		t.Fatalf("stale failure returned %v, want conflict", err)
	}
	record, err := store.GetByVersion(ctx, draft.Version)
	if err != nil || record.Status != StatusApplied || record.ExecutedBy != "editor" || record.ErrorMessage != "" {
		t.Fatalf("stale failure overwrote applied migration: %+v, %v", record, err)
	}
	start := make(chan struct{})
	type outcome struct {
		version int64
		err     error
	}
	results := make(chan outcome, 2)
	var group sync.WaitGroup
	for _, run := range []bool{false, true} {
		group.Add(1)
		go func(run bool) {
			defer group.Done()
			<-start
			if run {
				record, err := store.RunNew(ctx, "direct", "SELECT 1", "editor", time.Now())
				results <- outcome{record.Version, err}
				return
			}
			record, err := store.CreateDraft(ctx, "draft", "SELECT 1", "editor", time.Now())
			results <- outcome{record.Version, err}
		}(run)
	}
	close(start)
	group.Wait()
	close(results)
	versions := make(map[int64]bool)
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		versions[result.version] = true
	}
	if len(versions) != 2 || !versions[2] || !versions[3] {
		t.Fatalf("concurrent creation used versions %v, want 2 and 3", versions)
	}
}

func TestMigrationFixtureRefusesExistingHistory(t *testing.T) {
	dsn := os.Getenv("MIGRATION_READONLY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MIGRATION_READONLY_TEST_DATABASE_URL to a disposable PostgreSQL database without if3110_platform")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname='if3110_platform')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Skip("history schema already exists; refusing to modify it")
	}
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA if3110_platform; CREATE TABLE if3110_platform.keep_me (id integer)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), `DROP SCHEMA if3110_platform CASCADE`); err != nil {
			t.Errorf("drop test-owned sentinel schema: %v", err)
		}
	})
	if err := requireUnusedMigrationSchema(ctx, db); !errors.Is(err, errMigrationTestSchemaExists) {
		t.Fatalf("fixture check returned %v, want refusal", err)
	}
	var table sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('if3110_platform.keep_me')::text`).Scan(&table); err != nil || !table.Valid {
		t.Fatalf("fixture check removed an existing table: %v", err)
	}
}
