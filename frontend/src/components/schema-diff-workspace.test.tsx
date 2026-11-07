import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createApiClient } from "@/lib/api/client";
import { SchemaDiffWorkspace } from "./schema-diff-workspace";

const api = createApiClient();
api.setCsrfToken("test-csrf");
let params = new URLSearchParams();
let canDraft = false;
const push = vi.fn((url: string) => {
  params = new URLSearchParams(url.split("?")[1]);
});
vi.mock("@/components/auth-provider", () => ({
  useAuth: () => ({ api, user: { is_platform_admin: true } }),
}));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
  usePathname: () => "/databases/1/schema-diff",
  useSearchParams: () => params,
}));

const difference = {
  reference_schema: "reference",
  target_schema: "public",
  changes: [
    {
      kind: "table",
      action: "drop",
      table: "",
      name: "obsolete",
      before: "Table",
      after: "",
      risk: "destructive",
      sql: "DROP TABLE public.obsolete;",
    },
  ],
  unsupported: [{ schema: "reference", table: "", name: "custom_function", kind: "function" }],
  sql: "DROP TABLE public.obsolete;",
};

function transport(rejectDraft = false) {
  return vi.fn().mockImplementation((path: string, init: RequestInit) => {
    if (init.method === "POST")
      return Promise.resolve(
        rejectDraft
          ? Response.json({ error: { code: "FORBIDDEN" } }, { status: 403 })
          : Response.json({ data: { migration_id: "77" } }, { status: 201 }),
      );
    if (path.endsWith("/schemas"))
      return Promise.resolve(Response.json({ data: { schemas: ["reference", "public"] } }));
    if (path.includes("/schema-diff?"))
      return Promise.resolve(Response.json({ data: { diff: difference } }));
    return Promise.resolve(
      Response.json({
        data: {
          database: { id: "1", name: "Inventory", level: canDraft ? 2 : 1 },
        },
      }),
    );
  });
}
function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <SchemaDiffWorkspace id="1" />
    </QueryClientProvider>,
  );
  return () =>
    view.rerender(
      <QueryClientProvider client={client}>
        <SchemaDiffWorkspace id="1" />
      </QueryClientProvider>,
    );
}
beforeEach(() => {
  params = new URLSearchParams();
  canDraft = false;
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it("compares the selected schemas, explains destructive and unsupported changes, and does not infer edit access from admin", async () => {
  const fetcher = transport();
  vi.stubGlobal("fetch", fetcher);
  const user = userEvent.setup();
  const rerender = mount();
  await user.selectOptions(
    await screen.findByRole("combobox", { name: "Schema referensi" }),
    "reference",
  );
  await user.selectOptions(screen.getByRole("combobox", { name: "Schema target" }), "public");
  await user.click(screen.getByRole("button", { name: "Bandingkan" }));
  rerender();
  expect(await screen.findByText(/Perubahan destructive terdeteksi/)).toBeTruthy();
  expect(screen.getByText(/custom_function/)).toBeTruthy();
  expect((screen.getByLabelText("SQL usulan") as HTMLTextAreaElement).value).toBe(difference.sql);
  expect(screen.queryByRole("button", { name: "Buat draft migration" })).toBeNull();
  expect(fetcher.mock.calls.some(([, init]) => init.method === "POST")).toBe(false);
});

it("rejects comparing a schema with itself without sending a diff request", async () => {
  const fetcher = transport();
  vi.stubGlobal("fetch", fetcher);
  const user = userEvent.setup();
  mount();
  await user.selectOptions(
    await screen.findByRole("combobox", { name: "Schema referensi" }),
    "public",
  );
  await user.selectOptions(screen.getByRole("combobox", { name: "Schema target" }), "public");
  await user.click(screen.getByRole("button", { name: "Bandingkan" }));
  expect(await screen.findByText("Pilih dua schema yang berbeda.")).toBeTruthy();
  expect(fetcher.mock.calls.some(([path]) => String(path).includes("/schema-diff?"))).toBe(false);
});

it.each([false, true])(
  "creates a recomputed draft or handles a server-side permission rejection (rejected=%s)",
  async (rejected) => {
    canDraft = true;
    params = new URLSearchParams({ reference_schema: "reference", target_schema: "public" });
    const fetcher = transport(rejected);
    vi.stubGlobal("fetch", fetcher);
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Buat draft migration" }));
    await user.click(screen.getByRole("button", { name: "Buat draft" }));
    if (rejected) {
      expect(
        await screen.findByText("Akses ditolak. Hak akses Anda mungkin telah berubah."),
      ).toBeTruthy();
      expect(screen.queryByText(/Draft #77 berhasil dibuat/)).toBeNull();
    } else {
      expect(await screen.findByText(/Draft #77 berhasil dibuat/)).toBeTruthy();
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    }
    expect(fetcher).toHaveBeenCalledWith(
      "/api/v1/databases/1/diff-drafts",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          reference_schema: "reference",
          target_schema: "public",
          title: "Sync public with reference",
        }),
      }),
    );
  },
);
