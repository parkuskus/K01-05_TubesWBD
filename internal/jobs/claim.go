package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

const (
	DefaultBaseRetryBackoff = 1 * time.Second
	DefaultMaxRetryBackoff  = 30 * time.Second
	DefaultRetryJitterRatio = 0.20
)

const attemptColumns = `
	id,
	job_id,
	execution_id,
	attempt_no,
	worker_id,
	status,
	executed_sql,
	COALESCE(error_code, ''),
	COALESCE(error_message, ''),
	started_at,
	finished_at
`

// ScheduleRetryInput describes a transient attempt failure that should either be
// scheduled for bounded exponential-backoff retry or transitioned to terminal FAILED
// once max_retries is reached.
type ScheduleRetryInput struct {
	JobID               contracts.JobID
	AttemptID           contracts.AttemptID
	LeaseToken          string
	AttemptStatus       contracts.AttemptStatus
	ExecutedSQL         string
	ErrorCode           string
	ErrorMessage        string
	BaseBackoff         time.Duration
	MaxBackoff          time.Duration
	DisableRandomJitter bool
}

// ComputeBackoff calculates exponential backoff (base * 2^(retryNo-1)) capped at maxDelay,
// plus optional cryptographic random jitter in [0, jitterRatio * delay].
func ComputeBackoff(retryNo int, baseDelay, maxDelay time.Duration, jitterRatio float64) time.Duration {
	if baseDelay <= 0 {
		baseDelay = DefaultBaseRetryBackoff
	}
	if maxDelay <= 0 {
		maxDelay = DefaultMaxRetryBackoff
	}
	if retryNo < 1 {
		retryNo = 1
	}

	shift := retryNo - 1
	if shift > 20 {
		shift = 20
	}
	multiplier := math.Pow(2, float64(shift))
	raw := float64(baseDelay) * multiplier
	if raw > float64(maxDelay) || raw < 0 {
		raw = float64(maxDelay)
	}
	delay := time.Duration(raw)

	if jitterRatio > 0 {
		maxJitterNs := int64(float64(delay) * jitterRatio)
		if maxJitterNs > 0 {
			var b [8]byte
			if _, err := rand.Read(b[:]); err == nil {
				jitter := int64(binary.BigEndian.Uint64(b[:]) % uint64(maxJitterNs))
				delay += time.Duration(jitter)
			}
		}
	}
	return delay
}

