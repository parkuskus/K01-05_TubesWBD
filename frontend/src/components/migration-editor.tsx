"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { useAuth } from "@/components/auth-provider";
import { ActionButton, MigrationFrame, PlanDetails } from "@/components/migration-ui";
import {
  mutationMessage,
  MutationDialog,
  ResourceError,
  SelectField,
} from "@/components/resource-ui";
import { Button, buttonStyles } from "@/components/ui/button";
import { Badge, EmptyState, LoadingState, SqlField, TextField } from "@/components/ui/primitives";
import { ApiError } from "@/lib/api/client";
import {
  migrationApi,
  type DraftInput,
  type MigrationDefinition,
  type ValidationReport,
} from "@/lib/api/migrations";
import { resourceID } from "@/lib/api/resources";
import { directionLabels } from "@/lib/migrations";

export function MigrationEditor({ id, migrationId }: { id: string; migrationId?: string }) {
  return (
    <MigrationFrame id={id}>
      {(database) =>
        migrationId ? (
          <ExistingEditor id={id} migrationId={migrationId} />
        ) : database.capabilities.create_migration ? (
          <EditorForm id={id} />
        ) : (
          <EmptyState
            title="Akses ditolak"
            description="Akses View + Edit diperlukan untuk membuat migration."
          />
        )
      }
    </MigrationFrame>
  );
}

function ExistingEditor({ id, migrationId }: { id: string; migrationId: string }) {
  const { api } = useAuth();
  const definition = useQuery({
    queryKey: ["migrations", id, migrationId],
    queryFn: ({ signal }) => migrationApi(api).get(id, migrationId, signal),
  });
  if (definition.isPending) return <LoadingState />;
  if (definition.isError)
    return <ResourceError error={definition.error} retry={definition.refetch} />;
  return (
    <EditorForm
      key={`${definition.data.id}/${definition.data.revision_id}`}
      id={id}
      definition={definition.data}
    />
  );
}

