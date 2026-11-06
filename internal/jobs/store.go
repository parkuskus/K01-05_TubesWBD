package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/trace"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/telemetry"
)

const jobColumns = `
	id,
	request_id,
	execution_id,
	database_id,
	connection_revision,
	direction,
	mode,
	status,
	actor_id,
	idempotency_key,
	idempotency_hash,
	plan_payload,
	trace_carrier,
	available_at,
	lease_owner,
	lease_token,
	lease_expires_at,
	retry_count,
	max_retries,
	committed_batches,
	committed_rows,
	total_scope_rows,
	last_committed_cursor,
	last_error_code,
	last_error_message,
	created_at,
	updated_at,
	completed_at
`

// PostgresJobStore implements durable job publishing, claiming, lease heartbeats,
// and status queries backed by PostgreSQL.
type PostgresJobStore struct {
	db                *sql.DB
	now               func() time.Time
	newID             func(prefix string) (string, error)
	defaultMaxRetries int
}

// NewPostgresJobStore creates a PostgresJobStore with safe defaults.
func NewPostgresJobStore(db *sql.DB) *PostgresJobStore {
	return &PostgresJobStore{
		db:                db,
		now:               func() time.Time { return time.Now().UTC() },
		newID:             generateRandomID,
		defaultMaxRetries: DefaultMaxRetries,
	}
}

// WithClock overrides the time source used by the store (useful for deterministic tests).
func (s *PostgresJobStore) WithClock(now func() time.Time) *PostgresJobStore {
	if now != nil {
		s.now = now
	}
	return s
}

// WithIDGenerator overrides the random ID generator used by the store.
func (s *PostgresJobStore) WithIDGenerator(gen func(prefix string) (string, error)) *PostgresJobStore {
	if gen != nil {
		s.newID = gen
	}
	return s
}

// WithDefaultMaxRetries sets the default retry budget for newly published jobs.
func (s *PostgresJobStore) WithDefaultMaxRetries(maxRetries int) *PostgresJobStore {
	if maxRetries >= 0 {
		s.defaultMaxRetries = maxRetries
	}
	return s
}

// PublisherAdapter adapts PostgresJobStore to the contracts.JobPublisher interface.
type PublisherAdapter struct {
	store *PostgresJobStore
}

// AsContractPublisher returns a contracts.JobPublisher backed by this store.
func (s *PostgresJobStore) AsContractPublisher() contracts.JobPublisher {
	return PublisherAdapter{store: s}
}

// Publish implements contracts.JobPublisher by delegating to PostgresJobStore.Publish.
func (a PublisherAdapter) Publish(ctx context.Context, req contracts.EnqueueJobRequest) (contracts.JobSummary, error) {
	if a.store == nil {
		return contracts.JobSummary{}, errors.New("job store is nil")
	}
	job, err := a.store.Publish(ctx, PublishInput{
		Plan:           req.Plan,
		IdempotencyKey: req.IdempotencyKey,
		TraceCarrier:   req.TraceCarrier,
	})
	if err != nil {
		return contracts.JobSummary{}, err
	}
	return job.Summary(), nil
}

