package contracts

import (
	"context"
	"time"
)

type JobStatus string

const (
	JobStatusPending           JobStatus = "PENDING"
	JobStatusRunning           JobStatus = "RUNNING"
	JobStatusRetryWait         JobStatus = "RETRY_WAIT"
	JobStatusBlocked           JobStatus = "BLOCKED"
	JobStatusPartial           JobStatus = "PARTIAL"
	JobStatusPartiallyReverted JobStatus = "PARTIALLY_REVERTED"
	JobStatusUnknown           JobStatus = "UNKNOWN"
	JobStatusSuccess           JobStatus = "SUCCESS"
	JobStatusFailed            JobStatus = "FAILED"
)

func (s JobStatus) IsValid() bool {
	switch s {
	case JobStatusPending,
		JobStatusRunning,
		JobStatusRetryWait,
		JobStatusBlocked,
		JobStatusPartial,
		JobStatusPartiallyReverted,
		JobStatusUnknown,
		JobStatusSuccess,
		JobStatusFailed:
		return true
	default:
		return false
	}
}

func (s JobStatus) IsTerminal() bool {
	return s == JobStatusSuccess || s == JobStatusFailed
}

type AttemptStatus string

const (
	AttemptStatusStarted    AttemptStatus = "STARTED"
	AttemptStatusCommitted  AttemptStatus = "COMMITTED"
	AttemptStatusRolledBack AttemptStatus = "ROLLED_BACK"
	AttemptStatusFailed     AttemptStatus = "FAILED"
	AttemptStatusUnknown    AttemptStatus = "UNKNOWN"
)

func (s AttemptStatus) IsValid() bool {
	switch s {
	case AttemptStatusStarted,
		AttemptStatusCommitted,
		AttemptStatusRolledBack,
		AttemptStatusFailed,
		AttemptStatusUnknown:
		return true
	default:
		return false
	}
}

// JobCapabilities indicates recovery actions available for a job in the SPA.
type JobCapabilities struct {
	RequestResume       bool
	RequestCompensation bool
}

// EnqueueJobRequest carries an approved ExecutionPlan, client idempotency key,
// and W3C trace context carrier to be persisted in the job queue.
type EnqueueJobRequest struct {
	Plan           ExecutionPlan
	IdempotencyKey string
	TraceCarrier   map[string]string
}

// JobSummary represents the operational state and committed batch progress of a queued job.
type JobSummary struct {
	JobID               JobID
	RequestID           OperationRequestID
	ExecutionID         ExecutionID
	DatabaseID          DatabaseID
	Direction           OperationDirection
	Mode                ExecutionMode
	Status              JobStatus
	ActorID             UserID
	RetryCount          int
	MaxRetries          int
	CommittedBatches    int
	CommittedRows       int64
	TotalScopeRows      int64
	LastCommittedCursor string
	ErrorCode           string
	ErrorMessage        string
	Capabilities        JobCapabilities
	CreatedAt           time.Time
	UpdatedAt           time.Time
	CompletedAt         *time.Time
}

// JobAttemptSummary records a single execution attempt (including failures and retries) for a job.
type JobAttemptSummary struct {
	AttemptID    AttemptID
	JobID        JobID
	ExecutionID  ExecutionID
	AttemptNo    int
	WorkerID     string
	Status       AttemptStatus
	ExecutedSQL  string
	ErrorCode    string
	ErrorMessage string
	StartedAt    time.Time
	FinishedAt   *time.Time
}

// JobPublisher atomically enqueues an approved operation request into the durable queue.
type JobPublisher interface {
	Publish(ctx context.Context, req EnqueueJobRequest) (JobSummary, error)
}

// JobReader queries operational job state and attempt timelines for the API and SPA monitor.
type JobReader interface {
	GetJob(ctx context.Context, jobID JobID) (JobSummary, error)
	ListJobAttempts(ctx context.Context, jobID JobID) ([]JobAttemptSummary, error)
	ListDatabaseAttempts(ctx context.Context, databaseID DatabaseID) ([]JobAttemptSummary, error)
}

