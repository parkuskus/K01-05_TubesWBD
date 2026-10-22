package migration

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

const ensureSchemaSQL = `
CREATE SCHEMA IF NOT EXISTS if3110_platform;

CREATE TABLE IF NOT EXISTS if3110_platform.migration_history (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    version BIGINT NOT NULL UNIQUE CHECK (version > 0),
    title TEXT NOT NULL DEFAULT '',
    sql_text TEXT NOT NULL CHECK (length(btrim(sql_text)) > 0),
    status TEXT NOT NULL CHECK (status IN ('DRAFT', 'APPLIED', 'FAILED')),
    created_by TEXT NOT NULL CHECK (length(btrim(created_by)) > 0),
    executed_by TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    executed_at TIMESTAMPTZ
);
`

// bikin schema dan tabel migration_history kalau belum ada
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return ErrNilDatabase
	}
	_, err := db.ExecContext(ctx, ensureSchemaSQL)
	return err
}

// store buat akses tabel migration_history di db target
type PostgresStore struct {
	db         *sql.DB
	databaseID contracts.DatabaseID
}

func NewPostgresStore(db *sql.DB, databaseID contracts.DatabaseID) (*PostgresStore, error) {
	if db == nil {
		return nil, ErrNilDatabase
	}
	return &PostgresStore{
		db:         db,
		databaseID: databaseID,
	}, nil
}

func (s *PostgresStore) EnsureSchema(ctx context.Context) error {
	return EnsureSchema(ctx, s.db)
}

// simpan draft baru dengan nomor versi berikutnya
func (s *PostgresStore) CreateDraft(ctx context.Context, title, sqlText string, createdBy contracts.UserID, now time.Time) (MigrationRecord, error) {
	cleanedSQL, err := normalizeSQL(sqlText)
	if err != nil {
		return MigrationRecord{}, err
	}
	if strings.TrimSpace(string(createdBy)) == "" {
		return MigrationRecord{}, ErrInvalidUser
	}
	cleanedTitle := normalizeTitle(title)
	if now.IsZero() {
		now = time.Now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MigrationRecord{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `LOCK TABLE if3110_platform.migration_history IN EXCLUSIVE MODE`); err != nil {
		return MigrationRecord{}, err
	}

	var nextVersion int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) + 1
		FROM if3110_platform.migration_history`).Scan(&nextVersion); err != nil {
		return MigrationRecord{}, err
	}

	row := tx.QueryRowContext(ctx, `
		INSERT INTO if3110_platform.migration_history (
			version, title, sql_text, status, created_by, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		RETURNING id, version, title, sql_text, status, created_by, executed_by, error_message, created_at, updated_at, executed_at`,
		nextVersion, cleanedTitle, cleanedSQL, string(StatusDraft), string(createdBy), now,
	)

	record, err := s.scanRecord(row)
	if err != nil {
		return MigrationRecord{}, err
	}

	if err := tx.Commit(); err != nil {
		return MigrationRecord{}, err
	}
	return record, nil
}

// ubah sql atau judul draft selama belum pernah dieksekusi
func (s *PostgresStore) UpdateDraft(ctx context.Context, version int64, title, sqlText string, now time.Time) (MigrationRecord, error) {
	if version <= 0 {
		return MigrationRecord{}, ErrInvalidVersion
	}
	cleanedSQL, err := normalizeSQL(sqlText)
	if err != nil {
		return MigrationRecord{}, err
	}
	cleanedTitle := normalizeTitle(title)
	if now.IsZero() {
		now = time.Now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MigrationRecord{}, err
	}
	defer tx.Rollback()

	var currentStatus string
	err = tx.QueryRowContext(ctx, `
		SELECT status
		FROM if3110_platform.migration_history
		WHERE version = $1
		FOR UPDATE`, version).Scan(&currentStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return MigrationRecord{}, ErrMigrationNotFound
	}
	if err != nil {
		return MigrationRecord{}, err
	}
	if Status(currentStatus) != StatusDraft {
		return MigrationRecord{}, ErrMigrationAlreadyApplied
	}

	row := tx.QueryRowContext(ctx, `
		UPDATE if3110_platform.migration_history
		SET title = $2, sql_text = $3, updated_at = $4
		WHERE version = $1
		RETURNING id, version, title, sql_text, status, created_by, executed_by, error_message, created_at, updated_at, executed_at`,
		version, cleanedTitle, cleanedSQL, now,
	)

	record, err := s.scanRecord(row)
	if err != nil {
		return MigrationRecord{}, err
	}

	if err := tx.Commit(); err != nil {
		return MigrationRecord{}, err
	}
	return record, nil
}

// ambil semua riwayat migration urut dari versi paling awal
func (s *PostgresStore) List(ctx context.Context) ([]MigrationRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, version, title, sql_text, status, created_by, executed_by, error_message, created_at, updated_at, executed_at
		FROM if3110_platform.migration_history
		ORDER BY version ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []MigrationRecord
	for rows.Next() {
		record, err := s.scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// ambil detail migration berdasarkan versi
func (s *PostgresStore) GetByVersion(ctx context.Context, version int64) (MigrationRecord, error) {
	if version <= 0 {
		return MigrationRecord{}, ErrInvalidVersion
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, version, title, sql_text, status, created_by, executed_by, error_message, created_at, updated_at, executed_at
		FROM if3110_platform.migration_history
		WHERE version = $1`, version)

	record, err := s.scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MigrationRecord{}, ErrMigrationNotFound
	}
	if err != nil {
		return MigrationRecord{}, err
	}
	return record, nil
}

// ambil detail migration berdasarkan id
func (s *PostgresStore) GetByID(ctx context.Context, id int64) (MigrationRecord, error) {
	if id <= 0 {
		return MigrationRecord{}, ErrMigrationNotFound
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, version, title, sql_text, status, created_by, executed_by, error_message, created_at, updated_at, executed_at
		FROM if3110_platform.migration_history
		WHERE id = $1`, id)

	record, err := s.scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MigrationRecord{}, ErrMigrationNotFound
	}
	if err != nil {
		return MigrationRecord{}, err
	}
	return record, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *PostgresStore) scanRecord(scanner rowScanner) (MigrationRecord, error) {
	var (
		record     MigrationRecord
		statusStr  string
		createdBy  string
		executedBy string
		executedAt sql.NullTime
	)
	err := scanner.Scan(
		&record.ID,
		&record.Version,
		&record.Title,
		&record.SQLText,
		&statusStr,
		&createdBy,
		&executedBy,
		&record.ErrorMessage,
		&record.CreatedAt,
		&record.UpdatedAt,
		&executedAt,
	)
	if err != nil {
		return MigrationRecord{}, err
	}
	record.DatabaseID = s.databaseID
	record.Status = Status(statusStr)
	record.CreatedBy = contracts.UserID(createdBy)
	record.ExecutedBy = contracts.UserID(executedBy)
	if executedAt.Valid {
		t := executedAt.Time
		record.ExecutedAt = &t
	}
	return record, nil
}
