import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createApiClient } from "@/lib/api/client";
import { ConnectionsWorkspace } from "./connections-workspace";

const api = createApiClient();
api.setCsrfToken("test-csrf");
vi.mock("@/components/auth-provider", () => ({
  useAuth: () => ({ api, user: { id: "1", is_platform_admin: true } }),
}));

const connection = {
  id: "1",
  name: "Inventory",
  host: "localhost",
  port: 5432,
  database: "inventory",
  username: "developer",
  ssl_mode: "require",
};
function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ConnectionsWorkspace />
    </QueryClientProvider>,
  );
}
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it("edits a connection without including an empty password and refreshes the list", async () => {
  const fetcher = vi
    .fn()
    .mockImplementation((_path: string, init: RequestInit) =>
      Promise.resolve(
        init.method === "PATCH"
          ? new Response(null, { status: 204 })
          : Response.json({ data: { connections: [connection] } }),
      ),
    );
  vi.stubGlobal("fetch", fetcher);
  const user = userEvent.setup();
  mount();
  await user.click(await screen.findByRole("button", { name: "Edit" }));
  await user.clear(screen.getByLabelText("Nama koneksi"));
  await user.type(screen.getByLabelText("Nama koneksi"), "Inventory new");
  await user.click(screen.getByRole("button", { name: "Simpan koneksi" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  const [, init] = fetcher.mock.calls.find(([, init]) => init.method === "PATCH")!;
  expect(JSON.parse(init.body)).toEqual({
    name: "Inventory new",
    host: "localhost",
    port: 5432,
    database: "inventory",
    username: "developer",
    ssl_mode: "require",
  });
  expect(fetcher.mock.calls.filter(([, init]) => !init.method)).toHaveLength(2);
});

it("confirms deletion and leaves an in-use connection visible when the server rejects it", async () => {
  const fetcher = vi
    .fn()
    .mockImplementation((_path: string, init: RequestInit) =>
      Promise.resolve(
        init.method === "DELETE"
          ? Response.json({ error: { code: "IN_USE" } }, { status: 409 })
          : Response.json({ data: { connections: [connection] } }),
      ),
    );
  vi.stubGlobal("fetch", fetcher);
  const user = userEvent.setup();
  mount();
  await user.click(await screen.findByRole("button", { name: "Hapus" }));
  expect(fetcher.mock.calls.some(([, init]) => init.method === "DELETE")).toBe(false);
  await user.click(screen.getByRole("button", { name: "Hapus koneksi" }));
  expect(await screen.findByRole("alert")).toBeTruthy();
  expect(screen.getByRole("dialog")).toBeTruthy();
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Batal" }));
  expect(screen.getByText("Inventory")).toBeTruthy();
});

it("tests the saved connection without rendering a raw credential-bearing failure", async () => {
  const fetcher = vi.fn().mockImplementation((_path: string, init: RequestInit) =>
    Promise.resolve(
      init.method === "POST"
        ? Response.json(
            {
              error: {
                code: "CONNECTION_FAILED",
                message: "postgres://user:private-password@host/db",
              },
            },
            { status: 503 },
          )
        : Response.json({ data: { connections: [connection] } }),
    ),
  );
  vi.stubGlobal("fetch", fetcher);
  mount();
  await userEvent.click(await screen.findByRole("button", { name: "Uji koneksi" }));
  expect(
    await screen.findByText("Koneksi belum dapat diuji. Periksa konfigurasi dan layanan."),
  ).toBeTruthy();
  expect(screen.queryByText(/private-password/)).toBeNull();
  expect(fetcher).toHaveBeenCalledWith(
    "/api/v1/connections/1/test",
    expect.objectContaining({ method: "POST" }),
  );
});
