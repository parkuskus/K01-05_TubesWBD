package definition_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/definition"
	"if3110.local/db-platform/internal/platform"
)

func definitionDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("PLATFORM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set PLATFORM_TEST_DATABASE_URL for definition integration tests")
	}
	u, e := url.Parse(dsn)
	if e != nil || u.Path != "/platform_test" {
		t.Fatal("use dedicated platform_test database")
	}
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil {
		t.Fatal("invalid test database URL")
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	var suffix [8]byte
	if _, e = rand.Read(suffix[:]); e != nil {
		t.Fatal(e)
	}
	schema := "definition_test_" + hex.EncodeToString(suffix[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, e = admin.ExecContext(context.Background(), "CREATE SCHEMA "+quoted); e != nil {
		t.Fatal("cannot create isolated schema")
	}
	t.Cleanup(func() {
		if _, e := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); e != nil {
			t.Error("cannot drop isolated schema")
		}
	})
	copy := cfg.Copy()
	if copy.RuntimeParams == nil {
		copy.RuntimeParams = map[string]string{}
	}
	copy.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*copy)
	t.Cleanup(func() { _ = db.Close() })
	if e = migrations.Apply(context.Background(), db); e != nil {
		t.Fatal(e)
	}
	return db
}

func definitionFixture(t *testing.T) (*sql.DB, *definition.Service, contracts.DatabaseID, contracts.UserID) {
	t.Helper()
	db := definitionDB(t)
	ctx := context.Background()
	var userID, groupID, dbID int64
	if e := db.QueryRowContext(ctx, `INSERT INTO users(username,password_hash) VALUES('editor','hash') RETURNING id`).Scan(&userID); e != nil {
		t.Fatal(e)
	}
	if e := db.QueryRowContext(ctx, `INSERT INTO groups(name) VALUES('editors') RETURNING id`).Scan(&groupID); e != nil {
		t.Fatal(e)
	}
	if _, e := db.ExecContext(ctx, `INSERT INTO group_memberships(group_id,user_id) VALUES($1,$2)`, groupID, userID); e != nil {
		t.Fatal(e)
	}
	if e := db.QueryRowContext(ctx, `INSERT INTO database_connections(name,host,port,database_name,username,ssl_mode,password_ciphertext) VALUES('target','localhost',5432,'target','tester','disable',$1) RETURNING id`, make([]byte, 29)).Scan(&dbID); e != nil {
		t.Fatal(e)
	}
	if _, e := db.ExecContext(ctx, `INSERT INTO group_database_access(group_id,database_id,level) VALUES($1,$2,2)`, groupID, dbID); e != nil {
		t.Fatal(e)
	}
	s, e := definition.NewService(db, platform.NewService(db))
	if e != nil {
		t.Fatal(e)
	}
	return db, s, contracts.DatabaseID(fmtID(dbID)), contracts.UserID(fmtID(userID))
}

func fmtID(n int64) string { return strconv.FormatInt(n, 10) }

func TestPostgresDefinitionRevisionLifecycle(t *testing.T) {
	db, s, id, actor := definitionFixture(t)
	ctx := context.Background()
	in := definition.Input{Title: "Initial", UpSQL: "SELECT 1", DownSQL: "SELECT 2", Mode: "NORMAL"}
	created, err := s.Create(ctx, actor, id, in)
	if err != nil || created.ID == "" || created.Version != 1 || created.Revision != 1 || created.Checksum == "" {
		t.Fatalf("create: %+v %v", created, err)
	}
	if _, err := s.Create(ctx, actor, id, in); err != nil {
		t.Fatal(err)
	}
	items, err := s.List(ctx, actor, id)
	if err != nil || len(items) != 2 || items[1].Version != 2 {
		t.Fatalf("list: %+v %v", items, err)
	}
	if _, err := s.Get(ctx, actor, "99999", created.ID); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("cross-target read: %v", err)
	}
	in.UpSQL = "SELECT 3"
	revised, err := s.Revise(ctx, actor, id, created.ID, 1, in)
	if err != nil || revised.Revision != 2 || revised.Checksum == created.Checksum {
		t.Fatalf("revise: %+v %v", revised, err)
	}
	if _, err := s.Revise(ctx, actor, id, created.ID, 1, in); !errors.Is(err, definition.ErrConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	var oldSQL string
	if err := db.QueryRowContext(ctx, `SELECT up_sql FROM migration_revisions WHERE migration_id=$1 AND revision=1`, created.ID).Scan(&oldSQL); err != nil || oldSQL != "SELECT 1" {
		t.Fatalf("old revision changed: %q %v", oldSQL, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE migration_revisions SET up_sql='ALTER SYSTEM' WHERE migration_id=$1`, created.ID); err == nil {
		t.Fatal("direct revision overwrite accepted")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM migration_revisions WHERE migration_id=$1 AND revision=1`, created.ID); err == nil {
		t.Fatal("direct revision deletion accepted")
	}
	if err := s.MarkAttempted(ctx, created.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAttempted(ctx, created.ID, 2); err != nil {
		t.Fatalf("idempotent attempt: %v", err)
	}
	if _, err := s.Revise(ctx, actor, id, created.ID, 2, in); !errors.Is(err, definition.ErrConflict) {
		t.Fatalf("attempted edit: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE migration_definitions SET attempted_at=NULL WHERE id=$1`, created.ID); err == nil {
		t.Fatal("attempt lock removed directly")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO migration_revisions(migration_id,revision,up_sql,down_sql,irreversible,reason,mode,batch_plan,notes,checksum) VALUES($1,3,'SELECT 3','SELECT 4',false,'','NORMAL','{}','','`+strings.Repeat("0", 64)+`')`, created.ID); err == nil {
		t.Fatal("new revision inserted after attempt")
	}
}

func TestPostgresDefinitionConcurrentVersions(t *testing.T) {
	_, s, id, actor := definitionFixture(t)
	ctx := context.Background()
	in := definition.Input{Title: "Parallel", UpSQL: "SELECT 1", DownSQL: "SELECT 2", Mode: "NORMAL"}
	var wg sync.WaitGroup
	versions := make(chan int64, 10)
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, err := s.Create(ctx, actor, id, in)
			if err != nil {
				errs <- err
				return
			}
			versions <- item.Version
		}()
	}
	wg.Wait()
	close(versions)
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	seen := map[int64]bool{}
	for v := range versions {
		if seen[v] {
			t.Fatalf("duplicate version %d", v)
		}
		seen[v] = true
	}
	if len(seen) != 10 || !seen[1] || !seen[10] {
		t.Fatalf("versions: %v", seen)
	}
}

func TestPostgresDefinitionBatchChecksumUsesStoredPlan(t *testing.T) {
	_, s, id, actor := definitionFixture(t)
	base := definition.Input{Title: "Batch", UpSQL: "UPDATE items SET done=true", DownSQL: "UPDATE items SET done=false", Mode: "BATCH"}
	base.BatchPlan = []byte(`{"batch_size":100,"schema":"public"}`)
	a, err := s.Create(context.Background(), actor, id, base)
	if err != nil {
		t.Fatal(err)
	}
	base.BatchPlan = []byte(`{ "schema": "public", "batch_size": 100 }`)
	b, err := s.Create(context.Background(), actor, id, base)
	if err != nil {
		t.Fatal(err)
	}
	if a.Checksum != b.Checksum {
		t.Fatalf("equivalent persisted plans have different checksums: %s, %s", a.Checksum, b.Checksum)
	}
}
