package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

var (
	ErrUnsafeBatchCompensation = errors.New("unsafe batch compensation: target row was modified or deleted externally after batch commit; create a forward-fix migration instead")
)

// BuildBatchCompensateSQL validates an approved batch DownSQL statement and
// scopes it to join against if3110_platform.batch_recovery_rows br for ($1 = execution_id, $2 = batch_no)
// so only rows belonging to that committed batch are reverted.
func BuildBatchCompensateSQL(downSQL string, cfg contracts.BatchConfig) (string, error) {
	if err := ValidateUserSQL(downSQL); err != nil {
		return "", err
	}
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	quotedPK, err := QuoteIdentifier(cfg.PrimaryKeyColumn)
	if err != nil {
		return "", err
	}

	trimmed := strings.TrimSpace(downSQL)
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

	joinFilter := fmt.Sprintf(
		`br.execution_id = $1 AND br.batch_no = $2 AND %s::bigint = br.row_id AND (br.before_data IS NOT NULL)`,
		quotedPK,
	)

	whereIdx := findTopLevelWhereIndex(masked)
	if whereIdx >= 0 {
		prefix := strings.TrimSpace(trimmed[:whereIdx])
		condition := strings.TrimSpace(trimmed[whereIdx+len("where"):])
		if condition == "" {
			return "", ErrInvalidBatchUpdateSQL
		}
		return fmt.Sprintf(
			"%s FROM if3110_platform.batch_recovery_rows br WHERE (%s) AND %s",
			prefix,
			condition,
			joinFilter,
		), nil
	}

	return fmt.Sprintf(
		"%s FROM if3110_platform.batch_recovery_rows br WHERE %s",
		trimmed,
		joinFilter,
	), nil
}

// ExecuteBatchCompensation reverts committed batches of a PARTIAL or PARTIALLY_REVERTED
// batch execution in reverse batch order (batch N-1 down to 1).
//
// Safety invariants enforced:
//  1. Irreversible migrations (IsIrreversible == true) are rejected before touching data.
//  2. Before reverting each batch, every row in that batch is locked with FOR UPDATE
//     and compared against the stored post-image (after_data) in batch_recovery_rows.
//     If any row was modified or deleted externally after the batch committed,
//     compensation aborts immediately (BATCH-05) without overwriting newer data.
//  3. If compensation halts after reverting at least one batch, the job and gate
//     transition to PARTIALLY_REVERTED so a subsequent compensation resume continues
//     from last_compensated_batch - 1 without repeating already-reverted batches.
//  4. Once batch 1 is reverted, the PARTIAL/PARTIALLY_REVERTED gate is cleared and
//     active_head remains at its pre-batch position.
func (e *BatchExecutor) ExecuteBatchCompensation(ctx context.Context, env ExecutionEnv) (ExecutionResult, error) {
	if env.Conn == nil {
		return ExecutionResult{}, ErrNilTargetConn
	}
	plan := env.Claimed.Job.Plan
	if plan.IsIrreversible {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "IRREVERSIBLE_MIGRATION",
			ErrorMessage:  contracts.ErrIrreversibleMigration.Error(),
		}, nil
	}
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
			ErrorMessage:  fmt.Sprintf("batch compensation requires BATCH mode, got %s", plan.Mode),
		}, nil
	}

	originID := plan.OriginExecutionID
	if strings.TrimSpace(string(originID)) == "" {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "MISSING_ORIGIN_EXECUTION_ID",
			ErrorMessage:  "batch compensation requires a non-empty origin_execution_id",
		}, nil
	}

	scopedDownSQL, err := BuildBatchCompensateSQL(plan.DownSQL, plan.Batch)
	if err != nil {
		code := "INVALID_BATCH_DOWN_SQL"
		if errors.Is(err, ErrForbiddenManualTxControl) {
			code = "FORBIDDEN_TX_CONTROL"
		} else if errors.Is(err, ErrForbiddenInternalSchema) {
			code = "FORBIDDEN_INTERNAL_SCHEMA"
		}
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   plan.DownSQL,
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

	ckpt, found, err := ReadBatchCheckpoint(ctx, env.Conn, originID)
	if err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   plan.DownSQL,
			ErrorCode:     extractSQLState(err, "COMPENSATE_READ_CHECKPOINT_ERROR"),
			ErrorMessage:  SanitizeExecutionError(err),
		}, nil
	}
	if !found {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ExecutedSQL:   plan.DownSQL,
			ErrorCode:     "COMPENSATE_CHECKPOINT_NOT_FOUND",
			ErrorMessage:  ErrNoPartialBatchToResume.Error(),
		}, nil
	}

	// If 0 batches were committed before the execution halted, clear gate and finish.
	if ckpt.CommittedBatches == 0 {
		return e.finalizeBatchCompensation(ctx, env, originID, plan.DownSQL, 0, ckpt.TotalRows)
	}

	startBatchNo := ckpt.CommittedBatches
	if ckpt.LastCompensatedBatch > 0 && ckpt.LastCompensatedBatch <= ckpt.CommittedBatches {
		startBatchNo = ckpt.LastCompensatedBatch - 1
	}
	compensatedBatches := ckpt.CompensatedBatches

	for batchNo := startBatchNo; batchNo >= 1; batchNo-- {
		if ctx.Err() != nil || (e.shouldPause != nil && e.shouldPause(ctx, env.Claimed.Job.ID, batchNo)) {
			return e.haltOnCompensationBoundary(
				ctx,
				env,
				originID,
				scopedDownSQL,
				ckpt.CommittedBatches,
				compensatedBatches,
				ckpt.CommittedRows,
				ckpt.TotalRows,
				"COMPENSATION_PAUSED_AT_BOUNDARY",
				"batch compensation paused safely at committed batch boundary",
			)
		}

		revertedRows, status, errCode, errMsg := e.runSingleDownBatchTx(
			ctx,
			env,
			originID,
			batchNo,
			quotedSchema,
			quotedTable,
			quotedPK,
			scopedDownSQL,
			compensatedBatches+1,
		)
		_ = revertedRows
		if status != contracts.JobStatusSuccess {
			if status == contracts.JobStatusUnknown {
				if e.gates != nil {
					_ = e.gates.SetGate(ctx, env.Claimed.Job.DatabaseID, originID, env.Claimed.Job.ID, contracts.GateReasonUnknown, errMsg)
				}
				_ = SetTargetMigrationGate(ctx, env.Conn, originID, contracts.GateReasonUnknown)
				return ExecutionResult{
					JobStatus:        contracts.JobStatusUnknown,
					AttemptStatus:    contracts.AttemptStatusUnknown,
					ExecutedSQL:      scopedDownSQL,
					ErrorCode:        errCode,
					ErrorMessage:     errMsg,
					CommittedBatches: ckpt.CommittedBatches - compensatedBatches,
					CommittedRows:    ckpt.CommittedRows,
					TotalScopeRows:   ckpt.TotalRows,
				}, nil
			}
			return e.haltOnCompensationBoundary(
				ctx,
				env,
				originID,
				scopedDownSQL,
				ckpt.CommittedBatches,
				compensatedBatches,
				ckpt.CommittedRows,
				ckpt.TotalRows,
				errCode,
				errMsg,
			)
		}

		compensatedBatches++
	}

	return e.finalizeBatchCompensation(ctx, env, originID, plan.DownSQL, compensatedBatches, ckpt.TotalRows)
}

