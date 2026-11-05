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
	"if3110.local/db-platform/internal/jobs"
)

type fakeOrdinaryTargetDB struct {
	schemaInitialized bool
	head              contracts.ActiveHeadState
	history           []contracts.CommitHistoryEvent
	proofs            map[string]ExecutionCommitProof
	executedUserSQL   []string
	failNextUserSQL   error

	inTx          bool
	stagedHead    contracts.ActiveHeadState
	stagedHistory []contracts.CommitHistoryEvent
	stagedProofs  map[string]ExecutionCommitProof
	stagedUserSQL []string
}

func newFakeOrdinaryTargetDB() *fakeOrdinaryTargetDB {
	return &fakeOrdinaryTargetDB{
		proofs: make(map[string]ExecutionCommitProof),
	}
}

type fakeOrdinaryConnector struct {
	db *fakeOrdinaryTargetDB
}

func (c fakeOrdinaryConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeOrdinaryConn{db: c.db}, nil
}

func (fakeOrdinaryConnector) Driver() driver.Driver { return fakeOrdinaryDriver{} }

type fakeOrdinaryDriver struct{}

func (fakeOrdinaryDriver) Open(string) (driver.Conn, error) { return nil, errors.New("unused") }

type fakeOrdinaryConn struct {
	db *fakeOrdinaryTargetDB
}

func (*fakeOrdinaryConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (*fakeOrdinaryConn) Close() error                        { return nil }

func (c *fakeOrdinaryConn) Begin() (driver.Tx, error) {
	c.db.inTx = true
	c.db.stagedHead = c.db.head
	c.db.stagedHistory = append([]contracts.CommitHistoryEvent(nil), c.db.history...)
	c.db.stagedProofs = make(map[string]ExecutionCommitProof, len(c.db.proofs))
	for k, v := range c.db.proofs {
		c.db.stagedProofs[k] = v
	}
	c.db.stagedUserSQL = append([]string(nil), c.db.executedUserSQL...)
	return &fakeOrdinaryTx{db: c.db}, nil
}

type fakeOrdinaryTx struct {
	db *fakeOrdinaryTargetDB
}

func (tx *fakeOrdinaryTx) Commit() error {
	if tx.db.inTx {
		tx.db.head = tx.db.stagedHead
		tx.db.history = tx.db.stagedHistory
		tx.db.proofs = tx.db.stagedProofs
		tx.db.executedUserSQL = tx.db.stagedUserSQL
		tx.db.inTx = false
	}
	return nil
}

func (tx *fakeOrdinaryTx) Rollback() error {
	if tx.db.inTx {
		tx.db.stagedHistory = nil
		tx.db.stagedProofs = nil
		tx.db.stagedUserSQL = nil
		tx.db.inTx = false
	}
	return nil
}

func (c *fakeOrdinaryConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	trimmed := strings.TrimSpace(query)
	switch {
	case strings.Contains(trimmed, "CREATE TABLE IF NOT EXISTS if3110_platform.migration_history_v2"):
		c.db.schemaInitialized = true
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "INSERT INTO if3110_platform.execution_commits"):
		proof := ExecutionCommitProof{
			ExecutionID:  contracts.ExecutionID(args[0].Value.(string)),
			Phase:        CommitPhase(args[1].Value.(string)),
			BatchNo:      0,
			AttemptID:    contracts.AttemptID(args[2].Value.(string)),
			ExecutedSQL:  args[3].Value.(string),
			AffectedRows: args[4].Value.(int64),
			ActorID:      contracts.UserID(args[5].Value.(string)),
			CommittedAt:  args[6].Value.(time.Time),
		}
		key := string(proof.ExecutionID) + "|" + string(proof.Phase) + "|0"
		c.db.stagedProofs[key] = proof
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "INSERT INTO if3110_platform.migration_history_v2"):
		ev := contracts.CommitHistoryEvent{
			ID:                 int64(len(c.db.stagedHistory) + 1),
			ExecutionID:        contracts.ExecutionID(args[0].Value.(string)),
			JobID:              contracts.JobID(args[1].Value.(string)),
			RequestID:          contracts.OperationRequestID(args[2].Value.(string)),
			MigrationID:        contracts.MigrationID(args[3].Value.(string)),
			RevisionID:         contracts.RevisionID(args[4].Value.(string)),
			Version:            args[5].Value.(int64),
			Direction:          contracts.OperationDirection(args[6].Value.(string)),
			Mode:               contracts.ExecutionMode(args[7].Value.(string)),
			ExecutedSQL:        args[8].Value.(string),
			PlanChecksum:       args[9].Value.(string),
			ActorID:            contracts.UserID(args[10].Value.(string)),
			HeadBefore:         contracts.ExecutionID(args[11].Value.(string)),
			HeadAfter:          contracts.ExecutionID(args[12].Value.(string)),
			RevertsExecutionID: contracts.ExecutionID(args[13].Value.(string)),
			CommittedAt:        args[14].Value.(time.Time),
		}
		c.db.stagedHistory = append(c.db.stagedHistory, ev)
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "UPDATE if3110_platform.database_migration_state"):
		c.db.stagedHead.MigrationID = contracts.MigrationID(args[0].Value.(string))
		c.db.stagedHead.ExecutionID = contracts.ExecutionID(args[1].Value.(string))
		c.db.stagedHead.Version = args[2].Value.(int64)
		return driver.RowsAffected(1), nil

	default:
		// User SQL statement inside the transaction!
		if c.db.failNextUserSQL != nil {
			err := c.db.failNextUserSQL
			c.db.failNextUserSQL = nil
			return nil, err
		}
		c.db.stagedUserSQL = append(c.db.stagedUserSQL, trimmed)
		return driver.RowsAffected(1), nil
	}
}

