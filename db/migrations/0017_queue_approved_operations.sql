ALTER TABLE operation_requests
    DROP CONSTRAINT operation_requests_status_check,
    ADD CONSTRAINT operation_requests_status_check
        CHECK (status IN ('PENDING_REVIEW', 'APPROVED', 'QUEUED', 'STALE'));

ALTER TABLE operation_requests ADD COLUMN origin_execution_id TEXT NOT NULL DEFAULT '';

CREATE OR REPLACE FUNCTION protect_operation_request_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (OLD.id, OLD.database_id, OLD.migration_id, OLD.revision, OLD.direction, OLD.expected_head, OLD.origin_execution_id,
        OLD.connection_revision, OLD.policy_version, OLD.revision_checksum, OLD.review_round,
        OLD.created_by, OLD.created_at)
        IS DISTINCT FROM
       (NEW.id, NEW.database_id, NEW.migration_id, NEW.revision, NEW.direction, NEW.expected_head, NEW.origin_execution_id,
        NEW.connection_revision, NEW.policy_version, NEW.revision_checksum, NEW.review_round,
        NEW.created_by, NEW.created_at) THEN
        RAISE EXCEPTION 'operation request snapshot is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.checksum IS DISTINCT FROM NEW.checksum AND (OLD.checksum <> repeat('0', 64)
        OR EXISTS (SELECT 1 FROM approval_requirements WHERE request_id=OLD.id)) THEN
        RAISE EXCEPTION 'operation request checksum is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.status <> NEW.status AND NOT (
        OLD.status IN ('PENDING_REVIEW', 'APPROVED') AND NEW.status IN ('APPROVED', 'STALE')
        OR OLD.status = 'APPROVED' AND NEW.status = 'QUEUED'
    ) THEN
        RAISE EXCEPTION 'invalid operation request status transition' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
