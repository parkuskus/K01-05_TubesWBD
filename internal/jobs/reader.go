package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"if3110.local/db-platform/internal/contracts"
)

const jobAttemptColumns = `
	id,
	job_id,
	execution_id,
	attempt_no,
	worker_id,
	status,
	executed_sql,
	error_code,
	error_message,
	started_at,
	finished_at
`

var _ contracts.JobReader = (*PostgresJobStore)(nil)
var _ contracts.JobReader = ReaderAdapter{}

// ReaderAdapter adapts PostgresJobStore to the contracts.JobReader interface.
type ReaderAdapter struct {
	store *PostgresJobStore
}

// AsContractReader returns a contracts.JobReader backed by this store.
func (s *PostgresJobStore) AsContractReader() contracts.JobReader {
	return ReaderAdapter{store: s}
}

// GetJob implements contracts.JobReader by delegating to PostgresJobStore.GetJob.
func (a ReaderAdapter) GetJob(ctx context.Context, jobID contracts.JobID) (contracts.JobSummary, error) {
	if a.store == nil {
		return contracts.JobSummary{}, errors.New("job store is nil")
	}
	return a.store.GetJob(ctx, jobID)
}

// ListJobAttempts implements contracts.JobReader by delegating to PostgresJobStore.ListJobAttempts.
func (a ReaderAdapter) ListJobAttempts(ctx context.Context, jobID contracts.JobID) ([]contracts.JobAttemptSummary, error) {
	if a.store == nil {
		return nil, errors.New("job store is nil")
	}
	return a.store.ListJobAttempts(ctx, jobID)
}

// ListDatabaseAttempts implements contracts.JobReader by delegating to PostgresJobStore.ListDatabaseAttempts.
func (a ReaderAdapter) ListDatabaseAttempts(ctx context.Context, databaseID contracts.DatabaseID) ([]contracts.JobAttemptSummary, error) {
	if a.store == nil {
		return nil, errors.New("job store is nil")
	}
	return a.store.ListDatabaseAttempts(ctx, databaseID)
}

// GetJob returns a contracts.JobSummary (including recovery Capabilities for
// PARTIAL and PARTIALLY_REVERTED jobs) for the given jobID.
func (s *PostgresJobStore) GetJob(ctx context.Context, jobID contracts.JobID) (contracts.JobSummary, error) {
	job, err := s.GetByID(ctx, jobID)
	if err != nil {
		return contracts.JobSummary{}, err
	}
	return job.Summary(), nil
}

// GetByRequestID fetches a Job by its operation request_id from the platform database.
func (s *PostgresJobStore) GetByRequestID(ctx context.Context, requestID contracts.OperationRequestID) (Job, error) {
	if s == nil || s.db == nil {
		return Job{}, errors.New("job store database is required")
	}
	trimmed := strings.TrimSpace(string(requestID))
	if trimmed == "" {
		return Job{}, ErrJobNotFound
	}
	query := `SELECT ` + jobColumns + ` FROM jobs WHERE request_id = $1 ORDER BY created_at DESC LIMIT 1`
	job, err := scanJob(s.db.QueryRowContext(ctx, query, trimmed))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrJobNotFound
		}
		return Job{}, fmt.Errorf("query job by request_id %s: %w", trimmed, err)
	}
	return job, nil
}

// ListJobAttempts returns all execution attempts for jobID ordered chronologically
// (attempt_no ASC, started_at ASC). Returns ErrJobNotFound if jobID does not exist.
func (s *PostgresJobStore) ListJobAttempts(ctx context.Context, jobID contracts.JobID) ([]contracts.JobAttemptSummary, error) {
	attempts, err := s.ListAttemptsByJobID(ctx, jobID)
	if err != nil {
		return nil, err
	}
	summaries := make([]contracts.JobAttemptSummary, 0, len(attempts))
	for _, a := range attempts {
		summaries = append(summaries, a.Summary())
	}
	return summaries, nil
}

