package execution

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

type memoryGateStore struct {
	mu    sync.Mutex
	gates map[contracts.DatabaseID]GateRecord
}

func newMemoryGateStore() *memoryGateStore {
	return &memoryGateStore{
		gates: make(map[contracts.DatabaseID]GateRecord),
	}
}

func (m *memoryGateStore) SetGate(
	_ context.Context,
	databaseID contracts.DatabaseID,
	executionID contracts.ExecutionID,
	jobID contracts.JobID,
	reason contracts.GateReason,
	details string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	m.gates[databaseID] = GateRecord{
		DatabaseID:  databaseID,
		ExecutionID: executionID,
		JobID:       jobID,
		Reason:      reason,
		Details:     details,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return nil
}

func (m *memoryGateStore) ClearGate(_ context.Context, databaseID contracts.DatabaseID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.gates, databaseID)
	return nil
}

func (m *memoryGateStore) GetGate(_ context.Context, databaseID contracts.DatabaseID) (GateRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.gates[databaseID]
	return rec, ok, nil
}

type fakeRecoveryTargetDB struct {
	*fakeOrdinaryTargetDB
	lockServer             *fakeLockServer
	dbName                 string
	commitErr              error
	commitActuallySucceeds bool
}

func newFakeRecoveryTargetDB(dbName string) *fakeRecoveryTargetDB {
	return &fakeRecoveryTargetDB{
		fakeOrdinaryTargetDB: newFakeOrdinaryTargetDB(),
		lockServer:           &fakeLockServer{},
		dbName:               dbName,
	}
}

type fakeRecoveryConnector struct {
	target *fakeRecoveryTargetDB
}

func (c fakeRecoveryConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeRecoveryConn{
		fakeOrdinaryConn: &fakeOrdinaryConn{db: c.target.fakeOrdinaryTargetDB},
		target:           c.target,
		lockSession: &fakeLockSession{
			connector: &fakeLockConnector{
				server: c.target.lockServer,
				dbName: c.target.dbName,
			},
			held: make(map[[2]int32]bool),
		},
	}, nil
}

func (fakeRecoveryConnector) Driver() driver.Driver { return fakeOrdinaryDriver{} }

type fakeRecoveryConn struct {
	*fakeOrdinaryConn
	target      *fakeRecoveryTargetDB
	lockSession *fakeLockSession
}

func (c *fakeRecoveryConn) Close() error {
	return c.lockSession.Close()
}

func (c *fakeRecoveryConn) Begin() (driver.Tx, error) {
	_, _ = c.fakeOrdinaryConn.Begin()
	return &fakeRecoveryTx{target: c.target}, nil
}

type fakeRecoveryTx struct {
	target *fakeRecoveryTargetDB
}

func (tx *fakeRecoveryTx) Commit() error {
	db := tx.target.fakeOrdinaryTargetDB
	if tx.target.commitErr != nil {
		err := tx.target.commitErr
		tx.target.commitErr = nil
		if tx.target.commitActuallySucceeds {
			// Server committed the transaction before the network dropped!
			db.head = db.stagedHead
			db.history = db.stagedHistory
			db.proofs = db.stagedProofs
			db.executedUserSQL = db.stagedUserSQL
		} else {
			// Server rolled back the transaction when the connection died before commit!
			db.stagedHistory = nil
			db.stagedProofs = nil
			db.stagedUserSQL = nil
		}
		db.inTx = false
		return err
	}
	if db.inTx {
		db.head = db.stagedHead
		db.history = db.stagedHistory
		db.proofs = db.stagedProofs
		db.executedUserSQL = db.stagedUserSQL
		db.inTx = false
	}
	return nil
}

func (tx *fakeRecoveryTx) Rollback() error {
	db := tx.target.fakeOrdinaryTargetDB
	if db.inTx {
		db.stagedHistory = nil
		db.stagedProofs = nil
		db.stagedUserSQL = nil
		db.inTx = false
	}
	return nil
}