// ClaimNextDue atomically claims the next eligible job using FOR UPDATE SKIP LOCKED
// without starving other target databases that have no active RUNNING job or blocking gate.
//
// The claim transaction commits immediately after assigning a fresh lease_token and
// inserting a STARTED row in job_attempts; it is never held open during target SQL execution.
func (s *PostgresJobStore) ClaimNextDue(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
) (ClaimedJob, error) {
	if s == nil || s.db == nil {
		return ClaimedJob{}, errors.New("job store database is required")
	}
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return ClaimedJob{}, errors.New("workerID is required")
	}
	if leaseDuration <= 0 {
		leaseDuration = 30 * time.Second
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ClaimedJob{}, fmt.Errorf("begin claim tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	now := s.now().UTC()
	hasGates, err := executionGatesTableExists(ctx, tx)
	if err != nil {
		return ClaimedJob{}, err
	}

	candidate, found, err := selectCandidateJobForClaim(ctx, tx, now, hasGates)
	if err != nil {
		return ClaimedJob{}, err
	}
	if !found {
		return ClaimedJob{}, ErrNoJobAvailable
	}

	wasExpiredRunning := candidate.Status == contracts.JobStatusRunning
	if wasExpiredRunning {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE job_attempts
			 SET status = $1,
			     error_code = CASE WHEN COALESCE(error_code, '') = '' THEN 'LEASE_EXPIRED' ELSE error_code END,
			     error_message = CASE WHEN COALESCE(error_message, '') = '' THEN 'worker lease expired before attempt completion' ELSE error_message END,
			     finished_at = $2
			 WHERE job_id = $3 AND status = $4`,
			string(contracts.AttemptStatusFailed),
			now,
			string(candidate.ID),
			string(contracts.AttemptStatusStarted),
		); err != nil {
			return ClaimedJob{}, fmt.Errorf("close expired attempt for job %s: %w", candidate.ID, err)
		}
	}

	nextRetryCount := candidate.RetryCount
	if wasExpiredRunning {
		nextRetryCount++
	}

	leaseToken, err := s.newID("lease")
	if err != nil {
		return ClaimedJob{}, fmt.Errorf("generate lease token: %w", err)
	}
	attemptID, err := s.newID("att")
	if err != nil {
		return ClaimedJob{}, fmt.Errorf("generate attempt id: %w", err)
	}

	var maxAttemptNo int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COALESCE(MAX(attempt_no), 0) FROM job_attempts WHERE job_id = $1`,
		string(candidate.ID),
	).Scan(&maxAttemptNo); err != nil {
		return ClaimedJob{}, fmt.Errorf("query max attempt_no for job %s: %w", candidate.ID, err)
	}
	nextAttemptNo := maxAttemptNo + 1

	leaseExpiresAt := now.Add(leaseDuration)
	updateJobQuery := `
		UPDATE jobs
		SET status = $1,
		    lease_owner = $2,
		    lease_token = $3,
		    lease_expires_at = $4,
		    retry_count = $5,
		    updated_at = $6
		WHERE id = $7
		RETURNING ` + jobColumns

	claimedJob, err := scanJob(tx.QueryRowContext(
		ctx,
		updateJobQuery,
		string(contracts.JobStatusRunning),
		workerID,
		leaseToken,
		leaseExpiresAt,
		nextRetryCount,
		now,
		string(candidate.ID),
	))
	if err != nil {
		return ClaimedJob{}, fmt.Errorf("update claimed job %s: %w", candidate.ID, err)
	}

	insertAttemptQuery := `
		INSERT INTO job_attempts (
			id,
			job_id,
			execution_id,
			attempt_no,
			worker_id,
			status,
			executed_sql,
			started_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, '', $7
		)
		RETURNING ` + attemptColumns

	attempt, err := scanJobAttempt(tx.QueryRowContext(
		ctx,
		insertAttemptQuery,
		attemptID,
		string(claimedJob.ID),
		string(claimedJob.ExecutionID),
		nextAttemptNo,
		workerID,
		string(contracts.AttemptStatusStarted),
		now,
	))
	if err != nil {
		return ClaimedJob{}, fmt.Errorf("insert job attempt for %s: %w", claimedJob.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return ClaimedJob{}, fmt.Errorf("commit job claim: %w", err)
	}

	return ClaimedJob{
		Job:     claimedJob,
		Attempt: attempt,
	}, nil
}

// Heartbeat extends the lease_expires_at timestamp of a RUNNING job using
// compare-and-set on (id, lease_token, status = 'RUNNING').
// If the lease was already reclaimed by another worker, Heartbeat returns ErrLeaseMismatch.
func (s *PostgresJobStore) Heartbeat(
	ctx context.Context,
	jobID contracts.JobID,
	leaseToken string,
	leaseDuration time.Duration,
) (time.Time, error) {
	if s == nil || s.db == nil {
		return time.Time{}, errors.New("job store database is required")
	}
	trimmedID := strings.TrimSpace(string(jobID))
	trimmedToken := strings.TrimSpace(leaseToken)
	if trimmedID == "" || trimmedToken == "" {
		return time.Time{}, ErrLeaseMismatch
	}
	if leaseDuration <= 0 {
		leaseDuration = 30 * time.Second
	}

	now := s.now().UTC()
	newExpiry := now.Add(leaseDuration)

	var returnedExpiry time.Time
	err := s.db.QueryRowContext(
		ctx,
		`UPDATE jobs
		 SET lease_expires_at = $1,
		     updated_at = $2
		 WHERE id = $3
		   AND lease_token = $4
		   AND status = $5
		 RETURNING lease_expires_at`,
		newExpiry,
		now,
		trimmedID,
		trimmedToken,
		string(contracts.JobStatusRunning),
	).Scan(&returnedExpiry)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, ErrLeaseMismatch
		}
		return time.Time{}, fmt.Errorf("heartbeat job %s: %w", trimmedID, err)
	}
	return returnedExpiry.UTC(), nil
}