func (c *fakeOrdinaryConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	trimmed := strings.TrimSpace(query)
	switch {
	case strings.Contains(trimmed, "FROM if3110_platform.execution_commits"):
		execID := args[0].Value.(string)
		phase := args[1].Value.(string)
		key := execID + "|" + phase + "|0"
		proofsMap := c.db.proofs
		if c.db.inTx {
			proofsMap = c.db.stagedProofs
		}
		if proof, ok := proofsMap[key]; ok {
			return &fakeCommitProofRows{proofs: []ExecutionCommitProof{proof}}, nil
		}
		return &fakeCommitProofRows{}, nil

	case strings.Contains(trimmed, "FROM if3110_platform.database_migration_state"):
		head := c.db.head
		if c.db.inTx {
			head = c.db.stagedHead
		}
		return &fakeTargetStateRows{head: head}, nil

	case strings.HasPrefix(trimmed, "SELECT head_before FROM if3110_platform.migration_history_v2"):
		targetExecID := contracts.ExecutionID(args[0].Value.(string))
		hist := c.db.history
		if c.db.inTx {
			hist = c.db.stagedHistory
		}
		for _, ev := range hist {
			if ev.ExecutionID == targetExecID {
				return &lockSingleValueRows{col: "head_before", val: string(ev.HeadBefore)}, nil
			}
		}
		return &fakeEmptyRows{cols: []string{"head_before"}}, nil

	case strings.HasPrefix(trimmed, "SELECT migration_id, version FROM if3110_platform.migration_history_v2"):
		targetExecID := contracts.ExecutionID(args[0].Value.(string))
		hist := c.db.history
		if c.db.inTx {
			hist = c.db.stagedHistory
		}
		for _, ev := range hist {
			if ev.ExecutionID == targetExecID {
				return &fakeMigVersionRows{migID: string(ev.MigrationID), version: ev.Version}, nil
			}
		}
		return &fakeEmptyRows{cols: []string{"migration_id", "version"}}, nil

	default:
		return nil, errors.New("unexpected QueryContext query: " + trimmed)
	}
}

type fakeEmptyRows struct {
	cols []string
}

func (r *fakeEmptyRows) Columns() []string        { return r.cols }
func (*fakeEmptyRows) Close() error               { return nil }
func (*fakeEmptyRows) Next(_ []driver.Value) error { return io.EOF }

type fakeMigVersionRows struct {
	migID   string
	version int64
	done    bool
}

func (*fakeMigVersionRows) Columns() []string  { return []string{"migration_id", "version"} }
func (*fakeMigVersionRows) Close() error       { return nil }
func (r *fakeMigVersionRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.migID
	dest[1] = r.version
	return nil
}

