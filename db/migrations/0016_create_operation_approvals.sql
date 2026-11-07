CREATE TABLE operation_requests (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id BIGINT NOT NULL REFERENCES database_connections(id) ON DELETE RESTRICT,
    migration_id BIGINT NOT NULL,
    revision INT NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('APPLY', 'ROLLBACK', 'FORWARD', 'RESUME', 'COMPENSATE')),
    expected_head TEXT NOT NULL DEFAULT '',
    connection_revision BIGINT NOT NULL CHECK (connection_revision > 0),
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    revision_checksum TEXT NOT NULL CHECK (revision_checksum ~ '^[0-9a-f]{64}$'),
    checksum TEXT NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
    status TEXT NOT NULL CHECK (status IN ('PENDING_REVIEW', 'APPROVED', 'STALE')),
    review_round INT NOT NULL CHECK (review_round > 0),
    created_by BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (migration_id, revision) REFERENCES migration_revisions(migration_id, revision) ON DELETE RESTRICT,
    UNIQUE (migration_id, review_round)
);

CREATE INDEX operation_requests_database_created_idx ON operation_requests(database_id, created_at DESC, id DESC);

CREATE TABLE approval_requirements (
    request_id BIGINT NOT NULL REFERENCES operation_requests(id) ON DELETE RESTRICT,
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE RESTRICT,
    leader_user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    grant_level SMALLINT NOT NULL CHECK (grant_level IN (1, 2)),
    PRIMARY KEY (request_id, group_id)
);

CREATE TABLE approval_votes (
    request_id BIGINT NOT NULL,
    review_round INT NOT NULL,
    group_id BIGINT NOT NULL,
    migration_revision INT NOT NULL CHECK (migration_revision > 0),
    checksum TEXT NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
    actor_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    decision TEXT NOT NULL CHECK (decision = 'APPROVE'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (request_id, review_round, group_id),
    FOREIGN KEY (request_id, group_id) REFERENCES approval_requirements(request_id, group_id) ON DELETE RESTRICT
);

CREATE FUNCTION protect_approval_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'approval evidence is append-only' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER approval_requirement_guard BEFORE UPDATE OR DELETE ON approval_requirements
    FOR EACH ROW EXECUTE FUNCTION protect_approval_evidence();
CREATE TRIGGER approval_vote_guard BEFORE UPDATE OR DELETE ON approval_votes
    FOR EACH ROW EXECUTE FUNCTION protect_approval_evidence();

CREATE FUNCTION protect_operation_request_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (OLD.id, OLD.database_id, OLD.migration_id, OLD.revision, OLD.direction, OLD.expected_head,
        OLD.connection_revision, OLD.policy_version, OLD.revision_checksum, OLD.review_round,
        OLD.created_by, OLD.created_at)
        IS DISTINCT FROM
       (NEW.id, NEW.database_id, NEW.migration_id, NEW.revision, NEW.direction, NEW.expected_head,
        NEW.connection_revision, NEW.policy_version, NEW.revision_checksum, NEW.review_round,
        NEW.created_by, NEW.created_at) THEN
        RAISE EXCEPTION 'operation request snapshot is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.checksum IS DISTINCT FROM NEW.checksum AND (OLD.checksum <> repeat('0', 64)
        OR EXISTS (SELECT 1 FROM approval_requirements WHERE request_id=OLD.id)) THEN
        RAISE EXCEPTION 'operation request checksum is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.status <> NEW.status AND NOT (OLD.status = 'PENDING_REVIEW' AND NEW.status IN ('APPROVED', 'STALE')) THEN
        RAISE EXCEPTION 'invalid operation request status transition' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER operation_request_snapshot_guard BEFORE UPDATE ON operation_requests
    FOR EACH ROW EXECUTE FUNCTION protect_operation_request_snapshot();
