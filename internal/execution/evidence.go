package execution

import (
	"context"
	"errors"
	"fmt"

	"if3110.local/db-platform/internal/contracts"
)

// TargetCommitEvidenceReader reads target metadata without initializing or mutating it.
type TargetCommitEvidenceReader struct {
	provider contracts.DatabaseProvider
}

func NewTargetCommitEvidenceReader(provider contracts.DatabaseProvider) (*TargetCommitEvidenceReader, error) {
	if provider == nil {
		return nil, errors.New("target evidence requires a database provider")
	}
	return &TargetCommitEvidenceReader{provider: provider}, nil
}

func (r *TargetCommitEvidenceReader) ActiveHead(ctx context.Context, databaseID contracts.DatabaseID) (contracts.ActiveHeadState, error) {
	pool, err := r.provider.Pool(ctx, databaseID)
	if err != nil {
		return contracts.ActiveHeadState{}, err
	}
	return ReadActiveHead(ctx, pool)
}

func (r *TargetCommitEvidenceReader) ListCommitHistory(ctx context.Context, databaseID contracts.DatabaseID) ([]contracts.CommitHistoryEvent, error) {
	pool, err := r.provider.Pool(ctx, databaseID)
	if err != nil {
		return nil, err
	}
	rows, err := pool.QueryContext(ctx, `SELECT id,execution_id,job_id,request_id,migration_id,revision_id,version,direction,mode,executed_sql,plan_checksum,actor_id,head_before,head_after,reverts_execution_id,committed_at FROM if3110_platform.migration_history_v2 ORDER BY id`)
	if err != nil {
		if IsMissingTargetSchemaError(err) {
			return []contracts.CommitHistoryEvent{}, nil
		}
		return nil, fmt.Errorf("read target commit history: %w", err)
	}
	defer rows.Close()
	events := make([]contracts.CommitHistoryEvent, 0)
	for rows.Next() {
		var event contracts.CommitHistoryEvent
		var executionID, jobID, requestID, migrationID, revisionID, direction, mode, actorID, headBefore, headAfter, reverts string
		if err := rows.Scan(&event.ID, &executionID, &jobID, &requestID, &migrationID, &revisionID, &event.Version, &direction, &mode, &event.ExecutedSQL, &event.PlanChecksum, &actorID, &headBefore, &headAfter, &reverts, &event.CommittedAt); err != nil {
			return nil, fmt.Errorf("scan target commit history: %w", err)
		}
		event.ExecutionID = contracts.ExecutionID(executionID)
		event.JobID = contracts.JobID(jobID)
		event.RequestID = contracts.OperationRequestID(requestID)
		event.MigrationID = contracts.MigrationID(migrationID)
		event.RevisionID = contracts.RevisionID(revisionID)
		event.Direction = contracts.OperationDirection(direction)
		event.Mode = contracts.ExecutionMode(mode)
		event.ActorID = contracts.UserID(actorID)
		event.HeadBefore = contracts.ExecutionID(headBefore)
		event.HeadAfter = contracts.ExecutionID(headAfter)
		event.RevertsExecutionID = contracts.ExecutionID(reverts)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read target commit history: %w", err)
	}
	return events, nil
}

func (r *TargetCommitEvidenceReader) BatchProgress(ctx context.Context, databaseID contracts.DatabaseID, executionID contracts.ExecutionID) (contracts.BatchCheckpointState, error) {
	pool, err := r.provider.Pool(ctx, databaseID)
	if err != nil {
		return contracts.BatchCheckpointState{}, err
	}
	return ReadBatchProgress(ctx, pool, executionID)
}

var _ contracts.CommitEvidenceReader = (*TargetCommitEvidenceReader)(nil)
