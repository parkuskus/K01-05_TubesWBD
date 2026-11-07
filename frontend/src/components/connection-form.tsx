"use client";

import { useEffect, useRef, useState, type FormEvent } from "react";
import { LoaderCircle, Plus } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { TextField } from "@/components/ui/primitives";
import { ApiError } from "@/lib/api/client";
import type { Connection, ConnectionInput } from "@/lib/api/databases";
import { mutationMessage } from "@/components/resource-ui";

const connectionFields = [
  "name",
  "host",
  "port",
  "database",
  "username",
  "password",
  "sslmode",
] as const;
const sslModes = [
  "require",
  "disable",
  "verify-full",
] as const satisfies readonly ConnectionInput["sslmode"][];

type ConnectionFormProps = {
  initial?: Connection;
  onSubmit?: (input: ConnectionInput) => Promise<void>;
  onSaved?: () => void;
  onCancel?: () => void;
  onPendingChange?: (pending: boolean) => void;
};

export function ConnectionForm({
  initial,
  onSubmit,
  onSaved,
  onCancel,
  onPendingChange,
}: ConnectionFormProps) {
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [submitError, setSubmitError] = useState("");
  const [pending, setPending] = useState(false);
  const formRef = useRef<HTMLFormElement>(null);

  useEffect(() => {
    // Server validation arrives while the fieldset is disabled. Focus only
    // after React has re-enabled the fields following the failed request.
    if (pending) return;
    const field = connectionFields.find((name) => errors[name]);
    if (field) formRef.current?.querySelector<HTMLElement>(`[name="${field}"]`)?.focus();
  }, [errors, pending]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (pending || !onSubmit) return;
    const values = new FormData(event.currentTarget);
    const sslmode = sslModes.find((mode) => mode === values.get("sslmode"));
    const input = {
      name: String(values.get("name") ?? "").trim(),
      host: String(values.get("host") ?? "").trim(),
      port: Number(values.get("port")),
      database: String(values.get("database") ?? "").trim(),
      username: String(values.get("username") ?? "").trim(),
      password: String(values.get("password") ?? ""),
    };
    const nextErrors: Record<string, string> = {};
    if (!input.name) nextErrors.name = "Masukkan nama koneksi.";
    if (!input.host) nextErrors.host = "Masukkan host database.";
    if (!Number.isInteger(input.port) || input.port < 1 || input.port > 65535)
      nextErrors.port = "Port harus berada di antara 1–65535.";
    if (!input.database) nextErrors.database = "Masukkan nama database.";
    if (!input.username) nextErrors.username = "Masukkan username.";
    if (!input.password && !initial) nextErrors.password = "Masukkan password PostgreSQL.";
    else if (
      new TextEncoder().encode(input.password).length > 4096 ||
      input.password.includes("\0")
    )
      nextErrors.password = "Isi password yang valid, maksimal 4096 byte, tanpa karakter NUL.";
    if (!sslmode) nextErrors.sslmode = "Pilih salah satu pengaturan TLS yang tersedia.";
    setErrors(nextErrors);
    setSubmitError("");
    if (Object.keys(nextErrors).length || !sslmode) return;
    setPending(true);
    onPendingChange?.(true);
    try {
      const { password, ...metadata } = input;
      await onSubmit({ ...metadata, ...(password ? { password } : {}), sslmode });
      formRef.current?.reset();
      onSaved?.();
    } catch (error) {
      const fieldErrors: Record<string, string> = {};
      if (error instanceof ApiError && (error.status === 400 || error.status === 422)) {
        for (const field of connectionFields) {
          const message = error.fieldErrors[field];
          if (typeof message === "string" && message) fieldErrors[field] = message;
        }
      }
      if (Object.keys(fieldErrors).length) {
        // The API contract supplies sanitized validation messages for known fields.
        setErrors(fieldErrors);
      } else {
        // Raw connection errors may contain a DSN and must never be echoed.
        setSubmitError(
          error instanceof ApiError && [403, 404, 409, 501].includes(error.status)
            ? mutationMessage(error)
            : "Koneksi belum dapat disimpan. Periksa isian dan coba lagi.",
        );
      }
    } finally {
      setPending(false);
      onPendingChange?.(false);
    }
  }

  return (
    <form ref={formRef} noValidate onSubmit={submit} className="space-y-5">
      <fieldset disabled={pending} className="min-w-0 space-y-5 disabled:opacity-60">
        <TextField
          id="connection-name"
          name="name"
          label="Nama koneksi"
          placeholder="Misalnya: Inventory"
          defaultValue={initial?.name}
          error={errors.name}
          required
          autoComplete="off"
        />
        <div className="grid grid-cols-[minmax(0,1fr)_96px] gap-4">
          <TextField
            id="connection-host"
            name="host"
            label="Host"
            placeholder="db.example.com"
            defaultValue={initial?.host}
            error={errors.host}
            required
            autoComplete="off"
            spellCheck={false}
          />
          <TextField
            id="connection-port"
            name="port"
            label="Port"
            type="number"
            defaultValue={initial?.port ?? 5432}
            min={1}
            max={65535}
            error={errors.port}
            required
          />
        </div>
        <TextField
          id="connection-database"
          name="database"
          label="Database"
          placeholder="inventory"
          defaultValue={initial?.database}
          error={errors.database}
          required
          autoComplete="off"
          spellCheck={false}
        />
        <div className="grid gap-4 sm:grid-cols-2">
          <TextField
            id="connection-username"
            name="username"
            label="Username"
            defaultValue={initial?.username}
            error={errors.username}
            required
            autoComplete="off"
            spellCheck={false}
          />
          <TextField
            id="connection-password"
            name="password"
            label="Password"
            type="password"
            error={errors.password}
            required={!initial}
            hint={initial ? "Kosongkan untuk mempertahankan password tersimpan." : undefined}
            maxLength={4096}
            autoComplete="new-password"
          />
        </div>
        <div className="grid gap-2">
          <label htmlFor="connection-sslmode" className="text-sm font-medium">
            SSL
          </label>
          <select
            id="connection-sslmode"
            name="sslmode"
            defaultValue={initial?.sslmode ?? "require"}
            required
            aria-invalid={errors.sslmode ? true : undefined}
            aria-describedby={errors.sslmode ? "connection-sslmode-description" : undefined}
            className="min-h-11 rounded-md border border-control-border bg-surface px-3 text-base sm:text-sm"
          >
            <option value="require">Require</option>
            <option value="verify-full">Verify full — verifikasi sertifikat dan hostname</option>
            <option value="disable">Disable — koneksi lokal</option>
          </select>
          {errors.sslmode && (
            <p id="connection-sslmode-description" className="text-xs leading-relaxed text-danger">
              {errors.sslmode}
            </p>
          )}
        </div>
      </fieldset>
      {!onSubmit && (
        <p className="rounded-md bg-ground px-3 py-2.5 text-xs leading-5 text-ink-secondary">
          Layanan koneksi belum terhubung. Penyimpanan akan tersedia setelah layanan aktif.
        </p>
      )}
      {submitError && (
        <p role="alert" className="text-sm leading-6 text-danger">
          {submitError}
        </p>
      )}
      <div className="flex flex-wrap justify-end gap-2 border-t border-line-soft pt-5">
        {onCancel && (
          <Button variant="secondary" disabled={pending} onClick={onCancel}>
            Batal
          </Button>
        )}
        <Button type="submit" disabled={pending || !onSubmit}>
          {pending && (
            <LoaderCircle className="animate-spin motion-reduce:animate-none" aria-hidden="true" />
          )}
          {pending ? "Menyimpan…" : "Simpan koneksi"}
        </Button>
      </div>
    </form>
  );
}

export function AddConnection({ onSubmit }: ConnectionFormProps) {
  const [open, setOpen] = useState(false);
  const [pending, setPending] = useState(false);
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!pending) setOpen(next);
      }}
      title="Tambah database"
      description="Simpan koneksi PostgreSQL untuk mengakses schema dan mengelola migration."
      trigger={
        <Button>
          <Plus aria-hidden="true" />
          Tambah database
        </Button>
      }
    >
      <ConnectionForm
        onSubmit={onSubmit}
        onPendingChange={setPending}
        onSaved={() => setOpen(false)}
        onCancel={() => setOpen(false)}
      />
    </Dialog>
  );
}
