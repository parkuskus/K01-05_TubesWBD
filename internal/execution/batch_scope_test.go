package execution

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

type fakeBatchScopeBackend struct {
	schemaReady       bool
	scopes            map[string]BatchScopeRecord
	scopeRows         map[string][]int64
	checkpoints       map[string]BatchCheckpointRecord
	sourceRowIDs      []int64
	materializeCount  int
}

func newFakeBatchScopeBackend(rowIDs ...int64) *fakeBatchScopeBackend {
	return &fakeBatchScopeBackend{
		scopes:       make(map[string]BatchScopeRecord),
		scopeRows:    make(map[string][]int64),
		checkpoints:  make(map[string]BatchCheckpointRecord),
		sourceRowIDs: append([]int64(nil), rowIDs...),
	}
}

type fakeBatchScopeConnector struct {
	backend *fakeBatchScopeBackend
}

func (c fakeBatchScopeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeBatchScopeConn{backend: c.backend}, nil
}

func (fakeBatchScopeConnector) Driver() driver.Driver { return fakeOrdinaryDriver{} }

type fakeBatchScopeConn struct {
	backend *fakeBatchScopeBackend
}

func (*fakeBatchScopeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (*fakeBatchScopeConn) Close() error                        { return nil }
func (*fakeBatchScopeConn) Begin() (driver.Tx, error)           { return fakeBatchScopeTx{}, nil }

type fakeBatchScopeTx struct{}

func (fakeBatchScopeTx) Commit() error   { return nil }
func (fakeBatchScopeTx) Rollback() error { return nil }

func (c *fakeBatchScopeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	trimmed := strings.TrimSpace(query)
	switch {
	case strings.Contains(trimmed, "CREATE TABLE IF NOT EXISTS if3110_platform.migration_history_v2"),
		strings.Contains(trimmed, "CREATE TABLE IF NOT EXISTS if3110_platform.batch_scopes"):
		c.backend.schemaReady = true
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "INSERT INTO if3110_platform.batch_scopes"):
		execID := args[0].Value.(string)
		c.backend.scopes[execID] = BatchScopeRecord{
			ExecutionID: contracts.ExecutionID(execID),
			SchemaName:  args[1].Value.(string),
			TableName:   args[2].Value.(string),
			PKColumn:    args[3].Value.(string),
			ScopeWhere:  args[4].Value.(string),
			BatchSize:   int(args[5].Value.(int64)),
			CreatedAt:   args[6].Value.(time.Time),
		}
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "INSERT INTO if3110_platform.batch_scope_rows"):
		execID := args[0].Value.(string)
		c.backend.materializeCount++
		c.backend.scopeRows[execID] = append([]int64(nil), c.backend.sourceRowIDs...)
		return driver.RowsAffected(int64(len(c.backend.sourceRowIDs))), nil

	case strings.HasPrefix(trimmed, "UPDATE if3110_platform.batch_scopes"):
		totalRows := args[0].Value.(int64)
		minPK := args[1].Value.(int64)
		maxPK := args[2].Value.(int64)
		execID := args[3].Value.(string)
		sc := c.backend.scopes[execID]
		sc.TotalRows = totalRows
		sc.MinPK = minPK
		sc.MaxPK = maxPK
		c.backend.scopes[execID] = sc
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "INSERT INTO if3110_platform.batch_checkpoints"):
		execID := args[0].Value.(string)
		c.backend.checkpoints[execID] = BatchCheckpointRecord{
			ExecutionID:      contracts.ExecutionID(execID),
			Phase:            CommitPhase(args[1].Value.(string)),
			TotalRows:        args[2].Value.(int64),
			CommittedBatches: 0,
			CommittedRows:    0,
			LastCursor:       args[3].Value.(int64),
			UpdatedAt:        args[4].Value.(time.Time),
		}
		return driver.RowsAffected(1), nil

	default:
		return nil, errors.New("unexpected ExecContext query: " + trimmed)
	}
}

