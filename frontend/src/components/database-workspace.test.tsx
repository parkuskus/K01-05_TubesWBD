// @vitest-environment jsdom

import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DatabaseWorkspace } from "./database-workspace";
import { WorkspaceProvider } from "./workspace-provider";
import type { WorkspaceSnapshot } from "@/lib/workspace/types";

const workspace: WorkspaceSnapshot = {
  source: "connected",
  activity: [],
  databases: [
    {
      id: "1",
      name: "primary",
      host: "localhost",
      port: 5432,
      database: "primary",
      environment: "Development",
      schemas: 1,
      tables: 1,
      access: "View + Edit",
    },
    {
      id: "2",
      name: "reporting",
      host: "localhost",
      port: 5432,
      database: "reporting",
      environment: "Staging",
      schemas: 1,
      tables: 1,
      access: "View Only",
    },
  ],
};

afterEach(cleanup);

it("combines search, environment and access filters without affecting the source data", async () => {
  const user = userEvent.setup();
  render(
    <WorkspaceProvider snapshot={workspace}>
      <DatabaseWorkspace />
    </WorkspaceProvider>,
  );
  const databases = within(screen.getByRole("region", { name: "Daftar database" }));
  await user.selectOptions(
    screen.getByRole("combobox", { name: "Lingkungan database" }),
    "Staging",
  );
  expect(databases.getByRole("button", { name: "reporting" })).toBeTruthy();
  expect(databases.queryByRole("button", { name: "primary" })).toBeNull();
  await user.click(screen.getByRole("button", { name: /Dapat diedit/ }));
  expect(databases.getByText("Database tidak ditemukan")).toBeTruthy();
  await user.click(databases.getByRole("button", { name: "Reset filter" }));
  await user.type(screen.getByRole("searchbox", { name: "Cari database" }), "primary");
  expect(databases.getByRole("button", { name: "primary" })).toBeTruthy();
  expect(databases.queryByRole("button", { name: "reporting" })).toBeNull();
  expect(workspace.databases).toHaveLength(2);
});

it("shows a real disconnected state instead of filling production with sample records", () => {
  render(
    <WorkspaceProvider>
      <DatabaseWorkspace />
    </WorkspaceProvider>,
  );
  expect(screen.getByText("Layanan belum terhubung")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "primary" })).toBeNull();
});
