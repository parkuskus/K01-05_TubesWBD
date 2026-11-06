package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/jobs"
	"if3110.local/db-platform/internal/telemetry"
)

// JobQueue defines the durable queue operations consumed by Worker.
type JobQueue interface {
	ClaimNextDue(ctx context.Context, workerID string, leaseDuration time.Duration) (jobs.ClaimedJob, error)
	Heartbeat(ctx context.Context, jobID contracts.JobID, leaseToken string, leaseDuration time.Duration) (time.Time, error)
	CompleteAttempt(ctx context.Context, in jobs.CompleteAttemptInput) (jobs.Job, jobs.JobAttempt, error)
	ScheduleRetry(ctx context.Context, in jobs.ScheduleRetryInput) (jobs.Job, jobs.JobAttempt, error)
}

// ExecutorConnProvider acquires an isolated physical *sql.Conn and its active
// connection revision for a target database.
type ExecutorConnProvider interface {
	AcquireExecutorConn(ctx context.Context, databaseID contracts.DatabaseID) (*sql.Conn, int64, error)
}

// HeadStateReader inspects the current committed migration head and unresolved
// gate state on the pinned target database connection while the session lock is held.
type HeadStateReader interface {
	ReadActiveHeadOnConn(ctx context.Context, conn *sql.Conn) (contracts.ActiveHeadState, error)
}

// ExecutionEnv bundles the claimed job, pinned target connection, active session
// advisory lock, and pre-execution target head state passed to a PlanExecutor.
type ExecutionEnv struct {
	Claimed    jobs.ClaimedJob
	Conn       *sql.Conn
	Lock       *TargetSessionLock
	HeadBefore contracts.ActiveHeadState
}

// ExecutionResult describes the outcome of executing a claimed migration plan.
type ExecutionResult struct {
	JobStatus           contracts.JobStatus
	AttemptStatus       contracts.AttemptStatus
	ExecutedSQL         string
	ErrorCode           string
	ErrorMessage        string
	CommittedBatches    int
	CommittedRows       int64
	TotalScopeRows      int64
	LastCommittedCursor string
	TransientRetryable  bool
}

// PlanExecutor executes a validated ExecutionPlan on a locked target connection.
type PlanExecutor interface {
	Execute(ctx context.Context, env ExecutionEnv) (ExecutionResult, error)
}

// PlanExecutorFunc adapts a function to the PlanExecutor interface.
type PlanExecutorFunc func(ctx context.Context, env ExecutionEnv) (ExecutionResult, error)

// Execute calls fn(ctx, env).
func (fn PlanExecutorFunc) Execute(ctx context.Context, env ExecutionEnv) (ExecutionResult, error) {
	return fn(ctx, env)
}

// WorkerConfig configures the multi-slot asynchronous migration worker.
type WorkerConfig struct {
	WorkerID          string
	Concurrency       int
	PollInterval      time.Duration
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	ExecutionTimeout  time.Duration
	ShutdownTimeout   time.Duration
	Queue             JobQueue
	ConnProvider      ExecutorConnProvider
	HeadReader        HeadStateReader
	Revalidator       contracts.OperationRevalidator
	Executor          PlanExecutor
	Logger            *slog.Logger
}

// Worker orchestrates bounded concurrent job claiming, lease heartbeats,
// target session advisory locking, preflight revalidation, and execution.
type Worker struct {
	cfg WorkerConfig
}

// NewWorker validates configuration and returns a ready-to-run Worker.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.Queue == nil {
		return nil, errors.New("worker job queue is required")
	}
	if cfg.ConnProvider == nil {
		return nil, errors.New("worker executor connection provider is required")
	}
	cfg.WorkerID = strings.TrimSpace(cfg.WorkerID)
	if cfg.WorkerID == "" {
		cfg.WorkerID = "worker-default"
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 2
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 30 * time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = cfg.LeaseDuration / 3
		if cfg.HeartbeatInterval <= 0 {
			cfg.HeartbeatInterval = 1 * time.Second
		}
	}
	if cfg.ExecutionTimeout <= 0 {
		cfg.ExecutionTimeout = 10 * time.Minute
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 15 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Executor == nil {
		cfg.Executor = defaultNoopExecutor{}
	}
	return &Worker{cfg: cfg}, nil
}

type defaultNoopExecutor struct{}

func (defaultNoopExecutor) Execute(_ context.Context, env ExecutionEnv) (ExecutionResult, error) {
	sqlText, err := env.Claimed.Job.Plan.SelectedSQL()
	if err != nil {
		return ExecutionResult{
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "INVALID_PLAN",
			ErrorMessage:  err.Error(),
		}, nil
	}
	return ExecutionResult{
		JobStatus:     contracts.JobStatusSuccess,
		AttemptStatus: contracts.AttemptStatusCommitted,
		ExecutedSQL:   sqlText,
	}, nil
}

