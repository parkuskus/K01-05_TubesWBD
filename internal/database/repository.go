package database

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

type connectionRecord struct {
	Connection
	ciphertext []byte
	revision   int64
}

type connectionStore interface {
	create(context.Context, ConnectionInput, []byte) (Connection, error)
	get(context.Context, contracts.DatabaseID) (connectionRecord, error)
	list(context.Context) ([]Connection, error)
	update(context.Context, contracts.DatabaseID, ConnectionInput, []byte) (Connection, error)
	delete(context.Context, contracts.DatabaseID) error
}

type postgresStore struct{ db *sql.DB }

const metadataColumns = `id::text, name, host, port, database_name, username, ssl_mode, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanConnection(row scanner) (Connection, error) {
	var c Connection
	err := row.Scan(&c.ID, &c.Name, &c.Host, &c.Port, &c.Database, &c.Username, &c.SSLMode, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func (s *postgresStore) create(ctx context.Context, in ConnectionInput, ciphertext []byte) (Connection, error) {
	return scanConnection(s.db.QueryRowContext(ctx, `INSERT INTO database_connections
		(name, host, port, database_name, username, ssl_mode, password_ciphertext)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+metadataColumns,
		in.Name, in.Host, in.Port, in.Database, in.Username, in.SSLMode, ciphertext))
}

func (s *postgresStore) get(ctx context.Context, id contracts.DatabaseID) (connectionRecord, error) {
	var r connectionRecord
	err := s.db.QueryRowContext(ctx, `SELECT `+metadataColumns+`, password_ciphertext, revision
		FROM database_connections WHERE id = $1`, id).Scan(
		&r.ID, &r.Name, &r.Host, &r.Port, &r.Database, &r.Username, &r.SSLMode,
		&r.CreatedAt, &r.UpdatedAt, &r.ciphertext, &r.revision)
	return r, err
}

func (s *postgresStore) list(ctx context.Context) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+metadataColumns+` FROM database_connections ORDER BY lower(name), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Connection, 0)
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

func (s *postgresStore) update(ctx context.Context, id contracts.DatabaseID, in ConnectionInput, ciphertext []byte) (Connection, error) {
	// A nil ciphertext keeps the existing password atomically, including across processes.
	return scanConnection(s.db.QueryRowContext(ctx, `UPDATE database_connections SET
		name=$2, host=$3, port=$4, database_name=$5, username=$6, ssl_mode=$7,
		password_ciphertext=COALESCE($8, password_ciphertext), revision=revision+1, updated_at=NOW()
		WHERE id=$1 RETURNING `+metadataColumns,
		id, in.Name, in.Host, in.Port, in.Database, in.Username, in.SSLMode, ciphertext))
}

func (s *postgresStore) delete(ctx context.Context, id contracts.DatabaseID) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM database_connections WHERE id=$1`, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}

func storageError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return ErrConflict
		case "23503":
			return ErrInUse
		}
	}
	return ErrUnavailable
}
