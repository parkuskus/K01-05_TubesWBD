package execution

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

type fakeBatchRowData struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

type fakeBatchEngineDB struct {
	schemaReady           bool
	tableRows             map[int64]fakeBatchRowData
	scopes                map[string]BatchScopeRecord
	scopeRows             map[string][]int64
	checkpoints           map[string]BatchCheckpointRecord
	recoveryRows          map[string]map[int][]BatchRecoveryRow
	proofs                map[string]ExecutionCommitProof
	history               []contracts.CommitHistoryEvent
	head                  contracts.ActiveHeadState
	txCommitCount         int
	failOnBatchNo         int
	failBatchErr          error
	compensatedBatchOrder []int

	inTx               bool
	stagedTableRows    map[int64]fakeBatchRowData
	stagedScopes       map[string]BatchScopeRecord
	stagedScopeRows    map[string][]int64
	stagedCheckpoints  map[string]BatchCheckpointRecord
	stagedRecoveryRows map[string]map[int][]BatchRecoveryRow
	stagedProofs       map[string]ExecutionCommitProof
	stagedHistory      []contracts.CommitHistoryEvent
	stagedHead         contracts.ActiveHeadState
}

func newFakeBatchEngineDB(initialRows map[int64]string) *fakeBatchEngineDB {
	rows := make(map[int64]fakeBatchRowData, len(initialRows))
	for id, st := range initialRows {
		rows[id] = fakeBatchRowData{ID: id, Status: st}
	}
	return &fakeBatchEngineDB{
		tableRows:    rows,
		scopes:       make(map[string]BatchScopeRecord),
		scopeRows:    make(map[string][]int64),
		checkpoints:  make(map[string]BatchCheckpointRecord),
		recoveryRows: make(map[string]map[int][]BatchRecoveryRow),
		proofs:       make(map[string]ExecutionCommitProof),
	}
}

