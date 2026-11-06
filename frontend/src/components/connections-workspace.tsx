"use client";

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useAuth } from "@/components/auth-provider";
import { AddConnection, ConnectionForm } from "@/components/connection-form";
import { AdminOnly, MutationDialog, ResourceError } from "@/components/resource-ui";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { EmptyState, LoadingState } from "@/components/ui/primitives";
import { PageHeading, SearchInput, TableViewport } from "@/components/workspace-ui";
import { databaseApi, type Connection } from "@/lib/api/databases";

export function ConnectionsWorkspace() {
  return (
    <AdminOnly>
      <ConnectionsList />
    </AdminOnly>
  );
}

function EditConnection({
  connection,
  saved,
}: {
  connection: Connection;
  saved: () => Promise<unknown>;
}) {
  const { api } = useAuth();
  const [open, setOpen] = useState(false);
  const [pending, setPending] = useState(false);
  return (
    <Dialog
      title={`Edit ${connection.name}`}
      description="Password tersimpan tidak ditampilkan. Kosongkan password untuk mempertahankannya."
      trigger={
        <Button variant="ghost" size="small">
          Edit
        </Button>
      }
      open={open}
      onOpenChange={(next) => {
        if (!pending) setOpen(next);
      }}
    >
      <ConnectionForm
        initial={connection}
        onPendingChange={setPending}
        onSubmit={async (input) => {
          await databaseApi(api).updateConnection(connection.id, input);
          await saved();
        }}
        onSaved={() => setOpen(false)}
        onCancel={() => setOpen(false)}
      />
    </Dialog>
  );
}

function ConnectionTest({ id }: { id: string }) {
  const { api } = useAuth();
  const [pending, setPending] = useState(false);
  const [result, setResult] = useState<"success" | "error" | null>(null);
  return (
    <div>
      <Button
        variant="ghost"
        size="small"
        disabled={pending}
        onClick={async () => {
          if (pending) return;
          setPending(true);
          setResult(null);
          try {
            await databaseApi(api).testConnection(id);
            setResult("success");
          } catch {
            setResult("error");
          } finally {
            setPending(false);
          }
        }}
      >
        {pending ? "Menguji…" : "Uji koneksi"}
      </Button>
      {result && (
        <p
          role={result === "error" ? "alert" : "status"}
          className={`max-w-52 px-3 text-xs leading-5 ${result === "error" ? "text-danger" : "text-accent"}`}
        >
          {result === "success"
            ? "Koneksi berhasil."
            : "Koneksi belum dapat diuji. Periksa konfigurasi dan layanan."}
        </p>
      )}
    </div>
  );
}

function ConnectionsList() {
  const { api } = useAuth();
  const databases = databaseApi(api);
  const client = useQueryClient();
  const [search, setSearch] = useState("");
  const connections = useQuery({
    queryKey: ["connections"],
    queryFn: ({ signal }) => databases.connections(signal),
  });
  const filtered =
    connections.data?.connections.filter((connection) =>
      `${connection.name} ${connection.host} ${connection.database}`
        .toLowerCase()
        .includes(search.trim().toLowerCase()),
    ) ?? [];
  async function saved() {
    await client.invalidateQueries({ queryKey: ["connections"] });
    await client.invalidateQueries({ queryKey: ["databases"] });
  }
  return (
    <section className="min-w-0">
      <PageHeading
        title="Koneksi"
        description="Kelola konfigurasi koneksi PostgreSQL. Akses isi database diberikan melalui grup."
        action={
          connections.isSuccess && (
            <AddConnection
              onSubmit={async (input) => {
                await databases.createConnection(input);
                await saved();
              }}
            />
          )
        }
      />
      {connections.isPending ? (
        <LoadingState />
      ) : connections.isError ? (
        <ResourceError error={connections.error} retry={connections.refetch} />
      ) : (
        <>
          <SearchInput
            id="connection-search"
            label="Cari koneksi"
            placeholder="Cari nama, host, atau database…"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            className="mb-5 sm:max-w-80"
          />
          <TableViewport label="Daftar koneksi">
            <table className="data-table min-w-[760px]">
              <caption className="sr-only">Koneksi PostgreSQL terdaftar</caption>
              <thead>
                <tr>
                  <th scope="col">Nama</th>
                  <th scope="col">Host / database</th>
                  <th scope="col">Username / SSL</th>
                  <th scope="col">Tindakan</th>
                </tr>
              </thead>
              <tbody>
                {filtered.map((connection) => (
                  <tr key={connection.id}>
                    <th scope="row" className="max-w-48 font-medium [overflow-wrap:anywhere]">
                      {connection.name}
                    </th>
                    <td className="max-w-64 font-mono text-xs [overflow-wrap:anywhere]">
                      {connection.host}:{connection.port}
                      <p className="mt-1 text-ink-secondary">{connection.database}</p>
                    </td>
                    <td className="max-w-48 text-xs [overflow-wrap:anywhere]">
                      {connection.username}
                      <p className="mt-1 text-ink-secondary">{connection.sslmode}</p>
                    </td>
                    <td>
                      <div className="flex flex-wrap items-start gap-1">
                        <EditConnection connection={connection} saved={saved} />
                        <ConnectionTest id={connection.id} />
                        <MutationDialog
                          title={`Hapus koneksi ${connection.name}?`}
                          description="Konfigurasi koneksi akan dihapus dari platform. Server akan menolak penghapusan bila koneksi masih digunakan."
                          submitLabel="Hapus koneksi"
                          danger
                          trigger={
                            <Button variant="ghost" size="small" className="text-danger">
                              Hapus
                            </Button>
                          }
                          onSubmit={async () => {
                            await databases.deleteConnection(connection.id);
                            await saved();
                          }}
                        />
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {!filtered.length && (
              <EmptyState
                title={search ? "Koneksi tidak ditemukan" : "Belum ada koneksi"}
                description={
                  search
                    ? "Coba nama atau host lain."
                    : "Tambahkan koneksi PostgreSQL untuk mulai bekerja."
                }
              />
            )}
          </TableViewport>
        </>
      )}
    </section>
  );
}
