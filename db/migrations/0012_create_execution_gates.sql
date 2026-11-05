-- Per-database execution gate table in platform DB for Milestone 2 (M2-K03).
-- Blocks subsequent migrations on a target database when an earlier execution
-- halted in PARTIAL, PARTIALLY_REVERTED, or UNKNOWN state.
CREATE TABLE IF NOT EXISTS database_execution_gates (
    database_id TEXT PRIMARY KEY CHECK (length(btrim(database_id)) > 0),
    execution_id TEXT NOT NULL CHECK (length(btrim(execution_id)) > 0),
    job_id TEXT NOT NULL CHECK (length(btrim(job_id)) > 0),
    reason TEXT NOT NULL CHECK (reason IN ('PARTIAL', 'PARTIALLY_REVERTED', 'UNKNOWN')),
    details TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

