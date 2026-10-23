package platform_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
)

func platformTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("PLATFORM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set PLATFORM_TEST_DATABASE_URL in the process to a dedicated platform_test database")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" || parsed.Path != "/platform_test" {
		t.Fatal("PLATFORM_TEST_DATABASE_URL must point to the dedicated platform_test database")
	}

	adminDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("could not open test database")
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal("could not generate test schema name")
	}
	schema := "platform_test_" + hex.EncodeToString(suffix[:])
	if _, err := adminDB.ExecContext(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatal("could not create isolated test schema")
	}
	t.Cleanup(func() {
		if _, err := adminDB.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Error("could not remove isolated test schema")
		}
	})

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("could not open test connection")
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), `SET search_path TO `+schema); err != nil {
		t.Fatal("could not set test schema")
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatal("could not apply platform migrations")
	}
	return db
}

func TestGroupNameRejectsBlankAndCaseInsensitiveDuplicate(t *testing.T) {
	db := platformTestDB(t)
	if _, err := db.Exec(`INSERT INTO groups (name) VALUES ('Editors')`); err != nil {
		t.Fatal("could not insert group")
	}
	for _, name := range []string{"editors", "  "} {
		if _, err := db.Exec(`INSERT INTO groups (name) VALUES ($1)`, name); err == nil {
			t.Errorf("invalid group name was accepted: %q", name)
		}
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatal("reapplying platform migrations failed")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM groups WHERE name = 'Editors'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reapplying migrations changed group data: count=%d, error=%v", count, err)
	}
}

func TestGroupMembershipRejectsMissingUserOrGroupAndDuplicatePair(t *testing.T) {
	db := platformTestDB(t)
	var userID, groupID int64
	if err := db.QueryRow(`INSERT INTO users (username, password_hash) VALUES ('member', 'test-hash') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal("could not insert test user")
	}
	if err := db.QueryRow(`INSERT INTO groups (name) VALUES ('Members') RETURNING id`).Scan(&groupID); err != nil {
		t.Fatal("could not insert test group")
	}
	if _, err := db.Exec(`INSERT INTO group_memberships (group_id, user_id) VALUES ($1, $2)`, groupID, userID); err != nil {
		t.Fatal("valid membership was rejected")
	}
	for _, pair := range [][2]int64{{groupID, userID}, {groupID + 1000000, userID}, {groupID, userID + 1000000}} {
		if _, err := db.Exec(`INSERT INTO group_memberships (group_id, user_id) VALUES ($1, $2)`, pair[0], pair[1]); err == nil {
			t.Errorf("duplicate or invalid membership was accepted")
		}
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatal("could not delete test user")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM group_memberships`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("user deletion left memberships: count=%d, error=%v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM groups WHERE id = $1`, groupID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("user deletion removed group: count=%d, error=%v", count, err)
	}
	if err := db.QueryRow(`INSERT INTO users (username, password_hash) VALUES ('another-member', 'test-hash') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal("could not insert second test user")
	}
	if _, err := db.Exec(`INSERT INTO group_memberships (group_id, user_id) VALUES ($1, $2)`, groupID, userID); err != nil {
		t.Fatal("could not insert second membership")
	}
	if _, err := db.Exec(`DELETE FROM groups WHERE id = $1`, groupID); err != nil {
		t.Fatal("could not delete test group")
	}
	if err := db.QueryRow(`SELECT count(*) FROM group_memberships`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("group deletion left memberships: count=%d, error=%v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("group deletion removed user: count=%d, error=%v", count, err)
	}
}
