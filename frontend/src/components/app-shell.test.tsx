// @vitest-environment jsdom

import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AppShell } from "./app-shell";
import { WorkspaceProvider } from "./workspace-provider";

vi.mock("next/navigation", () => ({ usePathname: () => "/" }));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
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
