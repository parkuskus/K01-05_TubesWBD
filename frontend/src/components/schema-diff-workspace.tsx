"use client";

import Link from "next/link";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useAuth } from "@/components/auth-provider";
import { DatabaseHeading } from "@/components/database-viewer";
import { MutationDialog, ResourceError, SelectField, validateText } from "@/components/resource-ui";
import { Button } from "@/components/ui/button";
import { Badge, EmptyState, LoadingState, SqlField, TextField } from "@/components/ui/primitives";
import { TableViewport } from "@/components/workspace-ui";
import { ApiError } from "@/lib/api/client";
import { databaseApi, type SchemaDiff } from "@/lib/api/databases";

export function SchemaDiffWorkspace({ id }: { id: string }) {
  const { api } = useAuth();
  const databases = databaseApi(api);
  const params = useSearchParams();
  const pathname = usePathname();
  const router = useRouter();
  const [error, setError] = useState("");
  const reference = params.get("reference_schema") ?? "";
  const target = params.get("target_schema") ?? "";
  const database = useQuery({
    queryKey: ["databases", id],
    queryFn: ({ signal }) => databases.database(id, signal),
  });
  const schemas = useQuery({
    queryKey: ["databases", id, "schemas"],
    queryFn: ({ signal }) => databases.schemas(id, signal),
    enabled: database.isSuccess,
  });
  const valid =
    reference !== target &&
    (schemas.data?.schemas.includes(reference) ?? false) &&
    (schemas.data?.schemas.includes(target) ?? false);
  const diff = useQuery({
    queryKey: ["databases", id, "schema-diff", reference, target],
    queryFn: ({ signal }) => databases.diff(id, reference, target, signal),
    enabled: database.isSuccess && schemas.isSuccess && valid,
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
      <DatabaseHeading database={database.data} diff />
      <h2 className="text-lg font-semibold">Bandingkan schema</h2>
      <p className="mt-2 mb-6 text-sm leading-6 text-ink-secondary">
        Bandingkan dua schema pada database ini. SQL yang dihasilkan mengubah target agar sesuai
        dengan referensi.
      </p>
      {schemas.isPending ? (
        <LoadingState />
      ) : schemas.isError ? (
        <ResourceError error={schemas.error} retry={schemas.refetch} />
      ) : schemas.data.schemas.length < 2 ? (
        <EmptyState
          title="Perlu dua schema"
          description="Perbandingan memerlukan sedikitnya dua schema yang dapat diakses."
        />
      ) : (
        <>
          <form
            key={`${reference}/${target}`}
            className="mb-8 grid min-w-0 items-end gap-4 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]"
            onSubmit={(event) => {
              event.preventDefault();
              const values = new FormData(event.currentTarget);
              const reference_schema = String(values.get("reference_schema"));
              const target_schema = String(values.get("target_schema"));
              if (reference_schema === target_schema) {
                setError("Pilih dua schema yang berbeda.");
                return;
              }
              setError("");
              router.push(
                `${pathname}?${new URLSearchParams({ reference_schema, target_schema })}`,
                { scroll: false },
              );
            }}
          >
            <SelectField
              id="diff-reference"
              name="reference_schema"
              label="Schema referensi"
              defaultValue={schemas.data.schemas.includes(reference) ? reference : ""}
              required
            >
              <option value="" disabled>
                Pilih referensi
              </option>
              {schemas.data.schemas.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </SelectField>
            <SelectField
              id="diff-target"
              name="target_schema"
              label="Schema target"
              defaultValue={schemas.data.schemas.includes(target) ? target : ""}
              required
            >
              <option value="" disabled>
                Pilih target
              </option>
              {schemas.data.schemas.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </SelectField>
            <Button type="submit">Bandingkan</Button>
          </form>
          {error && (
            <p role="alert" className="mb-5 text-sm text-danger">
              {error}
            </p>
          )}
          {!valid ? (
            <EmptyState
              title={reference || target ? "Pilihan schema tidak valid" : "Pilih schema"}
              description="Pilih dua schema berbeda yang tersedia untuk melihat perbandingan."
            />
          ) : diff.isPending ? (
            <LoadingState />
          ) : diff.isError ? (
            <ResourceError error={diff.error} retry={diff.refetch} />
          ) : (
            <DiffResult
              key={`${id}/${reference}/${target}`}
              id={id}
              diff={diff.data}
              canDraft={database.data.capabilities?.create_migration === true}
            />
          )}
        </>
      )}
    </section>
  );
}

function DiffResult({ id, diff, canDraft }: { id: string; diff: SchemaDiff; canDraft: boolean }) {
  const { api } = useAuth();
  const [created, setCreated] = useState("");
  const destructive = diff.changes.some((change) => change.risk === "destructive");
  return (
    <div className="min-w-0 space-y-6">
      <p className="text-sm leading-6 text-ink-secondary">
        Referensi <strong className="[overflow-wrap:anywhere]">{diff.reference_schema}</strong> →
        target <strong className="[overflow-wrap:anywhere]">{diff.target_schema}</strong>
      </p>
      {destructive && (
        <p className="rounded-md bg-warning-surface p-4 text-sm leading-6 text-warning">
          Perubahan destructive terdeteksi. Tinjau SQL dan dampaknya pada data sebelum membuat
          migration.
        </p>
      )}
      <TableViewport label="Perbedaan schema">
        <table className="data-table min-w-[660px]">
          <caption className="sr-only">Perubahan pada schema target</caption>
          <thead>
            <tr>
              <th scope="col">Objek</th>
              <th scope="col">Perubahan</th>
              <th scope="col">Sebelum → sesudah</th>
              <th scope="col">Risiko</th>
            </tr>
          </thead>
          <tbody>
            {diff.changes.map((change, index) => (
              <tr key={index}>
                <th scope="row" className="max-w-52 font-normal [overflow-wrap:anywhere]">
                  {change.table ? `${change.table}.` : ""}
                  {change.name}
                  <p className="mt-1 text-xs text-ink-secondary">{change.kind}</p>
                </th>
                <td>{change.action}</td>
                <td className="max-w-72 font-mono text-xs [overflow-wrap:anywhere]">
                  {change.before || "—"}
                  <span className="block text-ink-secondary">→ {change.after || "—"}</span>
                </td>
                <td>
                  <Badge tone={change.risk === "destructive" ? "danger" : "warning"}>
                    {change.risk === "destructive" ? "Destructive" : "Perlu review"}
                  </Badge>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {!diff.changes.length && (
          <EmptyState
            title="Tidak ada perbedaan yang didukung"
            description="Periksa juga daftar objek yang tidak didukung sebelum menyimpulkan schema sama."
          />
        )}
      </TableViewport>
      {diff.unsupported.length > 0 && (
        <section className="min-w-0 rounded-md border border-line p-5">
          <h3 className="text-sm font-semibold">Objek yang tidak didukung</h3>
          <p className="mt-2 text-xs leading-5 text-ink-secondary">
            Objek berikut memerlukan pemeriksaan manual dan tidak sepenuhnya tercakup pada SQL
            otomatis.
          </p>
          <ul className="mt-4 space-y-2 text-sm">
            {diff.unsupported.map((object, index) => (
              <li key={index} className="[overflow-wrap:anywhere]">
                {object.schema}.{object.table ? `${object.table}.` : ""}
                {object.name} <span className="text-ink-secondary">({object.kind})</span>
              </li>
            ))}
          </ul>
        </section>
      )}
      {diff.sql && (
        <SqlField
          id="diff-sql"
          label="SQL usulan"
          value={diff.sql}
          readOnly
          hint="SQL ini belum dijalankan. Draft tetap memerlukan tinjauan sebelum eksekusi."
        />
      )}
      {created ? (
        <p role="status" className="rounded-md bg-accent-soft p-4 text-sm leading-6 text-accent">
          Draft #{created} berhasil dibuat. Lengkapi SQL down atau alasan irreversible sebelum
          mengajukan review.
        </p>
      ) : canDraft && diff.sql ? (
        <MutationDialog
          title="Buat draft dari schema diff"
          description="Server akan membandingkan ulang schema sebelum menyimpan draft. SQL tidak langsung dijalankan."
          submitLabel="Buat draft"
          trigger={<Button>Buat draft migration</Button>}
          onSubmit={async (values) => {
            const result = await databaseApi(api).createDiffDraft(id, {
              reference_schema: diff.reference_schema,
              target_schema: diff.target_schema,
              title: validateText(values.get("title"), "title", 200),
            });
            if (!result?.migration_id)
              throw new ApiError("Respons draft tidak valid.", 200, "INVALID_RESPONSE");
            setCreated(result.migration_id);
          }}
        >
          {(errors) => (
            <TextField
              id="diff-draft-title"
              name="title"
              label="Judul migration"
              defaultValue={`Sync ${diff.target_schema} with ${diff.reference_schema}`}
              required
              error={errors.title}
            />
          )}
        </MutationDialog>
      ) : (
        !canDraft && (
          <p className="text-sm text-ink-secondary">
            Akses View + Edit diperlukan untuk membuat draft migration.
          </p>
        )
      )}
    </div>
  );
}
