import { afterEach, describe, expect, it, vi } from "vitest";
import { createApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

describe("API transport foundation", () => {
  it("blocks a mutation until CSRF bootstrap is ready, and after the token is cleared", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    const api = createApiClient();
    await expect(api.request("/example", { method: "POST" })).rejects.toMatchObject({
      code: "CSRF_NOT_READY",
    });
    expect(fetchMock).not.toHaveBeenCalled();

    api.setCsrfToken("example-csrf");
    await api.request("/example", { method: "POST", body: JSON.stringify({ name: "Demo" }) });
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/example");
    expect(init.credentials).toBe("same-origin");
    expect(init.redirect).toBe("error");
    expect(new Headers(init.headers).get("X-CSRF-Token")).toBe("example-csrf");

    api.setCsrfToken(null);
    await expect(api.request("/example", { method: "DELETE" })).rejects.toMatchObject({
      code: "CSRF_NOT_READY",
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("unwraps JSON data and preserves structured errors for feature consumers", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(Response.json({ data: { name: "Demo" } }))
      .mockResolvedValueOnce(
        Response.json(
          { error: { code: "FORBIDDEN", message: "Akses ditolak." }, request_id: "demo-request" },
          { status: 403 },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);
    const api = createApiClient();
    await expect(api.request("/example")).resolves.toEqual({ name: "Demo" });
    await expect(api.request("/example")).rejects.toMatchObject({
      status: 403,
      code: "FORBIDDEN",
      requestId: "demo-request",
      message: "Akses ditolak.",
    });
  });

  it("rejects an HTML login page rather than treating it as successful API data", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(
          new Response("<h1>Login</h1>", { headers: { "Content-Type": "text/html" } }),
        ),
    );
    await expect(createApiClient().request("/example")).rejects.toMatchObject({
      code: "INVALID_RESPONSE",
    });
  });

  it("preserves field validation errors alongside the API error metadata", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        Response.json(
          {
            error: {
              code: "VALIDATION_FAILED",
              message: "Periksa isian koneksi.",
              field_errors: { password: "Isi password PostgreSQL untuk koneksi baru." },
            },
            request_id: "validation-request",
          },
          { status: 422 },
        ),
      ),
    );
    const api = createApiClient();
    api.setCsrfToken("example-csrf");
    await expect(api.request("/connections", { method: "POST" })).rejects.toMatchObject({
      status: 422,
      code: "VALIDATION_FAILED",
      requestId: "validation-request",
      fieldErrors: { password: "Isi password PostgreSQL untuk koneksi baru." },
    });
  });

  it("handles a validation response with a null field-error map", async () => {
    const response = Response.json(
      {
        error: {
          code: "VALIDATION_FAILED",
          message: "Input tidak valid.",
          field_errors: null,
        },
      },
      { status: 422 },
    );
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response));
    await expect(createApiClient().request("/connections")).rejects.toMatchObject({
      status: 422,
      code: "VALIDATION_FAILED",
      fieldErrors: {},
    });
  });

  it("rejects absolute and protocol-relative paths before sending a request", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const api = createApiClient();
    await expect(api.request("https://example.com")).rejects.toThrow("API paths must be relative");
    await expect(api.request("//example.com")).rejects.toThrow("API paths must be relative");
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
