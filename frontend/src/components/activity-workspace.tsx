"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useAuth } from "@/components/auth-provider";
import { OperationBadge } from "@/components/migration-ui";
import { ResourceError, SelectField } from "@/components/resource-ui";
import { buttonStyles } from "@/components/ui/button";
import { EmptyState, LoadingState } from "@/components/ui/primitives";
import { FilterSelect, PageHeading, SearchInput, TableViewport } from "@/components/workspace-ui";
import { databaseApi } from "@/lib/api/databases";
import { ApiError } from "@/lib/api/client";
import { migrationApi } from "@/lib/api/migrations";
import { resourceID } from "@/lib/api/resources";
import { directionLabels, formatTime, pollInterval } from "@/lib/migrations";

export function ActivityWorkspace() {
  const { api } = useAuth();
  const router = useRouter();
  const params = useSearchParams();
  const databases = useQuery({
    queryKey: ["databases"],
    queryFn: ({ signal }) => databaseApi(api).databases(signal),
  });
  const selected = params.get("database") ?? databases.data?.databases[0]?.id ?? "";
  const valid = databases.data?.databases.some((database) => database.id === selected) ?? false;
  return (
    <section className="min-w-0 space-y-6">
      <PageHeading
        title="Aktivitas"
        description="Tinjau pengajuan perubahan dan pantau job pada database yang dapat Anda akses."
      />
      {databases.isPending ? (
        <LoadingState />
      ) : databases.isError ? (
        <ResourceError error={databases.error} retry={databases.refetch} />
      ) : !databases.data.databases.length ? (
        <EmptyState
          title="Belum ada database yang dapat diakses"
          description="Minta administrator memberikan akses melalui grup Anda."
        />
      ) : (
        <>
          <SelectField
            id="activity-database"
            label="Database aktivitas"
            value={valid ? selected : ""}
            onChange={(event) =>
              router.push(`/activity?${new URLSearchParams({ database: event.target.value })}`, {
                scroll: false,
              })
            }
          >
            <option value="" disabled>
              Pilih database
            </option>
            {databases.data.databases.map((database) => (
              <option key={database.id} value={database.id}>
                {database.name}
              </option>
            ))}
          </SelectField>
          {valid ? (
            <OperationInbox key={selected} id={selected} />
          ) : (
            <EmptyState
              title="Database tidak tersedia"
              description="Pilih database yang dapat Anda akses."
            />
          )}
        </>
      )}
    </section>
  );
}

function OperationInbox({ id }: { id: string }) {
  const { api } = useAuth();
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState("all");
  const operations = useQuery({
    queryKey: ["operations", id],
    queryFn: ({ signal }) => migrationApi(api).operations(id, signal),
    refetchInterval: (query) =>
      pollInterval(undefined, query.state.error, query.state.fetchFailureCount),
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
  });
  if (operations.isPending) return <LoadingState />;
  if (
    operations.isError &&
    (!operations.data ||
      (operations.error instanceof ApiError && [401, 403, 404].includes(operations.error.status)))
  )
    return <ResourceError error={operations.error} retry={operations.refetch} />;
  const requests = operations.data?.requests ?? [];
  const filtered = requests.filter(
    (request) =>
      `${request.title} ${request.id}`.toLowerCase().includes(search.trim().toLowerCase()) &&
      (status === "all" || request.status === status),
  );
  return (
    <div className="min-w-0 space-y-5">
      {operations.isError && <ResourceError error={operations.error} retry={operations.refetch} />}
      <div className="flex flex-wrap gap-3">
        <SearchInput
          id="review-search"
          label="Cari pengajuan"
          placeholder="Cari migration atau request…"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          className="flex-1 basis-52 sm:max-w-80"
        />
        <FilterSelect
          id="review-status"
          label="Status pengajuan"
          value={status}
          onChange={(event) => setStatus(event.target.value)}
        >
          <option value="all">Semua status</option>
          <option value="PENDING_REVIEW">Menunggu review</option>
          <option value="APPROVED">Disetujui</option>
          <option value="QUEUED">Dalam antrean</option>
          <option value="STALE">Review kedaluwarsa</option>
          <option value="REJECTED">Ditolak</option>
          <option value="CANCELLED">Dibatalkan</option>
        </FilterSelect>
      </div>
      <TableViewport label="Daftar pengajuan migration">
        <table className="data-table min-w-[660px]">
          <caption className="sr-only">Review dan job migration</caption>
          <thead>
            <tr>
              <th scope="col">Migration</th>
              <th scope="col">Operasi</th>
              <th scope="col">Status review</th>
              <th scope="col">Waktu</th>
              <th scope="col">Job</th>
            </tr>
          </thead>
          <tbody>
            {filtered.map((request) => (
              <tr key={request.id}>
                <th scope="row" className="max-w-72 font-medium [overflow-wrap:anywhere]">
                  <Link
                    href={`/operation-requests/${resourceID(request.id)}`}
                    className="inline-flex min-h-11 items-center text-accent"
                  >
                    {request.title}
                  </Link>
                  <p className="font-mono text-xs text-ink-secondary">{request.id}</p>
                </th>
                <td>{directionLabels[request.direction]}</td>
                <td>
                  <OperationBadge status={request.status} />
                </td>
                <td>
                  <time dateTime={request.created_at}>{formatTime(request.created_at)}</time>
                </td>
                <td>
                  {request.job_id ? (
                    <Link
                      href={`/jobs/${resourceID(request.job_id)}`}
                      className="inline-flex min-h-11 items-center text-accent"
                    >
                      Pantau job
                    </Link>
                  ) : (
                    "Belum diantrekan"
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {!filtered.length && (
          <EmptyState
            title={requests.length ? "Pengajuan tidak ditemukan" : "Belum ada pengajuan"}
            description={
              requests.length
                ? "Coba kata kunci atau status lain."
                : "Pengajuan review, termasuk pemulihan, akan ditampilkan di sini."
            }
          />
        )}
      </TableViewport>
      <Link
        href={`/databases/${resourceID(id)}/migrations`}
        className={buttonStyles({ variant: "secondary" })}
      >
        Lihat migration
      </Link>
    </div>
  );
}
