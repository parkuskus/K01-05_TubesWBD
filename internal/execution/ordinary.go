package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

var (
	ErrForbiddenManualTxControl = errors.New("migration SQL must not contain manual transaction control statements (BEGIN, COMMIT, ROLLBACK)")
	ErrForbiddenInternalSchema  = errors.New("migration SQL must not reference or modify internal schema if3110_platform")
	ErrUnsupportedDirection     = errors.New("unsupported operation direction for ordinary executor")
)

// OrdinaryExecutor implements PlanExecutor for Mode == ModeNormal migrations
// (APPLY, ROLLBACK, and FORWARD) inside a single target PostgreSQL transaction.
type OrdinaryExecutor struct {
	now func() time.Time
}

// NewOrdinaryExecutor returns a ready-to-use OrdinaryExecutor.
func NewOrdinaryExecutor() *OrdinaryExecutor {
	return &OrdinaryExecutor{
		now: func() time.Time { return time.Now().UTC() },
	}
}

// WithClock overrides the time source used by the executor (for deterministic tests).
func (e *OrdinaryExecutor) WithClock(now func() time.Time) *OrdinaryExecutor {
	if now != nil {
		e.now = now
	}
	return e
}

// Execute runs an approved NORMAL-mode ExecutionPlan atomically on env.Conn.
//
// Invariants enforced:
//  1. User SQL, execution_commits proof, migration_history_v2 event, and
//     database_migration_state active_head update all share a single transaction.
//  2. If user SQL fails, the transaction rolls back completely: migration_history_v2
//     receives no row and active_head remains unchanged.
//  3. Failed attempts are recorded via the returned ExecutionResult so the worker
//     persists them in job_attempts and structured activity logs.
func (e *OrdinaryExecutor) Execute(ctx context.Context, env ExecutionEnv) (ExecutionResult, error) {
	if env.Conn == nil {
		return ExecutionResult{}, ErrNilTargetConn
	}
	plan := env.Claimed.Job.Plan
	if err := plan.Validate(); err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "INVALID_PLAN",
			ErrorMessage:  err.Error(),
		}, nil
	}
	if plan.Mode != contracts.ModeNormal {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "INVALID_MODE",
			ErrorMessage:  fmt.Sprintf("ordinary executor requires NORMAL mode, got %s", plan.Mode),
		}, nil
	}

	switch plan.Direction {
	case contracts.DirectionApply, contracts.DirectionForward, contracts.DirectionRollback:
	default:
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "UNSUPPORTED_DIRECTION",
			ErrorMessage:  ErrUnsupportedDirection.Error(),
		}, nil
	}

	sqlToRun, err := plan.SelectedSQL()
	if err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "INVALID_PLAN_SQL",
			ErrorMessage:  err.Error(),
		}, nil
	}

	if err := ValidateUserSQL(sqlToRun); err != nil {
		code := "FORBIDDEN_SQL"
		if errors.Is(err, ErrForbiddenManualTxControl) {
			code = "FORBIDDEN_TX_CONTROL"
		} else if errors.Is(err, ErrForbiddenInternalSchema) {
			code = "FORBIDDEN_INTERNAL_SCHEMA"
		}
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   sqlToRun,
			ErrorCode:     code,
			ErrorMessage:  err.Error(),
		}, nil
	}

	if err := EnsureTargetSchemaV2(ctx, env.Conn); err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusFailed,
			ExecutedSQL:   sqlToRun,
			ErrorCode:     "TARGET_SCHEMA_INIT_ERROR",
			ErrorMessage:  SanitizeExecutionError(err),
		}, nil
	}

	tx, err := env.Conn.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusFailed,
			ExecutedSQL:   sqlToRun,
			ErrorCode:     extractSQLState(err, "BEGIN_TX_ERROR"),
			ErrorMessage:  SanitizeExecutionError(err),
		}, nil
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// 1. Check execution_commits for an existing NORMAL commit proof to prevent double execution.
	if proof, found, err := LookupCommitProof(ctx, tx, plan.ExecutionID, CommitPhaseNormal, 0); err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   sqlToRun,
			ErrorCode:     extractSQLState(err, "PROOF_LOOKUP_ERROR"),
			ErrorMessage:  SanitizeExecutionError(err),
		}, nil
	} else if found {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusSuccess,
			AttemptStatus: contracts.AttemptStatusCommitted,
			ExecutedSQL:   proof.ExecutedSQL,
			CommittedRows: proof.AffectedRows,
		}, nil
	}

	// 2. Lock and verify current database_migration_state inside the transaction.
	currentHead, err := lockAndReadMigrationState(ctx, tx)
	if err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   sqlToRun,
			ErrorCode:     extractSQLState(err, "STATE_LOCK_ERROR"),
			ErrorMessage:  SanitizeExecutionError(err),
		}, nil
	}

	if currentHead.UnresolvedReason != "" {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusBlocked,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   sqlToRun,
			ErrorCode:     "EXECUTION_GATE_BLOCKED",
			ErrorMessage:  contracts.ErrExecutionGateBlocked.Error(),
		}, nil
	}

	if currentHead.ExecutionID != plan.ExpectedActiveHead {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   sqlToRun,
			ErrorCode:     "HEAD_MISMATCH",
			ErrorMessage:  contracts.ErrHeadMismatch.Error(),
		}, nil
	}

	var (
		nextHeadMigrationID string
		nextHeadExecutionID string
		nextHeadVersion     int64
		revertsExecutionID  string
	)

	if plan.Direction == contracts.DirectionRollback {
		if plan.OriginExecutionID != currentHead.ExecutionID {
			return ExecutionResult{
				JobStatus:     contracts.JobStatusFailed,
				AttemptStatus: contracts.AttemptStatusRolledBack,
				ExecutedSQL:   sqlToRun,
				ErrorCode:     "HEAD_MISMATCH",
				ErrorMessage:  contracts.ErrHeadMismatch.Error(),
			}, nil
		}
		prevMigID, prevExecID, prevVersion, err := resolveRollbackTargetHead(ctx, tx, plan.OriginExecutionID)
		if err != nil {
			return ExecutionResult{
				JobStatus:     contracts.JobStatusFailed,
				AttemptStatus: contracts.AttemptStatusRolledBack,
				ExecutedSQL:   sqlToRun,
				ErrorCode:     extractSQLState(err, "ROLLBACK_HEAD_LOOKUP_ERROR"),
				ErrorMessage:  SanitizeExecutionError(err),
			}, nil
		}
		nextHeadMigrationID = prevMigID
		nextHeadExecutionID = prevExecID
		nextHeadVersion = prevVersion
		revertsExecutionID = string(plan.OriginExecutionID)
	} else {
		nextHeadMigrationID = string(plan.MigrationID)
		nextHeadExecutionID = string(plan.ExecutionID)
		nextHeadVersion = plan.Version
		revertsExecutionID = ""
	}

	// 3. Execute the immutable user SQL inside the target transaction.
	_, affectedRows, execErr := execUserSQLWithTelemetry(ctx, tx, sqlToRun)
	if execErr != nil {
		rollbackTxWithTelemetry(ctx, tx, sqlToRun, execErr)
		return ExecutionResult{
			JobStatus:          contracts.JobStatusFailed,
			AttemptStatus:      contracts.AttemptStatusRolledBack,
			ExecutedSQL:        sqlToRun,
			ErrorCode:          extractSQLState(execErr, "SQL_EXECUTION_ERROR"),
			ErrorMessage:       SanitizeExecutionError(execErr),
			TransientRetryable: IsTransientPreCommitError(execErr),
		}, nil
	}

	now := time.Now().UTC()
	if e != nil && e.now != nil {
		now = e.now().UTC()
	}

	planChecksum := strings.TrimSpace(plan.PlanChecksum)
	if planChecksum == "" {
		planChecksum = plan.ComputeChecksum()
	}

	// 4. Write atomic commit proof, append-only migration_history_v2 event, and active_head update.
	const insertCommitProofSQL = `
		INSERT INTO if3110_platform.execution_commits (
			execution_id,
			phase,
			batch_no,
			attempt_id,
			executed_sql,
			affected_rows,
			cursor_after,
			actor_id,
			committed_at
		) VALUES (
			$1, $2, 0, $3, $4, $5, '', $6, $7
		)
	`
	if _, err := tx.ExecContext(
		ctx,
		insertCommitProofSQL,
		string(plan.ExecutionID),
		string(CommitPhaseNormal),
		string(env.Claimed.Attempt.ID),
		sqlToRun,
		affectedRows,
		string(plan.ActorID),
		now,
	); err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   sqlToRun,
			ErrorCode:     extractSQLState(err, "INSERT_COMMIT_PROOF_ERROR"),
			ErrorMessage:  SanitizeExecutionError(err),
		}, nil
	}

	const insertHistoryV2SQL = `
		INSERT INTO if3110_platform.migration_history_v2 (
			execution_id,
			job_id,
			request_id,
			migration_id,
			revision_id,
			version,
			direction,
			mode,
			executed_sql,
			plan_checksum,
			actor_id,
			head_before,
			head_after,
			reverts_execution_id,
			committed_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
		)
	`
	if _, err := tx.ExecContext(
		ctx,
		insertHistoryV2SQL,
		string(plan.ExecutionID),
		string(env.Claimed.Job.ID),
		string(plan.RequestID),
		string(plan.MigrationID),
		string(plan.RevisionID),
		plan.Version,
		string(plan.Direction),
		string(plan.Mode),
		sqlToRun,
		planChecksum,
		string(plan.ActorID),
		string(currentHead.ExecutionID),
		nextHeadExecutionID,
		revertsExecutionID,
		now,
	); err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   sqlToRun,
			ErrorCode:     extractSQLState(err, "INSERT_HISTORY_V2_ERROR"),
			ErrorMessage:  SanitizeExecutionError(err),
		}, nil
	}

	const updateMigrationStateSQL = `
		UPDATE if3110_platform.database_migration_state
		SET active_head_migration_id = $1,
		    active_head_execution_id = $2,
		    active_head_version = $3,
		    updated_at = $4
		WHERE singleton_id = 1
	`
	if _, err := tx.ExecContext(
		ctx,
		updateMigrationStateSQL,
		nextHeadMigrationID,
		nextHeadExecutionID,
		nextHeadVersion,
		now,
	); err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   sqlToRun,
			ErrorCode:     extractSQLState(err, "UPDATE_HEAD_STATE_ERROR"),
			ErrorMessage:  SanitizeExecutionError(err),
		}, nil
	}

	if err := commitTxWithTelemetry(ctx, tx, sqlToRun, affectedRows); err != nil {
		switch ClassifyCommitError(err) {
		case ErrorCategoryPermanent:
			return ExecutionResult{
				JobStatus:     contracts.JobStatusFailed,
				AttemptStatus: contracts.AttemptStatusRolledBack,
				ExecutedSQL:   sqlToRun,
				ErrorCode:     extractSQLState(err, "COMMIT_FAILED"),
				ErrorMessage:  SanitizeExecutionError(err),
			}, nil
		case ErrorCategoryTransient:
			return ExecutionResult{
				JobStatus:          contracts.JobStatusFailed,
				AttemptStatus:      contracts.AttemptStatusRolledBack,
				ExecutedSQL:        sqlToRun,
				ErrorCode:          extractSQLState(err, "COMMIT_RETRYABLE"),
				ErrorMessage:       SanitizeExecutionError(err),
				TransientRetryable: true,
			}, nil
		default:
			return ExecutionResult{
				JobStatus:     contracts.JobStatusUnknown,
				AttemptStatus: contracts.AttemptStatusUnknown,
				ExecutedSQL:   sqlToRun,
				ErrorCode:     extractSQLState(err, "COMMIT_UNKNOWN"),
				ErrorMessage:  SanitizeExecutionError(err),
			}, nil
		}
	}

	return ExecutionResult{
		JobStatus:     contracts.JobStatusSuccess,
		AttemptStatus: contracts.AttemptStatusCommitted,
		ExecutedSQL:   sqlToRun,
		CommittedRows: affectedRows,
	}, nil
}

