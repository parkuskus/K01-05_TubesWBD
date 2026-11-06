"use client";

import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Plus } from "lucide-react";
import { useAuth } from "@/components/auth-provider";
import {
  AdminOnly,
  MutationDialog,
  ResourceError,
  useResourceRefresh,
  validatePassword,
  validateText,
} from "@/components/resource-ui";
import { Button } from "@/components/ui/button";
import { Badge, EmptyState, LoadingState, TextField } from "@/components/ui/primitives";
import { PageHeading, SearchInput, TableViewport } from "@/components/workspace-ui";
import { platformApi } from "@/lib/api/platform";

export function UsersWorkspace() {
  return (
    <AdminOnly>
      <UsersList />
    </AdminOnly>
  );
}

function UsersList() {
  const { api, user: currentUser } = useAuth();
  const platform = platformApi(api);
  const refresh = useResourceRefresh();
  const [search, setSearch] = useState("");
  const users = useQuery({
    queryKey: ["platform", "users"],
    queryFn: ({ signal }) => platform.users(signal),
  });
  const filtered =
    users.data?.users.filter((user) =>
      user.username.toLowerCase().includes(search.trim().toLowerCase()),
    ) ?? [];
  async function change(action: Promise<unknown>) {
    await action;
    await refresh();
  }
  return (
    <section className="min-w-0">
      <PageHeading
        title="Pengguna"
        description="Kelola akun dan hak administrator platform."
        action={
          users.isSuccess && (
            <MutationDialog
              title="Tambah pengguna"
              description="Akun baru dibuat sebagai pengguna biasa. Hak database diberikan melalui grup."
              trigger={
                <Button>
                  <Plus aria-hidden="true" />
                  Tambah pengguna
                </Button>
              }
              onSubmit={(values) =>
                change(
                  platform.createUser(
                    validateText(values.get("username"), "username", 64),
                    validatePassword(values.get("password")),
                  ),
                )
              }
            >
              {(errors) => (
                <>
                  <TextField
                    id="new-user-name"
                    name="username"
                    label="Username"
                    required
                    autoComplete="off"
                    error={errors.username}
                  />
                  <TextField
                    id="new-user-password"
                    name="password"
                    label="Password"
                    type="password"
                    required
                    autoComplete="new-password"
                    hint="Maksimal 72 byte."
                    error={errors.password}
                  />
                </>
              )}
            </MutationDialog>
          )
        }
      />
      {users.isPending ? (
        <LoadingState />
      ) : users.isError ? (
        <ResourceError error={users.error} retry={users.refetch} />
      ) : (
        <>
          <SearchInput
            id="user-search"
            label="Cari pengguna"
            placeholder="Cari username…"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            className="mb-5 sm:max-w-80"
          />
          <TableViewport label="Daftar pengguna">
            <table className="data-table min-w-[620px]">
              <caption className="sr-only">Akun pengguna platform</caption>
              <thead>
                <tr>
                  <th scope="col">Username</th>
                  <th scope="col">Peran</th>
                  <th scope="col">Tindakan</th>
                </tr>
              </thead>
              <tbody>
                {filtered.map((user) => (
                  <tr key={user.id}>
                    <th scope="row" className="max-w-60 font-medium [overflow-wrap:anywhere]">
                      {user.username}
                      {user.id === currentUser?.id && (
                        <span className="ml-2 text-xs font-normal text-ink-secondary">Anda</span>
                      )}
                    </th>
                    <td>
                      <Badge>{user.is_platform_admin ? "Administrator" : "Pengguna"}</Badge>
                    </td>
                    <td>
                      <div className="flex flex-wrap gap-1">
                        <MutationDialog
                          title={`Ubah username ${user.username}`}
                          description="Username baru digunakan untuk login berikutnya."
                          trigger={
                            <Button variant="ghost" size="small">
                              Ubah nama
                            </Button>
                          }
                          onSubmit={(values) =>
                            change(
                              platform.updateUser(user.id, {
                                username: validateText(values.get("username"), "username", 64),
                              }),
                            )
                          }
                        >
                          {(errors) => (
                            <TextField
                              id={`username-${user.id}`}
                              name="username"
                              label="Username"
                              defaultValue={user.username}
                              required
                              autoComplete="off"
                              error={errors.username}
                            />
                          )}
                        </MutationDialog>
                        <MutationDialog
                          title={`Ganti password ${user.username}`}
                          description="Seluruh sesi aktif pengguna ini akan dicabut. Pengguna perlu masuk kembali."
                          submitLabel="Ganti password"
                          trigger={
                            <Button variant="ghost" size="small">
                              Password
                            </Button>
                          }
                          onSubmit={(values) =>
                            change(
                              platform.setPassword(
                                user.id,
                                validatePassword(values.get("password")),
                              ),
                            )
                          }
                        >
                          {(errors) => (
                            <TextField
                              id={`password-${user.id}`}
                              name="password"
                              label="Password baru"
                              type="password"
                              required
                              autoComplete="new-password"
                              hint="Maksimal 72 byte."
                              error={errors.password}
                            />
                          )}
                        </MutationDialog>
                        <MutationDialog
                          title={
                            user.is_platform_admin
                              ? `Cabut admin ${user.username}?`
                              : `Jadikan ${user.username} admin?`
                          }
                          description={
                            user.is_platform_admin
                              ? "Hak administrasi akan dicabut dan sesi aktif pengguna akan berakhir. Administrator terakhir tetap dilindungi oleh server."
                              : "Pengguna dapat mengelola akun, grup, dan koneksi. Akses isi database tetap memerlukan grant grup."
                          }
                          submitLabel={user.is_platform_admin ? "Cabut admin" : "Jadikan admin"}
                          danger={user.is_platform_admin}
                          trigger={
                            <Button variant="ghost" size="small">
                              {user.is_platform_admin ? "Cabut admin" : "Jadikan admin"}
                            </Button>
                          }
                          onSubmit={() =>
                            change(
                              platform.updateUser(user.id, {
                                is_platform_admin: !user.is_platform_admin,
                              }),
                            )
                          }
                        />
                        <MutationDialog
                          title={`Hapus ${user.username}?`}
                          description="Akun, keanggotaan grup, dan seluruh sesi aktif pengguna akan dihapus. Tindakan ini tidak dapat dibatalkan."
                          submitLabel="Hapus pengguna"
                          danger
                          trigger={
                            <Button variant="ghost" size="small" className="text-danger">
                              Hapus
                            </Button>
                          }
                          onSubmit={() => change(platform.deleteUser(user.id))}
                        />
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {!filtered.length && (
              <EmptyState
                title={search ? "Pengguna tidak ditemukan" : "Belum ada pengguna"}
                description={
                  search ? "Coba username lain." : "Tambahkan akun untuk mulai mengatur akses."
                }
              />
            )}
          </TableViewport>
          <p role="status" className="mt-4 text-xs text-ink-secondary">
            Menampilkan {filtered.length} pengguna
          </p>
        </>
      )}
    </section>
  );
}
