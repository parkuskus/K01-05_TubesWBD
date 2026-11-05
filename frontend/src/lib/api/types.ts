// Mirrors internal/contracts/jobs.go. Public API DTOs still need backend agreement.
export type JobStatus =
  | "PENDING"
  | "RUNNING"
  | "RETRY_WAIT"
  | "BLOCKED"
  | "PARTIAL"
  | "PARTIALLY_REVERTED"
  | "UNKNOWN"
  | "SUCCESS"
  | "FAILED";

export type ApiEnvelope<T> = { data: T; request_id?: string };
export type ApiErrorBody = {
  error?: { code?: string; message?: string; field_errors?: Record<string, string> };
  request_id?: string;
};
