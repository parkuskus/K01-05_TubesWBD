package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

// CommitPhase identifies the atomic stage of an execution recorded in
// if3110_platform.execution_commits on the target database.
type CommitPhase string

const (
	CommitPhaseNormal    CommitPhase = "NORMAL"
	CommitPhaseBatchUp   CommitPhase = "BATCH_UP"
	CommitPhaseBatchDown CommitPhase = "BATCH_DOWN"
	CommitPhaseFinalize  CommitPhase = "FINALIZE"
)

func (p CommitPhase) IsValid() bool {
	switch p {
	case CommitPhaseNormal, CommitPhaseBatchUp, CommitPhaseBatchDown, CommitPhaseFinalize:
		return true
	default:
		return false
	}
}

const ensureTargetSchemaV2SQL = `
CREATE SCHEMA IF NOT EXISTS if3110_platform;

CREATE TABLE IF NOT EXISTS if3110_platform.migration_history_v2 (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    execution_id TEXT NOT NULL UNIQUE CHECK (length(btrim(execution_id)) > 0),
    job_id TEXT NOT NULL CHECK (length(btrim(job_id)) > 0),
    request_id TEXT NOT NULL CHECK (length(btrim(request_id)) > 0),
    migration_id TEXT NOT NULL CHECK (length(btrim(migration_id)) > 0),
    revision_id TEXT NOT NULL CHECK (length(btrim(revision_id)) > 0),
    version BIGINT NOT NULL CHECK (version > 0),
    direction TEXT NOT NULL CHECK (direction IN ('APPLY', 'ROLLBACK', 'FORWARD', 'RESUME', 'COMPENSATE')),
    mode TEXT NOT NULL CHECK (mode IN ('NORMAL', 'BATCH')),
    executed_sql TEXT NOT NULL CHECK (length(btrim(executed_sql)) > 0),
    plan_checksum TEXT NOT NULL CHECK (length(btrim(plan_checksum)) > 0),
    actor_id TEXT NOT NULL CHECK (length(btrim(actor_id)) > 0),
    head_before TEXT NOT NULL DEFAULT '',
    head_after TEXT NOT NULL DEFAULT '',
    reverts_execution_id TEXT NOT NULL DEFAULT '',
    committed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS migration_history_v2_committed_idx
    ON if3110_platform.migration_history_v2 (id DESC);

CREATE INDEX IF NOT EXISTS migration_history_v2_reverts_idx
    ON if3110_platform.migration_history_v2 (reverts_execution_id)
    WHERE reverts_execution_id <> '';

CREATE TABLE IF NOT EXISTS if3110_platform.execution_commits (
    execution_id TEXT NOT NULL CHECK (length(btrim(execution_id)) > 0),
    phase TEXT NOT NULL CHECK (phase IN ('NORMAL', 'BATCH_UP', 'BATCH_DOWN', 'FINALIZE')),
    batch_no INT NOT NULL DEFAULT 0 CHECK (batch_no >= 0),
    attempt_id TEXT NOT NULL CHECK (length(btrim(attempt_id)) > 0),
    executed_sql TEXT NOT NULL,
    affected_rows BIGINT NOT NULL DEFAULT 0 CHECK (affected_rows >= 0),
    cursor_after TEXT NOT NULL DEFAULT '',
    actor_id TEXT NOT NULL CHECK (length(btrim(actor_id)) > 0),
    committed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (execution_id, phase, batch_no)
);

CREATE TABLE IF NOT EXISTS if3110_platform.database_migration_state (
    singleton_id INT PRIMARY KEY DEFAULT 1 CHECK (singleton_id = 1),
    active_head_migration_id TEXT NOT NULL DEFAULT '',
    active_head_execution_id TEXT NOT NULL DEFAULT '',
    active_head_version BIGINT NOT NULL DEFAULT 0 CHECK (active_head_version >= 0),
    unresolved_execution_id TEXT NOT NULL DEFAULT '',
    unresolved_reason TEXT NOT NULL DEFAULT '' CHECK (
        unresolved_reason IN ('', 'PARTIAL', 'PARTIALLY_REVERTED', 'UNKNOWN')
    ),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO if3110_platform.database_migration_state (singleton_id)
VALUES (1)
ON CONFLICT (singleton_id) DO NOTHING;
`

type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type sqlRowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// EnsureTargetSchemaV2 initializes the Milestone 2 target database metadata
// tables inside if3110_platform without modifying or dropping the Milestone 1
// if3110_platform.migration_history table.
//
// IMPORTANT: EnsureTargetSchemaV2 must only be called on writable execution paths
// after the worker holds the target session advisory lock; it must never be called
// on View-Only read paths.
func EnsureTargetSchemaV2(ctx context.Context, conn *sql.Conn) error {
	if conn == nil {
		return ErrNilTargetConn
	}
	return EnsureTargetSchemaV2OnExecer(ctx, conn)
}

