import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createApiClient } from "@/lib/api/client";
import { UsersWorkspace } from "./users-workspace";
import { GroupsWorkspace, GroupWorkspace } from "./groups-workspace";

const api = createApiClient();
const identity = { id: "1", username: "admin", is_platform_admin: true };
const refreshSession = vi.fn();
const push = vi.fn();
vi.mock("@/components/auth-provider", () => ({
  useAuth: () => ({ api, user: identity, refreshSession }),
}));
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));

const users = [
  identity,
  { id: "2", username: "member", is_platform_admin: false },
  { id: "3", username: "newcomer", is_platform_admin: false },
];

function mount(content: React.ReactNode) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(<QueryClientProvider client={client}>{content}</QueryClientProvider>);
}

beforeEach(() => {
  api.setCsrfToken("test-csrf");
  identity.is_platform_admin = true;
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it.each([
  <UsersWorkspace key="users" />,
  <GroupsWorkspace key="groups" />,
  <GroupWorkspace key="group" id="7" />,
])("blocks non-admin pages before making resource requests", async (page) => {
  identity.is_platform_admin = false;
  const fetcher = vi.fn();
  vi.stubGlobal("fetch", fetcher);
  mount(page);
  expect(screen.getByText("Akses ditolak")).toBeTruthy();
  expect(fetcher).not.toHaveBeenCalled();
});

it("creates a user through JSON and refreshes the account list", async () => {
  const fetcher = vi.fn().mockImplementation((path: string, init: RequestInit) => {
    if (init.method === "POST")
      return Promise.resolve(Response.json({ data: { id: "4" } }, { status: 201 }));
    return Promise.resolve(Response.json({ data: { users } }));
  });
  vi.stubGlobal("fetch", fetcher);
  const user = userEvent.setup();
  mount(<UsersWorkspace />);
  await user.click(await screen.findByRole("button", { name: "Tambah pengguna" }));
  await user.type(screen.getByLabelText("Username"), "new-user");
  await user.type(screen.getByLabelText("Password"), "new-password");
  await user.click(screen.getByRole("button", { name: "Simpan" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  const [, init] = fetcher.mock.calls.find(([, init]) => init.method === "POST")!;
  expect(JSON.parse(init.body)).toEqual({ username: "new-user", password: "new-password" });
  expect(init.headers.get("X-CSRF-Token")).toBe("test-csrf");
  expect(refreshSession).toHaveBeenCalled();
});

it("requires confirmation for role removal and preserves a last-admin rejection", async () => {
  const fetcher = vi
    .fn()
    .mockImplementation((_path: string, init: RequestInit) =>
      Promise.resolve(
        init.method === "PATCH"
          ? Response.json(
              { error: { code: "LAST_ADMIN", message: "internal unsafe detail" } },
              { status: 409 },
            )
          : Response.json({ data: { users: [identity] } }),
      ),
    );
  vi.stubGlobal("fetch", fetcher);
  const user = userEvent.setup();
  mount(<UsersWorkspace />);
  await user.click(await screen.findByRole("button", { name: "Cabut admin" }));
  expect(fetcher.mock.calls.filter(([, init]) => init.method === "PATCH")).toHaveLength(0);
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Cabut admin" }));
  expect(
    await screen.findByText(
      "Administrator terakhir tidak dapat dihapus atau diturunkan hak aksesnya.",
    ),
  ).toBeTruthy();
  expect(screen.getByRole("dialog")).toBeTruthy();
  expect(screen.queryByText("internal unsafe detail")).toBeNull();
});

it("focuses sanitized field validation and keeps the user form available for correction", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockImplementation((_path: string, init: RequestInit) =>
      Promise.resolve(
        init.method === "POST"
          ? Response.json(
              {
                error: {
                  code: "VALIDATION_FAILED",
                  field_errors: { username: "Username sudah dipakai.", unknown: "Hidden field" },
                },
              },
              { status: 422 },
            )
          : Response.json({ data: { users } }),
      ),
    ),
  );
  const user = userEvent.setup();
  mount(<UsersWorkspace />);
  await user.click(await screen.findByRole("button", { name: "Tambah pengguna" }));
  await user.type(screen.getByLabelText("Username"), "member");
  await user.type(screen.getByLabelText("Password"), "test-password");
  await user.click(screen.getByRole("button", { name: "Simpan" }));
  expect(await screen.findByText("Username sudah dipakai.")).toBeTruthy();
  await waitFor(() => expect(document.activeElement).toBe(screen.getByLabelText("Username")));
  expect(screen.queryByText("Hidden field")).toBeNull();
});

function groupTransport() {
  let leader = "1";
  return vi.fn().mockImplementation((path: string, init: RequestInit) => {
    if (path === "/api/v1/groups/7/leader") {
      leader = JSON.parse(String(init.body)).user_id;
      return Promise.resolve(new Response(null, { status: 204 }));
    }
    if (path === "/api/v1/groups/7")
      return Promise.resolve(
        Response.json({
          data: {
            group: { id: "7", name: "Team", leader_user_id: leader },
          },
        }),
      );
    if (path === "/api/v1/groups/7/members")
      return Promise.resolve(Response.json({ data: { members: users.slice(0, 2) } }));
    if (path === "/api/v1/groups/7/grants")
      return Promise.resolve(
        Response.json({ data: { grants: [{ database_id: "99", level: 1 }] } }),
      );
    if (path === "/api/v1/users") return Promise.resolve(Response.json({ data: { users } }));
    if (path === "/api/v1/connections")
      return Promise.resolve(
        Response.json({ data: { connections: [{ id: "99", name: "Restricted target" }] } }),
      );
    return Promise.resolve(new Response(null, { status: 204 }));
  });
}

it("offers only members as leader candidates and requires replacing a leader before removal", async () => {
  const fetcher = groupTransport();
  vi.stubGlobal("fetch", fetcher);
  const user = userEvent.setup();
  mount(<GroupWorkspace id="7" />);
  await user.click(await screen.findByRole("button", { name: "Ganti leader" }));
  const select = screen.getByRole("combobox", { name: "Leader" });
  expect(within(select).queryByRole("option", { name: "newcomer" })).toBeNull();
  await user.selectOptions(select, "2");
  await user.click(
    within(screen.getByRole("dialog")).getByRole("button", { name: "Tetapkan leader" }),
  );
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  const members = within(screen.getByRole("region", { name: "Daftar anggota" }));
  const leaderRow = members.getByRole("row", { name: /member Leader/ });
  expect(within(leaderRow).queryByRole("button", { name: "Hapus anggota" })).toBeNull();
  expect(fetcher).toHaveBeenCalledWith(
    "/api/v1/groups/7/leader",
    expect.objectContaining({ method: "PUT", body: JSON.stringify({ user_id: "2" }) }),
  );
});

it("adds members and sets grants using the admin connection list without implying target access", async () => {
  const fetcher = groupTransport();
  vi.stubGlobal("fetch", fetcher);
  const user = userEvent.setup();
  mount(<GroupWorkspace id="7" />);
  await user.click(await screen.findByRole("button", { name: "Tambah anggota" }));
  const select = screen.getByRole("combobox", { name: "Pengguna" });
  expect(within(select).queryByRole("option", { name: "member" })).toBeNull();
  await user.selectOptions(select, "3");
  await user.click(screen.getByRole("button", { name: "Simpan" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(fetcher).toHaveBeenCalledWith(
    "/api/v1/groups/7/members/3",
    expect.objectContaining({ method: "PUT" }),
  );
  await user.click(screen.getByRole("button", { name: "Atur akses" }));
  await user.selectOptions(screen.getByRole("combobox", { name: "Database" }), "99");
  await user.selectOptions(screen.getByRole("combobox", { name: "Hak akses" }), "2");
  await user.click(screen.getByRole("button", { name: "Simpan" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(fetcher).toHaveBeenCalledWith(
    "/api/v1/groups/7/grants/99",
    expect.objectContaining({ method: "PUT", body: JSON.stringify({ level: 2 }) }),
  );
  expect(fetcher.mock.calls.some(([path]) => path === "/api/v1/databases")).toBe(false);
});

it("creates a group with no invented leader and links to its detail page", async () => {
  const fetcher = vi
    .fn()
    .mockImplementation((_path: string, init: RequestInit) =>
      Promise.resolve(
        init.method === "POST"
          ? new Response(null, { status: 204 })
          : Response.json({ data: { groups: [{ id: "7", name: "Team", leader_user_id: null }] } }),
      ),
    );
  vi.stubGlobal("fetch", fetcher);
  const user = userEvent.setup();
  mount(<GroupsWorkspace />);
  expect((await screen.findByRole("link", { name: "Team" })).getAttribute("href")).toBe(
    "/platform/groups/7",
  );
  expect(screen.getByText("Belum ditetapkan")).toBeTruthy();
  await user.click(screen.getByRole("button", { name: "Tambah grup" }));
  await user.type(screen.getByLabelText("Nama grup"), "New team");
  await user.click(screen.getByRole("button", { name: "Simpan" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(fetcher).toHaveBeenCalledWith(
    "/api/v1/groups",
    expect.objectContaining({ method: "POST", body: JSON.stringify({ name: "New team" }) }),
  );
});
