package jobs

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/contracts"
)

type fakeJobsDB struct {
	mu              sync.Mutex
	rows            []fakeJobRecord
	attempts        []fakeAttemptRecord
	gates           map[string]string
	operationStatus string
	definitionState string
}

type fakeJobRecord struct {
	id                  string
	requestID           string
	executionID         string
	databaseID          string
	connectionRevision  int64
	direction           string
	mode                string
	status              string
	actorID             string
	idempotencyKey      string
	idempotencyHash     string
	planPayload         []byte
	traceCarrier        []byte
	availableAt         time.Time
	leaseOwner          string
	leaseToken          string
	leaseExpiresAt      any
	retryCount          int64
	maxRetries          int64
	committedBatches    int64
	committedRows       int64
	totalScopeRows      int64
	lastCommittedCursor string
	lastErrorCode       string
	lastErrorMessage    string
	createdAt           time.Time
	updatedAt           time.Time
	completedAt         any
}

type fakeAttemptRecord struct {
	id           string
	jobID        string
	executionID  string
	attemptNo    int64
	workerID     string
	status       string
	executedSQL  string
	errorCode    string
	errorMessage string
	startedAt    time.Time
	finishedAt   any
}

type fakeJobsConnector struct {
	db *fakeJobsDB
}

func (c fakeJobsConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeJobsConn{db: c.db}, nil
}

func (fakeJobsConnector) Driver() driver.Driver { return fakeJobsDriver{} }

type fakeJobsDriver struct{}

func (fakeJobsDriver) Open(string) (driver.Conn, error) { return nil, errors.New("unused") }

type fakeJobsConn struct {
	db                    *fakeJobsDB
	inTx                  bool
	staged                []fakeJobRecord
	stagedAttempts        []fakeAttemptRecord
	stagedOperationStatus string
	stagedDefinitionState string
}

func (c *fakeJobsConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *fakeJobsConn) Close() error                        { return nil }

func (c *fakeJobsConn) Begin() (driver.Tx, error) {
	c.db.mu.Lock()
	c.inTx = true
	c.staged = append([]fakeJobRecord(nil), c.db.rows...)
	c.stagedAttempts = append([]fakeAttemptRecord(nil), c.db.attempts...)
	c.stagedOperationStatus = c.db.operationStatus
	c.stagedDefinitionState = c.db.definitionState
	return &fakeJobsTx{conn: c}, nil
}

type fakeJobsTx struct {
	conn *fakeJobsConn
}

func (tx *fakeJobsTx) Commit() error {
	if tx.conn.inTx {
		tx.conn.db.rows = tx.conn.staged
		tx.conn.db.attempts = tx.conn.stagedAttempts
		tx.conn.db.operationStatus = tx.conn.stagedOperationStatus
		tx.conn.db.definitionState = tx.conn.stagedDefinitionState
		tx.conn.staged = nil
		tx.conn.stagedAttempts = nil
		tx.conn.stagedOperationStatus = ""
		tx.conn.stagedDefinitionState = ""
		tx.conn.inTx = false
		tx.conn.db.mu.Unlock()
	}
	return nil
}

func (tx *fakeJobsTx) Rollback() error {
	if tx.conn.inTx {
		tx.conn.staged = nil
		tx.conn.stagedAttempts = nil
		tx.conn.stagedOperationStatus = ""
		tx.conn.stagedDefinitionState = ""
		tx.conn.inTx = false
		tx.conn.db.mu.Unlock()
	}
	return nil
}

