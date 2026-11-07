"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/components/auth-provider";
import { MutationDialog, ResourceError } from "@/components/resource-ui";
import { Button, buttonStyles } from "@/components/ui/button";
import {
  Badge,
  EmptyState,
  LoadingState,
  ProgressBar,
  SqlField,
  StatusBadge,
} from "@/components/ui/primitives";
import { DetailRow, PageHeading } from "@/components/workspace-ui";
import { migrationApi, type Job } from "@/lib/api/migrations";
import { ApiError } from "@/lib/api/client";
import { resourceID } from "@/lib/api/resources";
import { directionLabels, formatTime, pollInterval } from "@/lib/migrations";

export function JobWorkspace({ id }: { id: string }) {
  const { api } = useAuth();
  const migrations = migrationApi(api);
  const job = useQuery({
    queryKey: ["job", id],
    queryFn: ({ signal }) => migrations.job(id, signal),
    refetchInterval: (query) =>
      pollInterval(query.state.data?.status, query.state.error, query.state.fetchFailureCount),
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
  });
  if (job.isPending) return <LoadingState />;
  if (
    job.isError &&
    (!job.data || (job.error instanceof ApiError && [401, 403, 404].includes(job.error.status)))
  )
    return <ResourceError error={job.error} retry={job.refetch} />;
  const item = job.data!;
  const progress =
    item.total_scope_rows > 0
      ? Math.floor((item.committed_rows / item.total_scope_rows) * 100)
      : null;
  return (
    <section className="min-w-0 space-y-6">
      <PageHeading
        title={`Job ${item.job_id}`}
        description={`${directionLabels[item.direction]} · ${item.mode === "BATCH" ? "Per batch" : "Biasa"}`}
        action={<StatusBadge status={item.status} />}
      />
      {job.isError && <ResourceError error={job.error} retry={job.refetch} />}
      <div className="flex flex-wrap gap-3">
        <Link
          href={`/operation-requests/${resourceID(item.request_id)}`}
          className={buttonStyles({ variant: "secondary" })}
        >
          Lihat review
        </Link>
        <Link
          href={`/databases/${resourceID(item.database_id)}/history`}
          className={buttonStyles({ variant: "secondary" })}
        >
          History database
        </Link>
        <Button variant="ghost" disabled={job.isFetching} onClick={() => void job.refetch()}>
          Perbarui status
        </Button>
      </div>
      <dl className="divide-y divide-line-soft">
        <DetailRow label="Execution ID">{item.execution_id}</DetailRow>
        <DetailRow label="Database ID">{item.database_id}</DetailRow>
        <DetailRow label="Actor">{item.actor_id}</DetailRow>
        <DetailRow label="Dibuat">{formatTime(item.created_at)}</DetailRow>
        <DetailRow label="Diperbarui">{formatTime(item.updated_at)}</DetailRow>
        {item.completed_at && (
          <DetailRow label="Selesai">{formatTime(item.completed_at)}</DetailRow>
        )}
        <DetailRow label="Retry otomatis">
          {item.retry_count} / {item.max_retries}
        </DetailRow>
      </dl>
      {item.mode === "BATCH" && (
        <section
          aria-label="Progress committed"
          className="space-y-4 rounded-md border border-line p-5"
        >
          <h2 className="text-base font-semibold">Progress committed</h2>
          <p role="status" className="text-sm text-ink-secondary">
            {item.committed_batches.toLocaleString("id-ID")} batch ·{" "}
            {item.committed_rows.toLocaleString("id-ID")} row sudah committed
            {item.total_scope_rows > 0
              ? ` dari ${item.total_scope_rows.toLocaleString("id-ID")} row dalam cakupan`
              : " · cakupan belum tersedia"}
            .
          </p>
          {progress !== null && (
            <ProgressBar value={progress} label="Row batch yang sudah committed" />
          )}
          <p className="text-xs leading-5 [overflow-wrap:anywhere] text-ink-secondary">
            Cursor committed terakhir: {item.last_committed_cursor || "belum ada"}. Batch yang
            sedang diproses belum dihitung.
          </p>
        </section>
      )}
      {item.error_code && (
        <p role="alert" className="rounded-md bg-danger-surface p-4 text-sm leading-6 text-danger">
          {item.error_code} · {item.error_message || "Eksekusi memerlukan pemeriksaan."}
        </p>
      )}
      {item.error_code === "UNSAFE_BATCH_COMPENSATION" && (
        <p className="rounded-md bg-warning-surface p-4 text-sm leading-6 text-warning">
          Kompensasi tidak aman karena data telah berubah. Buat migration perbaikan baru dan ajukan
          review; periksa pemulihan database bersama pengelola.
        </p>
      )}
      <JobRecovery
        item={item}
        disabled={job.isError || job.isFetching || item.error_code === "UNSAFE_BATCH_COMPENSATION"}
      />
      <JobAttempts id={id} status={item.status} />
    </section>
  );
}

