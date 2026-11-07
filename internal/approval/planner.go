package approval

import (
	"encoding/json"
	"fmt"
	"strings"

	"if3110.local/db-platform/internal/contracts"
)

// BuildExecutionPlan seals an approved request against the current target evidence.
func BuildExecutionPlan(request Request, history []contracts.CommitHistoryEvent, head contracts.ActiveHeadState, executionID contracts.ExecutionID) (contracts.ExecutionPlan, error) {
	if request.Status != "APPROVED" && request.Status != "QUEUED" || len(request.Requirements) == 0 || strings.TrimSpace(string(executionID)) == "" {
		return contracts.ExecutionPlan{}, contracts.ErrStaleApproval
	}
	for _, requirement := range request.Requirements {
		if requirement.Vote == nil || requirement.Vote.Decision != "APPROVE" || requirement.Vote.ActorID != requirement.LeaderUserID {
			return contracts.ExecutionPlan{}, contracts.ErrStaleApproval
		}
	}

	plan := contracts.ExecutionPlan{
		RequestID: request.ID, ExecutionID: executionID,
		MigrationID: contracts.MigrationID(request.Migration.ID), RevisionID: contracts.RevisionID(fmt.Sprintf("%s:%d", request.Migration.ID, request.Revision)),
		Version: request.Migration.Version, Direction: request.Direction, Mode: contracts.ExecutionMode(request.Migration.Mode),
		ActorID: request.CreatedBy, TargetDatabaseID: request.DatabaseID,
		TargetConnectionRevision: request.ConnectionRevision, ExpectedActiveHead: request.ExpectedHead,
		PolicyVersion: request.PolicyVersion, UpSQL: request.Migration.UpSQL, DownSQL: request.Migration.DownSQL,
		IsIrreversible: request.Migration.Irreversible, IrreversibleReason: request.Migration.Reason,
	}
	if request.Migration.Checksum == "" || request.Checksum == "" || request.Migration.Revision != request.Revision || request.Migration.DatabaseID != request.DatabaseID {
		return contracts.ExecutionPlan{}, contracts.ErrStaleApproval
	}
	if head.ExecutionID != request.ExpectedHead {
		return contracts.ExecutionPlan{}, contracts.ErrHeadMismatch
	}

	switch request.Direction {
	case contracts.DirectionApply:
		if request.Migration.Version != head.Version+1 || head.UnresolvedReason != "" {
			return contracts.ExecutionPlan{}, contracts.ErrHeadMismatch
		}
	case contracts.DirectionRollback:
		if request.Migration.ID != string(head.MigrationID) || request.Migration.Version != head.Version || head.UnresolvedReason != "" {
			return contracts.ExecutionPlan{}, contracts.ErrHeadMismatch
		}
		if request.Migration.Irreversible {
			return contracts.ExecutionPlan{}, contracts.ErrIrreversibleMigration
		}
		plan.OriginExecutionID = head.ExecutionID
	case contracts.DirectionForward:
		origin, rolledBack := rollbackOrigin(history, request.Migration.ID)
		if request.Migration.Version != head.Version+1 || head.UnresolvedReason != "" || !rolledBack || request.OriginExecutionID != origin {
			return contracts.ExecutionPlan{}, contracts.ErrHeadMismatch
		}
		plan.OriginExecutionID = origin
	case contracts.DirectionResume, contracts.DirectionCompensate:
		if head.UnresolvedExecutionID == "" || request.OriginExecutionID != head.UnresolvedExecutionID || head.UnresolvedReason != contracts.GateReasonPartial && head.UnresolvedReason != contracts.GateReasonPartiallyReverted {
			return contracts.ExecutionPlan{}, contracts.ErrExecutionGateBlocked
		}
		plan.OriginExecutionID = head.UnresolvedExecutionID
	default:
		return contracts.ExecutionPlan{}, contracts.ErrInvalidExecutionPlan
	}
	if plan.Mode == contracts.ModeBatch {
		if err := json.Unmarshal(request.Migration.BatchPlan, &plan.Batch); err != nil {
			return contracts.ExecutionPlan{}, contracts.ErrInvalidExecutionPlan
		}
	}
	plan.PlanChecksum = plan.ComputeChecksum()
	if err := plan.Validate(); err != nil {
		return contracts.ExecutionPlan{}, err
	}
	return plan, nil
}

func rollbackOrigin(history []contracts.CommitHistoryEvent, migrationID string) (contracts.ExecutionID, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		event := history[i]
		if event.MigrationID == contracts.MigrationID(migrationID) && event.Direction == contracts.DirectionRollback && event.RevertsExecutionID != "" {
			return event.RevertsExecutionID, true
		}
	}
	return "", false
}
