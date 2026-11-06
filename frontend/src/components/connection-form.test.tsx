// @vitest-environment jsdom

import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createApiClient } from "@/lib/api/client";
import { ConnectionForm } from "./connection-form";
import type { Connection } from "@/lib/api/databases";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

async function fillConnection(includePassword = true) {
  const user = userEvent.setup();
  await user.type(screen.getByRole("textbox", { name: "Nama koneksi" }), "Inventory");
  await user.type(screen.getByRole("textbox", { name: "Host" }), "localhost");
  await user.type(screen.getByRole("textbox", { name: "Database" }), "inventory");
  await user.type(screen.getByRole("textbox", { name: "Username" }), "developer");
  if (includePassword) await user.type(screen.getByLabelText("Password"), "test-password");
  return user;
}

it.each(["require", "disable", "verify-full"] as const)(
  "preserves the selected TLS mode %s when submitting a connection",
  async (sslmode) => {
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    render(<ConnectionForm onSubmit={onSubmit} />);
    const user = await fillConnection();
    await user.selectOptions(screen.getByRole("combobox", { name: "SSL" }), sslmode);
    await user.click(screen.getByRole("button", { name: "Simpan koneksi" }));
    expect(onSubmit).toHaveBeenCalledWith(expect.objectContaining({ sslmode }));
  },
);

it("rejects an unsupported TLS mode instead of silently replacing it", async () => {
  const onSubmit = vi.fn().mockResolvedValue(undefined);
  render(<ConnectionForm onSubmit={onSubmit} />);
  const user = await fillConnection();
  const sslmode = screen.getByRole("combobox", { name: "SSL" });
  const invalidOption = document.createElement("option");
  invalidOption.value = "unsupported";
  invalidOption.textContent = "Unsupported";
  sslmode.appendChild(invalidOption);
  await user.selectOptions(sslmode, "unsupported");
  await user.click(screen.getByRole("button", { name: "Simpan koneksi" }));
  expect(onSubmit).not.toHaveBeenCalled();
  expect(sslmode.getAttribute("aria-invalid")).toBe("true");
  expect(document.activeElement).toBe(sslmode);
});

it("requires a password for a new connection and focuses its validation error", async () => {
  const onSubmit = vi.fn().mockResolvedValue(undefined);
  render(<ConnectionForm onSubmit={onSubmit} />);
  const user = await fillConnection(false);
  await user.click(screen.getByRole("button", { name: "Simpan koneksi" }));
  const password = screen.getByLabelText("Password") as HTMLInputElement;
  expect(onSubmit).not.toHaveBeenCalled();
  expect(password.required).toBe(true);
  expect(password.getAttribute("aria-invalid")).toBe("true");
  expect(password.getAttribute("aria-describedby")).toBe("connection-password-description");
  expect(document.activeElement).toBe(password);
});

it.each([
  ["over 4096 UTF-8 bytes", "é".repeat(2049)],
  ["containing NUL", "before\u0000after"],
])("rejects a password %s before sending it", async (_description, value) => {
  const onSubmit = vi.fn().mockResolvedValue(undefined);
  render(<ConnectionForm onSubmit={onSubmit} />);
  const user = await fillConnection(false);
  const password = screen.getByLabelText("Password");
  fireEvent.change(password, { target: { value } });
  await user.click(screen.getByRole("button", { name: "Simpan koneksi" }));
  expect(onSubmit).not.toHaveBeenCalled();
  expect(password.getAttribute("aria-invalid")).toBe("true");
  expect(document.activeElement).toBe(password);
});

it("renders structured API validation at the matching fields without exposing raw errors", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      Response.json(
        {
          error: {
            code: "VALIDATION_FAILED",
            message: "postgres://developer:private-password@localhost/inventory",
            field_errors: {
              password: "Password belum valid.",
              host: "Gunakan satu hostname tanpa port.",
              unknown: "Field di luar form.",
            },
          },
        },
        { status: 422 },
      ),
    ),
  );
  const api = createApiClient();
  api.setCsrfToken("example-csrf");
  const onSaved = vi.fn();
  render(
    <ConnectionForm
      onSubmit={async (input) => {
        await api.request("/connections", { method: "POST", body: JSON.stringify(input) });
      }}
      onSaved={onSaved}
    />,
  );
  const user = await fillConnection();
  await user.click(screen.getByRole("button", { name: "Simpan koneksi" }));
  await screen.findByText("Password belum valid.");
  const host = screen.getByRole("textbox", { name: "Host" });
  expect(host.getAttribute("aria-invalid")).toBe("true");
  expect(host.getAttribute("aria-describedby")).toBe("connection-host-description");
  expect(screen.getByLabelText("Password").getAttribute("aria-invalid")).toBe("true");
  await waitFor(() => expect(document.activeElement).toBe(host));
  expect(screen.queryByText(/private-password/)).toBeNull();
  expect(screen.queryByText("Field di luar form.")).toBeNull();
  expect(onSaved).not.toHaveBeenCalled();
  expect(
    (screen.getByRole("button", { name: "Simpan koneksi" }) as HTMLButtonElement).disabled,
  ).toBe(false);
});

it("keeps unstructured connection failures generic", async () => {
  const onSubmit = vi
    .fn()
    .mockRejectedValue(new Error("postgres://developer:private-password@localhost/inventory"));
  render(<ConnectionForm onSubmit={onSubmit} />);
  const user = await fillConnection();
  await user.click(screen.getByRole("button", { name: "Simpan koneksi" }));
  expect((await screen.findByRole("alert")).textContent).toBe(
    "Koneksi belum dapat disimpan. Periksa isian dan coba lagi.",
  );
  expect(screen.queryByText(/private-password/)).toBeNull();
});

const connection: Connection = {
  id: "1",
  name: "Inventory",
  host: "localhost",
  port: 5432,
  database: "inventory",
  username: "developer",
  sslmode: "verify-full",
};

it("leaves the edit password empty and omits it to preserve the stored credential", async () => {
  const onSubmit = vi.fn().mockResolvedValue(undefined);
  render(<ConnectionForm initial={connection} onSubmit={onSubmit} />);
  const password = screen.getByLabelText("Password") as HTMLInputElement;
  expect(password.value).toBe("");
  expect(password.required).toBe(false);
  expect((screen.getByLabelText("Username") as HTMLInputElement).required).toBe(true);
  expect(screen.getByText("Kosongkan untuk mempertahankan password tersimpan.")).toBeTruthy();
  await userEvent.click(screen.getByRole("button", { name: "Simpan koneksi" }));
  expect(onSubmit).toHaveBeenCalledWith({
    name: "Inventory",
    host: "localhost",
    port: 5432,
    database: "inventory",
    username: "developer",
    sslmode: "verify-full",
  });
});

it("sends a replacement password only when the administrator enters one", async () => {
  const onSubmit = vi.fn().mockResolvedValue(undefined);
  render(<ConnectionForm initial={connection} onSubmit={onSubmit} />);
  const user = userEvent.setup();
  await user.type(screen.getByLabelText("Password"), "replacement");
  await user.click(screen.getByRole("button", { name: "Simpan koneksi" }));
  expect(onSubmit).toHaveBeenCalledWith(expect.objectContaining({ password: "replacement" }));
});
