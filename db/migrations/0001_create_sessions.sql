CREATE TABLE IF NOT EXISTS sessions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    absolute_expires_at TIMESTAMPTZ NOT NULL,
    rotated_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS session_tokens (
    token_hash BYTEA PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    session_id BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    valid_until TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS session_tokens_one_current_per_session
    ON session_tokens (session_id)
    WHERE valid_until IS NULL;

CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions (user_id);
