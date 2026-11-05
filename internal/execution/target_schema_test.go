package execution

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

type fakeTargetSchemaBackend struct {
	initialized  bool
	ddlCalls     []string
	head         contracts.ActiveHeadState
	commitProofs map[string]ExecutionCommitProof
}

type fakeTargetSchemaConnector struct {
	backend *fakeTargetSchemaBackend
}

func (c fakeTargetSchemaConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeTargetSchemaConn{backend: c.backend}, nil
}

func (fakeTargetSchemaConnector) Driver() driver.Driver { return fakeTargetSchemaDriver{} }

type fakeTargetSchemaDriver struct{}

func (fakeTargetSchemaDriver) Open(string) (driver.Conn, error) { return nil, errors.New("unused") }

type fakeTargetSchemaConn struct {
	backend *fakeTargetSchemaBackend
}

func (*fakeTargetSchemaConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (*fakeTargetSchemaConn) Close() error                        { return nil }
func (*fakeTargetSchemaConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }

func (c *fakeTargetSchemaConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	trimmed := strings.TrimSpace(query)
	c.backend.ddlCalls = append(c.backend.ddlCalls, trimmed)
	if strings.Contains(trimmed, "CREATE TABLE IF NOT EXISTS if3110_platform.migration_history_v2") &&
		strings.Contains(trimmed, "CREATE TABLE IF NOT EXISTS if3110_platform.execution_commits") &&
		strings.Contains(trimmed, "CREATE TABLE IF NOT EXISTS if3110_platform.database_migration_state") {
		c.backend.initialized = true
		return driver.RowsAffected(1), nil
	}
	return nil, errors.New("unexpected ExecContext query: " + trimmed)
}

func (c *fakeTargetSchemaConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	trimmed := strings.TrimSpace(query)
	if !c.backend.initialized {
		return nil, &pgconn.PgError{Code: "42P01", Message: "relation does not exist"}
	}

	switch {
	case strings.Contains(trimmed, "FROM if3110_platform.database_migration_state"):
		return &fakeTargetStateRows{head: c.backend.head}, nil

	case strings.Contains(trimmed, "FROM if3110_platform.execution_commits"):
		execID := args[0].Value.(string)
		phase := args[1].Value.(string)
		batchNo := int(args[2].Value.(int64))
		key := execID + "|" + phase + "|" + string(rune(batchNo))
		if proof, ok := c.backend.commitProofs[key]; ok {
			return &fakeCommitProofRows{proofs: []ExecutionCommitProof{proof}}, nil
		}
		return &fakeCommitProofRows{}, nil

	default:
		return nil, errors.New("unexpected QueryContext query: " + trimmed)
	}
}

type fakeTargetStateRows struct {
	head contracts.ActiveHeadState
	done bool
}

func (*fakeTargetStateRows) Columns() []string {
	return []string{
		"active_head_migration_id",
		"active_head_execution_id",
		"active_head_version",
		"unresolved_execution_id",
		"unresolved_reason",
	}
}

func (*fakeTargetStateRows) Close() error { return nil }

func (r *fakeTargetStateRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = string(r.head.MigrationID)
	dest[1] = string(r.head.ExecutionID)
	dest[2] = r.head.Version
	dest[3] = string(r.head.UnresolvedExecutionID)
	dest[4] = string(r.head.UnresolvedReason)
	return nil
}

type fakeCommitProofRows struct {
	proofs []ExecutionCommitProof
	idx    int
}

func (*fakeCommitProofRows) Columns() []string {
	return []string{
		"execution_id",
		"phase",
		"batch_no",
		"attempt_id",
		"executed_sql",
		"affected_rows",
		"cursor_after",
		"actor_id",
		"committed_at",
	}
}

func (*fakeCommitProofRows) Close() error { return nil }

func (r *fakeCommitProofRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.proofs) {
		return io.EOF
	}
	p := r.proofs[r.idx]
	r.idx++
	dest[0] = string(p.ExecutionID)
	dest[1] = string(p.Phase)
	dest[2] = int64(p.BatchNo)
	dest[3] = string(p.AttemptID)
	dest[4] = p.ExecutedSQL
	dest[5] = p.AffectedRows
	dest[6] = p.CursorAfter
	dest[7] = string(p.ActorID)
	dest[8] = p.CommittedAt
	return nil
}

