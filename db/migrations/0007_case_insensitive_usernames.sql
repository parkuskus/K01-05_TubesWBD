DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM users
        GROUP BY lower(username)
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'case-insensitive username migration blocked: resolve usernames that differ only by case';
    END IF;
END
$$;

DROP INDEX IF EXISTS users_username_idx;
CREATE UNIQUE INDEX IF NOT EXISTS users_username_lower_idx ON users (lower(username));