func (db *fakeBatchEngineDB) sortedMatchingRowIDs() []int64 {
	var ids []int64
	for id := range db.stagedTableRows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

type fakeBatchEngineConnector struct {
	db *fakeBatchEngineDB
}

func (c fakeBatchEngineConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeBatchEngineConn{db: c.db}, nil
}

func (fakeBatchEngineConnector) Driver() driver.Driver { return fakeOrdinaryDriver{} }

type fakeBatchEngineConn struct {
	db *fakeBatchEngineDB
}

func (*fakeBatchEngineConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (*fakeBatchEngineConn) Close() error                        { return nil }

func (c *fakeBatchEngineConn) Begin() (driver.Tx, error) {
	db := c.db
	db.inTx = true
	db.stagedTableRows = make(map[int64]fakeBatchRowData, len(db.tableRows))
	for k, v := range db.tableRows {
		db.stagedTableRows[k] = v
	}
	db.stagedScopes = make(map[string]BatchScopeRecord, len(db.scopes))
	for k, v := range db.scopes {
		db.stagedScopes[k] = v
	}
	db.stagedScopeRows = make(map[string][]int64, len(db.scopeRows))
	for k, v := range db.scopeRows {
		db.stagedScopeRows[k] = append([]int64(nil), v...)
	}
	db.stagedCheckpoints = make(map[string]BatchCheckpointRecord, len(db.checkpoints))
	for k, v := range db.checkpoints {
		db.stagedCheckpoints[k] = v
	}
	db.stagedRecoveryRows = make(map[string]map[int][]BatchRecoveryRow, len(db.recoveryRows))
	for execID, byBatch := range db.recoveryRows {
		copiedByBatch := make(map[int][]BatchRecoveryRow, len(byBatch))
		for bNo, list := range byBatch {
			copiedByBatch[bNo] = append([]BatchRecoveryRow(nil), list...)
		}
		db.stagedRecoveryRows[execID] = copiedByBatch
	}
	db.stagedProofs = make(map[string]ExecutionCommitProof, len(db.proofs))
	for k, v := range db.proofs {
		db.stagedProofs[k] = v
	}
	db.stagedHistory = append([]contracts.CommitHistoryEvent(nil), db.history...)
	db.stagedHead = db.head
	return &fakeBatchEngineTx{db: db}, nil
}

type fakeBatchEngineTx struct {
	db *fakeBatchEngineDB
}

func (tx *fakeBatchEngineTx) Commit() error {
	db := tx.db
	if db.inTx {
		db.tableRows = db.stagedTableRows
		db.scopes = db.stagedScopes
		db.scopeRows = db.stagedScopeRows
		db.checkpoints = db.stagedCheckpoints
		db.recoveryRows = db.stagedRecoveryRows
		db.proofs = db.stagedProofs
		db.history = db.stagedHistory
		db.head = db.stagedHead
		db.inTx = false
		db.txCommitCount++
	}
	return nil
}

func (tx *fakeBatchEngineTx) Rollback() error {
	db := tx.db
	if db.inTx {
		db.stagedTableRows = nil
		db.stagedScopes = nil
		db.stagedScopeRows = nil
		db.stagedCheckpoints = nil
		db.stagedRecoveryRows = nil
		db.stagedProofs = nil
		db.stagedHistory = nil
		db.inTx = false
	}
	return nil
}

func (c *fakeBatchEngineConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	trimmed := strings.TrimSpace(query)
	db := c.db

	switch {
	case strings.Contains(trimmed, "CREATE TABLE IF NOT EXISTS if3110_platform.migration_history_v2"),
		strings.Contains(trimmed, "CREATE TABLE IF NOT EXISTS if3110_platform.batch_scopes"):
		db.schemaReady = true
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "INSERT INTO if3110_platform.batch_scopes"):
		execID := args[0].Value.(string)
		db.stagedScopes[execID] = BatchScopeRecord{
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
		ids := db.sortedMatchingRowIDs()
		db.stagedScopeRows[execID] = ids
		return driver.RowsAffected(int64(len(ids))), nil

	case strings.HasPrefix(trimmed, "UPDATE if3110_platform.batch_scopes"):
		totalRows := args[0].Value.(int64)
		minPK := args[1].Value.(int64)
		maxPK := args[2].Value.(int64)
		execID := args[3].Value.(string)
		sc := db.stagedScopes[execID]
		sc.TotalRows = totalRows
		sc.MinPK = minPK
		sc.MaxPK = maxPK
		db.stagedScopes[execID] = sc
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "INSERT INTO if3110_platform.batch_checkpoints"):
		execID := args[0].Value.(string)
		db.stagedCheckpoints[execID] = BatchCheckpointRecord{
			ExecutionID: contracts.ExecutionID(execID),
			Phase:       CommitPhase(args[1].Value.(string)),
			TotalRows:   args[2].Value.(int64),
			LastCursor:  args[3].Value.(int64),
			UpdatedAt:   args[4].Value.(time.Time),
		}
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "INSERT INTO if3110_platform.batch_recovery_rows"):
		execID := args[0].Value.(string)
		batchNo := int(args[1].Value.(int64))
		minID := args[2].Value.(int64)
		maxID := args[3].Value.(int64)
		if db.stagedRecoveryRows[execID] == nil {
			db.stagedRecoveryRows[execID] = make(map[int][]BatchRecoveryRow)
		}
		var list []BatchRecoveryRow
		for _, id := range db.stagedScopeRows[execID] {
			if id >= minID && id <= maxID {
				row, ok := db.stagedTableRows[id]
				if !ok {
					continue
				}
				raw, _ := json.Marshal(row)
				list = append(list, BatchRecoveryRow{
					ExecutionID: contracts.ExecutionID(execID),
					BatchNo:     batchNo,
					RowID:       id,
					BeforeData:  raw,
					AfterData:   raw,
				})
			}
		}
		db.stagedRecoveryRows[execID][batchNo] = list
		return driver.RowsAffected(int64(len(list))), nil

	case strings.HasPrefix(trimmed, "UPDATE if3110_platform.batch_recovery_rows r"):
		execID := args[0].Value.(string)
		batchNo := int(args[1].Value.(int64))
		list := db.stagedRecoveryRows[execID][batchNo]
		for i := range list {
			row := db.stagedTableRows[list[i].RowID]
			raw, _ := json.Marshal(row)
			list[i].AfterData = raw
		}
		db.stagedRecoveryRows[execID][batchNo] = list
		return driver.RowsAffected(int64(len(list))), nil

	case strings.HasPrefix(trimmed, "INSERT INTO if3110_platform.execution_commits"):
		execID := args[0].Value.(string)
		phase := args[1].Value.(string)
		batchNo := 0
		var attID, sqlText, cursorAfter, actorID string
		var affected int64
		var committedAt time.Time
		if len(args) == 9 {
			batchNo = int(args[2].Value.(int64))
			attID = args[3].Value.(string)
			sqlText = args[4].Value.(string)
			affected = args[5].Value.(int64)
			cursorAfter = args[6].Value.(string)
			actorID = args[7].Value.(string)
			committedAt = args[8].Value.(time.Time)
		} else {
			attID = args[2].Value.(string)
			sqlText = args[3].Value.(string)
			affected = args[4].Value.(int64)
			cursorAfter = args[5].Value.(string)
			actorID = args[6].Value.(string)
			committedAt = args[7].Value.(time.Time)
		}
		key := fmt.Sprintf("%s|%s|%d", execID, phase, batchNo)
		db.stagedProofs[key] = ExecutionCommitProof{
			ExecutionID:  contracts.ExecutionID(execID),
			Phase:        CommitPhase(phase),
			BatchNo:      batchNo,
			AttemptID:    contracts.AttemptID(attID),
			ExecutedSQL:  sqlText,
			AffectedRows: affected,
			CursorAfter:  cursorAfter,
			ActorID:      contracts.UserID(actorID),
			CommittedAt:  committedAt,
		}
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "UPDATE if3110_platform.batch_checkpoints") && strings.Contains(trimmed, "committed_batches = $2"):
		phase := CommitPhase(args[0].Value.(string))
		committedBatches := int(args[1].Value.(int64))
		committedRows := args[2].Value.(int64)
		lastCursor := args[3].Value.(int64)
		updatedAt := args[4].Value.(time.Time)
		execID := args[5].Value.(string)
		ckpt := db.stagedCheckpoints[execID]
		ckpt.Phase = phase
		ckpt.CommittedBatches = committedBatches
		ckpt.CommittedRows = committedRows
		ckpt.LastCursor = lastCursor
		ckpt.UpdatedAt = updatedAt
		db.stagedCheckpoints[execID] = ckpt
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "UPDATE if3110_platform.batch_checkpoints") && strings.Contains(trimmed, "compensated_batches = $2"):
		phase := CommitPhase(args[0].Value.(string))
		compBatches := int(args[1].Value.(int64))
		lastCompBatch := int(args[2].Value.(int64))
		updatedAt := args[3].Value.(time.Time)
		execID := args[4].Value.(string)
		ckpt := db.stagedCheckpoints[execID]
		ckpt.Phase = phase
		ckpt.CompensatedBatches = compBatches
		ckpt.LastCompensatedBatch = lastCompBatch
		ckpt.UpdatedAt = updatedAt
		db.stagedCheckpoints[execID] = ckpt
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "UPDATE if3110_platform.batch_checkpoints") && len(args) == 3:
		phase := CommitPhase(args[0].Value.(string))
		updatedAt := args[1].Value.(time.Time)
		execID := args[2].Value.(string)
		ckpt := db.stagedCheckpoints[execID]
		ckpt.Phase = phase
		ckpt.UpdatedAt = updatedAt
		db.stagedCheckpoints[execID] = ckpt
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "INSERT INTO if3110_platform.migration_history_v2"):
		ev := contracts.CommitHistoryEvent{
			ID:           int64(len(db.stagedHistory) + 1),
			ExecutionID:  contracts.ExecutionID(args[0].Value.(string)),
			JobID:        contracts.JobID(args[1].Value.(string)),
			RequestID:    contracts.OperationRequestID(args[2].Value.(string)),
			MigrationID:  contracts.MigrationID(args[3].Value.(string)),
			RevisionID:   contracts.RevisionID(args[4].Value.(string)),
			Version:      args[5].Value.(int64),
			Direction:    contracts.OperationDirection(args[6].Value.(string)),
			Mode:         contracts.ExecutionMode(args[7].Value.(string)),
			ExecutedSQL:  args[8].Value.(string),
			PlanChecksum: args[9].Value.(string),
			ActorID:      contracts.UserID(args[10].Value.(string)),
			HeadBefore:   contracts.ExecutionID(args[11].Value.(string)),
			HeadAfter:    contracts.ExecutionID(args[12].Value.(string)),
			CommittedAt:  args[13].Value.(time.Time),
		}
		db.stagedHistory = append(db.stagedHistory, ev)
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "UPDATE if3110_platform.database_migration_state") &&
		strings.Contains(trimmed, "unresolved_execution_id = $1"):
		targetHead := &db.head
		if db.inTx {
			targetHead = &db.stagedHead
		}
		targetHead.UnresolvedExecutionID = contracts.ExecutionID(args[0].Value.(string))
		targetHead.UnresolvedReason = contracts.GateReason(args[1].Value.(string))
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "UPDATE if3110_platform.database_migration_state") &&
		strings.Contains(trimmed, "unresolved_execution_id = ''") && len(args) == 0:
		targetHead := &db.head
		if db.inTx {
			targetHead = &db.stagedHead
		}
		targetHead.UnresolvedExecutionID = ""
		targetHead.UnresolvedReason = ""
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "UPDATE if3110_platform.database_migration_state"):
		db.stagedHead.MigrationID = contracts.MigrationID(args[0].Value.(string))
		db.stagedHead.ExecutionID = contracts.ExecutionID(args[1].Value.(string))
		db.stagedHead.Version = args[2].Value.(int64)
		db.stagedHead.UnresolvedExecutionID = ""
		db.stagedHead.UnresolvedReason = ""
		return driver.RowsAffected(1), nil

	case strings.Contains(trimmed, "FROM if3110_platform.batch_recovery_rows WHERE execution_id = $1 AND batch_no = $2"):
		// Scoped user UPDATE statement for a batch!
		execID := args[0].Value.(string)
		batchNo := int(args[1].Value.(int64))
		if db.failOnBatchNo > 0 && batchNo == db.failOnBatchNo {
			err := db.failBatchErr
			if err == nil {
				err = errors.New("simulated batch update error")
			}
			db.failOnBatchNo = 0
			return nil, err
		}
		recRows := db.stagedRecoveryRows[execID][batchNo]
		for _, r := range recRows {
			cur := db.stagedTableRows[r.RowID]
			cur.Status = "MIGRATED"
			db.stagedTableRows[r.RowID] = cur
		}
		return driver.RowsAffected(int64(len(recRows))), nil

	case strings.Contains(trimmed, "FROM if3110_platform.batch_recovery_rows br") &&
		strings.Contains(trimmed, "before_data"):
		// Compensation down SQL restoring before_data for (execution_id = $1, batch_no = $2)!
		execID := args[0].Value.(string)
		batchNo := int(args[1].Value.(int64))
		if db.failOnBatchNo > 0 && batchNo == db.failOnBatchNo {
			err := db.failBatchErr
			if err == nil {
				err = errors.New("simulated compensation error")
			}
			db.failOnBatchNo = 0
			return nil, err
		}
		recRows := db.stagedRecoveryRows[execID][batchNo]
		for _, r := range recRows {
			var before fakeBatchRowData
			_ = json.Unmarshal(r.BeforeData, &before)
			db.stagedTableRows[r.RowID] = before
		}
		db.compensatedBatchOrder = append(db.compensatedBatchOrder, batchNo)
		return driver.RowsAffected(int64(len(recRows))), nil

	default:
		return nil, fmt.Errorf("unexpected ExecContext query: %s", trimmed)
	}
}