function JobRecovery({ item, disabled }: { item: Job; disabled: boolean }) {
  const { api } = useAuth();
  const router = useRouter();
  const unresolved = ["PARTIAL", "PARTIALLY_REVERTED", "UNKNOWN", "BLOCKED"].includes(item.status);
  if (!unresolved && !item.capabilities.request_resume && !item.capabilities.request_compensation)
    return null;
  return (
    <section className="space-y-4 rounded-md border border-line p-5">
      <h2 className="text-base font-semibold">Pemulihan eksekusi</h2>
      <p className="text-sm leading-6 text-ink-secondary">
        {item.status === "UNKNOWN"
          ? "Hasil commit belum dapat dipastikan. Database ditahan sampai worker memeriksa bukti eksekusi. Tunggu rekonsiliasi sebelum mengajukan tindakan lain."
          : item.status === "PARTIALLY_REVERTED"
            ? "Sebagian batch sudah dibatalkan. Lanjutkan kompensasi dari checkpoint; perubahan yang sudah dibatalkan tidak diulang."
            : item.status === "PARTIAL"
              ? "Sebagian batch sudah committed. Migration lain pada database ini ditahan sampai eksekusi dilanjutkan atau dikompensasi."
              : "Periksa penyebab job tertahan atau gagal dan tindakan pemulihan yang tersedia."}
      </p>
      {item.status !== "UNKNOWN" && (
        <div className="flex flex-wrap gap-3">
          {(
            [
              {
                direction: "RESUME",
                allowed: item.capabilities.request_resume && item.status !== "PARTIALLY_REVERTED",
                label: "Ajukan lanjutkan",
                description:
                  "Lanjutkan execution dan cakupan yang sama dari checkpoint terakhir. Batch committed tidak dijalankan ulang.",
              },
              {
                direction: "COMPENSATE",
                allowed: item.capabilities.request_compensation,
                label:
                  item.status === "PARTIALLY_REVERTED"
                    ? "Ajukan lanjutkan kompensasi"
                    : "Ajukan kompensasi",
                description:
                  "Gunakan SQL down asli dalam urutan batch terbalik. Kompensasi yang akan menimpa perubahan baru harus ditolak oleh server.",
              },
            ] as const
          )
            .filter((action) => action.allowed)
            .map((action) => (
              <MutationDialog
                key={action.direction}
                title={action.label}
                description={`${action.description} Tindakan memerlukan validasi, approval seluruh leader, dan job baru.`}
                trigger={
                  <Button variant="secondary" disabled={disabled}>
                    {action.label}
                  </Button>
                }
                submitLabel="Ajukan review pemulihan"
                onSubmit={async () => {
                  const request = await migrationApi(api).requestOperation(item.database_id, {
                    direction: action.direction,
                    origin_execution_id: item.execution_id,
                  });
                  if (!request.id)
                    throw new ApiError("Respons review tidak valid.", 200, "INVALID_RESPONSE");
                  router.push(`/operation-requests/${resourceID(request.id)}`);
                }}
              />
            ))}
        </div>
      )}
    </section>
  );
}

function JobAttempts({ id, status }: { id: string; status: Job["status"] }) {
  const { api } = useAuth();
  const attempts = useQuery({
    queryKey: ["job-attempts", id, status],
    queryFn: ({ signal }) => migrationApi(api).attempts(id, signal),
    refetchInterval: (query) =>
      pollInterval(status, query.state.error, query.state.fetchFailureCount),
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
  });
  return (
    <section className="min-w-0 space-y-4">
      <h2 className="text-base font-semibold">Attempt eksekusi</h2>
      <p className="text-sm leading-6 text-ink-secondary">
        Attempt gagal atau transaksi yang dibatalkan tetap terlihat di sini. History database hanya
        berisi eksekusi committed.
      </p>
      {attempts.isPending ? (
        <LoadingState />
      ) : attempts.isError ? (
        <ResourceError error={attempts.error} retry={attempts.refetch} />
      ) : !attempts.data.attempts.length ? (
        <EmptyState
          title="Belum ada attempt"
          description="Worker belum memulai eksekusi job ini."
        />
      ) : (
        attempts.data.attempts.map((attempt) => (
          <details key={attempt.attempt_id} className="min-w-0 rounded-md border border-line p-4">
            <summary className="min-h-11 cursor-pointer text-sm leading-7 [overflow-wrap:anywhere]">
              Attempt {attempt.attempt_no} ·{" "}
              <Badge tone={attempt.status === "COMMITTED" ? "success" : "warning"}>
                {attempt.status}
              </Badge>{" "}
              · {formatTime(attempt.started_at)}
            </summary>
            <dl className="divide-y divide-line-soft">
              <DetailRow label="Attempt ID">{attempt.attempt_id}</DetailRow>
              <DetailRow label="Execution ID">{attempt.execution_id}</DetailRow>
              {attempt.finished_at && (
                <DetailRow label="Selesai">{formatTime(attempt.finished_at)}</DetailRow>
              )}
            </dl>
            {attempt.error_code && (
              <p className="my-4 text-sm leading-6 text-danger">
                {attempt.error_code} · {attempt.error_message}
              </p>
            )}
            <SqlField
              id={`attempt-${attempt.attempt_id}`}
              label="SQL attempt"
              value={attempt.executed_sql}
              readOnly
            />
          </details>
        ))
      )}
    </section>
  );
}