// UpdateProgress updates batch execution counters on a RUNNING job while verifying
// that the worker still holds the active lease_token.
func (s *PostgresJobStore) UpdateProgress(
	ctx context.Context,
	jobID contracts.JobID,
	leaseToken string,
	committedBatches int,
	committedRows int64,
	totalScopeRows int64,
	lastCursor string,
) error {
	if s == nil || s.db == nil {
		return errors.New("job store database is required")
	}
	trimmedID := strings.TrimSpace(string(jobID))
	trimmedToken := strings.TrimSpace(leaseToken)
	if trimmedID == "" || trimmedToken == "" {
		return ErrLeaseMismatch
	}

	now := s.now().UTC()
	res, err := s.db.ExecContext(
		ctx,
		`UPDATE jobs
		 SET committed_batches = $1,
		     committed_rows = $2,
		     total_scope_rows = $3,
		     last_committed_cursor = $4,
		     updated_at = $5
		 WHERE id = $6
		   AND lease_token = $7
		   AND status = $8`,
		committedBatches,
		committedRows,
		totalScopeRows,
		lastCursor,
		now,
		trimmedID,
		trimmedToken,
		string(contracts.JobStatusRunning),
	)
	if err != nil {
		return fmt.Errorf("update job progress %s: %w", trimmedID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for progress %s: %w", trimmedID, err)
	}
	if rows == 0 {
		return ErrLeaseMismatch
	}
	return nil
}

// CompleteAttempt atomically transitions a RUNNING job and its current attempt
// to a post-attempt state (e.g. SUCCESS, FAILED, PARTIAL, PARTIALLY_REVERTED,
// BLOCKED, or UNKNOWN) guarded by compare-and-set on the lease_token.
func (s *PostgresJobStore) CompleteAttempt(
	ctx context.Context,
	in CompleteAttemptInput,
) (Job, JobAttempt, error) {
	if s == nil || s.db == nil {
		return Job{}, JobAttempt{}, errors.New("job store database is required")
	}
	trimmedJobID := strings.TrimSpace(string(in.JobID))
	trimmedAttemptID := strings.TrimSpace(string(in.AttemptID))
	trimmedToken := strings.TrimSpace(in.LeaseToken)
	if trimmedJobID == "" || trimmedAttemptID == "" || trimmedToken == "" {
		return Job{}, JobAttempt{}, ErrLeaseMismatch
	}
	if !in.JobStatus.IsValid() {
		return Job{}, JobAttempt{}, fmt.Errorf("invalid target job status %q", in.JobStatus)
	}
	if !in.AttemptStatus.IsValid() {
		return Job{}, JobAttempt{}, fmt.Errorf("invalid target attempt status %q", in.AttemptStatus)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, JobAttempt{}, fmt.Errorf("begin complete attempt tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	now := s.now().UTC()
	var completedAt any
	if in.JobStatus.IsTerminal() {
		completedAt = now
	}

	updateJobQuery := `
		UPDATE jobs
		SET status = $1,
		    lease_owner = '',
		    lease_token = '',
		    lease_expires_at = NULL,
		    committed_batches = $2,
		    committed_rows = $3,
		    total_scope_rows = $4,
		    last_committed_cursor = $5,
		    last_error_code = $6,
		    last_error_message = $7,
		    updated_at = $8,
		    completed_at = $9
		WHERE id = $10
		  AND lease_token = $11
		  AND status = $12
		RETURNING ` + jobColumns

	updatedJob, err := scanJob(tx.QueryRowContext(
		ctx,
		updateJobQuery,
		string(in.JobStatus),
		in.CommittedBatches,
		in.CommittedRows,
		in.TotalScopeRows,
		in.LastCommittedCursor,
		in.ErrorCode,
		in.ErrorMessage,
		now,
		completedAt,
		trimmedJobID,
		trimmedToken,
		string(contracts.JobStatusRunning),
	))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, JobAttempt{}, ErrLeaseMismatch
		}
		return Job{}, JobAttempt{}, fmt.Errorf("complete job %s: %w", trimmedJobID, err)
	}

	updateAttemptQuery := `
		UPDATE job_attempts
		SET status = $1,
		    executed_sql = $2,
		    error_code = $3,
		    error_message = $4,
		    finished_at = $5
		WHERE id = $6
		  AND job_id = $7
		RETURNING ` + attemptColumns

	updatedAttempt, err := scanJobAttempt(tx.QueryRowContext(
		ctx,
		updateAttemptQuery,
		string(in.AttemptStatus),
		in.ExecutedSQL,
		in.ErrorCode,
		in.ErrorMessage,
		now,
		trimmedAttemptID,
		trimmedJobID,
	))
	if err != nil {
		return Job{}, JobAttempt{}, fmt.Errorf("complete attempt %s: %w", trimmedAttemptID, err)
	}

	if err := tx.Commit(); err != nil {
		return Job{}, JobAttempt{}, fmt.Errorf("commit complete attempt: %w", err)
	}

	return updatedJob, updatedAttempt, nil
}

// ScheduleRetry records a failed attempt and either transitions the job to RETRY_WAIT
// with exponential backoff + jitter (when retry_count < max_retries) or transitions the
// job terminally to FAILED once max_retries is exhausted.
//
// All updates are guarded by compare-and-set on (id, lease_token, status = 'RUNNING').
func (s *PostgresJobStore) ScheduleRetry(
	ctx context.Context,
	in ScheduleRetryInput,
) (Job, JobAttempt, error) {
	if s == nil || s.db == nil {
		return Job{}, JobAttempt{}, errors.New("job store database is required")
	}
	trimmedJobID := strings.TrimSpace(string(in.JobID))
	trimmedAttemptID := strings.TrimSpace(string(in.AttemptID))
	trimmedToken := strings.TrimSpace(in.LeaseToken)
	if trimmedJobID == "" || trimmedAttemptID == "" || trimmedToken == "" {
		return Job{}, JobAttempt{}, ErrLeaseMismatch
	}

	attemptStatus := in.AttemptStatus
	if attemptStatus == "" {
		attemptStatus = contracts.AttemptStatusFailed
	}
	if !attemptStatus.IsValid() {
		return Job{}, JobAttempt{}, fmt.Errorf("invalid attempt status %q", attemptStatus)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, JobAttempt{}, fmt.Errorf("begin schedule retry tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	current, err := scanJob(tx.QueryRowContext(
		ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE id = $1 AND lease_token = $2 AND status = $3 FOR UPDATE`,
		trimmedJobID,
		trimmedToken,
		string(contracts.JobStatusRunning),
	))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, JobAttempt{}, ErrLeaseMismatch
		}
		return Job{}, JobAttempt{}, fmt.Errorf("lock job %s for retry: %w", trimmedJobID, err)
	}

	now := s.now().UTC()
	var (
		nextStatus  contracts.JobStatus
		nextRetries int
		availableAt time.Time
		completedAt any
	)

	if current.RetryCount < current.MaxRetries {
		nextRetries = current.RetryCount + 1
		jitterRatio := DefaultRetryJitterRatio
		if in.DisableRandomJitter {
			jitterRatio = 0
		}
		backoff := ComputeBackoff(nextRetries, in.BaseBackoff, in.MaxBackoff, jitterRatio)
		availableAt = now.Add(backoff)
		nextStatus = contracts.JobStatusRetryWait
		completedAt = nil
	} else {
		nextRetries = current.RetryCount
		availableAt = current.AvailableAt
		nextStatus = contracts.JobStatusFailed
		completedAt = now
	}

	updateJobQuery := `
		UPDATE jobs
		SET status = $1,
		    lease_owner = '',
		    lease_token = '',
		    lease_expires_at = NULL,
		    retry_count = $2,
		    available_at = $3,
		    last_error_code = $4,
		    last_error_message = $5,
		    updated_at = $6,
		    completed_at = $7
		WHERE id = $8
		  AND lease_token = $9
		  AND status = $10
		RETURNING ` + jobColumns

	updatedJob, err := scanJob(tx.QueryRowContext(
		ctx,
		updateJobQuery,
		string(nextStatus),
		nextRetries,
		availableAt,
		in.ErrorCode,
		in.ErrorMessage,
		now,
		completedAt,
		trimmedJobID,
		trimmedToken,
		string(contracts.JobStatusRunning),
	))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, JobAttempt{}, ErrLeaseMismatch
		}
		return Job{}, JobAttempt{}, fmt.Errorf("update job %s for retry: %w", trimmedJobID, err)
	}

	updateAttemptQuery := `
		UPDATE job_attempts
		SET status = $1,
		    executed_sql = $2,
		    error_code = $3,
		    error_message = $4,
		    finished_at = $5
		WHERE id = $6
		  AND job_id = $7
		RETURNING ` + attemptColumns

	updatedAttempt, err := scanJobAttempt(tx.QueryRowContext(
		ctx,
		updateAttemptQuery,
		string(attemptStatus),
		in.ExecutedSQL,
		in.ErrorCode,
		in.ErrorMessage,
		now,
		trimmedAttemptID,
		trimmedJobID,
	))
	if err != nil {
		return Job{}, JobAttempt{}, fmt.Errorf("update attempt %s for retry: %w", trimmedAttemptID, err)
	}

	if err := tx.Commit(); err != nil {
		return Job{}, JobAttempt{}, fmt.Errorf("commit schedule retry: %w", err)
	}

	return updatedJob, updatedAttempt, nil
}

func executionGatesTableExists(ctx context.Context, tx *sql.Tx) (bool, error) {
	var reg sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT to_regclass('database_execution_gates')`).Scan(&reg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return false, nil
		}
		return false, fmt.Errorf("check database_execution_gates existence: %w", err)
	}
	return reg.Valid && strings.TrimSpace(reg.String) != "", nil
}

func selectCandidateJobForClaim(
	ctx context.Context,
	tx *sql.Tx,
	now time.Time,
	hasGates bool,
) (Job, bool, error) {
	gateClause := ""
	if hasGates {
		gateClause = `
		  AND NOT EXISTS (
			SELECT 1
			FROM database_execution_gates g
			WHERE g.database_id = j.database_id
			  AND NOT (
				j.direction IN ('RESUME', 'COMPENSATE')
				AND g.reason IN ('PARTIAL', 'PARTIALLY_REVERTED')
			  )
		  )`
	}

	query := `
		SELECT ` + jobColumns + `
		FROM jobs j
		WHERE (
			(j.status IN ('PENDING', 'RETRY_WAIT') AND j.available_at <= $1)
			OR (
				j.status = 'RUNNING'
				AND j.lease_expires_at IS NOT NULL
				AND j.lease_expires_at < $1
				AND j.retry_count < j.max_retries
			)
		)
		  AND NOT EXISTS (
			SELECT 1
			FROM jobs active
			WHERE active.database_id = j.database_id
			  AND active.id <> j.id
			  AND active.status = 'RUNNING'
			  AND active.lease_expires_at IS NOT NULL
			  AND active.lease_expires_at >= $1
		  )` + gateClause + `
		ORDER BY j.available_at ASC, j.created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`

	job, err := scanJob(tx.QueryRowContext(ctx, query, now))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, false, nil
		}
		return Job{}, false, fmt.Errorf("select due job for claim: %w", err)
	}
	return job, true, nil
}

func scanJobAttempt(scanner rowScanner) (JobAttempt, error) {
	var (
		attempt     JobAttempt
		id          string
		jobID       string
		executionID string
		status      string
		finishedAt  sql.NullTime
	)
	err := scanner.Scan(
		&id,
		&jobID,
		&executionID,
		&attempt.AttemptNo,
		&attempt.WorkerID,
		&status,
		&attempt.ExecutedSQL,
		&attempt.ErrorCode,
		&attempt.ErrorMessage,
		&attempt.StartedAt,
		&finishedAt,
	)
	if err != nil {
		return JobAttempt{}, err
	}
	attempt.ID = contracts.AttemptID(id)
	attempt.JobID = contracts.JobID(jobID)
	attempt.ExecutionID = contracts.ExecutionID(executionID)
	attempt.Status = contracts.AttemptStatus(status)
	attempt.StartedAt = attempt.StartedAt.UTC()
	if finishedAt.Valid {
		t := finishedAt.Time.UTC()
		attempt.FinishedAt = &t
	}
	return attempt, nil
}

