package migration

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

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