func (c *fakeRecoveryConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	trimmed := strings.TrimSpace(query)
	switch {
	case strings.HasPrefix(trimmed, "UPDATE if3110_platform.database_migration_state") &&
		strings.Contains(trimmed, "unresolved_execution_id = $1"):
		c.target.head.UnresolvedExecutionID = contracts.ExecutionID(args[0].Value.(string))
		c.target.head.UnresolvedReason = contracts.GateReason(args[1].Value.(string))
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "UPDATE if3110_platform.database_migration_state") &&
		strings.Contains(trimmed, "unresolved_execution_id = ''"):
		c.target.head.UnresolvedExecutionID = ""
		c.target.head.UnresolvedReason = ""
		return driver.RowsAffected(1), nil

	default:
		return c.fakeOrdinaryConn.ExecContext(ctx, query, args)
	}
}

func (c *fakeRecoveryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	trimmed := strings.TrimSpace(query)
	if strings.HasPrefix(trimmed, "SELECT current_database()") ||
		strings.HasPrefix(trimmed, "SELECT pg_try_advisory_lock") ||
		strings.HasPrefix(trimmed, "SELECT pg_advisory_unlock") {
		return c.lockSession.QueryContext(ctx, query, args)
	}
	return c.fakeOrdinaryConn.QueryContext(ctx, query, args)
}

// TestRecoveryReconcilesUnknownCommitWhenProofExistsWithoutReexecutingSQL verifies UNKNOWN-01:
// When tx.Commit() fails due to a dropped connection after PostgreSQL actually committed
// the transaction on the server:
// 1. Attempt 1 returns UNKNOWN and activates the UNKNOWN gate in platform & target.
// 2. Recovery Attempt 2 (after acquiring the session advisory lock on a healthy connection)
//    finds the commit proof in execution_commits, clears the UNKNOWN gate, and reconciles
//    the job to SUCCESS without re-executing user SQL.
func TestRecoveryReconcilesUnknownCommitWhenProofExistsWithoutReexecutingSQL(t *testing.T) {
	target := newFakeRecoveryTargetDB("target_unknown_01")
	target.commitErr = io.ErrUnexpectedEOF
	target.commitActuallySucceeds = true

	sqlDB := sql.OpenDB(fakeRecoveryConnector{target: target})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn1, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lock1, err := AcquireTargetSessionLock(ctx, conn1)
	if err != nil {
		t.Fatal(err)
	}

	gates := newMemoryGateStore()
	recoveryExec := NewRecoveryExecutor(NewOrdinaryExecutor(), gates)

	plan := contracts.ExecutionPlan{
		RequestID:                "req-unk-1",
		ExecutionID:              "exec-unk-1",
		MigrationID:              "mig-unk-1",
		RevisionID:               "rev-1",
		Version:                  1,
		Direction:                contracts.DirectionApply,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-1",
		TargetDatabaseID:         "db-1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "",
		PolicyVersion:            1,
		UpSQL:                    "INSERT INTO audit_events (event_name) VALUES ('init');",
		DownSQL:                  "DELETE FROM audit_events WHERE event_name = 'init';",
	}

	env1 := buildExecutionEnv(conn1, plan, "job-unk-1", "att-unk-1")
	env1.Lock = lock1

	// Attempt 1: tx.Commit() drops connection after server committed -> JobStatusUnknown + UNKNOWN gate set!
	res1, err := recoveryExec.Execute(ctx, env1)
	if err != nil {
		t.Fatalf("Attempt 1 Execute() error = %v", err)
	}
	if res1.JobStatus != contracts.JobStatusUnknown || res1.AttemptStatus != contracts.AttemptStatusUnknown {
		t.Fatalf("Attempt 1 result = %+v, want UNKNOWN/UNKNOWN", res1)
	}
	_ = lock1.Release(ctx)

	gateRec, hasGate, _ := gates.GetGate(ctx, "db-1")
	if !hasGate || gateRec.Reason != contracts.GateReasonUnknown {
		t.Fatalf("expected UNKNOWN gate on platform DB after ambiguous commit, got hasGate=%v gate=%+v", hasGate, gateRec)
	}
	if target.head.UnresolvedReason != contracts.GateReasonUnknown {
		t.Fatalf("expected target database_migration_state unresolved_reason=UNKNOWN, got %+v", target.head)
	}
	if len(target.executedUserSQL) != 1 {
		t.Fatalf("expected user SQL executed once in Attempt 1, got %d", len(target.executedUserSQL))
	}

	// Attempt 2 (Recovery on a new healthy connection holding the session advisory lock):
	// Must reconcile from execution_commits to SUCCESS without running user SQL a second time!
	conn2, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lock2, err := AcquireTargetSessionLock(ctx, conn2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock2.Release(ctx) }()

	env2 := buildExecutionEnv(conn2, plan, "job-unk-1", "att-unk-2")
	env2.Lock = lock2

	res2, err := recoveryExec.Execute(ctx, env2)
	if err != nil {
		t.Fatalf("Attempt 2 (recovery) Execute() error = %v", err)
	}
	if res2.JobStatus != contracts.JobStatusSuccess || res2.AttemptStatus != contracts.AttemptStatusCommitted {
		t.Fatalf("Attempt 2 reconciled result = %+v, want SUCCESS/COMMITTED", res2)
	}
	if len(target.executedUserSQL) != 1 {
		t.Fatalf("UNKNOWN-01 violated: user SQL was executed %d times (expected 1)", len(target.executedUserSQL))
	}
	if _, stillGated, _ := gates.GetGate(ctx, "db-1"); stillGated {
		t.Fatal("expected platform UNKNOWN gate to be cleared after reconciliation")
	}
	if target.head.UnresolvedReason != "" {
		t.Fatalf("expected target UNKNOWN gate to be cleared after reconciliation, got %+v", target.head)
	}
}