func (c *fakeBatchEngineConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	trimmed := strings.TrimSpace(query)
	db := c.db

	switch {
	case strings.Contains(trimmed, "FROM if3110_platform.batch_scopes"):
		execID := args[0].Value.(string)
		scopesMap := db.scopes
		if db.inTx {
			scopesMap = db.stagedScopes
		}
		if sc, ok := scopesMap[execID]; ok {
			return &fakeScopeHeaderRows{records: []BatchScopeRecord{sc}}, nil
		}
		return &fakeScopeHeaderRows{}, nil

	case strings.Contains(trimmed, "COUNT(*)") && strings.Contains(trimmed, "FROM if3110_platform.batch_scope_rows"):
		execID := args[0].Value.(string)
		rows := db.stagedScopeRows[execID]
		var total, minPK, maxPK int64
		total = int64(len(rows))
		if total > 0 {
			minPK = rows[0]
			maxPK = rows[len(rows)-1]
		}
		return &fakeScopeAggRows{total: total, minPK: minPK, maxPK: maxPK}, nil

	case strings.Contains(trimmed, "SELECT row_id") && strings.Contains(trimmed, "FROM if3110_platform.batch_scope_rows"):
		execID := args[0].Value.(string)
		lastCursor := args[1].Value.(int64)
		limit := int(args[2].Value.(int64))
		var selected []int64
		for _, id := range db.scopeRows[execID] {
			if id > lastCursor {
				selected = append(selected, id)
				if len(selected) == limit {
					break
				}
			}
		}
		return &fakeInt64Rows{vals: selected}, nil

	case strings.Contains(trimmed, "FROM if3110_platform.batch_checkpoints"):
		execID := args[0].Value.(string)
		ckpts := db.checkpoints
		if db.inTx {
			ckpts = db.stagedCheckpoints
		}
		if ckpt, ok := ckpts[execID]; ok {
			return &fakeCheckpointRows{records: []BatchCheckpointRecord{ckpt}}, nil
		}
		return &fakeCheckpointRows{}, nil

	case strings.Contains(trimmed, "FROM if3110_platform.execution_commits"):
		execID := args[0].Value.(string)
		phase := args[1].Value.(string)
		batchNo := int(args[2].Value.(int64))
		key := fmt.Sprintf("%s|%s|%d", execID, phase, batchNo)
		proofs := db.proofs
		if db.inTx {
			proofs = db.stagedProofs
		}
		if proof, ok := proofs[key]; ok {
			return &fakeCommitProofRows{proofs: []ExecutionCommitProof{proof}}, nil
		}
		return &fakeCommitProofRows{}, nil

	case strings.Contains(trimmed, "FROM if3110_platform.database_migration_state"):
		head := db.head
		if db.inTx {
			head = db.stagedHead
		}
		return &fakeTargetStateRows{head: head}, nil

	case strings.Contains(trimmed, "FROM if3110_platform.batch_recovery_rows r") &&
		strings.Contains(trimmed, "LEFT JOIN"):
		execID := args[0].Value.(string)
		batchNo := int(args[1].Value.(int64))
		recRows := db.stagedRecoveryRows[execID][batchNo]
		var verifyRows []fakeCompVerifyRow
		for _, r := range recRows {
			cur, exists := db.stagedTableRows[r.RowID]
			var curJSON []byte
			if exists {
				curJSON, _ = json.Marshal(cur)
			}
			verifyRows = append(verifyRows, fakeCompVerifyRow{
				rowID:       r.RowID,
				beforeData:  r.BeforeData,
				afterData:   r.AfterData,
				currentData: curJSON,
				exists:      exists,
			})
		}
		return &fakeCompVerifyRows{rows: verifyRows}, nil

	default:
		return nil, fmt.Errorf("unexpected QueryContext query: %s", trimmed)
	}
}

