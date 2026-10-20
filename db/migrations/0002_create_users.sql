CREATE TABLE IF NOT EXISTS users (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    is_platform_admin BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT users_username_not_blank CHECK (length(btrim(username)) > 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS users_username_lower_idx ON users (lower(username));
