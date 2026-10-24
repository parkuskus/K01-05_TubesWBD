package migration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/internal/contracts"
)

var errMigrationTestSchemaExists = errors.New("migration history schema already exists in the selected test database")

func requireUnusedMigrationSchema(ctx context.Context, db *sql.DB) error {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname='if3110_platform')`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errMigrationTestSchemaExists
	}
	return nil
}

func TestPostgresMigrationServiceCommitAndRollback(t *testing.T) {
	// pakai variabel TARGET_TEST_DATABASE_URL khusus database target uji
	dsn := os.Getenv("TARGET_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TARGET_TEST_DATABASE_URL in the process environment to a disposable PostgreSQL target database without if3110_platform")
	}

	adminDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := adminDB.Close(); err != nil {
			t.Errorf("close admin test database: %v", err)
		}
	})
	ctx := context.Background()
	if err := requireUnusedMigrationSchema(ctx, adminDB); err != nil {
		if errors.Is(err, errMigrationTestSchemaExists) {
			t.Skip("history schema already exists; refusing to modify it")
		}
		t.Fatal(err)
	}

	randomBytes := make([]byte, 8)
	if _, err := rand.Read(randomBytes); err != nil {
		t.Fatal(err)
	}
	appSchema := "migration_test_" + hex.EncodeToString(randomBytes)
	if _, err := adminDB.Exec(`CREATE SCHEMA ` + appSchema); err != nil {
		t.Fatal(err)
	}
	historyCreated := false
	t.Cleanup(func() {
		if _, err := adminDB.Exec(`DROP SCHEMA IF EXISTS ` + appSchema + ` CASCADE`); err != nil {
			t.Errorf("drop test app schema: %v", err)
		}
		if historyCreated {
			if _, err := adminDB.Exec(`DROP SCHEMA IF EXISTS if3110_platform CASCADE`); err != nil {
				t.Errorf("drop test-owned migration history schema: %v", err)
			}
		}
	})

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.ExecContext(ctx, `SET search_path TO `+appSchema); err != nil {
		t.Fatal(err)
	}

	authz := newFakeAuthorizer()
	editorUser := contracts.UserID("user-editor")
	viewerUser := contracts.UserID("user-viewer")
	targetDB := contracts.DatabaseID("db-target-1")

	authz.setGrant(editorUser, targetDB, contracts.AccessViewEdit)
	authz.setGrant(viewerUser, targetDB, contracts.AccessViewOnly)

	provider := &fakeDatabaseProvider{db: db}
	svc, err := NewService(authz, provider)
	if err != nil {
		t.Fatal(err)
	}

	fixedNow := time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixedNow }

	// 1. buat draft migration versi 1, ubah sql-nya, lalu jalankan (skenario sukses commit)
	historyCreated = true
	draft1, err := svc.CreateDraft(ctx, editorUser, targetDB, "buat tabel buku", `CREATE TABLE books (id INT PRIMARY KEY, title TEXT NOT NULL);`)
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if draft1.Version != 1 || draft1.Status != StatusDraft || draft1.CreatedBy != editorUser {
		t.Fatalf("draft1 = %+v, want version 1 DRAFT by %s", draft1, editorUser)
	}

	fixedNow = fixedNow.Add(time.Minute)
	updated1, err := svc.UpdateDraft(
		ctx,
		editorUser,
		targetDB,
		1,
		"buat tabel buku dan isi data awal",
		`CREATE TABLE books (id INT PRIMARY KEY, title TEXT NOT NULL); INSERT INTO books (id, title) VALUES (1, 'Clean Code');`,
	)
	if err != nil {
		t.Fatalf("UpdateDraft() error = %v", err)
	}
	if updated1.Title != "buat tabel buku dan isi data awal" || !strings.Contains(updated1.SQLText, "Clean Code") {
		t.Fatalf("updated1 = %+v, want updated title and SQL", updated1)
	}
	if _, err := svc.ExecuteMigration(ctx, editorUser, targetDB, 1, reviewedDraft(draft1)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale draft executed: %v", err)
	}
	var prematureTable sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('books')::text`).Scan(&prematureTable); err != nil || prematureTable.Valid {
		t.Fatalf("stale draft changed target: table=%v, err=%v", prematureTable, err)
	}

	fixedNow = fixedNow.Add(time.Minute)
	applied1, err := svc.ExecuteMigration(ctx, editorUser, targetDB, 1, reviewedDraft(updated1))
	if err != nil {
		t.Fatalf("ExecuteMigration(1) error = %v", err)
	}
	if applied1.Status != StatusApplied || applied1.ExecutedBy != editorUser || applied1.ExecutedAt == nil || applied1.ErrorMessage != "" {
		t.Fatalf("applied1 = %+v, want APPLIED status with executed metadata", applied1)
	}

	// pastikan migration yang sudah dieksekusi tidak bisa diubah atau dijalankan ulang
	if _, err := svc.UpdateDraft(ctx, editorUser, targetDB, 1, "ubah lagi", "SELECT 1;"); !errors.Is(err, ErrMigrationAlreadyApplied) {
		t.Fatalf("UpdateDraft() after execution error = %v, want ErrMigrationAlreadyApplied", err)
	}
	if _, err := svc.ExecuteMigration(ctx, editorUser, targetDB, 1, reviewedDraft(updated1)); !errors.Is(err, ErrMigrationAlreadyApplied) {
		t.Fatalf("ExecuteMigration() second time error = %v, want ErrMigrationAlreadyApplied", err)
	}

	// 2. jalankan migration versi 2 secara langsung lewat RunMigration (skenario sukses commit)
	fixedNow = fixedNow.Add(time.Minute)
	applied2, err := svc.RunMigration(
		ctx,
		editorUser,
		targetDB,
		"tambah buku kedua",
		`INSERT INTO books (id, title) VALUES (2, 'Designing Data-Intensive Applications');`,
	)
	if err != nil {
		t.Fatalf("RunMigration(2) error = %v", err)
	}
	if applied2.Version != 2 || applied2.Status != StatusApplied {
		t.Fatalf("applied2 = %+v, want version 2 APPLIED", applied2)
	}

	// 3. jalankan migration versi 3 yang gagal di tengah jalan (skenario gagal wajib rollback)
	fixedNow = fixedNow.Add(time.Minute)
	failed3, execErr := svc.RunMigration(
		ctx,
		editorUser,
		targetDB,
		"migrasi rusak harus rollback",
		`INSERT INTO books (id, title) VALUES (3, 'Buku Harus Batal'); SELECT * FROM tabel_tidak_ada_sama_sekali;`,
	)
	if execErr == nil {
		t.Fatal("RunMigration(3) expected SQL execution error, got nil")
	}
	if failed3.Version != 3 || failed3.Status != StatusFailed || failed3.ExecutedBy != editorUser || failed3.ErrorMessage == "" || failed3.ExecutedAt == nil {
		t.Fatalf("failed3 = %+v, want version 3 FAILED with error message", failed3)
	}

	// pastikan baris id = 3 benar-benar ter-rollback dan baris 1 & 2 tetap ada
	var bookCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM books`).Scan(&bookCount); err != nil {
		t.Fatalf("count books error = %v", err)
	}
	if bookCount != 2 {
		t.Fatalf("books row count after rollback = %d, want 2", bookCount)
	}

	// 4. buat draft versi 4 yang sql-nya gagal saat ExecuteMigration (uji rollback dari draft)
	fixedNow = fixedNow.Add(time.Minute)
	draft4, err := svc.CreateDraft(
		ctx,
		editorUser,
		targetDB,
		"draft gagal saat dieksekusi",
		`INSERT INTO books (id, title) VALUES (4, 'Draft Batal'); INSERT INTO books (id, title) VALUES (1, 'Duplikat PK');`,
	)
	if err != nil {
		t.Fatalf("CreateDraft(4) error = %v", err)
	}
	if draft4.Version != 4 || draft4.Status != StatusDraft {
		t.Fatalf("draft4 = %+v, want version 4 DRAFT", draft4)
	}

	fixedNow = fixedNow.Add(time.Minute)
	failed4, execErr4 := svc.ExecuteMigration(ctx, editorUser, targetDB, 4, reviewedDraft(draft4))
	if execErr4 == nil {
		t.Fatal("ExecuteMigration(4) expected duplicate key error, got nil")
	}
	if failed4.Version != 4 || failed4.Status != StatusFailed || failed4.ErrorMessage == "" {
		t.Fatalf("failed4 = %+v, want version 4 FAILED", failed4)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM books`).Scan(&bookCount); err != nil {
		t.Fatalf("count books after draft rollback error = %v", err)
	}
	if bookCount != 2 {
		t.Fatalf("books row count after draft rollback = %d, want 2", bookCount)
	}

	// 5. pastikan user view-only bisa melihat daftar dan detail riwayat secara linear
	history, err := svc.ListHistory(ctx, viewerUser, targetDB)
	if err != nil {
		t.Fatalf("ListHistory() by viewer error = %v", err)
	}
	if len(history) != 4 {
		t.Fatalf("len(history) = %d, want 4", len(history))
	}
	if history[0].Version != 1 || history[0].Status != StatusApplied ||
		history[1].Version != 2 || history[1].Status != StatusApplied ||
		history[2].Version != 3 || history[2].Status != StatusFailed ||
		history[3].Version != 4 || history[3].Status != StatusFailed {
		t.Fatalf("unexpected linear history = %+v", history)
	}

	detail3, err := svc.GetMigration(ctx, viewerUser, targetDB, 3)
	if err != nil {
		t.Fatalf("GetMigration(3) by viewer error = %v", err)
	}
	if detail3.Status != StatusFailed || detail3.ErrorMessage == "" {
		t.Fatalf("detail3 = %+v, want FAILED with error message", detail3)
	}

	if _, err := svc.GetMigration(ctx, viewerUser, targetDB, 999); !errors.Is(err, ErrMigrationNotFound) {
		t.Fatalf("GetMigration(999) error = %v, want ErrMigrationNotFound", err)
	}
}