type fakeInt64Rows struct {
	vals []int64
	idx  int
}

func (*fakeInt64Rows) Columns() []string { return []string{"row_id"} }
func (*fakeInt64Rows) Close() error      { return nil }
func (r *fakeInt64Rows) Next(dest []driver.Value) error {
	if r.idx >= len(r.vals) {
		return io.EOF
	}
	dest[0] = r.vals[r.idx]
	r.idx++
	return nil
}

type fakeCompVerifyRow struct {
	rowID       int64
	beforeData  []byte
	afterData   []byte
	currentData []byte
	exists      bool
}

type fakeCompVerifyRows struct {
	rows []fakeCompVerifyRow
	idx  int
}

func (*fakeCompVerifyRows) Columns() []string {
	return []string{"row_id", "before_data", "after_data", "current_data"}
}
func (*fakeCompVerifyRows) Close() error { return nil }
func (r *fakeCompVerifyRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	item := r.rows[r.idx]
	r.idx++
	dest[0] = item.rowID
	dest[1] = item.beforeData
	dest[2] = item.afterData
	if item.exists {
		dest[3] = item.currentData
	} else {
		dest[3] = nil
	}
	return nil
}

type progressSnapshot struct {
	batches int
	rows    int64
	total   int64
	cursor  string
}

