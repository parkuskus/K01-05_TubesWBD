"use client";

import Link from "next/link";
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type ComponentProps, type ReactNode } from "react";
import { useAuth } from "@/components/auth-provider";
import { Button, buttonStyles } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { EmptyState } from "@/components/ui/primitives";
import { ApiError } from "@/lib/api/client";

export function useResourceRefresh() {
  const client = useQueryClient();
  const { refreshSession } = useAuth();
  return async () => {
    await client.invalidateQueries();
    await refreshSession();
  };
}

export function AdminOnly({ children }: { children: ReactNode }) {
  const { user } = useAuth();
  if (!user?.is_platform_admin) {
    return (
      <EmptyState
        title="Akses ditolak"
        description="Halaman ini hanya tersedia untuk administrator platform."
        action={
          <Link className={buttonStyles({ variant: "secondary" })} href="/">
            Kembali ke database
          </Link>
        }
      />
    );
  }
  return children;
}

export function ResourceError({ error, retry }: { error: unknown; retry: () => unknown }) {
  const status = error instanceof ApiError ? error.status : 0;
  return (
    <div role="alert">
      <EmptyState
        title={
          status === 403
            ? "Akses ditolak"
            : status === 404 || status === 501
              ? "Data atau layanan belum tersedia"
              : "Data belum dapat dimuat"
        }
        description={
          status === 403
            ? "Hak akses Anda tidak mengizinkan tindakan ini. Hubungi administrator bila diperlukan."
            : status === 404 || status === 501
              ? "Data yang diminta atau API fitur ini belum tersedia. Silakan kembali atau coba lagi nanti."
              : "Periksa koneksi Anda dan coba lagi."
        }
        action={
          status !== 403 ? (
            <Button variant="secondary" onClick={() => void retry()}>
              Coba lagi
            </Button>
          ) : undefined
        }
      />
    </div>
  );
}

export function mutationMessage(error: unknown) {
  if (error instanceof ApiError) {
    if (["APPROVAL_STALE", "STALE_APPROVAL"].includes(error.code))
      return "Approval kedaluwarsa karena revisi atau kebijakan berubah. Muat ulang dan ajukan review baru.";
    if (["HEAD_MISMATCH", "EXPECTED_HEAD_CONFLICT"].includes(error.code))
      return "Posisi migration aktif berubah. Muat ulang dan periksa urutan migration.";
    if (["REVISION_CONFLICT", "IMMUTABLE_MIGRATION"].includes(error.code))
      return "Revisi berubah atau SQL sudah pernah dieksekusi. Periksa revisi terbaru; perbaikan SQL memerlukan migration baru.";
    if (error.code === "IRREVERSIBLE_MIGRATION")
      return "Migration irreversible tidak dapat di-rollback atau dikompensasi.";
    if (error.code === "EXECUTION_GATE_BLOCKED")
      return "Database tertahan oleh eksekusi yang belum terselesaikan. Periksa job dan pemulihannya.";
    if (error.code === "UNSAFE_BATCH_COMPENSATION")
      return "Kompensasi tidak aman karena data berubah. Buat migration perbaikan baru dan ajukan review.";
    if (error.code === "LAST_ADMIN")
      return "Administrator terakhir tidak dapat dihapus atau diturunkan hak aksesnya.";
    if (error.code === "LEADER_REQUIRED" || error.code === "LEADER_REMOVAL")
      return "Tetapkan leader pengganti sebelum menghapus anggota ini.";
    if (error.code === "LEADER_NOT_MEMBER")
      return "Leader harus merupakan anggota grup. Muat ulang daftar anggota.";
    if (error.code === "CSRF_INVALID" || error.code === "CSRF_NOT_READY")
      return "Sesi formulir berakhir. Muat ulang halaman dan coba lagi.";
    if (error.status === 403) return "Akses ditolak. Hak akses Anda mungkin telah berubah.";
    if (error.status === 404 || error.status === 501)
      return "Data atau API fitur ini belum tersedia. Perubahan belum disimpan.";
    if (error.status === 409)
      return "Perubahan ditolak karena data berubah atau masih digunakan. Muat ulang data dan periksa kembali.";
  }
  return "Perubahan belum dapat disimpan. Periksa isian dan coba lagi.";
}

