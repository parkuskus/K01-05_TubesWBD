"use client";

import Link from "next/link";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useAuth } from "@/components/auth-provider";
import { ResourceError } from "@/components/resource-ui";
import { buttonStyles } from "@/components/ui/button";
import { Badge, EmptyState, LoadingState } from "@/components/ui/primitives";
import { FilterSelect, PageHeading, SearchInput, TableViewport } from "@/components/workspace-ui";
import { accessLabel, databaseApi } from "@/lib/api/databases";
import { resourceID } from "@/lib/api/resources";

export function DatabaseWorkspace() {
  const { api, user } = useAuth();
  const [search, setSearch] = useState("");
  const [access, setAccess] = useState("all");
  const databases = useQuery({
    queryKey: ["databases"],
    queryFn: ({ signal }) => databaseApi(api).databases(signal),
  });
  const filtered =
    databases.data?.databases.filter(
      (database) =>
        database.name.toLowerCase().includes(search.trim().toLowerCase()) &&
        (access === "all" || String(database.access_level) === access),
    ) ?? [];
  return (
    <section className="min-w-0">
      <PageHeading
        title="Database"
        description="Jelajahi schema dan data PostgreSQL yang dapat Anda akses."
        action={
          user?.is_platform_admin && (
            <Link href="/connections" className={buttonStyles({ variant: "secondary" })}>
              Kelola koneksi
            </Link>
          )
        }
      />
      {databases.isPending ? (
        <LoadingState />
      ) : databases.isError ? (
        <ResourceError error={databases.error} retry={databases.refetch} />
      ) : (
        <>
          <div className="mb-5 flex flex-wrap gap-3">
            <SearchInput
              id="database-search"
              label="Cari database"
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              placeholder="Cari database…"
              className="flex-1 basis-52 sm:max-w-80"
            />
            <FilterSelect
              id="database-access"
              label="Hak akses database"
              value={access}
              onChange={(event) => setAccess(event.target.value)}
            >
              <option value="all">Semua akses</option>
              <option value="1">View Only</option>
              <option value="2">View + Edit</option>
            </FilterSelect>
          </div>
          <TableViewport label="Daftar database">
            <table className="data-table">
              <caption className="sr-only">Database yang dapat diakses</caption>
              <thead>
                <tr>
                  <th scope="col">Nama database</th>
                  <th scope="col">Akses</th>
                </tr>
              </thead>
              <tbody>
                {filtered.map((database) => (
                  <tr key={database.id}>
                    <th scope="row" className="max-w-72 font-medium [overflow-wrap:anywhere]">
                      <Link
                        href={`/databases/${resourceID(database.id)}`}
                        className="inline-flex min-h-11 items-center hover:text-accent"
                      >
                        {database.name}
                      </Link>
                    </th>
                    <td>
                      <Badge>{accessLabel(database.access_level)}</Badge>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {!filtered.length && (
              <EmptyState
                title={
                  databases.data.databases.length
                    ? "Database tidak ditemukan"
                    : "Belum ada database yang dapat diakses"
                }
                description={
                  databases.data.databases.length
                    ? "Coba kata kunci atau filter lain."
                    : "Minta administrator memberikan akses database melalui grup Anda."
                }
              />
            )}
          </TableViewport>
          <p role="status" className="mt-4 text-xs text-ink-secondary">
            Menampilkan {filtered.length} dari {databases.data.databases.length} database
          </p>
        </>
      )}
    </section>
  );
}