type recordingProgressUpdater struct {
	snapshots []progressSnapshot
}

func (u *recordingProgressUpdater) UpdateProgress(
	_ context.Context,
	_ contracts.JobID,
	_ string,
	committedBatches int,
	committedRows int64,
	totalScopeRows int64,
	lastCursor string,
) error {
	u.snapshots = append(u.snapshots, progressSnapshot{
		batches: committedBatches,
		rows:    committedRows,
		total:   totalScopeRows,
		cursor:  lastCursor,
	})
	return nil
}

func sampleBatchPlan(direction contracts.OperationDirection) contracts.ExecutionPlan {
	p := contracts.ExecutionPlan{
		RequestID:                "req-batch-1",
		ExecutionID:              "exec-batch-1",
		MigrationID:              "mig-batch-1",
		RevisionID:               "rev-batch-1",
		Version:                  1,
		Direction:                direction,
		Mode:                     contracts.ModeBatch,
		ActorID:                  "user-1",
		TargetDatabaseID:         "db-1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "",
		PolicyVersion:            1,
		UpSQL:                    "UPDATE public.orders SET status = 'MIGRATED' WHERE status = 'PENDING';",
		DownSQL:                  "UPDATE public.orders SET status = 'PENDING';",
		Batch: contracts.BatchConfig{
			TableSchema:      "public",
			TableName:        "orders",
			PrimaryKeyColumn: "id",
			ScopeWhere:       "status = 'PENDING'",
			BatchSize:        2,
		},
	}
	if direction == contracts.DirectionResume || direction == contracts.DirectionCompensate {
		p.OriginExecutionID = "exec-batch-1"
		p.ExecutionID = "exec-batch-recovery"
	}
	p.PlanChecksum = p.ComputeChecksum()
	return p
}

