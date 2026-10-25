DROP INDEX IF EXISTS users_username_lower_idx;
CREATE UNIQUE INDEX IF NOT EXISTS users_username_idx ON users (username);
