package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"if3110.local/db-platform/internal/contracts"
)

var (
	ErrNoPartialBatchToResume = errors.New("no partial batch execution checkpoint found for origin execution")
	ErrBatchResumeMismatch    = errors.New("target unresolved execution gate does not match origin execution for resume")
)

// BatchControlRegistry coordinates cooperative pause/cancel signals at safe
// committed batch boundaries for in-flight batch jobs.
type BatchControlRegistry struct {
	mu     sync.Mutex
	paused map[contracts.JobID]bool
}

// NewBatchControlRegistry creates an empty BatchControlRegistry.
func NewBatchControlRegistry() *BatchControlRegistry {
	return &BatchControlRegistry{
		paused: make(map[contracts.JobID]bool),
	}
}

// RequestPause marks jobID to halt cleanly at the next committed batch boundary.
func (r *BatchControlRegistry) RequestPause(jobID contracts.JobID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused[jobID] = true
}

// ClearPause removes any pending pause signal for jobID.
func (r *BatchControlRegistry) ClearPause(jobID contracts.JobID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.paused, jobID)
}

// ShouldPause implements BatchPauseChecker.
func (r *BatchControlRegistry) ShouldPause(_ context.Context, jobID contracts.JobID, _ int) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused[jobID]
}

// Execute implements PlanExecutor for Mode == ModeBatch plans, dispatching
// APPLY/FORWARD to ExecuteBatchUp and RESUME to ExecuteBatchResume.
func (e *BatchExecutor) Execute(ctx context.Context, env ExecutionEnv) (ExecutionResult, error) {
	if env.Conn == nil {
		return ExecutionResult{}, ErrNilTargetConn
	}
	plan := env.Claimed.Job.Plan
	if err := plan.Validate(); err != nil {
		code := "INVALID_PLAN"
		if errors.Is(err, contracts.ErrIrreversibleMigration) {
			code = "IRREVERSIBLE_MIGRATION"
		}
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     code,
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

	switch plan.Direction {
	case contracts.DirectionApply, contracts.DirectionForward:
		return e.ExecuteBatchUp(ctx, env)
	case contracts.DirectionResume:
		return e.ExecuteBatchResume(ctx, env)
	case contracts.DirectionCompensate, contracts.DirectionRollback:
		return e.ExecuteBatchCompensation(ctx, env)
	default:
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "UNSUPPORTED_BATCH_DIRECTION",
			ErrorMessage:  fmt.Sprintf("unsupported batch direction %s", plan.Direction),
		}, nil
	}
}

// ExecuteBatchResume resumes an interrupted PARTIAL batch execution from its
// last committed checkpoint cursor (plan.OriginExecutionID) without repeating
// already-committed batches 1..N-1.
func (e *BatchExecutor) ExecuteBatchResume(ctx context.Context, env ExecutionEnv) (ExecutionResult, error) {
	if env.Conn == nil {
		return ExecutionResult{}, ErrNilTargetConn
	}
	plan := env.Claimed.Job.Plan
	if err := plan.Validate(); err != nil {
		code := "INVALID_PLAN"
		if errors.Is(err, contracts.ErrIrreversibleMigration) {
			code = "IRREVERSIBLE_MIGRATION"
		}
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     code,
			ErrorMessage:  err.Error(),
		}, nil
	}
	if plan.Mode != contracts.ModeBatch || plan.Direction != contracts.DirectionResume {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "INVALID_RESUME_DIRECTION",
			ErrorMessage:  "batch resume requires Mode=BATCH and Direction=RESUME",
		}, nil
	}

	originID := plan.OriginExecutionID
	if strings.TrimSpace(string(originID)) == "" {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "MISSING_ORIGIN_EXECUTION_ID",
			ErrorMessage:  "batch resume requires a non-empty origin_execution_id",
		}, nil
	}

	// Verify target migration state and existing checkpoint for originID before resuming.
	head, err := ReadActiveHead(ctx, env.Conn)
	if err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     extractSQLState(err, "RESUME_READ_HEAD_ERROR"),
			ErrorMessage:  SanitizeExecutionError(err),
		}, nil
	}
	if head.UnresolvedReason != "" {
		if head.UnresolvedReason != contracts.GateReasonPartial {
			return ExecutionResult{
				JobStatus:     contracts.JobStatusBlocked,
				AttemptStatus: contracts.AttemptStatusRolledBack,
				ErrorCode:     "EXECUTION_GATE_BLOCKED",
				ErrorMessage:  fmt.Sprintf("cannot resume forward batch while gate is %s", head.UnresolvedReason),
			}, nil
		}
		if head.UnresolvedExecutionID != "" && head.UnresolvedExecutionID != originID {
			return ExecutionResult{
				JobStatus:     contracts.JobStatusFailed,
				AttemptStatus: contracts.AttemptStatusRolledBack,
				ErrorCode:     "RESUME_ORIGIN_MISMATCH",
				ErrorMessage:  ErrBatchResumeMismatch.Error(),
			}, nil
		}
	}

	ckpt, found, err := ReadBatchCheckpoint(ctx, env.Conn, originID)
	if err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     extractSQLState(err, "RESUME_READ_CHECKPOINT_ERROR"),
			ErrorMessage:  SanitizeExecutionError(err),
		}, nil
	}
	if !found {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "RESUME_CHECKPOINT_NOT_FOUND",
			ErrorMessage:  ErrNoPartialBatchToResume.Error(),
		}, nil
	}

	// Delegate to ExecuteBatchUp, which reads the existing frozen scope and
	// checkpoint for originID and continues from WHERE row_id > ckpt.LastCursor
	// (starting at batch_no = ckpt.CommittedBatches + 1).
	res, execErr := e.ExecuteBatchUp(ctx, env)
	if res.CommittedBatches < ckpt.CommittedBatches {
		res.CommittedBatches = ckpt.CommittedBatches
	}
	return res, execErr
}

