package migrations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"net/url"
	"os"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresUpgradeRetainsExistingPlatformData(t *testing.T) {
	dsn := os.Getenv("PLATFORM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set PLATFORM_TEST_DATABASE_URL for dedicated PostgreSQL upgrade test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Path != "/platform_test" {
		t.Fatal("PLATFORM_TEST_DATABASE_URL must point to dedicated platform_test")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	adminDB := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = adminDB.Close() })
	for _, previous := range []string{"0007", "0012"} {
		t.Run(previous, func(t *testing.T) {
			var suffix [8]byte
			if _, err := rand.Read(suffix[:]); err != nil {
				t.Fatal(err)
			}
			schema := "upgrade_" + hex.EncodeToString(suffix[:])
			if _, err := adminDB.ExecContext(context.Background(), `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
				t.Fatal("cannot create upgrade test schema")
			}
			t.Cleanup(func() {
				if _, err := adminDB.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
					t.Error("cannot clean upgrade test schema")
				}
			})
			cfg := config.Copy()
			if cfg.RuntimeParams == nil {
				cfg.RuntimeParams = make(map[string]string)
			}
			cfg.RuntimeParams["search_path"] = schema
			db := stdlib.OpenDB(*cfg)
			t.Cleanup(func() { _ = db.Close() })
			prior := fstest.MapFS{}
			filesList, err := fs.Glob(files, "*.sql")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range filesList {
				if name[:4] > previous {
					continue
				}
				raw, err := fs.ReadFile(files, name)
				if err != nil {
					t.Fatal(err)
				}
				prior[name] = &fstest.MapFile{Data: raw}
			}
			ctx := context.Background()
			if err := applyWithFS(ctx, db, prior); err != nil {
				t.Fatalf("old schema setup failed: %v", err)
			}
			var userID, groupID, databaseID int64
			if err := db.QueryRowContext(ctx, `INSERT INTO users (username,password_hash) VALUES ('old-user','test-hash') RETURNING id`).Scan(&userID); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `INSERT INTO groups (name) VALUES ('old-group') RETURNING id`).Scan(&groupID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO group_memberships(group_id,user_id) VALUES ($1,$2)`, groupID, userID); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `INSERT INTO database_connections(name,host,port,database_name,username,ssl_mode,password_ciphertext) VALUES ('old-target','localhost',5432,'target','tester','disable',$1) RETURNING id`, make([]byte, 29)).Scan(&databaseID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO group_database_access(group_id,database_id,level) VALUES ($1,$2,2)`, groupID, databaseID); err != nil {
				t.Fatal(err)
			}
			if err := Apply(ctx, db); err != nil {
				t.Fatalf("upgrade failed: %v", err)
			}
			if err := VerifyUpToDate(ctx, db); err != nil {
				t.Fatalf("upgraded schema not current: %v", err)
			}
			if err := Apply(ctx, db); err != nil {
				t.Fatalf("repeat migration failed: %v", err)
			}
			var leader sql.NullInt64
			var username, groupName string
			var level, version int64
			if err := db.QueryRowContext(ctx, `SELECT username FROM users WHERE id=$1`, userID).Scan(&username); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `SELECT name, leader_user_id FROM groups WHERE id=$1`, groupID).Scan(&groupName, &leader); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `SELECT level FROM group_database_access WHERE group_id=$1 AND database_id=$2`, groupID, databaseID).Scan(&level); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `SELECT version FROM platform_policy_version WHERE id=true`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if username != "old-user" || groupName != "old-group" || leader.Valid || level != 2 || version != 1 {
				t.Fatalf("upgrade changed old data: user=%q group=%q leader=%v level=%d policy=%d", username, groupName, leader.Valid, level, version)
			}
		})
	}
}
