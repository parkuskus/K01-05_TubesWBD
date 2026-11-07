import { QueryClient, QueryClientProvider, focusManager } from "@tanstack/react-query";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createApiClient } from "@/lib/api/client";
import type {
  HistoryEvent,
  Job,
  MigrationDefinition,
  OperationRequest,
} from "@/lib/api/migrations";
import { ActivityWorkspace } from "./activity-workspace";
import { HistoryWorkspace } from "./history-workspace";
import { JobWorkspace } from "./job-workspace";
import { MigrationEditor } from "./migration-editor";
import { MigrationWorkspace } from "./migration-workspace";
import { OperationWorkspace } from "./operation-workspace";

const api = createApiClient();
const push = vi.fn();
const replace = vi.fn();
let params = new URLSearchParams();
const pathname = "/databases/1/history";
vi.mock("@/components/auth-provider", () => ({
  useAuth: () => ({ api, user: { id: "leader", is_platform_admin: false } }),
}));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push, replace }),
  useSearchParams: () => params,
  usePathname: () => pathname,
}));

const definition: MigrationDefinition = {
  id: "m1",
  database_id: "1",
  title: "Backfill items",
  version: "1",
  revision_id: "r1",
  up_sql: "UPDATE public.items SET migrated = true;",
  down_sql: "UPDATE public.items SET migrated = false;",
  is_irreversible: false,
  irreversible_reason: "",
  mode: "NORMAL",
  batch: null,
  compatibility_notes: "Check compatibility with the reporting app.",
  ever_executed: false,
  expected_active_head: "E0",
  active_execution_id: "",
  capabilities: {
    edit: true,
    request_apply: true,
    request_rollback: false,
    request_forward: false,
  },
};
const operation: OperationRequest = {
  id: "req1",
  database_id: "1",
  migration_id: "m1",
  title: definition.title,
  direction: "APPLY",
  status: "PENDING_REVIEW",
  created_at: "2026-11-06T00:00:00Z",
  job_id: null,
  round: 2,
  plan_checksum: "approved-checksum",
  compatibility_notes: definition.compatibility_notes,
  plan: { ...definition, origin_execution_id: "" },
  requirements: [
    {
      group_id: "viewers",
      group_name: "Reporting",
      access_level: 1,
      leader_id: "leader",
      leader_name: "Viewer leader",
      status: "PENDING",
      can_approve: true,
    },
    {
      group_id: "editors",
      group_name: "Core app",
      access_level: 2,
      leader_id: "other",
      leader_name: "Editor leader",
      status: "PENDING",
      can_approve: false,
    },
  ],
  capabilities: { enqueue: false },
};
const job: Job = {
  job_id: "j1",
  request_id: "req1",
  execution_id: "E1",
  database_id: "1",
  direction: "APPLY",
  mode: "BATCH",
  status: "RUNNING",
  actor_id: "editor",
  retry_count: 0,
  max_retries: 3,
  committed_batches: 2,
  committed_rows: 20000,
  total_scope_rows: 50000,
  last_committed_cursor: "20000",
  error_code: "",
  error_message: "",
  capabilities: { request_resume: false, request_compensation: false },
  created_at: "2026-11-06T00:00:00Z",
  updated_at: "2026-11-06T00:01:00Z",
  completed_at: null,
};

type Handler = (path: string, init: RequestInit) => Response | Promise<Response>;
function transport(handler: Handler, level = 2) {
  const fetcher = vi.fn((path: string, init: RequestInit) => {
    if (path === "/api/v1/databases/1")
      return Promise.resolve(
        Response.json({ data: { database: { id: "1", name: "Inventory", level } } }),
      );
    return Promise.resolve(handler(path, init));
  });
  vi.stubGlobal("fetch", fetcher);
  return fetcher;
}
function mount(content: React.ReactNode) {
  const client = new QueryClient({
    defaultOptions: {
      queries: { retry: false, refetchOnWindowFocus: false },
      mutations: { retry: false },
    },
  });
  const view = render(<QueryClientProvider client={client}>{content}</QueryClientProvider>);
  return {
    client,
    rerender: (next = content) =>
      view.rerender(<QueryClientProvider client={client}>{next}</QueryClientProvider>),
  };
}
const data = (value: unknown, status = 200) => Response.json({ data: value }, { status });
const errorResponse = (status: number, code: string) =>
  Response.json({ error: { code, message: "Unsafe internal detail" } }, { status });