func (c *fakeJobsConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	trimmed := strings.TrimSpace(query)
	switch {
	case strings.HasPrefix(trimmed, "UPDATE operation_requests SET status='QUEUED'"):
		c.stagedOperationStatus = "QUEUED"
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "UPDATE migration_definitions SET state='LOCKED'"):
		c.stagedDefinitionState = "LOCKED"
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(trimmed, "SELECT pg_advisory_xact_lock"):
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(trimmed, "UPDATE job_attempts") && strings.Contains(trimmed, "LEASE_EXPIRED"):
		newStatus := args[0].Value.(string)
		finishedAt := args[1].Value.(time.Time)
		jobID := args[2].Value.(string)
		oldStatus := args[3].Value.(string)
		var affected int64
		for i := range c.stagedAttempts {
			if c.stagedAttempts[i].jobID == jobID && c.stagedAttempts[i].status == oldStatus {
				c.stagedAttempts[i].status = newStatus
				if c.stagedAttempts[i].errorCode == "" {
					c.stagedAttempts[i].errorCode = "LEASE_EXPIRED"
				}
				if c.stagedAttempts[i].errorMessage == "" {
					c.stagedAttempts[i].errorMessage = "worker lease expired before attempt completion"
				}
				c.stagedAttempts[i].finishedAt = finishedAt
				affected++
			}
		}
		return driver.RowsAffected(affected), nil

	case strings.HasPrefix(trimmed, "UPDATE jobs") && strings.Contains(trimmed, "SET committed_batches = $1"):
		committedBatches := args[0].Value.(int64)
		committedRows := args[1].Value.(int64)
		totalScopeRows := args[2].Value.(int64)
		lastCursor := args[3].Value.(string)
		now := args[4].Value.(time.Time)
		jobID := args[5].Value.(string)
		leaseToken := args[6].Value.(string)
		expectedStatus := args[7].Value.(string)

		c.db.mu.Lock()
		defer c.db.mu.Unlock()
		for i := range c.db.rows {
			if c.db.rows[i].id == jobID && c.db.rows[i].leaseToken == leaseToken && c.db.rows[i].status == expectedStatus {
				c.db.rows[i].committedBatches = committedBatches
				c.db.rows[i].committedRows = committedRows
				c.db.rows[i].totalScopeRows = totalScopeRows
				c.db.rows[i].lastCommittedCursor = lastCursor
				c.db.rows[i].updatedAt = now
				return driver.RowsAffected(1), nil
			}
		}
		return driver.RowsAffected(0), nil

	default:
		return nil, fmt.Errorf("unexpected ExecContext query: %s", trimmed)
	}
}

