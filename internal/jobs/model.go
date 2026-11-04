package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

const DefaultMaxRetries = 3

var (
	ErrJobNotFound          = errors.New("job not found")
	ErrNoJobAvailable       = errors.New("no eligible job available to claim")
	ErrLeaseMismatch        = errors.New("job lease token mismatch or expired")
	ErrInvalidPublishInput  = errors.New("invalid job publish input")
	ErrRetryBudgetExhausted = errors.New("job retry budget exhausted")
	ErrIdempotencyConflict  = contracts.ErrIdempotencyConflict
)

// Job represents a durable queued migration or recovery operation stored in the platform database.
type Job struct {
	ID                  contracts.JobID
	RequestID           contracts.OperationRequestID
	ExecutionID         contracts.ExecutionID
	DatabaseID          contracts.DatabaseID
	ConnectionRevision  int64
	Direction           contracts.OperationDirection
	Mode                contracts.ExecutionMode
	Status              contracts.JobStatus
	ActorID             contracts.UserID
	IdempotencyKey      string
	IdempotencyHash     string
	Plan                contracts.ExecutionPlan
	TraceCarrier        map[string]string
	AvailableAt         time.Time
	LeaseOwner          string
	LeaseToken          string
	LeaseExpiresAt      *time.Time
	RetryCount          int
	MaxRetries          int
	CommittedBatches    int
	CommittedRows       int64
	TotalScopeRows      int64
	LastCommittedCursor string
	LastErrorCode       string
	LastErrorMessage    string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	CompletedAt         *time.Time
}

// Capabilities returns the recovery actions available for the job in the SPA monitor.
func (j Job) Capabilities() contracts.JobCapabilities {
	isPartial := j.Status == contracts.JobStatusPartial || j.Status == contracts.JobStatusPartiallyReverted
	return contracts.JobCapabilities{
		RequestResume:       isPartial,
		RequestCompensation: isPartial && !j.Plan.IsIrreversible,
	}
}

// Summary converts the domain Job into a contracts.JobSummary for API and cross-module callers.
func (j Job) Summary() contracts.JobSummary {
	return contracts.JobSummary{
		JobID:               j.ID,
		RequestID:           j.RequestID,
		ExecutionID:         j.ExecutionID,
		DatabaseID:          j.DatabaseID,
		Direction:           j.Direction,
		Mode:                j.Mode,
		Status:              j.Status,
		ActorID:             j.ActorID,
		RetryCount:          j.RetryCount,
		MaxRetries:          j.MaxRetries,
		CommittedBatches:    j.CommittedBatches,
		CommittedRows:       j.CommittedRows,
		TotalScopeRows:      j.TotalScopeRows,
		LastCommittedCursor: j.LastCommittedCursor,
		ErrorCode:           j.LastErrorCode,
		ErrorMessage:        j.LastErrorMessage,
		Capabilities:        j.Capabilities(),
		CreatedAt:           j.CreatedAt,
		UpdatedAt:           j.UpdatedAt,
		CompletedAt:         j.CompletedAt,
	}
}

// JobAttempt represents a single worker execution attempt recorded in job_attempts.
type JobAttempt struct {
	ID           contracts.AttemptID
	JobID        contracts.JobID
	ExecutionID  contracts.ExecutionID
	AttemptNo    int
	WorkerID     string
	Status       contracts.AttemptStatus
	ExecutedSQL  string
	ErrorCode    string
	ErrorMessage string
	StartedAt    time.Time
	FinishedAt   *time.Time
}

// Summary converts a JobAttempt into a contracts.JobAttemptSummary.
func (a JobAttempt) Summary() contracts.JobAttemptSummary {
	return contracts.JobAttemptSummary{
		AttemptID:    a.ID,
		JobID:        a.JobID,
		ExecutionID:  a.ExecutionID,
		AttemptNo:    a.AttemptNo,
		WorkerID:     a.WorkerID,
		Status:       a.Status,
		ExecutedSQL:  a.ExecutedSQL,
		ErrorCode:    a.ErrorCode,
		ErrorMessage: a.ErrorMessage,
		StartedAt:    a.StartedAt,
		FinishedAt:   a.FinishedAt,
	}
}

// PublishInput contains the parameters required to atomically enqueue a job.
type PublishInput struct {
	JobID          contracts.JobID
	Plan           contracts.ExecutionPlan
	IdempotencyKey string
	TraceCarrier   map[string]string
	MaxRetries     int
	AvailableAt    time.Time
}

// Validate checks that the publish request contains a valid plan and non-empty idempotency key.
func (in PublishInput) Validate() error {
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return ErrInvalidPublishInput
	}
	if in.MaxRetries < 0 {
		return ErrInvalidPublishInput
	}
	if err := in.Plan.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPublishInput, err)
	}
	return nil
}

// ComputeIdempotencyHash produces a deterministic SHA-256 hex digest of the semantic publish payload.
func (in PublishInput) ComputeIdempotencyHash() (string, error) {
	if err := in.Validate(); err != nil {
		return "", err
	}
	checksum := strings.TrimSpace(in.Plan.PlanChecksum)
	if checksum == "" {
		checksum = in.Plan.ComputeChecksum()
	}
	payload := fmt.Sprintf(
		"req=%s\x00exec=%s\x00actor=%s\x00db=%s\x00rev=%d\x00dir=%s\x00mode=%s\x00checksum=%s",
		in.Plan.RequestID,
		in.Plan.ExecutionID,
		in.Plan.ActorID,
		in.Plan.TargetDatabaseID,
		in.Plan.TargetConnectionRevision,
		in.Plan.Direction,
		in.Plan.Mode,
		checksum,
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:]), nil
}

// ClaimedJob pairs a claimed Job with its newly started JobAttempt row.
type ClaimedJob struct {
	Job     Job
	Attempt JobAttempt
}

// CompleteAttemptInput describes the state transition applied when a worker finishes an attempt.
type CompleteAttemptInput struct {
	JobID               contracts.JobID
	AttemptID           contracts.AttemptID
	LeaseToken          string
	JobStatus           contracts.JobStatus
	AttemptStatus       contracts.AttemptStatus
	ExecutedSQL         string
	ErrorCode           string
	ErrorMessage        string
	CommittedBatches    int
	CommittedRows       int64
	TotalScopeRows      int64
	LastCommittedCursor string
}