func buildExecutionEnv(conn *sql.Conn, plan contracts.ExecutionPlan, jobID, attemptID string) ExecutionEnv {
	plan.PlanChecksum = plan.ComputeChecksum()
	return ExecutionEnv{
		Conn: conn,
		Claimed: jobs.ClaimedJob{
			Job: jobs.Job{
				ID:          contracts.JobID(jobID),
				RequestID:   plan.RequestID,
				ExecutionID: plan.ExecutionID,
				DatabaseID:  plan.TargetDatabaseID,
				Direction:   plan.Direction,
				Mode:        plan.Mode,
				ActorID:     plan.ActorID,
				Plan:        plan,
			},
			Attempt: jobs.JobAttempt{
				ID:          contracts.AttemptID(attemptID),
				JobID:       contracts.JobID(jobID),
				ExecutionID: plan.ExecutionID,
				AttemptNo:   1,
				WorkerID:    "worker-1",
				Status:      contracts.AttemptStatusStarted,
			},
		},
	}
}

// TestOrdinaryExecutorApplyRollbackAndForwardAtomicLifecycle verifies EXEC-02:
// 1. APPLY #1 and APPLY #2 execute UpSQL, write commit proofs and migration_history_v2 events, and advance active_head.
// 2. ROLLBACK of #2 executes immutable DownSQL, records reverts_execution_id = exec-2, and rewinds active_head to #1.
// 3. FORWARD #3 executes UpSQL from the rewound head #1 and advances active_head to #3.
func TestOrdinaryExecutorApplyRollbackAndForwardAtomicLifecycle(t *testing.T) {
	targetDB := newFakeOrdinaryTargetDB()
	sqlDB := sql.OpenDB(fakeOrdinaryConnector{db: targetDB})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	fixedNow := time.Date(2026, 11, 5, 16, 0, 0, 0, time.UTC)
	exec := NewOrdinaryExecutor().WithClock(func() time.Time { return fixedNow })

	// 1. APPLY Migration 1 (E1)
	plan1 := contracts.ExecutionPlan{
		RequestID:                "req-1",
		ExecutionID:              "exec-1",
		MigrationID:              "mig-1",
		RevisionID:               "rev-1",
		Version:                  1,
		Direction:                contracts.DirectionApply,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-1",
		TargetDatabaseID:         "db-1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "",
		PolicyVersion:            1,
		UpSQL:                    "CREATE TABLE orders (id BIGINT PRIMARY KEY);",
		DownSQL:                  "DROP TABLE orders;",
	}
	res1, err := exec.Execute(ctx, buildExecutionEnv(conn, plan1, "job-1", "att-1"))
	if err != nil {
		t.Fatalf("Execute E1 error = %v", err)
	}
	if res1.JobStatus != contracts.JobStatusSuccess || res1.AttemptStatus != contracts.AttemptStatusCommitted {
		t.Fatalf("E1 result = %+v, want SUCCESS/COMMITTED", res1)
	}
	if targetDB.head.ExecutionID != "exec-1" || targetDB.head.Version != 1 {
		t.Fatalf("head after E1 = %+v, want exec-1 v1", targetDB.head)
	}

	// 2. APPLY Migration 2 (E2)
	plan2 := contracts.ExecutionPlan{
		RequestID:                "req-2",
		ExecutionID:              "exec-2",
		MigrationID:              "mig-2",
		RevisionID:               "rev-2",
		Version:                  2,
		Direction:                contracts.DirectionApply,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-1",
		TargetDatabaseID:         "db-1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "exec-1",
		PolicyVersion:            1,
		UpSQL:                    "ALTER TABLE orders ADD COLUMN note TEXT;",
		DownSQL:                  "ALTER TABLE orders DROP COLUMN note;",
	}
	res2, err := exec.Execute(ctx, buildExecutionEnv(conn, plan2, "job-2", "att-2"))
	if err != nil || res2.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("Execute E2 = %+v, %v", res2, err)
	}
	if targetDB.head.ExecutionID != "exec-2" || targetDB.head.MigrationID != "mig-2" || targetDB.head.Version != 2 {
		t.Fatalf("head after E2 = %+v, want exec-2/mig-2/v2", targetDB.head)
	}

	// 3. ROLLBACK Migration 2 (E3 reverts E2) -> rewinds active_head to E1!
	plan3Rollback := contracts.ExecutionPlan{
		RequestID:                "req-3",
		ExecutionID:              "exec-3",
		MigrationID:              "mig-2",
		RevisionID:               "rev-2",
		Version:                  2,
		Direction:                contracts.DirectionRollback,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-1",
		TargetDatabaseID:         "db-1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "exec-2",
		OriginExecutionID:        "exec-2",
		PolicyVersion:            1,
		UpSQL:                    "ALTER TABLE orders ADD COLUMN note TEXT;",
		DownSQL:                  "ALTER TABLE orders DROP COLUMN note;",
	}
	res3, err := exec.Execute(ctx, buildExecutionEnv(conn, plan3Rollback, "job-3", "att-3"))
	if err != nil || res3.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("Execute E3 (ROLLBACK) = %+v, %v", res3, err)
	}
	if res3.ExecutedSQL != "ALTER TABLE orders DROP COLUMN note;" {
		t.Fatalf("E3 ExecutedSQL = %q, want DownSQL", res3.ExecutedSQL)
	}
	if targetDB.head.ExecutionID != "exec-1" || targetDB.head.MigrationID != "mig-1" || targetDB.head.Version != 1 {
		t.Fatalf("head after ROLLBACK E3 = %+v, want rewound to exec-1/mig-1/v1", targetDB.head)
	}
	if len(targetDB.history) != 3 || targetDB.history[2].RevertsExecutionID != "exec-2" {
		t.Fatalf("history[2] = %+v, want RevertsExecutionID=exec-2", targetDB.history)
	}

	// 4. FORWARD Migration 3 (E4) from rewound head exec-1
	plan4Forward := contracts.ExecutionPlan{
		RequestID:                "req-4",
		ExecutionID:              "exec-4",
		MigrationID:              "mig-3",
		RevisionID:               "rev-3",
		Version:                  3,
		Direction:                contracts.DirectionForward,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-1",
		TargetDatabaseID:         "db-1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "exec-1",
		PolicyVersion:            1,
		UpSQL:                    "ALTER TABLE orders ADD COLUMN status TEXT DEFAULT 'NEW';",
		DownSQL:                  "ALTER TABLE orders DROP COLUMN status;",
	}
	res4, err := exec.Execute(ctx, buildExecutionEnv(conn, plan4Forward, "job-4", "att-4"))
	if err != nil || res4.JobStatus != contracts.JobStatusSuccess {
		t.Fatalf("Execute E4 (FORWARD) = %+v, %v", res4, err)
	}
	if targetDB.head.ExecutionID != "exec-4" || targetDB.head.MigrationID != "mig-3" || targetDB.head.Version != 3 {
		t.Fatalf("head after FORWARD E4 = %+v, want exec-4/mig-3/v3", targetDB.head)
	}
	if len(targetDB.history) != 4 {
		t.Fatalf("len(history) = %d, want 4 append-only events", len(targetDB.history))
	}
}

