"use client";

import { useState } from "react";
import { ArrowDownLeft, ArrowUpRight } from "lucide-react";
import { useWorkspace } from "@/components/workspace-provider";
import {
  DetailRow,
  FilterSelect,
  PageHeading,
  SearchInput,
  TableViewport,
} from "@/components/workspace-ui";
import { EmptyState, ProgressBar, StatusBadge } from "@/components/ui/primitives";
import { Dialog } from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import type { ActivityItem } from "@/lib/workspace/types";

const dateFormatter = new Intl.DateTimeFormat("id-ID", {
  day: "numeric",
  month: "short",
  timeZone: "Asia/Jakarta",
});
const timeFormatter = new Intl.DateTimeFormat("id-ID", {
  hour: "2-digit",
  minute: "2-digit",
  timeZone: "Asia/Jakarta",
});
const directionLabels = { APPLY: "Apply", ROLLBACK: "Rollback", FORWARD: "Forward" };

function ActivityDetails({ item }: { item: ActivityItem }) {
  return (
    <Dialog
      title={item.name}
      description="Detail eksekusi migration."
      trigger={
        <button className="min-h-11 text-left text-sm font-medium [overflow-wrap:anywhere] hover:text-accent">
          {item.name}
        </button>
      }
    >
      <dl className="divide-y divide-line-soft">
        <DetailRow label="Database">{item.target}</DetailRow>
        <DetailRow label="Operasi">{directionLabels[item.direction]}</DetailRow>
        <DetailRow label="Status">
          <StatusBadge status={item.status} />
        </DetailRow>
        <DetailRow label="Waktu">
          {dateFormatter.format(new Date(item.createdAt))},{" "}
          {timeFormatter.format(new Date(item.createdAt))} WIB
        </DetailRow>
        <DetailRow label="Job ID">
          <span className="font-mono text-xs">{item.id}</span>
        </DetailRow>
      </dl>
      {item.totalBatches !== undefined &&
        item.completedBatches !== undefined &&
        item.totalBatches > 0 && (
          <div className="mt-5 border-t border-line-soft pt-5">
            <div className="mb-3 flex justify-between gap-3 text-sm">
              <span>Progres batch</span>
              <span className="text-ink-secondary tabular-nums">
                {item.completedBatches} / {item.totalBatches}
              </span>
            </div>
            <ProgressBar
              value={Math.round((item.completedBatches / item.totalBatches) * 100)}
              label="Progres batch migration"
            />
            <p className="mt-3 text-xs leading-5 text-ink-secondary">
              Jumlah batch yang sudah committed.
            </p>
          </div>
        )}
      {item.error && (
        <p className="mt-5 rounded-md bg-danger-surface p-3 text-sm leading-6 text-danger">
          {item.error}
        </p>
      )}
    </Dialog>
  );
}

export function ActivityTable({
  items,
  compact = false,
  filtered = false,
  onReset,
}: {
  items: ActivityItem[];
  compact?: boolean;
  filtered?: boolean;
  onReset?: () => void;
}) {
  return (
    <TableViewport label="Daftar aktivitas migration">
      <table className="data-table table-fixed sm:min-w-[680px] sm:table-auto">
        <caption className="sr-only">Aktivitas eksekusi migration</caption>
        <thead>
          <tr>
            <th scope="col" className="w-3/5 sm:w-auto">
              Migration
            </th>
            <th scope="col" className="hidden sm:table-cell">
              Operasi
            </th>
            <th scope="col">Status</th>
            <th scope="col" className="hidden sm:table-cell">
              Waktu
            </th>
          </tr>
        </thead>
        <tbody>
          {items.map((item) => (
            <tr key={item.id}>
              <th scope="row" className="font-normal">
                <ActivityDetails item={item} />
                <p className="text-xs [overflow-wrap:anywhere] text-ink-secondary">{item.target}</p>
              </th>
              <td className="hidden sm:table-cell">
                <span className="inline-flex items-center gap-1.5 text-ink-secondary">
                  {item.direction === "ROLLBACK" ? (
                    <ArrowDownLeft className="size-3.5" aria-hidden="true" />
                  ) : (
                    <ArrowUpRight className="size-3.5" aria-hidden="true" />
                  )}
                  {directionLabels[item.direction]}
                </span>
              </td>
              <td>
                <StatusBadge status={item.status} />
                {item.status === "RUNNING" &&
                  item.completedBatches !== undefined &&
                  item.totalBatches !== undefined && (
                    <p className="mt-1 text-xs text-ink-secondary tabular-nums">
                      {item.completedBatches}/{item.totalBatches} batch
                    </p>
                  )}
              </td>
              <td className="hidden whitespace-nowrap text-ink-secondary sm:table-cell">
                <time dateTime={item.createdAt}>
                  {!compact && (
                    <span className="mr-2">{dateFormatter.format(new Date(item.createdAt))}</span>
                  )}
                  {timeFormatter.format(new Date(item.createdAt))}
                </time>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {!items.length && (
        <EmptyState
          title={filtered ? "Aktivitas tidak ditemukan" : "Belum ada aktivitas"}
          description={
            filtered
              ? "Coba kata kunci atau status lain."
              : "Eksekusi migration akan ditampilkan di sini."
          }
          action={
            filtered && onReset ? (
              <Button variant="secondary" onClick={onReset}>
                Reset filter
              </Button>
            ) : undefined
          }
        />
      )}
    </TableViewport>
  );
}

export function ActivityWorkspace() {
  const { activity } = useWorkspace();
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState("all");
  const filtered = activity.filter(
    (item) =>
      `${item.name} ${item.target}`.toLowerCase().includes(search.trim().toLowerCase()) &&
      (status === "all" || item.status === status),
  );
  return (
    <section className="min-w-0">
      <PageHeading
        title="Aktivitas"
        description="Status dan progres eksekusi migration di seluruh database."
      />
      <div className="mb-5 flex flex-wrap justify-between gap-3">
        <SearchInput
          id="activity-search"
          label="Cari aktivitas"
          placeholder="Cari migration atau database…"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          className="flex-1 basis-52 sm:max-w-80"
        />
        <FilterSelect
          id="activity-status"
          label="Status aktivitas"
          value={status}
          onChange={(event) => setStatus(event.target.value)}
        >
          <option value="all">Semua status</option>
          <option value="PENDING">Dalam antrean</option>
          <option value="RUNNING">Berjalan</option>
          <option value="SUCCESS">Berhasil</option>
          <option value="FAILED">Gagal</option>
          <option value="RETRY_WAIT">Menunggu retry</option>
          <option value="BLOCKED">Tertahan</option>
          <option value="PARTIAL">Sebagian selesai</option>
          <option value="PARTIALLY_REVERTED">Sebagian dibatalkan</option>
          <option value="UNKNOWN">Perlu pemeriksaan</option>
        </FilterSelect>
      </div>
      <ActivityTable
        items={filtered}
        filtered={activity.length > 0}
        onReset={() => {
          setSearch("");
          setStatus("all");
        }}
      />
      <p className="mt-4 text-xs text-ink-secondary" role="status">
        Menampilkan {filtered.length} dari {activity.length} aktivitas
      </p>
    </section>
  );
}