beforeEach(() => {
  api.setCsrfToken("test-csrf");
  params = new URLSearchParams();
  focusManager.setFocused(true);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  focusManager.setFocused(undefined);
});

it("shows saved definitions and links to a new draft and the correct revision", async () => {
  transport(() => data({ migrations: [definition] }));
  mount(<MigrationWorkspace id="1" />);
  expect((await screen.findByRole("link", { name: /v1 · Backfill/ })).getAttribute("href")).toBe(
    "/databases/1/migrations/m1",
  );
  expect(screen.getByRole("link", { name: "Buat migration" }).getAttribute("href")).toBe(
    "/databases/1/migrations/new",
  );
});

it("blocks new draft creation for view-only users without loading migrations", async () => {
  const fetcher = transport(() => {
    throw new Error("Unexpected request");
  }, 1);
  mount(<MigrationEditor id="1" />);
  expect(await screen.findByText("Akses ditolak")).toBeTruthy();
  expect(fetcher).toHaveBeenCalledTimes(1);
});

it("creates an irreversible batch draft without down SQL and opens its saved URL", async () => {
  const fetcher = transport(() => data({ ...definition, id: "m2" }, 201));
  const user = userEvent.setup();
  mount(<MigrationEditor id="1" />);
  fireEvent.change(await screen.findByLabelText("Judul migration"), {
    target: { value: "Backfill" },
  });
  fireEvent.change(screen.getByLabelText("SQL up"), { target: { value: definition.up_sql } });
  await user.click(screen.getByLabelText("Irreversible"));
  fireEvent.change(screen.getByLabelText("Alasan irreversible"), {
    target: { value: "Original values cannot be restored" },
  });
  await user.selectOptions(screen.getByLabelText("Mode eksekusi"), "BATCH");
  fireEvent.change(screen.getByLabelText("Tabel batch"), { target: { value: "items" } });
  await user.click(screen.getByRole("button", { name: "Simpan draft" }));
  await waitFor(() => expect(replace).toHaveBeenCalledWith("/databases/1/migrations/m2"));
  const [, init] = fetcher.mock.calls.find(([, init]) => init.method === "POST")!;
  expect(JSON.parse(String(init.body))).toMatchObject({
    is_irreversible: true,
    down_sql: "",
    irreversible_reason: "Original values cannot be restored",
    mode: "BATCH",
    batch: {
      table_schema: "public",
      table_name: "items",
      primary_key_column: "id",
      scope_where: "",
      batch_size: 10000,
    },
  });
  expect(init.headers).toBeInstanceOf(Headers);
  expect((init.headers as Headers).get("X-CSRF-Token")).toBe("test-csrf");
});

it("sends optimistic revision and preserves the editor after a conflict", async () => {
  const fetcher = transport((_path, init) =>
    init.method === "PATCH" ? errorResponse(409, "REVISION_CONFLICT") : data(definition),
  );
  mount(<MigrationEditor id="1" migrationId="m1" />);
  fireEvent.change(await screen.findByLabelText("Judul migration"), {
    target: { value: "Updated title" },
  });
  await userEvent.click(screen.getByRole("button", { name: "Simpan draft" }));
  expect(await screen.findByRole("alert")).toBeTruthy();
  expect((screen.getByLabelText("Judul migration") as HTMLInputElement).value).toBe(
    "Updated title",
  );
  const [, init] = fetcher.mock.calls.find(([, init]) => init.method === "PATCH")!;
  expect(JSON.parse(String(init.body)).expected_revision).toBe("r1");
  expect((screen.getByRole("button", { name: "Simpan draft" }) as HTMLButtonElement).disabled).toBe(
    true,
  );
  expect(screen.getByRole("button", { name: "Muat revisi terbaru" })).toBeTruthy();
  expect(screen.queryByText("Unsafe internal detail")).toBeNull();
});

