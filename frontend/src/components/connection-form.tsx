"use client";

import { useRef, useState, type FormEvent } from "react";
import { LoaderCircle, Plus } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { TextField } from "@/components/ui/primitives";
import type { ConnectionInput } from "@/lib/workspace/types";

type ConnectionFormProps = {
  onSubmit?: (input: ConnectionInput) => Promise<void>;
  onSaved?: () => void;
  onCancel?: () => void;
};

export function ConnectionForm({ onSubmit, onSaved, onCancel }: ConnectionFormProps) {
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [submitError, setSubmitError] = useState("");
  const [pending, setPending] = useState(false);
  const formRef = useRef<HTMLFormElement>(null);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (pending || !onSubmit) return;
    const values = new FormData(event.currentTarget);
    const input: ConnectionInput = {
      name: String(values.get("name") ?? "").trim(),
      host: String(values.get("host") ?? "").trim(),
      port: Number(values.get("port")),
      database: String(values.get("database") ?? "").trim(),
      username: String(values.get("username") ?? "").trim(),
      password: String(values.get("password") ?? ""),
      sslmode: values.get("sslmode") === "disable" ? "disable" : "require",
    };
    const nextErrors: Record<string, string> = {};
    if (!input.name) nextErrors.name = "Masukkan nama koneksi.";
    if (!input.host) nextErrors.host = "Masukkan host database.";
    if (!Number.isInteger(input.port) || input.port < 1 || input.port > 65535)
      nextErrors.port = "Port harus berada di antara 1–65535.";
    if (!input.database) nextErrors.database = "Masukkan nama database.";
    if (!input.username) nextErrors.username = "Masukkan username.";
    setErrors(nextErrors);
    setSubmitError("");
    if (Object.keys(nextErrors).length) {
      formRef.current
        ?.querySelector<HTMLInputElement>(`[name="${Object.keys(nextErrors)[0]}"]`)
        ?.focus();
      return;
    }
    setPending(true);
    try {
      await onSubmit(input);
      formRef.current?.reset();
      onSaved?.();
    } catch {
      // Do not echo arbitrary backend errors: connection failures may contain a DSN.
      setSubmitError("Koneksi belum dapat disimpan. Periksa isian dan coba lagi.");
    } finally {
      setPending(false);
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
            defaultValue={5432}
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
            defaultValue="require"
            className="min-h-11 rounded-md border border-control-border bg-surface px-3 text-base sm:text-sm"
          >
            <option value="require">Require</option>
            <option value="disable">Disable — koneksi lokal</option>
          </select>
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
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
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
        onSaved={() => setOpen(false)}
        onCancel={() => setOpen(false)}
      />
    </Dialog>
  );
}
