package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"if3110.local/db-platform/internal/contracts"
)

var (
	ErrNilDatabaseProvider = errors.New("target database provider is required")
	ErrEmptyDatabaseID     = errors.New("target database_id is required")
)

type sqlQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// TargetEvidenceReader implements contracts.CommitEvidenceReader using strictly
// read-only queries against if3110_platform tables on a target database.
//
// It never executes CREATE SCHEMA or CREATE TABLE DDL, ensuring View-Only users
// and uninitialized target databases can be inspected safely.
type TargetEvidenceReader struct {
	dbProvider contracts.DatabaseProvider
}

var _ contracts.CommitEvidenceReader = (*TargetEvidenceReader)(nil)

// NewTargetEvidenceReader creates a TargetEvidenceReader backed by dbProvider.
func NewTargetEvidenceReader(dbProvider contracts.DatabaseProvider) *TargetEvidenceReader {
	return &TargetEvidenceReader{
		dbProvider: dbProvider,
	}
}

// ActiveHead reads the current active migration head and any unresolved execution
// gate from if3110_platform.database_migration_state on the target database.
// If the target database has not yet initialized if3110_platform tables, it returns
// a zero ActiveHeadState and nil error without running DDL.
func (r *TargetEvidenceReader) ActiveHead(ctx context.Context, databaseID contracts.DatabaseID) (contracts.ActiveHeadState, error) {
	if r == nil || r.dbProvider == nil {
		return contracts.ActiveHeadState{}, ErrNilDatabaseProvider
	}
	if strings.TrimSpace(string(databaseID)) == "" {
		return contracts.ActiveHeadState{}, ErrEmptyDatabaseID
	}
	pool, err := r.dbProvider.Pool(ctx, databaseID)
	if err != nil {
		return contracts.ActiveHeadState{}, err
	}
	return ReadActiveHead(ctx, pool)
}

// ListCommitHistory reads all committed migration events from
// if3110_platform.migration_history_v2 ordered chronologically (id ASC) so callers
// can project linear history and apply hide-rollback filtering before pagination.
// If the schema or table does not exist yet (42P01 / 3F000), it returns an empty
// slice and nil error without executing DDL.
func (r *TargetEvidenceReader) ListCommitHistory(ctx context.Context, databaseID contracts.DatabaseID) ([]contracts.CommitHistoryEvent, error) {
	if r == nil || r.dbProvider == nil {
		return nil, ErrNilDatabaseProvider
	}
	if strings.TrimSpace(string(databaseID)) == "" {
		return nil, ErrEmptyDatabaseID
	}
	pool, err := r.dbProvider.Pool(ctx, databaseID)
	if err != nil {
		return nil, err
	}
	return ReadCommitHistory(ctx, pool)
}

// BatchProgress reads the durable batch checkpoint state for executionID on the
// target database without executing DDL. If no checkpoint or schema exists yet,
// it returns a zero BatchCheckpointState with ExecutionID populated and nil error.
func (r *TargetEvidenceReader) BatchProgress(
	ctx context.Context,
	databaseID contracts.DatabaseID,
	executionID contracts.ExecutionID,
) (contracts.BatchCheckpointState, error) {
	if r == nil || r.dbProvider == nil {
		return contracts.BatchCheckpointState{}, ErrNilDatabaseProvider
	}
	if strings.TrimSpace(string(databaseID)) == "" {
		return contracts.BatchCheckpointState{}, ErrEmptyDatabaseID
	}
	pool, err := r.dbProvider.Pool(ctx, databaseID)
	if err != nil {
		return contracts.BatchCheckpointState{}, err
	}
	return ReadBatchProgress(ctx, pool, executionID)
}