it("focuses API field errors and retains SQL for correction", async () => {
  transport((_path, init) =>
    init.method === "PATCH"
      ? Response.json(
          { error: { code: "VALIDATION_FAILED", field_errors: { up_sql: "SQL up belum valid." } } },
          { status: 422 },
        )
      : data(definition),
  );
  mount(<MigrationEditor id="1" migrationId="m1" />);
  await userEvent.click(await screen.findByRole("button", { name: "Simpan draft" }));
  expect(await screen.findByText("SQL up belum valid.")).toBeTruthy();
  await waitFor(() => expect(document.activeElement).toBe(screen.getByLabelText("SQL up")));
  expect((screen.getByLabelText("SQL up") as HTMLTextAreaElement).value).toBe(definition.up_sql);
});

it("preserves down SQL and batch settings when switching execution options", async () => {
  transport(() => data(definition));
  mount(<MigrationEditor id="1" migrationId="m1" />);
  fireEvent.change(await screen.findByLabelText("SQL down"), {
    target: { value: "UPDATE items SET count = 0;" },
  });
  await userEvent.click(screen.getByLabelText("Irreversible"));
  await userEvent.click(screen.getByLabelText("Irreversible"));
  expect((screen.getByLabelText("SQL down") as HTMLTextAreaElement).value).toBe(
    "UPDATE items SET count = 0;",
  );
  await userEvent.selectOptions(screen.getByLabelText("Mode eksekusi"), "BATCH");
  fireEvent.change(screen.getByLabelText("Tabel batch"), { target: { value: "items" } });
  await userEvent.selectOptions(screen.getByLabelText("Mode eksekusi"), "NORMAL");
  await userEvent.selectOptions(screen.getByLabelText("Mode eksekusi"), "BATCH");
  expect((screen.getByLabelText("Tabel batch") as HTMLInputElement).value).toBe("items");
});

it("requires saved validation, submits the original revision for review, and invalidates validation on edit", async () => {
  const fetcher = transport((path) =>
    path.endsWith("/validate")
      ? data({
          revision_id: "r1",
          valid: true,
          checks: [{ code: "SQL", valid: true, message: "SQL valid" }],
          warnings: ["Review application compatibility"],
        })
      : path.endsWith("/operation-requests")
        ? data(operation, 201)
        : data(definition),
  );
  mount(<MigrationEditor id="1" migrationId="m1" />);
  await screen.findByLabelText("SQL up");
  expect(screen.queryByRole("button", { name: "Ajukan Apply untuk review" })).toBeNull();
  await userEvent.click(screen.getByRole("button", { name: "Validasi revisi tersimpan" }));
  await userEvent.click(await screen.findByRole("button", { name: "Ajukan Apply untuk review" }));
  await userEvent.click(screen.getByRole("button", { name: "Ajukan review" }));
  await waitFor(() => expect(push).toHaveBeenCalledWith("/operation-requests/req1"));
  const [, init] = fetcher.mock.calls.find(([path]) => path.endsWith("/operation-requests"))!;
  expect(JSON.parse(String(init.body))).toEqual({
    migration_id: "m1",
    revision_id: "r1",
    direction: "APPLY",
    expected_active_head: "E0",
  });
  fireEvent.change(screen.getByLabelText("SQL up"), {
    target: { value: "UPDATE items SET migrated = false;" },
  });
  expect(screen.queryByText("Validasi lolos")).toBeNull();
  expect(
    (screen.getByRole("button", { name: "Validasi revisi tersimpan" }) as HTMLButtonElement)
      .disabled,
  ).toBe(true);
});