func (c *fakeJobsConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	trimmed := strings.TrimSpace(query)
	switch {
	case strings.HasPrefix(trimmed, "SELECT status,database_id::text,migration_id::text,revision,direction,expected_head"):
		return &fakeValueRows{columns: []string{"status", "database_id", "migration_id", "revision", "direction", "expected_head", "origin_execution_id", "connection_revision", "policy_version", "revision_checksum", "checksum", "review_round", "created_by"}, values: []driver.Value{"APPROVED", args[1].Value.(string), args[2].Value.(string), args[3].Value, args[4].Value.(string), args[5].Value.(string), args[6].Value.(string), args[7].Value, args[8].Value, strings.Repeat("a", 64), strings.Repeat("b", 64), int64(1), args[9].Value.(string)}}, nil

	case strings.HasPrefix(trimmed, "SELECT version FROM platform_policy_version"):
		return &fakeSingleValRows{col: "version", val: int64(1)}, nil

	case strings.HasPrefix(trimmed, "SELECT revision FROM database_connections WHERE id=$1 FOR SHARE"):
		return &fakeSingleValRows{col: "revision", val: int64(2)}, nil

	case strings.HasPrefix(trimmed, "SELECT d.current_revision,d.state,r.checksum"):
		state := "LOCKED"
		if args[2].Value.(string) == "APPLY" {
			state = "IN_REVIEW"
		}
		return &fakeValueRows{columns: []string{"current_revision", "state", "checksum"}, values: []driver.Value{int64(1), state, strings.Repeat("a", 64)}}, nil

	case strings.HasPrefix(trimmed, "SELECT count(*),count(*) FILTER"):
		return &fakeValueRows{columns: []string{"count", "count"}, values: []driver.Value{int64(1), int64(1)}}, nil

	case strings.HasPrefix(trimmed, "SELECT to_regclass('database_execution_gates')"):
		if len(c.db.gates) > 0 {
			return &fakeSingleValRows{col: "to_regclass", val: "database_execution_gates"}, nil
		}
		return &fakeSingleValRows{col: "to_regclass", val: nil}, nil

	case strings.Contains(trimmed, "FROM jobs j") && strings.Contains(trimmed, "FOR UPDATE SKIP LOCKED"):
		now := args[0].Value.(time.Time)
		activeDBs := make(map[string]bool)
		for _, r := range c.staged {
			if r.status == string(contracts.JobStatusRunning) {
				if exp, ok := r.leaseExpiresAt.(time.Time); ok && !exp.Before(now) {
					activeDBs[r.databaseID] = true
				}
			}
		}

		var candidates []fakeJobRecord
		for _, r := range c.staged {
			duePending := (r.status == string(contracts.JobStatusPending) || r.status == string(contracts.JobStatusRetryWait)) && !r.availableAt.After(now)
			expiredRunning := false
			if r.status == string(contracts.JobStatusRunning) && r.retryCount < r.maxRetries {
				if exp, ok := r.leaseExpiresAt.(time.Time); ok && exp.Before(now) {
					expiredRunning = true
				}
			}
			if !duePending && !expiredRunning {
				continue
			}
			if activeDBs[r.databaseID] {
				continue
			}
			if reason, gated := c.db.gates[r.databaseID]; gated {
				isRecovery := (r.direction == string(contracts.DirectionResume) || r.direction == string(contracts.DirectionCompensate)) &&
					(reason == string(contracts.GateReasonPartial) || reason == string(contracts.GateReasonPartiallyReverted))
				if !isRecovery {
					continue
				}
			}
			candidates = append(candidates, r)
		}

		sort.Slice(candidates, func(i, j int) bool {
			if !candidates[i].availableAt.Equal(candidates[j].availableAt) {
				return candidates[i].availableAt.Before(candidates[j].availableAt)
			}
			return candidates[i].createdAt.Before(candidates[j].createdAt)
		})
		if len(candidates) > 1 {
			candidates = candidates[:1]
		}
		return &fakeJobRows{records: candidates}, nil

	case strings.HasPrefix(trimmed, "SELECT COALESCE(MAX(attempt_no), 0) FROM job_attempts"):
		jobID := args[0].Value.(string)
		var maxNo int64
		for _, a := range c.stagedAttempts {
			if a.jobID == jobID && a.attemptNo > maxNo {
				maxNo = a.attemptNo
			}
		}
		return &fakeSingleValRows{col: "coalesce", val: maxNo}, nil

	case strings.HasPrefix(trimmed, "UPDATE jobs") && strings.Contains(trimmed, "SET status = $1") && strings.Contains(trimmed, "lease_owner = $2"):
		status := args[0].Value.(string)
		leaseOwner := args[1].Value.(string)
		leaseToken := args[2].Value.(string)
		leaseExpiresAt := args[3].Value.(time.Time)
		retryCount := args[4].Value.(int64)
		now := args[5].Value.(time.Time)
		jobID := args[6].Value.(string)

		for i := range c.staged {
			if c.staged[i].id == jobID {
				c.staged[i].status = status
				c.staged[i].leaseOwner = leaseOwner
				c.staged[i].leaseToken = leaseToken
				c.staged[i].leaseExpiresAt = leaseExpiresAt
				c.staged[i].retryCount = retryCount
				c.staged[i].updatedAt = now
				return &fakeJobRows{records: []fakeJobRecord{c.staged[i]}}, nil
			}
		}
		return &fakeJobRows{}, nil

	case strings.HasPrefix(trimmed, "INSERT INTO job_attempts"):
		rec := fakeAttemptRecord{
			id:          args[0].Value.(string),
			jobID:       args[1].Value.(string),
			executionID: args[2].Value.(string),
			attemptNo:   args[3].Value.(int64),
			workerID:    args[4].Value.(string),
			status:      args[5].Value.(string),
			startedAt:   args[6].Value.(time.Time),
		}
		c.stagedAttempts = append(c.stagedAttempts, rec)
		return &fakeAttemptRows{records: []fakeAttemptRecord{rec}}, nil

	case strings.HasPrefix(trimmed, "UPDATE jobs") && strings.Contains(trimmed, "RETURNING lease_expires_at"):
		newExpiry := args[0].Value.(time.Time)
		now := args[1].Value.(time.Time)
		jobID := args[2].Value.(string)
		leaseToken := args[3].Value.(string)
		expectedStatus := args[4].Value.(string)

		c.db.mu.Lock()
		defer c.db.mu.Unlock()
		for i := range c.db.rows {
			if c.db.rows[i].id == jobID && c.db.rows[i].leaseToken == leaseToken && c.db.rows[i].status == expectedStatus {
				c.db.rows[i].leaseExpiresAt = newExpiry
				c.db.rows[i].updatedAt = now
				return &fakeSingleValRows{col: "lease_expires_at", val: newExpiry}, nil
			}
		}
		return &fakeSingleValRows{empty: true}, nil

	case strings.Contains(trimmed, "FROM jobs WHERE id = $1 AND lease_token = $2 AND status = $3 FOR UPDATE"):
		jobID := args[0].Value.(string)
		leaseToken := args[1].Value.(string)
		status := args[2].Value.(string)
		for _, r := range c.staged {
			if r.id == jobID && r.leaseToken == leaseToken && r.status == status {
				return &fakeJobRows{records: []fakeJobRecord{r}}, nil
			}
		}
		return &fakeJobRows{}, nil

	case strings.HasPrefix(trimmed, "UPDATE jobs") && strings.Contains(trimmed, "committed_batches = $2"):
		status := args[0].Value.(string)
		committedBatches := args[1].Value.(int64)
		committedRows := args[2].Value.(int64)
		totalScopeRows := args[3].Value.(int64)
		lastCursor := args[4].Value.(string)
		errCode := args[5].Value.(string)
		errMsg := args[6].Value.(string)
		now := args[7].Value.(time.Time)
		completedAt := args[8].Value
		jobID := args[9].Value.(string)
		leaseToken := args[10].Value.(string)
		expectedStatus := args[11].Value.(string)

		for i := range c.staged {
			if c.staged[i].id == jobID && c.staged[i].leaseToken == leaseToken && c.staged[i].status == expectedStatus {
				c.staged[i].status = status
				c.staged[i].leaseOwner = ""
				c.staged[i].leaseToken = ""
				c.staged[i].leaseExpiresAt = nil
				c.staged[i].committedBatches = committedBatches
				c.staged[i].committedRows = committedRows
				c.staged[i].totalScopeRows = totalScopeRows
				c.staged[i].lastCommittedCursor = lastCursor
				c.staged[i].lastErrorCode = errCode
				c.staged[i].lastErrorMessage = errMsg
				c.staged[i].updatedAt = now
				c.staged[i].completedAt = completedAt
				return &fakeJobRows{records: []fakeJobRecord{c.staged[i]}}, nil
			}
		}
		return &fakeJobRows{}, nil

	case strings.HasPrefix(trimmed, "UPDATE jobs") && strings.Contains(trimmed, "retry_count = $2"):
		status := args[0].Value.(string)
		retryCount := args[1].Value.(int64)
		availableAt := args[2].Value.(time.Time)
		errCode := args[3].Value.(string)
		errMsg := args[4].Value.(string)
		now := args[5].Value.(time.Time)
		completedAt := args[6].Value
		jobID := args[7].Value.(string)
		leaseToken := args[8].Value.(string)
		expectedStatus := args[9].Value.(string)

		for i := range c.staged {
			if c.staged[i].id == jobID && c.staged[i].leaseToken == leaseToken && c.staged[i].status == expectedStatus {
				c.staged[i].status = status
				c.staged[i].leaseOwner = ""
				c.staged[i].leaseToken = ""
				c.staged[i].leaseExpiresAt = nil
				c.staged[i].retryCount = retryCount
				c.staged[i].availableAt = availableAt
				c.staged[i].lastErrorCode = errCode
				c.staged[i].lastErrorMessage = errMsg
				c.staged[i].updatedAt = now
				c.staged[i].completedAt = completedAt
				return &fakeJobRows{records: []fakeJobRecord{c.staged[i]}}, nil
			}
		}
		return &fakeJobRows{}, nil

	case strings.HasPrefix(trimmed, "UPDATE job_attempts") && strings.Contains(trimmed, "WHERE id = $6"):
		status := args[0].Value.(string)
		executedSQL := args[1].Value.(string)
		errCode := args[2].Value.(string)
		errMsg := args[3].Value.(string)
		finishedAt := args[4].Value.(time.Time)
		attemptID := args[5].Value.(string)
		jobID := args[6].Value.(string)

		for i := range c.stagedAttempts {
			if c.stagedAttempts[i].id == attemptID && c.stagedAttempts[i].jobID == jobID {
				c.stagedAttempts[i].status = status
				c.stagedAttempts[i].executedSQL = executedSQL
				c.stagedAttempts[i].errorCode = errCode
				c.stagedAttempts[i].errorMessage = errMsg
				c.stagedAttempts[i].finishedAt = finishedAt
				return &fakeAttemptRows{records: []fakeAttemptRecord{c.stagedAttempts[i]}}, nil
			}
		}
		return &fakeAttemptRows{}, nil

	case strings.Contains(trimmed, "FROM jobs") && strings.Contains(trimmed, "FOR UPDATE"):
		actorID := args[0].Value.(string)
		databaseID := args[1].Value.(string)
		idempotencyKey := args[2].Value.(string)
		requestID := args[3].Value.(string)
		executionID := args[4].Value.(string)

		var matches []fakeJobRecord
		for _, r := range c.staged {
			if r.actorID == actorID && r.databaseID == databaseID && r.idempotencyKey == idempotencyKey {
				matches = append([]fakeJobRecord{r}, matches...)
			} else if r.requestID == requestID || r.executionID == executionID {
				matches = append(matches, r)
			}
		}
		if len(matches) > 1 {
			matches = matches[:1]
		}
		return &fakeJobRows{records: matches}, nil

	case strings.HasPrefix(trimmed, "INSERT INTO jobs"):
		rec := fakeJobRecord{
			id:                 args[0].Value.(string),
			requestID:          args[1].Value.(string),
			executionID:        args[2].Value.(string),
			databaseID:         args[3].Value.(string),
			connectionRevision: args[4].Value.(int64),
			direction:          args[5].Value.(string),
			mode:               args[6].Value.(string),
			status:             args[7].Value.(string),
			actorID:            args[8].Value.(string),
			idempotencyKey:     args[9].Value.(string),
			idempotencyHash:    args[10].Value.(string),
			planPayload:        []byte(args[11].Value.(string)),
			traceCarrier:       []byte(args[12].Value.(string)),
			availableAt:        args[13].Value.(time.Time),
			retryCount:         0,
			maxRetries:         args[14].Value.(int64),
			createdAt:          args[15].Value.(time.Time),
			updatedAt:          args[15].Value.(time.Time),
		}
		c.staged = append(c.staged, rec)
		return &fakeJobRows{records: []fakeJobRecord{rec}}, nil

	case strings.Contains(trimmed, "FROM jobs WHERE id = $1"):
		targetID := args[0].Value.(string)
		c.db.mu.Lock()
		defer c.db.mu.Unlock()
		for _, r := range c.db.rows {
			if r.id == targetID {
				return &fakeJobRows{records: []fakeJobRecord{r}}, nil
			}
		}
		return &fakeJobRows{}, nil

	default:
		return nil, fmt.Errorf("unexpected QueryContext query: %s", trimmed)
	}
}

