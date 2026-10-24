package migration

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

type readonlyConnector struct{ conn *readonlyConn }

func (c readonlyConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (readonlyConnector) Driver() driver.Driver                          { return readonlyDriver{} }

type readonlyDriver struct{}

func (readonlyDriver) Open(string) (driver.Conn, error) { return nil, errors.New("unused") }

type readonlyConn struct {
	missing   bool
	queryCode string
	queries   int
	updates   int
}

func (*readonlyConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (*readonlyConn) Close() error                        { return nil }
func (*readonlyConn) Begin() (driver.Tx, error)           { return readonlyTx{}, nil }

type readonlyTx struct{}

func (readonlyTx) Commit() error   { return nil }
func (readonlyTx) Rollback() error { return nil }
func (c *readonlyConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.updates++
	return nil, errors.New("read-only connection: DDL forbidden")
}
func (c *readonlyConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.queries++
	if !strings.Contains(query, "FROM if3110_platform.migration_history") {
		return nil, errors.New("unexpected query")
	}
	if c.queryCode != "" {
		return nil, &pgconn.PgError{Code: c.queryCode}
	}
	if c.missing {
		return nil, &pgconn.PgError{Code: "42P01"}
	}
	return &emptyHistoryRows{}, nil
}

func TestViewOnlyHistoryDoesNotHidePermissionErrors(t *testing.T) {
	conn := &readonlyConn{queryCode: "42501"}
	db := sql.OpenDB(readonlyConnector{conn: conn})
	t.Cleanup(func() { _ = db.Close() })
	authz := newFakeAuthorizer()
	authz.setGrant("viewer", "db-1", contracts.AccessViewOnly)
	svc, err := NewService(authz, &fakeDatabaseProvider{db: db})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListHistory(context.Background(), "viewer", "db-1"); err == nil {
		t.Fatal("ListHistory hid a permission error")
	}
	if _, err := svc.GetMigration(context.Background(), "viewer", "db-1", 1); err == nil || errors.Is(err, ErrMigrationNotFound) {
		t.Fatalf("GetMigration treated permission error as missing history: %v", err)
	}
	if conn.updates != 0 {
		t.Fatalf("read performed %d writes", conn.updates)
	}
}

type emptyHistoryRows struct{}

func (*emptyHistoryRows) Columns() []string {
	return []string{"id", "version", "title", "sql_text", "status", "created_by", "executed_by", "error_message", "created_at", "updated_at", "executed_at"}
}
func (*emptyHistoryRows) Close() error              { return nil }
func (*emptyHistoryRows) Next([]driver.Value) error { return io.EOF }

func TestViewOnlyHistoryDoesNotExecuteDDL(t *testing.T) {
	for _, missing := range []bool{true, false} {
		t.Run(map[bool]string{true: "uninitialized", false: "existing"}[missing], func(t *testing.T) {
			conn := &readonlyConn{missing: missing}
			db := sql.OpenDB(readonlyConnector{conn: conn})
			t.Cleanup(func() { _ = db.Close() })
			authz := newFakeAuthorizer()
			authz.setGrant("viewer", "db-1", contracts.AccessViewOnly)
			svc, err := NewService(authz, &fakeDatabaseProvider{db: db})
			if err != nil {
				t.Fatal(err)
			}
			history, err := svc.ListHistory(context.Background(), "viewer", "db-1")
			if err != nil || len(history) != 0 {
				t.Fatalf("ListHistory = %v, %v; want empty list", history, err)
			}
			_, err = svc.GetMigration(context.Background(), "viewer", "db-1", 1)
			if !errors.Is(err, ErrMigrationNotFound) {
				t.Fatalf("GetMigration = %v; want ErrMigrationNotFound", err)
			}
			if conn.updates != 0 || conn.queries != 2 {
				t.Fatalf("read performed %d writes and %d queries", conn.updates, conn.queries)
			}
		})
	}
}

func TestMissingHistoryWritesDoNotInitializeExceptCreation(t *testing.T) {
	conn := &readonlyConn{missing: true}
	db := sql.OpenDB(readonlyConnector{conn: conn})
	t.Cleanup(func() { _ = db.Close() })
	authz := newFakeAuthorizer()
	authz.setGrant("editor", "db-1", contracts.AccessViewEdit)
	svc, err := NewService(authz, &fakeDatabaseProvider{db: db})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.UpdateDraft(ctx, "editor", "db-1", 1, "title", "SELECT 1"); !errors.Is(err, ErrMigrationNotFound) {
		t.Fatalf("UpdateDraft = %v; want ErrMigrationNotFound", err)
	}
	if _, err := svc.ExecuteMigration(ctx, "editor", "db-1", 1, "reviewed"); !errors.Is(err, ErrMigrationNotFound) {
		t.Fatalf("ExecuteMigration = %v; want ErrMigrationNotFound", err)
	}
	if conn.updates != 0 {
		t.Fatalf("existing-draft operations performed %d DDL writes", conn.updates)
	}
	if _, err := svc.CreateDraft(ctx, "editor", "db-1", "title", "SELECT 1"); err == nil {
		t.Fatal("CreateDraft should fail on a read-only connection")
	}
	if conn.updates != 1 {
		t.Fatalf("CreateDraft performed %d DDL writes; want 1", conn.updates)
	}
}

// fake authorizer buat ngetes izin akses per user dan database
type fakeAuthorizer struct {
	grants map[contracts.UserID]map[contracts.DatabaseID]contracts.AccessLevel
}

func newFakeAuthorizer() *fakeAuthorizer {
	return &fakeAuthorizer{
		grants: make(map[contracts.UserID]map[contracts.DatabaseID]contracts.AccessLevel),
	}
}

func (f *fakeAuthorizer) setGrant(userID contracts.UserID, dbID contracts.DatabaseID, level contracts.AccessLevel) {
	if f.grants[userID] == nil {
		f.grants[userID] = make(map[contracts.DatabaseID]contracts.AccessLevel)
	}
	f.grants[userID][dbID] = level
}

func (f *fakeAuthorizer) RequirePlatformAdmin(ctx context.Context, userID contracts.UserID) error {
	return contracts.ErrForbidden
}

func (f *fakeAuthorizer) RequireDatabaseAccess(ctx context.Context, userID contracts.UserID, databaseID contracts.DatabaseID, action contracts.DatabaseAction) error {
	userGrants, ok := f.grants[userID]
	if !ok {
		return contracts.ErrForbidden
	}
	level, ok := userGrants[databaseID]
	if !ok || !level.Allows(action) {
		return contracts.ErrForbidden
	}
	return nil
}

// fake provider koneksi database buat unit test
type fakeDatabaseProvider struct {
	db        *sql.DB
	err       error
	poolCalls int
}

func (f *fakeDatabaseProvider) Pool(ctx context.Context, databaseID contracts.DatabaseID) (*sql.DB, error) {
	f.poolCalls++
	if f.err != nil {
		return nil, f.err
	}
	return f.db, nil
}

func TestNewServiceValidation(t *testing.T) {
	authz := newFakeAuthorizer()
	provider := &fakeDatabaseProvider{}

	if _, err := NewService(nil, provider); !errors.Is(err, ErrInvalidServiceConfig) {
		t.Fatalf("NewService(nil, provider) error = %v, want ErrInvalidServiceConfig", err)
	}
	if _, err := NewService(authz, nil); !errors.Is(err, ErrInvalidServiceConfig) {
		t.Fatalf("NewService(authz, nil) error = %v, want ErrInvalidServiceConfig", err)
	}
	svc, err := NewService(authz, provider)
	if err != nil || svc == nil {
		t.Fatalf("NewService(authz, provider) = (%v, %v), want non-nil service", svc, err)
	}
}

func TestServiceRejectsViewOnlyUserOnWriteAndExecute(t *testing.T) {
	authz := newFakeAuthorizer()
	userID := contracts.UserID("user-view-only")
	dbID := contracts.DatabaseID("db-1")
	authz.setGrant(userID, dbID, contracts.AccessViewOnly)

	provider := &fakeDatabaseProvider{}
	svc, err := NewService(authz, provider)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	if _, err := svc.CreateDraft(ctx, userID, dbID, "buat tabel", "CREATE TABLE t (id INT);"); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("CreateDraft() by view-only user error = %v, want ErrForbidden", err)
	}
	if _, err := svc.UpdateDraft(ctx, userID, dbID, 1, "ubah tabel", "CREATE TABLE t2 (id INT);"); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("UpdateDraft() by view-only user error = %v, want ErrForbidden", err)
	}
	if _, err := svc.ExecuteMigration(ctx, userID, dbID, 1, "reviewed"); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("ExecuteMigration() by view-only user error = %v, want ErrForbidden", err)
	}
	if _, err := svc.RunMigration(ctx, userID, dbID, "langsung jalan", "CREATE TABLE t3 (id INT);"); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("RunMigration() by view-only user error = %v, want ErrForbidden", err)
	}

	// pastikan koneksi ke db target gak pernah disentuh kalau otorisasi ditolak
	if provider.poolCalls != 0 {
		t.Fatalf("provider.Pool called %d times on forbidden actions, want 0", provider.poolCalls)
	}
}

