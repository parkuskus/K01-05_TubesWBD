package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/jobs"
)

// GateStore manages per-database execution gates in the platform database.
type GateStore interface {
	SetGate(
		ctx context.Context,
		databaseID contracts.DatabaseID,
		executionID contracts.ExecutionID,
		jobID contracts.JobID,
		reason contracts.GateReason,
		details string,
	) error
	ClearGate(ctx context.Context, databaseID contracts.DatabaseID) error
	GetGate(ctx context.Context, databaseID contracts.DatabaseID) (GateRecord, bool, error)
}

// PostgresGateStore implements GateStore using the platform database_execution_gates table.
type PostgresGateStore struct {
	db *sql.DB
}

// NewPostgresGateStore creates a PostgresGateStore backed by platformDB.
func NewPostgresGateStore(platformDB *sql.DB) *PostgresGateStore {
	return &PostgresGateStore{db: platformDB}
}

func (s *PostgresGateStore) SetGate(
	ctx context.Context,
	databaseID contracts.DatabaseID,
	executionID contracts.ExecutionID,
	jobID contracts.JobID,
	reason contracts.GateReason,
	details string,
) error {
	if s == nil || s.db == nil {
		return errors.New("platform gate store database is required")
	}
	return UpsertExecutionGate(ctx, s.db, databaseID, executionID, jobID, reason, details)
}

func (s *PostgresGateStore) ClearGate(ctx context.Context, databaseID contracts.DatabaseID) error {
	if s == nil || s.db == nil {
		return errors.New("platform gate store database is required")
	}
	return ClearExecutionGate(ctx, s.db, databaseID)
}

func (s *PostgresGateStore) GetGate(ctx context.Context, databaseID contracts.DatabaseID) (GateRecord, bool, error) {
	if s == nil || s.db == nil {
		return GateRecord{}, false, errors.New("platform gate store database is required")
	}
	var (
		rec    GateRecord
		dbID   string
		execID string
		jobID  string
		reason string
	)
	err := s.db.QueryRowContext(
		ctx,
		`SELECT database_id, execution_id, job_id, reason, details, created_at, updated_at
		 FROM database_execution_gates
		 WHERE database_id = $1`,
		string(databaseID),
	).Scan(
		&dbID,
		&execID,
		&jobID,
		&reason,
		&rec.Details,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GateRecord{}, false, nil
		}
		return GateRecord{}, false, fmt.Errorf("get execution gate for %s: %w", databaseID, err)
	}
	rec.DatabaseID = contracts.DatabaseID(dbID)
	rec.ExecutionID = contracts.ExecutionID(execID)
	rec.JobID = contracts.JobID(jobID)
	rec.Reason = contracts.GateReason(reason)
	rec.CreatedAt = rec.CreatedAt.UTC()
	rec.UpdatedAt = rec.UpdatedAt.UTC()
	return rec, true, nil
}

// SetTargetMigrationGate records an unresolved gate reason (PARTIAL, PARTIALLY_REVERTED, or UNKNOWN)
// inside if3110_platform.database_migration_state on the target database.
func SetTargetMigrationGate(
	ctx context.Context,
	exec sqlExecer,
	executionID contracts.ExecutionID,
	reason contracts.GateReason,
) error {
	if exec == nil {
		return ErrNilTargetConn
	}
	if !reason.IsValid() {
		return fmt.Errorf("invalid target gate reason %q", reason)
	}
	_, err := exec.ExecContext(
		ctx,
		`UPDATE if3110_platform.database_migration_state
		 SET unresolved_execution_id = $1,
		     unresolved_reason = $2,
		     updated_at = NOW()
		 WHERE singleton_id = 1`,
		string(executionID),
		string(reason),
	)
	if err != nil {
		return fmt.Errorf("set target migration gate (%s, %s): %w", executionID, reason, err)
	}
	return nil
}

// ClearTargetMigrationGate clears any unresolved gate in if3110_platform.database_migration_state
// on the target database.
func ClearTargetMigrationGate(ctx context.Context, exec sqlExecer) error {
	if exec == nil {
		return ErrNilTargetConn
	}
	_, err := exec.ExecContext(
		ctx,
		`UPDATE if3110_platform.database_migration_state
		 SET unresolved_execution_id = '',
		     unresolved_reason = '',
		     updated_at = NOW()
		 WHERE singleton_id = 1`,
	)
	if err != nil {
		if IsMissingTargetSchemaError(err) {
			return nil
		}
		return fmt.Errorf("clear target migration gate: %w", err)
	}
	return nil
}

// ReconcileResult reports the outcome of inspecting target commit proofs for an
// interrupted or UNKNOWN execution while holding the target session advisory lock.
type ReconcileResult struct {
	ProofFound  bool
	Proof       ExecutionCommitProof
	GateCleared bool
}

