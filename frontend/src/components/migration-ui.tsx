"use client";

import Link from "next/link";
import { useQuery } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { useAuth } from "@/components/auth-provider";
import { DatabaseHeading } from "@/components/database-viewer";
import { mutationMessage, ResourceError } from "@/components/resource-ui";
import { Button } from "@/components/ui/button";
import { Badge, LoadingState, SqlField } from "@/components/ui/primitives";
import { DetailRow } from "@/components/workspace-ui";
import { databaseApi, type DatabaseChoice } from "@/lib/api/databases";
import type { DraftInput, OperationStatus } from "@/lib/api/migrations";

export function MigrationFrame({
  id,
  section = "migrations",
  children,
}: {
  id: string;
  section?: "migrations" | "history";
  children: (database: DatabaseChoice) => ReactNode;
}) {
  const { api } = useAuth();
  const database = useQuery({
    queryKey: ["databases", id],
    queryFn: ({ signal }) => databaseApi(api).database(id, signal),
  });
  if (database.isPending) return <LoadingState />;
  if (database.isError)
    return (
      <>
        <Link href="/" className="text-sm text-accent">
          Kembali ke database
        </Link>
        <ResourceError error={database.error} retry={database.refetch} />
      </>
    );
  return (
    <section className="min-w-0">
      <DatabaseHeading database={database.data} section={section} />
      {children(database.data)}
    </section>
  );
}

export function OperationBadge({ status }: { status: OperationStatus }) {
  const labels: Record<OperationStatus, string> = {
    PENDING_REVIEW: "Menunggu review",
    APPROVED: "Disetujui",
    QUEUED: "Dalam antrean",
    REJECTED: "Ditolak",
    STALE: "Review kedaluwarsa",
    CANCELLED: "Dibatalkan",
  };
  return (
    <Badge tone={status === "APPROVED" ? "success" : status === "REJECTED" ? "danger" : "warning"}>
      {labels[status] ?? status}
    </Badge>
  );
}

export function ActionButton({
  children,
  action,
  disabled = false,
}: {
  children: ReactNode;
  action: () => Promise<unknown>;
  disabled?: boolean;
}) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  return (
    <div className="min-w-0 space-y-2">
      <Button
        variant="secondary"
        disabled={disabled || pending}
        onClick={async () => {
          if (pending) return;
          setPending(true);
          setError("");
          try {
            await action();
          } catch (failure) {
            setError(mutationMessage(failure));
          } finally {
            setPending(false);
          }
        }}
      >
        {pending ? "Memproses…" : children}
      </Button>
      {error && (
        <p role="alert" className="max-w-lg text-sm leading-6 text-danger">
          {error}
        </p>
      )}
    </div>
  );
}

export function PlanDetails({ plan }: { plan: DraftInput }) {
  return (
    <div className="min-w-0 space-y-5">
      <dl className="divide-y divide-line-soft">
        <DetailRow label="Mode">
          {plan.mode === "BATCH" ? "Per batch" : "Biasa · satu transaksi"}
        </DetailRow>
        <DetailRow label="Pemulihan">
          {plan.is_irreversible
            ? `Irreversible: ${plan.irreversible_reason}`
            : "Reversible · menggunakan SQL down asli"}
        </DetailRow>
        {plan.batch && (
          <>
            <DetailRow label="Target batch">
              {plan.batch.table_schema}.{plan.batch.table_name}
            </DetailRow>
            <DetailRow label="Kolom key">{plan.batch.primary_key_column}</DetailRow>
            <DetailRow label="Cakupan">
              {plan.batch.scope_where || "Seluruh row pada cakupan awal"}
            </DetailRow>
            <DetailRow label="Ukuran batch">
              {plan.batch.batch_size.toLocaleString("id-ID")} row
            </DetailRow>
          </>
        )}
      </dl>
      <SqlField id="plan-up" label="SQL up asli" value={plan.up_sql} readOnly />
      {!plan.is_irreversible && (
        <SqlField id="plan-down" label="SQL down asli" value={plan.down_sql} readOnly />
      )}
      <div className="rounded-md border border-line p-4">
        <h3 className="text-sm font-semibold">Catatan kompatibilitas</h3>
        <p className="mt-2 text-sm leading-6 [overflow-wrap:anywhere] whitespace-pre-wrap text-ink-secondary">
          {plan.compatibility_notes || "Belum ada catatan kompatibilitas."}
        </p>
      </div>
    </div>
  );
}