// EnsureTargetSchemaV2OnExecer runs the idempotent target schema v2 DDL on the
// provided connection or transaction.
func EnsureTargetSchemaV2OnExecer(ctx context.Context, exec sqlExecer) error {
	if exec == nil {
		return ErrNilTargetConn
	}
	if _, err := exec.ExecContext(ctx, ensureTargetSchemaV2SQL); err != nil {
		return fmt.Errorf("ensure target schema v2: %w", err)
	}
	return nil
}

// IsMissingTargetSchemaError reports whether err is a PostgreSQL undefined_table (42P01)
// or invalid_schema_name (3F000) error indicating the target database has not yet
// initialized if3110_platform tables.
func IsMissingTargetSchemaError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "3F000")
}

// TargetHeadReader implements HeadStateReader using read-only queries against
// if3110_platform.database_migration_state on the target database.
type TargetHeadReader struct{}

// ReadActiveHeadOnConn reads the current active migration head and unresolved
// gate state on conn without executing any DDL. If the target database has no
// metadata tables yet, it returns a zero ActiveHeadState and nil error.
func (TargetHeadReader) ReadActiveHeadOnConn(ctx context.Context, conn *sql.Conn) (contracts.ActiveHeadState, error) {
	if conn == nil {
		return contracts.ActiveHeadState{}, ErrNilTargetConn
	}
	return ReadActiveHead(ctx, conn)
}

// ReadActiveHead queries if3110_platform.database_migration_state on any *sql.Conn,
// *sql.DB, or *sql.Tx without running DDL.
func ReadActiveHead(ctx context.Context, q sqlRowQueryer) (contracts.ActiveHeadState, error) {
	if q == nil {
		return contracts.ActiveHeadState{}, ErrNilTargetConn
	}
	const query = `
		SELECT
			active_head_migration_id,
			active_head_execution_id,
			active_head_version,
			unresolved_execution_id,
			unresolved_reason
		FROM if3110_platform.database_migration_state
		WHERE singleton_id = 1
	`
	var (
		migID        string
		execID       string
		version      int64
		unresolvedID string
		reason       string
	)
	err := q.QueryRowContext(ctx, query).Scan(&migID, &execID, &version, &unresolvedID, &reason)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || IsMissingTargetSchemaError(err) {
			return contracts.ActiveHeadState{}, nil
		}
		return contracts.ActiveHeadState{}, fmt.Errorf("read target active head: %w", err)
	}
	return contracts.ActiveHeadState{
		MigrationID:           contracts.MigrationID(migID),
		ExecutionID:           contracts.ExecutionID(execID),
		Version:               version,
		UnresolvedExecutionID: contracts.ExecutionID(unresolvedID),
		UnresolvedReason:      contracts.GateReason(reason),
	}, nil
}

// ExecutionCommitProof represents a durable row in if3110_platform.execution_commits
// proving that a specific execution phase or batch committed on the target database.
type ExecutionCommitProof struct {
	ExecutionID  contracts.ExecutionID
	Phase        CommitPhase
	BatchNo      int
	AttemptID    contracts.AttemptID
	ExecutedSQL  string
	AffectedRows int64
	CursorAfter  string
	ActorID      contracts.UserID
	CommittedAt  time.Time
}

// LookupCommitProof checks whether a specific (execution_id, phase, batch_no)
// commit proof exists in if3110_platform.execution_commits without running DDL.
func LookupCommitProof(
	ctx context.Context,
	q sqlRowQueryer,
	executionID contracts.ExecutionID,
	phase CommitPhase,
	batchNo int,
) (ExecutionCommitProof, bool, error) {
	if q == nil {
		return ExecutionCommitProof{}, false, ErrNilTargetConn
	}
	const query = `
		SELECT
			execution_id,
			phase,
			batch_no,
			attempt_id,
			executed_sql,
			affected_rows,
			cursor_after,
			actor_id,
			committed_at
		FROM if3110_platform.execution_commits
		WHERE execution_id = $1
		  AND phase = $2
		  AND batch_no = $3
	`
	var (
		proof     ExecutionCommitProof
		execIDStr string
		phaseStr  string
		attIDStr  string
		actorStr  string
	)
	err := q.QueryRowContext(ctx, query, string(executionID), string(phase), batchNo).Scan(
		&execIDStr,
		&phaseStr,
		&proof.BatchNo,
		&attIDStr,
		&proof.ExecutedSQL,
		&proof.AffectedRows,
		&proof.CursorAfter,
		&actorStr,
		&proof.CommittedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || IsMissingTargetSchemaError(err) {
			return ExecutionCommitProof{}, false, nil
		}
		return ExecutionCommitProof{}, false, fmt.Errorf("lookup commit proof (%s, %s, %d): %w", executionID, phase, batchNo, err)
	}
	proof.ExecutionID = contracts.ExecutionID(execIDStr)
	proof.Phase = CommitPhase(phaseStr)
	proof.AttemptID = contracts.AttemptID(attIDStr)
	proof.ActorID = contracts.UserID(actorStr)
	proof.CommittedAt = proof.CommittedAt.UTC()
	return proof, true, nil
}

