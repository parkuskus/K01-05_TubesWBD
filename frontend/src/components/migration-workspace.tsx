"use client";

import Link from "next/link";
import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/components/auth-provider";
import { MigrationFrame } from "@/components/migration-ui";
import { ResourceError } from "@/components/resource-ui";
import { buttonStyles } from "@/components/ui/button";
import { Badge, EmptyState, LoadingState } from "@/components/ui/primitives";
import { TableViewport } from "@/components/workspace-ui";
import { migrationApi } from "@/lib/api/migrations";
import { resourceID } from "@/lib/api/resources";

export function MigrationWorkspace({ id }: { id: string }) {
  return (
    <MigrationFrame id={id}>
      {(database) => <MigrationList id={id} canCreate={database.capabilities.create_migration} />}
    </MigrationFrame>
  );
}

function MigrationList({ id, canCreate }: { id: string; canCreate: boolean }) {
  const { api } = useAuth();
  const migrations = useQuery({
    queryKey: ["migrations", id],
    queryFn: ({ signal }) => migrationApi(api).list(id, signal),
  });
  const base = `/databases/${resourceID(id)}/migrations`;
  return (
    <div className="min-w-0 space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold">Definisi migration</h2>
          <p className="mt-2 text-sm leading-6 text-ink-secondary">
            SQL yang pernah dieksekusi terkunci, termasuk setelah attempt gagal.
          </p>
        </div>
        {canCreate && (
          <Link href={`${base}/new`} className={buttonStyles()}>
            Buat migration
          </Link>
        )}
      </div>
      {migrations.isPending ? (
        <LoadingState />
      ) : migrations.isError ? (
        <ResourceError error={migrations.error} retry={migrations.refetch} />
      ) : (
        <TableViewport label="Definisi migration">
          <table className="data-table min-w-[560px]">
            <caption className="sr-only">Definisi dan revisi migration</caption>
            <thead>
              <tr>
                <th scope="col">Migration</th>
                <th scope="col">Revisi</th>
                <th scope="col">Mode</th>
                <th scope="col">SQL</th>
              </tr>
            </thead>
            <tbody>
              {migrations.data.migrations.map((item) => (
                <tr key={item.id}>
                  <th scope="row" className="max-w-80 font-medium [overflow-wrap:anywhere]">
                    <Link
                      href={`${base}/${resourceID(item.id)}`}
                      className="inline-flex min-h-11 items-center text-accent"
                    >
                      v{item.version} · {item.title}
                    </Link>
                  </th>
                  <td className="font-mono text-xs">{item.revision_id}</td>
                  <td>{item.mode === "BATCH" ? "Per batch" : "Biasa"}</td>
                  <td>
                    <Badge>{item.ever_executed ? "Terkunci" : "Draft"}</Badge>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {!migrations.data.migrations.length && (
            <EmptyState
              title="Belum ada migration"
              description="Buat draft untuk menyiapkan SQL dan mengajukan review."
            />
          )}
        </TableViewport>
      )}
      <div className="flex flex-wrap gap-3">
        <Link
          href={`/activity?database=${resourceID(id)}`}
          className={buttonStyles({ variant: "secondary" })}
        >
          Lihat review & job
        </Link>
        <button
          type="button"
          disabled={migrations.isFetching}
          className={buttonStyles({ variant: "ghost" })}
          onClick={() => void migrations.refetch()}
        >
          Perbarui daftar
        </button>
      </div>
    </div>
  );
}
