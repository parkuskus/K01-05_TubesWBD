package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"if3110.local/db-platform/internal/contracts"
)

var (
	ErrInvalidBatchIdentifier = errors.New("invalid batch schema, table, or primary key identifier")
	ErrForbiddenBatchScope    = errors.New("batch scope WHERE clause contains forbidden statements or references")
)

// QuoteIdentifier validates and double-quotes a PostgreSQL identifier (schema, table, or column).
func QuoteIdentifier(ident string) (string, error) {
	trimmed := strings.TrimSpace(ident)
	if trimmed == "" || len(trimmed) > 63 {
		return "", ErrInvalidBatchIdentifier
	}
	lower := strings.ToLower(trimmed)
	if lower == "if3110_platform" || lower == "pg_catalog" || lower == "information_schema" {
		return "", ErrForbiddenInternalSchema
	}
	for i, r := range trimmed {
		if r == 0 {
			return "", ErrInvalidBatchIdentifier
		}
		if i == 0 && !unicode.IsLetter(r) && r != '_' {
			return "", ErrInvalidBatchIdentifier
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' {
			return "", ErrInvalidBatchIdentifier
		}
	}
	escaped := strings.ReplaceAll(trimmed, `"`, `""`)
	return `"` + escaped + `"`, nil
}

// ValidateScopeWhere ensures a batch scope WHERE predicate is safe to embed in a
// server-side SELECT query (no semicolons, no transaction control, no DDL/DML,
// and no reference to if3110_platform).
func ValidateScopeWhere(whereClause string) error {
	trimmed := strings.TrimSpace(whereClause)
	if trimmed == "" {
		return nil
	}

	stripped := stripLiteralsAndComments(trimmed)
	if strings.Contains(stripped, ";") {
		return ErrForbiddenBatchScope
	}
	lower := strings.ToLower(stripped)
	if strings.Contains(lower, "if3110_platform") {
		return ErrForbiddenInternalSchema
	}

	for _, tok := range tokenizeWords(lower) {
		switch tok {
		case "insert", "update", "delete", "drop", "alter", "create", "truncate",
			"grant", "revoke", "begin", "commit", "rollback", "savepoint", "copy", "call", "do":
			return ErrForbiddenBatchScope
		}
	}
	return nil
}

// BuildMaterializeScopeSQL constructs the server-side INSERT ... SELECT query
// that freezes target primary keys directly inside PostgreSQL without loading
// row IDs into Go memory.
func BuildMaterializeScopeSQL(cfg contracts.BatchConfig) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	quotedSchema, err := QuoteIdentifier(cfg.TableSchema)
	if err != nil {
		return "", err
	}
	quotedTable, err := QuoteIdentifier(cfg.TableName)
	if err != nil {
		return "", err
	}
	quotedPK, err := QuoteIdentifier(cfg.PrimaryKeyColumn)
	if err != nil {
		return "", err
	}
	if err := ValidateScopeWhere(cfg.ScopeWhere); err != nil {
		return "", err
	}

	whereClause := ""
	if trimmedWhere := strings.TrimSpace(cfg.ScopeWhere); trimmedWhere != "" {
		whereClause = " WHERE (" + trimmedWhere + ")"
	}

	query := fmt.Sprintf(
		`INSERT INTO if3110_platform.batch_scope_rows (execution_id, row_id) SELECT $1, %s::bigint FROM %s.%s%s ORDER BY %s ASC`,
		quotedPK,
		quotedSchema,
		quotedTable,
		whereClause,
		quotedPK,
	)
	return query, nil
}