func lockAndReadMigrationState(ctx context.Context, tx *sql.Tx) (contracts.ActiveHeadState, error) {
	const query = `
		SELECT
			active_head_migration_id,
			active_head_execution_id,
			active_head_version,
			unresolved_execution_id,
			unresolved_reason
		FROM if3110_platform.database_migration_state
		WHERE singleton_id = 1
		FOR UPDATE
	`
	var (
		migID        string
		execID       string
		version      int64
		unresolvedID string
		reason       string
	)
	if err := tx.QueryRowContext(ctx, query).Scan(&migID, &execID, &version, &unresolvedID, &reason); err != nil {
		return contracts.ActiveHeadState{}, err
	}
	return contracts.ActiveHeadState{
		MigrationID:           contracts.MigrationID(migID),
		ExecutionID:           contracts.ExecutionID(execID),
		Version:               version,
		UnresolvedExecutionID: contracts.ExecutionID(unresolvedID),
		UnresolvedReason:      contracts.GateReason(reason),
	}, nil
}

func resolveRollbackTargetHead(
	ctx context.Context,
	tx *sql.Tx,
	originExecutionID contracts.ExecutionID,
) (string, string, int64, error) {
	var headBefore string
	err := tx.QueryRowContext(
		ctx,
		`SELECT head_before FROM if3110_platform.migration_history_v2 WHERE execution_id = $1`,
		string(originExecutionID),
	).Scan(&headBefore)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", 0, nil
		}
		return "", "", 0, err
	}
	headBefore = strings.TrimSpace(headBefore)
	if headBefore == "" {
		return "", "", 0, nil
	}

	var (
		prevMigID   string
		prevVersion int64
	)
	err = tx.QueryRowContext(
		ctx,
		`SELECT migration_id, version FROM if3110_platform.migration_history_v2 WHERE execution_id = $1`,
		headBefore,
	).Scan(&prevMigID, &prevVersion)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", headBefore, 0, nil
		}
		return "", "", 0, err
	}
	return prevMigID, headBefore, prevVersion, nil
}

