"use client";

import Link from "next/link";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/components/auth-provider";
import { MigrationFrame } from "@/components/migration-ui";
import { ResourceError } from "@/components/resource-ui";
import { Button } from "@/components/ui/button";
import { EmptyState, LoadingState, SqlField } from "@/components/ui/primitives";
import { DetailRow } from "@/components/workspace-ui";
import { migrationApi } from "@/lib/api/migrations";
import { resourceID } from "@/lib/api/resources";
import { directionLabels, formatTime } from "@/lib/migrations";

export function HistoryWorkspace({ id }: { id: string }) {
  return (
    <MigrationFrame id={id} section="history">
      {() => <HistoryList id={id} />}
    </MigrationFrame>
  );
}

function HistoryList({ id }: { id: string }) {
  const { api } = useAuth();
  const params = useSearchParams();
  const router = useRouter();
  const pathname = usePathname();
  const hide = params.get("hide_rollback") === "true";
  const cursor = params.get("cursor") ?? "";
  const history = useQuery({
    queryKey: ["history", id, hide, cursor],
    queryFn: ({ signal }) => migrationApi(api).history(id, hide, cursor, signal),
  });
  function navigate(hidden: boolean, next = "") {
    router.push(
      `${pathname}?${new URLSearchParams({ hide_rollback: String(hidden), ...(next ? { cursor: next } : {}) })}`,
      { scroll: false },
    );
  }
  return (
    <div className="min-w-0 space-y-5">
      <h2 className="text-lg font-semibold">History committed</h2>
      <p className="text-sm leading-6 text-ink-secondary">
        Riwayat eksekusi yang berhasil di-commit. Hide rollback menyembunyikan pasangan berdasarkan
        execution ID tanpa menghapus history. Attempt gagal tetap tersedia pada job.
      </p>
      <label className="flex min-h-11 items-center gap-3 text-sm">
        <input
          type="checkbox"
          className="size-4 accent-accent"
          checked={hide}
          onChange={(event) => navigate(event.target.checked)}
        />
        Hide rollback
      </label>
      {history.isPending ? (
        <LoadingState />
      ) : history.isError ? (
        <ResourceError error={history.error} retry={history.refetch} />
      ) : (
        <>
          {!history.data.events.length ? (
            <EmptyState
              title="Belum ada history yang ditampilkan"
              description="Eksekusi committed akan muncul di sini sesuai filter yang dipilih."
            />
          ) : (
            <ol className="min-w-0 space-y-4">
              {history.data.events.map((event) => (
                <li key={event.execution_id} className="min-w-0">
                  <details className="min-w-0 rounded-md border border-line p-5">
                    <summary className="min-h-11 cursor-pointer text-sm leading-7 font-medium [overflow-wrap:anywhere]">
                      v{event.version} · {directionLabels[event.direction]} · {event.execution_id}
                      <time
                        dateTime={event.committed_at}
                        className="ml-3 font-normal text-ink-secondary"
                      >
                        {formatTime(event.committed_at)}
                      </time>
                    </summary>
                    <dl className="divide-y divide-line-soft">
                      <DetailRow label="Migration ID">{event.migration_id}</DetailRow>
                      <DetailRow label="Execution ID">{event.execution_id}</DetailRow>
                      <DetailRow label="Revisi">{event.revision_id}</DetailRow>
                      <DetailRow label="Actor">{event.actor_id}</DetailRow>
                      <DetailRow label="Mode">{event.mode}</DetailRow>
                      {event.reverts_execution_id && (
                        <DetailRow label="Membatalkan execution">
                          {event.reverts_execution_id}
                        </DetailRow>
                      )}
                      <DetailRow label="Job">
                        <Link
                          href={`/jobs/${resourceID(event.job_id)}`}
                          className="inline-flex min-h-11 items-center text-accent"
                        >
                          {event.job_id}
                        </Link>
                      </DetailRow>
                    </dl>
                    <SqlField
                      id={`history-${event.execution_id}`}
                      label="SQL committed"
                      value={event.executed_sql}
                      readOnly
                    />
                  </details>
                </li>
              ))}
            </ol>
          )}
          <div className="flex flex-wrap gap-3">
            {cursor && (
              <Button variant="secondary" onClick={() => navigate(hide)}>
                Halaman pertama
              </Button>
            )}
            {history.data.next_cursor && (
              <Button variant="secondary" onClick={() => navigate(hide, history.data.next_cursor!)}>
                Halaman berikutnya
              </Button>
            )}
            <Button
              variant="ghost"
              disabled={history.isFetching}
              onClick={() => void history.refetch()}
            >
              Perbarui history
            </Button>
          </div>
        </>
      )}
      <Link
        href={`/activity?database=${resourceID(id)}`}
        className="inline-flex min-h-11 items-center text-sm text-accent"
      >
        Lihat review, job, dan attempt gagal
      </Link>
    </div>
  );
}
