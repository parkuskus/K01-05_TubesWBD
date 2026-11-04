package contracts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type MigrationID string
type RevisionID string
type OperationRequestID string
type JobID string
type ExecutionID string
type AttemptID string

type OperationDirection string

const (
	DirectionApply      OperationDirection = "APPLY"
	DirectionRollback   OperationDirection = "ROLLBACK"
	DirectionForward    OperationDirection = "FORWARD"
	DirectionResume     OperationDirection = "RESUME"
	DirectionCompensate OperationDirection = "COMPENSATE"
)

func (d OperationDirection) IsValid() bool {
	switch d {
	case DirectionApply, DirectionRollback, DirectionForward, DirectionResume, DirectionCompensate:
		return true
	default:
		return false
	}
}

type ExecutionMode string

const (
	ModeNormal ExecutionMode = "NORMAL"
	ModeBatch  ExecutionMode = "BATCH"
)

func (m ExecutionMode) IsValid() bool {
	switch m {
	case ModeNormal, ModeBatch:
		return true
	default:
		return false
	}
}

type GateReason string

const (
	GateReasonPartial           GateReason = "PARTIAL"
	GateReasonPartiallyReverted GateReason = "PARTIALLY_REVERTED"
	GateReasonUnknown           GateReason = "UNKNOWN"
)

func (r GateReason) IsValid() bool {
	switch r {
	case GateReasonPartial, GateReasonPartiallyReverted, GateReasonUnknown:
		return true
	default:
		return false
	}
}

var (
	ErrStaleApproval         = errors.New("operation approval or policy version is stale")
	ErrHeadMismatch          = errors.New("target active head does not match expected head")
	ErrIrreversibleMigration = errors.New("migration is irreversible and cannot be rolled back")
	ErrExecutionGateBlocked  = errors.New("target database is blocked by an unresolved execution gate")
	ErrIdempotencyConflict   = errors.New("idempotency key already used with different payload")
	ErrInvalidExecutionPlan  = errors.New("invalid execution plan")
)

// BatchConfig defines the structured batch execution scope and keyset parameters.
type BatchConfig struct {
	TableSchema      string
	TableName        string
	PrimaryKeyColumn string
	ScopeWhere       string
	BatchSize        int
}

func (b BatchConfig) Validate() error {
	if strings.TrimSpace(b.TableSchema) == "" ||
		strings.TrimSpace(b.TableName) == "" ||
		strings.TrimSpace(b.PrimaryKeyColumn) == "" ||
		b.BatchSize <= 0 {
		return ErrInvalidExecutionPlan
	}
	return nil
}

// ExecutionPlan is an immutable, checksum-sealed plan produced after approval
// and consumed by the asynchronous worker and target transaction executor.
type ExecutionPlan struct {
	RequestID                OperationRequestID
	ExecutionID              ExecutionID
	MigrationID              MigrationID
	RevisionID               RevisionID
	Version                  int64
	Direction                OperationDirection
	Mode                     ExecutionMode
	ActorID                  UserID
	TargetDatabaseID         DatabaseID
	TargetConnectionRevision int64
	ExpectedActiveHead       ExecutionID
	PolicyVersion            int64
	UpSQL                    string
	DownSQL                  string
	IsIrreversible           bool
	IrreversibleReason       string
	OriginExecutionID        ExecutionID
	Batch                    BatchConfig
	PlanChecksum             string
}