// TestOrdinaryExecutorRollsBackCleanlyOnSQLFailure verifies EXEC-02 failure atomicity:
// When user SQL fails, the transaction rolls back completely, migration_history_v2
// does not grow, and active_head does not change.
func TestOrdinaryExecutorRollsBackCleanlyOnSQLFailure(t *testing.T) {
	targetDB := newFakeOrdinaryTargetDB()
	targetDB.head = contracts.ActiveHeadState{
		MigrationID: "mig-1",
		ExecutionID: "exec-1",
		Version:     1,
	}
	targetDB.failNextUserSQL = &pgconn.PgError{
		Code:    "42P07",
		Message: `relation "orders" already exists`,
	}

	sqlDB := sql.OpenDB(fakeOrdinaryConnector{db: targetDB})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	exec := NewOrdinaryExecutor()
	planFail := contracts.ExecutionPlan{
		RequestID:                "req-fail",
		ExecutionID:              "exec-fail",
		MigrationID:              "mig-2",
		RevisionID:               "rev-2",
		Version:                  2,
		Direction:                contracts.DirectionApply,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-1",
		TargetDatabaseID:         "db-1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "exec-1",
		PolicyVersion:            1,
		UpSQL:                    "CREATE TABLE orders (id BIGINT PRIMARY KEY);",
		DownSQL:                  "DROP TABLE orders;",
	}

	res, err := exec.Execute(ctx, buildExecutionEnv(conn, planFail, "job-fail", "att-fail"))
	if err != nil {
		t.Fatalf("Execute() returned unexpected Go error = %v", err)
	}
	if res.JobStatus != contracts.JobStatusFailed || res.AttemptStatus != contracts.AttemptStatusRolledBack {
		t.Fatalf("failed SQL result = %+v, want FAILED/ROLLED_BACK", res)
	}
	if res.ErrorCode != "42P07" || !strings.Contains(res.ErrorMessage, "42P07") {
		t.Fatalf("expected SQLSTATE 42P07 in result, got code=%q msg=%q", res.ErrorCode, res.ErrorMessage)
	}

	// Verify atomic rollback: zero rows in migration_history_v2, zero commit proofs, active_head unchanged!
	if len(targetDB.history) != 0 {
		t.Fatalf("migration_history_v2 grew on failed SQL: %+v", targetDB.history)
	}
	if len(targetDB.proofs) != 0 {
		t.Fatalf("execution_commits grew on failed SQL: %+v", targetDB.proofs)
	}
	if targetDB.head.ExecutionID != "exec-1" || targetDB.head.Version != 1 {
		t.Fatalf("active_head changed on failed SQL: %+v", targetDB.head)
	}
	if len(targetDB.executedUserSQL) != 0 {
		t.Fatalf("staged user SQL was not rolled back: %v", targetDB.executedUserSQL)
	}
}

