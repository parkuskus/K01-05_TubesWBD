CREATE TABLE migration_definitions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id BIGINT NOT NULL REFERENCES database_connections(id) ON DELETE RESTRICT,
    connection_revision BIGINT NOT NULL CHECK (connection_revision > 0),
    version BIGINT NOT NULL CHECK (version > 0),
    title TEXT NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 200),
    state TEXT NOT NULL DEFAULT 'DRAFT' CHECK (state IN ('DRAFT', 'IN_REVIEW', 'LOCKED')),
    current_revision INT NOT NULL DEFAULT 1 CHECK (current_revision > 0),
    created_by BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    attempted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (database_id, version)
);

CREATE TABLE migration_revisions (
    migration_id BIGINT NOT NULL REFERENCES migration_definitions(id) ON DELETE RESTRICT,
    revision INT NOT NULL CHECK (revision > 0),
    up_sql TEXT NOT NULL CHECK (length(btrim(up_sql)) BETWEEN 1 AND 1048576),
    down_sql TEXT NOT NULL DEFAULT '' CHECK (length(down_sql) <= 1048576),
    irreversible BOOLEAN NOT NULL,
    reason TEXT NOT NULL DEFAULT '' CHECK (length(reason) <= 2000),
    mode TEXT NOT NULL CHECK (mode IN ('NORMAL', 'BATCH')),
    batch_plan JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(batch_plan) = 'object'),
    notes TEXT NOT NULL DEFAULT '' CHECK (length(notes) <= 8192),
    checksum TEXT NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (migration_id, revision),
    CHECK ((NOT irreversible AND length(btrim(down_sql)) > 0 AND reason = '')
        OR (irreversible AND down_sql = '' AND length(btrim(reason)) > 0)),
    CHECK ((mode = 'NORMAL' AND batch_plan = '{}'::jsonb)
        OR (mode = 'BATCH' AND batch_plan <> '{}'::jsonb))
);

ALTER TABLE migration_definitions ADD CONSTRAINT migration_definitions_current_revision_fk
    FOREIGN KEY (id, current_revision) REFERENCES migration_revisions(migration_id, revision)
    DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION protect_migration_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'migration revisions are append-only' USING ERRCODE = '23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM migration_definitions WHERE id = NEW.migration_id
            AND state = 'DRAFT' AND attempted_at IS NULL FOR UPDATE
    ) THEN
        RAISE EXCEPTION 'migration definition cannot be revised' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_revision_guard BEFORE INSERT OR UPDATE OR DELETE ON migration_revisions
    FOR EACH ROW EXECUTE FUNCTION protect_migration_revision();

CREATE FUNCTION protect_attempted_definition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.attempted_at IS NOT NULL AND (NEW.title, NEW.state, NEW.current_revision, NEW.attempted_at,
        NEW.database_id, NEW.connection_revision, NEW.version, NEW.created_by)
        IS DISTINCT FROM (OLD.title, OLD.state, OLD.current_revision, OLD.attempted_at,
        OLD.database_id, OLD.connection_revision, OLD.version, OLD.created_by) THEN
        RAISE EXCEPTION 'attempted migration definition is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER attempted_definition_guard BEFORE UPDATE ON migration_definitions
    FOR EACH ROW EXECUTE FUNCTION protect_attempted_definition();
