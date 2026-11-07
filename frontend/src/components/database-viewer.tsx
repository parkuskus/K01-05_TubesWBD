"use client";

import Link from "next/link";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/components/auth-provider";
import { ResourceError, SelectField } from "@/components/resource-ui";
import { Button } from "@/components/ui/button";
import { Badge, EmptyState, LoadingState } from "@/components/ui/primitives";
import { PageHeading, TableViewport } from "@/components/workspace-ui";
import {
  accessLabel,
  databaseApi,
  type DatabaseChoice,
  type Definition,
  type TableDetails,
} from "@/lib/api/databases";
import { resourceID } from "@/lib/api/resources";

export function DatabaseHeading({
  database,
  diff = false,
  section,
}: {
  database: DatabaseChoice;
  diff?: boolean;
  section?: "migrations" | "history";
}) {
  return (
    <>
      <Link
        href="/"
        className="mb-4 inline-flex min-h-11 items-center text-sm text-ink-secondary hover:text-accent"
      >
        ← Semua database
      </Link>
      <PageHeading
        title={database.name}
        description={`${accessLabel(database.access_level)} · PostgreSQL`}
      />
      <nav
        aria-label="Navigasi database"
        className="mb-8 flex flex-wrap gap-6 border-b border-line-soft text-sm"
      >
        <Link
          href={`/databases/${resourceID(database.id)}`}
          aria-current={!diff && !section ? "page" : undefined}
          className={`inline-flex min-h-12 items-center border-b-2 ${!diff && !section ? "border-accent font-medium text-accent" : "border-transparent text-ink-secondary hover:text-accent"}`}
        >
          Schema & data
        </Link>
        <Link
          href={`/databases/${resourceID(database.id)}/schema-diff`}
          aria-current={diff ? "page" : undefined}
          className={`inline-flex min-h-12 items-center border-b-2 ${diff ? "border-accent font-medium text-accent" : "border-transparent text-ink-secondary hover:text-accent"}`}
        >
          Schema diff
        </Link>
        {(
          [
            ["migrations", "Migration"],
            ["history", "History"],
          ] as const
        ).map(([key, label]) => (
          <Link
            key={key}
            href={`/databases/${resourceID(database.id)}/${key}`}
            aria-current={section === key ? "page" : undefined}
            className={`inline-flex min-h-12 items-center border-b-2 ${section === key ? "border-accent font-medium text-accent" : "border-transparent text-ink-secondary hover:text-accent"}`}
          >
            {label}
          </Link>
        ))}
        <Link
          href={`/activity?database=${resourceID(database.id)}`}
          className="inline-flex min-h-12 items-center text-ink-secondary hover:text-accent"
        >
          Review & job
        </Link>
      </nav>
    </>
  );
}