// TestRecoveryReconcilesUnknownCommitWhenProofMissingAndAllowsSafeRetry verifies UNKNOWN-02:
// When tx.Commit() fails and PostgreSQL rolled back the transaction on the server:
// 1. Attempt 1 returns UNKNOWN and sets the UNKNOWN gate.
// 2. Recovery Attempt 2 (holding the session advisory lock, proving the old session ended)
//    finds NO commit proof in execution_commits, clears the UNKNOWN gate, and safely executes
//    the migration once to completion.
func TestRecoveryReconcilesUnknownCommitWhenProofMissingAndAllowsSafeRetry(t *testing.T) {
	target := newFakeRecoveryTargetDB("target_unknown_02")
	target.commitErr = io.ErrUnexpectedEOF
	target.commitActuallySucceeds = false // Server rolled back!

	sqlDB := sql.OpenDB(fakeRecoveryConnector{target: target})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn1, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lock1, err := AcquireTargetSessionLock(ctx, conn1)
	if err != nil {
		t.Fatal(err)
	}

	gates := newMemoryGateStore()
	recoveryExec := NewRecoveryExecutor(NewOrdinaryExecutor(), gates)

	plan := contracts.ExecutionPlan{
		RequestID:                "req-unk-2",
		ExecutionID:              "exec-unk-2",
		MigrationID:              "mig-unk-2",
		RevisionID:               "rev-1",
		Version:                  1,
		Direction:                contracts.DirectionApply,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-1",
		TargetDatabaseID:         "db-2",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "",
		PolicyVersion:            1,
		UpSQL:                    "CREATE TABLE payments (id BIGINT PRIMARY KEY);",
		DownSQL:                  "DROP TABLE payments;",
	}

	env1 := buildExecutionEnv(conn1, plan, "job-unk-2", "att-unk-2-1")
	env1.Lock = lock1

	res1, err := recoveryExec.Execute(ctx, env1)
	if err != nil {
		t.Fatalf("Attempt 1 error = %v", err)
	}
	if res1.JobStatus != contracts.JobStatusUnknown {
		t.Fatalf("Attempt 1 JobStatus = %s, want UNKNOWN", res1.JobStatus)
	}
	_ = lock1.Release(ctx)

	if len(target.proofs) != 0 || len(target.history) != 0 {
		t.Fatalf("expected 0 proofs and 0 history rows after server rollback, got %d and %d", len(target.proofs), len(target.history))
	}

	// Attempt 2 acquires session advisory lock on healthy connection, sees no commit proof,
	// clears the UNKNOWN gate, and safely runs the migration to SUCCESS!
	conn2, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lock2, err := AcquireTargetSessionLock(ctx, conn2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock2.Release(ctx) }()

	env2 := buildExecutionEnv(conn2, plan, "job-unk-2", "att-unk-2-2")
	env2.Lock = lock2

	res2, err := recoveryExec.Execute(ctx, env2)
	if err != nil {
		t.Fatalf("Attempt 2 error = %v", err)
	}
	if res2.JobStatus != contracts.JobStatusSuccess || res2.AttemptStatus != contracts.AttemptStatusCommitted {
		t.Fatalf("Attempt 2 result = %+v, want SUCCESS/COMMITTED", res2)
	}
	if len(target.executedUserSQL) != 1 || len(target.history) != 1 || target.head.ExecutionID != "exec-unk-2" {
		t.Fatalf("expected 1 committed SQL & history row after safe retry, got sql=%d hist=%d head=%+v",
			len(target.executedUserSQL), len(target.history), target.head)
	}
	if _, stillGated, _ := gates.GetGate(ctx, "db-2"); stillGated {
		t.Fatal("expected platform UNKNOWN gate to be cleared")
	}
}