func TestEnsureTargetSchemaV2CreatesLedgerTablesAndPreservesM1History(t *testing.T) {
	lowerDDL := strings.ToLower(ensureTargetSchemaV2SQL)
	if strings.Contains(lowerDDL, "drop table") || strings.Contains(lowerDDL, "alter table if3110_platform.migration_history") {
		t.Fatal("ensureTargetSchemaV2SQL must never drop or alter M1 if3110_platform.migration_history")
	}

	backend := &fakeTargetSchemaBackend{}
	db := sql.OpenDB(fakeTargetSchemaConnector{backend: backend})
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	if err := EnsureTargetSchemaV2(ctx, conn); err != nil {
		t.Fatalf("EnsureTargetSchemaV2() error = %v", err)
	}
	if !backend.initialized || len(backend.ddlCalls) != 1 {
		t.Fatalf("expected 1 DDL execution and initialized=true, got %d and %v", len(backend.ddlCalls), backend.initialized)
	}

	// Second call is idempotent.
	if err := EnsureTargetSchemaV2(ctx, conn); err != nil {
		t.Fatalf("second EnsureTargetSchemaV2() error = %v", err)
	}
}

func TestReadActiveHeadAndLookupCommitProofAreStrictlyReadOnlyOnMissingSchema(t *testing.T) {
	committedAt := time.Date(2026, 11, 5, 15, 0, 0, 0, time.UTC)
	backend := &fakeTargetSchemaBackend{
		initialized: false,
		head: contracts.ActiveHeadState{
			MigrationID:           "mig-10",
			ExecutionID:           "exec-10",
			Version:               10,
			UnresolvedExecutionID: "",
			UnresolvedReason:      "",
		},
		commitProofs: map[string]ExecutionCommitProof{
			"exec-10|NORMAL|" + string(rune(0)): {
				ExecutionID:  "exec-10",
				Phase:        CommitPhaseNormal,
				BatchNo:      0,
				AttemptID:    "att-10",
				ExecutedSQL:  "ALTER TABLE users ADD COLUMN phone TEXT;",
				AffectedRows: 0,
				ActorID:      "user-1",
				CommittedAt:  committedAt,
			},
		},
	}
	db := sql.OpenDB(fakeTargetSchemaConnector{backend: backend})
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	reader := TargetHeadReader{}

	// Before schema initialization (42P01), ReadActiveHeadOnConn and LookupCommitProof
	// must return zero values without executing any DDL.
	head, err := reader.ReadActiveHeadOnConn(ctx, conn)
	if err != nil {
		t.Fatalf("ReadActiveHeadOnConn on uninitialized target error = %v", err)
	}
	if head != (contracts.ActiveHeadState{}) {
		t.Fatalf("expected empty ActiveHeadState on uninitialized target, got %+v", head)
	}

	_, found, err := LookupCommitProof(ctx, conn, "exec-10", CommitPhaseNormal, 0)
	if err != nil || found {
		t.Fatalf("LookupCommitProof on uninitialized target: found=%v err=%v", found, err)
	}
	if len(backend.ddlCalls) != 0 {
		t.Fatalf("read-only methods executed DDL: %v", backend.ddlCalls)
	}

	// Initialize schema on writable path, then verify ReadActiveHeadOnConn and LookupCommitProof return committed state.
	if err := EnsureTargetSchemaV2(ctx, conn); err != nil {
		t.Fatal(err)
	}

	head, err = reader.ReadActiveHeadOnConn(ctx, conn)
	if err != nil {
		t.Fatalf("ReadActiveHeadOnConn after init error = %v", err)
	}
	if head.ExecutionID != "exec-10" || head.Version != 10 {
		t.Fatalf("unexpected head after init: %+v", head)
	}

	proof, found, err := LookupCommitProof(ctx, conn, "exec-10", CommitPhaseNormal, 0)
	if err != nil || !found {
		t.Fatalf("LookupCommitProof after init: found=%v err=%v", found, err)
	}
	if proof.AttemptID != "att-10" || proof.ExecutedSQL != "ALTER TABLE users ADD COLUMN phone TEXT;" {
		t.Fatalf("unexpected proof: %+v", proof)
	}
}