function EditorForm({ id, definition }: { id: string; definition?: MigrationDefinition }) {
  const { api } = useAuth();
  const migrations = migrationApi(api);
  const client = useQueryClient();
  const router = useRouter();
  const form = useRef<HTMLFormElement>(null);
  const [irreversible, setIrreversible] = useState(definition?.is_irreversible ?? false);
  const [mode, setMode] = useState(definition?.mode ?? "NORMAL");
  const [dirty, setDirty] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const [fields, setFields] = useState<Record<string, string>>({});
  const [report, setReport] = useState<ValidationReport | null>(null);
  const [conflict, setConflict] = useState(false);
  const editable = !definition || (!definition.ever_executed && definition.capabilities.edit);
  const base = `/databases/${resourceID(id)}/migrations`;
  useEffect(() => {
    const first = form.current?.querySelector<HTMLElement>('[aria-invalid="true"]');
    first?.focus();
  }, [fields]);

  return (
    <div className="min-w-0 space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 className="min-w-0 text-lg font-semibold [overflow-wrap:anywhere]">
          {definition ? `v${definition.version} · ${definition.title}` : "Buat migration"}
        </h2>
        <Link href={base} className={buttonStyles({ variant: "ghost" })}>
          Semua migration
        </Link>
      </div>
      {definition && (
        <p className="text-sm [overflow-wrap:anywhere] text-ink-secondary">
          Revisi <span className="font-mono">{definition.revision_id}</span> ·{" "}
          <Badge>{definition.ever_executed ? "SQL terkunci" : "Draft"}</Badge>
        </p>
      )}
      {definition?.ever_executed && (
        <p className="rounded-md bg-warning-surface p-4 text-sm leading-6 text-warning">
          Migration ini pernah dieksekusi. Perbaikan SQL harus dibuat sebagai migration baru.
        </p>
      )}
      {!editable && definition ? (
        <PlanDetails plan={definition} />
      ) : (
        <form
          ref={form}
          className="min-w-0 space-y-5"
          onChange={() => {
            setDirty(true);
            setReport(null);
          }}
          onSubmit={async (event) => {
            event.preventDefault();
            if (pending || !editable || conflict) return;
            const values = new FormData(event.currentTarget);
            const text = (key: string) => String(values.get(key) ?? "").trim();
            const input: DraftInput = {
              title: text("title"),
              up_sql: text("up_sql"),
              down_sql: irreversible ? "" : text("down_sql"),
              is_irreversible: irreversible,
              irreversible_reason: irreversible ? text("irreversible_reason") : "",
              mode,
              compatibility_notes: text("compatibility_notes"),
              batch:
                mode === "BATCH"
                  ? {
                      table_schema: text("table_schema"),
                      table_name: text("table_name"),
                      primary_key_column: text("primary_key_column"),
                      scope_where: text("scope_where"),
                      batch_size: Number(values.get("batch_size")),
                    }
                  : null,
            };
            setPending(true);
            setError("");
            setFields({});
            try {
              const saved = definition
                ? await migrations.update(id, definition.id, definition.revision_id, input)
                : await migrations.create(id, input);
              if (!saved.id || !saved.revision_id)
                throw new ApiError("Respons draft tidak valid.", 200, "INVALID_RESPONSE");
              client.setQueryData(["migrations", id, saved.id], saved);
              await client.invalidateQueries({ queryKey: ["migrations", id], exact: true });
              setDirty(false);
              setReport(null);
              if (!definition) router.replace(`${base}/${resourceID(saved.id)}`);
            } catch (failure) {
              setError(mutationMessage(failure));
              if (failure instanceof ApiError) {
                if ([400, 422].includes(failure.status)) setFields(failure.fieldErrors);
                if (failure.status === 409) setConflict(true);
              }
            } finally {
              setPending(false);
            }
          }}
        >
          <fieldset
            disabled={pending || !editable || conflict}
            className="min-w-0 space-y-5 disabled:opacity-70"
          >
            <TextField
              id="migration-title"
              name="title"
              label="Judul migration"
              defaultValue={definition?.title ?? ""}
              required
              maxLength={200}
              error={fields.title}
            />
            <SqlField
              id="migration-up"
              name="up_sql"
              label="SQL up"
              defaultValue={definition?.up_sql ?? ""}
              required
              error={fields.up_sql}
              hint="SQL diperiksa dan diajukan untuk review sebelum masuk antrean."
            />
            <label className="flex min-h-11 items-center gap-3 text-sm">
              <input
                type="checkbox"
                name="is_irreversible"
                checked={irreversible}
                onChange={(event) => setIrreversible(event.target.checked)}
                className="size-4 accent-accent"
              />
              Irreversible
            </label>
            <fieldset hidden={!irreversible} disabled={!irreversible}>
              <TextField
                id="migration-reason"
                name="irreversible_reason"
                label="Alasan irreversible"
                defaultValue={definition?.irreversible_reason ?? ""}
                required
                error={fields.irreversible_reason}
                hint="Perubahan ini tidak dapat di-rollback atau dikompensasi."
              />
            </fieldset>
            <fieldset hidden={irreversible} disabled={irreversible}>
              <SqlField
                id="migration-down"
                name="down_sql"
                label="SQL down"
                defaultValue={definition?.down_sql ?? ""}
                required
                error={fields.down_sql}
                hint="Gunakan SQL pembatalan yang telah diperiksa, termasuk kebutuhan pemulihan data."
              />
            </fieldset>
            <SelectField
              id="migration-mode"
              name="mode"
              label="Mode eksekusi"
              value={mode}
              error={fields.mode}
              onChange={(event) => setMode(event.target.value as DraftInput["mode"])}
            >
              <option value="NORMAL">Biasa · satu transaksi</option>
              <option value="BATCH">Per batch · commit bertahap</option>
            </SelectField>
            <fieldset
              hidden={mode !== "BATCH"}
              disabled={mode !== "BATCH"}
              className="min-w-0 space-y-4 rounded-md border border-line p-4"
            >
              <p className="text-sm leading-6 text-ink-secondary">
                Gunakan satu UPDATE dengan key integer unik dan tidak berubah. Batch yang sudah
                committed tetap tersimpan jika batch berikutnya gagal.
              </p>
              <div className="grid gap-4 sm:grid-cols-2">
                <TextField
                  id="batch-schema"
                  name="table_schema"
                  label="Schema batch"
                  defaultValue={definition?.batch?.table_schema ?? "public"}
                  required
                  error={fields.table_schema ?? fields["batch.table_schema"]}
                />
                <TextField
                  id="batch-table"
                  name="table_name"
                  label="Tabel batch"
                  defaultValue={definition?.batch?.table_name ?? ""}
                  required
                  error={fields.table_name ?? fields["batch.table_name"]}
                />
                <TextField
                  id="batch-key"
                  name="primary_key_column"
                  label="Kolom key batch"
                  defaultValue={definition?.batch?.primary_key_column ?? "id"}
                  required
                  error={fields.primary_key_column ?? fields["batch.primary_key_column"]}
                />
                <TextField
                  id="batch-size"
                  name="batch_size"
                  label="Ukuran batch"
                  type="number"
                  min={1}
                  step={1}
                  defaultValue={definition?.batch?.batch_size ?? 10000}
                  required
                  error={fields.batch_size ?? fields["batch.batch_size"]}
                />
              </div>
              <SqlField
                id="batch-scope"
                name="scope_where"
                label="Kondisi cakupan batch"
                defaultValue={definition?.batch?.scope_where ?? ""}
                error={fields.scope_where ?? fields["batch.scope_where"]}
                hint="Opsional. Kondisi WHERE tanpa kata WHERE; kosong untuk seluruh row pada cakupan awal."
              />
            </fieldset>
            <TextField
              id="compatibility-notes"
              name="compatibility_notes"
              label="Catatan kompatibilitas aplikasi"
              defaultValue={definition?.compatibility_notes ?? ""}
              error={fields.compatibility_notes}
              hint="Jelaskan dampak schema/data pada aplikasi dan langkah pemeriksaan untuk leader."
            />
          </fieldset>
          {error && (
            <p role="alert" className="text-sm leading-6 text-danger">
              {error}
            </p>
          )}
          {editable && (
            <Button type="submit" disabled={pending || conflict}>
              {pending ? "Menyimpan…" : "Simpan draft"}
            </Button>
          )}
        </form>
      )}
      {conflict && (
        <ActionButton
          action={async () => {
            await client.refetchQueries(
              { queryKey: ["migrations", id, definition?.id], exact: true },
              { throwOnError: true },
            );
            setConflict(false);
            setError("");
            setFields({});
            setReport(null);
          }}
        >
          Muat revisi terbaru
        </ActionButton>
      )}
      {definition && (
        <section className="min-w-0 space-y-5 border-t border-line-soft pt-6">
          <h3 className="text-base font-semibold">Validasi & pengajuan review</h3>
          <p className="text-sm leading-6 text-ink-secondary">
            Validasi memeriksa revisi tersimpan. Keberhasilan SQL tidak menjamin kompatibilitas
            aplikasi; seluruh leader grup berakses, termasuk View Only, harus meninjau perubahan.
          </p>
          {dirty && (
            <p role="status" className="text-sm text-warning">
              Simpan perubahan sebelum validasi dan pengajuan review.
            </p>
          )}
          <ActionButton
            disabled={dirty || pending || conflict}
            action={async () => {
              setReport(null);
              setPending(true);
              try {
                const next = await migrations.validate(id, definition.id, definition.revision_id);
                if (next.revision_id !== definition.revision_id)
                  throw new ApiError("Revisi berubah.", 409, "REVISION_CONFLICT");
                setReport(next);
              } finally {
                setPending(false);
              }
            }}
          >
            Validasi revisi tersimpan
          </ActionButton>
          {report && (
            <div className="space-y-3 rounded-md border border-line p-4" role="status">
              <p className="text-sm font-semibold">
                {report.valid ? "Validasi lolos" : "Validasi belum lolos"}
              </p>
              <ul className="space-y-2 text-sm">
                {report.checks.map((check, index) => (
                  <li key={index} className={check.valid ? "text-ink-secondary" : "text-danger"}>
                    {check.valid ? "Lolos" : "Gagal"} · {check.message}
                  </li>
                ))}
                {report.warnings.map((warning, index) => (
                  <li key={`warning-${index}`} className="text-warning">
                    Perhatian · {warning}
                  </li>
                ))}
              </ul>
            </div>
          )}
          {report?.valid && !dirty && !conflict && (
            <div className="flex flex-wrap gap-3">
              {(["APPLY", "ROLLBACK", "FORWARD"] as const)
                .filter(
                  (direction) =>
                    definition.capabilities[
                      `request_${direction.toLowerCase()}` as
                        "request_apply" | "request_rollback" | "request_forward"
                    ] && !(direction === "ROLLBACK" && definition.is_irreversible),
                )
                .map((direction) => (
                  <MutationDialog
                    key={direction}
                    title={`Ajukan ${directionLabels[direction]}`}
                    description={`Ajukan v${definition.version}, revisi ${definition.revision_id}, mode ${definition.mode}. Posisi aktif yang ditinjau: ${definition.expected_active_head || "awal"}. SQL asli akan ditinjau seluruh leader sebelum dapat masuk antrean.`}
                    trigger={
                      <Button variant="secondary">
                        Ajukan {directionLabels[direction]} untuk review
                      </Button>
                    }
                    submitLabel="Ajukan review"
                    onSubmit={async () => {
                      const request = await migrations.requestOperation(id, {
                        migration_id: definition.id,
                        revision_id: definition.revision_id,
                        direction,
                        expected_active_head: definition.expected_active_head,
                        ...(direction === "ROLLBACK"
                          ? { origin_execution_id: definition.active_execution_id }
                          : {}),
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
      )}
    </div>
  );
}