// TestRecoveryCannotRunWhileOldInterruptedSessionStillHoldsTargetLock verifies LOCK-02:
// A replacement worker cannot inspect execution_commits or execute SQL while the old
// backend session still holds the PostgreSQL session advisory lock on the target database.
func TestRecoveryCannotRunWhileOldInterruptedSessionStillHoldsTargetLock(t *testing.T) {
	target := newFakeRecoveryTargetDB("target_lock_02")
	sqlDB := sql.OpenDB(fakeRecoveryConnector{target: target})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	oldConn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldLock, err := AcquireTargetSessionLock(ctx, oldConn)
	if err != nil {
		t.Fatal(err)
	}

	// Replacement worker opens a new connection while oldConn is still alive on PostgreSQL.
	newConn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = newConn.Close() }()

	if _, err := AcquireTargetSessionLock(ctx, newConn); !errors.Is(err, ErrTargetLockBusy) {
		t.Fatalf("AcquireTargetSessionLock on replacement worker while old session alive = %v, want ErrTargetLockBusy", err)
	}

	// Once the old session terminates and releases its session lock, replacement worker acquires the lock!
	if err := oldLock.Release(ctx); err != nil {
		t.Fatal(err)
	}
	newLock, err := AcquireTargetSessionLock(ctx, newConn)
	if err != nil {
		t.Fatalf("AcquireTargetSessionLock after old session terminated = %v", err)
	}
	_ = newLock.Release(ctx)
}

func TestClassifyPreCommitAndCommitErrors(t *testing.T) {
	transientPreCommit := []error{
		&pgconn.PgError{Code: "08006", Message: "connection failure"},
		&pgconn.PgError{Code: "40001", Message: "could not serialize access"},
		&pgconn.PgError{Code: "40P01", Message: "deadlock detected"},
		&pgconn.PgError{Code: "55P03", Message: "lock not available"},
		driver.ErrBadConn,
		io.EOF,
		ErrTargetLockBusy,
	}
	for _, err := range transientPreCommit {
		if got := ClassifyPreCommitError(err); got != ErrorCategoryTransient {
			t.Fatalf("ClassifyPreCommitError(%v) = %s, want TRANSIENT", err, got)
		}
	}

	permanentPreCommit := []error{
		&pgconn.PgError{Code: "42601", Message: "syntax error at or near SELECT"},
		&pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"},
		&pgconn.PgError{Code: "42501", Message: "permission denied for table users"},
		contracts.ErrStaleApproval,
		contracts.ErrHeadMismatch,
		ErrForbiddenManualTxControl,
		ErrForbiddenInternalSchema,
	}
	for _, err := range permanentPreCommit {
		if got := ClassifyPreCommitError(err); got != ErrorCategoryPermanent {
			t.Fatalf("ClassifyPreCommitError(%v) = %s, want PERMANENT", err, got)
		}
	}

	if got := ClassifyCommitError(io.ErrUnexpectedEOF); got != ErrorCategoryUnknownCommit {
		t.Fatalf("ClassifyCommitError(ErrUnexpectedEOF) = %s, want UNKNOWN_COMMIT", got)
	}
	if got := ClassifyCommitError(&pgconn.PgError{Code: "08006"}); got != ErrorCategoryUnknownCommit {
		t.Fatalf("ClassifyCommitError(08006) = %s, want UNKNOWN_COMMIT", got)
	}
	if got := ClassifyCommitError(&pgconn.PgError{Code: "23505"}); got != ErrorCategoryPermanent {
		t.Fatalf("ClassifyCommitError(23505) = %s, want PERMANENT", got)
	}
}
