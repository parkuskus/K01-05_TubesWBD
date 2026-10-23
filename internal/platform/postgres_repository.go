package platform

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

type postgresRepository struct{ db *sql.DB }

func dbError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return ErrConflict
		case "23503":
			return ErrNotFound
		}
	}
	return err
}

func requireAdmin(ctx context.Context, tx *sql.Tx, actor int64) error {
	var admin bool
	err := tx.QueryRowContext(ctx, `SELECT is_platform_admin FROM users WHERE id = $1`, actor).Scan(&admin)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !admin {
		return contracts.ErrForbidden
	}
	return err
}

// Transactions serialize administrative changes, including the last-admin check.
func (p *postgresRepository) mutate(ctx context.Context, actor int64, fn func(*sql.Tx) error) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `LOCK TABLE users IN EXCLUSIVE MODE`); err != nil {
		return err
	}
	if err := requireAdmin(ctx, tx, actor); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return dbError(err)
	}
	return tx.Commit()
}

func (p *postgresRepository) listUsers(ctx context.Context, actor int64) ([]User, error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireAdmin(ctx, tx, actor); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, username, is_platform_admin FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var users []User
	for rows.Next() {
		var u User
		var id int64
		if err := rows.Scan(&id, &u.Username, &u.IsPlatformAdmin); err != nil {
			rows.Close()
			return nil, err
		}
		u.ID = contracts.UserID(strconv.FormatInt(id, 10))
		users = append(users, u)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return users, nil
}

func (p *postgresRepository) createUser(ctx context.Context, actor int64, username, hash string) (contracts.UserID, error) {
	var id int64
	err := p.mutate(ctx, actor, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO users (username, password_hash) VALUES ($1, $2) RETURNING id`, username, hash).Scan(&id)
	})
	if err != nil {
		return "", err
	}
	return contracts.UserID(strconv.FormatInt(id, 10)), nil
}

func updateUser(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func revokeSessions(ctx context.Context, tx *sql.Tx, user int64) error {
	_, err := tx.ExecContext(ctx, `WITH revoked AS (
		UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL RETURNING id
	) DELETE FROM session_tokens USING revoked WHERE session_tokens.session_id = revoked.id`, strconv.FormatInt(user, 10), time.Now())
	return err
}

func (p *postgresRepository) updateUsername(ctx context.Context, actor, user int64, username string) error {
	return p.mutate(ctx, actor, func(tx *sql.Tx) error {
		return updateUser(ctx, tx, `UPDATE users SET username = $2 WHERE id = $1`, user, username)
	})
}

func (p *postgresRepository) setPassword(ctx context.Context, actor, user int64, hash string) error {
	return p.mutate(ctx, actor, func(tx *sql.Tx) error {
		if err := updateUser(ctx, tx, `UPDATE users SET password_hash = $2 WHERE id = $1`, user, hash); err != nil {
			return err
		}
		return revokeSessions(ctx, tx, user)
	})
}

func ensureAnotherAdmin(ctx context.Context, tx *sql.Tx, user int64) error {
	var last bool
	if err := tx.QueryRowContext(ctx, `SELECT is_platform_admin AND NOT EXISTS (SELECT 1 FROM users WHERE id <> $1 AND is_platform_admin) FROM users WHERE id = $1`, user).Scan(&last); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if last {
		return ErrConflict
	}
	return nil
}

func (p *postgresRepository) setPlatformAdmin(ctx context.Context, actor, user int64, admin bool) error {
	return p.mutate(ctx, actor, func(tx *sql.Tx) error {
		if !admin {
			if err := ensureAnotherAdmin(ctx, tx, user); err != nil {
				return err
			}
		}
		if err := updateUser(ctx, tx, `UPDATE users SET is_platform_admin = $2 WHERE id = $1`, user, admin); err != nil {
			return err
		}
		return revokeSessions(ctx, tx, user)
	})
}

func (p *postgresRepository) deleteUser(ctx context.Context, actor, user int64) error {
	return p.mutate(ctx, actor, func(tx *sql.Tx) error {
		if err := ensureAnotherAdmin(ctx, tx, user); err != nil {
			return err
		}
		if err := revokeSessions(ctx, tx, user); err != nil {
			return err
		}
		return updateUser(ctx, tx, `DELETE FROM users WHERE id = $1`, user)
	})
}
