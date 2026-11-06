import { ApiError, type createApiClient } from "./client";

export type ApiClient = ReturnType<typeof createApiClient>;

export async function resource<T>(api: ApiClient, path: string, signal?: AbortSignal) {
  const data = await api.request<T>(path, { signal });
  if (data === undefined || data === null) {
    throw new ApiError("Respons data tidak valid.", 200, "INVALID_RESPONSE");
  }
  return data;
}

export function json(method: string, body: unknown): RequestInit {
  return { method, body: JSON.stringify(body) };
}

export const resourceID = encodeURIComponent;
