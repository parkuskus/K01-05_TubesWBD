package execution

import (
	"context"

	"if3110.local/db-platform/internal/contracts"
)

// ModeDispatchExecutor routes a plan to the executor for its declared mode.
type ModeDispatchExecutor struct {
	normal PlanExecutor
	batch  PlanExecutor
}

func NewModeDispatchExecutor(normal, batch PlanExecutor) *ModeDispatchExecutor {
	return &ModeDispatchExecutor{normal: normal, batch: batch}
}

func (e *ModeDispatchExecutor) Execute(ctx context.Context, env ExecutionEnv) (ExecutionResult, error) {
	var executor PlanExecutor
	switch env.Claimed.Job.Plan.Mode {
	case contracts.ModeNormal:
		executor = e.normal
	case contracts.ModeBatch:
		executor = e.batch
	default:
		return ExecutionResult{JobStatus: contracts.JobStatusFailed, AttemptStatus: contracts.AttemptStatusRolledBack, ErrorCode: "INVALID_MODE", ErrorMessage: contracts.ErrInvalidExecutionPlan.Error()}, nil
	}
	if executor == nil {
		return ExecutionResult{JobStatus: contracts.JobStatusFailed, AttemptStatus: contracts.AttemptStatusRolledBack, ErrorCode: "EXECUTOR_UNAVAILABLE", ErrorMessage: "plan executor is not configured"}, nil
	}
	return executor.Execute(ctx, env)
}
