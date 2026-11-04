package migrations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
)

type fakeMigrationState struct {
	tableExists      bool
	existingObjects  map[string]bool
	applied          []appliedMigration
	executedSQL      []string
	lockAcquired     int
	lockReleased     int
	failUnlock       bool
	rawBadConnCalled int
}

type fakeConnector struct {
	state *fakeMigrationState
}

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{state: c.state}, nil
}

func (fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("unused") }

type fakeConn struct {
	state *fakeMigrationState
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return fakeTx{}, nil }

func (c *fakeConn) ResetSession(context.Context) error {
	return nil
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	trimmed := strings.TrimSpace(query)
	switch {
	case strings.HasPrefix(trimmed, "SELECT pg_advisory_lock"):
		c.state.lockAcquired++
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(trimmed, "CREATE TABLE IF NOT EXISTS schema_migrations"):
		c.state.tableExists = true
		return driver.RowsAffected(0), nil
	case strings.HasPrefix(trimmed, "INSERT INTO schema_migrations"):
		if len(args) != 3 {
			return nil, errors.New("expected 3 args for schema_migrations insert")
		}
		c.state.applied = append(c.state.applied, appliedMigration{
			Version:  args[0].Value.(string),
			Filename: args[1].Value.(string),
			Checksum: args[2].Value.(string),
		})
		return driver.RowsAffected(1), nil
	default:
		c.state.executedSQL = append(c.state.executedSQL, trimmed)
		return driver.RowsAffected(1), nil
	}
}

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	trimmed := strings.TrimSpace(query)
	switch {
	case strings.HasPrefix(trimmed, "SELECT pg_advisory_unlock"):
		if c.state.failUnlock {
			return &singleValueRows{col: "pg_advisory_unlock", val: false}, nil
		}
		c.state.lockReleased++
		return &singleValueRows{col: "pg_advisory_unlock", val: true}, nil
	case strings.HasPrefix(trimmed, "SELECT version, filename, checksum FROM schema_migrations"):
		if !c.state.tableExists {
			return nil, &pgconn.PgError{Code: "42P01"}
		}
		rows := make([]appliedMigration, len(c.state.applied))
		copy(rows, c.state.applied)
		return &appliedRows{rows: rows}, nil
	case strings.HasPrefix(trimmed, "SELECT to_regclass"):
		obj := args[0].Value.(string)
		if c.state.existingObjects[obj] {
			return &singleValueRows{col: "to_regclass", val: obj}, nil
		}
		return &singleValueRows{col: "to_regclass", val: nil}, nil
	default:
		return nil, errors.New("unexpected query: " + trimmed)
	}
}

type singleValueRows struct {
	col  string
	val  any
	done bool
}

func (r *singleValueRows) Columns() []string { return []string{r.col} }
func (r *singleValueRows) Close() error      { return nil }
func (r *singleValueRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.val
	return nil
}

type appliedRows struct {
	rows []appliedMigration
	idx  int
}

func (*appliedRows) Columns() []string { return []string{"version", "filename", "checksum"} }
func (*appliedRows) Close() error      { return nil }
func (r *appliedRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	rec := r.rows[r.idx]
	r.idx++
	dest[0] = rec.Version
	dest[1] = rec.Filename
	dest[2] = rec.Checksum
	return nil
}

func testMigrationFS() fstest.MapFS {
	return fstest.MapFS{
		"0001_create_sessions.sql":              {Data: []byte("CREATE TABLE sessions (id BIGINT);")},
		"0002_create_users.sql":                 {Data: []byte("CREATE TABLE users (id BIGINT);")},
		"0003_create_groups.sql":                {Data: []byte("CREATE TABLE groups (id BIGINT);")},
		"0004_create_database_connections.sql":  {Data: []byte("CREATE TABLE database_connections (id BIGINT);")},
		"0005_create_group_database_access.sql": {Data: []byte("CREATE TABLE group_database_access (group_id BIGINT);")},
		"0006_case_sensitive_usernames.sql":     {Data: []byte("CREATE UNIQUE INDEX users_username_idx ON users (username);")},
		"0007_case_insensitive_usernames.sql":   {Data: []byte("CREATE UNIQUE INDEX users_username_lower_idx ON users (lower(username));")},
		"0008_add_group_leader.sql":             {Data: []byte("ALTER TABLE groups ADD COLUMN leader_user_id BIGINT;")},
	}
}

