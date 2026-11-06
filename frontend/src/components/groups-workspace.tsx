"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Plus } from "lucide-react";
import { useAuth } from "@/components/auth-provider";
import {
  AdminOnly,
  MutationDialog,
  ResourceError,
  SelectField,
  useResourceRefresh,
  validateText,
} from "@/components/resource-ui";
import { Button, buttonStyles } from "@/components/ui/button";
import {
  Badge,
  EmptyState,
  LoadingState,
  Panel,
  PanelHeader,
  TextField,
} from "@/components/ui/primitives";
import { PageHeading, SearchInput, TableViewport } from "@/components/workspace-ui";
import { platformApi } from "@/lib/api/platform";
import { accessLabel, databaseApi } from "@/lib/api/databases";
import { resourceID } from "@/lib/api/resources";

export function GroupsWorkspace() {
  return (
    <AdminOnly>
      <GroupsList />
    </AdminOnly>
  );
}

function GroupsList() {
  const { api } = useAuth();
  const platform = platformApi(api);
  const refresh = useResourceRefresh();
  const [search, setSearch] = useState("");
  const groups = useQuery({
    queryKey: ["platform", "groups"],
    queryFn: ({ signal }) => platform.groups(signal),
  });
  const filtered =
    groups.data?.groups.filter((group) =>
      group.name.toLowerCase().includes(search.trim().toLowerCase()),
    ) ?? [];
  return (
    <section className="min-w-0">
      <PageHeading
        title="Grup"
        description="Atur anggota, leader, dan hak akses database setiap tim."
        action={
          groups.isSuccess && (
            <MutationDialog
              title="Tambah grup"
              description="Tambahkan anggota lalu tetapkan salah satunya sebagai leader."
              trigger={
                <Button>
                  <Plus aria-hidden="true" />
                  Tambah grup
                </Button>
              }
              onSubmit={async (values) => {
                await platform.createGroup(validateText(values.get("name"), "name", 64));
                await refresh();
              }}
            >
              {(errors) => (
                <TextField
                  id="new-group-name"
                  name="name"
                  label="Nama grup"
                  required
                  autoComplete="off"
                  error={errors.name}
                />
              )}
            </MutationDialog>
          )
        }
      />
      {groups.isPending ? (
        <LoadingState />
      ) : groups.isError ? (
        <ResourceError error={groups.error} retry={groups.refetch} />
      ) : (
        <>
          <SearchInput
            id="group-search"
            label="Cari grup"
            placeholder="Cari grup…"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            className="mb-5 sm:max-w-80"
          />
          <TableViewport label="Daftar grup">
            <table className="data-table">
              <caption className="sr-only">Grup platform</caption>
              <thead>
                <tr>
                  <th scope="col">Nama grup</th>
                  <th scope="col">Leader</th>
                </tr>
              </thead>
              <tbody>
                {filtered.map((group) => (
                  <tr key={group.id}>
                    <th scope="row" className="font-medium">
                      <Link
                        href={`/platform/groups/${resourceID(group.id)}`}
                        className="inline-flex min-h-11 items-center [overflow-wrap:anywhere] hover:text-accent"
                      >
                        {group.name}
                      </Link>
                    </th>
                    <td>
                      <Badge tone={group.leader_id ? "neutral" : "warning"}>
                        {group.leader_id ? "Sudah ditetapkan" : "Belum ditetapkan"}
                      </Badge>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {!filtered.length && (
              <EmptyState
                title={search ? "Grup tidak ditemukan" : "Belum ada grup"}
                description={search ? "Coba nama lain." : "Buat grup untuk mengatur akses tim."}
              />
            )}
          </TableViewport>
        </>
      )}
    </section>
  );
}

export function GroupWorkspace({ id }: { id: string }) {
  return (
    <AdminOnly>
      <GroupContent key={id} id={id} />
    </AdminOnly>
  );
}

function GroupContent({ id }: { id: string }) {
  const { api } = useAuth();
  const router = useRouter();
  const platform = platformApi(api);
  const refresh = useResourceRefresh();
  const detail = useQuery({
    queryKey: ["platform", "groups", id],
    queryFn: ({ signal }) => platform.group(id, signal),
  });
  const users = useQuery({
    queryKey: ["platform", "users"],
    queryFn: ({ signal }) => platform.users(signal),
    enabled: detail.isSuccess,
  });
  const connections = useQuery({
    queryKey: ["connections"],
    queryFn: ({ signal }) => databaseApi(api).connections(signal),
    enabled: detail.isSuccess,
  });
  async function change(action: Promise<unknown>) {
    await action;
    await refresh();
  }
  if (detail.isPending) return <LoadingState />;
  if (detail.isError)
    return (
      <>
        <Link href="/platform/groups" className={buttonStyles({ variant: "ghost" })}>
          Kembali ke grup
        </Link>
        <ResourceError error={detail.error} retry={detail.refetch} />
      </>
    );
  const { group, members, grants } = detail.data;
  const available =
    users.data?.users.filter((user) => !members.some((member) => member.id === user.id)) ?? [];
  const leader = members.find((member) => member.id === group.leader_id);
  return (
    <div className="min-w-0 space-y-8">
      <Link
        href="/platform/groups"
        className="inline-flex min-h-11 items-center text-sm text-ink-secondary hover:text-accent"
      >
        ← Semua grup
      </Link>
      <PageHeading
        title={group.name}
        description="Kelola anggota dan akses database grup ini."
        action={
          <div className="flex flex-wrap gap-2">
            <MutationDialog
              title="Ubah nama grup"
              description="Nama baru akan tampil di seluruh workspace."
              trigger={<Button variant="secondary">Ubah nama</Button>}
              onSubmit={(values) =>
                change(platform.renameGroup(id, validateText(values.get("name"), "name", 64)))
              }
            >
              {(errors) => (
                <TextField
                  id="group-name"
                  name="name"
                  label="Nama grup"
                  defaultValue={group.name}
                  required
                  error={errors.name}
                />
              )}
            </MutationDialog>
            <MutationDialog
              title={`Hapus grup ${group.name}?`}
              description="Seluruh keanggotaan dan grant grup akan dicabut. Sesi pengguna yang terdampak dapat berakhir. Tindakan ini tidak dapat dibatalkan."
              submitLabel="Hapus grup"
              danger
              trigger={
                <Button variant="secondary" className="text-danger">
                  Hapus grup
                </Button>
              }
              onSubmit={async () => {
                await platform.deleteGroup(id);
                router.push("/platform/groups");
                await refresh();
              }}
            />
          </div>
        }
      />
      <Panel aria-label="Leader grup">
        <PanelHeader
          title="Leader"
          action={
            members.length > 0 && (
              <MutationDialog
                title="Tetapkan leader"
                description="Leader harus merupakan anggota grup. Perubahan leader dapat memerlukan review ulang migration."
                submitLabel="Tetapkan leader"
                trigger={
                  <Button variant="secondary">{leader ? "Ganti leader" : "Tetapkan leader"}</Button>
                }
                onSubmit={(values) =>
                  change(platform.setLeader(id, validateText(values.get("user_id"), "user_id", 64)))
                }
              >
                {(errors) => (
                  <SelectField
                    id="group-leader"
                    name="user_id"
                    label="Leader"
                    defaultValue={group.leader_id ?? ""}
                    required
                    error={errors.user_id}
                  >
                    <option value="" disabled>
                      Pilih anggota
                    </option>
                    {members.map((member) => (
                      <option key={member.id} value={member.id}>
                        {member.username}
                      </option>
                    ))}
                  </SelectField>
                )}
              </MutationDialog>
            )
          }
        />
        <div className="px-5 py-5 sm:px-6">
          {leader ? (
            <p className="text-sm font-medium [overflow-wrap:anywhere]">{leader.username}</p>
          ) : (
            <p className="text-sm leading-6 text-warning">
              Leader belum ditetapkan.{" "}
              {members.length
                ? "Pilih salah satu anggota agar grup dapat memberikan approval."
                : "Tambahkan anggota terlebih dahulu."}
            </p>
          )}
        </div>
      </Panel>
      <section aria-label="Anggota grup" className="min-w-0">
        <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
          <h2 className="text-base font-semibold">Anggota</h2>
          {available.length > 0 && (
            <MutationDialog
              title="Tambah anggota"
              description="Pengguna akan memperoleh hak akses database yang diberikan kepada grup ini."
              trigger={
                <Button variant="secondary">
                  <Plus aria-hidden="true" />
                  Tambah anggota
                </Button>
              }
              onSubmit={(values) =>
                change(
                  platform.setMember(id, validateText(values.get("user_id"), "user_id", 64), true),
                )
              }
            >
              {(errors) => (
                <SelectField
                  id="group-member"
                  name="user_id"
                  label="Pengguna"
                  required
                  defaultValue=""
                  error={errors.user_id}
                >
                  <option value="" disabled>
                    Pilih pengguna
                  </option>
                  {available.map((user) => (
                    <option key={user.id} value={user.id}>
                      {user.username}
                    </option>
                  ))}
                </SelectField>
              )}
            </MutationDialog>
          )}
        </div>
        {users.isError && <ResourceError error={users.error} retry={users.refetch} />}
        {users.isPending && (
          <p role="status" className="mb-3 text-sm text-ink-secondary">
            Memuat pilihan pengguna…
          </p>
        )}
        {users.isSuccess && !available.length && (
          <p className="mb-3 text-sm text-ink-secondary">
            Seluruh pengguna sudah menjadi anggota grup ini.
          </p>
        )}
        <TableViewport label="Daftar anggota">
          <table className="data-table min-w-[440px]">
            <caption className="sr-only">Anggota {group.name}</caption>
            <thead>
              <tr>
                <th scope="col">Username</th>
                <th scope="col">Peran grup</th>
                <th scope="col">Tindakan</th>
              </tr>
            </thead>
            <tbody>
              {members.map((member) => (
                <tr key={member.id}>
                  <th scope="row" className="max-w-60 font-medium [overflow-wrap:anywhere]">
                    {member.username}
                  </th>
                  <td>{member.id === group.leader_id ? <Badge>Leader</Badge> : "Anggota"}</td>
                  <td>
                    {member.id === group.leader_id ? (
                      <p className="max-w-48 text-xs leading-5 text-ink-secondary">
                        Ganti leader sebelum menghapus anggota ini.
                      </p>
                    ) : (
                      <MutationDialog
                        title={`Hapus ${member.username} dari grup?`}
                        description="Hak akses dari grup ini akan dicabut dan sesi aktif pengguna dapat berakhir."
                        submitLabel="Hapus anggota"
                        danger
                        trigger={
                          <Button variant="ghost" size="small" className="text-danger">
                            Hapus anggota
                          </Button>
                        }
                        onSubmit={() => change(platform.setMember(id, member.id, false))}
                      />
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {!members.length && (
            <EmptyState
              title="Belum ada anggota"
              description="Tambahkan pengguna lalu tetapkan leader grup."
            />
          )}
        </TableViewport>
      </section>
      <section aria-label="Grant database" className="min-w-0">
        <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
          <h2 className="text-base font-semibold">Akses database</h2>
          {connections.data && connections.data.connections.length > 0 && (
            <MutationDialog
              title="Atur akses database"
              description="Hak akses tertinggi dari seluruh grup pengguna akan berlaku. Pengurangan akses dapat mencabut sesi aktif."
              trigger={<Button variant="secondary">Atur akses</Button>}
              onSubmit={(values) =>
                change(
                  platform.setGrant(
                    id,
                    validateText(values.get("database_id"), "database_id", 64),
                    values.get("level") === "2" ? 2 : 1,
                  ),
                )
              }
            >
              {(errors) => (
                <>
                  <SelectField
                    id="grant-database"
                    name="database_id"
                    label="Database"
                    defaultValue=""
                    required
                    error={errors.database_id}
                  >
                    <option value="" disabled>
                      Pilih database
                    </option>
                    {connections.data.connections.map((connection) => (
                      <option key={connection.id} value={connection.id}>
                        {connection.name}
                      </option>
                    ))}
                  </SelectField>
                  <SelectField
                    id="grant-level"
                    name="level"
                    label="Hak akses"
                    defaultValue="1"
                    required
                    error={errors.level}
                  >
                    <option value="1">View Only</option>
                    <option value="2">View + Edit</option>
                  </SelectField>
                </>
              )}
            </MutationDialog>
          )}
        </div>
        <p className="mb-4 text-sm leading-6 text-ink-secondary">
          Platform admin tetap memerlukan grant grup untuk melihat isi database. Leader grup dengan
          akses View Only juga ikut menyetujui migration.
        </p>
        {connections.isError && (
          <ResourceError error={connections.error} retry={connections.refetch} />
        )}
        {connections.isPending && (
          <p role="status" className="mb-3 text-sm text-ink-secondary">
            Memuat pilihan database…
          </p>
        )}
        {connections.isSuccess && !connections.data.connections.length && (
          <p className="mb-3 text-sm text-ink-secondary">
            Tambahkan koneksi database terlebih dahulu melalui{" "}
            <Link href="/connections" className="underline">
              Kelola koneksi
            </Link>
            .
          </p>
        )}
        <TableViewport label="Daftar grant">
          <table className="data-table min-w-[480px]">
            <caption className="sr-only">Hak akses database {group.name}</caption>
            <thead>
              <tr>
                <th scope="col">Database</th>
                <th scope="col">Hak akses</th>
                <th scope="col">Tindakan</th>
              </tr>
            </thead>
            <tbody>
              {grants.map((grant) => (
                <tr key={grant.database_id}>
                  <th scope="row" className="max-w-60 font-medium [overflow-wrap:anywhere]">
                    {connections.data?.connections.find(
                      (connection) => connection.id === grant.database_id,
                    )?.name ?? `Database #${grant.database_id}`}
                  </th>
                  <td>{accessLabel(grant.level)}</td>
                  <td>
                    <div className="flex flex-wrap gap-1">
                      <MutationDialog
                        title="Ubah hak akses"
                        description="Pengurangan akses dapat mencabut sesi aktif pengguna dalam grup."
                        trigger={
                          <Button variant="ghost" size="small">
                            Ubah akses
                          </Button>
                        }
                        onSubmit={(values) =>
                          change(
                            platform.setGrant(
                              id,
                              grant.database_id,
                              values.get("level") === "2" ? 2 : 1,
                            ),
                          )
                        }
                      >
                        {(errors) => (
                          <SelectField
                            id={`grant-${grant.database_id}`}
                            name="level"
                            label="Hak akses"
                            defaultValue={String(grant.level)}
                            required
                            error={errors.level}
                          >
                            <option value="1">View Only</option>
                            <option value="2">View + Edit</option>
                          </SelectField>
                        )}
                      </MutationDialog>
                      <MutationDialog
                        title="Cabut akses database?"
                        description="Grant database dari grup ini akan dihapus dan sesi pengguna terdampak dapat berakhir."
                        submitLabel="Cabut akses"
                        danger
                        trigger={
                          <Button variant="ghost" size="small" className="text-danger">
                            Cabut akses
                          </Button>
                        }
                        onSubmit={() => change(platform.deleteGrant(id, grant.database_id))}
                      />
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {!grants.length && (
            <EmptyState
              title="Belum ada akses database"
              description="Berikan grant agar anggota dapat mengakses database."
            />
          )}
        </TableViewport>
      </section>
    </div>
  );
}
