import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createApiClient } from "@/lib/api/client";
import { DatabaseWorkspace } from "./database-workspace";

const api = createApiClient();
const identity = { id: "1", username: "admin", is_platform_admin: true };
vi.mock("@/components/auth-provider", () => ({ useAuth: () => ({ api, user: identity }) }));

function mount() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <DatabaseWorkspace />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it("loads authorized databases and combines name and access filters", async () => {
  const records = [
    { id: "1", name: "primary", level: 2 },
    { id: "2", name: "reporting", level: 1 },
  ];
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(Response.json({ data: { databases: records } })),
  );
  const user = userEvent.setup();
  mount();
  expect((await screen.findByRole("link", { name: "primary" })).getAttribute("href")).toBe(
    "/databases/1",
  );
  await user.selectOptions(screen.getByRole("combobox", { name: "Hak akses database" }), "1");
  expect(screen.queryByRole("link", { name: "primary" })).toBeNull();
  await user.type(screen.getByRole("searchbox", { name: "Cari database" }), "primary");
  expect(screen.getByText("Database tidak ditemukan")).toBeTruthy();
  expect(records).toHaveLength(2);
});

it("keeps connection administration separate from target access for an admin without grants", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(Response.json({ data: { databases: [] } })));
  mount();
  expect(await screen.findByText("Belum ada database yang dapat diakses")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Kelola koneksi" }).getAttribute("href")).toBe(
    "/connections",
  );
  expect(screen.queryByRole("button", { name: "Tambah database" })).toBeNull();
});

it("reports a missing API and allows a real retry without manufacturing database records", async () => {
  const fetcher = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ error: { code: "NOT_FOUND" } }, { status: 404 }))
    .mockResolvedValueOnce(Response.json({ data: { databases: [] } }));
  vi.stubGlobal("fetch", fetcher);
  mount();
  expect(await screen.findByText("Data atau layanan belum tersedia")).toBeTruthy();
  await userEvent.click(screen.getByRole("button", { name: "Coba lagi" }));
  expect(await screen.findByText("Belum ada database yang dapat diakses")).toBeTruthy();
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
});