export function MutationDialog({
  trigger,
  title,
  description,
  submitLabel = "Simpan",
  danger = false,
  onSubmit,
  children,
}: {
  trigger: ReactNode;
  title: string;
  description: string;
  submitLabel?: string;
  danger?: boolean;
  onSubmit: (values: FormData) => Promise<unknown>;
  children?: (errors: Readonly<Record<string, string>>) => ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const [fields, setFields] = useState<Record<string, string>>({});
  const form = useRef<HTMLFormElement>(null);

  useEffect(() => {
    if (pending) return;
    const first = Array.from(form.current?.elements ?? []).find(
      (element) => element.getAttribute("aria-invalid") === "true",
    );
    if (first instanceof HTMLElement) first.focus();
  }, [pending, fields]);

  return (
    <Dialog
      trigger={trigger}
      title={title}
      description={description}
      open={open}
      onOpenChange={(next) => {
        if (pending) return;
        setOpen(next);
        setError("");
        setFields({});
      }}
    >
      <form
        ref={form}
        className="space-y-5"
        onSubmit={async (event) => {
          event.preventDefault();
          if (pending) return;
          const values = new FormData(event.currentTarget);
          setPending(true);
          setError("");
          setFields({});
          try {
            await onSubmit(values);
            setOpen(false);
          } catch (failure) {
            const nextFields: Record<string, string> = {};
            if (failure instanceof ApiError && [400, 422].includes(failure.status)) {
              for (const element of Array.from(form.current?.elements ?? [])) {
                const name = element.getAttribute("name");
                if (name && failure.fieldErrors[name]) nextFields[name] = failure.fieldErrors[name];
              }
            }
            setFields(nextFields);
            setError(mutationMessage(failure));
          } finally {
            setPending(false);
          }
        }}
      >
        <fieldset disabled={pending} className="min-w-0 space-y-5 disabled:opacity-60">
          {children?.(fields)}
        </fieldset>
        {error && (
          <p role="alert" className="text-sm leading-6 text-danger">
            {error}
          </p>
        )}
        <div className="flex flex-wrap justify-end gap-2 border-t border-line-soft pt-5">
          <Button variant="secondary" disabled={pending} onClick={() => setOpen(false)}>
            Batal
          </Button>
          <Button type="submit" variant={danger ? "danger" : "primary"} disabled={pending}>
            {pending ? "Memproses…" : submitLabel}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

export function SelectField({
  id,
  label,
  error,
  hint,
  children,
  ...props
}: ComponentProps<"select"> & { id: string; label: string; error?: string; hint?: string }) {
  return (
    <div className="grid min-w-0 gap-2">
      <label className="text-sm font-medium" htmlFor={id}>
        {label}
      </label>
      <select
        {...props}
        id={id}
        aria-invalid={error ? true : undefined}
        aria-describedby={error || hint ? `${id}-description` : undefined}
        className="min-h-11 w-full min-w-0 rounded-md border border-control-border bg-surface px-3 text-base sm:text-sm"
      >
        {children}
      </select>
      {(error || hint) && (
        <p
          id={`${id}-description`}
          className={
            error ? "text-xs leading-5 text-danger" : "text-xs leading-5 text-ink-secondary"
          }
        >
          {error || hint}
        </p>
      )}
    </div>
  );
}

export function validateText(value: FormDataEntryValue | null, field: string, maxBytes: number) {
  const text = String(value ?? "").trim();
  if (!text || new TextEncoder().encode(text).length > maxBytes) {
    throw new ApiError("Input tidak valid.", 422, "VALIDATION_FAILED", undefined, {
      [field]: `Isi nilai yang valid, maksimal ${maxBytes} byte.`,
    });
  }
  return text;
}

export function validatePassword(value: FormDataEntryValue | null) {
  const password = String(value ?? "");
  if (!password || new TextEncoder().encode(password).length > 72) {
    throw new ApiError("Password tidak valid.", 422, "VALIDATION_FAILED", undefined, {
      password: "Isi password, maksimal 72 byte.",
    });
  }
  return password;
}
