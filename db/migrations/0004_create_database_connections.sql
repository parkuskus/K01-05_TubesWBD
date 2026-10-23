-- Slot 0003 is reserved for Aufar's groups/memberships migration.
-- Grants must reference database_connections(id) with ON DELETE CASCADE.
CREATE TABLE IF NOT EXISTS database_connections (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name TEXT NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
    host TEXT NOT NULL CHECK (length(host) BETWEEN 1 AND 253),
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    database_name TEXT NOT NULL CHECK (length(database_name) BETWEEN 1 AND 63),
    username TEXT NOT NULL CHECK (length(username) BETWEEN 1 AND 63),
    ssl_mode TEXT NOT NULL CHECK (ssl_mode IN ('disable', 'require', 'verify-full')),
    password_ciphertext BYTEA NOT NULL CHECK (octet_length(password_ciphertext) >= 29),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS database_connections_name_lower_idx
    ON database_connections (lower(name));