// FreezeBatchScope atomically freezes the set of target row primary keys for
// executionID inside if3110_platform.batch_scopes, batch_scope_rows, and
// batch_checkpoints on the target database.
//
// Invariants:
//  1. Primary key materialization runs entirely inside PostgreSQL via
//     INSERT INTO batch_scope_rows SELECT ...; Go never loads all row IDs into RAM.
//  2. Calling FreezeBatchScope again for the same executionID is idempotent and
//     returns the existing frozen scope and current checkpoint without re-querying
//     the user table (so rows added or modified later never shift the scope on resume).
func FreezeBatchScope(
	ctx context.Context,
	conn *sql.Conn,
	executionID contracts.ExecutionID,
	cfg contracts.BatchConfig,
) (BatchScopeRecord, BatchCheckpointRecord, error) {
	if conn == nil {
		return BatchScopeRecord{}, BatchCheckpointRecord{}, ErrNilTargetConn
	}
	trimmedExecID := strings.TrimSpace(string(executionID))
	if trimmedExecID == "" {
		return BatchScopeRecord{}, BatchCheckpointRecord{}, contracts.ErrInvalidExecutionPlan
	}
	materializeSQL, err := BuildMaterializeScopeSQL(cfg)
	if err != nil {
		return BatchScopeRecord{}, BatchCheckpointRecord{}, err
	}

	if err := EnsureBatchSchema(ctx, conn); err != nil {
		return BatchScopeRecord{}, BatchCheckpointRecord{}, err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return BatchScopeRecord{}, BatchCheckpointRecord{}, fmt.Errorf("begin freeze batch scope tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	existingScope, foundScope, err := readBatchScopeInTx(ctx, tx, executionID)
	if err != nil {
		return BatchScopeRecord{}, BatchCheckpointRecord{}, err
	}
	if foundScope {
		ckpt, foundCkpt, err := ReadBatchCheckpoint(ctx, tx, executionID)
		if err != nil {
			return BatchScopeRecord{}, BatchCheckpointRecord{}, err
		}
		if !foundCkpt {
			return BatchScopeRecord{}, BatchCheckpointRecord{}, fmt.Errorf("batch scope %s exists without checkpoint", executionID)
		}
		if err := tx.Commit(); err != nil {
			return BatchScopeRecord{}, BatchCheckpointRecord{}, fmt.Errorf("commit idempotent scope lookup: %w", err)
		}
		return existingScope, ckpt, nil
	}

	now := time.Now().UTC()
	trimmedSchema := strings.TrimSpace(cfg.TableSchema)
	trimmedTable := strings.TrimSpace(cfg.TableName)
	trimmedPK := strings.TrimSpace(cfg.PrimaryKeyColumn)
	trimmedWhere := strings.TrimSpace(cfg.ScopeWhere)

	const insertScopeHeaderSQL = `
		INSERT INTO if3110_platform.batch_scopes (
			execution_id,
			schema_name,
			table_name,
			pk_column,
			scope_where,
			batch_size,
			total_rows,
			min_pk,
			max_pk,
			created_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, 0, 0, 0, $7
		)
	`
	if _, err := tx.ExecContext(
		ctx,
		insertScopeHeaderSQL,
		trimmedExecID,
		trimmedSchema,
		trimmedTable,
		trimmedPK,
		trimmedWhere,
		cfg.BatchSize,
		now,
	); err != nil {
		return BatchScopeRecord{}, BatchCheckpointRecord{}, fmt.Errorf("insert batch scope header: %w", err)
	}

	// Materialize target row IDs inside PostgreSQL server without loading into Go memory.
	if _, err := tx.ExecContext(ctx, materializeSQL, trimmedExecID); err != nil {
		return BatchScopeRecord{}, BatchCheckpointRecord{}, fmt.Errorf("materialize batch scope rows: %w", err)
	}

	var (
		totalRows int64
		minPK     int64
		maxPK     int64
	)
	const aggregateScopeSQL = `
		SELECT
			COUNT(*),
			COALESCE(MIN(row_id), 0),
			COALESCE(MAX(row_id), 0)
		FROM if3110_platform.batch_scope_rows
		WHERE execution_id = $1
	`
	if err := tx.QueryRowContext(ctx, aggregateScopeSQL, trimmedExecID).Scan(&totalRows, &minPK, &maxPK); err != nil {
		return BatchScopeRecord{}, BatchCheckpointRecord{}, fmt.Errorf("aggregate batch scope bounds: %w", err)
	}

	var initialCursor int64
	if totalRows > 0 {
		initialCursor = minPK - 1
	}

	const updateScopeBoundsSQL = `
		UPDATE if3110_platform.batch_scopes
		SET total_rows = $1,
		    min_pk = $2,
		    max_pk = $3
		WHERE execution_id = $4
	`
	if _, err := tx.ExecContext(ctx, updateScopeBoundsSQL, totalRows, minPK, maxPK, trimmedExecID); err != nil {
		return BatchScopeRecord{}, BatchCheckpointRecord{}, fmt.Errorf("update batch scope bounds: %w", err)
	}

	const insertCheckpointSQL = `
		INSERT INTO if3110_platform.batch_checkpoints (
			execution_id,
			phase,
			total_rows,
			committed_batches,
			committed_rows,
			last_cursor,
			compensated_batches,
			last_compensated_batch,
			updated_at
		) VALUES (
			$1, $2, $3, 0, 0, $4, 0, 0, $5
		)
	`
	if _, err := tx.ExecContext(
		ctx,
		insertCheckpointSQL,
		trimmedExecID,
		string(CommitPhaseBatchUp),
		totalRows,
		initialCursor,
		now,
	); err != nil {
		return BatchScopeRecord{}, BatchCheckpointRecord{}, fmt.Errorf("insert initial batch checkpoint: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return BatchScopeRecord{}, BatchCheckpointRecord{}, fmt.Errorf("commit batch scope freeze: %w", err)
	}

	scope := BatchScopeRecord{
		ExecutionID: executionID,
		SchemaName:  trimmedSchema,
		TableName:   trimmedTable,
		PKColumn:    trimmedPK,
		ScopeWhere:  trimmedWhere,
		BatchSize:   cfg.BatchSize,
		TotalRows:   totalRows,
		MinPK:       minPK,
		MaxPK:       maxPK,
		CreatedAt:   now,
	}
	checkpoint := BatchCheckpointRecord{
		ExecutionID:      executionID,
		Phase:            CommitPhaseBatchUp,
		TotalRows:        totalRows,
		CommittedBatches: 0,
		CommittedRows:    0,
		LastCursor:       initialCursor,
		UpdatedAt:        now,
	}
	return scope, checkpoint, nil
}

func readBatchScopeInTx(
	ctx context.Context,
	q sqlRowQueryer,
	executionID contracts.ExecutionID,
) (BatchScopeRecord, bool, error) {
	const query = `
		SELECT
			execution_id,
			schema_name,
			table_name,
			pk_column,
			scope_where,
			batch_size,
			total_rows,
			min_pk,
			max_pk,
			created_at
		FROM if3110_platform.batch_scopes
		WHERE execution_id = $1
		FOR UPDATE
	`
	var (
		rec       BatchScopeRecord
		execIDStr string
	)
	err := q.QueryRowContext(ctx, query, string(executionID)).Scan(
		&execIDStr,
		&rec.SchemaName,
		&rec.TableName,
		&rec.PKColumn,
		&rec.ScopeWhere,
		&rec.BatchSize,
		&rec.TotalRows,
		&rec.MinPK,
		&rec.MaxPK,
		&rec.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BatchScopeRecord{}, false, nil
		}
		return BatchScopeRecord{}, false, fmt.Errorf("read batch scope %s: %w", executionID, err)
	}
	rec.ExecutionID = contracts.ExecutionID(execIDStr)
	rec.CreatedAt = rec.CreatedAt.UTC()
	return rec, true, nil
}