func (e *BatchExecutor) runSingleDownBatchTx(
	ctx context.Context,
	env ExecutionEnv,
	originID contracts.ExecutionID,
	batchNo int,
	quotedSchema string,
	quotedTable string,
	quotedPK string,
	scopedDownSQL string,
	nextCompensatedCount int,
) (revertedRows int64, batchStatus contracts.JobStatus, errCode string, errMsg string) {
	ctx, finishBatchSpan := startBatchSpan(ctx, batchNo, env.Claimed.Job.Plan.Batch.BatchSize, env.Claimed.Job.Direction)
	defer func() {
		finishBatchSpan(revertedRows, batchStatus, errCode, errMsg)
	}()

	tx, err := env.Conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, contracts.JobStatusPartiallyReverted, extractSQLState(err, "COMPENSATE_BEGIN_TX_ERROR"), SanitizeExecutionError(err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// Skip if this batch_no was already compensated.
	if proof, found, err := LookupCommitProof(ctx, tx, originID, CommitPhaseBatchDown, batchNo); err != nil {
		return 0, contracts.JobStatusPartiallyReverted, extractSQLState(err, "COMPENSATE_PROOF_LOOKUP_ERROR"), SanitizeExecutionError(err)
	} else if found {
		return proof.AffectedRows, contracts.JobStatusSuccess, "", ""
	}

	// 1. Lock the batch's target rows and verify that their current values still match after_data (BATCH-05).
	if err := verifyBatchPostImagesInTx(ctx, tx, originID, batchNo, quotedSchema, quotedTable, quotedPK); err != nil {
		rollbackTxWithTelemetry(ctx, tx, scopedDownSQL, err)
		code := extractSQLState(err, "COMPENSATE_VERIFY_ERROR")
		if errors.Is(err, ErrUnsafeBatchCompensation) {
			code = "UNSAFE_BATCH_COMPENSATION"
		}
		return 0, contracts.JobStatusFailed, code, SanitizeExecutionError(err)
	}

	// 2. Execute the scoped DownSQL to restore before_data for this batch.
	_, affected, err := execUserSQLWithTelemetry(ctx, tx, scopedDownSQL, string(originID), batchNo)
	if err != nil {
		rollbackTxWithTelemetry(ctx, tx, scopedDownSQL, err)
		return 0, contracts.JobStatusPartiallyReverted, extractSQLState(err, "COMPENSATE_DOWN_SQL_ERROR"), SanitizeExecutionError(err)
	}

	now := time.Now().UTC()
	if e != nil && e.now != nil {
		now = e.now().UTC()
	}

	// 3. Record BATCH_DOWN commit proof in execution_commits.
	const insertDownProofSQL = `
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
		insertDownProofSQL,
		string(originID),
		string(CommitPhaseBatchDown),
		batchNo,
		string(env.Claimed.Attempt.ID),
		scopedDownSQL,
		affected,
		strconv.Itoa(batchNo),
		string(env.Claimed.Job.ActorID),
		now,
	); err != nil {
		_ = tx.Rollback()
		return 0, contracts.JobStatusPartiallyReverted, extractSQLState(err, "COMPENSATE_INSERT_PROOF_ERROR"), SanitizeExecutionError(err)
	}

	// 4. Update compensated_batches and last_compensated_batch in batch_checkpoints.
	const updateCheckpointDownSQL = `
		UPDATE if3110_platform.batch_checkpoints
		SET phase = $1,
		    compensated_batches = $2,
		    last_compensated_batch = $3,
		    updated_at = $4
		WHERE execution_id = $5
	`
	if _, err := tx.ExecContext(
		ctx,
		updateCheckpointDownSQL,
		string(CommitPhaseBatchDown),
		nextCompensatedCount,
		batchNo,
		now,
		string(originID),
	); err != nil {
		_ = tx.Rollback()
		return 0, contracts.JobStatusPartiallyReverted, extractSQLState(err, "COMPENSATE_UPDATE_CHECKPOINT_ERROR"), SanitizeExecutionError(err)
	}

	// 5. Commit this reverse batch transaction!
	if err := commitTxWithTelemetry(ctx, tx, scopedDownSQL, affected); err != nil {
		if ClassifyCommitError(err) == ErrorCategoryUnknownCommit {
			return 0, contracts.JobStatusUnknown, extractSQLState(err, "COMPENSATE_COMMIT_UNKNOWN"), SanitizeExecutionError(err)
		}
		return 0, contracts.JobStatusPartiallyReverted, extractSQLState(err, "COMPENSATE_COMMIT_ERROR"), SanitizeExecutionError(err)
	}

	return affected, contracts.JobStatusSuccess, "", ""
}

func verifyBatchPostImagesInTx(
	ctx context.Context,
	tx *sql.Tx,
	originID contracts.ExecutionID,
	batchNo int,
	quotedSchema string,
	quotedTable string,
	quotedPK string,
) error {
	verifySQL := fmt.Sprintf(
		`SELECT
			r.row_id,
			r.before_data,
			r.after_data,
			to_jsonb(t.*) AS current_data
		 FROM if3110_platform.batch_recovery_rows r
		 LEFT JOIN %s.%s t ON t.%s::bigint = r.row_id
		 WHERE r.execution_id = $1
		   AND r.batch_no = $2
		 ORDER BY r.row_id ASC
		 FOR UPDATE OF t`,
		quotedSchema,
		quotedTable,
		quotedPK,
	)

	rows, err := tx.QueryContext(ctx, verifySQL, string(originID), batchNo)
	if err != nil {
		return err
	}
	defer func() {
		_ = rows.Close()
	}()

	for rows.Next() {
		var (
			rowID       int64
			beforeData  []byte
			afterData   []byte
			currentData []byte
		)
		if err := rows.Scan(&rowID, &beforeData, &afterData, &currentData); err != nil {
			return err
		}
		if len(currentData) == 0 || !jsonValuesEqual(afterData, currentData) {
			return fmt.Errorf("%w (batch %d, row_id %d)", ErrUnsafeBatchCompensation, batchNo, rowID)
		}
	}
	return rows.Err()
}

func jsonValuesEqual(a, b []byte) bool {
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

func (e *BatchExecutor) haltOnCompensationBoundary(
	ctx context.Context,
	env ExecutionEnv,
	originID contracts.ExecutionID,
	executedSQL string,
	totalCommittedBatches int,
	compensatedBatches int,
	committedRows int64,
	totalScopeRows int64,
	errCode string,
	errMsg string,
) (ExecutionResult, error) {
	baseCtx := context.Background()
	if ctx != nil {
		baseCtx = context.WithoutCancel(ctx)
	}

	gateReason := contracts.GateReasonPartial
	jobStatus := contracts.JobStatusPartial
	if compensatedBatches > 0 {
		gateReason = contracts.GateReasonPartiallyReverted
		jobStatus = contracts.JobStatusPartiallyReverted
	}
	if errCode == "UNSAFE_BATCH_COMPENSATION" && compensatedBatches == 0 {
		jobStatus = contracts.JobStatusFailed
	}

	if e.gates != nil {
		_ = e.gates.SetGate(
			baseCtx,
			env.Claimed.Job.DatabaseID,
			originID,
			env.Claimed.Job.ID,
			gateReason,
			errMsg,
		)
	}
	_ = SetTargetMigrationGate(baseCtx, env.Conn, originID, gateReason)

	remainingBatches := totalCommittedBatches - compensatedBatches
	if remainingBatches < 0 {
		remainingBatches = 0
	}

	return ExecutionResult{
		JobStatus:        jobStatus,
		AttemptStatus:    contracts.AttemptStatusFailed,
		ExecutedSQL:      executedSQL,
		ErrorCode:        errCode,
		ErrorMessage:     errMsg,
		CommittedBatches: remainingBatches,
		CommittedRows:    committedRows,
		TotalScopeRows:   totalScopeRows,
	}, nil
}

func (e *BatchExecutor) finalizeBatchCompensation(
	ctx context.Context,
	env ExecutionEnv,
	originID contracts.ExecutionID,
	downSQL string,
	compensatedBatches int,
	totalScopeRows int64,
) (ExecutionResult, error) {
	tx, err := env.Conn.BeginTx(ctx, nil)
	if err != nil {
		return e.haltOnCompensationBoundary(
			ctx,
			env,
			originID,
			downSQL,
			compensatedBatches,
			compensatedBatches,
			0,
			totalScopeRows,
			extractSQLState(err, "COMPENSATE_FINALIZE_BEGIN_ERROR"),
			SanitizeExecutionError(err),
		)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	currentHead, err := lockAndReadMigrationState(ctx, tx)
	if err != nil {
		return e.haltOnCompensationBoundary(
			ctx,
			env,
			originID,
			downSQL,
			compensatedBatches,
			compensatedBatches,
			0,
			totalScopeRows,
			extractSQLState(err, "COMPENSATE_FINALIZE_STATE_ERROR"),
			SanitizeExecutionError(err),
		)
	}

	now := time.Now().UTC()
	if e != nil && e.now != nil {
		now = e.now().UTC()
	}

	const clearGateStateSQL = `
		UPDATE if3110_platform.database_migration_state
		SET unresolved_execution_id = '',
		    unresolved_reason = ''
		WHERE singleton_id = 1
	`
	if _, err := tx.ExecContext(ctx, clearGateStateSQL); err != nil {
		return e.haltOnCompensationBoundary(
			ctx,
			env,
			originID,
			downSQL,
			compensatedBatches,
			compensatedBatches,
			0,
			totalScopeRows,
			extractSQLState(err, "COMPENSATE_CLEAR_GATE_ERROR"),
			SanitizeExecutionError(err),
		)
	}

	const updateCheckpointDoneSQL = `
		UPDATE if3110_platform.batch_checkpoints
		SET phase = $1,
		    updated_at = $2
		WHERE execution_id = $3
	`
	if _, err := tx.ExecContext(ctx, updateCheckpointDoneSQL, string(CommitPhaseFinalize), now, string(originID)); err != nil {
		return e.haltOnCompensationBoundary(
			ctx,
			env,
			originID,
			downSQL,
			compensatedBatches,
			compensatedBatches,
			0,
			totalScopeRows,
			extractSQLState(err, "COMPENSATE_CHECKPOINT_FINALIZE_ERROR"),
			SanitizeExecutionError(err),
		)
	}

	if err := tx.Commit(); err != nil {
		return e.haltOnCompensationBoundary(
			ctx,
			env,
			originID,
			downSQL,
			compensatedBatches,
			compensatedBatches,
			0,
			totalScopeRows,
			extractSQLState(err, "COMPENSATE_FINALIZE_COMMIT_ERROR"),
			SanitizeExecutionError(err),
		)
	}

	_ = currentHead
	if e.gates != nil {
		_ = e.gates.ClearGate(ctx, env.Claimed.Job.DatabaseID)
	}

	return ExecutionResult{
		JobStatus:        contracts.JobStatusSuccess,
		AttemptStatus:    contracts.AttemptStatusCommitted,
		ExecutedSQL:      downSQL,
		CommittedBatches: 0,
		CommittedRows:    0,
		TotalScopeRows:   totalScopeRows,
	}, nil
}
