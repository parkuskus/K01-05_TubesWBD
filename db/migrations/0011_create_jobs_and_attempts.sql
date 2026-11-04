-- Durable job queue and execution attempt audit trail for Milestone 2 (M2-K02).
CREATE TABLE IF NOT EXISTS jobs (
    id TEXT PRIMARY KEY CHECK (length(btrim(id)) > 0),
    request_id TEXT NOT NULL UNIQUE CHECK (length(btrim(request_id)) > 0),
    execution_id TEXT NOT NULL UNIQUE CHECK (length(btrim(execution_id)) > 0),
    database_id TEXT NOT NULL CHECK (length(btrim(database_id)) > 0),
    connection_revision BIGINT NOT NULL DEFAULT 1 CHECK (connection_revision > 0),
    direction TEXT NOT NULL CHECK (direction IN ('APPLY', 'ROLLBACK', 'FORWARD', 'RESUME', 'COMPENSATE')),
    mode TEXT NOT NULL CHECK (mode IN ('NORMAL', 'BATCH')),
    status TEXT NOT NULL CHECK (
        status IN (
            'PENDING',
            'RUNNING',
            'RETRY_WAIT',
            'BLOCKED',
            'PARTIAL',
            'PARTIALLY_REVERTED',
            'UNKNOWN',
            'SUCCESS',
            'FAILED'
        )
    ),
    actor_id TEXT NOT NULL CHECK (length(btrim(actor_id)) > 0),
    idempotency_key TEXT NOT NULL CHECK (length(btrim(idempotency_key)) > 0),
    idempotency_hash TEXT NOT NULL CHECK (length(btrim(idempotency_hash)) = 64),
    plan_payload JSONB NOT NULL,
    trace_carrier JSONB NOT NULL DEFAULT '{}'::jsonb,
    available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_owner TEXT NOT NULL DEFAULT '',
    lease_token TEXT NOT NULL DEFAULT '',
    lease_expires_at TIMESTAMPTZ,
    retry_count INT NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    max_retries INT NOT NULL DEFAULT 3 CHECK (max_retries >= 0),
    committed_batches INT NOT NULL DEFAULT 0 CHECK (committed_batches >= 0),
    committed_rows BIGINT NOT NULL DEFAULT 0 CHECK (committed_rows >= 0),
    total_scope_rows BIGINT NOT NULL DEFAULT 0 CHECK (total_scope_rows >= 0),
    last_committed_cursor TEXT NOT NULL DEFAULT '',
    last_error_code TEXT NOT NULL DEFAULT '',
    last_error_message TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    CONSTRAINT jobs_actor_database_idempotency_uq UNIQUE (actor_id, database_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS jobs_poll_due_idx
    ON jobs (status, available_at, database_id);

CREATE INDEX IF NOT EXISTS jobs_database_status_idx
    ON jobs (database_id, status, created_at DESC);

CREATE TABLE IF NOT EXISTS job_attempts (
    id TEXT PRIMARY KEY CHECK (length(btrim(id)) > 0),
    job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    execution_id TEXT NOT NULL CHECK (length(btrim(execution_id)) > 0),
    attempt_no INT NOT NULL CHECK (attempt_no > 0),
    worker_id TEXT NOT NULL CHECK (length(btrim(worker_id)) > 0),
    status TEXT NOT NULL CHECK (status IN ('STARTED', 'COMMITTED', 'ROLLED_BACK', 'FAILED', 'UNKNOWN')),
    executed_sql TEXT NOT NULL DEFAULT '',
    error_code TEXT,
    error_message TEXT,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ,
    CONSTRAINT job_attempts_job_attempt_no_uq UNIQUE (job_id, attempt_no)
);

CREATE INDEX IF NOT EXISTS job_attempts_job_id_idx
    ON job_attempts (job_id, attempt_no DESC);

CREATE INDEX IF NOT EXISTS job_attempts_execution_id_idx
    ON job_attempts (execution_id);