func TestServiceRejectsUserWithoutDatabaseAccess(t *testing.T) {
	authz := newFakeAuthorizer()
	userID := contracts.UserID("user-tanpa-akses")
	dbID := contracts.DatabaseID("db-rahasia")

	provider := &fakeDatabaseProvider{}
	svc, err := NewService(authz, provider)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	if _, err := svc.ListHistory(ctx, userID, dbID); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("ListHistory() without access error = %v, want ErrForbidden", err)
	}
	if _, err := svc.GetMigration(ctx, userID, dbID, 1); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("GetMigration() without access error = %v, want ErrForbidden", err)
	}
	if provider.poolCalls != 0 {
		t.Fatalf("provider.Pool called %d times without access, want 0", provider.poolCalls)
	}
}

func TestServiceInputValidation(t *testing.T) {
	authz := newFakeAuthorizer()
	userID := contracts.UserID("editor-1")
	dbID := contracts.DatabaseID("db-1")
	authz.setGrant(userID, dbID, contracts.AccessViewEdit)

	provider := &fakeDatabaseProvider{}
	svc, err := NewService(authz, provider)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	if _, err := svc.ListHistory(ctx, "", dbID); !errors.Is(err, ErrInvalidUser) {
		t.Fatalf("ListHistory() empty user error = %v, want ErrInvalidUser", err)
	}
	if _, err := svc.ListHistory(ctx, userID, "   "); !errors.Is(err, ErrInvalidDatabaseID) {
		t.Fatalf("ListHistory() blank dbID error = %v, want ErrInvalidDatabaseID", err)
	}
	if _, err := svc.GetMigration(ctx, userID, dbID, 0); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("GetMigration() version 0 error = %v, want ErrInvalidVersion", err)
	}
	if _, err := svc.CreateDraft(ctx, userID, dbID, "judul", "   \n\t  "); !errors.Is(err, ErrInvalidSQL) {
		t.Fatalf("CreateDraft() empty SQL error = %v, want ErrInvalidSQL", err)
	}
	if _, err := svc.UpdateDraft(ctx, userID, dbID, -1, "judul", "SELECT 1;"); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("UpdateDraft() negative version error = %v, want ErrInvalidVersion", err)
	}
	if _, err := svc.UpdateDraft(ctx, userID, dbID, 1, "judul", ""); !errors.Is(err, ErrInvalidSQL) {
		t.Fatalf("UpdateDraft() empty SQL error = %v, want ErrInvalidSQL", err)
	}
	if _, err := svc.ExecuteMigration(ctx, userID, dbID, 0, "reviewed"); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("ExecuteMigration() version 0 error = %v, want ErrInvalidVersion", err)
	}
	if _, err := svc.RunMigration(ctx, userID, dbID, "judul", ""); !errors.Is(err, ErrInvalidSQL) {
		t.Fatalf("RunMigration() empty SQL error = %v, want ErrInvalidSQL", err)
	}

	if provider.poolCalls != 0 {
		t.Fatalf("provider.Pool called %d times on invalid input, want 0", provider.poolCalls)
	}
}