// ValidateUserSQL rejects empty SQL, manual transaction control statements
// (BEGIN, START TRANSACTION, COMMIT, ROLLBACK, SAVEPOINT), and any reference
// to the internal if3110_platform schema outside quoted string literals and comments.
func ValidateUserSQL(sqlText string) error {
	trimmed := strings.TrimSpace(sqlText)
	if trimmed == "" {
		return contracts.ErrInvalidExecutionPlan
	}

	stripped := stripLiteralsAndComments(trimmed)
	lowerStripped := strings.ToLower(stripped)
	if strings.Contains(lowerStripped, "if3110_platform") {
		return ErrForbiddenInternalSchema
	}

	for _, stmt := range strings.Split(lowerStripped, ";") {
		tokens := tokenizeWords(stmt)
		if len(tokens) == 0 {
			continue
		}
		switch tokens[0] {
		case "begin", "commit", "rollback", "savepoint":
			return ErrForbiddenManualTxControl
		case "start":
			if len(tokens) >= 2 && tokens[1] == "transaction" {
				return ErrForbiddenManualTxControl
			}
		case "release":
			if len(tokens) >= 2 && tokens[1] == "savepoint" {
				return ErrForbiddenManualTxControl
			}
		case "end":
			if len(tokens) == 1 || (len(tokens) >= 2 && (tokens[1] == "transaction" || tokens[1] == "work")) {
				return ErrForbiddenManualTxControl
			}
		}
	}
	return nil
}

func stripLiteralsAndComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	n := len(s)
	for i := 0; i < n; {
		// Line comment --
		if i+1 < n && s[i] == '-' && s[i+1] == '-' {
			i += 2
			for i < n && s[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
			continue
		}
		// Block comment /* ... */
		if i+1 < n && s[i] == '/' && s[i+1] == '*' {
			i += 2
			for i+1 < n && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			}
			b.WriteByte(' ')
			continue
		}
		// Single-quoted string literal '...'
		if s[i] == '\'' {
			i++
			for i < n {
				if s[i] == '\'' {
					if i+1 < n && s[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func tokenizeWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
}

func extractSQLState(err error, fallback string) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.TrimSpace(pgErr.Code) != "" {
		return pgErr.Code
	}
	return fallback
}

// SanitizeExecutionError formats a database error for user-facing job/attempt
// records without leaking connection strings or passwords.
func SanitizeExecutionError(err error) string {
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Detail != "" {
			return fmt.Sprintf("SQLSTATE %s: %s (%s)", pgErr.Code, pgErr.Message, pgErr.Detail)
		}
		return fmt.Sprintf("SQLSTATE %s: %s", pgErr.Code, pgErr.Message)
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "password=") || strings.Contains(lower, "postgres://") || strings.Contains(lower, "postgresql://") {
		return "database execution error"
	}
	return msg
}