export function DatabaseViewer({ id }: { id: string }) {
  const { api } = useAuth();
  const databases = databaseApi(api);
  const router = useRouter();
  const pathname = usePathname();
  const params = useSearchParams();
  const database = useQuery({
    queryKey: ["databases", id],
    queryFn: ({ signal }) => databases.database(id, signal),
  });
  const schemas = useQuery({
    queryKey: ["databases", id, "schemas"],
    queryFn: ({ signal }) => databases.schemas(id, signal),
    enabled: database.isSuccess,
  });
  const schema = params.get("schema") ?? schemas.data?.schemas[0] ?? "";
  const validSchema = schemas.data?.schemas.includes(schema) ?? false;
  const tables = useQuery({
    queryKey: ["databases", id, "tables", schema],
    queryFn: ({ signal }) => databases.tables(id, schema, signal),
    enabled: database.isSuccess && schemas.isSuccess && validSchema,
  });
  const table = params.get("table") ?? "";
  const validTable = tables.data?.tables.some((item) => item.name === table) ?? false;
  const view = params.get("view") === "data" ? "data" : "structure";
  const rawPage = Number(params.get("page") ?? "1");
  const page = Number.isInteger(rawPage) && rawPage >= 1 && rawPage <= 1000000 ? rawPage : 1;
  function navigate(next: { schema?: string; table?: string; view?: string; page?: number }) {
    const query = new URLSearchParams({
      schema,
      ...(table ? { table } : {}),
      view,
      page: String(page),
    });
    for (const [key, value] of Object.entries(next)) {
      if (value === "") query.delete(key);
      else query.set(key, String(value));
    }
    router.push(`${pathname}?${query}`, { scroll: false });
  }
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
      <DatabaseHeading database={database.data} />
      {schemas.isPending ? (
        <LoadingState />
      ) : schemas.isError ? (
        <ResourceError error={schemas.error} retry={schemas.refetch} />
      ) : !schemas.data.schemas.length ? (
        <EmptyState
          title="Belum ada schema"
          description="Tidak ada schema yang dapat ditampilkan pada database ini."
        />
      ) : (
        <>
          <div className="mb-6 sm:max-w-80">
            <SelectField
              id="viewer-schema"
              label="Schema"
              value={validSchema ? schema : ""}
              onChange={(event) => navigate({ schema: event.target.value, table: "", page: 1 })}
            >
              <option value="" disabled>
                Pilih schema
              </option>
              {schemas.data.schemas.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </SelectField>
          </div>
          {!validSchema ? (
            <EmptyState
              title="Schema tidak ditemukan"
              description="Pilih schema lain untuk melanjutkan."
            />
          ) : tables.isPending ? (
            <LoadingState />
          ) : tables.isError ? (
            <ResourceError error={tables.error} retry={tables.refetch} />
          ) : (
            <div className="grid min-w-0 items-start gap-6 xl:grid-cols-[220px_minmax(0,1fr)]">
              <aside
                aria-label="Tabel schema"
                className="min-w-0 rounded-md border border-line p-3"
              >
                <h2 className="px-2 py-2 text-sm font-semibold">Tabel & view</h2>
                {tables.data.tables.length ? (
                  <nav aria-label="Pilih tabel" className="max-h-80 overflow-y-auto">
                    {tables.data.tables.map((item) => (
                      <Link
                        key={item.name}
                        href={`${pathname}?${new URLSearchParams({ schema, table: item.name, view, page: "1" })}`}
                        scroll={false}
                        aria-current={item.name === table ? "page" : undefined}
                        className={`block min-h-11 rounded-md px-2 py-2.5 text-sm [overflow-wrap:anywhere] ${item.name === table ? "bg-accent-soft text-accent" : "hover:bg-ground"}`}
                      >
                        {item.name}
                        <span className="mt-0.5 block text-xs text-ink-secondary">{item.kind}</span>
                      </Link>
                    ))}
                  </nav>
                ) : (
                  <p className="px-2 py-3 text-sm text-ink-secondary">Belum ada tabel.</p>
                )}
              </aside>
              <div className="min-w-0">
                {!table ? (
                  <EmptyState
                    title="Pilih tabel"
                    description="Pilih tabel atau view untuk melihat struktur dan datanya."
                  />
                ) : !validTable ? (
                  <EmptyState
                    title="Tabel tidak ditemukan"
                    description="Pilih tabel lain dari daftar schema."
                  />
                ) : (
                  <>
                    <h2 className="mb-3 text-lg font-semibold [overflow-wrap:anywhere]">
                      {schema}.{table}
                    </h2>
                    <div role="group" aria-label="Tampilan tabel" className="mb-5 flex gap-2">
                      <Button
                        variant={view === "structure" ? "primary" : "secondary"}
                        aria-pressed={view === "structure"}
                        onClick={() => navigate({ view: "structure" })}
                      >
                        Struktur
                      </Button>
                      <Button
                        variant={view === "data" ? "primary" : "secondary"}
                        aria-pressed={view === "data"}
                        onClick={() => navigate({ view: "data" })}
                      >
                        Data
                      </Button>
                    </div>
                    {view === "structure" ? (
                      <Structure
                        key={`${id}/${schema}/${table}`}
                        id={id}
                        schema={schema}
                        table={table}
                      />
                    ) : (
                      <TableData
                        key={`${id}/${schema}/${table}`}
                        id={id}
                        schema={schema}
                        table={table}
                        page={page}
                        onPage={(page) => navigate({ page })}
                      />
                    )}
                  </>
                )}
              </div>
            </div>
          )}
        </>
      )}
    </section>
  );
}

function Structure({ id, schema, table }: { id: string; schema: string; table: string }) {
  const { api } = useAuth();
  const details = useQuery({
    queryKey: ["databases", id, "table", schema, table],
    queryFn: ({ signal }) => databaseApi(api).details(id, schema, table, signal),
  });
  if (details.isPending) return <LoadingState />;
  if (details.isError) return <ResourceError error={details.error} retry={details.refetch} />;
  return <TableStructure details={details.data} />;
}