// Run starts N concurrent worker slots and blocks until ctx is cancelled and
// all in-flight attempts finish or ShutdownTimeout expires.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil {
		return errors.New("worker is nil")
	}

	var wg sync.WaitGroup
	for slot := 1; slot <= w.cfg.Concurrency; slot++ {
		slotID := fmt.Sprintf("%s-slot-%d", w.cfg.WorkerID, slot)
		wg.Add(1)
		go func(workerSlotID string) {
			defer wg.Done()
			w.runSlot(ctx, workerSlotID)
		}(slotID)
	}

	<-ctx.Done()
	wg.Wait()
	return nil
}

func (w *Worker) runSlot(ctx context.Context, slotWorkerID string) {
	timer := time.NewTimer(w.cfg.PollInterval)
	defer timer.Stop()

	for {
		if ctx.Err() != nil {
			return
		}

		processed, err := w.ProcessOnce(ctx, slotWorkerID)
		if err != nil && ctx.Err() == nil {
			w.cfg.Logger.ErrorContext(ctx,
				"worker.poll.failed",
				slog.String("worker_id", slotWorkerID),
				slog.Any("error", err),
			)
		}

		if processed {
			// Immediately check if another due job is available without sleeping.
			continue
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(w.cfg.PollInterval)

		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// ProcessOnce claims and processes at most one due job using the given slotWorkerID.
// It returns processed=true if a job was claimed, or processed=false when no job is due.
func (w *Worker) ProcessOnce(ctx context.Context, slotWorkerID string) (processed bool, processErr error) {
	if w == nil {
		return false, errors.New("worker is nil")
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	slotWorkerID = strings.TrimSpace(slotWorkerID)
	if slotWorkerID == "" {
		slotWorkerID = w.cfg.WorkerID
	}

	claimed, err := w.cfg.Queue.ClaimNextDue(ctx, slotWorkerID, w.cfg.LeaseDuration)
	if err != nil {
		if errors.Is(err, jobs.ErrNoJobAvailable) {
			return false, nil
		}
		return false, err
	}
	ctx = telemetry.Extract(ctx, claimed.Job.TraceCarrier)
	ctx = telemetry.WithFields(ctx, telemetry.Fields{
		ActorID: string(claimed.Job.ActorID), DatabaseID: string(claimed.Job.DatabaseID),
		MigrationID: string(claimed.Job.Plan.MigrationID), RevisionID: string(claimed.Job.Plan.RevisionID),
		JobID: string(claimed.Job.ID), ExecutionID: string(claimed.Job.ExecutionID),
		AttemptID: string(claimed.Attempt.ID), Attempt: claimed.Attempt.AttemptNo,
		Direction: string(claimed.Job.Direction),
	})
	ctx, span := telemetry.StartSpan(ctx, "worker.attempt", trace.WithSpanKind(trace.SpanKindConsumer))
	defer func() { telemetry.FinishSpan(span, processErr) }()
	w.cfg.Logger.InfoContext(ctx, "worker.attempt.start", "worker_id", slotWorkerID, "status", "STARTED")

	// Detach from poll loop cancellation so an in-flight attempt gets a bounded
	// graceful shutdown window to finish or roll back cleanly.
	attemptBaseCtx, cancelAttemptBase := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.ExecutionTimeout)
	defer cancelAttemptBase()

	stopShutdownWatch := context.AfterFunc(ctx, func() {
		time.AfterFunc(w.cfg.ShutdownTimeout, cancelAttemptBase)
	})
	defer stopShutdownWatch()

	attemptCtx, cancelAttempt := context.WithCancel(attemptBaseCtx)
	defer cancelAttempt()

	stopHeartbeat := w.startHeartbeatLoop(attemptCtx, cancelAttempt, claimed.Job.ID, claimed.Job.LeaseToken)
	defer stopHeartbeat()

	plan := claimed.Job.Plan
	if err := plan.Validate(); err != nil {
		_, _, completeErr := w.completeAttempt(attemptBaseCtx, jobs.CompleteAttemptInput{
			JobID:         claimed.Job.ID,
			AttemptID:     claimed.Attempt.ID,
			LeaseToken:    claimed.Job.LeaseToken,
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "INVALID_PLAN",
			ErrorMessage:  err.Error(),
		})
		return true, completeErr
	}

	conn, connRev, err := w.cfg.ConnProvider.AcquireExecutorConn(attemptCtx, claimed.Job.DatabaseID)
	if err != nil {
		_, _, retryErr := w.scheduleRetry(attemptBaseCtx, jobs.ScheduleRetryInput{
			JobID:         claimed.Job.ID,
			AttemptID:     claimed.Attempt.ID,
			LeaseToken:    claimed.Job.LeaseToken,
			AttemptStatus: contracts.AttemptStatusFailed,
			ErrorCode:     "TARGET_CONNECTION_ERROR",
			ErrorMessage:  err.Error(),
		})
		return true, retryErr
	}

	if plan.TargetConnectionRevision > 0 && connRev != plan.TargetConnectionRevision {
		_ = conn.Close()
		_, _, completeErr := w.completeAttempt(attemptBaseCtx, jobs.CompleteAttemptInput{
			JobID:         claimed.Job.ID,
			AttemptID:     claimed.Attempt.ID,
			LeaseToken:    claimed.Job.LeaseToken,
			JobStatus:     contracts.JobStatusFailed,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     "STALE_CONNECTION_REVISION",
			ErrorMessage:  contracts.ErrStaleApproval.Error(),
		})
		return true, completeErr
	}

	targetLock, err := AcquireTargetSessionLock(attemptCtx, conn)
	if err != nil {
		if !errors.Is(err, ErrTargetLockBusy) {
			discardPhysicalConn(conn)
		}
		_ = conn.Close()

		errCode := "TARGET_LOCK_ERROR"
		if errors.Is(err, ErrTargetLockBusy) {
			errCode = "TARGET_LOCK_BUSY"
		}
		_, _, retryErr := w.scheduleRetry(attemptBaseCtx, jobs.ScheduleRetryInput{
			JobID:         claimed.Job.ID,
			AttemptID:     claimed.Attempt.ID,
			LeaseToken:    claimed.Job.LeaseToken,
			AttemptStatus: contracts.AttemptStatusFailed,
			ErrorCode:     errCode,
			ErrorMessage:  err.Error(),
		})
		return true, retryErr
	}
	defer func() {
		_ = targetLock.Release(attemptBaseCtx)
	}()

	var currentHead contracts.ActiveHeadState
	if w.cfg.HeadReader != nil {
		currentHead, err = w.cfg.HeadReader.ReadActiveHeadOnConn(attemptCtx, targetLock.Conn())
		if err != nil {
			_, _, retryErr := w.scheduleRetry(attemptBaseCtx, jobs.ScheduleRetryInput{
				JobID:         claimed.Job.ID,
				AttemptID:     claimed.Attempt.ID,
				LeaseToken:    claimed.Job.LeaseToken,
				AttemptStatus: contracts.AttemptStatusFailed,
				ErrorCode:     "READ_HEAD_ERROR",
				ErrorMessage:  err.Error(),
			})
			return true, retryErr
		}
	}

	if status, code, msg, rejected := w.revalidateUnderLock(attemptCtx, plan, currentHead); rejected {
		_, _, completeErr := w.completeAttempt(attemptBaseCtx, jobs.CompleteAttemptInput{
			JobID:         claimed.Job.ID,
			AttemptID:     claimed.Attempt.ID,
			LeaseToken:    claimed.Job.LeaseToken,
			JobStatus:     status,
			AttemptStatus: contracts.AttemptStatusRolledBack,
			ErrorCode:     code,
			ErrorMessage:  msg,
		})
		return true, completeErr
	}

	execCtx, execSpan := telemetry.StartSpan(attemptCtx, "worker.execute")
	execResult, execErr := w.cfg.Executor.Execute(execCtx, ExecutionEnv{
		Claimed:    claimed,
		Conn:       targetLock.Conn(),
		Lock:       targetLock,
		HeadBefore: currentHead,
	})
	spanErr := execErr
	if spanErr == nil && (execResult.TransientRetryable || execResult.JobStatus != "" && execResult.JobStatus != contracts.JobStatusSuccess) {
		spanErr = errors.New("execution did not succeed")
	}
	telemetry.FinishSpan(execSpan, spanErr)
	if execResult.TransientRetryable {
		errMsg := execResult.ErrorMessage
		if errMsg == "" && execErr != nil {
			errMsg = execErr.Error()
		}
		errCode := execResult.ErrorCode
		if errCode == "" {
			errCode = "EXECUTION_TRANSIENT_ERROR"
		}
		_, _, retryErr := w.scheduleRetry(attemptBaseCtx, jobs.ScheduleRetryInput{
			JobID:         claimed.Job.ID,
			AttemptID:     claimed.Attempt.ID,
			LeaseToken:    claimed.Job.LeaseToken,
			AttemptStatus: contracts.AttemptStatusFailed,
			ExecutedSQL:   execResult.ExecutedSQL,
			ErrorCode:     errCode,
			ErrorMessage:  errMsg,
		})
		return true, retryErr
	}

	jobStatus := execResult.JobStatus
	attemptStatus := execResult.AttemptStatus
	errCode := execResult.ErrorCode
	errMsg := execResult.ErrorMessage

	if execErr != nil {
		if jobStatus == "" {
			jobStatus = contracts.JobStatusFailed
		}
		if attemptStatus == "" {
			attemptStatus = contracts.AttemptStatusRolledBack
		}
		if errCode == "" {
			errCode = "EXECUTION_FAILED"
		}
		if errMsg == "" {
			errMsg = execErr.Error()
		}
	} else {
		if jobStatus == "" {
			jobStatus = contracts.JobStatusSuccess
		}
		if attemptStatus == "" {
			attemptStatus = contracts.AttemptStatusCommitted
		}
	}

	_, _, completeErr := w.completeAttempt(attemptBaseCtx, jobs.CompleteAttemptInput{
		JobID:               claimed.Job.ID,
		AttemptID:           claimed.Attempt.ID,
		LeaseToken:          claimed.Job.LeaseToken,
		JobStatus:           jobStatus,
		AttemptStatus:       attemptStatus,
		ExecutedSQL:         execResult.ExecutedSQL,
		ErrorCode:           errCode,
		ErrorMessage:        errMsg,
		CommittedBatches:    execResult.CommittedBatches,
		CommittedRows:       execResult.CommittedRows,
		TotalScopeRows:      execResult.TotalScopeRows,
		LastCommittedCursor: execResult.LastCommittedCursor,
	})
	return true, completeErr
}

func (w *Worker) revalidateUnderLock(
	ctx context.Context,
	plan contracts.ExecutionPlan,
	currentHead contracts.ActiveHeadState,
) (status contracts.JobStatus, code string, message string, rejected bool) {
	ctx, span := telemetry.StartSpan(ctx, "worker.revalidate")
	defer func() {
		var err error
		if rejected {
			err = errors.New("revalidation rejected")
		}
		telemetry.FinishSpan(span, err)
	}()
	if currentHead.UnresolvedReason != "" {
		isRecovery := (plan.Direction == contracts.DirectionResume || plan.Direction == contracts.DirectionCompensate) &&
			(currentHead.UnresolvedReason == contracts.GateReasonPartial || currentHead.UnresolvedReason == contracts.GateReasonPartiallyReverted) &&
			(currentHead.UnresolvedExecutionID == "" || currentHead.UnresolvedExecutionID == plan.OriginExecutionID)
		if !isRecovery {
			return contracts.JobStatusBlocked, "EXECUTION_GATE_BLOCKED", contracts.ErrExecutionGateBlocked.Error(), true
		}
	}

	if w.cfg.HeadReader != nil {
		switch plan.Direction {
		case contracts.DirectionApply, contracts.DirectionRollback, contracts.DirectionForward:
			if currentHead.ExecutionID != plan.ExpectedActiveHead {
				return contracts.JobStatusFailed, "HEAD_MISMATCH", contracts.ErrHeadMismatch.Error(), true
			}
		}
	}

	if w.cfg.Revalidator != nil {
		if err := w.cfg.Revalidator.Revalidate(ctx, plan, currentHead); err != nil {
			switch {
			case errors.Is(err, contracts.ErrExecutionGateBlocked):
				return contracts.JobStatusBlocked, "EXECUTION_GATE_BLOCKED", err.Error(), true
			case errors.Is(err, contracts.ErrHeadMismatch):
				return contracts.JobStatusFailed, "HEAD_MISMATCH", err.Error(), true
			case errors.Is(err, contracts.ErrStaleApproval):
				return contracts.JobStatusFailed, "STALE_APPROVAL", err.Error(), true
			default:
				return contracts.JobStatusFailed, "REVALIDATION_FAILED", err.Error(), true
			}
		}
	}

	return "", "", "", false
}

func (w *Worker) startHeartbeatLoop(
	ctx context.Context,
	cancelAttempt context.CancelFunc,
	jobID contracts.JobID,
	leaseToken string,
) func() {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(w.cfg.HeartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, err := w.cfg.Queue.Heartbeat(ctx, jobID, leaseToken, w.cfg.LeaseDuration)
				if err != nil {
					w.cfg.Logger.WarnContext(ctx, "worker.heartbeat.failed", "status", "FAILED", "error", err)
				}
				if errors.Is(err, jobs.ErrLeaseMismatch) {
					cancelAttempt()
					return
				}
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stopCh)
			<-doneCh
		})
	}
}

