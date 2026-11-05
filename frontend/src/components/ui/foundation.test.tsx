// @vitest-environment jsdom

import * as DialogPrimitive from "@radix-ui/react-dialog";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ConnectionForm } from "@/components/connection-form";
import { Dialog } from "./dialog";
import { Button } from "./button";

afterEach(cleanup);

it("focuses an invalid connection field, submits valid input, and clears credentials after saving", async () => {
  const user = userEvent.setup();
  const onSubmit = vi.fn().mockResolvedValue(undefined);
  const onSaved = vi.fn();
  render(<ConnectionForm onSubmit={onSubmit} onSaved={onSaved} />);
  const name = screen.getByRole("textbox", { name: "Nama koneksi" });
  await user.click(screen.getByRole("button", { name: "Simpan koneksi" }));
  expect(name.getAttribute("aria-invalid")).toBe("true");
  expect(document.activeElement).toBe(name);
  expect(name.getAttribute("aria-describedby")).toBe("connection-name-description");
  await user.type(name, "Inventory");
  await user.type(screen.getByRole("textbox", { name: "Host" }), "localhost");
  await user.type(screen.getByRole("textbox", { name: "Database" }), "inventory");
  await user.type(screen.getByRole("textbox", { name: "Username" }), "developer");
  const password = screen.getByLabelText("Password") as HTMLInputElement;
  await user.type(password, "local-test-value");
  await user.click(screen.getByRole("button", { name: "Simpan koneksi" }));
  await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
  expect(onSubmit).toHaveBeenCalledWith({
    name: "Inventory",
    host: "localhost",
    port: 5432,
    database: "inventory",
    username: "developer",
    password: "local-test-value",
    sslmode: "require",
  });
  expect(password.value).toBe("");
  expect(name.hasAttribute("aria-invalid")).toBe(false);
});

it("does not offer a successful save without a connection adapter", () => {
  render(<ConnectionForm />);
  expect(
    (screen.getByRole("button", { name: "Simpan koneksi" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  expect(screen.getByText(/Layanan koneksi belum terhubung/)).toBeTruthy();
});

it("closes a dialog with Escape and restores focus to its trigger", async () => {
  const user = userEvent.setup();
  render(
    <Dialog
      trigger={<Button>Buka contoh</Button>}
      title="Dialog contoh"
      description="Tidak ada perubahan data."
    />,
  );
  const trigger = screen.getByRole("button", { name: "Buka contoh" });
  await user.click(trigger);
  expect(screen.getByRole("dialog", { name: "Dialog contoh" })).toBeTruthy();
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(document.activeElement).toBe(trigger);
});

it("closes the shared dialog when its navigation link is selected", async () => {
  const user = userEvent.setup();
  render(
    <Dialog
      trigger={<Button>Buka contoh</Button>}
      title="Navigasi contoh"
      description="Menuju komponen."
    >
      <DialogPrimitive.Close asChild>
        <a href="#components">Jelajahi komponen</a>
      </DialogPrimitive.Close>
    </Dialog>,
  );
  await user.click(screen.getByRole("button", { name: "Buka contoh" }));
  await user.click(screen.getByRole("link", { name: "Jelajahi komponen" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});
