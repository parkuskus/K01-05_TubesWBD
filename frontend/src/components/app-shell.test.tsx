// @vitest-environment jsdom

import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AppShell } from "./app-shell";
import { WorkspaceProvider } from "./workspace-provider";

let pathname = "/";
const identity = { username: "current-user", is_platform_admin: false };
vi.mock("next/navigation", () => ({ usePathname: () => pathname }));
vi.mock("@/components/auth-provider", () => ({
  useAuth: () => ({
    user: identity,
    logout: vi.fn(),
  }),
}));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  pathname = "/";
  identity.is_platform_admin = false;
});

it("exposes admin navigation only to an admin and marks nested pages as active", () => {
  vi.stubGlobal(
    "matchMedia",
    vi.fn(() => new EventTarget()),
  );
  pathname = "/platform/groups/7";
  const view = render(
    <AppShell>
      <p>Konten grup</p>
    </AppShell>,
  );
  expect(screen.queryByRole("link", { name: "Pengguna" })).toBeNull();
  expect(screen.queryByRole("link", { name: "Grup" })).toBeNull();
  expect(screen.queryByRole("link", { name: "Koneksi" })).toBeNull();
  identity.is_platform_admin = true;
  view.rerender(
    <AppShell>
      <p>Konten grup</p>
    </AppShell>,
  );
  expect(screen.getByRole("link", { name: "Grup" }).getAttribute("aria-current")).toBe("page");
  expect(screen.getByRole("link", { name: "Pengguna" }).getAttribute("href")).toBe(
    "/platform/users",
  );
  expect(screen.getByRole("link", { name: "Koneksi" }).getAttribute("href")).toBe("/connections");
  pathname = "/databases/1/schema-diff";
  view.rerender(
    <AppShell>
      <p>Schema diff</p>
    </AppShell>,
  );
  expect(screen.getByRole("link", { name: "Database" }).getAttribute("aria-current")).toBe("page");
});

it("closes mobile navigation at the desktop breakpoint and restores page interaction", async () => {
  const desktop = Object.assign(new EventTarget(), {
    matches: false,
    media: "(min-width: 64rem)",
  });
  vi.stubGlobal(
    "matchMedia",
    vi.fn(() => desktop),
  );
  const user = userEvent.setup();
  render(
    <WorkspaceProvider snapshot={{ source: "disconnected", databases: [], activity: [] }}>
      <AppShell>
        <p>Contoh konten</p>
      </AppShell>
    </WorkspaceProvider>,
  );
  await user.click(screen.getByRole("button", { name: "Buka navigasi" }));
  expect(screen.getByRole("dialog")).toBeTruthy();
  expect(document.body.style.pointerEvents).toBe("none");
  act(() => desktop.dispatchEvent(Object.assign(new Event("change"), { matches: false })));
  expect(screen.getByRole("dialog")).toBeTruthy();

  act(() => {
    desktop.matches = true;
    desktop.dispatchEvent(Object.assign(new Event("change"), { matches: true }));
  });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(document.body.style.pointerEvents).toBe("");
  expect(document.activeElement).toBe(screen.getByRole("main"));
  expect(screen.getByRole("main").closest('[aria-hidden="true"]')).toBeNull();

  act(() => {
    desktop.matches = false;
    desktop.dispatchEvent(Object.assign(new Event("change"), { matches: false }));
  });
  expect(screen.queryByRole("dialog")).toBeNull();
  await user.click(screen.getByRole("button", { name: "Buka navigasi" }));
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Buka navigasi" }));
});