// ReadCommitHistory queries if3110_platform.migration_history_v2 on any *sql.DB,
// *sql.Conn, or *sql.Tx without running DDL.
func ReadCommitHistory(ctx context.Context, q sqlQueryer) ([]contracts.CommitHistoryEvent, error) {
	if q == nil {
		return nil, ErrNilTargetConn
	}
	const query = `
		SELECT
			id,
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
		FROM if3110_platform.migration_history_v2
		ORDER BY id ASC
	`
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		if IsMissingTargetSchemaError(err) {
			return []contracts.CommitHistoryEvent{}, nil
		}
		return nil, fmt.Errorf("query target migration_history_v2: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	events := make([]contracts.CommitHistoryEvent, 0)
	for rows.Next() {
		var (
			ev          contracts.CommitHistoryEvent
			execID      string
			jobID       string
			reqID       string
			migID       string
			revID       string
			dir         string
			mode        string
			actorID     string
			headBefore  string
			headAfter   string
			revertsExec string
		)
		if err := rows.Scan(
			&ev.ID,
			&execID,
			&jobID,
			&reqID,
			&migID,
			&revID,
			&ev.Version,
			&dir,
			&mode,
			&ev.ExecutedSQL,
			&ev.PlanChecksum,
			&actorID,
			&headBefore,
			&headAfter,
			&revertsExec,
			&ev.CommittedAt,
		); err != nil {
			return nil, fmt.Errorf("scan migration_history_v2 row: %w", err)
		}
		ev.ExecutionID = contracts.ExecutionID(execID)
		ev.JobID = contracts.JobID(jobID)
		ev.RequestID = contracts.OperationRequestID(reqID)
		ev.MigrationID = contracts.MigrationID(migID)
		ev.RevisionID = contracts.RevisionID(revID)
		ev.Direction = contracts.OperationDirection(dir)
		ev.Mode = contracts.ExecutionMode(mode)
		ev.ActorID = contracts.UserID(actorID)
		ev.HeadBefore = contracts.ExecutionID(headBefore)
		ev.HeadAfter = contracts.ExecutionID(headAfter)
		ev.RevertsExecutionID = contracts.ExecutionID(revertsExec)
		ev.CommittedAt = ev.CommittedAt.UTC()
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		if IsMissingTargetSchemaError(err) {
			return []contracts.CommitHistoryEvent{}, nil
		}
		return nil, fmt.Errorf("iterate migration_history_v2 rows: %w", err)
	}
	return events, nil
}

// ReadBatchProgress queries if3110_platform.batch_checkpoints and batch_scopes
// on any *sql.DB, *sql.Conn, or *sql.Tx without running DDL.
func ReadBatchProgress(
	ctx context.Context,
	q sqlRowQueryer,
	executionID contracts.ExecutionID,
) (contracts.BatchCheckpointState, error) {
	if q == nil {
		return contracts.BatchCheckpointState{}, ErrNilTargetConn
	}
	if strings.TrimSpace(string(executionID)) == "" {
		return contracts.BatchCheckpointState{}, nil
	}
	ckpt, found, err := ReadBatchCheckpoint(ctx, q, executionID)
	if err != nil {
		return contracts.BatchCheckpointState{}, err
	}
	if !found {
		return contracts.BatchCheckpointState{
			ExecutionID: executionID,
		}, nil
	}
	lastCursor := ""
	if ckpt.CommittedBatches > 0 {
		lastCursor = strconv.FormatInt(ckpt.LastCursor, 10)
	}
	return contracts.BatchCheckpointState{
		ExecutionID:          ckpt.ExecutionID,
		TotalScopeRows:       ckpt.TotalRows,
		CommittedBatches:     ckpt.CommittedBatches,
		CommittedRows:        ckpt.CommittedRows,
		LastCommittedCursor:  lastCursor,
		CompensatedBatches:   ckpt.CompensatedBatches,
		LastCompensatedBatch: ckpt.LastCompensatedBatch,
		UpdatedAt:            ckpt.UpdatedAt,
	}, nil
}

// FilterHideRollbacks filters a chronological or reverse-chronological slice of
// CommitHistoryEvent for the "Hide rollback" view.
//
// When a rollback/compensate event (E3) references RevertsExecutionID (E2),
// both E2 and E3 are omitted from the returned slice while earlier active events (E1)
// and any subsequent forward re-executions (E4) remain visible.
func FilterHideRollbacks(events []contracts.CommitHistoryEvent) []contracts.CommitHistoryEvent {
	if len(events) == 0 {
		return []contracts.CommitHistoryEvent{}
	}
	hiddenExecIDs := make(map[contracts.ExecutionID]struct{})
	for _, ev := range events {
		if (ev.Direction == contracts.DirectionRollback || ev.Direction == contracts.DirectionCompensate) &&
			strings.TrimSpace(string(ev.RevertsExecutionID)) != "" {
			hiddenExecIDs[ev.ExecutionID] = struct{}{}
			hiddenExecIDs[ev.RevertsExecutionID] = struct{}{}
		}
	}
	if len(hiddenExecIDs) == 0 {
		out := make([]contracts.CommitHistoryEvent, len(events))
		copy(out, events)
		return out
	}

	filtered := make([]contracts.CommitHistoryEvent, 0, len(events))
	for _, ev := range events {
		if _, hidden := hiddenExecIDs[ev.ExecutionID]; hidden {
			continue
		}
		filtered = append(filtered, ev)
	}
	return filtered
}