func TestExecuteBatchUpProcessesKeysetBatchesAndUpdatesProgress(t *testing.T) {
	engine := newFakeBatchEngineDB(map[int64]string{
		10: "PENDING",
		20: "PENDING",
		30: "PENDING",
		40: "PENDING",
		50: "PENDING",
	})
	sqlDB := sql.OpenDB(fakeBatchEngineConnector{db: engine})
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	gates := newMemoryGateStore()
	progress := &recordingProgressUpdater{}
	batchExec := NewBatchExecutor(gates, progress)

	plan := sampleBatchPlan(contracts.DirectionApply)
	env := buildExecutionEnv(conn, plan, "job-batch-1", "att-batch-1")
	env.Claimed.Job.LeaseToken = "lease-valid-1"

	res, err := batchExec.ExecuteBatchUp(ctx, env)
	if err != nil {
		t.Fatalf("ExecuteBatchUp() error = %v", err)
	}
	if res.JobStatus != contracts.JobStatusSuccess || res.AttemptStatus != contracts.AttemptStatusCommitted {
		t.Fatalf("ExecuteBatchUp() result = %+v, want SUCCESS/COMMITTED", res)
	}
	if res.CommittedBatches != 3 || res.CommittedRows != 5 || res.TotalScopeRows != 5 || res.LastCommittedCursor != "50" {
		t.Fatalf("unexpected batch counters: %+v", res)
	}

	// Verify real-time progress updates after each of the 3 batches:
	// Batch 1 -> (1, 2, 5, "20")
	// Batch 2 -> (2, 4, 5, "40")
	// Batch 3 -> (3, 5, 5, "50")
	if len(progress.snapshots) != 3 {
		t.Fatalf("len(progress.snapshots) = %d, want 3", len(progress.snapshots))
	}
	wantCursors := []string{"20", "40", "50"}
	wantRows := []int64{2, 4, 5}
	for i, snap := range progress.snapshots {
		if snap.batches != i+1 || snap.rows != wantRows[i] || snap.cursor != wantCursors[i] {
			t.Fatalf("progress snapshot[%d] = %+v, want batches=%d rows=%d cursor=%s",
				i, snap, i+1, wantRows[i], wantCursors[i])
		}
	}

	// Verify all 5 rows in the target table were updated and before/after images were stored per batch!
	for _, id := range []int64{10, 20, 30, 40, 50} {
		if engine.tableRows[id].Status != "MIGRATED" {
			t.Fatalf("row %d status = %q, want MIGRATED", id, engine.tableRows[id].Status)
		}
	}
	if len(engine.recoveryRows["exec-batch-1"]) != 3 {
		t.Fatalf("expected recovery rows for 3 batches, got %d", len(engine.recoveryRows["exec-batch-1"]))
	}

	// Verify finalization advanced active_head and wrote migration_history_v2!
	if engine.head.ExecutionID != "exec-batch-1" || engine.head.Version != 1 || len(engine.history) != 1 {
		t.Fatalf("expected finalized active_head=exec-batch-1 and 1 history row, got head=%+v hist=%d",
			engine.head, len(engine.history))
	}
}