func (c *fakeBatchScopeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	trimmed := strings.TrimSpace(query)
	if !c.backend.schemaReady {
		return nil, &pgconn.PgError{Code: "42P01", Message: "relation does not exist"}
	}

	switch {
	case strings.Contains(trimmed, "FROM if3110_platform.batch_scopes"):
		execID := args[0].Value.(string)
		if sc, ok := c.backend.scopes[execID]; ok {
			return &fakeScopeHeaderRows{records: []BatchScopeRecord{sc}}, nil
		}
		return &fakeScopeHeaderRows{}, nil

	case strings.Contains(trimmed, "FROM if3110_platform.batch_scope_rows"):
		execID := args[0].Value.(string)
		rows := c.backend.scopeRows[execID]
		var total, minPK, maxPK int64
		total = int64(len(rows))
		if total > 0 {
			minPK = rows[0]
			maxPK = rows[len(rows)-1]
		}
		return &fakeScopeAggRows{total: total, minPK: minPK, maxPK: maxPK}, nil

	case strings.Contains(trimmed, "FROM if3110_platform.batch_checkpoints"):
		execID := args[0].Value.(string)
		if ckpt, ok := c.backend.checkpoints[execID]; ok {
			return &fakeCheckpointRows{records: []BatchCheckpointRecord{ckpt}}, nil
		}
		return &fakeCheckpointRows{}, nil

	default:
		return nil, errors.New("unexpected QueryContext query: " + trimmed)
	}
}

type fakeScopeHeaderRows struct {
	records []BatchScopeRecord
	idx     int
}

func (*fakeScopeHeaderRows) Columns() []string {
	return []string{
		"execution_id", "schema_name", "table_name", "pk_column", "scope_where",
		"batch_size", "total_rows", "min_pk", "max_pk", "created_at",
	}
}

func (*fakeScopeHeaderRows) Close() error { return nil }

func (r *fakeScopeHeaderRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.records) {
		return io.EOF
	}
	sc := r.records[r.idx]
	r.idx++
	dest[0] = string(sc.ExecutionID)
	dest[1] = sc.SchemaName
	dest[2] = sc.TableName
	dest[3] = sc.PKColumn
	dest[4] = sc.ScopeWhere
	dest[5] = int64(sc.BatchSize)
	dest[6] = sc.TotalRows
	dest[7] = sc.MinPK
	dest[8] = sc.MaxPK
	dest[9] = sc.CreatedAt
	return nil
}

type fakeScopeAggRows struct {
	total int64
	minPK int64
	maxPK int64
	done  bool
}

func (*fakeScopeAggRows) Columns() []string { return []string{"count", "min", "max"} }
func (*fakeScopeAggRows) Close() error      { return nil }
func (r *fakeScopeAggRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.total
	dest[1] = r.minPK
	dest[2] = r.maxPK
	return nil
}

type fakeCheckpointRows struct {
	records []BatchCheckpointRecord
	idx     int
}

func (*fakeCheckpointRows) Columns() []string {
	return []string{
		"execution_id", "phase", "total_rows", "committed_batches", "committed_rows",
		"last_cursor", "compensated_batches", "last_compensated_batch", "updated_at",
	}
}

func (*fakeCheckpointRows) Close() error { return nil }

func (r *fakeCheckpointRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.records) {
		return io.EOF
	}
	c := r.records[r.idx]
	r.idx++
	dest[0] = string(c.ExecutionID)
	dest[1] = string(c.Phase)
	dest[2] = c.TotalRows
	dest[3] = int64(c.CommittedBatches)
	dest[4] = c.CommittedRows
	dest[5] = c.LastCursor
	dest[6] = int64(c.CompensatedBatches)
	dest[7] = int64(c.LastCompensatedBatch)
	dest[8] = c.UpdatedAt
	return nil
}