it("keeps SQL locked after a failed attempt and offers rollback only for a reversible active head", async () => {
  transport((path) =>
    path.endsWith("/validate")
      ? data({ revision_id: "r1", valid: true, checks: [], warnings: [] })
      : data({
          ...definition,
          ever_executed: true,
          active_execution_id: "E1",
          capabilities: {
            ...definition.capabilities,
            request_apply: false,
            request_rollback: true,
          },
        }),
  );
  mount(<MigrationEditor id="1" migrationId="m1" />);
  await screen.findByText(/Perbaikan SQL harus dibuat/);
  expect(screen.queryByRole("button", { name: "Simpan draft" })).toBeNull();
  expect((screen.getByLabelText("SQL up asli") as HTMLTextAreaElement).readOnly).toBe(true);
  await userEvent.click(screen.getByRole("button", { name: "Validasi revisi tersimpan" }));
  expect(await screen.findByRole("button", { name: "Ajukan Rollback untuk review" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Ajukan Forward untuk review" })).toBeNull();
});

it("lets a view-only leader approve only their group and round without enqueue access", async () => {
  const fetcher = transport((path) =>
    path.endsWith("/approvals") ? new Response(null, { status: 204 }) : data(operation),
  );
  mount(<OperationWorkspace id="req1" />);
  await userEvent.click(await screen.findByRole("button", { name: "Setujui untuk Reporting" }));
  expect(screen.queryByRole("button", { name: "Setujui untuk Core app" })).toBeNull();
  await userEvent.click(screen.getByRole("button", { name: "Setujui plan" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  const [, init] = fetcher.mock.calls.find(([, init]) => init.method === "POST")!;
  expect(JSON.parse(String(init.body))).toEqual({
    group_id: "viewers",
    round: 2,
    plan_checksum: "approved-checksum",
    decision: "APPROVE",
  });
  expect(screen.queryByRole("button", { name: "Masukkan ke antrean" })).toBeNull();
});

it.each([403, 409])("handles approval rejection from the server (%s)", async (status) => {
  transport((path) =>
    path.endsWith("/approvals")
      ? errorResponse(status, status === 409 ? "APPROVAL_STALE" : "FORBIDDEN")
      : data(operation),
  );
  mount(<OperationWorkspace id="req1" />);
  await userEvent.click(await screen.findByRole("button", { name: "Setujui untuk Reporting" }));
  await userEvent.click(screen.getByRole("button", { name: "Setujui plan" }));
  expect(await screen.findByRole("alert")).toBeTruthy();
  expect(screen.getByRole("dialog")).toBeTruthy();
  expect(screen.queryByText("Unsafe internal detail")).toBeNull();
});

it("opens the job URL only after enqueue returns a job ID", async () => {
  const fetcher = transport((path) =>
    path.endsWith("/jobs")
      ? data({ job_id: "j1", execution_id: "E1", status: "PENDING" }, 202)
      : data({ ...operation, status: "APPROVED", capabilities: { enqueue: true } }),
  );
  mount(<OperationWorkspace id="req1" />);
  await userEvent.click(await screen.findByRole("button", { name: "Masukkan ke antrean" }));
  await userEvent.click(screen.getByRole("button", { name: "Antrekan job" }));
  await waitFor(() => expect(push).toHaveBeenCalledWith("/jobs/j1"));
  const [, init] = fetcher.mock.calls.find(([path]) => path.endsWith("/jobs"))!;
  expect((init.headers as Headers).get("Idempotency-Key")).toBe(
    "operation-req1-2-approved-checksum",
  );
});

it("checks the request after an ambiguous enqueue and never automatically publishes again", async () => {
  let queued = false;
  const fetcher = transport((path) => {
    if (path.endsWith("/jobs")) {
      queued = true;
      throw new TypeError("Connection lost");
    }
    return data({
      ...operation,
      status: queued ? "QUEUED" : "APPROVED",
      job_id: queued ? "j1" : null,
      capabilities: { enqueue: !queued },
    });
  });
  mount(<OperationWorkspace id="req1" />);
  await userEvent.click(await screen.findByRole("button", { name: "Masukkan ke antrean" }));
  await userEvent.click(screen.getByRole("button", { name: "Antrekan job" }));
  await userEvent.click(await screen.findByRole("button", { name: "Periksa status pengajuan" }));
  await waitFor(() => expect(push).toHaveBeenCalledWith("/jobs/j1"));
  expect(fetcher.mock.calls.filter(([, init]) => init.method === "POST")).toHaveLength(1);
});

it("loads a job from its deep link, updates only committed progress, and stops polling at success", async () => {
  vi.useFakeTimers();
  let current = { ...job };
  const fetcher = transport((path) =>
    path.endsWith("/attempts") ? data({ attempts: [] }) : data(current),
  );
  mount(<JobWorkspace id="j1" />);
  await act(async () => {
    await vi.advanceTimersByTimeAsync(10);
  });
  expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe("40");
  current = { ...job, status: "SUCCESS", committed_rows: 50000, committed_batches: 5 };
  await act(async () => {
    await vi.advanceTimersByTimeAsync(2100);
  });
  expect(screen.getByText("Berhasil")).toBeTruthy();
  expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe("100");
  const calls = fetcher.mock.calls.length;
  await act(async () => {
    await vi.advanceTimersByTimeAsync(10000);
  });
  expect(fetcher.mock.calls.length).toBe(calls);
});

it("stops interval fetching while the page is hidden and resumes when focused", async () => {
  vi.useFakeTimers();
  const fetcher = transport((path) =>
    path.endsWith("/attempts") ? data({ attempts: [] }) : data(job),
  );
  mount(<JobWorkspace id="j1" />);
  await act(async () => {
    await vi.advanceTimersByTimeAsync(10);
  });
  focusManager.setFocused(false);
  const count = fetcher.mock.calls.length;
  await act(async () => {
    await vi.advanceTimersByTimeAsync(5000);
  });
  expect(fetcher.mock.calls.length).toBe(count);
  await act(async () => {
    focusManager.setFocused(true);
    await vi.advanceTimersByTimeAsync(2100);
  });
  expect(fetcher.mock.calls.length).toBeGreaterThan(count);
});

it.each(["PARTIAL", "PARTIALLY_REVERTED"] as const)(
  "submits recovery through a new review for %s without sending replacement SQL",
  async (status) => {
    const fetcher = transport((path) =>
      path.endsWith("/operation-requests")
        ? data(operation, 201)
        : path.endsWith("/attempts")
          ? data({ attempts: [] })
          : data({
              ...job,
              status,
              capabilities: { request_resume: true, request_compensation: true },
            }),
    );
    mount(<JobWorkspace id="j1" />);
    const label = status === "PARTIAL" ? "Ajukan kompensasi" : "Ajukan lanjutkan kompensasi";
    await userEvent.click(await screen.findByRole("button", { name: label }));
    await userEvent.click(screen.getByRole("button", { name: "Ajukan review pemulihan" }));
    await waitFor(() => expect(push).toHaveBeenCalledWith("/operation-requests/req1"));
    const [, init] = fetcher.mock.calls.find(([, init]) => init.method === "POST")!;
    expect(JSON.parse(String(init.body))).toEqual({
      direction: "COMPENSATE",
      origin_execution_id: "E1",
    });
    if (status === "PARTIALLY_REVERTED")
      expect(screen.queryByRole("button", { name: "Ajukan lanjutkan" })).toBeNull();
  },
);

it("keeps UNKNOWN gated even if recovery capabilities are incorrectly offered", async () => {
  transport((path) =>
    path.endsWith("/attempts")
      ? data({ attempts: [] })
      : data({
          ...job,
          status: "UNKNOWN",
          capabilities: { request_resume: true, request_compensation: true },
        }),
  );
  mount(<JobWorkspace id="j1" />);
  expect(await screen.findByText(/Hasil commit belum dapat dipastikan/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Ajukan lanjutkan" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Ajukan kompensasi" })).toBeNull();
});

it("shows failed attempts and the exact SQL separately from committed history", async () => {
  transport((path) =>
    path.endsWith("/attempts")
      ? data({
          attempts: [
            {
              attempt_id: "a1",
              job_id: "j1",
              execution_id: "E1",
              attempt_no: 1,
              status: "ROLLED_BACK",
              executed_sql: definition.up_sql,
              error_code: "23505",
              error_message: "Constraint violation",
              started_at: job.created_at,
              finished_at: job.updated_at,
            },
          ],
        })
      : data({ ...job, status: "FAILED" }),
  );
  mount(<JobWorkspace id="j1" />);
  await userEvent.click(await screen.findByText(/Attempt 1 ·/));
  expect((screen.getByLabelText("SQL attempt") as HTMLTextAreaElement).value).toBe(
    definition.up_sql,
  );
  expect(screen.getByText(/23505 · Constraint violation/)).toBeTruthy();
});

it("uses server-filtered E1–E5 history before pagination and resets the cursor when Hide rollback changes", async () => {
  params = new URLSearchParams({ cursor: "old-cursor" });
  const events: HistoryEvent[] = ["E1", "E2", "E3", "E4", "E5"].map((execution_id, index) => ({
    execution_id,
    job_id: `j${index}`,
    migration_id: index === 0 ? "A" : index === 4 ? "C" : "B",
    revision_id: "r1",
    version: index === 0 ? "1" : index === 4 ? "3" : "2",
    direction: index === 2 ? "ROLLBACK" : index === 3 ? "FORWARD" : "APPLY",
    mode: "NORMAL",
    executed_sql: definition.up_sql,
    actor_id: "editor",
    reverts_execution_id: index === 2 ? "E2" : "",
    committed_at: job.created_at,
  }));
  const fetcher = transport((path) =>
    data({
      events: path.includes("hide_rollback=true")
        ? events.filter((event) => !["E2", "E3"].includes(event.execution_id))
        : events,
      next_cursor: "next",
    }),
  );
  const { rerender } = mount(<HistoryWorkspace id="1" />);
  await screen.findByText(/v2 · Rollback · E3/);
  await userEvent.click(screen.getByLabelText("Hide rollback"));
  expect(push).toHaveBeenCalledWith("/databases/1/history?hide_rollback=true", { scroll: false });
  params = new URLSearchParams({ hide_rollback: "true" });
  rerender(<HistoryWorkspace id="1" />);
  await waitFor(() => expect(screen.queryByText(/v2 · Rollback · E3/)).toBeNull());
  expect(await screen.findByText(/v2 · Forward · E4/)).toBeTruthy();
  expect(screen.getByText(/v1 · Apply · E1/)).toBeTruthy();
  expect(screen.getByText(/v3 · Apply · E5/)).toBeTruthy();
  expect(
    fetcher.mock.calls.some(([path]) => path === "/api/v1/databases/1/history?hide_rollback=true"),
  ).toBe(true);
  await userEvent.click(screen.getByRole("button", { name: "Halaman berikutnya" }));
  expect(push).toHaveBeenCalledWith("/databases/1/history?hide_rollback=true&cursor=next", {
    scroll: false,
  });
});

it.each([403, 404, 503])(
  "shows a real job API error without fabricated progress (%s)",
  async (status) => {
    const fetcher = transport(() => errorResponse(status, "UNAVAILABLE"));
    mount(<JobWorkspace id="j1" />);
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.queryByRole("progressbar")).toBeNull();
    expect(fetcher.mock.calls.some(([path]) => path.endsWith("/attempts"))).toBe(false);
  },
);

it("loads the review inbox for the selected accessible database and links to its job", async () => {
  params = new URLSearchParams({ database: "1" });
  transport((path) =>
    path === "/api/v1/databases"
      ? data({ databases: [{ id: "1", name: "Inventory", level: 1 }] })
      : data({ requests: [{ ...operation, status: "QUEUED", job_id: "j1" }] }),
  );
  mount(<ActivityWorkspace />);
  expect((await screen.findByRole("link", { name: "Pantau job" })).getAttribute("href")).toBe(
    "/jobs/j1",
  );
  expect(screen.getByRole("link", { name: definition.title }).getAttribute("href")).toBe(
    "/operation-requests/req1",
  );
});

it("hides previously loaded job details when access is revoked", async () => {
  let revoked = false;
  transport((path) =>
    revoked
      ? errorResponse(403, "FORBIDDEN")
      : path.endsWith("/attempts")
        ? data({ attempts: [] })
        : data(job),
  );
  mount(<JobWorkspace id="j1" />);
  await screen.findByRole("progressbar");
  revoked = true;
  await userEvent.click(screen.getByRole("button", { name: "Perbarui status" }));
  expect(await screen.findByText("Akses ditolak")).toBeTruthy();
  expect(screen.queryByRole("progressbar")).toBeNull();
  expect(screen.queryByText("E1")).toBeNull();
});

it("keeps the same enqueue key after checking an unsuccessful ambiguous submission", async () => {
  let submissions = 0;
  const fetcher = transport((path) => {
    if (path.endsWith("/jobs")) {
      submissions++;
      if (submissions === 1) throw new TypeError("Connection lost");
      return data({ job_id: "j1", execution_id: "E1", status: "PENDING" }, 202);
    }
    return data({ ...operation, status: "APPROVED", capabilities: { enqueue: true } });
  });
  mount(<OperationWorkspace id="req1" />);
  await userEvent.click(await screen.findByRole("button", { name: "Masukkan ke antrean" }));
  await userEvent.click(screen.getByRole("button", { name: "Antrekan job" }));
  await userEvent.click(await screen.findByRole("button", { name: "Periksa status pengajuan" }));
  await userEvent.click(await screen.findByRole("button", { name: "Masukkan ke antrean" }));
  await userEvent.click(screen.getByRole("button", { name: "Antrekan job" }));
  await waitFor(() => expect(push).toHaveBeenCalledWith("/jobs/j1"));
  const keys = fetcher.mock.calls
    .filter(([, init]) => init.method === "POST")
    .map(([, init]) => (init.headers as Headers).get("Idempotency-Key"));
  expect(keys).toEqual([
    "operation-req1-2-approved-checksum",
    "operation-req1-2-approved-checksum",
  ]);
});

it("requests a resume of the same execution and handles a forbidden recovery", async () => {
  const fetcher = transport((path) =>
    path.endsWith("/operation-requests")
      ? errorResponse(403, "FORBIDDEN")
      : path.endsWith("/attempts")
        ? data({ attempts: [] })
        : data({
            ...job,
            status: "PARTIAL",
            capabilities: { request_resume: true, request_compensation: false },
          }),
  );
  mount(<JobWorkspace id="j1" />);
  await userEvent.click(await screen.findByRole("button", { name: "Ajukan lanjutkan" }));
  await userEvent.click(screen.getByRole("button", { name: "Ajukan review pemulihan" }));
  expect(await screen.findByRole("alert")).toBeTruthy();
  const [, init] = fetcher.mock.calls.find(([, init]) => init.method === "POST")!;
  expect(JSON.parse(String(init.body))).toEqual({ direction: "RESUME", origin_execution_id: "E1" });
  expect(screen.getByRole("dialog")).toBeTruthy();
});

it("explains unsafe compensation and prevents another recovery submission", async () => {
  transport((path) =>
    path.endsWith("/attempts")
      ? data({ attempts: [] })
      : data({
          ...job,
          status: "PARTIALLY_REVERTED",
          error_code: "UNSAFE_BATCH_COMPENSATION",
          capabilities: { request_resume: true, request_compensation: true },
        }),
  );
  mount(<JobWorkspace id="j1" />);
  expect(await screen.findByText(/Buat migration perbaikan baru/)).toBeTruthy();
  expect(
    (screen.getByRole("button", { name: "Ajukan lanjutkan kompensasi" }) as HTMLButtonElement)
      .disabled,
  ).toBe(true);
});

it.each(["ROLLBACK", "FORWARD"] as const)(
  "submits %s with the saved revision and head, without replacement SQL",
  async (direction) => {
    const fetcher = transport((path) =>
      path.endsWith("/validate")
        ? data({ revision_id: "r1", valid: true, checks: [], warnings: [] })
        : path.endsWith("/operation-requests")
          ? data(operation, 201)
          : data({
              ...definition,
              active_execution_id: "E1",
              ever_executed: true,
              capabilities: {
                edit: false,
                request_apply: false,
                request_rollback: direction === "ROLLBACK",
                request_forward: direction === "FORWARD",
              },
            }),
    );
    mount(<MigrationEditor id="1" migrationId="m1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Validasi revisi tersimpan" }));
    await userEvent.click(
      await screen.findByRole("button", {
        name: `Ajukan ${direction === "ROLLBACK" ? "Rollback" : "Forward"} untuk review`,
      }),
    );
    await userEvent.click(screen.getByRole("button", { name: "Ajukan review" }));
    await waitFor(() => expect(push).toHaveBeenCalledWith("/operation-requests/req1"));
    const [, init] = fetcher.mock.calls.find(([path]) => path.endsWith("/operation-requests"))!;
    expect(JSON.parse(String(init.body))).toEqual({
      migration_id: "m1",
      revision_id: "r1",
      direction,
      expected_active_head: "E0",
      ...(direction === "ROLLBACK" ? { origin_execution_id: "E1" } : {}),
    });
  },
);
