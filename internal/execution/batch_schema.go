package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

const ensureBatchSchemaSQL = `
CREATE TABLE IF NOT EXISTS if3110_platform.batch_scopes (
    execution_id TEXT PRIMARY KEY CHECK (length(btrim(execution_id)) > 0),
    schema_name TEXT NOT NULL CHECK (length(btrim(schema_name)) > 0),
    table_name TEXT NOT NULL CHECK (length(btrim(table_name)) > 0),
    pk_column TEXT NOT NULL CHECK (length(btrim(pk_column)) > 0),
    scope_where TEXT NOT NULL DEFAULT '',
    batch_size INT NOT NULL CHECK (batch_size > 0),
    total_rows BIGINT NOT NULL DEFAULT 0 CHECK (total_rows >= 0),
    min_pk BIGINT NOT NULL DEFAULT 0,
    max_pk BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS if3110_platform.batch_scope_rows (
    execution_id TEXT NOT NULL REFERENCES if3110_platform.batch_scopes(execution_id) ON DELETE CASCADE,
    row_id BIGINT NOT NULL,
    PRIMARY KEY (execution_id, row_id)
);

CREATE TABLE IF NOT EXISTS if3110_platform.batch_checkpoints (
    execution_id TEXT PRIMARY KEY CHECK (length(btrim(execution_id)) > 0),
    phase TEXT NOT NULL DEFAULT 'BATCH_UP' CHECK (phase IN ('BATCH_UP', 'BATCH_DOWN', 'FINALIZE')),
    total_rows BIGINT NOT NULL DEFAULT 0 CHECK (total_rows >= 0),
    committed_batches INT NOT NULL DEFAULT 0 CHECK (committed_batches >= 0),
    committed_rows BIGINT NOT NULL DEFAULT 0 CHECK (committed_rows >= 0),
    last_cursor BIGINT NOT NULL DEFAULT 0,
    compensated_batches INT NOT NULL DEFAULT 0 CHECK (compensated_batches >= 0),
    last_compensated_batch INT NOT NULL DEFAULT 0 CHECK (last_compensated_batch >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS if3110_platform.batch_recovery_rows (
    execution_id TEXT NOT NULL CHECK (length(btrim(execution_id)) > 0),
    batch_no INT NOT NULL CHECK (batch_no > 0),
    row_id BIGINT NOT NULL,
    before_data JSONB NOT NULL,
    after_data JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (execution_id, batch_no, row_id)
);

CREATE INDEX IF NOT EXISTS batch_recovery_rows_exec_batch_idx
    ON if3110_platform.batch_recovery_rows (execution_id, batch_no DESC, row_id ASC);
`

// BatchScopeRecord represents a frozen row scope in if3110_platform.batch_scopes.
type BatchScopeRecord struct {
	ExecutionID contracts.ExecutionID
	SchemaName  string
	TableName   string
	PKColumn    string
	ScopeWhere  string
	BatchSize   int
	TotalRows   int64
	MinPK       int64
	MaxPK       int64
	CreatedAt   time.Time
}

// BatchCheckpointRecord represents durable batch progress in if3110_platform.batch_checkpoints.
type BatchCheckpointRecord struct {
	ExecutionID          contracts.ExecutionID
	Phase                CommitPhase
	TotalRows            int64
	CommittedBatches     int
	CommittedRows        int64
	LastCursor           int64
	CompensatedBatches   int
	LastCompensatedBatch int
	UpdatedAt            time.Time
}

// ToContract converts a BatchCheckpointRecord into contracts.BatchCheckpointState.
func (c BatchCheckpointRecord) ToContract() contracts.BatchCheckpointState {
	cursorStr := ""
	if c.CommittedBatches > 0 {
		cursorStr = strconv.FormatInt(c.LastCursor, 10)
	}
	return contracts.BatchCheckpointState{
		ExecutionID:          c.ExecutionID,
		TotalScopeRows:       c.TotalRows,
		CommittedBatches:     c.CommittedBatches,
		CommittedRows:        c.CommittedRows,
		LastCommittedCursor:  cursorStr,
		CompensatedBatches:   c.CompensatedBatches,
		LastCompensatedBatch: c.LastCompensatedBatch,
		UpdatedAt:            c.UpdatedAt,
	}
}

// BatchRecoveryRow stores the before-image and after-image of a single modified row
// in if3110_platform.batch_recovery_rows.
type BatchRecoveryRow struct {
	ExecutionID contracts.ExecutionID
	BatchNo     int
	RowID       int64
	BeforeData  json.RawMessage
	AfterData   json.RawMessage
}

// EnsureBatchSchema creates the Milestone 2 target ledger tables and batch
// support tables (batch_scopes, batch_scope_rows, batch_checkpoints, and
// batch_recovery_rows) on a writable target connection.
func EnsureBatchSchema(ctx context.Context, conn *sql.Conn) error {
	if conn == nil {
		return ErrNilTargetConn
	}
	return EnsureBatchSchemaOnExecer(ctx, conn)
}

// EnsureBatchSchemaOnExecer creates both the base target schema v2 and batch
// support tables on the provided connection or transaction.
func EnsureBatchSchemaOnExecer(ctx context.Context, exec sqlExecer) error {
	if err := EnsureTargetSchemaV2OnExecer(ctx, exec); err != nil {
		return err
	}
	if _, err := exec.ExecContext(ctx, ensureBatchSchemaSQL); err != nil {
		return fmt.Errorf("ensure batch schema: %w", err)
	}
	return nil
}

// ReadBatchCheckpoint reads a BatchCheckpointRecord from if3110_platform.batch_checkpoints
// without executing any DDL. If the table or row does not exist, it returns found=false, err=nil.
func ReadBatchCheckpoint(
	ctx context.Context,
	q sqlRowQueryer,
	executionID contracts.ExecutionID,
) (BatchCheckpointRecord, bool, error) {
	if q == nil {
		return BatchCheckpointRecord{}, false, ErrNilTargetConn
	}
	const query = `
		SELECT
			execution_id,
			phase,
			total_rows,
			committed_batches,
			committed_rows,
			last_cursor,
			compensated_batches,
			last_compensated_batch,
			updated_at
		FROM if3110_platform.batch_checkpoints
		WHERE execution_id = $1
	`
	var (
		rec       BatchCheckpointRecord
		execIDStr string
		phaseStr  string
	)
	err := q.QueryRowContext(ctx, query, string(executionID)).Scan(
		&execIDStr,
		&phaseStr,
		&rec.TotalRows,
		&rec.CommittedBatches,
		&rec.CommittedRows,
		&rec.LastCursor,
		&rec.CompensatedBatches,
		&rec.LastCompensatedBatch,
		&rec.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || IsMissingTargetSchemaError(err) {
			return BatchCheckpointRecord{}, false, nil
		}
		return BatchCheckpointRecord{}, false, fmt.Errorf("read batch checkpoint %s: %w", executionID, err)
	}
	rec.ExecutionID = contracts.ExecutionID(execIDStr)
	rec.Phase = CommitPhase(phaseStr)
	rec.UpdatedAt = rec.UpdatedAt.UTC()
	return rec, true, nil
}

