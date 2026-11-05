import type { ApiEnvelope, ApiErrorBody } from "./types";

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
    readonly requestId?: string,
    readonly fieldErrors: Readonly<Record<string, string>> = {},
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export function createApiClient(baseUrl = "/api/v1") {
  let csrfToken: string | null = null;
  let onUnauthorized: ((path: string) => void) | null = null;

  return {
    setCsrfToken(token: string | null) {
      csrfToken = token;
    },
    setUnauthorizedHandler(handler: ((path: string) => void) | null) {
      onUnauthorized = handler;
    },
    async request<T>(path: string, init: RequestInit = {}): Promise<T | undefined> {
      if (!path.startsWith("/") || path.startsWith("//")) {
        throw new Error("API paths must be relative to the configured API base.");
      }
      const headers = new Headers(init.headers);
      headers.set("Accept", "application/json");
      const method = (init.method ?? "GET").toUpperCase();
      if (!["GET", "HEAD", "OPTIONS"].includes(method)) {
        if (!csrfToken) throw new ApiError("CSRF token is required.", 0, "CSRF_NOT_READY");
        headers.set("X-CSRF-Token", csrfToken);
      }
      if (init.body && typeof init.body === "string")
        headers.set("Content-Type", "application/json");
      const response = await fetch(`${baseUrl}${path}`, {
        ...init,
        headers,
        credentials: "same-origin",
        cache: "no-store",
        redirect: "error",
      });
      if (response.status === 401) onUnauthorized?.(path);
      if (response.status === 204) return undefined;
      if (!response.headers.get("Content-Type")?.includes("application/json")) {
        throw new ApiError(
          "API returned an unexpected response.",
          response.status,
          "INVALID_RESPONSE",
        );
      }
      const body: ApiEnvelope<T> & ApiErrorBody = await response.json();
      if (!response.ok) {
        throw new ApiError(
          body.error?.message ?? "Permintaan belum dapat diproses.",
          response.status,
          body.error?.code ?? "REQUEST_FAILED",
          body.request_id,
          body.error?.field_errors ?? {},
        );
      }
      return body.data;
    },
  };
}
