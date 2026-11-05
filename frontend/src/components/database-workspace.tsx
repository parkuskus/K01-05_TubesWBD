"use client";

import Link from "next/link";
import { useState } from "react";
import { ArrowRight, Database } from "lucide-react";
import { AddConnection } from "@/components/connection-form";
import { ActivityTable } from "@/components/activity-workspace";
import { useWorkspace } from "@/components/workspace-provider";
import {
  DetailRow,
  FilterSelect,
  PageHeading,
  SearchInput,
  TableViewport,
} from "@/components/workspace-ui";
import { Badge, EmptyState } from "@/components/ui/primitives";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import type { DatabaseSummary } from "@/lib/workspace/types";
import { cx } from "@/lib/utils";

function DatabaseDetails({ database }: { database: DatabaseSummary }) {
  return (
    <Dialog
      title={database.name}
      description="Detail koneksi PostgreSQL."
      trigger={
        <button className="flex min-h-11 items-center gap-3 text-left text-ink hover:text-accent">
          <Database
            className="size-4 shrink-0 text-ink-secondary"
            strokeWidth={1.7}
            aria-hidden="true"
          />
          <span className="text-sm font-medium [overflow-wrap:anywhere]">{database.name}</span>
        </button>
      }
    >
      <dl className="divide-y divide-line-soft">
        <DetailRow label="Host">
          <span className="font-mono text-xs">
            {database.host}:{database.port}
          </span>
        </DetailRow>
        <DetailRow label="Database">
          <span className="font-mono text-xs">{database.database}</span>
        </DetailRow>
        <DetailRow label="Lingkungan">{database.environment}</DetailRow>
        <DetailRow label="Akses">{database.access}</DetailRow>
        <DetailRow label="Struktur">
          {database.schemas} schema · {database.tables} tabel
        </DetailRow>
      </dl>
    </Dialog>
  );
}

export function DatabaseWorkspace() {
  const { databases, activity, source } = useWorkspace();
  const [search, setSearch] = useState("");
  const [environment, setEnvironment] = useState("all");
  const [access, setAccess] = useState("all");
  const editable = databases.filter((database) => database.access === "View + Edit").length;
  const filtered = databases.filter(
    (database) =>
      `${database.name} ${database.host} ${database.database}`
        .toLowerCase()
        .includes(search.trim().toLowerCase()) &&
      (environment === "all" || database.environment === environment) &&
      (access === "all" || database.access === "View + Edit"),
  );

  return (
    <div className="space-y-12">
      <section aria-label="Database workspace" className="min-w-0">
        <PageHeading
          title="Database"
          description="Kelola koneksi PostgreSQL dalam workspace Anda."
          action={<AddConnection />}
        />
        <div
          className="mb-5 flex flex-wrap gap-x-6 gap-y-2 border-b border-line-soft"
          role="group"
          aria-label="Filter akses database"
        >
          {[
            { id: "all", label: "Semua database", count: databases.length },
            { id: "edit", label: "Dapat diedit", count: editable },
          ].map((tab) => (
            <button
              key={tab.id}
              onClick={() => setAccess(tab.id)}
              aria-pressed={access === tab.id}
              className={cx(
                "-mb-px flex min-h-12 items-center gap-2 border-b-2 px-0.5 text-sm",
                access === tab.id
                  ? "border-accent font-medium text-accent"
                  : "border-transparent text-ink-secondary hover:text-ink",
              )}
            >
              {tab.label}
              <span className="rounded bg-ground px-1.5 py-0.5 text-xs text-ink-secondary tabular-nums">
                {tab.count}
              </span>
            </button>
          ))}
        </div>
        <div className="mb-5 flex flex-wrap justify-between gap-3">
          <SearchInput
            id="database-search"
            label="Cari database"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            placeholder="Cari database…"
            className="flex-1 basis-52 sm:max-w-80"
          />
          <FilterSelect
            id="environment-filter"
            label="Lingkungan database"
            value={environment}
            onChange={(event) => setEnvironment(event.target.value)}
          >
            <option value="all">Semua lingkungan</option>
            <option value="Development">Development</option>
            <option value="Staging">Staging</option>
            <option value="Production">Production</option>
          </FilterSelect>
        </div>
        <TableViewport label="Daftar database">
          <table className="data-table table-fixed sm:min-w-[660px] sm:table-auto">
            <caption className="sr-only">Koneksi database dalam workspace</caption>
            <thead>
              <tr>
                <th scope="col" className="w-3/5 sm:w-auto">
                  Nama database
                </th>
                <th scope="col" className="hidden sm:table-cell">
                  Lingkungan
                </th>
                <th scope="col" className="hidden sm:table-cell">
                  Struktur
                </th>
                <th scope="col">Akses</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((database) => (
                <tr key={database.id}>
                  <th scope="row" className="font-normal">
                    <DatabaseDetails database={database} />
                    <p className="pl-7 text-xs text-ink-secondary">
                      {database.host}:{database.port}
                    </p>
                  </th>
                  <td className="hidden sm:table-cell">
                    <Badge>{database.environment}</Badge>
                  </td>
                  <td className="hidden text-ink-secondary sm:table-cell">
                    {database.schemas} schema <span className="px-1 text-line">/</span>{" "}
                    {database.tables} tabel
                  </td>
                  <td className="text-ink-secondary">{database.access}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {!filtered.length && (
            <EmptyState
              title={
                source === "disconnected"
                  ? "Layanan belum terhubung"
                  : databases.length
                    ? "Database tidak ditemukan"
                    : "Belum ada database"
              }
              description={
                source === "disconnected"
                  ? "Daftar database akan tersedia setelah layanan terhubung."
                  : databases.length
                    ? "Coba kata kunci atau filter lain."
                    : "Tambahkan koneksi PostgreSQL untuk mulai bekerja."
              }
              action={
                databases.length ? (
                  <Button
                    variant="secondary"
                    onClick={() => {
                      setSearch("");
                      setEnvironment("all");
                      setAccess("all");
                    }}
                  >
                    Reset filter
                  </Button>
                ) : undefined
              }
            />
          )}
        </TableViewport>
        <p className="mt-4 text-xs text-ink-secondary" role="status">
          Menampilkan {filtered.length} dari {databases.length} database
        </p>
      </section>
      <section aria-labelledby="recent-activity-title" className="min-w-0">
        <div className="mb-5 flex items-center justify-between gap-4">
          <h2 id="recent-activity-title" className="text-base font-semibold tracking-tight">
            Aktivitas terbaru
          </h2>
          <Link
            href="/activity"
            className="flex min-h-11 items-center gap-2 text-sm text-ink-secondary hover:text-accent"
          >
            Lihat semua
            <ArrowRight className="size-3.5" aria-hidden="true" />
          </Link>
        </div>
        <ActivityTable items={activity.slice(0, 4)} compact />
      </section>
    </div>
  );
}