func TestFreezeBatchScopeMaterializesRowsServerSideAndIsIdempotent(t *testing.T) {
	backend := newFakeBatchScopeBackend(10, 20, 30, 40, 50)
	sqlDB := sql.OpenDB(fakeBatchScopeConnector{backend: backend})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Before schema exists, ReadBatchCheckpoint is read-only and returns found=false.
	if _, found, err := ReadBatchCheckpoint(ctx, conn, "exec-batch-1"); err != nil || found {
		t.Fatalf("ReadBatchCheckpoint before init: found=%v err=%v", found, err)
	}

	cfg := contracts.BatchConfig{
		TableSchema:      "public",
		TableName:        "orders",
		PrimaryKeyColumn: "id",
		ScopeWhere:       "status = 'PENDING'",
		BatchSize:        2,
	}

	scope, ckpt, err := FreezeBatchScope(ctx, conn, "exec-batch-1", cfg)
	if err != nil {
		t.Fatalf("FreezeBatchScope() error = %v", err)
	}
	if scope.TotalRows != 5 || scope.MinPK != 10 || scope.MaxPK != 50 {
		t.Fatalf("unexpected frozen scope bounds: %+v", scope)
	}
	if ckpt.TotalRows != 5 || ckpt.CommittedBatches != 0 || ckpt.CommittedRows != 0 || ckpt.LastCursor != 9 {
		t.Fatalf("unexpected initial checkpoint: %+v", ckpt)
	}
	if backend.materializeCount != 1 {
		t.Fatalf("materializeCount = %d, want 1", backend.materializeCount)
	}

	// Simulate new rows added to public.orders after scope freeze; calling FreezeBatchScope
	// again for the same execution_id must return the existing frozen scope without re-materializing!
	backend.sourceRowIDs = append(backend.sourceRowIDs, 60, 70)
	scopeAgain, ckptAgain, err := FreezeBatchScope(ctx, conn, "exec-batch-1", cfg)
	if err != nil {
		t.Fatalf("second FreezeBatchScope() error = %v", err)
	}
	if scopeAgain.TotalRows != 5 || ckptAgain.TotalRows != 5 || backend.materializeCount != 1 {
		t.Fatalf("idempotent FreezeBatchScope re-materialized scope: scope=%+v ckpt=%+v count=%d",
			scopeAgain, ckptAgain, backend.materializeCount)
	}
}

func TestBuildMaterializeScopeSQLRejectsUnsafeIdentifiersAndWhereClauses(t *testing.T) {
	validCfg := contracts.BatchConfig{
		TableSchema:      "public",
		TableName:        "invoices",
		PrimaryKeyColumn: "invoice_id",
		ScopeWhere:       "created_at < '2026-01-01' AND note <> 'drop update'",
		BatchSize:        100,
	}
	sqlText, err := BuildMaterializeScopeSQL(validCfg)
	if err != nil {
		t.Fatalf("BuildMaterializeScopeSQL(valid) error = %v", err)
	}
	wantPrefix := `INSERT INTO if3110_platform.batch_scope_rows (execution_id, row_id) SELECT $1, "invoice_id"::bigint FROM "public"."invoices" WHERE (`
	if !strings.HasPrefix(sqlText, wantPrefix) {
		t.Fatalf("unexpected materialize SQL: %s", sqlText)
	}

	invalidConfigs := []struct {
		cfg     contracts.BatchConfig
		wantErr error
	}{
		{
			cfg: contracts.BatchConfig{
				TableSchema:      "if3110_platform",
				TableName:        "migration_history_v2",
				PrimaryKeyColumn: "id",
				BatchSize:        10,
			},
			wantErr: ErrForbiddenInternalSchema,
		},
		{
			cfg: contracts.BatchConfig{
				TableSchema:      "public",
				TableName:        "orders; DROP TABLE users",
				PrimaryKeyColumn: "id",
				BatchSize:        10,
			},
			wantErr: ErrInvalidBatchIdentifier,
		},
		{
			cfg: contracts.BatchConfig{
				TableSchema:      "public",
				TableName:        "orders",
				PrimaryKeyColumn: "id",
				ScopeWhere:       "1=1; DELETE FROM orders",
				BatchSize:        10,
			},
			wantErr: ErrForbiddenBatchScope,
		},
		{
			cfg: contracts.BatchConfig{
				TableSchema:      "public",
				TableName:        "orders",
				PrimaryKeyColumn: "id",
				ScopeWhere:       "EXISTS (SELECT 1 FROM if3110_platform.execution_commits)",
				BatchSize:        10,
			},
			wantErr: ErrForbiddenInternalSchema,
		},
	}

	for _, tc := range invalidConfigs {
		if _, err := BuildMaterializeScopeSQL(tc.cfg); !errors.Is(err, tc.wantErr) {
			t.Fatalf("BuildMaterializeScopeSQL(%+v) = %v, want %v", tc.cfg, err, tc.wantErr)
		}
	}
}

