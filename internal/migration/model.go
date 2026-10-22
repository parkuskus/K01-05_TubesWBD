package migration

import (
	"errors"
	"strings"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

const (
	// schema khusus di db target buat nyimpen riwayat migration
	SchemaName = "if3110_platform"

	// tabel riwayat migration linear
	HistoryTableName = "migration_history"
)

// status eksekusi migration
type Status string

const (
	StatusDraft   Status = "DRAFT"
	StatusApplied Status = "APPLIED"
	StatusFailed  Status = "FAILED"
)

var (
	ErrNilDatabase             = errors.New("database connection is required")
	ErrInvalidServiceConfig    = errors.New("migration service requires authorizer and database provider")
	ErrInvalidDatabaseID       = errors.New("database ID is required")
	ErrMigrationNotFound       = errors.New("migration not found")
	ErrInvalidSQL              = errors.New("migration SQL must not be empty")
	ErrInvalidUser             = errors.New("user ID is required")
	ErrInvalidVersion          = errors.New("migration version must be positive")
	ErrMigrationAlreadyApplied = errors.New("migration has already been executed and cannot be modified")
	ErrVersionConflict         = errors.New("migration version conflict")
)

// cek apakah status migration valid
func (s Status) IsValid() bool {
	switch s {
	case StatusDraft, StatusApplied, StatusFailed:
		return true
	default:
		return false
	}
}

// data satu entri migration di db target
type MigrationRecord struct {
	ID           int64
	DatabaseID   contracts.DatabaseID
	Version      int64
	Title        string
	SQLText      string
	Status       Status
	CreatedBy    contracts.UserID
	ExecutedBy   contracts.UserID
	ErrorMessage string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ExecutedAt   *time.Time
}

func normalizeSQL(sqlText string) (string, error) {
	trimmed := strings.TrimSpace(sqlText)
	if trimmed == "" {
		return "", ErrInvalidSQL
	}
	return trimmed, nil
}

func normalizeTitle(title string) string {
	return strings.TrimSpace(title)
}