// ListAttemptsByJobID fetches the full domain JobAttempt slice for jobID ordered
// by attempt_no ASC. Returns ErrJobNotFound if the parent job does not exist.
func (s *PostgresJobStore) ListAttemptsByJobID(ctx context.Context, jobID contracts.JobID) ([]JobAttempt, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("job store database is required")
	}
	trimmed := strings.TrimSpace(string(jobID))
	if trimmed == "" {
		return nil, ErrJobNotFound
	}
	if _, err := s.GetByID(ctx, jobID); err != nil {
		return nil, err
	}

	const query = `
		SELECT ` + jobAttemptColumns + `
		FROM job_attempts
		WHERE job_id = $1
		ORDER BY attempt_no ASC, started_at ASC
	`
	rows, err := s.db.QueryContext(ctx, query, trimmed)
	if err != nil {
		return nil, fmt.Errorf("query job attempts for %s: %w", trimmed, err)
	}
	defer func() {
		_ = rows.Close()
	}()

	attempts := make([]JobAttempt, 0)
	for rows.Next() {
		att, err := scanJobAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job attempt for %s: %w", trimmed, err)
		}
		attempts = append(attempts, att)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate job attempts for %s: %w", trimmed, err)
	}
	return attempts, nil
}

// ListDatabaseAttempts returns all execution attempts across jobs targeting
// databaseID, ordered by started_at DESC, attempt_no DESC so failed/interrupted
// attempts remain visible alongside committed history.
func (s *PostgresJobStore) ListDatabaseAttempts(ctx context.Context, databaseID contracts.DatabaseID) ([]contracts.JobAttemptSummary, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("job store database is required")
	}
	trimmed := strings.TrimSpace(string(databaseID))
	if trimmed == "" {
		return []contracts.JobAttemptSummary{}, nil
	}

	const query = `
		SELECT
			a.id,
			a.job_id,
			a.execution_id,
			a.attempt_no,
			a.worker_id,
			a.status,
			a.executed_sql,
			a.error_code,
			a.error_message,
			a.started_at,
			a.finished_at
		FROM job_attempts a
		JOIN jobs j ON j.id = a.job_id
		WHERE j.database_id = $1
		ORDER BY a.started_at DESC, a.attempt_no DESC
	`
	rows, err := s.db.QueryContext(ctx, query, trimmed)
	if err != nil {
		return nil, fmt.Errorf("query database attempts for %s: %w", trimmed, err)
	}
	defer func() {
		_ = rows.Close()
	}()

	summaries := make([]contracts.JobAttemptSummary, 0)
	for rows.Next() {
		att, err := scanJobAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan database attempt for %s: %w", trimmed, err)
		}
		summaries = append(summaries, att.Summary())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate database attempts for %s: %w", trimmed, err)
	}
	return summaries, nil
}

// ListDatabaseJobs returns all jobs targeting databaseID ordered by created_at DESC.
func (s *PostgresJobStore) ListDatabaseJobs(ctx context.Context, databaseID contracts.DatabaseID) ([]contracts.JobSummary, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("job store database is required")
	}
	trimmed := strings.TrimSpace(string(databaseID))
	if trimmed == "" {
		return []contracts.JobSummary{}, nil
	}

	const query = `
		SELECT ` + jobColumns + `
		FROM jobs
		WHERE database_id = $1
		ORDER BY created_at DESC
	`
	rows, err := s.db.QueryContext(ctx, query, trimmed)
	if err != nil {
		return nil, fmt.Errorf("query database jobs for %s: %w", trimmed, err)
	}
	defer func() {
		_ = rows.Close()
	}()

	summaries := make([]contracts.JobSummary, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan database job for %s: %w", trimmed, err)
		}
		summaries = append(summaries, job.Summary())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate database jobs for %s: %w", trimmed, err)
	}
	return summaries, nil
}

