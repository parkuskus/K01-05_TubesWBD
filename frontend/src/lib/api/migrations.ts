import { json, mutationResource, resource, resourceID, type ApiClient } from "./resources";
import type { JobStatus } from "./types";

export type Direction = "APPLY" | "ROLLBACK" | "FORWARD" | "RESUME" | "COMPENSATE";
export type ExecutionMode = "NORMAL" | "BATCH";
export type BatchConfig = {
  table_schema: string;
  table_name: string;
  primary_key_column: string;
  scope_where: string;
  batch_size: number;
};
export type DraftInput = {
  title: string;
  up_sql: string;
  down_sql: string;
  is_irreversible: boolean;
  irreversible_reason: string;
  mode: ExecutionMode;
  batch: BatchConfig | null;
  compatibility_notes: string;
};
export type MigrationDefinition = DraftInput & {
  id: string;
  database_id: string;
  version: string;
  revision_id: string;
  ever_executed: boolean;
  expected_active_head: string;
  active_execution_id: string;
  capabilities: {
    edit: boolean;
    request_apply: boolean;
    request_rollback: boolean;
    request_forward: boolean;
  };
};
export type ValidationReport = {
  revision_id: string;
  valid: boolean;
  checks: { code: string; valid: boolean; message: string }[];
  warnings: string[];
};
export type OperationStatus =
  "PENDING_REVIEW" | "APPROVED" | "QUEUED" | "REJECTED" | "STALE" | "CANCELLED";
export type OperationSummary = {
  id: string;
  database_id: string;
  migration_id: string;
  title: string;
  direction: Direction;
  status: OperationStatus;
  created_at: string;
  job_id: string | null;
};
export type OperationRequest = OperationSummary & {
  round: number;
  plan_checksum: string;
  compatibility_notes: string;
  plan: DraftInput & {
    revision_id: string;
    version: string;
    expected_active_head: string;
    origin_execution_id: string;
  };
  requirements: {
    group_id: string;
    group_name: string;
    access_level: 1 | 2;
    leader_id: string | null;
    leader_name: string | null;
    status: "PENDING" | "APPROVED" | "REJECTED" | "STALE";
    can_approve: boolean;
  }[];
  capabilities: { enqueue: boolean };
};
export type OperationInput =
  | {
      migration_id: string;
      revision_id: string;
      direction: "APPLY" | "ROLLBACK" | "FORWARD";
      expected_active_head: string;
      origin_execution_id?: string;
    }
  | { direction: "RESUME" | "COMPENSATE"; origin_execution_id: string };
export type Job = {
  job_id: string;
  request_id: string;
  execution_id: string;
  database_id: string;
  direction: Direction;
  mode: ExecutionMode;
  status: JobStatus;
  actor_id: string;
  retry_count: number;
  max_retries: number;
  committed_batches: number;
  committed_rows: number;
  total_scope_rows: number;
  last_committed_cursor: string;
  error_code: string;
  error_message: string;
  capabilities: { request_resume: boolean; request_compensation: boolean };
  created_at: string;
  updated_at: string;
  completed_at: string | null;
};
export type JobAttempt = {
  attempt_id: string;
  job_id: string;
  execution_id: string;
  attempt_no: number;
  status: "STARTED" | "COMMITTED" | "ROLLED_BACK" | "FAILED" | "UNKNOWN";
  executed_sql: string;
  error_code: string;
  error_message: string;
  started_at: string;
  finished_at: string | null;
};
export type HistoryEvent = {
  execution_id: string;
  job_id: string;
  migration_id: string;
  revision_id: string;
  version: string;
  direction: Direction;
  mode: ExecutionMode;
  executed_sql: string;
  actor_id: string;
  reverts_execution_id: string;
  committed_at: string;
};

export function migrationApi(api: ApiClient) {
  const database = (id: string) => `/databases/${resourceID(id)}`;
  const definition = (id: string, migration: string) =>
    `${database(id)}/migrations/${resourceID(migration)}`;
  const operation = (id: string) => `/operation-requests/${resourceID(id)}`;
  return {
    list: (id: string, signal?: AbortSignal) =>
      resource<{ migrations: MigrationDefinition[] }>(api, `${database(id)}/migrations`, signal),
    get: (id: string, migration: string, signal?: AbortSignal) =>
      resource<MigrationDefinition>(api, definition(id, migration), signal),
    create: (id: string, input: DraftInput) =>
      mutationResource<MigrationDefinition>(api, `${database(id)}/migrations`, json("POST", input)),
    update: (id: string, migration: string, expected_revision: string, input: DraftInput) =>
      mutationResource<MigrationDefinition>(
        api,
        definition(id, migration),
        json("PATCH", { ...input, expected_revision }),
      ),
    validate: (id: string, migration: string, expected_revision: string) =>
      mutationResource<ValidationReport>(
        api,
        `${definition(id, migration)}/validate`,
        json("POST", { expected_revision }),
      ),
    operations: (id: string, signal?: AbortSignal) =>
      resource<{ requests: OperationSummary[] }>(api, `${database(id)}/operation-requests`, signal),
    operation: (id: string, signal?: AbortSignal) =>
      resource<OperationRequest>(api, operation(id), signal),
    requestOperation: (id: string, input: OperationInput) =>
      mutationResource<OperationRequest>(
        api,
        `${database(id)}/operation-requests`,
        json("POST", input),
      ),
    approve: (request: OperationRequest, group_id: string) =>
      api.request(
        `${operation(request.id)}/approvals`,
        json("POST", {
          group_id,
          round: request.round,
          plan_checksum: request.plan_checksum,
          decision: "APPROVE",
        }),
      ),
    enqueue: (request: OperationRequest) =>
      mutationResource<{ job_id: string; execution_id: string; status: JobStatus }>(
        api,
        `${operation(request.id)}/jobs`,
        {
          method: "POST",
          headers: {
            "Idempotency-Key": `operation-${request.id}-${request.round}-${request.plan_checksum}`,
          },
        },
      ),
    job: (id: string, signal?: AbortSignal) =>
      resource<Job>(api, `/jobs/${resourceID(id)}`, signal),
    attempts: (id: string, signal?: AbortSignal) =>
      resource<{ attempts: JobAttempt[] }>(api, `/jobs/${resourceID(id)}/attempts`, signal),
    history: (id: string, hide: boolean, cursor: string, signal?: AbortSignal) =>
      resource<{ events: HistoryEvent[]; next_cursor: string | null }>(
        api,
        `${database(id)}/history?${new URLSearchParams({ hide_rollback: String(hide), ...(cursor ? { cursor } : {}) })}`,
        signal,
      ),
  };
}