func TestLoadEmbeddedChecksumNormalized(t *testing.T) {
	embedded, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded() error = %v", err)
	}
	if len(embedded) < 7 {
		t.Fatalf("len(LoadEmbedded()) = %d, want at least 7", len(embedded))
	}
	for i, m := range embedded {
		if m.Version == "" || m.Filename == "" || len(m.Checksum) != 64 {
			t.Fatalf("invalid embedded migration[%d] = %+v", i, m)
		}
		if i > 0 && embedded[i-1].Version >= m.Version {
			t.Fatalf("migrations not sorted: %s before %s", embedded[i-1].Version, m.Version)
		}
	}

	lfFS := fstest.MapFS{"0001_test.sql": {Data: []byte("SELECT 1;\nSELECT 2;\n")}}
	crlfFS := fstest.MapFS{"0001_test.sql": {Data: []byte("SELECT 1;\r\nSELECT 2;\r\n")}}
	lfList, err := loadFromFS(lfFS)
	if err != nil {
		t.Fatal(err)
	}
	crlfList, err := loadFromFS(crlfFS)
	if err != nil {
		t.Fatal(err)
	}
	if lfList[0].Checksum != crlfList[0].Checksum {
		t.Fatalf("CRLF vs LF checksum mismatch: %s != %s", lfList[0].Checksum, crlfList[0].Checksum)
	}
}

func TestApplyFreshInstallAndIdempotentRerun(t *testing.T) {
	state := &fakeMigrationState{existingObjects: make(map[string]bool)}
	db := sql.OpenDB(fakeConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })

	fsys := testMigrationFS()
	ctx := context.Background()

	if err := verifyUpToDateWithFS(ctx, db, fsys); !errors.Is(err, ErrPendingMigrations) {
		t.Fatalf("VerifyUpToDate before Apply = %v, want ErrPendingMigrations", err)
	}

	if err := applyWithFS(ctx, db, fsys); err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
	if len(state.executedSQL) != 8 || len(state.applied) != 8 {
		t.Fatalf("fresh install executed=%d applied=%d, want 8 and 8", len(state.executedSQL), len(state.applied))
	}
	if state.lockAcquired != 1 || state.lockReleased != 1 {
		t.Fatalf("lock acquired=%d released=%d, want 1 and 1", state.lockAcquired, state.lockReleased)
	}

	if err := verifyUpToDateWithFS(ctx, db, fsys); err != nil {
		t.Fatalf("VerifyUpToDate after Apply = %v, want nil", err)
	}

	// Running Apply a second time must not re-execute any migration SQL.
	if err := applyWithFS(ctx, db, fsys); err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}
	if len(state.executedSQL) != 8 {
		t.Fatalf("second Apply() re-executed SQL: count=%d, want 8", len(state.executedSQL))
	}
}

func TestApplyDetectsCompleteM1BaselineAndAppliesOnlyM2(t *testing.T) {
	existing := make(map[string]bool, len(m1BaselineObjects))
	for _, obj := range m1BaselineObjects {
		existing[obj] = true
	}
	state := &fakeMigrationState{existingObjects: existing}
	db := sql.OpenDB(fakeConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })

	fsys := testMigrationFS()
	ctx := context.Background()

	if err := applyWithFS(ctx, db, fsys); err != nil {
		t.Fatalf("Apply() on existing M1 database error = %v", err)
	}

	// Only 0008 should have been executed; 0001-0007 are baselined without execution.
	if len(state.executedSQL) != 1 || !strings.Contains(state.executedSQL[0], "leader_user_id") {
		t.Fatalf("executedSQL on M1 baseline = %v, want only 0008", state.executedSQL)
	}
	if len(state.applied) != 8 {
		t.Fatalf("recorded migrations = %d, want 8 (7 baselined + 1 applied)", len(state.applied))
	}
	if err := verifyUpToDateWithFS(ctx, db, fsys); err != nil {
		t.Fatalf("VerifyUpToDate after M1 baseline upgrade = %v, want nil", err)
	}
}

func TestApplyRejectsIncompleteM1Baseline(t *testing.T) {
	state := &fakeMigrationState{
		existingObjects: map[string]bool{
			"sessions": true,
			"users":    true,
			// groups and other M1 objects missing
		},
	}
	db := sql.OpenDB(fakeConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })

	err := applyWithFS(context.Background(), db, testMigrationFS())
	if !errors.Is(err, ErrIncompleteBaseline) {
		t.Fatalf("Apply() with partial M1 schema = %v, want ErrIncompleteBaseline", err)
	}
	if len(state.executedSQL) != 0 || len(state.applied) != 0 {
		t.Fatalf("partial baseline modified state: executed=%d applied=%d", len(state.executedSQL), len(state.applied))
	}
}

func TestApplyAndVerifyRejectChecksumMismatch(t *testing.T) {
	state := &fakeMigrationState{existingObjects: make(map[string]bool)}
	db := sql.OpenDB(fakeConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })

	fsys := testMigrationFS()
	ctx := context.Background()
	if err := applyWithFS(ctx, db, fsys); err != nil {
		t.Fatal(err)
	}

	// Tamper with the checksum of 0002 in the ledger.
	state.applied[1].Checksum = strings.Repeat("0", 64)

	if err := applyWithFS(ctx, db, fsys); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Apply() with tampered checksum = %v, want ErrChecksumMismatch", err)
	}
	if err := verifyUpToDateWithFS(ctx, db, fsys); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("VerifyUpToDate() with tampered checksum = %v, want ErrChecksumMismatch", err)
	}
}

