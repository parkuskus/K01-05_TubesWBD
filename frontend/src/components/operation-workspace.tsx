"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useAuth } from "@/components/auth-provider";
import { ActionButton, OperationBadge, PlanDetails } from "@/components/migration-ui";
import { MutationDialog, ResourceError } from "@/components/resource-ui";
import { Button, buttonStyles } from "@/components/ui/button";
import { Badge, LoadingState } from "@/components/ui/primitives";
import { DetailRow, PageHeading, TableViewport } from "@/components/workspace-ui";
import { migrationApi } from "@/lib/api/migrations";
import { ApiError } from "@/lib/api/client";
import { resourceID } from "@/lib/api/resources";
import { directionLabels, formatTime, pollInterval } from "@/lib/migrations";

export function OperationWorkspace({ id }: { id: string }) {
  const { api } = useAuth();
  const router = useRouter();
  const migrations = migrationApi(api);
  const [uncertain, setUncertain] = useState(false);
  const request = useQuery({
    queryKey: ["operation", id],
    queryFn: ({ signal }) => migrations.operation(id, signal),
    refetchInterval: (query) =>
      pollInterval(query.state.data?.status, query.state.error, query.state.fetchFailureCount),
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
  });
  if (request.isPending) return <LoadingState />;
  if (
    request.isError &&
    (!request.data ||
      (request.error instanceof ApiError && [401, 403, 404].includes(request.error.status)))
  )
    return <ResourceError error={request.error} retry={request.refetch} />;
  const item = request.data!;
  const fresh = !request.isError && !request.isFetching;
  return (
    <section className="min-w-0 space-y-6">
      <Link
        href={`/activity?database=${resourceID(item.database_id)}`}
        className="inline-flex min-h-11 items-center text-sm text-accent"
      >
        ← Review & job database
      </Link>
      <PageHeading
        title={item.title}
        description={`${directionLabels[item.direction]} · v${item.plan.version} · Revisi ${item.plan.revision_id}`}
        action={<OperationBadge status={item.status} />}
      />
      {request.isError && <ResourceError error={request.error} retry={request.refetch} />}
      {item.status === "STALE" && (
        <p
          role="alert"
          className="rounded-md bg-warning-surface p-4 text-sm leading-6 text-warning"
        >
          Revisi, akses, atau leader berubah. Tinjau keadaan terbaru dan ajukan review baru dari
          migration.
        </p>
      )}
      <dl className="divide-y divide-line-soft">
        <DetailRow label="Request ID">{item.id}</DetailRow>
        <DetailRow label="Round review">{item.round}</DetailRow>
        <DetailRow label="Diajukan">{formatTime(item.created_at)}</DetailRow>
        <DetailRow label="Head yang ditinjau">
          {item.plan.expected_active_head || "Posisi awal"}
        </DetailRow>
        {item.plan.origin_execution_id && (
          <DetailRow label="Execution asal">{item.plan.origin_execution_id}</DetailRow>
        )}
        <DetailRow label="Checksum plan">
          <span className="font-mono text-xs">{item.plan_checksum}</span>
        </DetailRow>
      </dl>
      <PlanDetails plan={{ ...item.plan, compatibility_notes: item.compatibility_notes }} />
      <section className="min-w-0 space-y-4">
        <h2 className="text-base font-semibold">Persetujuan seluruh grup</h2>
        <p className="text-sm leading-6 text-ink-secondary">
          Leader setiap grup dengan akses View Only maupun View + Edit harus menyetujui plan pada
          round ini.
        </p>
        <TableViewport label="Persetujuan per grup">
          <table className="data-table min-w-[620px]">
            <caption className="sr-only">Leader dan status persetujuan setiap grup</caption>
            <thead>
              <tr>
                <th scope="col">Grup</th>
                <th scope="col">Akses</th>
                <th scope="col">Leader</th>
                <th scope="col">Persetujuan</th>
                <th scope="col">Tindakan</th>
              </tr>
            </thead>
            <tbody>
              {item.requirements.map((group) => (
                <tr key={group.group_id}>
                  <th scope="row" className="max-w-60 font-medium [overflow-wrap:anywhere]">
                    {group.group_name}
                  </th>
                  <td>{group.access_level === 1 ? "View Only" : "View + Edit"}</td>
                  <td>{group.leader_name || group.leader_id || "Leader belum ditetapkan"}</td>
                  <td>
                    <Badge tone={group.status === "APPROVED" ? "success" : "warning"}>
                      {
                        {
                          PENDING: "Menunggu",
                          APPROVED: "Disetujui",
                          REJECTED: "Ditolak",
                          STALE: "Kedaluwarsa",
                        }[group.status]
                      }
                    </Badge>
                  </td>
                  <td>
                    {group.can_approve &&
                      group.leader_id &&
                      item.status === "PENDING_REVIEW" &&
                      group.status === "PENDING" && (
                        <MutationDialog
                          title={`Setujui untuk ${group.group_name}`}
                          description={`Setujui ${directionLabels[item.direction]} revisi ${item.plan.revision_id} pada round ${item.round}. Pastikan perubahan kompatibel dengan aplikasi grup Anda.`}
                          trigger={
                            <Button variant="secondary" disabled={!fresh}>
                              Setujui untuk {group.group_name}
                            </Button>
                          }
                          submitLabel="Setujui plan"
                          onSubmit={async () => {
                            try {
                              await migrations.approve(item, group.group_id);
                            } finally {
                              await request.refetch();
                            }
                          }}
                        />
                      )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableViewport>
        {!item.requirements.length && (
          <p className="text-sm text-warning">
            Belum ada persyaratan persetujuan yang tersedia. Periksa kebijakan akses database.
          </p>
        )}
      </section>
      <div className="flex flex-wrap gap-3 border-t border-line-soft pt-6">
        <Link
          href={`/databases/${resourceID(item.database_id)}/migrations/${resourceID(item.migration_id)}`}
          className={buttonStyles({ variant: "secondary" })}
        >
          Lihat migration
        </Link>
        {item.job_id ? (
          <Link href={`/jobs/${resourceID(item.job_id)}`} className={buttonStyles()}>
            Pantau job
          </Link>
        ) : (
          item.status === "APPROVED" &&
          item.capabilities.enqueue &&
          !uncertain && (
            <MutationDialog
              title="Masukkan ke antrean"
              description="Worker akan memeriksa ulang approval dan posisi aktif sebelum menjalankan SQL. Anda dapat memantau job setelah masuk antrean."
              trigger={<Button disabled={!fresh}>Masukkan ke antrean</Button>}
              submitLabel="Antrekan job"
              onSubmit={async () => {
                try {
                  const job = await migrations.enqueue(item);
                  if (!job.job_id)
                    throw new ApiError("Respons job tidak valid.", 200, "INVALID_RESPONSE");
                  router.push(`/jobs/${resourceID(job.job_id)}`);
                } catch (failure) {
                  setUncertain(true);
                  throw failure;
                }
              }}
            />
          )
        )}
      </div>
      {uncertain && !item.job_id && (
        <div className="space-y-3 rounded-md bg-warning-surface p-4">
          <p role="alert" className="text-sm leading-6 text-warning">
            Respons antrean belum terkonfirmasi. Periksa status pengajuan sebelum mencoba lagi;
            pengajuan yang sama menggunakan kunci antrean yang sama.
          </p>
          <ActionButton
            action={async () => {
              const result = await request.refetch();
              if (result.error) throw result.error;
              if (result.data?.job_id) router.push(`/jobs/${resourceID(result.data.job_id)}`);
              else setUncertain(false);
            }}
          >
            Periksa status pengajuan
          </ActionButton>
        </div>
      )}
    </section>
  );
}
