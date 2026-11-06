import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createApiClient } from "@/lib/api/client";
import { DatabaseViewer } from "./database-viewer";

const api = createApiClient();
let params = new URLSearchParams();
const push = vi.fn((url: string) => {
  params = new URLSearchParams(url.split("?")[1]);
});
vi.mock("@/components/auth-provider", () => ({ useAuth: () => ({ api }) }));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
  usePathname: () => "/databases/1",
  useSearchParams: () => params,
}));

const choice = {
  id: "1",
  name: "Inventory",
  access_level: 1,
  capabilities: { create_migration: false },
};
function transport() {
  return vi.fn().mockImplementation((path: string) => {
    const url = new URL(path, "http://localhost");
    if (url.pathname.endsWith("/schemas"))
      return Promise.resolve(Response.json({ data: { schemas: ["public", "odd schema"] } }));
    if (url.pathname.endsWith("/tables"))
      return Promise.resolve(
        Response.json({
          data: {
            tables: [
              { name: "odd/table?", kind: "Table" },
              { name: "another", kind: "View" },
            ],
          },
        }),
      );
    if (url.pathname.endsWith("/table"))
      return Promise.resolve(
        Response.json({
          data: {
            schema: "public",
            name: "odd/table?",
            columns: [{ name: "id", type: "integer", nullable: false, default: "" }],
            constraints: [{ name: "pk", sql: "PRIMARY KEY (id)" }],
            indexes: [{ name: "idx", sql: "CREATE INDEX idx ON items (id)" }],
          },
        }),
      );
    if (url.pathname.endsWith("/data"))
      return Promise.resolve(
        Response.json({
          data: {
            columns: ["id", "value", "note"],
            rows:
              url.searchParams.get("page") === "2"
                ? [[{ text: "second-page", null: false, truncated: false }]]
                : [
                    [
                      { text: "", null: true, truncated: false },
                      { text: "", null: false, truncated: false },
                      { text: "<script>unsafe()</script>", null: false, truncated: true },
                    ],
                  ],
            page: Number(url.searchParams.get("page")),
            has_next: url.searchParams.get("page") !== "2",
          },
        }),
      );
    return Promise.resolve(Response.json({ data: choice }));
  });
}
function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const content = (
    <QueryClientProvider client={client}>
      <DatabaseViewer id="1" />
    </QueryClientProvider>
  );
  const view = render(content);
  return () =>
    view.rerender(
      <QueryClientProvider client={client}>
        <DatabaseViewer id="1" />
      </QueryClientProvider>,
    );
}
beforeEach(() => {
  params = new URLSearchParams();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it("opens a deep link with safely encoded identifiers and shows columns, constraints, and indexes", async () => {
  params = new URLSearchParams({ schema: "odd schema", table: "odd/table?" });
  const fetcher = transport();
  vi.stubGlobal("fetch", fetcher);
  mount();
  expect(await screen.findByText("PRIMARY KEY (id)")).toBeTruthy();
  expect(screen.getByText("CREATE INDEX idx ON items (id)")).toBeTruthy();
  expect(screen.getByRole("region", { name: "Kolom tabel" })).toBeTruthy();
  expect(
    fetcher.mock.calls.some(([path]) =>
      String(path).includes("/table?schema=odd+schema&table=odd%2Ftable%3F"),
    ),
  ).toBe(true);
});

it("distinguishes NULL, empty and truncated cells, paginates, and resets the page on schema change", async () => {
  params = new URLSearchParams({ schema: "public", table: "odd/table?", view: "data" });
  const fetcher = transport();
  vi.stubGlobal("fetch", fetcher);
  const user = userEvent.setup();
  const rerender = mount();
  expect(await screen.findByText("NULL")).toBeTruthy();
  expect(screen.getByText("(kosong)")).toBeTruthy();
  expect(screen.getByText("Terpotong")).toBeTruthy();
  expect(document.querySelector("script")).toBeNull();
  await user.click(screen.getByRole("button", { name: "Berikutnya" }));
  expect(params.get("page")).toBe("2");
  rerender();
  expect(await screen.findByText("second-page")).toBeTruthy();
  expect(screen.queryByText("NULL")).toBeNull();
  expect((screen.getByRole("button", { name: "Berikutnya" }) as HTMLButtonElement).disabled).toBe(
    true,
  );
  await user.selectOptions(screen.getByRole("combobox", { name: "Schema" }), "odd schema");
  expect(params.get("page")).toBe("1");
  expect(params.has("table")).toBe(false);
  rerender();
  expect(await screen.findByText("Pilih tabel")).toBeTruthy();
});

it("does not load schemas or target data after a forbidden database response", async () => {
  const fetcher = vi
    .fn()
    .mockResolvedValue(Response.json({ error: { code: "FORBIDDEN" } }, { status: 403 }));
  vi.stubGlobal("fetch", fetcher);
  mount();
  expect(await screen.findByText("Akses ditolak")).toBeTruthy();
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
});

it("recovers from an obsolete schema deep link without requesting its tables", async () => {
  params = new URLSearchParams({ schema: "deleted", table: "old" });
  const fetcher = transport();
  vi.stubGlobal("fetch", fetcher);
  const rerender = mount();
  expect(await screen.findByText("Schema tidak ditemukan")).toBeTruthy();
  expect(fetcher.mock.calls.some(([path]) => String(path).includes("/tables?"))).toBe(false);
  await userEvent.selectOptions(screen.getByRole("combobox", { name: "Schema" }), "public");
  rerender();
  expect(await screen.findByRole("link", { name: /another/ })).toBeTruthy();
  expect(screen.getByText("Pilih tabel")).toBeTruthy();
});
