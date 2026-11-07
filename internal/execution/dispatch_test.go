package execution

import (
	"context"
	"testing"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/jobs"
)

func TestModeDispatchExecutorSelectsExecutorByPlanMode(t *testing.T) {
	called := contracts.ExecutionMode("")
	dispatch := NewModeDispatchExecutor(
		PlanExecutorFunc(func(_ context.Context, env ExecutionEnv) (ExecutionResult, error) {
			called = env.Claimed.Job.Plan.Mode
			return ExecutionResult{JobStatus: contracts.JobStatusSuccess}, nil
		}),
		PlanExecutorFunc(func(_ context.Context, env ExecutionEnv) (ExecutionResult, error) {
			called = env.Claimed.Job.Plan.Mode
			return ExecutionResult{JobStatus: contracts.JobStatusSuccess}, nil
		}),
	)
	for _, mode := range []contracts.ExecutionMode{contracts.ModeNormal, contracts.ModeBatch} {
		called = ""
		_, err := dispatch.Execute(context.Background(), ExecutionEnv{Claimed: jobs.ClaimedJob{Job: jobs.Job{Plan: contracts.ExecutionPlan{Mode: mode}}}})
		if err != nil || called != mode {
			t.Fatalf("mode %s dispatched as %s: %v", mode, called, err)
		}
	}
}

func TestModeDispatchExecutorFailsClosedForUnsupportedMode(t *testing.T) {
	dispatch := NewModeDispatchExecutor(nil, nil)
	result, err := dispatch.Execute(context.Background(), ExecutionEnv{Claimed: jobs.ClaimedJob{Job: jobs.Job{Plan: contracts.ExecutionPlan{Mode: "unknown"}}}})
	if err != nil || result.JobStatus != contracts.JobStatusFailed || result.ErrorCode != "INVALID_MODE" {
		t.Fatalf("unsupported mode result=%+v err=%v", result, err)
	}
}
