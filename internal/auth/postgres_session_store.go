package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

type postgresSessionStore struct {
	db *sql.DB
}

func (s *postgresSessionStore) Create(ctx context.Context, userID contracts.UserID, tokenHash, previousTokenHash []byte, now, expiresAt time.Time, idleTimeout time.Duration) (sessionRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sessionRecord{}, err
	}
	defer tx.Rollback()

	if len(previousTokenHash) != 0 {
		_, err = tx.ExecContext(ctx, `
			WITH revoked AS (
				UPDATE sessions
				SET revoked_at = $2
				WHERE id = (
					SELECT session_id FROM session_tokens
					WHERE token_hash = $1 AND (valid_until IS NULL OR valid_until > $2)
				)
				RETURNING id
			)
			DELETE FROM session_tokens
			USING revoked
			WHERE session_tokens.session_id = revoked.id`, previousTokenHash, now)
		if err != nil {
			return sessionRecord{}, err
		}
	}

	_, err = tx.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE revoked_at IS NOT NULL
		   OR absolute_expires_at <= $1
		   OR last_seen_at <= $2`, now, now.Add(-idleTimeout))
	if err != nil {
		return sessionRecord{}, err
	}

	var record sessionRecord
	record.UserID = userID
	record.CreatedAt = now
	record.LastSeenAt = now
	record.AbsoluteExpiresAt = expiresAt
	record.RotatedAt = now
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO sessions (user_id, created_at, last_seen_at, absolute_expires_at, rotated_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`, userID, now, now, expiresAt, now).Scan(&record.ID); err != nil {
		return sessionRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_tokens (token_hash, session_id)
		VALUES ($1, $2)`, tokenHash, record.ID); err != nil {
		return sessionRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return sessionRecord{}, err
	}
	return record, nil
}

func (s *postgresSessionStore) LoadAndTouch(ctx context.Context, tokenHash []byte, now time.Time, idleTimeout time.Duration) (sessionRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sessionRecord{}, err
	}
	defer tx.Rollback()

	var record sessionRecord
	err = tx.QueryRowContext(ctx, `
		SELECT s.id, s.user_id, s.created_at, s.last_seen_at, s.absolute_expires_at, s.rotated_at,
		       (t.valid_until IS NOT NULL)
		FROM session_tokens AS t
		JOIN sessions AS s ON s.id = t.session_id
		WHERE t.token_hash = $1
		  AND (t.valid_until IS NULL OR t.valid_until > $2)
		  AND s.revoked_at IS NULL
		  AND s.absolute_expires_at > $2
		  AND s.last_seen_at > $3
		FOR UPDATE OF s`, tokenHash, now, now.Add(-idleTimeout)).Scan(
		&record.ID, &record.UserID, &record.CreatedAt, &record.LastSeenAt,
		&record.AbsoluteExpiresAt, &record.RotatedAt, &record.TokenIsAlias)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionRecord{}, ErrUnauthenticated
	}
	if err != nil {
		return sessionRecord{}, err
	}
	if err := tx.QueryRowContext(ctx, `
		UPDATE sessions SET last_seen_at = GREATEST(last_seen_at, $2)
		WHERE id = $1
		RETURNING last_seen_at`, record.ID, now).Scan(&record.LastSeenAt); err != nil {
		return sessionRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return sessionRecord{}, err
	}
	return record, nil
}

func (s *postgresSessionStore) Rotate(ctx context.Context, sessionID int64, presentedHash, newHash []byte, now time.Time, idleTimeout, grace time.Duration) (sessionRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sessionRecord{}, err
	}
	defer tx.Rollback()

	var record sessionRecord
	err = tx.QueryRowContext(ctx, `
		SELECT s.id, s.user_id, s.created_at, s.last_seen_at, s.absolute_expires_at, s.rotated_at
		FROM session_tokens AS t
		JOIN sessions AS s ON s.id = t.session_id
		WHERE t.token_hash = $1
		  AND s.id = $2
		  AND (t.valid_until IS NULL OR t.valid_until > $3)
		  AND s.revoked_at IS NULL
		  AND s.absolute_expires_at > $3
		  AND s.last_seen_at > $4
		FOR UPDATE OF s`, presentedHash, sessionID, now, now.Add(-idleTimeout)).Scan(
		&record.ID, &record.UserID, &record.CreatedAt, &record.LastSeenAt,
		&record.AbsoluteExpiresAt, &record.RotatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionRecord{}, ErrUnauthenticated
	}
	if err != nil {
		return sessionRecord{}, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE session_tokens SET valid_until = $2
		WHERE session_id = $1 AND valid_until IS NULL`, sessionID, now.Add(grace))
	if err != nil {
		return sessionRecord{}, err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return sessionRecord{}, err
		}
		return sessionRecord{}, errors.New("session has no current token")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_tokens (token_hash, session_id) VALUES ($1, $2)`, newHash, sessionID); err != nil {
		return sessionRecord{}, err
	}
	if err := tx.QueryRowContext(ctx, `
		UPDATE sessions
		SET rotated_at = GREATEST(rotated_at, $2), last_seen_at = GREATEST(last_seen_at, $2)
		WHERE id = $1
		RETURNING last_seen_at, rotated_at`, sessionID, now).Scan(&record.LastSeenAt, &record.RotatedAt); err != nil {
		return sessionRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return sessionRecord{}, err
	}
	return record, nil
}

func (s *postgresSessionStore) RevokeToken(ctx context.Context, tokenHash []byte, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		WITH revoked AS (
			UPDATE sessions
			SET revoked_at = $2
			WHERE id = (
				SELECT session_id FROM session_tokens
				WHERE token_hash = $1
			)
			RETURNING id
		)
		DELETE FROM session_tokens
		USING revoked
		WHERE session_tokens.session_id = revoked.id`, tokenHash, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresSessionStore) RevokeUser(ctx context.Context, userID contracts.UserID, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		WITH revoked AS (
			UPDATE sessions SET revoked_at = $2
			WHERE user_id = $1 AND revoked_at IS NULL
			RETURNING id
		)
		DELETE FROM session_tokens
		USING revoked
		WHERE session_tokens.session_id = revoked.id`, userID, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}
