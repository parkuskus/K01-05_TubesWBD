package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/platform"
)

// This is the I2 handoff check against Aufar's real grant schema and authorizer.
// It deliberately does not create a substitute grant table when 0005 is absent.
func TestPostgresDatabaseWithRealGrants(t *testing.T) {
	db, _, _ := integrationDatabase(t, "PLATFORM_TEST_DATABASE_URL")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	var grantTable sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('group_database_access')::text`).Scan(&grantTable); err != nil {
		t.Fatal(err)
	}
	if !grantTable.Valid {
		t.Skip("I2 pending: platform migration 0005 with real group_database_access is not available")
	}
	target, cfg, schema := integrationDatabase(t, "TARGET_TEST_DATABASE_URL")
	if _, err := target.ExecContext(ctx, `CREATE TABLE sample (id integer PRIMARY KEY, label text); INSERT INTO sample VALUES (1, 'example')`); err != nil {
		t.Fatal(err)
	}
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "admin", "test-admin-password"); err != nil {
		t.Fatal(err)
	}
	admin, err := login.Authenticate(ctx, "admin", "test-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	service := platform.NewService(db)
	member, err := service.CreateUser(ctx, admin, "member", "test-member-password")
	if err != nil {
		t.Fatal(err)
	}
	group, err := service.CreateGroup(ctx, admin, "Readers")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetMembership(ctx, admin, member, group, true); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(db, service, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	in := validInput()
	in.Host, in.Port, in.Database, in.Username, in.Password = cfg.Host, int(cfg.Port), cfg.Database, cfg.User, &cfg.Password
	if cfg.TLSConfig != nil {
		in.SSLMode = "verify-full"
		if cfg.TLSConfig.InsecureSkipVerify {
			in.SSLMode = "require"
		}
	}
	c, err := manager.Create(ctx, admin, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetGrant(ctx, admin, group, c.ID, contracts.AccessViewOnly); err != nil {
		t.Fatal(err)
	}
	viewer, err := NewViewer(service, manager)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.Data(ctx, admin, c.ID, schema, "sample", 1); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatal("admin received implicit target access")
	}
	if len(manager.pools) != 0 {
		t.Fatal("unauthorized request opened a target pool")
	}
	choices, err := manager.Accessible(ctx, member)
	if err != nil || len(choices) != 1 || choices[0].Level != contracts.AccessViewOnly {
		t.Fatal("real grant did not reach database list")
	}
	data, err := viewer.Data(ctx, member, c.ID, schema, "sample", 1)
	if err != nil || len(data.Rows) != 1 || data.Rows[0][1].Text != "example" {
		t.Fatalf("real grant viewer: %v", err)
	}
	if err := service.RequireDatabaseAccess(ctx, member, c.ID, contracts.ActionRunMigration); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatal("View Only can execute migration")
	}
	editors, err := service.CreateGroup(ctx, admin, "Editors")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetMembership(ctx, admin, member, editors, true); err != nil {
		t.Fatal(err)
	}
	if err := service.SetGrant(ctx, admin, editors, c.ID, contracts.AccessViewEdit); err != nil {
		t.Fatal(err)
	}
	choices, err = manager.Accessible(ctx, member)
	if err != nil || len(choices) != 1 || choices[0].Level != contracts.AccessViewEdit {
		t.Fatal("strongest real group grant missing")
	}
	if err := service.SetGrant(ctx, admin, editors, c.ID, contracts.AccessNone); err != nil {
		t.Fatal(err)
	}
	choices, err = manager.Accessible(ctx, member)
	if err != nil || len(choices) != 1 || choices[0].Level != contracts.AccessViewOnly {
		t.Fatal("grant downgrade not reflected")
	}
	if err := service.SetMembership(ctx, admin, member, group, false); err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.Data(ctx, member, c.ID, schema, "sample", 1); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatal("cached pool bypassed revoked membership")
	}
	if _, err := manager.Create(ctx, member, in); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatal("non-admin managed connections")
	}
	if err := manager.Delete(ctx, admin, c.ID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM group_database_access WHERE database_id=$1`, c.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("real grant cascade left orphan rows")
	}
	var rows int
	if err := target.QueryRowContext(ctx, `SELECT count(*) FROM sample`).Scan(&rows); err != nil || rows != 1 {
		t.Fatal("connection deletion changed target data")
	}
}
