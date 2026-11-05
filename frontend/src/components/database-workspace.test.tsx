// @vitest-environment jsdom

import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DatabaseWorkspace } from "./database-workspace";
import { WorkspaceProvider } from "./workspace-provider";
import { workspaceFixture } from "@/lib/fixtures/workspace";

afterEach(cleanup);

it("combines search, environment and access filters without affecting the source data", async () => {
  const user = userEvent.setup();
  render(
    <WorkspaceProvider snapshot={workspaceFixture}>
      <DatabaseWorkspace />
    </WorkspaceProvider>,
  );
  const databases = within(screen.getByRole("region", { name: "Daftar database" }));
  await user.selectOptions(
    screen.getByRole("combobox", { name: "Lingkungan database" }),
    "Staging",
  );
  expect(databases.getByRole("button", { name: "analytics_staging" })).toBeTruthy();
  expect(databases.queryByRole("button", { name: "inventory_dev" })).toBeNull();
  await user.click(screen.getByRole("button", { name: /Dapat diedit/ }));
  expect(databases.getByText("Database tidak ditemukan")).toBeTruthy();
  await user.click(databases.getByRole("button", { name: "Reset filter" }));
  await user.type(screen.getByRole("searchbox", { name: "Cari database" }), "inventory");
  expect(databases.getByRole("button", { name: "inventory_dev" })).toBeTruthy();
  expect(databases.queryByRole("button", { name: "analytics_staging" })).toBeNull();
  expect(workspaceFixture.databases).toHaveLength(3);
});

it("shows a real disconnected state instead of filling production with sample records", () => {
  render(
    <WorkspaceProvider snapshot={{ source: "disconnected", databases: [], activity: [] }}>
      <DatabaseWorkspace />
    </WorkspaceProvider>,
  );
  expect(screen.getByText("Layanan belum terhubung")).toBeTruthy();
  expect(screen.queryByText("inventory_dev")).toBeNull();
});
