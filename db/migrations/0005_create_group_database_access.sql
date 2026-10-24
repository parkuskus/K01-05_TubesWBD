CREATE TABLE IF NOT EXISTS group_database_access (
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    database_id BIGINT NOT NULL REFERENCES database_connections(id) ON DELETE CASCADE,
    level SMALLINT NOT NULL CHECK (level IN (1, 2)),
    PRIMARY KEY (group_id, database_id)
);

CREATE INDEX IF NOT EXISTS group_database_access_database_id_idx
    ON group_database_access (database_id);