type fakeSingleValRows struct {
	col   string
	val   any
	empty bool
	done  bool
}

type fakeValueRows struct {
	columns []string
	values  []driver.Value
	done    bool
}

func (r *fakeValueRows) Columns() []string { return r.columns }
func (*fakeValueRows) Close() error        { return nil }
func (r *fakeValueRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.values)
	return nil
}

func (r *fakeSingleValRows) Columns() []string { return []string{r.col} }
func (r *fakeSingleValRows) Close() error      { return nil }
func (r *fakeSingleValRows) Next(dest []driver.Value) error {
	if r.empty || r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.val
	return nil
}

type fakeAttemptRows struct {
	records []fakeAttemptRecord
	idx     int
}

func (*fakeAttemptRows) Columns() []string {
	return []string{
		"id", "job_id", "execution_id", "attempt_no", "worker_id",
		"status", "executed_sql", "error_code", "error_message", "started_at", "finished_at",
	}
}

func (*fakeAttemptRows) Close() error { return nil }

func (r *fakeAttemptRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.records) {
		return io.EOF
	}
	rec := r.records[r.idx]
	r.idx++
	dest[0] = rec.id
	dest[1] = rec.jobID
	dest[2] = rec.executionID
	dest[3] = rec.attemptNo
	dest[4] = rec.workerID
	dest[5] = rec.status
	dest[6] = rec.executedSQL
	dest[7] = rec.errorCode
	dest[8] = rec.errorMessage
	dest[9] = rec.startedAt
	dest[10] = rec.finishedAt
	return nil
}