// ComputeChecksum calculates a deterministic SHA-256 digest covering all
// security-critical and execution-critical fields of the plan.
func (p ExecutionPlan) ComputeChecksum() string {
	payload := fmt.Sprintf(
		"req=%s\x00mig=%s\x00rev=%s\x00ver=%d\x00dir=%s\x00mode=%s\x00db=%s\x00conn_rev=%d\x00head=%s\x00policy=%d\x00up=%s\x00down=%s\x00irrev=%t\x00reason=%s\x00origin=%s\x00batch=%s.%s:%s:%s:%d",
		p.RequestID,
		p.MigrationID,
		p.RevisionID,
		p.Version,
		p.Direction,
		p.Mode,
		p.TargetDatabaseID,
		p.TargetConnectionRevision,
		p.ExpectedActiveHead,
		p.PolicyVersion,
		strings.TrimSpace(p.UpSQL),
		strings.TrimSpace(p.DownSQL),
		p.IsIrreversible,
		strings.TrimSpace(p.IrreversibleReason),
		p.OriginExecutionID,
		strings.TrimSpace(p.Batch.TableSchema),
		strings.TrimSpace(p.Batch.TableName),
		strings.TrimSpace(p.Batch.PrimaryKeyColumn),
		strings.TrimSpace(p.Batch.ScopeWhere),
		p.Batch.BatchSize,
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// Validate checks structural invariants, up/down XOR irreversible rules,
// direction constraints, batch configuration, and checksum integrity.
func (p ExecutionPlan) Validate() error {
	if strings.TrimSpace(string(p.RequestID)) == "" ||
		strings.TrimSpace(string(p.ExecutionID)) == "" ||
		strings.TrimSpace(string(p.MigrationID)) == "" ||
		strings.TrimSpace(string(p.RevisionID)) == "" ||
		p.Version <= 0 ||
		strings.TrimSpace(string(p.ActorID)) == "" ||
		strings.TrimSpace(string(p.TargetDatabaseID)) == "" ||
		p.TargetConnectionRevision <= 0 ||
		p.PolicyVersion <= 0 {
		return ErrInvalidExecutionPlan
	}
	if !p.Direction.IsValid() || !p.Mode.IsValid() {
		return ErrInvalidExecutionPlan
	}

	hasUp := strings.TrimSpace(p.UpSQL) != ""
	hasDown := strings.TrimSpace(p.DownSQL) != ""
	hasReason := strings.TrimSpace(p.IrreversibleReason) != ""

	if !hasUp {
		return ErrInvalidExecutionPlan
	}
	if p.IsIrreversible {
		if hasDown || !hasReason {
			return ErrInvalidExecutionPlan
		}
	} else {
		if !hasDown || hasReason {
			return ErrInvalidExecutionPlan
		}
	}

	switch p.Direction {
	case DirectionRollback, DirectionCompensate:
		if p.IsIrreversible {
			return ErrIrreversibleMigration
		}
		if strings.TrimSpace(string(p.OriginExecutionID)) == "" {
			return ErrInvalidExecutionPlan
		}
	case DirectionResume:
		if strings.TrimSpace(string(p.OriginExecutionID)) == "" {
			return ErrInvalidExecutionPlan
		}
	}

	if p.Mode == ModeBatch {
		if err := p.Batch.Validate(); err != nil {
			return err
		}
	}

	if strings.TrimSpace(p.PlanChecksum) != "" && p.PlanChecksum != p.ComputeChecksum() {
		return ErrInvalidExecutionPlan
	}
	return nil
}

// SelectedSQL returns the immutable SQL text to execute for the plan's direction.
func (p ExecutionPlan) SelectedSQL() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	switch p.Direction {
	case DirectionApply, DirectionForward, DirectionResume:
		return strings.TrimSpace(p.UpSQL), nil
	case DirectionRollback, DirectionCompensate:
		return strings.TrimSpace(p.DownSQL), nil
	default:
		return "", ErrInvalidExecutionPlan
	}
}

// ActiveHeadState represents the current committed migration position and
// any unresolved gate on the target database.
type ActiveHeadState struct {
	MigrationID           MigrationID
	ExecutionID           ExecutionID
	Version               int64
	UnresolvedExecutionID ExecutionID
	UnresolvedReason      GateReason
}

// OperationRevalidator is called by the worker after acquiring the target
// database lock to verify that approvals, policy version, and expected head
// remain valid right before SQL execution.
type OperationRevalidator interface {
	Revalidate(ctx context.Context, plan ExecutionPlan, currentHead ActiveHeadState) error
}

// CommitHistoryEvent represents a single append-only committed migration event
// stored in the target database's migration_history_v2 ledger.
type CommitHistoryEvent struct {
	ID                 int64
	ExecutionID        ExecutionID
	JobID              JobID
	RequestID          OperationRequestID
	MigrationID        MigrationID
	RevisionID         RevisionID
	Version            int64
	Direction          OperationDirection
	Mode               ExecutionMode
	ExecutedSQL        string
	PlanChecksum       string
	ActorID            UserID
	HeadBefore         ExecutionID
	HeadAfter          ExecutionID
	RevertsExecutionID ExecutionID
	CommittedAt        time.Time
}

// BatchCheckpointState reports the durable committed batch progress on a target database.
type BatchCheckpointState struct {
	ExecutionID          ExecutionID
	TotalScopeRows       int64
	CommittedBatches     int
	CommittedRows        int64
	LastCommittedCursor  string
	CompensatedBatches   int
	LastCompensatedBatch int
	UpdatedAt            time.Time
}

// CommitEvidenceReader reads committed migration history, active head state,
// and durable batch checkpoints from a target database without executing DDL.
type CommitEvidenceReader interface {
	ActiveHead(ctx context.Context, databaseID DatabaseID) (ActiveHeadState, error)
	ListCommitHistory(ctx context.Context, databaseID DatabaseID) ([]CommitHistoryEvent, error)
	BatchProgress(ctx context.Context, databaseID DatabaseID, executionID ExecutionID) (BatchCheckpointState, error)
}