func TestBuildScopedBatchUpdateSQL(t *testing.T) {
	cfg := contracts.BatchConfig{
		TableSchema:      "public",
		TableName:        "orders",
		PrimaryKeyColumn: "id",
		BatchSize:        50,
	}

	withWhere, err := BuildScopedBatchUpdateSQL("UPDATE public.orders SET status = 'DONE' WHERE status = 'PENDING';", cfg)
	if err != nil {
		t.Fatalf("BuildScopedBatchUpdateSQL(withWhere) error = %v", err)
	}
	if !strings.Contains(withWhere, "WHERE (status = 'PENDING') AND \"id\"::bigint IN (SELECT row_id FROM if3110_platform.batch_recovery_rows WHERE execution_id = $1 AND batch_no = $2)") {
		t.Fatalf("unexpected rewritten SQL with WHERE: %s", withWhere)
	}

	withoutWhere, err := BuildScopedBatchUpdateSQL("UPDATE public.orders SET status = 'DONE'", cfg)
	if err != nil {
		t.Fatalf("BuildScopedBatchUpdateSQL(withoutWhere) error = %v", err)
	}
	if !strings.Contains(withoutWhere, "WHERE \"id\"::bigint IN (SELECT row_id FROM if3110_platform.batch_recovery_rows WHERE execution_id = $1 AND batch_no = $2)") {
		t.Fatalf("unexpected rewritten SQL without WHERE: %s", withoutWhere)
	}
	if _, err := BuildScopedBatchUpdateSQL("UPDATE public.payments SET status = 'DONE'", cfg); !errors.Is(err, ErrInvalidBatchUpdateSQL) {
		t.Fatalf("mismatched update target error = %v, want ErrInvalidBatchUpdateSQL", err)
	}
	for _, matching := range []string{
		`UPDATE "public"."orders" AS o SET status = 'DONE'`,
		"UPDATE ONLY public.orders SET status = 'DONE'",
	} {
		if _, err := BuildScopedBatchUpdateSQL(matching, cfg); err != nil {
			t.Fatalf("matching update target %q rejected: %v", matching, err)
		}
	}

	// Non-UPDATE or multi-statement SQL must be rejected.
	for _, bad := range []string{
		"DELETE FROM public.orders WHERE id = 1",
		"UPDATE public.orders SET a = 1; UPDATE public.orders SET b = 2",
	} {
		if _, err := BuildScopedBatchUpdateSQL(bad, cfg); !errors.Is(err, ErrInvalidBatchUpdateSQL) {
			t.Fatalf("BuildScopedBatchUpdateSQL(%q) = %v, want ErrInvalidBatchUpdateSQL", strconv.Quote(bad), err)
		}
	}
}
