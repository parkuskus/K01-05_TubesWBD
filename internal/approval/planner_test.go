package approval

import (
	"errors"
	"testing"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/definition"
)

func TestBuildExecutionPlanRequiresEveryApprovalAndUsesOriginalSQL(t *testing.T) {
	request := validPlanRequest()
	history := []contracts.CommitHistoryEvent{{ExecutionID: "exec-1", MigrationID: "10", Version: 1}}
	head := contracts.ActiveHeadState{MigrationID: "10", ExecutionID: "exec-1", Version: 1}

	plan, err := BuildExecutionPlan(request, history, head, "exec-2")
	if err != nil {
		t.Fatalf("BuildExecutionPlan: %v", err)
	}
	if plan.UpSQL != request.Migration.UpSQL || plan.DownSQL != request.Migration.DownSQL || plan.ExpectedActiveHead != "exec-1" || plan.ExecutionID != "exec-2" || plan.PlanChecksum != plan.ComputeChecksum() {
		t.Fatalf("plan did not preserve approved snapshot: %+v", plan)
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("invalid plan: %v", err)
	}

	request.Requirements[1].Vote = nil
	if _, err := BuildExecutionPlan(request, history, head, "exec-3"); !errors.Is(err, contracts.ErrStaleApproval) {
		t.Fatalf("plan without every group vote = %v, want stale approval", err)
	}
}

func validPlanRequest() Request {
	return Request{
		ID: "7", DatabaseID: "3", MigrationID: "11", Revision: 2,
		Direction: contracts.DirectionApply, ExpectedHead: "exec-1",
		ConnectionRevision: 4, PolicyVersion: 9, Status: "APPROVED",
		CreatedBy: "5", Checksum: "request-checksum",
		Migration: definition.Definition{
			ID: "11", DatabaseID: "3", Version: 2, Revision: 2,
			Input: definition.Input{
				UpSQL: "  ALTER TABLE t ADD COLUMN x int;  ", DownSQL: "ALTER TABLE t DROP COLUMN x", Mode: "NORMAL",
			}, Checksum: "revision-checksum",
		},
		Requirements: []Requirement{
			{GroupID: "1", LeaderUserID: "8", Vote: &Vote{ActorID: "8", Decision: "APPROVE"}},
			{GroupID: "2", LeaderUserID: "9", Vote: &Vote{ActorID: "9", Decision: "APPROVE"}},
		},
	}
}

func TestBuildExecutionPlanConstrainsDirectionToActiveHistory(t *testing.T) {
	request := validPlanRequest()
	head := contracts.ActiveHeadState{MigrationID: "11", ExecutionID: "exec-11", Version: 2}
	history := []contracts.CommitHistoryEvent{{ExecutionID: "exec-11", MigrationID: "11", Direction: contracts.DirectionApply}}
	t.Run("rollback only active reversible head", func(t *testing.T) {
		rollback := request
		rollback.Direction = contracts.DirectionRollback
		rollback.ExpectedHead = "exec-11"
		plan, err := BuildExecutionPlan(rollback, history, head, "exec-12")
		if err != nil || plan.OriginExecutionID != "exec-11" {
			t.Fatalf("rollback plan = %+v, %v", plan, err)
		}
		blockedHead := head
		blockedHead.UnresolvedReason = contracts.GateReasonPartial
		blockedHead.UnresolvedExecutionID = "exec-partial"
		if _, err := BuildExecutionPlan(rollback, history, blockedHead, "exec-12"); !errors.Is(err, contracts.ErrHeadMismatch) {
			t.Fatalf("rollback through unresolved gate = %v", err)
		}
		rollback.Migration.Irreversible = true
		rollback.Migration.DownSQL = ""
		rollback.Migration.Reason = "irreversible"
		if _, err := BuildExecutionPlan(rollback, history, head, "exec-12"); !errors.Is(err, contracts.ErrIrreversibleMigration) {
			t.Fatalf("irreversible rollback = %v", err)
		}
	})
	t.Run("forward follows rollback and preserves order", func(t *testing.T) {
		forward := request
		forward.Direction = contracts.DirectionForward
		forward.ExpectedHead = "exec-11"
		forward.OriginExecutionID = "exec-11"
		forward.Migration.Version = 3
		if _, err := BuildExecutionPlan(forward, history, head, "exec-12"); !errors.Is(err, contracts.ErrHeadMismatch) {
			t.Fatalf("forward without rollback evidence = %v", err)
		}
		history = append(history, contracts.CommitHistoryEvent{ExecutionID: "exec-12", MigrationID: "11", Direction: contracts.DirectionRollback, RevertsExecutionID: "exec-11"})
		if _, err := BuildExecutionPlan(forward, history, head, "exec-13"); err != nil {
			t.Fatalf("forward after rollback = %v", err)
		}
		forward.Migration.Version = 4
		if _, err := BuildExecutionPlan(forward, history, head, "exec-13"); !errors.Is(err, contracts.ErrHeadMismatch) {
			t.Fatalf("forward skipped chain order = %v", err)
		}
	})
	t.Run("recovery requires a supported unresolved gate", func(t *testing.T) {
		resume := request
		resume.Direction = contracts.DirectionResume
		resume.ExpectedHead = "exec-11"
		resume.OriginExecutionID = "exec-partial"
		if _, err := BuildExecutionPlan(resume, history, head, "exec-12"); !errors.Is(err, contracts.ErrExecutionGateBlocked) {
			t.Fatalf("resume without gate = %v", err)
		}
		head.UnresolvedExecutionID = "exec-partial"
		head.UnresolvedReason = contracts.GateReasonPartial
		plan, err := BuildExecutionPlan(resume, history, head, "exec-12")
		if err != nil || plan.OriginExecutionID != "exec-partial" {
			t.Fatalf("resume plan = %+v, %v", plan, err)
		}
	})
}