export function TableStructure({ details }: { details: TableDetails }) {
  return (
    <div className="min-w-0 space-y-7">
      <TableViewport label="Kolom tabel">
        <table className="data-table min-w-[540px]">
          <caption className="sr-only">Struktur kolom</caption>
          <thead>
            <tr>
              <th scope="col">Kolom</th>
              <th scope="col">Tipe</th>
              <th scope="col">Nullable</th>
              <th scope="col">Default</th>
            </tr>
          </thead>
          <tbody>
            {details.columns.map((column) => (
              <tr key={column.name}>
                <th scope="row" className="font-mono text-xs">
                  {column.name}
                </th>
                <td className="font-mono text-xs">{column.type}</td>
                <td>{column.nullable ? "Ya" : "Tidak"}</td>
                <td className="max-w-60 font-mono text-xs [overflow-wrap:anywhere]">
                  {column.default || "—"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {!details.columns.length && (
          <EmptyState title="Tidak ada kolom" description="Tabel ini tidak memiliki kolom." />
        )}
      </TableViewport>
      <Definitions title="Constraint" items={details.constraints} />
      <Definitions title="Index" items={details.indexes} />
    </div>
  );
}

function Definitions({ title, items }: { title: string; items: Definition[] }) {
  return (
    <section className="min-w-0">
      <h3 className="mb-3 text-sm font-semibold">{title}</h3>
      {items.length ? (
        <div className="space-y-3">
          {items.map((item) => (
            <div key={item.name} className="min-w-0 rounded-md border border-line p-4">
              <p className="mb-2 text-xs font-medium [overflow-wrap:anywhere]">{item.name}</p>
              <pre
                tabIndex={0}
                aria-label={`${title} ${item.name}`}
                className="max-w-full overflow-x-auto font-mono text-xs leading-6"
              >
                {item.sql}
              </pre>
            </div>
          ))}
        </div>
      ) : (
        <p className="text-sm text-ink-secondary">Tidak ada {title.toLowerCase()}.</p>
      )}
    </section>
  );
}

function TableData({
  id,
  schema,
  table,
  page,
  onPage,
}: {
  id: string;
  schema: string;
  table: string;
  page: number;
  onPage: (page: number) => void;
}) {
  const { api } = useAuth();
  const data = useQuery({
    queryKey: ["databases", id, "data", schema, table, page],
    queryFn: ({ signal }) => databaseApi(api).data(id, schema, table, page, signal),
  });
  if (data.isPending) return <LoadingState />;
  if (data.isError) return <ResourceError error={data.error} retry={data.refetch} />;
  return (
    <div className="min-w-0 space-y-4">
      <p className="text-xs leading-5 text-ink-secondary">
        Data read-only · maksimal 50 baris per halaman. Nilai panjang dapat terpotong.
      </p>
      <TableViewport label="Data tabel">
        <table className="data-table">
          <caption className="sr-only">
            Data {schema}.{table}, halaman {page}
          </caption>
          <thead>
            <tr>
              {data.data.columns.length ? (
                data.data.columns.map((column) => (
                  <th key={column} scope="col">
                    {column}
                  </th>
                ))
              ) : (
                <th scope="col">Baris</th>
              )}
            </tr>
          </thead>
          <tbody>
            {data.data.rows.map((row, index) => (
              <tr key={index}>
                {row.length ? (
                  row.map((cell, column) => (
                    <td key={column} className="max-w-80 min-w-24 align-top">
                      <div className="font-mono text-xs [overflow-wrap:anywhere] whitespace-pre-wrap">
                        {cell.null ? (
                          <span className="text-ink-secondary italic">NULL</span>
                        ) : cell.text === "" ? (
                          <span className="text-ink-secondary italic">(kosong)</span>
                        ) : (
                          cell.text
                        )}
                        {cell.truncated && (
                          <span className="ml-2">
                            <Badge tone="warning">Terpotong</Badge>
                          </span>
                        )}
                      </div>
                    </td>
                  ))
                ) : (
                  <td>(baris tanpa kolom)</td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
        {!data.data.rows.length && (
          <EmptyState
            title="Tidak ada baris"
            description={
              page > 1
                ? "Kembali ke halaman sebelumnya atau halaman pertama."
                : "Tabel atau view ini belum berisi data."
            }
          />
        )}
      </TableViewport>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p role="status" className="text-xs text-ink-secondary">
          Halaman {data.data.page} · {data.data.rows.length} baris
        </p>
        <div className="flex flex-wrap gap-2">
          {page > 2 && (
            <Button variant="ghost" onClick={() => onPage(1)}>
              Halaman pertama
            </Button>
          )}
          <Button
            variant="secondary"
            disabled={page <= 1 || data.isFetching}
            onClick={() => onPage(page - 1)}
          >
            Sebelumnya
          </Button>
          <Button
            variant="secondary"
            disabled={!data.data.has_next || page >= 1000000 || data.isFetching}
            onClick={() => onPage(page + 1)}
          >
            Berikutnya
          </Button>
        </div>
      </div>
    </div>
  );
}
