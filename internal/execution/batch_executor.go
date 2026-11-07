package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/jobs"
)

var ErrInvalidBatchUpdateSQL = errors.New("batch migration SQL must be a single UPDATE statement")

// JobProgressUpdater updates real-time committed batch progress on the platform jobs table.
type JobProgressUpdater interface {
	UpdateProgress(
		ctx context.Context,
		jobID contracts.JobID,
		leaseToken string,
		committedBatches int,
		committedRows int64,
		totalScopeRows int64,
		lastCursor string,
	) error
}

// BatchPauseChecker optionally reports whether an in-flight batch execution should
// pause cleanly at the next committed batch boundary.
type BatchPauseChecker func(ctx context.Context, jobID contracts.JobID, nextBatchNo int) bool

// BatchExecutor orchestrates chunked keyset-paginated batch execution,
// checkpointing, pause/resume, and partial failure gating on a target database.
type BatchExecutor struct {
	gates        GateStore
	progress     JobProgressUpdater
	shouldPause  BatchPauseChecker
	now          func() time.Time
}

// NewBatchExecutor creates a BatchExecutor with optional platform GateStore and
// JobProgressUpdater.
func NewBatchExecutor(gates GateStore, progress JobProgressUpdater) *BatchExecutor {
	return &BatchExecutor{
		gates:    gates,
		progress: progress,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// WithClock overrides the time source used by BatchExecutor (for deterministic tests).
func (e *BatchExecutor) WithClock(now func() time.Time) *BatchExecutor {
	if now != nil {
		e.now = now
	}
	return e
}

// WithPauseChecker configures a callback invoked before each batch transaction
// to support safe pause/cancel at committed batch boundaries.
func (e *BatchExecutor) WithPauseChecker(fn BatchPauseChecker) *BatchExecutor {
	e.shouldPause = fn
	return e
}

// BuildScopedBatchUpdateSQL rewrites a user UPDATE statement so that it only
// modifies rows recorded in if3110_platform.batch_recovery_rows for ($1 = execution_id, $2 = batch_no).
func BuildScopedBatchUpdateSQL(upSQL string, cfg contracts.BatchConfig) (string, error) {
	if err := ValidateUserSQL(upSQL); err != nil {
		return "", err
	}
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	quotedPK, err := QuoteIdentifier(cfg.PrimaryKeyColumn)
	if err != nil {
		return "", err
	}

	trimmed := strings.TrimSpace(upSQL)
	trimmed = strings.TrimSuffix(trimmed, ";")
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" {
		return "", ErrInvalidBatchUpdateSQL
	}

	masked := maskLiteralsAndComments(trimmed)
	if strings.Contains(masked, ";") {
		return "", ErrInvalidBatchUpdateSQL
	}
	words := tokenizeWords(strings.ToLower(masked))
	if len(words) == 0 || words[0] != "update" {
		return "", ErrInvalidBatchUpdateSQL
	}

	batchFilter := fmt.Sprintf(
		`%s::bigint IN (SELECT row_id FROM if3110_platform.batch_recovery_rows WHERE execution_id = $1 AND batch_no = $2)`,
		quotedPK,
	)

	whereIdx := findTopLevelWhereIndex(masked)
	if whereIdx >= 0 {
		prefix := strings.TrimSpace(trimmed[:whereIdx])
		condition := strings.TrimSpace(trimmed[whereIdx+len("where"):])
		if condition == "" {
			return "", ErrInvalidBatchUpdateSQL
		}
		return fmt.Sprintf("%s WHERE (%s) AND %s", prefix, condition, batchFilter), nil
	}

	return fmt.Sprintf("%s WHERE %s", trimmed, batchFilter), nil
}

// maskLiteralsAndComments replaces string literals and comments with spaces of
// identical byte length so byte offsets in the masked string match the original SQL.
func maskLiteralsAndComments(s string) string {
	buf := []byte(s)
	n := len(buf)
	for i := 0; i < n; {
		if i+1 < n && buf[i] == '-' && buf[i+1] == '-' {
			for i < n && buf[i] != '\n' {
				buf[i] = ' '
				i++
			}
			continue
		}
		if i+1 < n && buf[i] == '/' && buf[i+1] == '*' {
			buf[i] = ' '
			buf[i+1] = ' '
			i += 2
			for i+1 < n && !(buf[i] == '*' && buf[i+1] == '/') {
				buf[i] = ' '
				i++
			}
			if i+1 < n {
				buf[i] = ' '
				buf[i+1] = ' '
				i += 2
			}
			continue
		}
		if buf[i] == '\'' {
			buf[i] = ' '
			i++
			for i < n {
				if buf[i] == '\'' {
					buf[i] = ' '
					if i+1 < n && buf[i+1] == '\'' {
						buf[i+1] = ' '
						i += 2
						continue
					}
					i++
					break
				}
				buf[i] = ' '
				i++
			}
			continue
		}
		i++
	}
	return string(buf)
}

func findTopLevelWhereIndex(stripped string) int {
	lower := strings.ToLower(stripped)
	depth := 0
	n := len(lower)
	for i := 0; i < n; i++ {
		switch lower[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && strings.HasPrefix(lower[i:], "where") {
				prevOK := i == 0 || !isIdentChar(lower[i-1])
				endIdx := i + len("where")
				nextOK := endIdx == n || !isIdentChar(lower[endIdx])
				if prevOK && nextOK {
					return i
				}
			}
		}
	}
	return -1
}

func isIdentChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

// ExecuteBatchUp freezes the target row scope (if not already frozen) and processes
// rows in keyset-paginated batch transactions (WHERE row_id > last_cursor ORDER BY row_id ASC LIMIT batch_size).
//
// Each batch commits its before-images, row updates, after-images, execution_commits
// proof, and batch_checkpoints update atomically in its own transaction, followed by
// a real-time progress update on the platform jobs table.
func (e *BatchExecutor) ExecuteBatchUp(ctx context.Context, env ExecutionEnv) (ExecutionResult, error) {
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
	if plan.Mode != contracts.ModeBatch {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "INVALID_MODE",
			ErrorMessage:  fmt.Sprintf("batch executor requires BATCH mode, got %s", plan.Mode),
		}, nil
	}

	targetExecutionID := plan.ExecutionID
	if plan.Direction == contracts.DirectionResume && strings.TrimSpace(string(plan.OriginExecutionID)) != "" {
		targetExecutionID = plan.OriginExecutionID
	}

	scopedUpdateSQL, err := BuildScopedBatchUpdateSQL(plan.UpSQL, plan.Batch)
	if err != nil {
		code := "INVALID_BATCH_SQL"
		if errors.Is(err, ErrForbiddenManualTxControl) {
			code = "FORBIDDEN_TX_CONTROL"
		} else if errors.Is(err, ErrForbiddenInternalSchema) {
			code = "FORBIDDEN_INTERNAL_SCHEMA"
		}
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   plan.UpSQL,
			ErrorCode:     code,
			ErrorMessage:  err.Error(),
		}, nil
	}

	quotedSchema, err := QuoteIdentifier(plan.Batch.TableSchema)
	if err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "INVALID_BATCH_IDENTIFIER",
			ErrorMessage:  err.Error(),
		}, nil
	}
	quotedTable, err := QuoteIdentifier(plan.Batch.TableName)
	if err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "INVALID_BATCH_IDENTIFIER",
			ErrorMessage:  err.Error(),
		}, nil
	}
	quotedPK, err := QuoteIdentifier(plan.Batch.PrimaryKeyColumn)
	if err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "INVALID_BATCH_IDENTIFIER",
			ErrorMessage:  err.Error(),
		}, nil
	}

	// Freeze scope idempotently (or load existing frozen scope and checkpoint on RESUME).
	scope, ckpt, err := FreezeBatchScope(ctx, env.Conn, targetExecutionID, plan.Batch)
	if err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   plan.UpSQL,
			ErrorCode:     extractSQLState(err, "BATCH_SCOPE_FREEZE_ERROR"),
			ErrorMessage:  SanitizeExecutionError(err),
		}, nil
	}

	// Check if FINALIZE commit proof already exists (idempotent replay).
	if proof, found, err := LookupCommitProof(ctx, env.Conn, targetExecutionID, CommitPhaseFinalize, 0); err == nil && found {
		return ExecutionResult{
			JobStatus:           contracts.JobStatusSuccess,
			AttemptStatus:       contracts.AttemptStatusCommitted,
			ExecutedSQL:         proof.ExecutedSQL,
			CommittedBatches:    ckpt.CommittedBatches,
			CommittedRows:       ckpt.CommittedRows,
			TotalScopeRows:      scope.TotalRows,
			LastCommittedCursor: strconv.FormatInt(ckpt.LastCursor, 10),
		}, nil
	}

	committedBatches := ckpt.CommittedBatches
	committedRows := ckpt.CommittedRows
	lastCursor := ckpt.LastCursor

	for {
		nextBatchNo := committedBatches + 1

		// Check for cooperative pause/cancellation at a safe batch boundary BEFORE starting the next transaction.
		if ctx.Err() != nil || (e.shouldPause != nil && e.shouldPause(ctx, env.Claimed.Job.ID, nextBatchNo)) {
			return e.haltOnPartialBoundary(
				ctx,
				env,
				targetExecutionID,
				scopedUpdateSQL,
				committedBatches,
				committedRows,
				scope.TotalRows,
				lastCursor,
				"BATCH_PAUSED_AT_BOUNDARY",
				"batch execution paused safely at committed batch boundary",
			)
		}

		batchIDs, err := selectNextBatchRowIDs(ctx, env.Conn, targetExecutionID, lastCursor, scope.BatchSize)
		if err != nil {
			return e.haltOnPartialBoundary(
				ctx,
				env,
				targetExecutionID,
				scopedUpdateSQL,
				committedBatches,
				committedRows,
				scope.TotalRows,
				lastCursor,
				extractSQLState(err, "BATCH_KEYSET_ERROR"),
				SanitizeExecutionError(err),
			)
		}
		if len(batchIDs) == 0 {
			break
		}

		minBatchID := batchIDs[0]
		maxBatchID := batchIDs[len(batchIDs)-1]

		batchRowsAffected, batchStatus, errCode, errMsg := e.runSingleUpBatchTx(
			ctx,
			env,
			targetExecutionID,
			nextBatchNo,
			minBatchID,
			maxBatchID,
			quotedSchema,
			quotedTable,
			quotedPK,
			scopedUpdateSQL,
			committedRows,
		)
		if batchStatus != contracts.JobStatusSuccess {
			if batchStatus == contracts.JobStatusUnknown {
				if e.gates != nil {
					_ = e.gates.SetGate(ctx, env.Claimed.Job.DatabaseID, targetExecutionID, env.Claimed.Job.ID, contracts.GateReasonUnknown, errMsg)
				}
				_ = SetTargetMigrationGate(ctx, env.Conn, targetExecutionID, contracts.GateReasonUnknown)
				return ExecutionResult{
					JobStatus:           contracts.JobStatusUnknown,
					AttemptStatus:       contracts.AttemptStatusUnknown,
					ExecutedSQL:         scopedUpdateSQL,
					ErrorCode:           errCode,
					ErrorMessage:        errMsg,
					CommittedBatches:    committedBatches,
					CommittedRows:       committedRows,
					TotalScopeRows:      scope.TotalRows,
					LastCommittedCursor: formatCursor(committedBatches, lastCursor),
				}, nil
			}

			return e.haltOnPartialBoundary(
				ctx,
				env,
				targetExecutionID,
				scopedUpdateSQL,
				committedBatches,
				committedRows,
				scope.TotalRows,
				lastCursor,
				errCode,
				errMsg,
			)
		}

		committedBatches = nextBatchNo
		committedRows += batchRowsAffected
		lastCursor = maxBatchID

		if e.progress != nil {
			if err := e.progress.UpdateProgress(
				ctx,
				env.Claimed.Job.ID,
				env.Claimed.Job.LeaseToken,
				committedBatches,
				committedRows,
				scope.TotalRows,
				strconv.FormatInt(lastCursor, 10),
			); errors.Is(err, jobs.ErrLeaseMismatch) {
				return ExecutionResult{
					JobStatus:           contracts.JobStatusPartial,
					AttemptStatus:       contracts.AttemptStatusFailed,
					ExecutedSQL:         scopedUpdateSQL,
					ErrorCode:           "LEASE_MISMATCH",
					ErrorMessage:        err.Error(),
					CommittedBatches:    committedBatches,
					CommittedRows:       committedRows,
					TotalScopeRows:      scope.TotalRows,
					LastCommittedCursor: strconv.FormatInt(lastCursor, 10),
				}, nil
			}
		}
	}

	return e.finalizeBatchUp(
		ctx,
		env,
		targetExecutionID,
		scopedUpdateSQL,
		committedBatches,
		committedRows,
		scope.TotalRows,
		lastCursor,
	)
}