// Publish atomically enqueues a validated ExecutionPlan into the durable jobs table.
//
// If a job with the same (actor_id, database_id, idempotency_key) already exists:
//   - If its idempotency_hash matches the incoming payload, Publish returns the existing Job.
//   - If its idempotency_hash differs, Publish returns ErrIdempotencyConflict.
func (s *PostgresJobStore) Publish(ctx context.Context, in PublishInput) (published Job, publishErr error) {
	if s == nil || s.db == nil {
		return Job{}, errors.New("job store database is required")
	}
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if err := in.Validate(); err != nil {
		return Job{}, err
	}
	if !trace.SpanContextFromContext(ctx).IsValid() {
		ctx = telemetry.Extract(ctx, in.TraceCarrier)
	}
	if telemetry.ContextFields(ctx).RequestID == "" {
		ctx = telemetry.Extract(ctx, nil)
	}
	ctx = telemetry.WithFields(ctx, telemetry.Fields{
		ActorID: string(in.Plan.ActorID), DatabaseID: string(in.Plan.TargetDatabaseID),
		MigrationID: string(in.Plan.MigrationID), RevisionID: string(in.Plan.RevisionID),
		ExecutionID: string(in.Plan.ExecutionID), Direction: string(in.Plan.Direction),
	})
	ctx, span := telemetry.StartSpan(ctx, "queue.publish", trace.WithSpanKind(trace.SpanKindProducer))
	defer func() {
		status, level := "SUCCESS", slog.LevelInfo
		if publishErr != nil {
			status, level = "FAILED", slog.LevelError
		}
		slog.LogAttrs(ctx, level, "queue.publish.result", slog.String("status", status),
			slog.String("job_id", string(published.ID)), slog.Any("error", publishErr))
		telemetry.FinishSpan(span, publishErr)
	}()

	plan := in.Plan
	if strings.TrimSpace(plan.PlanChecksum) == "" {
		plan.PlanChecksum = plan.ComputeChecksum()
	}
	in.Plan = plan

	idempotencyHash, err := in.ComputeIdempotencyHash()
	if err != nil {
		return Job{}, err
	}

	planJSON, err := json.Marshal(plan)
	if err != nil {
		return Job{}, fmt.Errorf("marshal execution plan: %w", err)
	}

	carrier := telemetry.Inject(ctx)
	carrierJSON, err := json.Marshal(carrier)
	if err != nil {
		return Job{}, fmt.Errorf("marshal trace carrier: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, fmt.Errorf("begin job publish tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if err := lockPublishKeys(ctx, tx, plan.ActorID, plan.TargetDatabaseID, in.IdempotencyKey, plan.RequestID); err != nil {
		return Job{}, err
	}

	existing, found, err := findExistingJobForPublish(
		ctx,
		tx,
		plan.ActorID,
		plan.TargetDatabaseID,
		in.IdempotencyKey,
		plan.RequestID,
		plan.ExecutionID,
	)
	if err != nil {
		return Job{}, err
	}
	if found {
		if existing.ActorID == plan.ActorID &&
			existing.DatabaseID == plan.TargetDatabaseID &&
			existing.IdempotencyKey == in.IdempotencyKey &&
			existing.IdempotencyHash == idempotencyHash {
			if err := tx.Commit(); err != nil {
				return Job{}, fmt.Errorf("commit idempotent job lookup: %w", err)
			}
			return existing, nil
		}
		return Job{}, ErrIdempotencyConflict
	}

	jobID := strings.TrimSpace(string(in.JobID))
	if jobID == "" {
		jobID, err = s.newID("job")
		if err != nil {
			return Job{}, fmt.Errorf("generate job id: %w", err)
		}
	}

	now := s.now().UTC()
	availableAt := now
	if !in.AvailableAt.IsZero() {
		availableAt = in.AvailableAt.UTC()
	}

	maxRetries := s.defaultMaxRetries
	if in.MaxRetries > 0 {
		maxRetries = in.MaxRetries
	}

	insertQuery := `
		INSERT INTO jobs (
			id,
			request_id,
			execution_id,
			database_id,
			connection_revision,
			direction,
			mode,
			status,
			actor_id,
			idempotency_key,
			idempotency_hash,
			plan_payload,
			trace_carrier,
			available_at,
			retry_count,
			max_retries,
			created_at,
			updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13::jsonb, $14, 0, $15, $16, $16
		)
		RETURNING ` + jobColumns

	row := tx.QueryRowContext(
		ctx,
		insertQuery,
		jobID,
		string(plan.RequestID),
		string(plan.ExecutionID),
		string(plan.TargetDatabaseID),
		plan.TargetConnectionRevision,
		string(plan.Direction),
		string(plan.Mode),
		string(contracts.JobStatusPending),
		string(plan.ActorID),
		in.IdempotencyKey,
		idempotencyHash,
		string(planJSON),
		string(carrierJSON),
		availableAt,
		maxRetries,
		now,
	)
	created, err := scanJob(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Job{}, ErrIdempotencyConflict
		}
		return Job{}, fmt.Errorf("insert job: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("commit job publish: %w", err)
	}
	return created, nil
}

// GetByID fetches a Job by its primary key from the platform database.
func (s *PostgresJobStore) GetByID(ctx context.Context, jobID contracts.JobID) (Job, error) {
	if s == nil || s.db == nil {
		return Job{}, errors.New("job store database is required")
	}
	trimmed := strings.TrimSpace(string(jobID))
	if trimmed == "" {
		return Job{}, ErrJobNotFound
	}
	query := `SELECT ` + jobColumns + ` FROM jobs WHERE id = $1`
	job, err := scanJob(s.db.QueryRowContext(ctx, query, trimmed))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrJobNotFound
		}
		return Job{}, fmt.Errorf("query job %s: %w", trimmed, err)
	}
	return job, nil
}

