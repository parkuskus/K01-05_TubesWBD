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

func revokeGroupSessions(ctx context.Context, tx *sql.Tx, group int64) error {
	_, err := tx.ExecContext(ctx, `WITH revoked AS (
		UPDATE sessions SET revoked_at = $2
		WHERE user_id IN (SELECT user_id::text FROM group_memberships WHERE group_id = $1)
		  AND revoked_at IS NULL RETURNING id
	) DELETE FROM session_tokens USING revoked WHERE session_tokens.session_id = revoked.id`, group, time.Now())
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

func (p *postgresRepository) listGroups(ctx context.Context, actor int64) ([]Group, error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireAdmin(ctx, tx, actor); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var groups []Group
	for rows.Next() {
		var group Group
		var id int64
		if err := rows.Scan(&id, &group.Name); err != nil {
			rows.Close()
			return nil, err
		}
		group.ID = contracts.GroupID(strconv.FormatInt(id, 10))
		groups = append(groups, group)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return groups, nil
}

func (p *postgresRepository) createGroup(ctx context.Context, actor int64, name string) (contracts.GroupID, error) {
	var id int64
	err := p.mutate(ctx, actor, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO groups (name) VALUES ($1) RETURNING id`, name).Scan(&id)
	})
	if err != nil {
		return "", err
	}
	return contracts.GroupID(strconv.FormatInt(id, 10)), nil
}

func (p *postgresRepository) renameGroup(ctx context.Context, actor, group int64, name string) error {
	return p.mutate(ctx, actor, func(tx *sql.Tx) error {
		return updateUser(ctx, tx, `UPDATE groups SET name = $2 WHERE id = $1`, group, name)
	})
}

func (p *postgresRepository) deleteGroup(ctx context.Context, actor, group int64) error {
	return p.mutate(ctx, actor, func(tx *sql.Tx) error {
		if err := revokeGroupSessions(ctx, tx, group); err != nil {
			return err
		}
		return updateUser(ctx, tx, `DELETE FROM groups WHERE id = $1`, group)
	})
}

func (p *postgresRepository) listMembers(ctx context.Context, actor, group int64) ([]User, error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireAdmin(ctx, tx, actor); err != nil {
		return nil, err
	}
	var exists int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM groups WHERE id = $1`, group).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT u.id, u.username, u.is_platform_admin
		FROM group_memberships AS m JOIN users AS u ON u.id = m.user_id
		WHERE m.group_id = $1 ORDER BY u.id`, group)
	if err != nil {
		return nil, err
	}
	var users []User
	for rows.Next() {
		var user User
		var id int64
		if err := rows.Scan(&id, &user.Username, &user.IsPlatformAdmin); err != nil {
			rows.Close()
			return nil, err
		}
		user.ID = contracts.UserID(strconv.FormatInt(id, 10))
		users = append(users, user)
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

func (p *postgresRepository) setMembership(ctx context.Context, actor, user, group int64, member bool) error {
	return p.mutate(ctx, actor, func(tx *sql.Tx) error {
		var groupExists, userExists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM groups WHERE id = $1), EXISTS(SELECT 1 FROM users WHERE id = $2)`, group, user).Scan(&groupExists, &userExists); err != nil {
			return err
		}
		if !groupExists || !userExists {
			return ErrNotFound
		}
		if member {
			result, err := tx.ExecContext(ctx, `INSERT INTO group_memberships (group_id, user_id) VALUES ($1, $2) ON CONFLICT (group_id, user_id) DO NOTHING`, group, user)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed > 0 {
				return revokeSessions(ctx, tx, user)
			}
			return nil
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM group_memberships WHERE group_id = $1 AND user_id = $2`, group, user)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed > 0 {
			return revokeSessions(ctx, tx, user)
		}
		return nil
	})
}

func (p *postgresRepository) listGrants(ctx context.Context, actor, group int64) ([]Grant, error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireAdmin(ctx, tx, actor); err != nil {
		return nil, err
	}
	var exists int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM groups WHERE id = $1`, group).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT database_id, level FROM group_database_access WHERE group_id = $1 ORDER BY database_id`, group)
	if err != nil {
		return nil, err
	}
	var grants []Grant
	for rows.Next() {
		var database int64
		var level int64
		if err := rows.Scan(&database, &level); err != nil {
			rows.Close()
			return nil, err
		}
		if level < int64(contracts.AccessViewOnly) || level > int64(contracts.AccessViewEdit) {
			rows.Close()
			return nil, ErrInvalidInput
		}
		grants = append(grants, Grant{GroupID: contracts.GroupID(strconv.FormatInt(group, 10)), DatabaseID: contracts.DatabaseID(strconv.FormatInt(database, 10)), Level: contracts.AccessLevel(level)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return grants, nil
}

func (p *postgresRepository) setGrant(ctx context.Context, actor, group, database int64, level contracts.AccessLevel) error {
	return p.mutate(ctx, actor, func(tx *sql.Tx) error {
		var exists int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM groups WHERE id = $1`, group).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var previous int64
		err := tx.QueryRowContext(ctx, `SELECT level FROM group_database_access WHERE group_id = $1 AND database_id = $2`, group, database).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if level == contracts.AccessNone {
			if _, err := tx.ExecContext(ctx, `DELETE FROM group_database_access WHERE group_id = $1 AND database_id = $2`, group, database); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `INSERT INTO group_database_access (group_id, database_id, level) VALUES ($1, $2, $3)
			ON CONFLICT (group_id, database_id) DO UPDATE SET level = EXCLUDED.level`, group, database, level); err != nil {
			return err
		}
		if previous != int64(level) {
			return revokeGroupSessions(ctx, tx, group)
		}
		return nil
	})
}

func (p *postgresRepository) isPlatformAdmin(ctx context.Context, user int64) (bool, error) {
	var admin bool
	err := p.db.QueryRowContext(ctx, `SELECT is_platform_admin FROM users WHERE id = $1`, user).Scan(&admin)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return admin, err
}

func (p *postgresRepository) databaseAccess(ctx context.Context, user, database int64) ([]contracts.AccessLevel, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT a.level FROM group_memberships AS m
		JOIN group_database_access AS a ON a.group_id = m.group_id
		WHERE m.user_id = $1 AND a.database_id = $2`, user, database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []contracts.AccessLevel
	for rows.Next() {
		var level int64
		if err := rows.Scan(&level); err != nil {
			return nil, err
		}
		if level < 0 || level > int64(contracts.AccessViewEdit) {
			return []contracts.AccessLevel{255}, nil
		}
		grants = append(grants, contracts.AccessLevel(level))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return grants, nil
}