// ReconcileUnknownExecution inspects if3110_platform.execution_commits on a locked
// target connection to determine whether a previously interrupted execution
// committed or rolled back on PostgreSQL.
//
// Invariant (LOCK-02): The caller MUST hold an active *TargetSessionLock on a
// healthy connection before calling ReconcileUnknownExecution. Because PostgreSQL
// session advisory locks are bound to backend sessions, acquiring the session
// lock proves that any previous interrupted backend session on the same target
// database has terminated and its transaction is no longer in-flight.
func ReconcileUnknownExecution(
	ctx context.Context,
	lock *TargetSessionLock,
	gates GateStore,
	job jobs.Job,
) (ReconcileResult, error) {
	if lock == nil || lock.Conn() == nil {
		return ReconcileResult{}, ErrNilTargetConn
	}

	phase := CommitPhaseNormal
	if job.Mode == contracts.ModeBatch {
		phase = CommitPhaseFinalize
	}

	proof, found, err := LookupCommitProof(ctx, lock.Conn(), job.ExecutionID, phase, 0)
	if err != nil {
		return ReconcileResult{}, err
	}

	// Clear any UNKNOWN gate left by the interrupted attempt now that the session
	// lock is held and the commit outcome is deterministically known.
	gateCleared := false
	head, headErr := ReadActiveHead(ctx, lock.Conn())
	if headErr == nil && head.UnresolvedReason == contracts.GateReasonUnknown &&
		(head.UnresolvedExecutionID == "" || head.UnresolvedExecutionID == job.ExecutionID) {
		if err := ClearTargetMigrationGate(ctx, lock.Conn()); err != nil {
			return ReconcileResult{}, err
		}
		gateCleared = true
	}

	if gates != nil {
		if gateRec, exists, err := gates.GetGate(ctx, job.DatabaseID); err != nil {
			return ReconcileResult{}, err
		} else if exists && gateRec.Reason == contracts.GateReasonUnknown &&
			(gateRec.ExecutionID == "" || gateRec.ExecutionID == job.ExecutionID) {
			if err := gates.ClearGate(ctx, job.DatabaseID); err != nil {
				return ReconcileResult{}, err
			}
			gateCleared = true
		}
	}

	return ReconcileResult{
		ProofFound:  found,
		Proof:       proof,
		GateCleared: gateCleared,
	}, nil
}

// RecoveryExecutor wraps a PlanExecutor with pre-execution commit proof
// reconciliation (UNKNOWN-01 / UNKNOWN-02) and automatic UNKNOWN gate activation
// when a transaction commit outcome is ambiguous.
type RecoveryExecutor struct {
	inner PlanExecutor
	gates GateStore
}

// NewRecoveryExecutor creates a RecoveryExecutor wrapping inner and using gates
// to persist platform-level execution gates.
func NewRecoveryExecutor(inner PlanExecutor, gates GateStore) *RecoveryExecutor {
	if inner == nil {
		inner = NewOrdinaryExecutor()
	}
	return &RecoveryExecutor{
		inner: inner,
		gates: gates,
	}
}

// Execute reconciles any prior interrupted attempt under the target session lock
// before delegating to the inner executor, and sets an UNKNOWN execution gate if
// tx.Commit() fails with an ambiguous outcome.
func (r *RecoveryExecutor) Execute(ctx context.Context, env ExecutionEnv) (ExecutionResult, error) {
	if env.Conn == nil {
		return ExecutionResult{}, ErrNilTargetConn
	}
	lock := env.Lock
	if lock == nil {
		lock = &TargetSessionLock{conn: env.Conn}
	}

	// 1. Reconcile against target commit proofs before executing any SQL.
	reconciled, err := ReconcileUnknownExecution(ctx, lock, r.gates, env.Claimed.Job)
	if err != nil {
		if IsTransientPreCommitError(err) {
			return ExecutionResult{
				AttemptStatus:      contracts.AttemptStatusFailed,
				ErrorCode:          extractSQLState(err, "RECONCILE_TRANSIENT_ERROR"),
				ErrorMessage:       SanitizeExecutionError(err),
				TransientRetryable: true,
			}, nil
		}
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     extractSQLState(err, "RECONCILE_ERROR"),
			ErrorMessage:  SanitizeExecutionError(err),
		}, nil
	}

	// UNKNOWN-01: Commit proof exists! Reconcile to SUCCESS without re-executing user SQL.
	if reconciled.ProofFound {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusSuccess,
			AttemptStatus: contracts.AttemptStatusCommitted,
			ExecutedSQL:   reconciled.Proof.ExecutedSQL,
			CommittedRows: reconciled.Proof.AffectedRows,
		}, nil
	}

	// 2. No commit proof exists (and holding the session lock proves any prior
	// interrupted transaction rolled back). Proceed with inner execution.
	res, execErr := r.inner.Execute(ctx, env)

	// 3. If commit outcome is UNKNOWN, activate the UNKNOWN gate on platform and target
	// so no subsequent migration can run until a future reconciliation proves the outcome.
	if res.JobStatus == contracts.JobStatusUnknown {
		job := env.Claimed.Job
		details := strings.TrimSpace(res.ErrorMessage)
		if details == "" {
			details = "commit outcome is unknown due to connection interruption during COMMIT"
		}
		if r.gates != nil {
			_ = r.gates.SetGate(ctx, job.DatabaseID, job.ExecutionID, job.ID, contracts.GateReasonUnknown, details)
		}
		_ = SetTargetMigrationGate(ctx, env.Conn, job.ExecutionID, contracts.GateReasonUnknown)
		return res, execErr
	}

	// 4. Classify pre-commit transient SQLSTATE errors (e.g. 40001, 40P01, 55P03, 08006)
	// so the worker schedules a bounded backoff retry instead of failing permanently.
	if res.JobStatus == contracts.JobStatusFailed && !res.TransientRetryable && isTransientErrorCode(res.ErrorCode) {
		res.TransientRetryable = true
	}

	return res, execErr
}

func isTransientErrorCode(code string) bool {
	code = strings.TrimSpace(code)
	if code == "" {
		return false
	}
	return strings.HasPrefix(code, "08") ||
		strings.HasPrefix(code, "40") ||
		code == "55P03" ||
		code == "53300" ||
		code == "57P01" ||
		code == "57P02" ||
		code == "57P03"
}