func TestServicePropagatesDatabaseProviderError(t *testing.T) {
	authz := newFakeAuthorizer()
	userID := contracts.UserID("editor-1")
	dbID := contracts.DatabaseID("db-mati")
	authz.setGrant(userID, dbID, contracts.AccessViewEdit)

	connErr := errors.New("target database connection refused")
	provider := &fakeDatabaseProvider{err: connErr}
	svc, err := NewService(authz, provider)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	if _, err := svc.ListHistory(ctx, userID, dbID); !errors.Is(err, connErr) {
		t.Fatalf("ListHistory() error = %v, want %v", err, connErr)
	}
	if _, err := svc.CreateDraft(ctx, userID, dbID, "draft", "CREATE TABLE x (id INT);"); !errors.Is(err, connErr) {
		t.Fatalf("CreateDraft() error = %v, want %v", err, connErr)
	}
	if _, err := svc.RunMigration(ctx, userID, dbID, "run", "CREATE TABLE x (id INT);"); !errors.Is(err, connErr) {
		t.Fatalf("RunMigration() error = %v, want %v", err, connErr)
	}
}

func TestStatusIsValid(t *testing.T) {
	if !StatusDraft.IsValid() || !StatusApplied.IsValid() || !StatusFailed.IsValid() {
		t.Fatal("expected DRAFT, APPLIED, and FAILED to be valid statuses")
	}
	if Status("UNKNOWN").IsValid() {
		t.Fatal("expected UNKNOWN status to be invalid")
	}
}