type fakeJobRows struct {
	records []fakeJobRecord
	idx     int
}

func (*fakeJobRows) Columns() []string {
	return []string{
		"id", "request_id", "execution_id", "database_id", "connection_revision",
		"direction", "mode", "status", "actor_id", "idempotency_key", "idempotency_hash",
		"plan_payload", "trace_carrier", "available_at", "lease_owner", "lease_token",
		"lease_expires_at", "retry_count", "max_retries", "committed_batches",
		"committed_rows", "total_scope_rows", "last_committed_cursor",
		"last_error_code", "last_error_message", "created_at", "updated_at", "completed_at",
	}
}

func (*fakeJobRows) Close() error { return nil }

func (r *fakeJobRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.records) {
		return io.EOF
	}
	rec := r.records[r.idx]
	r.idx++
	dest[0] = rec.id
	dest[1] = rec.requestID
	dest[2] = rec.executionID
	dest[3] = rec.databaseID
	dest[4] = rec.connectionRevision
	dest[5] = rec.direction
	dest[6] = rec.mode
	dest[7] = rec.status
	dest[8] = rec.actorID
	dest[9] = rec.idempotencyKey
	dest[10] = rec.idempotencyHash
	dest[11] = rec.planPayload
	dest[12] = rec.traceCarrier
	dest[13] = rec.availableAt
	dest[14] = rec.leaseOwner
	dest[15] = rec.leaseToken
	dest[16] = rec.leaseExpiresAt
	dest[17] = rec.retryCount
	dest[18] = rec.maxRetries
	dest[19] = rec.committedBatches
	dest[20] = rec.committedRows
	dest[21] = rec.totalScopeRows
	dest[22] = rec.lastCommittedCursor
	dest[23] = rec.lastErrorCode
	dest[24] = rec.lastErrorMessage
	dest[25] = rec.createdAt
	dest[26] = rec.updatedAt
	dest[27] = rec.completedAt
	return nil
}

