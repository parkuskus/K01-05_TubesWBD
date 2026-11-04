package jobs

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/contracts"
)

type fakeJobsDB struct {
	mu   sync.Mutex
	rows []fakeJobRecord
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
	db     *fakeJobsDB
	inTx   bool
	staged []fakeJobRecord
}

func (c *fakeJobsConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *fakeJobsConn) Close() error                        { return nil }

func (c *fakeJobsConn) Begin() (driver.Tx, error) {
	c.db.mu.Lock()
	c.inTx = true
	c.staged = append([]fakeJobRecord(nil), c.db.rows...)
	return &fakeJobsTx{conn: c}, nil
}

type fakeJobsTx struct {
	conn *fakeJobsConn
}

func (tx *fakeJobsTx) Commit() error {
	if tx.conn.inTx {
		tx.conn.db.rows = tx.conn.staged
		tx.conn.staged = nil
		tx.conn.inTx = false
		tx.conn.db.mu.Unlock()
	}
	return nil
}

func (tx *fakeJobsTx) Rollback() error {
	if tx.conn.inTx {
		tx.conn.staged = nil
		tx.conn.inTx = false
		tx.conn.db.mu.Unlock()
	}
	return nil
}

func (c *fakeJobsConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	trimmed := strings.TrimSpace(query)
	if strings.HasPrefix(trimmed, "SELECT pg_advisory_xact_lock") {
		return driver.RowsAffected(1), nil
	}
	return nil, fmt.Errorf("unexpected ExecContext query: %s", trimmed)
}

func (c *fakeJobsConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	trimmed := strings.TrimSpace(query)
	switch {
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
		RevisionID:               "rev-1",
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
	plan := sampleExecutionPlan()

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