func TestOrdinaryExecutorSkipsDuplicateExecutionWhenCommitProofExists(t *testing.T) {
	targetDB := newFakeOrdinaryTargetDB()
	targetDB.proofs["exec-dup|NORMAL|0"] = ExecutionCommitProof{
		ExecutionID:  "exec-dup",
		Phase:        CommitPhaseNormal,
		BatchNo:      0,
		AttemptID:    "att-prev",
		ExecutedSQL:  "CREATE TABLE widgets (id INT);",
		AffectedRows: 1,
		ActorID:      "user-1",
		CommittedAt:  time.Now().UTC(),
	}

	sqlDB := sql.OpenDB(fakeOrdinaryConnector{db: targetDB})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	exec := NewOrdinaryExecutor()
	plan := contracts.ExecutionPlan{
		RequestID:                "req-dup",
		ExecutionID:              "exec-dup",
		MigrationID:              "mig-dup",
		RevisionID:               "rev-1",
		Version:                  1,
		Direction:                contracts.DirectionApply,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-1",
		TargetDatabaseID:         "db-1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "",
		PolicyVersion:            1,
		UpSQL:                    "CREATE TABLE widgets (id INT);",
		DownSQL:                  "DROP TABLE widgets;",
	}

	res, err := exec.Execute(ctx, buildExecutionEnv(conn, plan, "job-dup", "att-dup-2"))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.JobStatus != contracts.JobStatusSuccess || res.AttemptStatus != contracts.AttemptStatusCommitted {
		t.Fatalf("expected idempotent SUCCESS/COMMITTED from existing proof, got %+v", res)
	}
	if len(targetDB.executedUserSQL) != 0 {
		t.Fatalf("expected 0 user SQL executions when commit proof already exists, got %d", len(targetDB.executedUserSQL))
	}
}

func TestValidateUserSQLRejectsManualTxControlAndInternalSchema(t *testing.T) {
	forbidden := []struct {
		sql     string
		wantErr error
	}{
		{"BEGIN; CREATE TABLE t (id INT); COMMIT;", ErrForbiddenManualTxControl},
		{"START TRANSACTION; ALTER TABLE t ADD COLUMN x INT;", ErrForbiddenManualTxControl},
		{"ROLLBACK;", ErrForbiddenManualTxControl},
		{"SAVEPOINT sp1;", ErrForbiddenManualTxControl},
		{"RELEASE SAVEPOINT sp1;", ErrForbiddenManualTxControl},
		{"DELETE FROM if3110_platform.migration_history_v2;", ErrForbiddenInternalSchema},
		{"DROP SCHEMA IF3110_PLATFORM CASCADE;", ErrForbiddenInternalSchema},
	}

	for _, tc := range forbidden {
		err := ValidateUserSQL(tc.sql)
		if !errors.Is(err, tc.wantErr) {
			t.Fatalf("ValidateUserSQL(%q) = %v, want %v", tc.sql, err, tc.wantErr)
		}
	}

	allowed := []string{
		"CREATE TABLE audit_log (id BIGINT PRIMARY KEY, action TEXT DEFAULT 'begin commit rollback if3110_platform');",
		"-- comment mentioning BEGIN and if3110_platform\nALTER TABLE users ADD COLUMN committed_at TIMESTAMPTZ;",
		"/* block comment COMMIT */ UPDATE orders SET status = 'COMPLETED' WHERE id = 1;",
	}
	for _, sqlText := range allowed {
		if err := ValidateUserSQL(sqlText); err != nil {
			t.Fatalf("ValidateUserSQL(%q) unexpected error = %v", sqlText, err)
		}
	}
}