func sampleExecutionPlan() contracts.ExecutionPlan {
	plan := contracts.ExecutionPlan{
		RequestID:                "req-100",
		ExecutionID:              "exec-100",
		MigrationID:              "mig-100",
		RevisionID:               "mig-100:1",
		Version:                  1,
		Direction:                contracts.DirectionApply,
		Mode:                     contracts.ModeNormal,
		ActorID:                  "user-42",
		TargetDatabaseID:         "db-1",
		TargetConnectionRevision: 2,
		ExpectedActiveHead:       "",
		PolicyVersion:            1,
		UpSQL:                    "CREATE TABLE invoices (id BIGINT PRIMARY KEY);",
		DownSQL:                  "DROP TABLE invoices;",
	}
	plan.PlanChecksum = plan.ComputeChecksum()
	return plan
}

func seedApprovedQueueRequest(t *testing.T, ctx context.Context, db *sql.DB, plan contracts.ExecutionPlan) contracts.ExecutionPlan {
	t.Helper()
	var actorID, databaseID, groupID, migrationID, policyVersion int64
	if err := db.QueryRowContext(ctx, `INSERT INTO users(username,password_hash) VALUES($1,'hash') RETURNING id`, "job-test-"+time.Now().Format("150405.000000000")).Scan(&actorID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO database_connections(name,host,port,database_name,username,ssl_mode,password_ciphertext) VALUES($1,'localhost',5432,'target','test','disable',$2) RETURNING id`, "job-target-"+time.Now().Format("150405.000000000"), make([]byte, 29)).Scan(&databaseID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO groups(name) VALUES($1) RETURNING id`, "job-group-"+time.Now().Format("150405.000000000")).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO group_memberships(group_id,user_id) VALUES($1,$2)`, groupID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE groups SET leader_user_id=$2 WHERE id=$1`, groupID, actorID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT version FROM platform_policy_version WHERE id=true`).Scan(&policyVersion); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO migration_definitions(database_id,connection_revision,version,title,state,created_by) VALUES($1,1,1,'job test','DRAFT',$2) RETURNING id`, databaseID, actorID).Scan(&migrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO migration_revisions(migration_id,revision,up_sql,down_sql,irreversible,reason,mode,batch_plan,checksum) VALUES($1,1,$2,'SELECT 2',false,'','NORMAL','{}',repeat('a',64))`, migrationID, plan.UpSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migration_definitions SET state='IN_REVIEW' WHERE id=$1`, migrationID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var requestID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO operation_requests(database_id,migration_id,revision,direction,expected_head,origin_execution_id,connection_revision,policy_version,revision_checksum,checksum,status,review_round,created_by) VALUES($1,$2,1,$3,$4,$5,1,$6,repeat('a',64),repeat('b',64),'APPROVED',1,$7) RETURNING id`, databaseID, migrationID, plan.Direction, plan.ExpectedActiveHead, plan.OriginExecutionID, policyVersion, actorID).Scan(&requestID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO approval_requirements(request_id,group_id,leader_user_id,grant_level) VALUES($1,$2,$3,2)`, requestID, groupID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO approval_votes(request_id,review_round,group_id,migration_revision,checksum,actor_id,decision) VALUES($1,1,$2,1,repeat('b',64),$3,'APPROVE')`, requestID, groupID, actorID); err != nil {
		t.Fatal(err)
	}
	plan.RequestID = contracts.OperationRequestID(fmt.Sprint(requestID))
	plan.MigrationID = contracts.MigrationID(fmt.Sprint(migrationID))
	plan.RevisionID = contracts.RevisionID(fmt.Sprintf("%d:1", migrationID))
	plan.TargetDatabaseID = contracts.DatabaseID(fmt.Sprint(databaseID))
	plan.ActorID = contracts.UserID(fmt.Sprint(actorID))
	plan.TargetConnectionRevision = 1
	plan.PolicyVersion = policyVersion
	plan.PlanChecksum = plan.ComputeChecksum()
	return plan
}

// TestPublishAtomicAndIdempotency verifies QUEUE-01:
// 1. Atomic publish persists a PENDING job with sealed plan & W3C trace carrier that survives store restart.
// 2. Duplicate publish with the same (actor_id, database_id, idempotency_key) and identical payload returns the existing job without creating a duplicate row.
// 3. Duplicate publish with the same idempotency_key or request_id but different payload returns ErrIdempotencyConflict.
func TestPublishAtomicAndIdempotency(t *testing.T) {
	backingDB := &fakeJobsDB{}
	sqlDB := sql.OpenDB(fakeJobsConnector{db: backingDB})
	t.Cleanup(func() { _ = sqlDB.Close() })

	fixedNow := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	seq := 0
	store := NewPostgresJobStore(sqlDB).
		WithClock(func() time.Time { return fixedNow }).
		WithIDGenerator(func(prefix string) (string, error) {
			seq++
			return fmt.Sprintf("%s-%03d", prefix, seq), nil
		})

	ctx := context.Background()
	plan := sampleExecutionPlan()
	traceCarrier := map[string]string{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"tracestate":  "congo=t61rcWkgMzE",
	}

	firstJob, err := store.Publish(ctx, PublishInput{
		Plan:           plan,
		IdempotencyKey: "idem-key-001",
		TraceCarrier:   traceCarrier,
	})
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	if firstJob.ID != "job-001" {
		t.Fatalf("firstJob.ID = %q, want job-001", firstJob.ID)
	}
	if firstJob.Status != contracts.JobStatusPending {
		t.Fatalf("firstJob.Status = %q, want PENDING", firstJob.Status)
	}
	if firstJob.MaxRetries != DefaultMaxRetries {
		t.Fatalf("firstJob.MaxRetries = %d, want %d", firstJob.MaxRetries, DefaultMaxRetries)
	}
	if firstJob.TraceCarrier["traceparent"] != traceCarrier["traceparent"] {
		t.Fatalf("TraceCarrier = %+v, want %+v", firstJob.TraceCarrier, traceCarrier)
	}

	// Simulate process restart by instantiating a fresh store over the same backing DB.
	restartedStore := NewPostgresJobStore(sqlDB)
	reloaded, err := restartedStore.GetByID(ctx, firstJob.ID)
	if err != nil {
		t.Fatalf("GetByID after restart error = %v", err)
	}
	if reloaded.ID != firstJob.ID || reloaded.Plan.PlanChecksum != plan.PlanChecksum {
		t.Fatalf("reloaded job mismatch: got %+v, want %+v", reloaded, firstJob)
	}

	// Duplicate publish with identical idempotency key and payload must return the exact same job.
	dupJob, err := restartedStore.Publish(ctx, PublishInput{
		Plan:           plan,
		IdempotencyKey: "idem-key-001",
		TraceCarrier:   traceCarrier,
	})
	if err != nil {
		t.Fatalf("idempotent duplicate Publish() error = %v", err)
	}
	if dupJob.ID != firstJob.ID {
		t.Fatalf("duplicate Publish() returned job ID %q, want %q", dupJob.ID, firstJob.ID)
	}
	if len(backingDB.rows) != 1 {
		t.Fatalf("expected exactly 1 row in jobs table after idempotent replay, got %d", len(backingDB.rows))
	}

	// Duplicate publish with same idempotency key but altered SQL payload must return ErrIdempotencyConflict.
	modifiedPlan := plan
	modifiedPlan.RequestID = "req-101"
	modifiedPlan.ExecutionID = "exec-101"
	modifiedPlan.UpSQL = "CREATE TABLE invoices (id BIGINT PRIMARY KEY, total NUMERIC);"
	modifiedPlan.PlanChecksum = modifiedPlan.ComputeChecksum()

	_, err = restartedStore.Publish(ctx, PublishInput{
		Plan:           modifiedPlan,
		IdempotencyKey: "idem-key-001",
	})
	if !errors.Is(err, ErrIdempotencyConflict) || !errors.Is(err, contracts.ErrIdempotencyConflict) {
		t.Fatalf("Publish() with conflicting payload = %v, want ErrIdempotencyConflict", err)
	}
	if len(backingDB.rows) != 1 {
		t.Fatalf("conflicting Publish() must not insert row: got %d rows", len(backingDB.rows))
	}

	// Duplicate publish for the same request_id with a different idempotency key must also return ErrIdempotencyConflict.
	_, err = restartedStore.Publish(ctx, PublishInput{
		Plan:           plan,
		IdempotencyKey: "idem-key-different",
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("Publish() with duplicate request_id and different key = %v, want ErrIdempotencyConflict", err)
	}
}

func TestPublisherAdapterContract(t *testing.T) {
	backingDB := &fakeJobsDB{}
	sqlDB := sql.OpenDB(fakeJobsConnector{db: backingDB})
	t.Cleanup(func() { _ = sqlDB.Close() })

	store := NewPostgresJobStore(sqlDB)
	publisher := store.AsContractPublisher()

	plan := sampleExecutionPlan()
	summary, err := publisher.Publish(context.Background(), contracts.EnqueueJobRequest{
		Plan:           plan,
		IdempotencyKey: "idem-contract-1",
		TraceCarrier:   map[string]string{"traceparent": "00-11111111111111111111111111111111-2222222222222222-01"},
	})
	if err != nil {
		t.Fatalf("publisher.Publish() error = %v", err)
	}
	if summary.RequestID != plan.RequestID || summary.ExecutionID != plan.ExecutionID || summary.Status != contracts.JobStatusPending {
		t.Fatalf("unexpected JobSummary: %+v", summary)
	}
}

func TestPublishIntegrationPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_PLATFORM_DATABASE_URL"))
	if dsn == "" {
		t.Skip("TEST_PLATFORM_DATABASE_URL not set; skipping live Postgres integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	schemaName := fmt.Sprintf("test_jobs_publish_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+schemaName); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
	}()
	if _, err := db.ExecContext(ctx, "SET search_path TO "+schemaName); err != nil {
		t.Fatalf("set search_path: %v", err)
	}

	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatalf("migrations.Apply: %v", err)
	}

	store := NewPostgresJobStore(db)
	plan := seedApprovedQueueRequest(t, ctx, db, sampleExecutionPlan())

	job1, err := store.Publish(ctx, PublishInput{
		Plan:           plan,
		IdempotencyKey: "pg-idem-1",
		TraceCarrier:   map[string]string{"traceparent": "00-abcdef-123456-01"},
	})
	if err != nil {
		t.Fatalf("Publish #1 error = %v", err)
	}

	job2, err := store.Publish(ctx, PublishInput{
		Plan:           plan,
		IdempotencyKey: "pg-idem-1",
	})
	if err != nil {
		t.Fatalf("Publish #2 (idempotent) error = %v", err)
	}
	if job1.ID != job2.ID {
		t.Fatalf("idempotent publish returned different IDs: %s vs %s", job1.ID, job2.ID)
	}

	conflictPlan := plan
	conflictPlan.RequestID = "req-conflict"
	conflictPlan.ExecutionID = "exec-conflict"
	conflictPlan.UpSQL = "CREATE TABLE other_table (id INT);"
	conflictPlan.PlanChecksum = conflictPlan.ComputeChecksum()

	_, err = store.Publish(ctx, PublishInput{
		Plan:           conflictPlan,
		IdempotencyKey: "pg-idem-1",
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}
}