func findExistingJobForPublish(
	ctx context.Context,
	tx *sql.Tx,
	actorID contracts.UserID,
	databaseID contracts.DatabaseID,
	idempotencyKey string,
	requestID contracts.OperationRequestID,
	executionID contracts.ExecutionID,
) (Job, bool, error) {
	query := `
		SELECT ` + jobColumns + `
		FROM jobs
		WHERE (actor_id = $1 AND database_id = $2 AND idempotency_key = $3)
		   OR request_id = $4
		   OR execution_id = $5
		ORDER BY CASE
			WHEN actor_id = $1 AND database_id = $2 AND idempotency_key = $3 THEN 0
			ELSE 1
		END ASC
		LIMIT 1
		FOR UPDATE
	`
	row := tx.QueryRowContext(
		ctx,
		query,
		string(actorID),
		string(databaseID),
		idempotencyKey,
		string(requestID),
		string(executionID),
	)
	job, err := scanJob(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, false, nil
		}
		return Job{}, false, fmt.Errorf("lookup existing job for publish: %w", err)
	}
	return job, true, nil
}

func lockPublishKeys(
	ctx context.Context,
	tx *sql.Tx,
	actorID contracts.UserID,
	databaseID contracts.DatabaseID,
	idempotencyKey string,
	requestID contracts.OperationRequestID,
) error {
	keys := []string{
		fmt.Sprintf("idem:%s\x00%s\x00%s", actorID, databaseID, idempotencyKey),
		fmt.Sprintf("req:%s", requestID),
	}
	sort.Strings(keys)
	for _, k := range keys {
		k1, k2 := hashAdvisoryKeyPair(k)
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, k1, k2); err != nil {
			return fmt.Errorf("acquire publish advisory lock: %w", err)
		}
	}
	return nil
}

func hashAdvisoryKeyPair(key string) (int32, int32) {
	h := fnv.New64a()
	_, _ = h.Write([]byte("rangkaiedb.jobs.publish:"))
	_, _ = h.Write([]byte(key))
	sum := h.Sum64()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], sum)
	k1 := int32(binary.BigEndian.Uint32(buf[0:4]))
	k2 := int32(binary.BigEndian.Uint32(buf[4:8]))
	return k1, k2
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(scanner rowScanner) (Job, error) {
	var (
		job            Job
		id             string
		requestID      string
		executionID    string
		databaseID     string
		direction      string
		mode           string
		status         string
		actorID        string
		planPayload    []byte
		tracePayload   []byte
		leaseExpiresAt sql.NullTime
		completedAt    sql.NullTime
	)

	err := scanner.Scan(
		&id,
		&requestID,
		&executionID,
		&databaseID,
		&job.ConnectionRevision,
		&direction,
		&mode,
		&status,
		&actorID,
		&job.IdempotencyKey,
		&job.IdempotencyHash,
		&planPayload,
		&tracePayload,
		&job.AvailableAt,
		&job.LeaseOwner,
		&job.LeaseToken,
		&leaseExpiresAt,
		&job.RetryCount,
		&job.MaxRetries,
		&job.CommittedBatches,
		&job.CommittedRows,
		&job.TotalScopeRows,
		&job.LastCommittedCursor,
		&job.LastErrorCode,
		&job.LastErrorMessage,
		&job.CreatedAt,
		&job.UpdatedAt,
		&completedAt,
	)
	if err != nil {
		return Job{}, err
	}

	job.ID = contracts.JobID(id)
	job.RequestID = contracts.OperationRequestID(requestID)
	job.ExecutionID = contracts.ExecutionID(executionID)
	job.DatabaseID = contracts.DatabaseID(databaseID)
	job.Direction = contracts.OperationDirection(direction)
	job.Mode = contracts.ExecutionMode(mode)
	job.Status = contracts.JobStatus(status)
	job.ActorID = contracts.UserID(actorID)

	if len(planPayload) > 0 {
		if err := json.Unmarshal(planPayload, &job.Plan); err != nil {
			return Job{}, fmt.Errorf("unmarshal job plan_payload: %w", err)
		}
	}
	job.TraceCarrier = make(map[string]string)
	if len(tracePayload) > 0 {
		if err := json.Unmarshal(tracePayload, &job.TraceCarrier); err != nil {
			return Job{}, fmt.Errorf("unmarshal job trace_carrier: %w", err)
		}
	}
	if leaseExpiresAt.Valid {
		t := leaseExpiresAt.Time.UTC()
		job.LeaseExpiresAt = &t
	}
	if completedAt.Valid {
		t := completedAt.Time.UTC()
		job.CompletedAt = &t
	}
	job.AvailableAt = job.AvailableAt.UTC()
	job.CreatedAt = job.CreatedAt.UTC()
	job.UpdatedAt = job.UpdatedAt.UTC()
	return job, nil
}

func generateRandomID(prefix string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(b[:])), nil
}

