package approval

import (
	"errors"
	"time"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/definition"
)

var (
	ErrInvalidInput  = errors.New("invalid operation request or approval")
	ErrNotFound      = errors.New("operation request not found")
	ErrConflict      = errors.New("operation request state conflict")
	ErrMissingLeader = errors.New("every granted group must have a leader")
	ErrStale         = contracts.ErrStaleApproval
)

type Vote struct {
	ActorID   contracts.UserID `json:"actor_id"`
	Decision  string           `json:"decision"`
	CreatedAt time.Time        `json:"created_at"`
}

type Requirement struct {
	GroupID        contracts.GroupID     `json:"group_id"`
	GroupName      string                `json:"group_name"`
	LeaderUserID   contracts.UserID      `json:"leader_user_id"`
	LeaderUsername string                `json:"leader_username"`
	GrantLevel     contracts.AccessLevel `json:"grant_level"`
	Vote           *Vote                 `json:"vote,omitempty"`
}

type Request struct {
	ID                 contracts.OperationRequestID `json:"id"`
	DatabaseID         contracts.DatabaseID         `json:"database_id"`
	MigrationID        contracts.MigrationID        `json:"migration_id"`
	Revision           int                          `json:"revision"`
	Direction          contracts.OperationDirection `json:"direction"`
	ExpectedHead       contracts.ExecutionID        `json:"expected_head"`
	ConnectionRevision int64                        `json:"connection_revision"`
	PolicyVersion      int64                        `json:"policy_version"`
	RevisionChecksum   string                       `json:"-"`
	Checksum           string                       `json:"checksum"`
	Status             string                       `json:"status"`
	Round              int                          `json:"review_round"`
	CreatedBy          contracts.UserID             `json:"created_by"`
	CreatedAt          time.Time                    `json:"created_at"`
	Migration          definition.Definition        `json:"migration"`
	Requirements       []Requirement                `json:"requirements"`
}