func (e *BatchExecutor) runSingleUpBatchTx(
	ctx context.Context,
	env ExecutionEnv,
	targetExecutionID contracts.ExecutionID,
	batchNo int,
	minBatchID int64,
	maxBatchID int64,
	quotedSchema string,
	quotedTable string,
	quotedPK string,
	scopedUpdateSQL string,
	committedRowsBefore int64,
) (batchRowsAffected int64, batchStatus contracts.JobStatus, errCode string, errMsg string) {
	ctx, finishBatchSpan := startBatchSpan(ctx, batchNo, env.Claimed.Job.Plan.Batch.BatchSize, env.Claimed.Job.Direction)
	defer func() {
		finishBatchSpan(batchRowsAffected, batchStatus, errCode, errMsg)
	}()

	tx, err := env.Conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, contracts.JobStatusPartial, extractSQLState(err, "BATCH_BEGIN_TX_ERROR"), SanitizeExecutionError(err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// Prevent double execution if this batch_no already committed.
	if proof, found, err := LookupCommitProof(ctx, tx, targetExecutionID, CommitPhaseBatchUp, batchNo); err != nil {
		return 0, contracts.JobStatusPartial, extractSQLState(err, "BATCH_PROOF_LOOKUP_ERROR"), SanitizeExecutionError(err)
	} else if found {
		return proof.AffectedRows, contracts.JobStatusSuccess, "", ""
	}

	// 1. Lock target rows in this keyset window and record before_data into batch_recovery_rows.
	captureBeforeSQL := fmt.Sprintf(
		`INSERT INTO if3110_platform.batch_recovery_rows (execution_id, batch_no, row_id, before_data, after_data)
		 SELECT $1, $2, s.row_id, to_jsonb(t.*), to_jsonb(t.*)
		 FROM if3110_platform.batch_scope_rows s
		 JOIN %s.%s t ON t.%s::bigint = s.row_id
		 WHERE s.execution_id = $1
		   AND s.row_id >= $3
		   AND s.row_id <= $4
		 ORDER BY s.row_id ASC
		 FOR UPDATE OF t`,
		quotedSchema,
		quotedTable,
		quotedPK,
	)
	if _, err := tx.ExecContext(ctx, captureBeforeSQL, string(targetExecutionID), batchNo, minBatchID, maxBatchID); err != nil {
		_ = tx.Rollback()
		return 0, contracts.JobStatusPartial, extractSQLState(err, "BATCH_CAPTURE_BEFORE_ERROR"), SanitizeExecutionError(err)
	}

	// 2. Execute the user's UPDATE scoped strictly to the rows of (targetExecutionID, batchNo).
	_, affected, err := execUserSQLWithTelemetry(ctx, tx, scopedUpdateSQL, string(targetExecutionID), batchNo)
	if err != nil {
		rollbackTxWithTelemetry(ctx, tx, scopedUpdateSQL, err)
		return 0, contracts.JobStatusPartial, extractSQLState(err, "BATCH_UPDATE_SQL_ERROR"), SanitizeExecutionError(err)
	}

	// 3. Record after_data in batch_recovery_rows so reverse compensation can verify post-images.
	captureAfterSQL := fmt.Sprintf(
		`UPDATE if3110_platform.batch_recovery_rows r
		 SET after_data = to_jsonb(t.*)
		 FROM %s.%s t
		 WHERE r.execution_id = $1
		   AND r.batch_no = $2
		   AND t.%s::bigint = r.row_id`,
		quotedSchema,
		quotedTable,
		quotedPK,
	)
	if _, err := tx.ExecContext(ctx, captureAfterSQL, string(targetExecutionID), batchNo); err != nil {
		_ = tx.Rollback()
		return 0, contracts.JobStatusPartial, extractSQLState(err, "BATCH_CAPTURE_AFTER_ERROR"), SanitizeExecutionError(err)
	}

	now := time.Now().UTC()
	if e != nil && e.now != nil {
		now = e.now().UTC()
	}

	// 4. Write atomic batch commit proof in execution_commits (phase = 'BATCH_UP', batch_no).
	const insertBatchProofSQL = `
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
			$1, $2, $3, $4, $5, $6, $7, $8, $9
		)
	`
	if _, err := tx.ExecContext(
		ctx,
		insertBatchProofSQL,
		string(targetExecutionID),
		string(CommitPhaseBatchUp),
		batchNo,
		string(env.Claimed.Attempt.ID),
		scopedUpdateSQL,
		affected,
		strconv.FormatInt(maxBatchID, 10),
		string(env.Claimed.Job.ActorID),
		now,
	); err != nil {
		_ = tx.Rollback()
		return 0, contracts.JobStatusPartial, extractSQLState(err, "BATCH_INSERT_PROOF_ERROR"), SanitizeExecutionError(err)
	}

	// 5. Advance durable checkpoint in batch_checkpoints inside the same batch transaction.
	const updateCheckpointSQL = `
		UPDATE if3110_platform.batch_checkpoints
		SET phase = $1,
		    committed_batches = $2,
		    committed_rows = $3,
		    last_cursor = $4,
		    updated_at = $5
		WHERE execution_id = $6
	`
	if _, err := tx.ExecContext(
		ctx,
		updateCheckpointSQL,
		string(CommitPhaseBatchUp),
		batchNo,
		committedRowsBefore+affected,
		maxBatchID,
		now,
		string(targetExecutionID),
	); err != nil {
		_ = tx.Rollback()
		return 0, contracts.JobStatusPartial, extractSQLState(err, "BATCH_UPDATE_CHECKPOINT_ERROR"), SanitizeExecutionError(err)
	}

	// 6. Commit this batch transaction!
	if err := commitTxWithTelemetry(ctx, tx, scopedUpdateSQL, affected); err != nil {
		if ClassifyCommitError(err) == ErrorCategoryUnknownCommit {
			return 0, contracts.JobStatusUnknown, extractSQLState(err, "BATCH_COMMIT_UNKNOWN"), SanitizeExecutionError(err)
		}
		return 0, contracts.JobStatusPartial, extractSQLState(err, "BATCH_COMMIT_ERROR"), SanitizeExecutionError(err)
	}

	return affected, contracts.JobStatusSuccess, "", ""
}

func selectNextBatchRowIDs(
	ctx context.Context,
	conn *sql.Conn,
	executionID contracts.ExecutionID,
	lastCursor int64,
	batchSize int,
) ([]int64, error) {
	const query = `
		SELECT row_id
		FROM if3110_platform.batch_scope_rows
		WHERE execution_id = $1
		  AND row_id > $2
		ORDER BY row_id ASC
		LIMIT $3
	`
	rows, err := conn.QueryContext(ctx, query, string(executionID), lastCursor, batchSize)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func formatCursor(committedBatches int, lastCursor int64) string {
	if committedBatches <= 0 {
		return ""
	}
	return strconv.FormatInt(lastCursor, 10)
}

func (e *BatchExecutor) haltOnPartialBoundary(
	ctx context.Context,
	env ExecutionEnv,
	targetExecutionID contracts.ExecutionID,
	executedSQL string,
	committedBatches int,
	committedRows int64,
	totalScopeRows int64,
	lastCursor int64,
	errCode string,
	errMsg string,
) (ExecutionResult, error) {
	baseCtx := context.Background()
	if ctx != nil {
		baseCtx = context.WithoutCancel(ctx)
	}
	if e.gates != nil {
		_ = e.gates.SetGate(
			baseCtx,
			env.Claimed.Job.DatabaseID,
			targetExecutionID,
			env.Claimed.Job.ID,
			contracts.GateReasonPartial,
			errMsg,
		)
	}
	_ = SetTargetMigrationGate(baseCtx, env.Conn, targetExecutionID, contracts.GateReasonPartial)

	return ExecutionResult{
		JobStatus:           contracts.JobStatusPartial,
		AttemptStatus:       contracts.AttemptStatusFailed,
		ExecutedSQL:         executedSQL,
		ErrorCode:           errCode,
		ErrorMessage:        errMsg,
		CommittedBatches:    committedBatches,
		CommittedRows:       committedRows,
		TotalScopeRows:      totalScopeRows,
		LastCommittedCursor: formatCursor(committedBatches, lastCursor),
	}, nil
}

func (e *BatchExecutor) finalizeBatchUp(
	ctx context.Context,
	env ExecutionEnv,
	targetExecutionID contracts.ExecutionID,
	executedSQL string,
	committedBatches int,
	committedRows int64,
	totalScopeRows int64,
	lastCursor int64,
) (ExecutionResult, error) {
	plan := env.Claimed.Job.Plan

	tx, err := env.Conn.BeginTx(ctx, nil)
	if err != nil {
		return e.haltOnPartialBoundary(
			ctx,
			env,
			targetExecutionID,
			executedSQL,
			committedBatches,
			committedRows,
			totalScopeRows,
			lastCursor,
			extractSQLState(err, "BATCH_FINALIZE_BEGIN_ERROR"),
			SanitizeExecutionError(err),
		)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if proof, found, err := LookupCommitProof(ctx, tx, targetExecutionID, CommitPhaseFinalize, 0); err != nil {
		return e.haltOnPartialBoundary(
			ctx,
			env,
			targetExecutionID,
			executedSQL,
			committedBatches,
			committedRows,
			totalScopeRows,
			lastCursor,
			extractSQLState(err, "BATCH_FINALIZE_PROOF_ERROR"),
			SanitizeExecutionError(err),
		)
	} else if found {
		if e.gates != nil {
			_ = e.gates.ClearGate(ctx, env.Claimed.Job.DatabaseID)
		}
		return ExecutionResult{
			JobStatus:           contracts.JobStatusSuccess,
			AttemptStatus:       contracts.AttemptStatusCommitted,
			ExecutedSQL:         proof.ExecutedSQL,
			CommittedBatches:    committedBatches,
			CommittedRows:       committedRows,
			TotalScopeRows:      totalScopeRows,
			LastCommittedCursor: formatCursor(committedBatches, lastCursor),
		}, nil
	}

	currentHead, err := lockAndReadMigrationState(ctx, tx)
	if err != nil {
		return e.haltOnPartialBoundary(
			ctx,
			env,
			targetExecutionID,
			executedSQL,
			committedBatches,
			committedRows,
			totalScopeRows,
			lastCursor,
			extractSQLState(err, "BATCH_FINALIZE_STATE_ERROR"),
			SanitizeExecutionError(err),
		)
	}

	now := time.Now().UTC()
	if e != nil && e.now != nil {
		now = e.now().UTC()
	}

	planChecksum := strings.TrimSpace(plan.PlanChecksum)
	if planChecksum == "" {
		planChecksum = plan.ComputeChecksum()
	}

	const insertFinalizeProofSQL = `
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
			$1, $2, 0, $3, $4, $5, $6, $7, $8
		)
	`
	if _, err := tx.ExecContext(
		ctx,
		insertFinalizeProofSQL,
		string(targetExecutionID),
		string(CommitPhaseFinalize),
		string(env.Claimed.Attempt.ID),
		plan.UpSQL,
		committedRows,
		formatCursor(committedBatches, lastCursor),
		string(plan.ActorID),
		now,
	); err != nil {
		return e.haltOnPartialBoundary(
			ctx,
			env,
			targetExecutionID,
			executedSQL,
			committedBatches,
			committedRows,
			totalScopeRows,
			lastCursor,
			extractSQLState(err, "BATCH_FINALIZE_INSERT_PROOF_ERROR"),
			SanitizeExecutionError(err),
		)
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
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, '', $14
		)
	`
	if _, err := tx.ExecContext(
		ctx,
		insertHistoryV2SQL,
		string(targetExecutionID),
		string(env.Claimed.Job.ID),
		string(plan.RequestID),
		string(plan.MigrationID),
		string(plan.RevisionID),
		plan.Version,
		string(contracts.DirectionApply),
		string(contracts.ModeBatch),
		plan.UpSQL,
		planChecksum,
		string(plan.ActorID),
		string(currentHead.ExecutionID),
		string(targetExecutionID),
		now,
	); err != nil {
		return e.haltOnPartialBoundary(
			ctx,
			env,
			targetExecutionID,
			executedSQL,
			committedBatches,
			committedRows,
			totalScopeRows,
			lastCursor,
			extractSQLState(err, "BATCH_FINALIZE_HISTORY_ERROR"),
			SanitizeExecutionError(err),
		)
	}

	const updateCheckpointFinalizeSQL = `
		UPDATE if3110_platform.batch_checkpoints
		SET phase = $1,
		    updated_at = $2
		WHERE execution_id = $3
	`
	if _, err := tx.ExecContext(
		ctx,
		updateCheckpointFinalizeSQL,
		string(CommitPhaseFinalize),
		now,
		string(targetExecutionID),
	); err != nil {
		return e.haltOnPartialBoundary(
			ctx,
			env,
			targetExecutionID,
			executedSQL,
			committedBatches,
			committedRows,
			totalScopeRows,
			lastCursor,
			extractSQLState(err, "BATCH_FINALIZE_CHECKPOINT_ERROR"),
			SanitizeExecutionError(err),
		)
	}

	const updateStateFinalizeSQL = `
		UPDATE if3110_platform.database_migration_state
		SET active_head_migration_id = $1,
		    active_head_execution_id = $2,
		    active_head_version = $3,
		    unresolved_execution_id = '',
		    unresolved_reason = '',
		    updated_at = $4
		WHERE singleton_id = 1
	`
	if _, err := tx.ExecContext(
		ctx,
		updateStateFinalizeSQL,
		string(plan.MigrationID),
		string(targetExecutionID),
		plan.Version,
		now,
	); err != nil {
		return e.haltOnPartialBoundary(
			ctx,
			env,
			targetExecutionID,
			executedSQL,
			committedBatches,
			committedRows,
			totalScopeRows,
			lastCursor,
			extractSQLState(err, "BATCH_FINALIZE_STATE_UPDATE_ERROR"),
			SanitizeExecutionError(err),
		)
	}

	if err := tx.Commit(); err != nil {
		if ClassifyCommitError(err) == ErrorCategoryUnknownCommit {
			if e.gates != nil {
				_ = e.gates.SetGate(ctx, env.Claimed.Job.DatabaseID, targetExecutionID, env.Claimed.Job.ID, contracts.GateReasonUnknown, SanitizeExecutionError(err))
			}
			_ = SetTargetMigrationGate(ctx, env.Conn, targetExecutionID, contracts.GateReasonUnknown)
			return ExecutionResult{
				JobStatus:           contracts.JobStatusUnknown,
				AttemptStatus:       contracts.AttemptStatusUnknown,
				ExecutedSQL:         executedSQL,
				ErrorCode:           extractSQLState(err, "BATCH_FINALIZE_COMMIT_UNKNOWN"),
				ErrorMessage:        SanitizeExecutionError(err),
				CommittedBatches:    committedBatches,
				CommittedRows:       committedRows,
				TotalScopeRows:      totalScopeRows,
				LastCommittedCursor: formatCursor(committedBatches, lastCursor),
			}, nil
		}
		return e.haltOnPartialBoundary(
			ctx,
			env,
			targetExecutionID,
			executedSQL,
			committedBatches,
			committedRows,
			totalScopeRows,
			lastCursor,
			extractSQLState(err, "BATCH_FINALIZE_COMMIT_ERROR"),
			SanitizeExecutionError(err),
		)
	}

	if e.gates != nil {
		_ = e.gates.ClearGate(ctx, env.Claimed.Job.DatabaseID)
	}

	return ExecutionResult{
		JobStatus:           contracts.JobStatusSuccess,
		AttemptStatus:       contracts.AttemptStatusCommitted,
		ExecutedSQL:         plan.UpSQL,
		CommittedBatches:    committedBatches,
		CommittedRows:       committedRows,
		TotalScopeRows:      totalScopeRows,
		LastCommittedCursor: formatCursor(committedBatches, lastCursor),
	}, nil
}

